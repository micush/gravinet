// Package ratelimit implements gravinet's brute-force throttle, shared by the
// handshake authenticator and the web-admin login.
//
// Policy (matching the spec): N failures within Window ⇒ ban for BanDuration.
// A Coalesce window collapses a rapid burst of failures into a single counted
// event — this is what makes "an initiator trying up to 8 keys in one attempt"
// count as ONE failed attempt rather than eight.
package ratelimit

import (
	"sync"
	"time"
)

// Throttle tracks failures per key (typically a source IP string).
type Throttle struct {
	mu       sync.Mutex
	max      int
	window   time.Duration
	ban      time.Duration
	coalesce time.Duration
	state    map[string]*entry
	now      func() time.Time // injectable for tests
}

type entry struct {
	fails       []time.Time
	lastCounted time.Time
	banUntil    time.Time
}

// New builds a Throttle. coalesce may be 0 to count every failure.
func New(max int, window, ban, coalesce time.Duration) *Throttle {
	return &Throttle{
		max:      max,
		window:   window,
		ban:      ban,
		coalesce: coalesce,
		state:    make(map[string]*entry),
		now:      time.Now,
	}
}

// Banned reports whether key is currently banned.
func (t *Throttle) Banned(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.state[key]
	if e == nil {
		return false
	}
	return t.now().Before(e.banUntil)
}

// Fail records a failure for key and reports whether key is now (or still)
// banned. Failures inside the coalesce window of the last counted failure are
// not counted again.
func (t *Throttle) Fail(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	e := t.state[key]
	if e == nil {
		e = &entry{}
		t.state[key] = e
	}
	if now.Before(e.banUntil) {
		return true // already banned; nothing to add
	}
	if t.coalesce > 0 && !e.lastCounted.IsZero() && now.Sub(e.lastCounted) < t.coalesce {
		return false // part of the same burst; one attempt only
	}

	// prune failures outside the sliding window
	cutoff := now.Add(-t.window)
	kept := e.fails[:0]
	for _, ts := range e.fails {
		if ts.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	e.fails = kept

	e.fails = append(e.fails, now)
	e.lastCounted = now
	if len(e.fails) >= t.max {
		e.banUntil = now.Add(t.ban)
		e.fails = nil
		return true
	}
	return false
}

// Reset clears all failure history for key (call on successful auth).
func (t *Throttle) Reset(key string) {
	t.mu.Lock()
	delete(t.state, key)
	t.mu.Unlock()
}

// BanUntil returns the time the key is banned until, or zero if not banned.
func (t *Throttle) BanUntil(key string) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.state[key]; e != nil && t.now().Before(e.banUntil) {
		return e.banUntil
	}
	return time.Time{}
}

// GC drops stale, unbanned entries to bound memory. Safe to call periodically.
func (t *Throttle) GC() {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	cutoff := now.Add(-t.window)
	for k, e := range t.state {
		if now.Before(e.banUntil) {
			continue
		}
		if e.lastCounted.Before(cutoff) {
			delete(t.state, k)
		}
	}
}
