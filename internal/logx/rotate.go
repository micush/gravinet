package logx

import (
	"fmt"
	"os"
	"sync"
)

// RotatingFile is an io.WriteCloser that appends to a log file and caps its
// size. It has two modes. In the default (numbered-backup) mode, once a write
// would push the file past MaxBytes it renames the current file to "<path>.1",
// shifting any existing "<path>.N" up by one and discarding anything beyond
// MaxBackups, then starts a fresh file. In FIFO mode (maxBackups is ignored)
// there are no backups: the single file is a rolling window, and when a write
// would exceed the cap the oldest whole lines are dropped from the front to
// make room for the newest, so the file never exceeds the cap and never loses
// the most recent output. Pure stdlib, safe for concurrent use. The file is
// closed before each rename/rewrite so it works on Windows too.
type RotatingFile struct {
	mu         sync.Mutex
	path       string
	maxBytes   int64
	maxBackups int
	fifo       bool
	f          *os.File
	size       int64
}

// NewRotatingFile opens (creating/appending) path and returns a rotating writer
// in numbered-backup mode. maxBytes <= 0 falls back to 10 MiB; maxBackups < 0
// is treated as 0.
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

// NewFIFOFile opens (creating/appending) path and returns a rotating writer in
// single-file FIFO mode: the file is capped at maxBytes and, when full, the
// oldest lines are dropped from the front rather than rotated into a numbered
// backup. maxBytes <= 0 falls back to 10 MiB. This is what the web admin's
// Log Size setting uses.
func NewFIFOFile(path string, maxBytes int64) (*RotatingFile, error) {
	if maxBytes <= 0 {
		maxBytes = 10 << 20
	}
	r := &RotatingFile{path: path, maxBytes: maxBytes, fifo: true}
	if err := r.open(); err != nil {
		return nil, err
	}
	// A file left over from a previous run (or a previous, larger cap) may
	// already be over the new cap; bring it into line immediately so the very
	// first write doesn't have to carry a huge trim, and so a shrink applied at
	// startup takes hold without waiting for the file to be written past the
	// old size first.
	if r.size > r.maxBytes {
		r.trimFront(0)
	}
	return r, nil
}

// SetMaxBytes changes the size cap at runtime. A shrink is applied immediately
// in FIFO mode (the file is trimmed to fit now, not on the next overflow), so
// lowering the cap in the web admin takes effect at once rather than only after
// the file has been written past the old, larger size. A no-op if the value is
// unchanged or not positive.
func (r *RotatingFile) SetMaxBytes(maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if maxBytes == r.maxBytes {
		return
	}
	r.maxBytes = maxBytes
	if r.fifo && r.size > r.maxBytes {
		r.trimFront(0)
	}
}

// MaxBytes reports the current cap. Mainly for tests and status.
func (r *RotatingFile) MaxBytes() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.maxBytes
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

// Write appends p, making room first if it would exceed the size cap. In FIFO
// mode "making room" trims the oldest lines from the front; otherwise it
// rotates to a numbered backup. A rotation/trim error is non-fatal: it keeps
// writing to the current file rather than dropping log lines.
func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		if r.fifo {
			r.trimFront(int64(len(p)))
		} else {
			r.rotate()
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

// trimFront drops the oldest bytes from the front of the file so that, after
// the pending write of `incoming` bytes, the file fits under the cap — keeping
// whole lines only. To avoid rewriting the whole file on every single line once
// it's full (which would make each write O(file size)), it trims in amortized
// chunks: it keeps at most half the cap worth of the newest bytes, so a trim
// only recurs roughly every (cap/2) bytes written rather than every line. The
// kept region is snapped forward to the byte after the next newline so the
// first surviving line isn't a fragment. Best-effort: on any filesystem error
// it leaves the current file as-is and returns, so logging continues.
func (r *RotatingFile) trimFront(incoming int64) {
	// Target size of the retained tail: leave room for the incoming write, and
	// keep no more than half the cap so trims amortize. Never negative.
	keep := r.maxBytes/2 - incoming
	if keep < 0 {
		keep = 0
	}
	if keep >= r.size {
		return // nothing to trim
	}
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	data, err := os.ReadFile(r.path)
	if err != nil {
		r.open() // try to keep logging
		return
	}
	start := int64(len(data)) - keep
	if start < 0 {
		start = 0
	}
	// Snap to the start of the next whole line so we don't keep a partial one.
	if nl := indexByteFrom(data, start, '\n'); nl >= 0 {
		start = nl + 1
	}
	tail := data[start:]
	// Rewrite the file with just the tail via a short-lived O_TRUNC handle, the
	// same platform-safe dance Truncate uses (O_APPEND handles can't truncate
	// on Windows).
	tf, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		r.open()
		return
	}
	tf.Write(tail)
	tf.Close()
	if err := r.open(); err != nil {
		r.f = nil
		r.size = 0
		return
	}
	r.size = int64(len(tail))
}

// indexByteFrom is bytes.IndexByte starting at offset `from`, returning an
// absolute index or -1. Kept local to avoid importing bytes for one call.
func indexByteFrom(b []byte, from int64, c byte) int64 {
	if from < 0 {
		from = 0
	}
	for i := from; i < int64(len(b)); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
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
