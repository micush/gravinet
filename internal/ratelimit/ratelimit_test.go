package ratelimit

import (
	"testing"
	"time"
)

// clock is a controllable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newTestThrottle(c *clock) *Throttle {
	// 3 failures / 60s ⇒ ban 15min; 3s burst coalescing.
	th := New(3, 60*time.Second, 15*time.Minute, 3*time.Second)
	th.now = c.now
	return th
}

func TestBurstCounts1Attempt(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	th := newTestThrottle(c)
	const src = "203.0.113.7"

	// One join attempt = 8 rapid key tries within the coalesce window.
	for i := 0; i < 8; i++ {
		if th.Fail(src) {
			t.Fatalf("a single burst (try %d) should not ban", i)
		}
		c.add(100 * time.Millisecond)
	}
	if th.Banned(src) {
		t.Fatal("one coalesced attempt must not ban")
	}
}

func Test3AttemptsBan(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	th := newTestThrottle(c)
	const src = "203.0.113.7"

	attempt := func() bool {
		var banned bool
		for i := 0; i < 8; i++ { // burst of key tries
			banned = th.Fail(src)
			c.add(100 * time.Millisecond)
		}
		return banned
	}

	c.add(5 * time.Second)
	if attempt() {
		t.Fatal("attempt 1 should not ban")
	}
	c.add(5 * time.Second)
	if attempt() {
		t.Fatal("attempt 2 should not ban")
	}
	c.add(5 * time.Second)
	if !attempt() {
		t.Fatal("attempt 3 within the window must ban")
	}
	if !th.Banned(src) {
		t.Fatal("source should be banned")
	}

	// Still banned 14 minutes later, free after 15.
	c.add(14 * time.Minute)
	if !th.Banned(src) {
		t.Fatal("should still be banned at 14 min")
	}
	c.add(2 * time.Minute)
	if th.Banned(src) {
		t.Fatal("should be unbanned after 15 min")
	}
}

func TestWindowExpiry(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	th := newTestThrottle(c)
	const src = "198.51.100.4"

	th.Fail(src) // attempt 1
	c.add(30 * time.Second)
	th.Fail(src)            // attempt 2
	c.add(40 * time.Second) // attempt 1 now outside the 60s window
	if th.Fail(src) {       // counts as 2nd live failure, not 3rd
		t.Fatal("expired failures must not contribute to a ban")
	}
	if th.Banned(src) {
		t.Fatal("should not be banned after window expiry")
	}
}

func TestResetClears(t *testing.T) {
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	th := newTestThrottle(c)
	const src = "198.51.100.9"
	th.Fail(src)
	c.add(5 * time.Second)
	th.Fail(src)
	th.Reset(src) // successful auth
	c.add(5 * time.Second)
	if th.Fail(src) {
		t.Fatal("after reset, a single failure must not ban")
	}
}
