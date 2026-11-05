// Package bridge joins a node link to the relay link.
//
// The two links are independent: the relay connection stays up while the node
// is missing (the relay sees "bridge online, node offline"), and the node link
// keeps retrying while the relay is unreachable. Frames pass through unchanged;
// the only companion parsing is the identity check at node link-up.
//
// Never log frame payloads. They can carry private messages and channel keys.
package bridge

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dpup/sierra-meshbridge/internal/config"
	"github.com/dpup/sierra-meshbridge/internal/nodelink"
	meshproto "github.com/dpup/sierra-meshrelay-protocol"
)

// Options configures a Bridge. Zero durations take defaults.
type Options struct {
	Config  config.Config
	Opener  nodelink.Opener
	Log     *slog.Logger
	Version string

	// LinkKind and LinkDetail describe the configured link for hello while no
	// node link is open.
	LinkKind, LinkDetail string

	IdentifyTimeout time.Duration // per identity-check reply (default 5s)
	NodeRetryMin    time.Duration // node reopen backoff start (default 1s)
	NodeRetryMax    time.Duration // node reopen backoff cap (default 30s)
	MismatchRetry   time.Duration // recheck interval after a pubkey mismatch (default 30s)
	StatsInterval   time.Duration // stats message interval (default 60s)
	HelloTimeout    time.Duration // wait for welcome (default 10s)
	OutQueue        int           // node→relay frame buffer (default 64)
	Backoff         *Backoff      // relay reconnect policy (default DefaultBackoff)
}

func (o *Options) defaults() {
	def := func(d *time.Duration, v time.Duration) {
		if *d == 0 {
			*d = v
		}
	}
	def(&o.IdentifyTimeout, 5*time.Second)
	def(&o.NodeRetryMin, time.Second)
	def(&o.NodeRetryMax, 30*time.Second)
	def(&o.MismatchRetry, 30*time.Second)
	def(&o.StatsInterval, time.Minute)
	def(&o.HelloTimeout, 10*time.Second)
	if o.OutQueue == 0 {
		o.OutQueue = 64
	}
	if o.Backoff == nil {
		o.Backoff = DefaultBackoff()
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.LinkKind == "" {
		o.LinkKind = meshproto.LinkTCP
	}
}

// Bridge runs the node and relay links.
type Bridge struct {
	opts    Options
	log     *slog.Logger
	started time.Time

	mu          sync.Mutex
	node        meshproto.Node
	linkKind    string
	linkDetail  string
	nodeVer     uint64
	nodeChanged chan struct{} // closed and replaced on every node change
	link        nodelink.Link // open, identity-checked link; nil otherwise
	session     *session      // established relay session; nil otherwise

	relink chan struct{}

	nodeOpens       atomic.Int64
	relayWelcomes   atomic.Int64
	framesToNode    atomic.Int64
	framesFromNode  atomic.Int64
	droppedFromNode atomic.Int64
	droppedToNode   atomic.Int64
}

// New creates a Bridge.
func New(opts Options) *Bridge {
	opts.defaults()
	return &Bridge{
		opts:        opts,
		log:         opts.Log,
		node:        meshproto.Node{State: meshproto.NodeDisconnected},
		linkKind:    opts.LinkKind,
		linkDetail:  opts.LinkDetail,
		nodeChanged: make(chan struct{}),
		relink:      make(chan struct{}, 1),
	}
}

// Run runs both links until ctx ends.
func (b *Bridge) Run(ctx context.Context) error {
	b.started = time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b.runNode(ctx) }()
	go func() { defer wg.Done(); b.runRelay(ctx) }()
	wg.Wait()
	return ctx.Err()
}

// Relink closes and reopens the node link.
func (b *Bridge) Relink() {
	select {
	case b.relink <- struct{}{}:
	default:
	}
}

// Status is a snapshot of the bridge's state.
type Status struct {
	Node            meshproto.Node
	LinkKind        string
	LinkDetail      string
	RelayConnected  bool
	Attached        bool
	FramesToNode    int64
	FramesFromNode  int64
	DroppedFromNode int64
	DroppedToNode   int64
}

// Status returns the current state.
func (b *Bridge) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{
		Node: b.node, LinkKind: b.linkKind, LinkDetail: b.linkDetail,
		RelayConnected:  b.session != nil,
		FramesToNode:    b.framesToNode.Load(),
		FramesFromNode:  b.framesFromNode.Load(),
		DroppedFromNode: b.droppedFromNode.Load(),
		DroppedToNode:   b.droppedToNode.Load(),
	}
	if b.session != nil {
		st.Attached = b.session.isAttached()
	}
	return st
}

// setNode records a node change and wakes anyone waiting on it.
func (b *Bridge) setNode(n meshproto.Node, link nodelink.Link) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.link = link
	if link != nil {
		b.linkKind, b.linkDetail = link.Kind(), link.Detail()
	}
	if n == b.node {
		return
	}
	b.node = n
	b.nodeVer++
	close(b.nodeChanged)
	b.nodeChanged = make(chan struct{})
}

// nodeSnapshot returns the node state, its version and a channel closed on the
// next change.
func (b *Bridge) nodeSnapshot() (meshproto.Node, meshproto.Link, uint64, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.node, meshproto.Link{Kind: b.linkKind, Detail: b.linkDetail}, b.nodeVer, b.nodeChanged
}

func (b *Bridge) currentLink() nodelink.Link {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.link
}

func (b *Bridge) currentSession() *session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.session
}

func (b *Bridge) setSession(s *session) {
	b.mu.Lock()
	b.session = s
	b.mu.Unlock()
}

func (b *Bridge) clearSession(s *session) {
	b.mu.Lock()
	if b.session == s {
		b.session = nil
	}
	b.mu.Unlock()
}

func (b *Bridge) agent() meshproto.Agent {
	return meshproto.Agent{Name: "meshbridge", Version: b.opts.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// sleep waits d or until ctx ends, reporting whether the full wait happened.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
