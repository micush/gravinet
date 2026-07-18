package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// routeMetric returns the metric B has for cidr on netID (0, ok=false if it
// doesn't know the route at all) — reloadRoutes_test's routeKnown only checks
// presence; the metric-change assertions below need the value too.
func routeMetric(n *testNode, netID uint64, cidr string) (int, bool) {
	for _, r := range n.eng.Routes(netID) {
		if r.CIDR == cidr {
			return r.Metric, true
		}
	}
	return 0, false
}

// TestSetBGPRoutesLive proves BGP-into-mesh redistribution (SetBGPRoutes,
// config.Network.RedistributeBGP's engine-side counterpart) applies live and
// propagates over the same gossip path a config-driven Advertise route
// already uses — a peer can't tell the two sources apart, by design (see
// bgpRedistSet's doc comment on netState): withdrawal, and a same-batch
// metric change, both need to work identically to the config-driven side
// routes_livereload_test.go already covers for that path.
func TestSetBGPRoutesLive(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xB6970E5)
	const cidr = "203.0.113.0/24"

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.51.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.51.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}
	if routeKnown(B, netID, cidr) {
		t.Fatal("B already has the route before it was redistributed")
	}

	// Redistribute a "BGP route" on A (SetBGPRoutes stands in for what
	// bgpMeshRedistributor would push after polling FRR) — B should learn it
	// exactly as if it were a config Advertise entry.
	if ok := A.eng.SetBGPRoutes(netID, []netip.Prefix{netip.MustParsePrefix(cidr)}, 5); !ok {
		t.Fatal("SetBGPRoutes reported networkID not found")
	}
	if !waitUntil(10*time.Second, func() bool { return routeKnown(B, netID, cidr) }) {
		t.Fatal("B never learned the BGP-redistributed route")
	}
	if m, ok := routeMetric(B, netID, cidr); !ok || m != 5 {
		t.Fatalf("B's route metric = %d (ok=%v), want 5", m, ok)
	}

	// Same batch, new metric (the operator changed RedistributeBGPMetric, or
	// a poll tick just re-pushed the same set at a new value) — re-advertises
	// live, same as reloadRoutes does for a config Advertise route's metric.
	if ok := A.eng.SetBGPRoutes(netID, []netip.Prefix{netip.MustParsePrefix(cidr)}, 9); !ok {
		t.Fatal("SetBGPRoutes (metric change) reported networkID not found")
	}
	if !waitUntil(10*time.Second, func() bool { m, ok := routeMetric(B, netID, cidr); return ok && m == 9 }) {
		t.Fatal("B never picked up the new metric")
	}

	// Withdrawing (an empty set — what the poller sends the moment
	// RedistributeBGP turns off, or BGP itself goes down) drops it on B.
	if ok := A.eng.SetBGPRoutes(netID, nil, 0); !ok {
		t.Fatal("SetBGPRoutes (withdraw) reported networkID not found")
	}
	if !waitUntil(10*time.Second, func() bool { return !routeKnown(B, netID, cidr) }) {
		t.Fatal("B kept the route after it was withdrawn")
	}
}

// TestSetBGPRoutesUnknownNetwork checks the ok=false contract for a
// networkID that isn't configured on this node — bgpMeshRedistributor treats
// that as "nothing to do", not an error, and relies on this return value to
// tell the two apart.
func TestSetBGPRoutesUnknownNetwork(t *testing.T) {
	key, _ := crypto.GenerateKey()
	A := spinNode(t, "A", uint64(0xB6970E6), key, netip.MustParseAddr("10.51.1.1"))
	defer func() { A.dev.Close(); A.eng.Stop(); A.tr.Close() }()

	if ok := A.eng.SetBGPRoutes(0xDEADBEEF, []netip.Prefix{netip.MustParsePrefix("203.0.113.0/24")}, 0); ok {
		t.Fatal("SetBGPRoutes on an unconfigured networkID reported ok=true")
	}
}

// TestSetBGPRoutesAdvertisedToNewPeer checks that advertiseRoutes (the "flood
// everything to a peer that just connected" path) includes the BGP-
// redistributed set, not just config Advertise routes — a peer joining
// *after* SetBGPRoutes was called must still learn the route, the same way
// it would for one advertised before it connected.
func TestSetBGPRoutesAdvertisedToNewPeer(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xB6970E7)
	const cidr = "203.0.113.0/25"

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.51.2.1"))
	defer func() { A.dev.Close(); A.eng.Stop(); A.tr.Close() }()

	if ok := A.eng.SetBGPRoutes(netID, []netip.Prefix{netip.MustParsePrefix(cidr)}, 3); !ok {
		t.Fatal("SetBGPRoutes reported networkID not found")
	}

	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.51.2.2"))
	defer func() { B.dev.Close(); B.eng.Stop(); B.tr.Close() }()
	lo := netip.MustParseAddr("127.0.0.1")
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return B.eng.PeerCount(netID) == 1 }) {
		t.Fatal("B did not connect to A")
	}
	if !waitUntil(10*time.Second, func() bool { return routeKnown(B, netID, cidr) }) {
		t.Fatal("B never learned the BGP-redistributed route on connecting to A")
	}
}
