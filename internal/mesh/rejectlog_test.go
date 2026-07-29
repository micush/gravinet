package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// A peer re-advertises its routes on every gossip round, so a rejected route
// is a standing condition rather than an event. Logging it unconditionally
// produced four lines every five seconds — two peers each advertising a v4 and
// a v6 default — which is enough to bury everything else in the log.
func TestRejectedRouteLogIsRateLimitedPerOriginAndPrefix(t *testing.T) {
	e := &Engine{}
	v4 := netip.MustParsePrefix("0.0.0.0/0")
	v6 := netip.MustParsePrefix("::/0")

	if !e.shouldLogRejectedRoute("cush1", v4) {
		t.Fatal("the first rejection for a given origin+prefix must be logged")
	}
	for i := 0; i < 100; i++ {
		if e.shouldLogRejectedRoute("cush1", v4) {
			t.Fatalf("repeat %d was logged; the whole point is that a standing condition is stated once per interval", i)
		}
	}

	// Each (origin, prefix) is tracked separately: suppressing one must not
	// hide a different peer or a different prefix being rejected.
	for _, c := range []struct {
		origin string
		p      netip.Prefix
	}{
		{"cush1", v6},
		{"cush2", v4},
		{"cush2", v6},
	} {
		if !e.shouldLogRejectedRoute(c.origin, c.p) {
			t.Errorf("%s %v was suppressed by an unrelated entry", c.origin, c.p)
		}
	}
}

// After the interval it must speak again, or a condition that persists for
// hours vanishes from the log entirely.
func TestRejectedRouteLogResumesAfterInterval(t *testing.T) {
	e := &Engine{}
	p := netip.MustParsePrefix("0.0.0.0/0")
	if !e.shouldLogRejectedRoute("cush1", p) {
		t.Fatal("first call must log")
	}
	e.rejectLogMu.Lock()
	e.rejectLogAt["cush1|"+p.String()] = time.Now().Add(-rejectedRouteLogEvery - time.Second)
	e.rejectLogMu.Unlock()

	if !e.shouldLogRejectedRoute("cush1", p) {
		t.Fatal("must log again once the interval has elapsed")
	}
}
