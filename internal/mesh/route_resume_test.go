package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestReassertOSStateAfterResume proves that reassertOSState (invoked from
// onResume) re-installs the overlay interface address and redistributed routes
// after the OS has flushed them behind gravinet's back — the situation that
// otherwise leaves the machine with broken networking until a manual restart
// when it wakes from a long sleep.
//
// It also pins the stale-cache bug that motivates the fix: with ns.osMetric
// still claiming the route is installed, a plain syncRoute is a no-op and will
// not repair the dropped route. Only clearing the cache (what reassertOSState
// does) recovers it.
func TestReassertOSStateAfterResume(t *testing.T) {
	const netID = uint64(0x9E5074)
	key, _ := crypto.GenerateKey()
	route := netip.MustParsePrefix("10.40.40.0/24")

	A := spinWithRoutes(t, "A", netID, key, netip.MustParseAddr("10.7.0.1"), []netip.Prefix{route})
	B := spinWithRoutes(t, "B", netID, key, netip.MustParseAddr("10.7.0.2"), nil)
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

	// B learns A's route and installs it in its (fake) OS table.
	if !waitUntil(10*time.Second, func() bool { return B.dev.hasRoute(route) }) {
		t.Fatal("B did not install the redistributed route before the simulated sleep")
	}
	metric := B.dev.metricOf(route)

	bns := (*B.eng.nets.Load())[netID]
	if bns == nil {
		t.Fatal("B has no netState for the test network")
	}

	// Give B a subnet so the interface address is re-assertable, and confirm its
	// overlay address is currently programmed on the device.
	bns.mu.Lock()
	bns.subnet4 = netip.MustParsePrefix("10.7.0.0/24")
	self4 := bns.self4
	bns.mu.Unlock()
	if !self4.IsValid() {
		t.Fatal("B has no overlay v4 address to re-assert")
	}

	// --- Simulate a long sleep: the kernel drops the interface address and the
	// route while the host is suspended, but gravinet's osMetric cache is left
	// untouched (it still believes the route is programmed). ---
	if err := B.dev.DelRoute(route, metric); err != nil {
		t.Fatalf("simulating route flush: %v", err)
	}
	B.dev.mu.Lock()
	B.dev.assigned4 = netip.Addr{}
	B.dev.mu.Unlock()

	if B.dev.hasRoute(route) {
		t.Fatal("precondition: route should be gone from the OS table after the simulated flush")
	}

	// A plain syncRoute cannot repair this: osMetric still claims the prefix is
	// installed at the same metric, so syncRoute short-circuits. This is exactly
	// the failure that left routes missing after wake.
	B.eng.syncRoute(bns, route)
	if B.dev.hasRoute(route) {
		t.Fatal("expected syncRoute to be a no-op while osMetric is stale (cache bug)")
	}

	// reassertOSState clears the cache and re-programs both address and routes.
	B.eng.reassertOSState(bns)

	if !B.dev.hasRoute(route) {
		t.Fatal("reassertOSState did not re-install the dropped route")
	}
	if got := B.dev.metricOf(route); got != metric {
		t.Fatalf("route re-installed at wrong metric: got %d want %d", got, metric)
	}
	if got := B.dev.addr4(); got != self4 {
		t.Fatalf("reassertOSState did not re-add the overlay address: got %v want %v", got, self4)
	}
}

// TestReassertOSStateReinstallsBaseSubnetRoute is the regression test for the
// gap TestReassertOSStateAfterResume didn't cover: the base overlay subnet's
// own connected route (as opposed to a peer-advertised redistributed route)
// was never explicitly (re-)installed at all — it was only ever a hoped-for
// side effect of re-running AddIPv4 with the subnet's netmask. On macOS in
// particular, a sleep/resume cycle can drop just that derived route while
// leaving the interface's own address untouched, in which case re-running the
// identical AddIPv4 call is a real no-op (same address, same mask, nothing
// for the OS to reconfigure) and the route never comes back — while every
// redistributed route, reinstalled explicitly via syncRoute, does. The
// symptom: one specific peer (whichever needs the base subnet's route, i.e.
// most of them) stays unreachable after the laptop wakes, while routes to
// peer-advertised subnets keep working fine.
func TestReassertOSStateReinstallsBaseSubnetRoute(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:      1,
		Name:    "n",
		Dev:     dev,
		Subnet4: netip.MustParsePrefix("10.20.0.0/24"),
		Self4:   netip.MustParseAddr("10.20.0.5"),
	}}})
	ns := e.netSnapshot()[1]

	// Base subnet route was never installed via AddRoute — matches every real
	// backend today, since the connected route is normally only a side effect
	// of AddIPv4's netmask, never an explicit AddRoute call of its own.
	if dev.hasRoute(ns.subnet4) {
		t.Fatal("precondition: base subnet route should not be tracked via AddRoute yet")
	}

	e.reassertOSState(ns)

	if !dev.hasRoute(ns.subnet4) {
		t.Fatal("reassertOSState should explicitly (re-)install the base overlay subnet's connected route, not rely solely on AddIPv4's side effect")
	}
}
