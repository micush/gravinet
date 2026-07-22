package mesh

import (
	"fmt"
	"net/netip"
	"testing"

	"gravinet/internal/tun"
)

// TestPhysicalGatewayCachedAcrossDemotion is the regression test for a field
// report: on a routeDemotionNeeded platform, once demotePhysicalDefaultRoute
// has actually removed the physical default route from the OS routing table
// (real behavior as of v320 — see gateway_freebsd.go/gateway_openbsd.go's
// DemoteDefaultRoute), a live gateway lookup excluding this network's own
// tun finds nothing at all: the physical route genuinely isn't there
// anymore. Simulated here by making the fake defaultGatewayFn start failing
// once demotion has "happened" — exactly what a real FreeBSD/OpenBSD kernel
// does. Without caching, every acquireBypassRoute call after that point
// would silently fail (logged only at Debug), which is exactly what the
// field report described: the mesh default route took, but none of the
// peer/seed bypass host routes did, breaking connectivity.
//
// This also exercises demotePhysicalDefaultRoute's own cache warm-up
// specifically: advertiseRoute here doesn't create any live peer session or
// seed, so resyncAllBypassRoutes/syncSeedBypassRoutes (which normally run
// first and would incidentally populate the cache themselves) have nothing
// to do — the only thing that can populate the cache before demotion is
// demotePhysicalDefaultRoute's own explicit pre-resolve.
func TestPhysicalGatewayCachedAcrossDemotion(t *testing.T) {
	e, ns := testEngineWithNet(t)

	fakeGW := tun.Gateway{Addr: netip.MustParseAddr("192.0.2.1"), IfIndex: 7}
	demoted := false

	origGW, origAdd, origDel := defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn
	origNeeded, origDemote := routeDemotionNeeded, demoteDefaultRouteFn
	t.Cleanup(func() {
		defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn = origGW, origAdd, origDel
		routeDemotionNeeded, demoteDefaultRouteFn = origNeeded, origDemote
	})

	var addCalls []netip.Prefix
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		if demoted {
			return tun.Gateway{}, fmt.Errorf("no physical default route found (simulated post-demotion)")
		}
		return fakeGW, nil
	}
	addGatewayRouteFn = func(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
		addCalls = append(addCalls, p)
		return nil
	}
	delGatewayRouteFn = func(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
		return nil
	}
	routeDemotionNeeded = true
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		demoted = true // simulate the physical route actually being deleted
		return 0, nil
	}

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)

	if !demoted {
		t.Fatal("setup: expected demotion to have run")
	}

	// A bypass-route acquisition happening strictly after demotion — what a
	// live peer connecting (or reconnecting) after full-tunnel is already
	// active hits — must still succeed via the cached gateway, not a fresh
	// (now-failing) live lookup.
	newPeerAddr := netip.MustParseAddr("198.51.100.7")
	e.acquireBypassRoute(ns, newPeerAddr)

	found := false
	for _, p := range addCalls {
		if p.Addr() == newPeerAddr {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a bypass route for %s installed via the cached physical gateway after demotion, got calls %+v", newPeerAddr, addCalls)
	}
}

// TestPhysicalGatewayCacheClearedOnWithdrawal confirms the cache doesn't
// outlive one full-tunnel activation: after withdrawal, a fresh activation
// re-resolves the physical gateway rather than reusing whatever was cached
// before, so a genuinely different physical gateway (a roam, a new lease)
// is picked up across activations rather than silently stuck on the first
// one ever observed.
func TestPhysicalGatewayCacheClearedOnWithdrawal(t *testing.T) {
	e, ns := testEngineWithNet(t)

	gwSeq := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.99")}
	call := 0

	origGW, origAdd, origDel := defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn
	origNeeded, origDemote := routeDemotionNeeded, demoteDefaultRouteFn
	t.Cleanup(func() {
		defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn = origGW, origAdd, origDel
		routeDemotionNeeded, demoteDefaultRouteFn = origNeeded, origDemote
	})

	var seenGateways []netip.Addr
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		gw := gwSeq[call]
		if call < len(gwSeq)-1 {
			call++
		}
		return tun.Gateway{Addr: gw, IfIndex: 7}, nil
	}
	addGatewayRouteFn = func(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
		seenGateways = append(seenGateways, gateway)
		return nil
	}
	delGatewayRouteFn = func(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
		return nil
	}
	routeDemotionNeeded = true
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		return 0, nil
	}

	def := netip.MustParsePrefix("0.0.0.0/0")
	peerAddr := netip.MustParseAddr("198.51.100.7")

	advertiseRoute(e, ns, "peerA", def, 10)
	e.acquireBypassRoute(ns, peerAddr)
	e.releaseBypassRoute(peerAddr)
	withdrawRouteFrom(e, ns, "peerA", def)

	advertiseRoute(e, ns, "peerA", def, 10)
	e.acquireBypassRoute(ns, peerAddr)

	if len(seenGateways) == 0 {
		t.Fatal("expected at least one bypass route installed across both activations")
	}
	// The second activation should have re-resolved and seen the updated
	// gateway, not the first activation's cached one.
	last := seenGateways[len(seenGateways)-1]
	if last != gwSeq[1] {
		t.Fatalf("expected the second activation to pick up the updated gateway %s, got %s", gwSeq[1], last)
	}
}
