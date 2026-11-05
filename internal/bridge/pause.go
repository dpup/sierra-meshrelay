package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/dpup/sierra-meshrelay/meshproto"
)

// Pausing is the host's right. A paused bridge closes its node link, which
// frees the node's one companion connection for the host's own app, and
// reports node_status "paused" (with "until" for a timed pause). The relay
// ends any lease and turns new clients away with node_paused.
//
// A timed pause resumes by itself when it runs out, and also on restart. A
// pause "until resumed" is saved to Options.StatePath so it survives a
// restart, and only Resume ends it.

// pauseState is guarded by Bridge.mu.
type pauseState struct {
	paused  bool
	until   time.Time     // zero: until resumed
	changed chan struct{} // closed and replaced on every change
}

// savedState is the bridge's small state file.
type savedState struct {
	PausedUntilResumed bool `json:"paused_until_resumed,omitempty"`
}

// Pause stops sharing the node until until, or until Resume if until is zero.
// A time in the past resumes at once.
func (b *Bridge) Pause(until time.Time) {
	if !until.IsZero() && !until.After(time.Now()) {
		b.Resume()
		return
	}
	b.mu.Lock()
	b.pause.paused, b.pause.until = true, until
	b.pauseChangedLocked()
	b.mu.Unlock()
	b.saveState(savedState{PausedUntilResumed: until.IsZero()})
	if until.IsZero() {
		b.log.Info("sharing paused until resumed")
	} else {
		b.log.Info("sharing paused", "until", until.Format(time.RFC3339))
	}
}

// Resume ends a pause. Resuming an unpaused bridge does nothing.
func (b *Bridge) Resume() {
	b.mu.Lock()
	was := b.pause.paused
	b.pause.paused, b.pause.until = false, time.Time{}
	if was {
		// Leave "paused" before the node loop wakes, so nothing reports a
		// paused node while sharing is on, and the loop's own "connected"
		// can't be overwritten by a late update here.
		if b.node.State == meshproto.NodePaused {
			b.setNodeLocked(meshproto.Node{State: meshproto.NodeDisconnected, Error: "resuming"})
		}
		b.pauseChangedLocked()
	}
	b.mu.Unlock()
	if was {
		b.saveState(savedState{})
		b.log.Info("sharing resumed")
	}
}

// Paused reports whether sharing is paused, and until when (zero: until
// resumed).
func (b *Bridge) Paused() (bool, time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pause.paused, b.pause.until
}

func (b *Bridge) pauseChangedLocked() {
	close(b.pause.changed)
	b.pause.changed = make(chan struct{})
}

func (b *Bridge) pauseSnapshot() (bool, time.Time, <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.pause.paused, b.pause.until, b.pause.changed
}

// waitWhilePaused holds the node loop while sharing is paused, reporting the
// paused state. It returns once sharing resumes or ctx ends.
func (b *Bridge) waitWhilePaused(ctx context.Context) {
	for ctx.Err() == nil {
		paused, until, changed := b.pauseSnapshot()
		if !paused {
			return
		}
		n := meshproto.Node{State: meshproto.NodePaused}
		var expire <-chan time.Time
		if !until.IsZero() {
			n.Until = until.UTC().Format(time.RFC3339)
			d := time.Until(until)
			if d <= 0 {
				b.Resume()
				continue
			}
			t := time.NewTimer(d)
			expire = t.C
			defer t.Stop()
		}
		b.setNode(n, nil)
		select {
		case <-changed:
		case <-expire:
			b.log.Info("pause ran out")
			b.Resume()
		case <-ctx.Done():
		}
	}
}

// loadState applies the saved state at startup.
func (b *Bridge) loadState() {
	if b.opts.StatePath == "" {
		return
	}
	data, err := os.ReadFile(b.opts.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var st savedState
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil {
		b.log.Warn("ignoring unreadable state file", "path", b.opts.StatePath, "error", err)
		return
	}
	if st.PausedUntilResumed {
		b.pause.paused = true
		b.log.Info("sharing is paused until resumed (saved from before restart)")
	}
}

func (b *Bridge) saveState(st savedState) {
	if b.opts.StatePath == "" {
		return
	}
	if st == (savedState{}) {
		if err := os.Remove(b.opts.StatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.log.Warn("couldn't clear state file", "path", b.opts.StatePath, "error", err)
		}
		return
	}
	data, _ := json.Marshal(st)
	tmp := b.opts.StatePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		b.log.Warn("couldn't save state; a pause until resumed won't survive a restart", "error", err)
		return
	}
	if err := os.Rename(tmp, b.opts.StatePath); err != nil {
		b.log.Warn("couldn't save state", "error", err)
	}
}
