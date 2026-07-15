package mesh

import (
	"fmt"
	"net/netip"
	"syscall"
	"testing"

	"gravinet/internal/tun"
)

// fakeDemotionCall records one demoteDefaultRouteFn invocation for
// assertions.
type fakeDemotionCall struct {
	family         int
	excludeIfIndex int32
	newMetric      int
}

// withFakeDemotion swaps routeDemotionNeeded/demoteDefaultRouteFn/
// defaultGatewayFn for fakes that always succeed, tracking the physical
// default route's metric starting from startMetric and updating that
// tracked state on every demote call — so assertions against it exercise
// the real demote-then-restore round trip (each call's "previous metric"
// reflects the last value this same fake was set to), not just that some
// function got called. defaultGatewayFn reflects the same tracked metric
// (not a frozen value) because demotePhysicalDefaultRoute (as of v323)
// reads it before deciding whether to demote — a fake that never changed
// would make every call look like either a permanent no-op or a permanent
// re-demote, neither of which matches a real kernel. Mirrors withFakeGateway's
// shape (fulltunnel_test.go): a call log plus a t.Cleanup-registered restore.
func withFakeDemotion(t *testing.T, startMetric int) *[]fakeDemotionCall {
	t.Helper()
	var calls []fakeDemotionCall
	current := startMetric

	origNeeded, origFn, origGW := routeDemotionNeeded, demoteDefaultRouteFn, defaultGatewayFn
	routeDemotionNeeded = true
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		calls = append(calls, fakeDemotionCall{family: family, excludeIfIndex: excludeIfIndex, newMetric: newMetric})
		old := current
		current = newMetric
		return old, nil
	}
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: netip.MustParseAddr("192.0.2.1"), IfIndex: 7, Metric: current}, nil
	}
	t.Cleanup(func() {
		routeDemotionNeeded, demoteDefaultRouteFn, defaultGatewayFn = origNeeded, origFn, origGW
	})
	return &calls
}

// TestSyncFullTunnelRouteDemotesExistingDefaultOnInstall confirms the core
// of what routeDemotionNeeded platforms (FreeBSD, OpenBSD, macOS, Windows)
// need that Linux doesn't: before the mesh's own default route is ever
// installed, the pre-existing physical one is deprioritized to
// demotedDefaultMetric first, excluding gravinet's own tun ifindex from the
// lookup, and the metric it had before that is recorded for later restore.
func TestSyncFullTunnelRouteDemotesExistingDefaultOnInstall(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	demoted := withFakeDemotion(t, 5) // physical default route currently at metric 5
	dev := ns.spec.Dev.(*fakeDev)

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)

	if !dev.hasRoute(def) {
		t.Fatal("expected the mesh default route to be installed")
	}
	if len(*demoted) != 1 {
		t.Fatalf("expected exactly one demotion call, got %+v", *demoted)
	}
	call := (*demoted)[0]
	if call.newMetric != demotedDefaultMetric {
		t.Fatalf("demoted to metric %d, want %d", call.newMetric, demotedDefaultMetric)
	}
	if call.family != syscall.AF_INET {
		t.Fatalf("expected AF_INET for a 0.0.0.0/0 default route, got %d", call.family)
	}
	tunIdx, _ := dev.IfIndex()
	if call.excludeIfIndex != tunIdx {
		t.Fatalf("demotion excluded ifindex %d, want the tun device's own %d", call.excludeIfIndex, tunIdx)
	}
	ns.osMu.Lock()
	got, ok := ns.demotedGatewayMetric[def]
	ns.osMu.Unlock()
	if !ok || got != 5 {
		t.Fatalf("expected demotedGatewayMetric[def] = 5 (the pre-demotion metric), got %d, ok=%v", got, ok)
	}
}

// TestSyncFullTunnelRouteDemotionUsesV6Family confirms an accepted ::/0
// demotes the physical default route in the v6 table, not v4.
func TestSyncFullTunnelRouteDemotionUsesV6Family(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	demoted := withFakeDemotion(t, 0)

	def6 := netip.MustParsePrefix("::/0")
	advertiseRoute(e, ns, "peerA", def6, 0)

	if len(*demoted) != 1 {
		t.Fatalf("expected one demotion call, got %+v", *demoted)
	}
	if (*demoted)[0].family != syscall.AF_INET6 {
		t.Fatalf("expected AF_INET6 for a ::/0 default route, got %d", (*demoted)[0].family)
	}
}

// TestSyncFullTunnelRouteDoesNotRedemoteOnMetricChange confirms demotion
// only happens once, on the transition into full-tunnel — a later
// metric-only update to an already-active mesh default route (the same
// scenario TestSyncFullTunnelRouteMetricChange exercises) must not demote
// the physical route a second time, since it was already moved out of the
// way when full-tunnel first went active.
func TestSyncFullTunnelRouteDoesNotRedemoteOnMetricChange(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	demoted := withFakeDemotion(t, 5)

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)
	if len(*demoted) != 1 {
		t.Fatalf("setup: expected one demotion call after initial install, got %+v", *demoted)
	}

	ns.mu.Lock()
	for i := range ns.redist {
		if ns.redist[i].origin == "peerA" && ns.redist[i].prefix == def {
			ns.redist[i].metric = 5
		}
	}
	ns.mu.Unlock()
	e.syncRoute(ns, def)

	if len(*demoted) != 1 {
		t.Fatalf("expected no additional demotion call on a metric-only update, got %+v", *demoted)
	}
}

// TestSyncFullTunnelRouteRestoresOnWithdrawal confirms the physical default
// route's metric is put back to what it was before demotion once the
// mesh's own default route is withdrawn — not left permanently pinned at
// demotedDefaultMetric.
func TestSyncFullTunnelRouteRestoresOnWithdrawal(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	demoted := withFakeDemotion(t, 5)

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)
	if len(*demoted) != 1 {
		t.Fatalf("setup: expected one demotion call, got %+v", *demoted)
	}

	withdrawRouteFrom(e, ns, "peerA", def)

	if len(*demoted) != 2 {
		t.Fatalf("expected a restore call after withdrawal, got %+v", *demoted)
	}
	restore := (*demoted)[1]
	if restore.newMetric != 5 {
		t.Fatalf("restore call set metric %d, want the original 5", restore.newMetric)
	}
	ns.osMu.Lock()
	_, ok := ns.demotedGatewayMetric[def]
	ns.osMu.Unlock()
	if ok {
		t.Fatal("expected demotedGatewayMetric entry to be cleared after restoration")
	}
}

// TestDemotePhysicalDefaultRouteSkipsIfAlreadyRecorded guards against the
// specific scenario demotePhysicalDefaultRoute's doc comment calls out:
// reassertOSState clears ns.osMetric on resume-from-sleep and re-drives
// syncRoute for every prefix, which would otherwise call this a second time
// and overwrite the real original metric in ns.demotedGatewayMetric with
// whatever the physical route measures at while already demoted.
func TestDemotePhysicalDefaultRouteSkipsIfAlreadyRecorded(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	demoted := withFakeDemotion(t, 5)

	def := netip.MustParsePrefix("0.0.0.0/0")
	e.demotePhysicalDefaultRoute(ns, def)
	e.demotePhysicalDefaultRoute(ns, def) // simulate reassertOSState re-driving this

	if len(*demoted) != 1 {
		t.Fatalf("expected only one demotion call across two invocations, got %+v", *demoted)
	}
	ns.osMu.Lock()
	got := ns.demotedGatewayMetric[def]
	ns.osMu.Unlock()
	if got != 5 {
		t.Fatalf("expected the recorded pre-demotion metric to stay 5, got %d", got)
	}
}

// TestDemotePhysicalDefaultRouteRedemotesAfterNetworkChange is the
// regression test for the v323 field report: switching Wi-Fi networks while
// full-tunnel stayed active silently stopped routing through the mesh, and
// nothing short of a full process restart recovered it. Simulates that by
// changing what defaultGatewayFn reports mid-test — a genuinely different
// physical default route (a new gateway/metric, standing in for a fresh
// DHCP lease on a new network) appearing where the old, already-demoted one
// used to be — and confirms demotePhysicalDefaultRoute treats that as a
// real route needing its own demotion, not as evidence the old one is still
// there. Contrast with TestDemotePhysicalDefaultRouteSkipsIfAlreadyRecorded
// just above, where the second call sees the *same* still-demoted route and
// correctly does nothing.
func TestDemotePhysicalDefaultRouteRedemotesAfterNetworkChange(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	demoted := withFakeDemotion(t, 5) // old network's physical default: metric 5

	def := netip.MustParsePrefix("0.0.0.0/0")
	e.demotePhysicalDefaultRoute(ns, def)
	if len(*demoted) != 1 {
		t.Fatalf("expected one demotion call after the first activation, got %+v", *demoted)
	}

	// Simulate a Wi-Fi switch: a brand-new physical default route shows up
	// at its own fresh, undemoted metric (a new DHCP lease's own value),
	// standing in for what a real network change hands back once the old
	// route is gone. Both fakes are replaced together, consistently — a
	// real kernel has one routing table backing both what a read reports
	// and what a write changes, so the test's two fakes need to agree the
	// same way withFakeDemotion's coordinated pair already does above.
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: netip.MustParseAddr("198.51.100.1"), IfIndex: 7, Metric: 600}, nil
	}
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		*demoted = append(*demoted, fakeDemotionCall{family: family, excludeIfIndex: excludeIfIndex, newMetric: newMetric})
		return 600, nil // the new network's route was at 600 before this call demotes it
	}
	// Also warm the stale physicalGateway cache the way a live activation
	// would have, so this test can confirm it gets invalidated too — a
	// stale bypass-route gateway is the second half of the same bug.
	ns.osMu.Lock()
	ns.physicalGW[syscall.AF_INET] = physicalGWCache{addr: netip.MustParseAddr("192.0.2.1"), ifIndex: 7}
	ns.osMu.Unlock()

	e.demotePhysicalDefaultRoute(ns, def) // simulate checkUnderlayChange re-driving this after the roam

	if len(*demoted) != 2 {
		t.Fatalf("expected a second demotion call for the new physical route, got %+v", *demoted)
	}
	second := (*demoted)[1]
	if second.newMetric != demotedDefaultMetric {
		t.Fatalf("second demotion targeted metric %d, want %d", second.newMetric, demotedDefaultMetric)
	}
	ns.osMu.Lock()
	got, ok := ns.demotedGatewayMetric[def]
	cached, cachedOK := ns.physicalGW[syscall.AF_INET]
	ns.osMu.Unlock()
	if !ok || got != 600 {
		t.Fatalf("expected demotedGatewayMetric[def] updated to the new network's real original metric 600, got %d, ok=%v", got, ok)
	}
	if !cachedOK || cached.addr != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("expected physicalGW cache refreshed to the new gateway 198.51.100.1, got %+v (ok=%v) — stale cache would silently point bypass routes at a gateway that's gone", cached, cachedOK)
	}
}


// failure is best-effort, not fatal: the mesh's own default route still
// gets installed (matching syncFullTunnelRoute's existing not-fatal
// handling of every other OS-table failure in this path), and no bogus
// demotedGatewayMetric entry is recorded for a demotion that never actually
// happened.
func TestSyncFullTunnelRouteInstallsEvenIfDemotionFails(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	origNeeded, origFn := routeDemotionNeeded, demoteDefaultRouteFn
	routeDemotionNeeded = true
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		return 0, fmt.Errorf("simulated demotion failure")
	}
	t.Cleanup(func() { routeDemotionNeeded, demoteDefaultRouteFn = origNeeded, origFn })

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)

	if !dev.hasRoute(def) {
		t.Fatal("expected the mesh default route to still install even though demotion failed")
	}
	ns.osMu.Lock()
	_, ok := ns.demotedGatewayMetric[def]
	ns.osMu.Unlock()
	if ok {
		t.Fatal("expected no demotedGatewayMetric entry recorded when demotion itself failed")
	}
}

// TestSyncFullTunnelRouteNoDemotionWhenNotNeeded confirms routeDemotionNeeded
// gates this entirely: forced false here (as of v317 no real platform with a
// working gateway backend actually sets it false — see
// gateway_unsupported.go, whose GatewaySupported=false already keeps
// full-tunnel from activating at all — but the mesh-layer gate itself is
// still worth covering directly, independent of what any platform currently
// chooses), no demotion call happens, and installing alongside the physical
// default route at a lower metric is exactly what full-tunnel already did
// through v316.
func TestSyncFullTunnelRouteNoDemotionWhenNotNeeded(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	origNeeded, origFn := routeDemotionNeeded, demoteDefaultRouteFn
	routeDemotionNeeded = false
	called := false
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		called = true
		return 0, nil
	}
	t.Cleanup(func() { routeDemotionNeeded, demoteDefaultRouteFn = origNeeded, origFn })

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)

	if !dev.hasRoute(def) {
		t.Fatal("expected the mesh default route to be installed")
	}
	if called {
		t.Fatal("expected no demotion call when routeDemotionNeeded is false")
	}
}
