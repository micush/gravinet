package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestRouteFailoverBetweenTwoOrigins reproduces a reported scenario: two
// peers (A, B) both redistribute the same CIDR at different metrics; C
// learns the route from both, with A's lower metric winning; A then goes
// silent (its process stops responding, not an explicit disable/ban on C).
// Expected: C's route table fails over to B's advertisement of the same
// prefix, and C's connectivity to an entirely unrelated peer D is
// unaffected throughout. No existing test covers two *origins* advertising
// the *same* prefix — TestSweepStaleRoutesWithdrawsAndKeeps and
// TestPrunedNodeRoutesRemoved both only ever exercise a single origin per
// prefix.
func TestRouteFailoverBetweenTwoOrigins(t *testing.T) {
	const netID = uint64(0xFA170)
	key, _ := crypto.GenerateKey()
	route := netip.MustParsePrefix("10.77.0.0/24")

	mk := func(name string, self netip.Addr, routes []netip.Prefix, metric map[netip.Prefix]int) *testNode {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, Routes: routes, RouteMetric: metric}}})
		tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
		if err != nil {
			t.Fatal(err)
		}
		eng.Attach(tr)
		eng.Start()
		return &testNode{eng, tr, dev}
	}

	A := mk("A", netip.MustParseAddr("10.9.0.1"), []netip.Prefix{route}, map[netip.Prefix]int{route: 10})
	B := mk("B", netip.MustParseAddr("10.9.0.2"), []netip.Prefix{route}, map[netip.Prefix]int{route: 20})
	C := mk("C", netip.MustParseAddr("10.9.0.3"), nil, nil)
	D := mk("D", netip.MustParseAddr("10.9.0.4"), nil, nil) // unrelated peer, no route involvement

	aStopped := false
	defer func() {
		if !aStopped {
			A.dev.Close()
			A.eng.Stop()
			A.tr.Close()
		}
		for _, n := range []*testNode{B, C, D} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	// C connects to A, B, and D. A and B don't need to know about each other.
	C.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	C.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	C.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(D.tr.Port())))
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(C.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(C.tr.Port())))
	D.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(C.tr.Port())))

	metricOnC := func() (int, bool) {
		for _, ri := range C.eng.Routes(netID) {
			if ri.CIDR == route.String() {
				return ri.Metric, true
			}
		}
		return 0, false
	}

	// C should connect to all three and learn the route at A's (lower) metric.
	if !waitUntil(15*time.Second, func() bool { return C.eng.PeerCount(netID) >= 3 }) {
		t.Fatalf("C should connect to A, B, and D; got PeerCount=%d", C.eng.PeerCount(netID))
	}
	if !waitUntil(10*time.Second, func() bool { m, ok := metricOnC(); return ok && m == 10 }) {
		m, ok := metricOnC()
		t.Fatalf("C should learn route at metric 10 (A, the lower metric); got %d ok=%v", m, ok)
	}
	// The OS-installed metric carries MeshRouteMetricFloor on top of the
	// gossip-layer value above (see its own doc comment) — a mesh-learned
	// route must never outrank a locally-sourced one for the same prefix.
	if !waitUntil(5*time.Second, func() bool { return C.dev.hasRoute(route) && C.dev.metricOf(route) == 10+MeshRouteMetricFloor }) {
		t.Fatalf("C's OS route table should show metric %d; got %d", 10+MeshRouteMetricFloor, C.dev.metricOf(route))
	}

	// Sanity: C can actually reach D before A goes away — establishes the
	// "connectivity to an unrelated peer" baseline this test is protecting.
	if C.eng.PeerCount(netID) < 3 {
		t.Fatalf("precondition failed: C not connected to all three peers before A goes silent")
	}
	dSession := func() bool {
		for _, pi := range C.eng.ListPeers(netID) {
			if pi.NodeID == "D" {
				return true
			}
		}
		return false
	}
	if !dSession() {
		t.Fatal("precondition failed: C has no session to D before A goes silent")
	}

	// A goes silent — not an explicit disable/ban on C, just stops responding,
	// matching "I turned off the peer" (the peer's own process/machine, not a
	// local admin action on the observing node).
	A.eng.Stop()
	A.tr.Close()
	aStopped = true

	// Let C's real, already-running maintLoop discover this on its own —
	// routeTTL (20s default) is independent of the session peerTimeout, so
	// the route-level staleness sweep doesn't need A's session pruned
	// first. Deliberately real wall-clock time, no manual clock
	// fast-forwarding: an earlier version of this test called pruneDead
	// with an artificially-advanced "now" on every poll iteration, which
	// (bug in the test, not the engine) made every peer's session look
	// stale on every single check, not just A's — repeatedly pruning and
	// reconnecting B and D too. Letting the real maintLoop tick naturally
	// avoids that entirely and is a more faithful reproduction of what the
	// report actually described.
	var lastMetric int
	var lastOK bool
	failedOver := waitUntil(45*time.Second, func() bool {
		lastMetric, lastOK = metricOnC()
		return lastOK && lastMetric == 20
	})
	if !failedOver {
		t.Errorf("route did not fail over to B (metric 20) after A went silent; last seen metric=%d ok=%v", lastMetric, lastOK)
	}
	if !waitUntil(10*time.Second, func() bool { return C.dev.hasRoute(route) && C.dev.metricOf(route) == 20+MeshRouteMetricFloor }) {
		t.Errorf("C's OS route table did not update to B's metric %d; got %d, hasRoute=%v", 20+MeshRouteMetricFloor, C.dev.metricOf(route), C.dev.hasRoute(route))
	}

	// A must no longer appear connected — this needs real time past
	// defaultPeerTimeout (20s as of this writing — see engine.go, now
	// independently configurable via mesh.Options.PeerTimeout/config's
	// peer_timeout), not just routeTTL (already elapsed above by the time
	// the route failover was confirmed); give it a comfortable margin past
	// that plus a couple of maintInterval ticks.
	if !waitUntil(45*time.Second, func() bool {
		for _, pi := range C.eng.ListPeers(netID) {
			if pi.NodeID == "A" {
				return false
			}
		}
		return true
	}) {
		t.Errorf("A still appears in ListPeers well past the peer timeout (%s) — this is exactly the reported \"still shows online and connected\" symptom", defaultPeerTimeout)
	}

	// And, the core of the report: connectivity to the totally unrelated peer
	// D must be completely unaffected by A's departure.
	if !dSession() {
		t.Errorf("C lost its session to unrelated peer D after A went silent — this is the reported \"unable to ping any other peer\" symptom")
	}
	if C.eng.PeerCount(netID) < 2 { // B and D should both still be connected
		t.Errorf("C.PeerCount = %d after A went silent, want >= 2 (B and D still connected)", C.eng.PeerCount(netID))
	}
}
