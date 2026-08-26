package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// Regression tests for the blackhole fixed in v965: a peer advertises its
// LAN-side interface address as a host candidate while redistributing that
// same LAN, and the seed-bypass path pinned the address to the physical
// gateway — a /32 that wins longest-prefix-match over the mesh /24 and hands
// the packet to a router with no way back into the peer's LAN.
//
// The field shape these reproduce: grav2 advertises 10.2.2.1:65432 as a
// candidate and redistributes 10.2.2.0/24, and grav1 installs
// "10.2.2.1 via 192.168.122.1 dev eth0", blackholing the peer's own gateway
// address for every user of the host, not just for the dial that caused it.

// learnRoute records a redistributed prefix from origin the way handleRouteAdd
// does, without needing a live session to flood it in.
func learnRoute(ns *netState, origin string, p netip.Prefix) {
	ns.mu.Lock()
	ns.redist = append(ns.redist, routeEntry{origin: origin, prefix: p, metric: 0, lastSeen: time.Now()})
	ns.mu.Unlock()
}

// seedFrom registers s as a seed attributed to node, mirroring what
// addLocalCandidates does for a learned host candidate.
func seedFrom(ns *netState, s netip.AddrPort, node string) {
	ns.mu.Lock()
	ns.seeds = append(ns.seeds, s)
	ns.seedOwner[s] = node
	ns.mu.Unlock()
}

// TestSeedBypassDeclinedForOwnerRoutedAddress is the core regression: the
// address is covered by a mesh route, which is what used to be sufficient to
// pin it, but the node that advertised the address is the same node that
// routes the covering prefix — so the mesh is the path to it and no bypass
// may be installed.
func TestSeedBypassDeclinedForOwnerRoutedAddress(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	cand := netip.MustParseAddrPort("10.2.2.1:65432")
	seedFrom(ns, cand, "grav2")
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))

	// Coverage alone must still hold — otherwise this test would pass for
	// the wrong reason (nothing capturing the address at all).
	ns.osMu.Lock()
	ns.osMetric[netip.MustParsePrefix("10.2.2.0/24")] = 9000
	ns.osMu.Unlock()
	if !ns.meshRouteCovers(cand.Addr()) {
		t.Fatal("precondition: the mesh route should cover the candidate address")
	}

	e.syncSeedBypassRoutes(ns)

	for _, c := range *calls {
		if c.add && c.prefix.Addr() == cand.Addr() {
			t.Fatalf("installed a bypass /32 for an owner-routed candidate: %+v", c)
		}
	}
}

// TestSeedBypassStillAcquiredForForeignRoutedAddress is the other half of the
// discriminator, and the case v552 added the meshRouteCovers trigger for:
// peer A's endpoint happens to sit inside a prefix peer B redistributes. B's
// route has nothing to do with how this host reaches A, so it really is
// capturing traffic that belongs on the physical path and the bypass is
// correct. Narrowing the trigger must not have cost this.
func TestSeedBypassStillAcquiredForForeignRoutedAddress(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	ep := netip.MustParseAddrPort("10.2.2.9:65432")
	seedFrom(ns, ep, "grav4") // advertised by grav4...
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))

	ns.osMu.Lock()
	ns.osMetric[netip.MustParsePrefix("10.2.2.0/24")] = 9000
	ns.osMu.Unlock()

	e.syncSeedBypassRoutes(ns)

	var got bool
	for _, c := range *calls {
		if c.add && c.prefix.Addr() == ep.Addr() {
			got = true
		}
	}
	if !got {
		t.Fatalf("expected a bypass for an endpoint covered by a *different* node's prefix, got %+v", *calls)
	}
}

// TestSeedBypassDeclineIsScopedToTheCoveringPrefix guards against the filter
// being read as "decline anything this owner advertises". A second candidate
// from the same peer that falls outside every prefix that peer routes is an
// ordinary underlay address and keeps its bypass.
func TestSeedBypassDeclineIsScopedToTheCoveringPrefix(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	lan := netip.MustParseAddrPort("10.2.2.1:65432")    // inside grav2's prefix
	wan := netip.MustParseAddrPort("203.0.113.7:65432") // grav2's real endpoint
	seedFrom(ns, lan, "grav2")
	seedFrom(ns, wan, "grav2")
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))
	ns.fullTunnel.Store(true) // so the wan address is covered by something too

	e.syncSeedBypassRoutes(ns)

	var sawLAN, sawWAN bool
	for _, c := range *calls {
		if !c.add {
			continue
		}
		switch c.prefix.Addr() {
		case lan.Addr():
			sawLAN = true
		case wan.Addr():
			sawWAN = true
		}
	}
	if sawLAN {
		t.Error("pinned the owner-routed LAN candidate")
	}
	if !sawWAN {
		t.Error("declined a same-owner address that no prefix of that owner covers")
	}
}

// TestFullTunnelDoesNotOverrideTheOwnerRoutedDecline: under an accepted
// default route every underlay address is covered, and it would be easy to
// let fullTunnel short-circuit the filter. It must not — the blackhole is
// just as fatal with a /0 in the table, and the /0 is not evidence that this
// particular address is reachable off-mesh.
func TestFullTunnelDoesNotOverrideTheOwnerRoutedDecline(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	cand := netip.MustParseAddrPort("10.2.2.1:65432")
	seedFrom(ns, cand, "grav2")
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))

	e.syncSeedBypassRoutes(ns)

	for _, c := range *calls {
		if c.add && c.prefix.Addr() == cand.Addr() {
			t.Fatalf("full-tunnel overrode the owner-routed decline: %+v", c)
		}
	}
}

// TestPeerSessionBypassIgnoresOwnerRouting: the filter is deliberately scoped
// to seeds, which are unproven guesses. A live session's endpoint has already
// carried a handshake, which is direct proof a physical path exists no matter
// which prefix covers it — declining there would tear down a working peer.
func TestPeerSessionBypassIgnoresOwnerRouting(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	ep := netip.MustParseAddrPort("10.2.2.1:65432")
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))
	ns.osMu.Lock()
	ns.osMetric[netip.MustParsePrefix("10.2.2.0/24")] = 9000
	ns.osMu.Unlock()

	ps := &peerSession{net: ns, nodeID: "grav2", endpoint: ep}
	e.syncPeerBypassRoute(ns, ps)

	var got bool
	for _, c := range *calls {
		if c.add && c.prefix.Addr() == ep.Addr() {
			got = true
		}
	}
	if !got {
		t.Fatalf("a proven session endpoint lost its bypass to the seed-path filter, got %+v", *calls)
	}
}

// TestSeedBypassReleasedWhenOwnerStartsRoutingIt covers the upgrade-in-place
// ordering: the bypass is acquired first (no covering route yet), and the
// route arrives afterwards. The reconcile has to withdraw the now-wrong /32
// rather than leave it because it was legitimate when installed.
func TestSeedBypassReleasedWhenOwnerStartsRoutingIt(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)
	ns.fullTunnel.Store(true)

	cand := netip.MustParseAddrPort("10.2.2.1:65432")
	seedFrom(ns, cand, "grav2")

	e.syncSeedBypassRoutes(ns)
	if len(*calls) != 1 || !(*calls)[0].add {
		t.Fatalf("expected the bypass to be acquired before any covering route, got %+v", *calls)
	}

	// grav2 now advertises the prefix containing its own candidate.
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))
	e.syncSeedBypassRoutes(ns)

	var released bool
	for _, c := range (*calls)[1:] {
		if !c.add && c.prefix.Addr() == cand.Addr() {
			released = true
		}
	}
	if !released {
		t.Fatalf("expected the stale bypass to be withdrawn once its owner routed the prefix, got %+v", *calls)
	}
}

// TestStaleBypassSweepSkipsAddressWithLiveReference: clearStaleBypassRoute
// exists to remove a pin an older build left behind, since Engine.Stop
// releases no bypass routes and a restart therefore leaves the /32 in the
// kernel table with the competing mesh route gone. It must never remove one
// this process legitimately holds — a live peer session at the same address.
func TestStaleBypassSweepSkipsAddressWithLiveReference(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	addr := netip.MustParseAddr("10.2.2.1")
	e.bypassMu.Lock()
	e.bypassRefs[addr] = bypassRef{count: 1, gateway: netip.MustParseAddr("192.0.2.1"), ifIndex: 7}
	e.bypassMu.Unlock()

	e.clearStaleBypassRoute(ns, addr)

	if len(*calls) != 0 {
		t.Fatalf("swept a bypass route that a live tracker still holds: %+v", *calls)
	}
}

// TestStaleBypassSweepRemovesUnreferencedRoute is the same function's positive
// case: no tracker in this process holds the address, so any route out there
// is a leftover and gets deleted.
func TestStaleBypassSweepRemovesUnreferencedRoute(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	addr := netip.MustParseAddr("10.2.2.1")
	e.clearStaleBypassRoute(ns, addr)

	if len(*calls) != 1 || (*calls)[0].add {
		t.Fatalf("expected exactly one Del for the leftover pin, got %+v", *calls)
	}
	if got := (*calls)[0].prefix; got != netip.MustParsePrefix("10.2.2.1/32") {
		t.Fatalf("swept the wrong prefix: %s", got)
	}
}

// TestRoutedByOrigin exercises the predicate directly, including the two ways
// it must answer no: right prefix but wrong origin, and right origin but a
// prefix that doesn't contain the address.
func TestRoutedByOrigin(t *testing.T) {
	_, ns := testEngineWithNet(t)
	learnRoute(ns, "grav2", netip.MustParsePrefix("10.2.2.0/24"))
	learnRoute(ns, "grav3", netip.MustParsePrefix("10.3.3.0/24"))

	cases := []struct {
		name string
		addr string
		node string
		want bool
	}{
		{"owner routes the covering prefix", "10.2.2.1", "grav2", true},
		{"covered, but by another node's prefix", "10.2.2.1", "grav3", false},
		{"right owner, address outside its prefix", "203.0.113.7", "grav2", false},
		{"no attribution at all", "10.2.2.1", "", false},
		{"unknown node", "10.2.2.1", "grav9", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ns.routedByOrigin(netip.MustParseAddr(tc.addr), tc.node)
			if got != tc.want {
				t.Fatalf("routedByOrigin(%s, %q) = %v, want %v", tc.addr, tc.node, got, tc.want)
			}
		})
	}
}

// TestRoutedByOriginV6 keeps the predicate honest on the family the reporting
// bundle happened not to show: grav1's fd01::1 is the same shape as 10.1.1.1
// and was only spared a pin because the host had no IPv6 default route for
// physicalGateway to resolve.
func TestRoutedByOriginV6(t *testing.T) {
	_, ns := testEngineWithNet(t)
	learnRoute(ns, "grav2", netip.MustParsePrefix("fd02::/64"))

	if !ns.routedByOrigin(netip.MustParseAddr("fd02::1"), "grav2") {
		t.Fatal("expected an IPv6 candidate inside its owner's prefix to be owner-routed")
	}
	if ns.routedByOrigin(netip.MustParseAddr("fd04::1"), "grav2") {
		t.Fatal("matched an IPv6 address outside the owner's prefix")
	}
}
