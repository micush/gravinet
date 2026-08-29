package tui

// Data a page needs that is too slow to fetch in the draw path.
//
// Most of the snapshot is a config file read and four unix-socket round
// trips, all of which finish in well under a frame. A few things do not:
// TakeHostSnapshot deliberately blocks for a second (CPU and throughput are
// rates and need two samples), vtysh is given an eight-second budget because
// FRR can hang right after boot, and lldpd, a log tail, and a document read
// are all at the mercy of a disk that may be the reason somebody opened this.
//
// Fetching those inline would freeze the console on the exact hosts where
// freezing is least acceptable. So a page *declares* what it needs by calling
// need(); the first call starts a goroutine and returns not-ready, the page
// renders a "reading…" state, and when the value lands the fetch signals the
// event loop to redraw. The page function itself never blocks and stays a
// pure function of (snapshot, lazyState) for testing, because a test can
// simply put the value in.

import (
	"errors"
	"sync"
)

// errOffline is what a fetch reports when lazyState is in offline mode.
var errOffline = errors.New("not read (this console is running without host access)")

// lazyState holds fetched values keyed by a page-chosen name.
type lazyState struct {
	mu      sync.Mutex
	entries map[string]*lazyEntry
	// offline makes need() answer immediately with an error instead of
	// starting a fetch. Set by tests, which walk every page and would
	// otherwise shell out to vtysh and lldpcli on the machine running them —
	// querying a real FRR if one happens to be installed, and making the
	// result depend on what the test host has. A page reached this way
	// renders its error state, which is a state worth covering anyway.
	offline bool
	// wake is signalled when a fetch completes, so the event loop knows to
	// redraw. Buffered depth 1 and sent non-blocking: a redraw is a redraw,
	// and two completions arriving together need one, not two.
	wake chan struct{}
}

type lazyEntry struct {
	val     any
	err     error
	done    bool
	running bool
}

func newLazyState() *lazyState {
	return &lazyState{entries: map[string]*lazyEntry{}, wake: make(chan struct{}, 1)}
}

// need returns a value if it has been fetched, and starts fetching it if not.
// ready is false while a fetch is in flight, which is the page's cue to
// render a reading state rather than an empty one.
//
// fn runs on its own goroutine and must not touch anything but its own
// inputs — it is running alongside a draw.
func (l *lazyState) need(key string, fn func() (any, error)) (val any, err error, ready bool) {
	l.mu.Lock()
	e, ok := l.entries[key]
	if !ok {
		e = &lazyEntry{}
		l.entries[key] = e
	}
	if e.done {
		v, er := e.val, e.err
		l.mu.Unlock()
		return v, er, true
	}
	if l.offline {
		l.mu.Unlock()
		return nil, errOffline, true
	}
	if e.running {
		l.mu.Unlock()
		return nil, nil, false
	}
	e.running = true
	l.mu.Unlock()

	go func() {
		v, er := fn()
		l.mu.Lock()
		e.val, e.err, e.done, e.running = v, er, true, false
		l.mu.Unlock()
		select {
		case l.wake <- struct{}{}:
		default:
		}
	}()
	return nil, nil, false
}

// set stores a value directly, without a fetch. Used by tests to make a page
// deterministic, and by the refresh path to seed a value it already has.
func (l *lazyState) set(key string, val any, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[key] = &lazyEntry{val: val, err: err, done: true}
}

// invalidate drops a key so the next need() re-fetches it. This is what the
// refresh key does: a console showing a five-minute-old log tail because it
// was cached is worse than one that takes a moment to re-read it.
//
// An in-flight fetch is left alone rather than cancelled — it will complete
// and store into an entry nobody is waiting for any more, which is harmless,
// where cancelling would mean threading a context through six readers that
// do not take one.
func (l *lazyState) invalidate(keys ...string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		if e, ok := l.entries[k]; ok && !e.running {
			delete(l.entries, k)
		} else if ok {
			e.done = false
		}
	}
}

// invalidateAll drops every cached value.
func (l *lazyState) invalidateAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, e := range l.entries {
		if !e.running {
			delete(l.entries, k)
		} else {
			e.done = false
		}
	}
}

// reading is the item a page shows while a fetch is in flight. One phrasing,
// so every slow page says the same thing.
func reading(what string) item {
	return para{text: "reading " + what + "\u2026", tone: "mut"}
}

// lazyLines is the common case: a fetch that produces []string. Wraps need()
// so the six pages doing exactly this do not each repeat the type assertion.
func lazyLines(l *lazyState, key string, fn func() ([]string, error)) (lines []string, err error, ready bool) {
	v, e, ok := l.need(key, func() (any, error) {
		out, err := fn()
		return out, err
	})
	if !ok {
		return nil, nil, false
	}
	if e != nil {
		return nil, e, true
	}
	if s, is := v.([]string); is {
		return s, nil, true
	}
	return nil, nil, true
}
