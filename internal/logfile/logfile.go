// Package logfile is a size-capped log file, for platforms where a service
// has no journal to write to (Windows). When the file passes MaxBytes it is
// renamed to <path>.1, replacing the previous one, so the logs on disk never
// exceed about twice MaxBytes.
package logfile

import (
	"os"
	"sync"
)

// DefaultMaxBytes caps each of the two files.
const DefaultMaxBytes = 5 << 20

// File is an io.Writer safe for concurrent use.
type File struct {
	Path     string
	MaxBytes int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens (appending to) path.
func Open(path string) (*File, error) {
	l := &File{Path: path, MaxBytes: DefaultMaxBytes}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	if l.size > 0 && l.size+int64(len(p)) > l.MaxBytes {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate closes the file before renaming it: Windows can't rename an open
// file. A failed rename keeps writing to the same file.
func (l *File) rotate() {
	l.f.Close()
	l.f = nil
	_ = os.Rename(l.Path, l.Path+".1")
	if err := l.open(); err != nil {
		l.f = nil
	}
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
