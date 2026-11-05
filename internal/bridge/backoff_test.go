package bridge

import (
	"testing"
	"time"
)

func TestBackoffSequence(t *testing.T) {
	b := DefaultBackoff()
	b.Jitter = 0
	var got []time.Duration
	for range 8 {
		got = append(got, b.Next(SessionResult{CloseCode: 1006}))
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60}
	for i := range want {
		if got[i] != want[i]*time.Second {
			t.Fatalf("attempt %d: %s, want %ds (all: %v)", i, got[i], want[i], got)
		}
	}
	// A session that stayed up 5 minutes resets the sequence.
	if d := b.Next(SessionResult{CloseCode: 1001, Welcomed: true, Uptime: 5 * time.Minute}); d != time.Second {
		t.Fatalf("after a long session: %s, want 1s", d)
	}
	// A short session doesn't.
	if d := b.Next(SessionResult{CloseCode: 1001, Welcomed: true, Uptime: time.Minute}); d != 2*time.Second {
		t.Fatalf("after a short session: %s, want 2s", d)
	}
}

func TestBackoffJitter(t *testing.T) {
	b := DefaultBackoff()
	for range 200 {
		b.attempt = 3 // 8s nominal
		d := b.Next(SessionResult{})
		if d < 6400*time.Millisecond || d > 9600*time.Millisecond {
			t.Fatalf("jittered wait %s outside 8s ±20%%", d)
		}
	}
}

func TestBackoffClasses(t *testing.T) {
	cases := []struct {
		r    SessionResult
		want time.Duration
	}{
		{SessionResult{CloseCode: 1000}, time.Second},
		{SessionResult{CloseCode: 1001}, time.Second},
		{SessionResult{CloseCode: 1009}, time.Second},
		{SessionResult{CloseCode: 4003}, time.Second},
		{SessionResult{CloseCode: 4009}, time.Second},
		{SessionResult{CloseCode: 4001}, 15 * time.Minute},
		{SessionResult{CloseCode: 4002}, 15 * time.Minute},
		{SessionResult{CloseCode: 4004}, time.Hour},
		{SessionResult{HTTPStatus: 401, CloseCode: -1}, 15 * time.Minute},
		{SessionResult{HTTPStatus: 404, CloseCode: -1}, 15 * time.Minute},
		{SessionResult{HTTPStatus: 429, CloseCode: -1}, 15 * time.Minute},
		{SessionResult{HTTPStatus: 426, CloseCode: -1}, time.Hour},
		{SessionResult{HTTPStatus: 502, CloseCode: -1}, time.Second},
	}
	for _, c := range cases {
		b := DefaultBackoff()
		b.Jitter = 0
		if got := b.Next(c.r); got != c.want {
			t.Errorf("%+v: %s, want %s", c.r, got, c.want)
		}
	}
}
