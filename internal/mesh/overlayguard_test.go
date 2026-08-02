package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestOverlayGuardRouteInstalledOnHandshake proves that once two nodes
// establish a session, each installs an explicit /32 host route to the
// other's overlay address, at metric 0 — the defense against a LAN route to
// a peer's exact overlay address shadowing gravinet's own (necessarily
// wider) subnet route. Without this, "the peer's subnet route is present and
// correct" is not enough to guarantee delivery, since routing always prefers
// the most specific match regardless of what installed it or when.
func TestOverlayGuardRouteInstalledOnHandshake(t *testing.T) {
	const netID = uint64(0x60021)
	key, _ := crypto.GenerateKey()

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.9.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.0.2"))
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

	guardOnA := netip.PrefixFrom(netip.MustParseAddr("10.9.0.2"), 32)
	guardOnB := netip.PrefixFrom(netip.MustParseAddr("10.9.0.1"), 32)

	if !waitUntil(10*time.Second, func() bool { return A.dev.hasRoute(guardOnA) }) {
		t.Fatal("A did not install a host guard route for B's overlay address")
	}
	if !waitUntil(10*time.Second, func() bool { return B.dev.hasRoute(guardOnB) }) {
		t.Fatal("B did not install a host guard route for A's overlay address")
	}
	if m := A.dev.metricOf(guardOnA); m != overlayGuardMetric {
		t.Fatalf("A's guard route metric = %d, want %d (lowest usable, to win a same-specificity tie)", m, overlayGuardMetric)
	}
	if m := B.dev.metricOf(guardOnB); m != overlayGuardMetric {
		t.Fatalf("B's guard route metric = %d, want %d", m, overlayGuardMetric)
	}
}

// TestOverlayGuardRouteRemovedOnBan proves a peer's guard route is torn down
// once it's banned — the same hygiene dropNodeRoutes already applies to
// redistributed prefixes, applied here to the guard route installed at
// session establishment.
func TestOverlayGuardRouteRemovedOnBan(t *testing.T) {
	const netID = uint64(0x60022)
	key, _ := crypto.GenerateKey()

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.9.1.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.1.2"))
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

	guardOnA := netip.PrefixFrom(netip.MustParseAddr("10.9.1.2"), 32)
	if !waitUntil(10*time.Second, func() bool { return A.dev.hasRoute(guardOnA) }) {
		t.Fatal("A did not install a host guard route for B's overlay address before ban")
	}

	if err := A.eng.BanNode(netID, "B", "test"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { return !A.dev.hasRoute(guardOnA) }) {
		t.Fatal("A did not remove B's host guard route after banning it")
	}
}

// TestReassertOverlayGuardRoutesSelfHeals proves reassertOverlayGuardRoutes
// re-installs a live peer's guard route after it's gone missing from the OS
// table — the scenario a real shadowing route produces (present, then
// overridden or aged out by whatever else is contesting that destination).
// This is what makes the guard a continuous defense rather than a one-time
// install: reconcileDataplane calls this on the same cadence it already
// re-asserts the base subnet route.
func TestReassertOverlayGuardRoutesSelfHeals(t *testing.T) {
	const netID = uint64(0x60023)
	key, _ := crypto.GenerateKey()

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.9.2.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.2.2"))
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

	guardOnA := netip.PrefixFrom(netip.MustParseAddr("10.9.2.2"), 32)
	if !waitUntil(10*time.Second, func() bool { return A.dev.hasRoute(guardOnA) }) {
		t.Fatal("A did not install a host guard route for B's overlay address")
	}

	// Simulate the field scenario directly: something else in the OS table
	// wins the destination out from under gravinet's own route.
	if err := A.dev.DelRoute(guardOnA, overlayGuardMetric); err != nil {
		t.Fatalf("simulate external route loss: %v", err)
	}
	if A.dev.hasRoute(guardOnA) {
		t.Fatal("test setup: guard route should be gone after the simulated DelRoute")
	}

	ns := A.eng.network(netID)
	if ns == nil {
		t.Fatal("network not found")
	}
	A.eng.reassertOverlayGuardRoutes(ns)

	if !A.dev.hasRoute(guardOnA) {
		t.Fatal("reassertOverlayGuardRoutes did not restore the guard route after it went missing")
	}
}
