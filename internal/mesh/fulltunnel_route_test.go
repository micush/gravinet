package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// advertiseRoute directly appends a redistributed-route entry the way
// onRouteAdd's accept path does (see routes.go), then syncs it — a lower-level
// way to drive syncRoute/syncFullTunnelRoute without going through the real
// wire protocol, for tests that only care about the OS-table/fullTunnel
// outcome. withdrawRouteFrom removes it the same way, driving the withdrawal
// path.
func advertiseRoute(e *Engine, ns *netState, origin string, p netip.Prefix, metric int) {
	ns.mu.Lock()
	ns.redist = append(ns.redist, routeEntry{origin: origin, prefix: p, metric: metric, lastSeen: time.Now()})
	ns.mu.Unlock()
	e.syncRoute(ns, p)
}

func withdrawRouteFrom(e *Engine, ns *netState, origin string, p netip.Prefix) {
	ns.mu.Lock()
	kept := ns.redist[:0]
	for _, re := range ns.redist {
		if re.origin == origin && re.prefix == p {
			continue
		}
		kept = append(kept, re)
	}
	ns.redist = kept
	ns.mu.Unlock()
	e.syncRoute(ns, p)
}

// TestSyncFullTunnelRouteInstallsLiteralDefault confirms an accepted default
// route is installed literally (0.0.0.0/0, at its advertised metric) rather
// than split into two /1s. A literal /0 is safe here specifically because
// fulltunnel.go's /32 peer-bypass routes already win longest-prefix-match
// against it — see syncFullTunnelRoute's doc comment for the full reasoning
// (an earlier version of this code split it instead, on a since-corrected
// assumption that the split was needed for peer safety, not just to dodge
// unrelated DHCP/NetworkManager route churn).
func TestSyncFullTunnelRouteInstallsLiteralDefault(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)

	if !dev.hasRoute(def) {
		t.Fatal("expected the literal 0.0.0.0/0 to be installed")
	}
	if got := dev.metricOf(def); got != 10 {
		t.Fatalf("default route installed at metric %d, want 10", got)
	}
	if !ns.fullTunnel.Load() {
		t.Fatal("expected ns.fullTunnel to be true once a default route is accepted and installed")
	}
}

func TestSyncFullTunnelRouteIPv6(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	def := netip.MustParsePrefix("::/0")
	advertiseRoute(e, ns, "peerA", def, 0)

	if !dev.hasRoute(def) {
		t.Fatal("expected the literal ::/0 to be installed")
	}
	if dev.hasRoute(netip.MustParsePrefix("0.0.0.0/0")) {
		t.Fatal("a v6 default route must not install a v4 one")
	}
}

func TestSyncFullTunnelRouteWithdrawal(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)
	if !ns.fullTunnel.Load() {
		t.Fatal("setup: expected fullTunnel true after advertisement")
	}

	withdrawRouteFrom(e, ns, "peerA", def)

	if dev.hasRoute(def) {
		t.Fatal("expected the default route to be removed after withdrawal")
	}
	if ns.fullTunnel.Load() {
		t.Fatal("expected ns.fullTunnel to be false after the default route was withdrawn")
	}
}

func TestSyncFullTunnelRouteMetricChange(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)
	// Re-advertise at a better (lower) metric — matches onRouteAdd's own
	// metric-change path, which calls syncRoute again for the same prefix.
	ns.mu.Lock()
	for i := range ns.redist {
		if ns.redist[i].origin == "peerA" && ns.redist[i].prefix == def {
			ns.redist[i].metric = 5
		}
	}
	ns.mu.Unlock()
	e.syncRoute(ns, def)

	if !dev.hasRoute(def) {
		t.Fatal("expected the default route to still be installed after a metric change")
	}
	if got := dev.metricOf(def); got != 5 {
		t.Fatalf("default route metric = %d after change, want 5", got)
	}
}

// TestSyncFullTunnelRouteRefusesWithoutGatewaySupport is the guard this whole
// design depends on: the default route is a plain on-link route, installable
// via AddRoute on every platform regardless of whether that platform's
// bypass-route backend (the safety net that keeps it from looping the mesh's
// own traffic into itself) actually exists yet. Accepting a full-tunnel
// default on a platform without one must refuse outright, not install the
// dangerous half with the protective half silently absent.
func TestSyncFullTunnelRouteRefusesWithoutGatewaySupport(t *testing.T) {
	e, ns := testEngineWithNet(t)
	withFakeGateway(t)
	dev := ns.spec.Dev.(*fakeDev)

	orig := gatewaySupported
	gatewaySupported = false
	t.Cleanup(func() { gatewaySupported = orig })

	def := netip.MustParsePrefix("0.0.0.0/0")
	advertiseRoute(e, ns, "peerA", def, 10)

	if dev.hasRoute(def) {
		t.Fatal("expected the default route NOT to be installed without gateway-route support on this platform")
	}
	if ns.fullTunnel.Load() {
		t.Fatal("expected ns.fullTunnel to stay false when the platform can't back it")
	}
}

// TestSyncFullTunnelRouteBackfillsExistingSessions proves the ordering this
// whole feature depends on: a peer session that existed *before* full-tunnel
// turned on must get its bypass route the moment it turns on, not wait for
// its next roam or re-handshake — otherwise there's a real window where the
// default route is up but that peer's own underlay traffic has no escape
// hatch.
func TestSyncFullTunnelRouteBackfillsExistingSessions(t *testing.T) {
	e, ns := testEngineWithNet(t)
	calls := withFakeGateway(t)

	ep := netip.MustParseAddrPort("203.0.113.5:51820")
	ps := &peerSession{nodeID: "peerA", net: ns, endpoint: ep}
	ns.mu.Lock()
	ns.byNode["peerA"] = ps
	ns.mu.Unlock()

	if len(*calls) != 0 {
		t.Fatalf("setup: expected no bypass-route calls before full-tunnel turns on, got %+v", *calls)
	}

	advertiseRoute(e, ns, "peerA", netip.MustParsePrefix("0.0.0.0/0"), 10)

	if len(*calls) != 1 || !(*calls)[0].add || (*calls)[0].prefix.Addr() != ep.Addr() {
		t.Fatalf("expected the pre-existing session to get a backfilled bypass route, got %+v", *calls)
	}
	if ps.bypassAddr != ep.Addr() {
		t.Fatalf("expected ps.bypassAddr to be set by the backfill, got %s", ps.bypassAddr)
	}
}

// TestFullTunnelRouteEndToEnd proves the whole path works through the real
// gossip/wire protocol, not just via direct syncRoute calls: two real
// engines over real UDP transport, A advertising 0.0.0.0/0, B accepting it
// (the mesh engine itself has no built-in reject default — see
// config.NewNetworkDefaults for where that policy actually lives — so an
// unconfigured NetSpec.RouteReject here already accepts everything, the same
// as TestRedistributedRouteInstalledInOS relies on for an ordinary prefix).
//
// withFakeGateway matters here specifically (unlike some other real-engine
// tests) because B accepting a default route drives both the peer-bypass
// route machinery (acquireBypassRoute, via the real defaultGatewayFn/
// addGatewayRouteFn without it) and, as of v317, demotePhysicalDefaultRoute
// — the latter would otherwise reprogram whatever machine actually runs
// this test suite's real physical default route via genuine rtnetlink
// calls, not just gravinet's own fake in-memory tun device (B.dev below).
func TestFullTunnelRouteEndToEnd(t *testing.T) {
	const netID = uint64(0x60012)
	key, _ := crypto.GenerateKey()
	def := netip.MustParsePrefix("0.0.0.0/0")
	withFakeGateway(t)

	A := spinWithRoutes(t, "A", netID, key, netip.MustParseAddr("10.7.1.1"), []netip.Prefix{def})
	B := spinWithRoutes(t, "B", netID, key, netip.MustParseAddr("10.7.1.2"), nil)
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

	if !waitUntil(10*time.Second, func() bool { return B.dev.hasRoute(def) }) {
		t.Fatal("B did not install the literal default route after learning A's advertised default route")
	}
	bns := B.eng.netSnapshot()[netID]
	if !bns.fullTunnel.Load() {
		t.Fatal("expected B's ns.fullTunnel to be true once it accepted A's advertised default route")
	}

	// A withdraws -> B should remove the default route and turn fullTunnel back off.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Routes: nil}); err != nil {
		t.Fatalf("withdraw reload: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { return !B.dev.hasRoute(def) }) {
		t.Fatal("B did not remove the default route after A withdrew it")
	}
	if bns.fullTunnel.Load() {
		t.Fatal("expected B's ns.fullTunnel to be false after the default route was withdrawn")
	}
}
