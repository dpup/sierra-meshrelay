// Package state is the bridge's small persistent state file (state.json next
// to the config): things it learns at run time that should survive a
// restart, as opposed to the config the host or setup writes.
package state

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
)

// State is what the bridge remembers across restarts.
type State struct {
	// PausedUntilResumed: the host paused sharing with no end time.
	PausedUntilResumed bool `json:"paused_until_resumed,omitempty"`
	// NodeAddr is the last host:port the node answered on, tried before a
	// LAN scan when the node's DHCP address may have changed.
	NodeAddr string `json:"node_addr,omitempty"`
}

// File is a State persisted at Path. A nil *File, or an empty Path, keeps
// state in memory only. Safe for concurrent use.
type File struct {
	Path string

	mu     sync.Mutex
	loaded bool
	st     State
}

// New returns the state file at path (not read until first use).
func New(path string) *File { return &File{Path: path} }

// Get returns the current state.
func (f *File) Get() State {
	if f == nil {
		return State{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	return f.st
}

// Update applies fn and saves the result if it changed.
func (f *File) Update(fn func(*State)) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loadLocked()
	next := f.st
	fn(&next)
	if next == f.st {
		return nil
	}
	f.st = next
	return f.saveLocked()
}

func (f *File) loadLocked() {
	if f.loaded {
		return
	}
	f.loaded = true
	if f.Path == "" {
		return
	}
	if data, err := os.ReadFile(f.Path); err == nil {
		_ = json.Unmarshal(data, &f.st) // an unreadable file reads as empty state
	}
}

func (f *File) saveLocked() error {
	if f.Path == "" {
		return nil
	}
	if f.st == (State{}) {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	data, _ := json.Marshal(f.st)
	tmp := f.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.Path)
}
