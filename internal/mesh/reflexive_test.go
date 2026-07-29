package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func reflexiveEngine() *Engine {
	return NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
}

func TestNATStatusClassification(t *testing.T) {
	pub1 := netip.MustParseAddrPort("203.0.113.5:51820")
	pub2 := netip.MustParseAddrPort("203.0.113.5:40000") // same IP, different port (symmetric)
	lo := netip.MustParseAddrPort("127.0.0.1:51820")     // a real local interface address

	set := func(obs map[string]netip.AddrPort, age time.Duration) *Engine {
		e := reflexiveEngine()
		now := time.Now()
		for id, a := range obs {
			e.reflexive[id] = reflexiveObs{addr: a, at: now.Add(-age)}
		}
		return e
	}

	if c, _ := reflexiveEngine().NATStatus(); c != "unknown" {
		t.Errorf("no reports: class=%s, want unknown", c)
	}
	if c, p := set(map[string]netip.AddrPort{"a": pub1}, 0).NATStatus(); c != "nat" || p != pub1 {
		t.Errorf("single report: class=%s public=%v, want nat %v", c, p, pub1)
	}
	if c, p := set(map[string]netip.AddrPort{"a": pub1, "b": pub1}, 0).NATStatus(); c != "cone" || p != pub1 {
		t.Errorf("two agreeing: class=%s public=%v, want cone %v", c, p, pub1)
	}
	if c, _ := set(map[string]netip.AddrPort{"a": pub1, "b": pub2}, 0).NATStatus(); c != "symmetric" {
		t.Errorf("two disagreeing: class=%s, want symmetric", c)
	}
	// Two peers reporting two different addresses is also exactly what a
	// directly-addressed, multi-homed host looks like — e.g. traffic to
	// different peers legitimately leaving from different real public IPs,
	// no NAT involved. The classification only cares whether the reported
	// IP is one this host owns (isLocalUnderlayAddr ignores port), so a
	// second observation on the same local IP but a different port
	// exercises that path without depending on which other real addresses
	// happen to exist wherever this test runs.
	loOtherPort := netip.MustParseAddrPort("127.0.0.1:40000")
	if c, _ := set(map[string]netip.AddrPort{"a": lo, "b": loOtherPort}, 0).NATStatus(); c != "open" {
		t.Errorf("two disagreeing but both local (multi-homed): class=%s, want open", c)
	}
	// One real local address and one genuinely foreign one must still read
	// as symmetric — a single self-report mixed in must not mask real
	// per-destination NAT remapping seen from elsewhere.
	if c, _ := set(map[string]netip.AddrPort{"a": lo, "b": pub2}, 0).NATStatus(); c != "symmetric" {
		t.Errorf("one local, one foreign: class=%s, want symmetric", c)
	}
	if c, p := set(map[string]netip.AddrPort{"a": lo}, 0).NATStatus(); c != "open" || p != lo {
		t.Errorf("local addr: class=%s public=%v, want open %v", c, p, lo)
	}
	if c, _ := set(map[string]netip.AddrPort{"a": pub1}, reflexiveTTL+time.Minute).NATStatus(); c != "unknown" {
		t.Errorf("stale report: class=%s, want unknown (expired)", c)
	}
}

// The wire path: a received reflexive control message updates our status.
func TestOnReflexiveRecords(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]
	ps := &peerSession{net: ns, nodeID: "reporter"}
	want := netip.MustParseAddrPort("198.51.100.7:51820")
	e.onReflexive(ps, appendEndpoint(nil, want)) // body is what onControl passes (sans ctrl byte)
	if c, p := e.NATStatus(); c != "nat" || p != want {
		t.Fatalf("after report: class=%s public=%v, want nat %v", c, p, want)
	}
}
