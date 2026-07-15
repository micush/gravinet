package mesh

import (
	"net/netip"
	"testing"

	"gravinet/internal/tun"
)

// fakeGatewayCall records one AddGatewayRoute/DelGatewayRoute invocation for
// assertions.
type fakeGatewayCall struct {
	add     bool // true for Add, false for Del
	prefix  netip.Prefix
	gateway netip.Addr
	ifIndex int32
}

// withFakeGateway swaps the package-level gateway/route function vars for
// fakes recording every call, and returns a restore func plus the call log.
// The fake gateway is fixed and always succeeds — tests that need a failure
// path override defaultGatewayFn themselves after calling this.
//
// Also fakes demoteDefaultRouteFn (always succeeds, previous metric 0) and
// forces routeDemotionNeeded true, rather than leaving demotion pointed at
// the real tun.DemoteDefaultRoute — as of v317 routeDemotionNeeded is true
// on every platform, Linux included (see gateway_linux.go's doc comment),
// so without this every test here would otherwise reach out to this
// container's actual kernel routing table via real rtnetlink calls. Tests
// that specifically exercise demotion behavior call withFakeDemotion after
// this to install their own call-tracking fake on top; t.Cleanup unwinds in
// LIFO order, so that override's own cleanup restores back to this
// function's simple fake, and this function's cleanup then restores the
// real tun functions, leaving nothing leaked either way.
func withFakeGateway(t *testing.T) *[]fakeGatewayCall {
	t.Helper()
	var calls []fakeGatewayCall
	fakeGW := tun.Gateway{Addr: netip.MustParseAddr("192.0.2.1"), IfIndex: 7, Metric: 0}

	origGW, origAdd, origDel := defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn
	origNeeded, origDemote := routeDemotionNeeded, demoteDefaultRouteFn
	defaultGatewayFn = func(family int, excludeIfIndex int32) (tun.Gateway, error) {
		return fakeGW, nil
	}
	addGatewayRouteFn = func(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
		calls = append(calls, fakeGatewayCall{add: true, prefix: p, gateway: gateway, ifIndex: ifIndex})
		return nil
	}
	delGatewayRouteFn = func(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
		calls = append(calls, fakeGatewayCall{add: false, prefix: p, gateway: gateway, ifIndex: ifIndex})
		return nil
	}
	routeDemotionNeeded = true
	demoteDefaultRouteFn = func(family int, excludeIfIndex int32, newMetric int) (int, error) {
		return 0, nil
	}
	t.Cleanup(func() {
		defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn = origGW, origAdd, origDel
		routeDemotionNeeded, demoteDefaultRouteFn = origNeeded, origDemote
	})
	return &calls
}

func TestSyncPeerBypassRouteFullTunnelOff(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(false) // explicit: this is the default, but be clear about what's under test

	ps := &peerSession{net: ns, endpoint: netip.MustParseAddrPort("203.0.113.5:51820")}
	e.syncPeerBypassRoute(ns, ps)

	if len(*calls) != 0 {
		t.Fatalf("expected no route calls while fullTunnel is off, got %+v", *calls)
	}
	if ps.bypassAddr.IsValid() {
		t.Fatalf("expected no bypassAddr recorded, got %s", ps.bypassAddr)
	}
}

func TestSyncPeerBypassRouteInstallsHostRoute(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	want := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{net: ns, endpoint: want}
	e.syncPeerBypassRoute(ns, ps)

	if len(*calls) != 1 || !(*calls)[0].add {
		t.Fatalf("expected exactly one Add call, got %+v", *calls)
	}
	c := (*calls)[0]
	if c.prefix != netip.PrefixFrom(want.Addr(), 32) {
		t.Fatalf("installed prefix %s, want %s/32", c.prefix, want.Addr())
	}
	if c.ifIndex != 7 { // from withFakeGateway's fakeGW
		t.Fatalf("installed via ifindex %d, want 7", c.ifIndex)
	}
	if ps.bypassAddr != want.Addr() {
		t.Fatalf("bypassAddr = %s, want %s", ps.bypassAddr, want.Addr())
	}

	// Calling again with nothing changed must be a no-op — reconciling
	// against ps.bypassAddr, not blindly re-adding every time.
	e.syncPeerBypassRoute(ns, ps)
	if len(*calls) != 1 {
		t.Fatalf("expected sync with no change to be a no-op, got %d total calls", len(*calls))
	}
}

func TestSyncPeerBypassRouteFollowsRoam(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	first := netip.MustParseAddrPort("203.0.113.5:51820")
	second := netip.MustParseAddrPort("203.0.113.9:51820") // roamed to a genuinely different address
	ps := &peerSession{net: ns, endpoint: first}
	e.syncPeerBypassRoute(ns, ps)

	ps.mu.Lock()
	ps.endpoint = second
	ps.mu.Unlock()
	e.syncPeerBypassRoute(ns, ps)

	if len(*calls) != 3 {
		t.Fatalf("expected Add(first), then Del(first)+Add(second) for the roam, got %+v", *calls)
	}
	if !(*calls)[0].add || (*calls)[1].add || !(*calls)[2].add {
		t.Fatalf("expected Add, Del, Add, got %+v", *calls)
	}
	if (*calls)[1].prefix.Addr() != first.Addr() || (*calls)[2].prefix.Addr() != second.Addr() {
		t.Fatalf("Del/Add targeted the wrong addresses: %+v", *calls)
	}
	if ps.bypassAddr != second.Addr() {
		t.Fatalf("bypassAddr = %s, want %s", ps.bypassAddr, second.Addr())
	}
}

// TestSyncPeerBypassRouteSamePortChangeIsNoop confirms a port-only NAT rebind
// (the common case — same public IP, new mapped port) doesn't touch the
// route at all: a /32 host route is IP-only, so there's nothing to update.
func TestSyncPeerBypassRouteSamePortChangeIsNoop(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	ip := netip.MustParseAddr("203.0.113.5")
	ps := &peerSession{net: ns, endpoint: netip.AddrPortFrom(ip, 51820)}
	e.syncPeerBypassRoute(ns, ps)
	if len(*calls) != 1 {
		t.Fatalf("setup: expected one Add call, got %+v", *calls)
	}

	ps.mu.Lock()
	ps.endpoint = netip.AddrPortFrom(ip, 60000) // same IP, NAT-rebound port
	ps.mu.Unlock()
	e.syncPeerBypassRoute(ns, ps)

	if len(*calls) != 1 {
		t.Fatalf("a port-only change shouldn't touch the /32 route, got %+v", *calls)
	}
}

func TestSyncPeerBypassRouteSkipsRelayedPeer(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	relay := &peerSession{net: ns, endpoint: netip.MustParseAddrPort("198.51.100.1:51820")}
	ps := &peerSession{net: ns, endpoint: netip.MustParseAddrPort("203.0.113.5:51820"), relay: relay}
	e.syncPeerBypassRoute(ns, ps)

	if len(*calls) != 0 {
		t.Fatalf("a relayed peer's own endpoint isn't dialed directly, expected no route calls, got %+v", *calls)
	}
}

func TestSyncPeerBypassRouteWithdrawsOnTransitionToRelayed(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	ep := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{net: ns, endpoint: ep}
	e.syncPeerBypassRoute(ns, ps) // direct: installs a route

	relay := &peerSession{net: ns, endpoint: netip.MustParseAddrPort("198.51.100.1:51820")}
	ps.mu.Lock()
	ps.relay = relay // now reached via a relay instead
	ps.mu.Unlock()
	e.syncPeerBypassRoute(ns, ps)

	if len(*calls) != 2 || (*calls)[0].add != true || (*calls)[1].add != false {
		t.Fatalf("expected Add then Del once the peer became relayed, got %+v", *calls)
	}
	if ps.bypassAddr.IsValid() {
		t.Fatalf("expected bypassAddr cleared once relayed, got %s", ps.bypassAddr)
	}
}

func TestRemovePeerBypassRoute(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	ep := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{net: ns, endpoint: ep}
	e.syncPeerBypassRoute(ns, ps)
	if len(*calls) != 1 {
		t.Fatalf("setup: expected one Add call, got %+v", *calls)
	}

	e.removePeerBypassRoute(ns, ps)
	if len(*calls) != 2 || (*calls)[1].add {
		t.Fatalf("expected a Del call from removePeerBypassRoute, got %+v", *calls)
	}
	if ps.bypassAddr.IsValid() {
		t.Fatalf("expected bypassAddr cleared, got %s", ps.bypassAddr)
	}

	// Removing again (already gone) must be a harmless no-op, not a second Del.
	e.removePeerBypassRoute(ns, ps)
	if len(*calls) != 2 {
		t.Fatalf("expected removing an already-clear bypass route to be a no-op, got %+v", *calls)
	}
}

func TestPhysicalGatewayErrorsWithoutDevice(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	ns.spec.Dev = nil // no overlay device configured on this network

	if _, _, err := e.physicalGateway(ns, netip.MustParseAddr("203.0.113.5")); err == nil {
		t.Fatal("expected an error when the network has no overlay device configured")
	}
}

// TestBypassRouteRefcountSurvivesOneOwnerReleasing is the scenario the
// ref-counted redesign exists for: a seed and the peer session it becomes
// both hold a reference to the same address for a while (the seed entry
// hasn't been pruned yet), and releasing either one alone must not delete
// the route the other still needs.
func TestBypassRouteRefcountSurvivesOneOwnerReleasing(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	addr := netip.MustParseAddr("203.0.113.5")
	e.acquireBypassRoute(ns, addr) // "seed" reference
	e.acquireBypassRoute(ns, addr) // "peer session" reference, same address

	if len(*calls) != 1 {
		t.Fatalf("expected exactly one Add for two references to the same address, got %+v", *calls)
	}

	e.releaseBypassRoute(addr) // release the "seed" side only
	if len(*calls) != 1 {
		t.Fatalf("releasing one of two references must not delete the route yet, got %+v", *calls)
	}

	e.releaseBypassRoute(addr) // release the "peer session" side too
	if len(*calls) != 2 || (*calls)[1].add {
		t.Fatalf("expected a Del once the last reference released, got %+v", *calls)
	}
}

func TestSyncSeedBypassRoutesAcquiresAndReleases(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	a := netip.MustParseAddrPort("203.0.113.5:51820")
	b := netip.MustParseAddrPort("203.0.113.9:51820")
	ns.mu.Lock()
	ns.seeds = []netip.AddrPort{a, b}
	ns.mu.Unlock()

	e.syncSeedBypassRoutes(ns)
	if len(*calls) != 2 {
		t.Fatalf("expected an Add per distinct seed address, got %+v", *calls)
	}

	// Re-syncing the same seed list must be a no-op.
	e.syncSeedBypassRoutes(ns)
	if len(*calls) != 2 {
		t.Fatalf("expected re-syncing an unchanged seed list to be a no-op, got %d calls", len(*calls))
	}

	// Dropping seed b must release its route; a stays untouched.
	ns.mu.Lock()
	ns.seeds = []netip.AddrPort{a}
	ns.mu.Unlock()
	e.syncSeedBypassRoutes(ns)

	if len(*calls) != 3 || (*calls)[2].add || (*calls)[2].prefix.Addr() != b.Addr() {
		t.Fatalf("expected a Del for the dropped seed b, got %+v", *calls)
	}
}

func TestSyncSeedBypassRoutesDedupesSameAddressDifferentPort(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	// A UDP seed and its resolved TCP/TLS fallback share an IP but not a
	// port (see seedFallback's doc comment) — that's one /32, not two.
	ns.mu.Lock()
	ns.seeds = []netip.AddrPort{netip.MustParseAddrPort("203.0.113.5:51820")}
	ns.tcpSeeds = []netip.AddrPort{netip.MustParseAddrPort("203.0.113.5:443")}
	ns.mu.Unlock()

	e.syncSeedBypassRoutes(ns)
	if len(*calls) != 1 {
		t.Fatalf("expected one Add for one distinct address regardless of port, got %+v", *calls)
	}
}

// TestSeedToPeerHandoffNeverDropsTheRoute exercises the actual collision
// this whole refactor was for, through the real code paths rather than
// acquireBypassRoute/releaseBypassRoute directly: a seed is bypass-routed
// before any session exists; the session then completes and takes its own
// reference; the (now-stale) seed entry is pruned and releases its
// reference — and the route must still be up throughout, since the peer
// session's own reference never went away.
func TestSeedToPeerHandoffNeverDropsTheRoute(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	ep := netip.MustParseAddrPort("203.0.113.5:51820")
	ns.mu.Lock()
	ns.seeds = []netip.AddrPort{ep}
	ns.mu.Unlock()
	e.syncSeedBypassRoutes(ns) // seed reference acquired

	ps := &peerSession{net: ns, endpoint: ep}
	e.syncPeerBypassRoute(ns, ps) // peer session's own reference acquired, same address

	if len(*calls) != 1 {
		t.Fatalf("two references to the same address from seed+peer must still be one Add, got %+v", *calls)
	}

	// The seed is now stale (superseded by the live session) and gets pruned.
	ns.mu.Lock()
	ns.seeds = nil
	ns.mu.Unlock()
	e.syncSeedBypassRoutes(ns) // releases the seed's reference

	if len(*calls) != 1 {
		t.Fatalf("releasing the seed's reference alone must not delete the route the peer session still holds, got %+v", *calls)
	}

	// Only once the peer session itself goes away should the route actually go.
	e.removePeerBypassRoute(ns, ps)
	if len(*calls) != 2 || (*calls)[1].add {
		t.Fatalf("expected a Del once the peer session's own reference was also released, got %+v", *calls)
	}
}
