package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

func routeKnown(n *testNode, netID uint64, cidr string) bool {
	for _, r := range n.eng.Routes(netID) {
		if r.CIDR == cidr {
			return true
		}
	}
	return false
}

// TestRouteRedistributionLive proves that redistributing a route applies live —
// no restart — and that withdrawing it propagates too, both through the same
// ReloadRuntime path the web UI and control socket use.
func TestRouteRedistributionLive(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x600D12E)
	const cidr = "192.168.60.0/24"

	// A starts with NO redistributed routes.
	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.41.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.41.0.2"))
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
		t.Fatal("B already has the route before it was added")
	}

	// Redistribute the route on A live (no restart) via the reload path.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Routes: []netip.Prefix{netip.MustParsePrefix(cidr)}}); err != nil {
		t.Fatalf("add-route reload: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { return routeKnown(B, netID, cidr) }) {
		t.Fatal("B never learned the route added live on A")
	}

	// Withdraw it on A live → B drops it.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID}); err != nil {
		t.Fatalf("withdraw reload: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { return !routeKnown(B, netID, cidr) }) {
		t.Fatal("B kept the route after it was withdrawn live on A")
	}
}

// TestRouteRejectLive checks that adding a reject live purges a route that was
// already learned — not just that it blocks future advertisements.
func TestRouteRejectLive(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x7E1EC7)
	const cidr = "192.168.70.0/24"

	A := spinNodeRoutes(t, "A", netID, key, netip.MustParseAddr("10.42.0.1"),
		[]netip.Prefix{netip.MustParsePrefix(cidr)}, nil)
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.42.0.2"))
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
	if !waitUntil(10*time.Second, func() bool { return routeKnown(B, netID, cidr) }) {
		t.Fatal("B never learned the route")
	}

	// Now reject it on B, live.
	if err := B.eng.ReloadRuntime(netID, NetSpec{ID: netID, RouteReject: []RejectRule{{Prefix: netip.MustParsePrefix(cidr)}}}); err != nil {
		t.Fatalf("reject reload: %v", err)
	}
	if !waitUntil(8*time.Second, func() bool { return !routeKnown(B, netID, cidr) }) {
		t.Fatal("B kept an already-learned route after rejecting it live")
	}
	// And re-advertisement must not bring it back.
	A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Routes: []netip.Prefix{netip.MustParsePrefix(cidr)}})
	time.Sleep(2 * time.Second)
	if routeKnown(B, netID, cidr) {
		t.Fatal("rejected route reappeared on B after re-advertisement")
	}
}
