package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatesAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meshbridge.log")
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.MaxBytes = 100
	line := strings.Repeat("x", 39) + "\n" // 40 bytes
	for i := 0; i < 5; i++ {
		if _, err := l.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	cur, _ := os.ReadFile(path)
	old, _ := os.ReadFile(path + ".1")
	// 2 lines, rotate, 2 lines, rotate, 1 line.
	if len(cur) != 40 || len(old) != 80 {
		t.Fatalf("current %d bytes, previous %d bytes", len(cur), len(old))
	}

	// Reopening appends and counts what is already there.
	l, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	l.MaxBytes = 100
	_, _ = l.Write([]byte(line))
	_ = l.Close()
	if cur, _ := os.ReadFile(path); len(cur) != 80 {
		t.Fatalf("after reopen: %d bytes", len(cur))
	}
}
