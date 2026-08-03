package mesh

import (
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gravinet/internal/logx"
	"gravinet/internal/tun"
)

// A bypass route steers traffic for an address AROUND the tunnel, out the
// physical gateway. That is right for a peer's underlay endpoint and
// catastrophic for a peer's overlay address: the mesh address gets pinned to
// the LAN gateway, which has no idea what it is, and the peer is blackholed
// at the routing layer while every mesh-level indicator stays green — session
// up, counters clean, inbound packets still arriving. Only the replies are
// lost, because they leave by the wrong interface.
//
// From the field: a macOS node answered 0 of 16 echo requests from one peer
// while replying to all twelve others. Its routing table held two host routes
// for that peer's overlay address, and the physical-gateway one won:
//
//	192.168.203.111      192.168.193.1   UGHS   en1
//	192.168.203.111/32   utun0           USc    utun0
//
// Its own log showed addSeed refusing that same address as an endpoint
// ("overlay (mesh) address, not a reachable underlay endpoint") 69 seconds
// before acquireProvenBypassRoute installed a route for it. isOverlayAddr
// existed and said exactly what to do; only one of the two paths asked it.

// overlayBypassEnv wires an Engine with one overlay subnet and stubs the
// gateway backend, returning the recorders for installs and deletes.
func overlayBypassEnv(t *testing.T) (e *Engine, ns *netState, installed *[]netip.Prefix, deleted *[]netip.Prefix, mu *sync.Mutex) {
	t.Helper()
	if !gatewaySupported {
		t.Skip("no gateway backend on this platform; bypass is impossible here by design")
	}
	var m sync.Mutex
	var ins, del []netip.Prefix

	origGW, origAdd, origDel := defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn
	defaultGatewayFn = func(int, int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: netip.MustParseAddr("192.168.193.1"), IfIndex: 3}, nil
	}
	addGatewayRouteFn = func(p netip.Prefix, _ netip.Addr, _ int32, _ int) error {
		m.Lock()
		ins = append(ins, p)
		m.Unlock()
		return nil
	}
	delGatewayRouteFn = func(p netip.Prefix, _ netip.Addr, _ int32, _ int) error {
		m.Lock()
		del = append(del, p)
		m.Unlock()
		return nil
	}
	t.Cleanup(func() { defaultGatewayFn, addGatewayRouteFn, delGatewayRouteFn = origGW, origAdd, origDel })

	e = &Engine{log: logx.New(io.Discard, logx.LevelDebug), bypassRefs: map[netip.Addr]bypassRef{}}
	ns = &netState{physicalGW: map[int]physicalGWCache{}}
	ns.spec.ID = 0xeb1c2a7e984f072e
	ns.spec.Dev = newFakeDev("mesh0")
	ns.subnet4 = netip.MustParsePrefix("192.168.203.0/24")
	ns.subnet6 = netip.MustParsePrefix("fd00:203::/64")

	// isOverlayAddr reads e.netSnapshot(), so the network has to be published.
	nm := map[uint64]*netState{ns.spec.ID: ns}
	e.nets.Store(&nm)

	return e, ns, &ins, &del, &m
}

// TestProvenBypassRefusesOverlayAddress is the bug: the loop guard proved a
// route was capturing traffic for the address, and installed a bypass without
// ever asking whether the address was one of ours.
func TestProvenBypassRefusesOverlayAddress(t *testing.T) {
	e, ns, installed, _, mu := overlayBypassEnv(t)

	e.noteUnderlayLoop(ns, netip.MustParseAddr("192.168.203.111")) // mcfed's overlay address

	mu.Lock()
	got := append([]netip.Prefix(nil), *installed...)
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("installed %v for an overlay address — that pins a mesh address to the physical gateway and blackholes the peer", got)
	}
	if _, held := e.bypassRefs[netip.MustParseAddr("192.168.203.111")]; held {
		t.Fatal("recorded a bypass reference for an overlay address")
	}
}

// The v6 side has to be refused on the same terms; it only escaped in the
// field because no bypass happened to be installed for it.
func TestProvenBypassRefusesOverlayAddressV6(t *testing.T) {
	e, ns, installed, _, mu := overlayBypassEnv(t)

	e.noteUnderlayLoop(ns, netip.MustParseAddr("fd00:203::111"))

	mu.Lock()
	n := len(*installed)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("installed %d routes for an overlay v6 address, want 0", n)
	}
}

// TestProvenBypassStillWorksForUnderlayAddress: the guard must not disarm the
// mechanism it sits in front of. The field case it exists for — a peer's real
// underlay endpoint swallowed by a redistributed prefix — still gets its
// route.
func TestProvenBypassStillWorksForUnderlayAddress(t *testing.T) {
	e, ns, installed, _, mu := overlayBypassEnv(t)

	peer := netip.MustParseAddr("192.168.5.106") // underlay, outside 192.168.203.0/24
	e.noteUnderlayLoop(ns, peer)

	mu.Lock()
	got := append([]netip.Prefix(nil), *installed...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("installed %v, want exactly one host route for %v", got, peer)
	}
	if got[0].Addr() != peer || got[0].Bits() != peer.BitLen() {
		t.Fatalf("installed %v, want a host route for %v exactly", got[0], peer)
	}
}

// TestAcquireBypassRefusesOverlayAddress: the ordinary acquire path takes its
// addresses from peer endpoints and seeds, both already filtered — so reaching
// it with an overlay address means a filter was bypassed, and refusing is
// strictly better than a silent blackhole.
func TestAcquireBypassRefusesOverlayAddress(t *testing.T) {
	e, ns, installed, _, mu := overlayBypassEnv(t)
	ns.fullTunnel.Store(true) // would otherwise decline for an unrelated reason

	e.acquireBypassRoute(ns, netip.MustParseAddr("192.168.203.140"))

	mu.Lock()
	n := len(*installed)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("installed %d routes for an overlay address under full-tunnel, want 0", n)
	}
}

// TestDropOverlayBypassRoutesWithdrawsStale covers the upgrade path. A node
// coming from the previous build can be holding a route acquired before the
// guard existed; the sweep has to take it back out, and the delete must be the
// exact route that was installed.
func TestDropOverlayBypassRoutesWithdrawsStale(t *testing.T) {
	e, ns, _, deleted, mu := overlayBypassEnv(t)

	stale := netip.MustParseAddr("192.168.203.111")
	good := netip.MustParseAddr("192.168.5.106")
	gw := netip.MustParseAddr("192.168.193.1")
	e.bypassRefs[stale] = bypassRef{count: 1, gateway: gw, ifIndex: 3}
	e.bypassRefs[good] = bypassRef{count: 1, gateway: gw, ifIndex: 3}

	e.dropOverlayBypassRoutes(ns)

	mu.Lock()
	del := append([]netip.Prefix(nil), *deleted...)
	mu.Unlock()
	if len(del) != 1 || del[0].Addr() != stale {
		t.Fatalf("deleted %v, want exactly the overlay address %v", del, stale)
	}
	if _, held := e.bypassRefs[stale]; held {
		t.Error("stale overlay reference survived the sweep")
	}
	if _, held := e.bypassRefs[good]; !held {
		t.Error("the sweep dropped a legitimate underlay bypass reference")
	}
}

// The sweep runs on every initLoop tick, so it must be a no-op when there is
// nothing wrong — no syscalls, no log noise.
func TestDropOverlayBypassRoutesNoopWhenClean(t *testing.T) {
	e, ns, _, deleted, mu := overlayBypassEnv(t)
	e.bypassRefs[netip.MustParseAddr("192.168.5.106")] = bypassRef{count: 1, gateway: netip.MustParseAddr("192.168.193.1"), ifIndex: 3}

	e.dropOverlayBypassRoutes(ns)
	e.dropOverlayBypassRoutes(ns)

	mu.Lock()
	n := len(*deleted)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("made %d delete calls with nothing stale to remove", n)
	}
}

// TestOrphanedOverlayBypassWarnThrottled: a route left by an earlier process
// cannot be enumerated or safely deleted (no route-query primitive, and BSD
// RTM_DELETE matches on destination, so deleting by prefix could take out the
// legitimate tunnel route instead). It is reported instead — but the trigger
// is per-packet and the condition is permanent until someone acts, so the
// message that says what to do must not bury itself.
func TestOrphanedOverlayBypassWarnThrottled(t *testing.T) {
	var lines atomic.Int64
	e, ns, _, _, _ := overlayBypassEnv(t)
	e.log = logx.New(countingWriter{&lines}, logx.LevelDebug)

	addr := netip.MustParseAddr("192.168.203.111")
	for i := 0; i < 50; i++ {
		e.noteOrphanedOverlayBypass(ns, addr)
	}
	if got := lines.Load(); got != 1 {
		t.Fatalf("logged %d times for 50 hits on the same address, want 1", got)
	}

	// A different address is different news and must not be swallowed.
	e.noteOrphanedOverlayBypass(ns, netip.MustParseAddr("192.168.203.140"))
	if got := lines.Load(); got != 2 {
		t.Fatalf("logged %d times, want 2 — a second address is separate news", got)
	}
}

type countingWriter struct{ n *atomic.Int64 }

func (w countingWriter) Write(p []byte) (int, error) {
	w.n.Add(1)
	return len(p), nil
}

// The bypass route was the symptom. The cause is one endpoint-learning path
// with no overlay filter: touch() follows the observed source of an inbound
// datagram for NAT roaming, and until now would happily roam a session onto
// one of this node's own overlay addresses.
//
// That happens when the far side's underlay datagram is itself captured by a
// route into its tunnel — the packet egresses over the overlay and arrives
// carrying that peer's mesh address as its source. Following it points every
// subsequent underlay send at a mesh address, this node's routes hand those
// back into its own tunnel, and the loop guard then installs a bypass that
// pins the mesh address to the physical gateway.
//
// The codebase already knew: the peer-cache filter in ban.go says it exists
// partly to catch "a session that roamed onto an overlay source". It stopped
// the poisoned endpoint reaching disk and left the live one in memory, which
// is the copy that feeds isUnderlayLoop.

func TestTouchRefusesRoamOntoOverlaySource(t *testing.T) {
	e, ns, _, _, _ := overlayBypassEnv(t)
	good := netip.MustParseAddrPort("192.168.193.26:65432")
	ps := &peerSession{nodeID: "mcfed", net: ns, endpoint: good}
	ps.initPMTU(1280, 1450)

	// mcfed's datagram arrives with its OVERLAY address as the source.
	roamed := ps.touch(e, netip.MustParseAddrPort("192.168.203.111:55804"), nil)

	if roamed {
		t.Error("reported a roam onto an overlay source")
	}
	if got := ps.ep(); got != good {
		t.Fatalf("endpoint = %v, want the last working underlay endpoint %v — roaming onto a mesh address points every later send into this node's own tunnel", got, good)
	}
	if n := ps.overlayRoamRefused.Load(); n != 1 {
		t.Errorf("overlayRoamRefused = %d, want 1 — the condition is invisible without it", n)
	}
}

// Liveness must still be recorded: the packet authenticated, so the peer is
// demonstrably alive even though its source address is unusable. Treating the
// refusal as "didn't hear from them" would expire a healthy session.
func TestTouchRefusedRoamStillCountsAsLiveness(t *testing.T) {
	e, ns, _, _, _ := overlayBypassEnv(t)
	ps := &peerSession{nodeID: "mcfed", net: ns, endpoint: netip.MustParseAddrPort("192.168.193.26:65432")}
	ps.initPMTU(1280, 1450)
	ps.mu.Lock()
	ps.lastRx = time.Now().Add(-time.Hour)
	ps.mu.Unlock()

	ps.touch(e, netip.MustParseAddrPort("192.168.203.111:55804"), nil)

	ps.mu.Lock()
	age := time.Since(ps.lastRx)
	ps.mu.Unlock()
	if age > time.Minute {
		t.Fatalf("lastRx is %v old — a refused roam still proves the peer is alive", age)
	}
}

// A genuine NAT rebind must still be followed. The guard only ever rejects a
// source that could not have carried traffic anyway.
func TestTouchStillRoamsOntoUnderlaySource(t *testing.T) {
	e, ns, _, _, _ := overlayBypassEnv(t)
	ps := &peerSession{nodeID: "mcfed", net: ns, endpoint: netip.MustParseAddrPort("192.168.193.26:65432")}
	ps.initPMTU(1280, 1450)

	moved := netip.MustParseAddrPort("198.51.100.9:41000")
	if roamed := ps.touch(e, moved, nil); !roamed {
		t.Fatal("refused a legitimate NAT roam onto an underlay address")
	}
	if got := ps.ep(); got != moved {
		t.Fatalf("endpoint = %v, want %v", got, moved)
	}
	if n := ps.overlayRoamRefused.Load(); n != 0 {
		t.Errorf("overlayRoamRefused = %d for an underlay roam, want 0", n)
	}
}

// A relayed packet carries the relay's source, not the peer's, and touch
// already declines to roam on those. The overlay guard must not change that
// path or start counting against it.
func TestTouchRelayedPacketUnaffected(t *testing.T) {
	e, ns, _, _, _ := overlayBypassEnv(t)
	ep := netip.MustParseAddrPort("192.168.193.26:65432")
	ps := &peerSession{nodeID: "mcfed", net: ns, endpoint: ep}
	ps.initPMTU(1280, 1450)
	relay := &peerSession{nodeID: "relay", net: ns}

	if roamed := ps.touch(e, netip.MustParseAddrPort("192.168.203.111:55804"), relay); roamed {
		t.Error("roamed on a relayed packet")
	}
	if got := ps.ep(); got != ep {
		t.Fatalf("endpoint = %v, want %v unchanged", got, ep)
	}
	if n := ps.overlayRoamRefused.Load(); n != 0 {
		t.Errorf("overlayRoamRefused = %d, want 0 — a relayed source is a different case", n)
	}
	if ps.relay != relay {
		t.Error("relay path was not refreshed")
	}
}
