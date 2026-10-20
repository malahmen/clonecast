// Package logcap is a log file with a size cap. A log that only grows is one
// bug away from filling the disk: an agent refused in a tight loop wrote 15 MB
// in 90 seconds (REFERENCE.md 4.27), and clonecast with 20+ clients logs a
// line or two per click. At the cap the file is renamed to <path>.1, replacing
// the one before, and a new one is started: at most twice the cap on disk, and
// the latest lines are always kept. Platform-neutral, for clonecast and the
// agent alike.
package logcap

import (
	"os"
	"sync"
)

// File is an append-only log file of at most Max bytes. Safe for concurrent
// use.
type File struct {
	path string
	max  int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open opens path for appending, creating it if needed. max <= 0 means no cap.
func Open(path string, max int64) (*File, error) {
	l := &File{path: path, max: max}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// Write appends p, starting a new file first when p would take this one past
// the cap.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.max > 0 && l.size > 0 && l.size+int64(len(p)) > l.max {
		l.rotate()
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate moves the full file to <path>.1 and starts a new one. Where the
// rename fails (another process has the file open, which Windows refuses),
// the file is emptied instead: the cap is what matters.
func (l *File) rotate() {
	_ = l.f.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		_ = os.Truncate(l.path, 0)
	}
	if err := l.open(); err != nil {
		// Nowhere to write: keep the size so the next write retries the
		// rotation, and let the writes fail.
		l.f, _ = os.Open(os.DevNull)
	}
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
