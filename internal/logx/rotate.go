package logx

import (
	"fmt"
	"os"
	"sync"
)

// RotatingFile is an io.WriteCloser that appends to a log file and rotates it
// once a write would push it past MaxBytes. Rotation renames the current file to
// "<path>.1", shifting any existing "<path>.N" up by one and discarding anything
// beyond MaxBackups, then starts a fresh file. Pure stdlib, safe for concurrent
// use. The file is closed before each rename so rotation works on Windows too.
type RotatingFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	f          *os.File
	size       int64
}

// NewRotatingFile opens (creating/appending) path and returns a rotating writer.
// maxBytes <= 0 falls back to 10 MiB; maxBackups < 0 is treated as 0.
func NewRotatingFile(path string, maxBytes int64, maxBackups int) (*RotatingFile, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	if maxBackups < 0 {
		maxBackups = 0
	}
	r := &RotatingFile{path: path, maxBytes: maxBytes, maxBackups: maxBackups}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f = f
	r.size = fi.Size()
	return nil
}

// Write appends p, rotating first if it would exceed the size cap. A rotation
// error is non-fatal: it keeps writing to the current file rather than dropping
// log lines.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		r.rotate()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// rotate closes the current file, shifts the numbered backups up, and reopens a
// fresh file. Best-effort: filesystem errors are ignored so logging continues.
func (r *RotatingFile) rotate() {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	if r.maxBackups > 0 {
		os.Remove(fmt.Sprintf("%s.%d", r.path, r.maxBackups)) // drop the oldest
		for i := r.maxBackups - 1; i >= 1; i-- {
			os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		os.Rename(r.path, r.path+".1")
	} else {
		os.Remove(r.path) // no backups kept: just start over
	}
	if err := r.open(); err != nil {
		// Couldn't reopen at the canonical path; nothing more we can do here.
		r.f = nil
		r.size = 0
	}
}

// Truncate empties the active log file and resets the size counter, so
// subsequent writes start a fresh file. Backups are left untouched. Safe for
// concurrent use with Write.
//
// It deliberately does NOT call Truncate(0) on the active handle. That handle is
// opened O_APPEND, which Windows maps to FILE_APPEND_DATA without FILE_WRITE_DATA;
// the underlying SetEndOfFile then fails with "Access is denied". Instead we close
// the handle, empty the file through a short-lived O_TRUNC (write-access) handle,
// and reopen in append mode. This works on every platform.
func (r *RotatingFile) Truncate() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		// Couldn't empty the file; try to restore an append handle so logging
		// keeps working, then surface the original error.
		if openErr := r.open(); openErr != nil {
			r.f = nil
			r.size = 0
		}
		return err
	}
	f.Close()
	// Reopen in the normal append mode; open() resets size from the now-empty file.
	if err := r.open(); err != nil {
		r.f = nil
		r.size = 0
		return err
	}
	return nil
}

// Close closes the underlying file.
func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		err := r.f.Close()
		r.f = nil
		return err
	}
	return nil
}
