package bridge_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dpup/sierra-meshbridge/internal/bridge"
	"github.com/dpup/sierra-meshbridge/internal/config"
	"github.com/dpup/sierra-meshbridge/internal/fakenode"
	"github.com/dpup/sierra-meshbridge/internal/nodelink"
	"github.com/dpup/sierra-meshbridge/internal/protocol"
	"github.com/dpup/sierra-meshbridge/internal/relaytest"
)

const token = "smb_test-token"

type harness struct {
	t      *testing.T
	relay  *relaytest.Server
	node   *fakenode.Node
	bridge *bridge.Bridge
	logs   *syncBuffer
	cancel context.CancelFunc
	done   chan struct{}
}

type setup struct {
	nodeOpts fakenode.Options
	expected string
	tune     func(*bridge.Options)
	noNode   bool
}

func start(t *testing.T, s setup) *harness {
	t.Helper()
	h := &harness{t: t, relay: relaytest.New(token), logs: &syncBuffer{}, done: make(chan struct{})}
	t.Cleanup(h.relay.Close)
	opener := nodelink.TCP{Host: "127.0.0.1", Port: 1} // nothing listens on port 1
	if !s.noNode {
		n, err := fakenode.Listen("127.0.0.1:0", s.nodeOpts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { n.Close() })
		h.node = n
		opener.Port = n.Addr().Port
	}
	cfg := config.Default()
	cfg.Gateway, cfg.Relay, cfg.Token = "test-gw", h.relay.URL(), token
	cfg.Node.ExpectedPubkey = s.expected
	opts := bridge.Options{
		Config: cfg, Opener: opener, Version: "test",
		Log:             slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		IdentifyTimeout: 300 * time.Millisecond,
		NodeRetryMin:    10 * time.Millisecond,
		NodeRetryMax:    50 * time.Millisecond,
		MismatchRetry:   50 * time.Millisecond,
		Backoff: &bridge.Backoff{Min: 10 * time.Millisecond, Max: 50 * time.Millisecond,
			ResetAfter: time.Minute, AuthWait: time.Hour, UpgradeWait: time.Hour},
	}
	if s.tune != nil {
		s.tune(&opts)
	}
	h.bridge = bridge.New(opts)
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { defer close(h.done); h.bridge.Run(ctx) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("bridge didn't stop")
	}
	// The core rule: no payload bytes in logs. Every test payload contains
	// this marker.
	if strings.Contains(h.logs.String(), "SECRET") {
		h.t.Error("frame payload appeared in logs")
	}
}

func (h *harness) session() *relaytest.Session {
	h.t.Helper()
	s, err := h.relay.Next()
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) waitFor(what string, cond func(bridge.Status) bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond(h.bridge.Status()) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s (status %+v)", what, h.bridge.Status())
}

func (h *harness) attach(s *relaytest.Session) {
	h.t.Helper()
	if err := s.Attach("l_1"); err != nil {
		h.t.Fatal(err)
	}
	h.waitFor("attach", func(st bridge.Status) bool { return st.Attached })
}

func expectNode(t *testing.T, s *relaytest.Session, state string) protocol.Node {
	t.Helper()
	n, err := s.ExpectNode(state)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func expectFrame(t *testing.T, s *relaytest.Session) []byte {
	t.Helper()
	f, err := s.ExpectFrame()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func expectClosed(t *testing.T, s *relaytest.Session) int {
	t.Helper()
	c, err := s.ExpectClosed()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHelloAndIdentity(t *testing.T) {
	h := start(t, setup{nodeOpts: fakenode.Options{Name: "JayNode", Model: "Heltec V4"}})
	s := h.session()
	if s.Hello.Protocol != 1 || s.Hello.Gateway != "test-gw" || s.Hello.Agent.Name != "meshbridge" ||
		s.Hello.Agent.Version != "test" || s.Hello.Agent.OS == "" || s.Hello.Link.Kind != "tcp" {
		t.Fatalf("hello = %+v", s.Hello)
	}
	n := expectNode(t, s, protocol.NodeConnected)
	if n.Pubkey != h.node.PubkeyHex() || n.Name != "JayNode" || n.Model != "Heltec V4" || n.FW != "v1.18.0" {
		t.Fatalf("node = %+v", n)
	}
}

func TestFramesRoundTrip(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	h.attach(s)
	for i := range 20 {
		f := append([]byte{0x20, byte(i)}, "SECRET"...)
		if err := s.SendFrame(f); err != nil {
			t.Fatal(err)
		}
		got := expectFrame(t, s)
		if !bytes.Equal(got, append([]byte{fakenode.EchoPrefix}, f...)) {
			t.Fatalf("frame %d came back as % x", i, got)
		}
	}
	st := h.bridge.Status()
	if st.FramesToNode != 20 || st.DroppedFromNode != 0 {
		t.Fatalf("status %+v", st)
	}
}

func TestNodeFramesDroppedWithoutLease(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	if err := h.node.Push([]byte("\x80SECRET")); err != nil {
		t.Fatal(err)
	}
	if err := s.NoFrame(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	h.waitFor("drop counted", func(st bridge.Status) bool { return st.DroppedFromNode == 1 })

	// After detach, frames are dropped again.
	h.attach(s)
	if err := s.Send(protocol.Detach{Type: protocol.TypeDetach, Lease: "l_1"}); err != nil {
		t.Fatal(err)
	}
	h.waitFor("detach", func(st bridge.Status) bool { return !st.Attached })
	_ = h.node.Push([]byte("\x80SECRET"))
	if err := s.NoFrame(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestRelink(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	if err := s.Send(protocol.Relink{Type: protocol.TypeRelink}); err != nil {
		t.Fatal(err)
	}
	expectNode(t, s, protocol.NodeDisconnected)
	expectNode(t, s, protocol.NodeConnected)
	if got := h.node.Accepted(); got != 2 {
		t.Fatalf("node accepted %d connections, want 2", got)
	}
}

func TestNodeDropRelayStays(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	h.node.DropConn() // node reboot
	n := expectNode(t, s, protocol.NodeDisconnected)
	if n.Error == "" || n.Pubkey != "" {
		t.Fatalf("disconnected node = %+v", n)
	}
	expectNode(t, s, protocol.NodeConnected)
	if err := h.relay.NoSession(100 * time.Millisecond); err != nil {
		t.Fatal("relay session was replaced:", err)
	}
}

func TestRelayUpWithoutNode(t *testing.T) {
	h := start(t, setup{noNode: true})
	s := h.session()
	if s.Hello.Node.State != protocol.NodeDisconnected {
		t.Fatalf("hello node = %+v", s.Hello.Node)
	}
	h.waitFor("relay connected", func(st bridge.Status) bool { return st.RelayConnected })
}

func TestPubkeyMismatch(t *testing.T) {
	h := start(t, setup{expected: strings.Repeat("ab", 32)})
	s := h.session()
	n := expectNode(t, s, protocol.NodeMismatch)
	if n.Pubkey != h.node.PubkeyHex() {
		t.Fatalf("mismatch node = %+v", n)
	}
	h.attach(s)
	_ = s.SendFrame([]byte("SECRET"))
	if err := s.NoFrame(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.logs.String(), "isn't the node SIERRA expects") {
		t.Error("mismatch not logged")
	}
}

func TestExpectedPubkeyMatches(t *testing.T) {
	key := fakenode.KeyFor("expected")
	h := start(t, setup{nodeOpts: fakenode.Options{Pubkey: key}, expected: strings.ToLower(hex.EncodeToString(key))})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
}

func TestSilentNodeNeverConnects(t *testing.T) {
	h := start(t, setup{nodeOpts: fakenode.Options{Silent: true}})
	s := h.session()
	_ = s
	time.Sleep(500 * time.Millisecond)
	if st := h.bridge.Status(); st.Node.State != protocol.NodeDisconnected {
		t.Fatalf("node state %q", st.Node.State)
	}
	if !strings.Contains(h.logs.String(), "identity check failed") {
		t.Error("identity failure not logged")
	}
}

func TestOversizeNodeFrameRelinks(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	h.attach(s)
	// A header announcing 600 bytes: the link is broken, not forwarded.
	_ = h.node.Push(make([]byte, 600))
	n := expectNode(t, s, protocol.NodeDisconnected)
	if n.Error != "node link out of sync" {
		t.Fatalf("error = %q", n.Error)
	}
	expectNode(t, s, protocol.NodeConnected)
	if err := s.NoFrame(100 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestReconnectAfterRelayClose(t *testing.T) {
	for _, code := range []int{protocol.CloseNormal, protocol.CloseGoingAway, protocol.CloseMessageTooBig, protocol.CloseReplaced} {
		h := start(t, setup{})
		s := h.session()
		expectNode(t, s, protocol.NodeConnected)
		_ = s.Close(code, "test")
		s2 := h.session()
		// The new session's hello carries the live node state.
		if s2.Hello.Node.State != protocol.NodeConnected {
			t.Fatalf("close %d: second hello node = %+v", code, s2.Hello.Node)
		}
		h.stop()
	}
}

func TestAuthCloseBacksOff(t *testing.T) {
	for _, code := range []int{protocol.CloseBadToken, protocol.CloseGatewayMismatch, protocol.CloseUnsupported} {
		h := start(t, setup{})
		s := h.session()
		h.waitFor("relay session", func(st bridge.Status) bool { return st.RelayConnected })
		_ = s.Close(code, "test")
		if err := h.relay.NoSession(300 * time.Millisecond); err != nil {
			t.Fatalf("close %d: %v", code, err)
		}
		h.stop()
	}
}

func TestHTTPRejectNoRetryLoop(t *testing.T) {
	for _, status := range []int32{401, 404, 429} {
		h := start(t, setup{})
		h.relay.RejectStatus.Store(status)
		// The first dial may already be in flight; allow it, then no more.
		time.Sleep(300 * time.Millisecond)
		if d := h.relay.Dials(); d > 1 {
			t.Fatalf("HTTP %d: %d dials, want 1 (would trip the relay's auth lockout)", status, d)
		}
		h.stop()
	}
}

func TestBackpressureNoDrops(t *testing.T) {
	const n = 1000
	h := start(t, setup{tune: func(o *bridge.Options) { o.OutQueue = 8 }})
	h.relay.ReadDelay = 200 * time.Microsecond
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	h.attach(s)
	go func() {
		for i := range n {
			f := []byte{0x83, byte(i >> 8), byte(i), 'S', 'E', 'C', 'R', 'E', 'T'}
			if err := h.node.Push(f); err != nil {
				return
			}
		}
	}()
	for i := range n {
		f := expectFrame(t, s)
		if got := int(f[1])<<8 | int(f[2]); got != i {
			t.Fatalf("frame %d arrived as %d (reordered or dropped)", i, got)
		}
	}
	if st := h.bridge.Status(); st.DroppedFromNode != 0 {
		t.Fatalf("dropped %d frames", st.DroppedFromNode)
	}
}

func TestStats(t *testing.T) {
	h := start(t, setup{tune: func(o *bridge.Options) { o.StatsInterval = 50 * time.Millisecond }})
	s := h.session()
	var st protocol.Stats
	if err := s.Expect(protocol.TypeStats, &st); err != nil {
		t.Fatal(err)
	}
	if st.RelayReconnects != 0 || st.UptimeS < 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestShutdownClosesGoingAway(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	expectNode(t, s, protocol.NodeConnected)
	h.waitFor("relay session", func(st bridge.Status) bool { return st.RelayConnected })
	h.cancel()
	code := expectClosed(t, s)
	if code != protocol.CloseGoingAway {
		t.Fatalf("close code %d, want 1001", code)
	}
}

func TestErrorMessageLogsCodeOnly(t *testing.T) {
	h := start(t, setup{})
	s := h.session()
	_ = s.Send(protocol.Error{Type: protocol.TypeError, Code: protocol.ErrPubkeyMismatch, Message: "SECRET detail"})
	_ = s.Send(map[string]string{"type": "some_future_message"})
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(h.logs.String(), "code=pubkey_mismatch") {
		if time.Now().After(deadline) {
			t.Fatal("error code not logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
