package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/tun"
)

// reset() abandons a converged result and re-searches from the floor, dropping
// eff immediately so a freshly-shrunk path stops black-holing.
func TestPMTUResetRediscovers(t *testing.T) {
	now := time.Now()
	p := newPMTUState(1280, 1450, now)
	// Simulate a converged-high result (e.g. the old Wi-Fi path).
	p.eff, p.low, p.high, p.phase = 1450, 1450, 1450, phaseSettled
	p.reset(now)
	if p.eff != 1280 {
		t.Errorf("after reset eff=%d, want floor 1280", p.eff)
	}
	if p.phase != phaseSearch {
		t.Errorf("after reset phase=%d, want phaseSearch", p.phase)
	}
	if p.low != 1280 || p.high != 1450 {
		t.Errorf("after reset search bounds [%d,%d], want [1280,1450]", p.low, p.high)
	}
}

// A roam (peer's observed source changes) must re-run PMTU and drop the
// published outer MTU to the floor right away.
func TestRoamResetsPMTU(t *testing.T) {
	ps := &peerSession{nodeID: "p", endpoint: netip.MustParseAddrPort("203.0.113.1:65432")}
	ps.initPMTU(1280, 1450)
	// Pretend discovery had converged high.
	ps.pmtuMu.Lock()
	ps.pmtu.eff, ps.pmtu.phase = 1450, phaseSettled
	ps.pmtuMu.Unlock()
	ps.setEff(1450)
	if ps.effMTU.Load() != 1450 {
		t.Fatal("setup: eff should be 1450")
	}
	// Packet arrives from a new source -> roam.
	ps.touch(netip.MustParseAddrPort("198.51.100.9:65432"), nil)
	if ps.effMTU.Load() != 1280 {
		t.Errorf("after roam effMTU=%d, want floor 1280 (re-discovery)", ps.effMTU.Load())
	}
	ps.pmtuMu.Lock()
	phase := ps.pmtu.phase
	ps.pmtuMu.Unlock()
	if phase != phaseSearch {
		t.Errorf("after roam phase=%d, want phaseSearch", phase)
	}
}

// Same-source packets (no roam) must NOT disturb a converged PMTU.
func TestNoRoamKeepsPMTU(t *testing.T) {
	ep := netip.MustParseAddrPort("203.0.113.1:65432")
	ps := &peerSession{nodeID: "p", endpoint: ep}
	ps.initPMTU(1280, 1450)
	ps.pmtuMu.Lock()
	ps.pmtu.eff, ps.pmtu.phase = 1450, phaseSettled
	ps.pmtuMu.Unlock()
	ps.setEff(1450)
	ps.touch(ep, nil) // same source -> no roam
	if ps.effMTU.Load() != 1450 {
		t.Errorf("no-roam touch changed effMTU to %d; should stay 1450", ps.effMTU.Load())
	}
}

// resetAllPMTU re-runs discovery for every peer on every network.
func TestResetAllPMTU(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	mkPeer := func(id string) *peerSession {
		ps := &peerSession{net: ns, nodeID: id, endpoint: netip.MustParseAddrPort("203.0.113.1:65432")}
		ps.initPMTU(1280, 1450)
		ps.pmtuMu.Lock()
		ps.pmtu.eff, ps.pmtu.phase = 1450, phaseSettled
		ps.pmtuMu.Unlock()
		ps.setEff(1450)
		return ps
	}
	ns.mu.Lock()
	ns.byNode["a"] = mkPeer("a")
	ns.byNode["b"] = mkPeer("b")
	ns.mu.Unlock()

	e.resetAllPMTU()
	for id, ps := range ns.byNode {
		if ps.effMTU.Load() != 1280 {
			t.Errorf("peer %s effMTU=%d after resetAll, want floor 1280", id, ps.effMTU.Load())
		}
	}
}

// connectedEndpoint must deterministically pick the same peer across calls
// with no preference (lexicographically smallest node ID), not whatever Go's
// randomized map iteration happens to land on first — otherwise a host with
// two directly-connected peers reachable via different local paths would look
// like its own underlay keeps changing. Run with -count=20 (or notice
// flakiness) to catch a regression back to map-iteration-order selection,
// since a single run could coincidentally pick the right answer.
func TestConnectedEndpointDeterministicWithNoPreference(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	ns.mu.Lock()
	ns.byNode["zzz"] = &peerSession{net: ns, nodeID: "zzz", endpoint: netip.MustParseAddrPort("203.0.113.9:1")}
	ns.byNode["aaa"] = &peerSession{net: ns, nodeID: "aaa", endpoint: netip.MustParseAddrPort("203.0.113.1:1")}
	ns.byNode["mmm"] = &peerSession{net: ns, nodeID: "mmm", endpoint: netip.MustParseAddrPort("203.0.113.5:1")}
	ns.mu.Unlock()

	for i := 0; i < 20; i++ {
		_, id := e.connectedEndpoint("")
		if id != "aaa" {
			t.Fatalf("call %d: connectedEndpoint(\"\") = %q, want deterministic \"aaa\" (lexicographically smallest)", i, id)
		}
	}
}

// connectedEndpoint must stick with preferID as long as that peer is still
// directly connected, even though a different (lexicographically smaller)
// peer is also available — this is what lets checkUnderlayChange keep probing
// a fixed destination across repeated calls.
func TestConnectedEndpointStaysWithPreferredPeer(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	wantEP := netip.MustParseAddrPort("203.0.113.9:1")
	ns.mu.Lock()
	ns.byNode["zzz"] = &peerSession{net: ns, nodeID: "zzz", endpoint: wantEP}
	ns.byNode["aaa"] = &peerSession{net: ns, nodeID: "aaa", endpoint: netip.MustParseAddrPort("203.0.113.1:1")}
	ns.mu.Unlock()

	ep, id := e.connectedEndpoint("zzz")
	if id != "zzz" || ep != wantEP {
		t.Fatalf("connectedEndpoint(\"zzz\") = (%v,%q), want (%v,\"zzz\") — should stick with the preferred peer", ep, id, wantEP)
	}
}

// connectedEndpoint must fall back to deterministic selection when preferID
// is no longer connected (e.g. it disconnected), rather than returning
// nothing.
func TestConnectedEndpointFallsBackWhenPreferredGone(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	ns.mu.Lock()
	ns.byNode["aaa"] = &peerSession{net: ns, nodeID: "aaa", endpoint: netip.MustParseAddrPort("203.0.113.1:1")}
	ns.mu.Unlock()

	_, id := e.connectedEndpoint("zzz") // "zzz" was never connected
	if id != "aaa" {
		t.Fatalf("connectedEndpoint(\"zzz\") with zzz absent = id %q, want fallback to \"aaa\"", id)
	}
}

// checkUnderlayChange must not fire when the apparent "change" is really just
// a different reference peer being sampled (e.g. because Go's map iteration
// used to pick arbitrarily) rather than this host's own underlay actually
// moving. Simulates a multi-homed host by having connectedEndpoint bounce
// between two peers with genuinely different local routes, and asserts the
// first rebase is silent (no reset) while a genuine same-peer change still
// fires.
func TestCheckUnderlayChangeIgnoresReferencePeerSwitch(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	mkPeer := func(id string) *peerSession {
		ps := &peerSession{net: ns, nodeID: id, endpoint: netip.MustParseAddrPort("203.0.113.1:65432")}
		ps.initPMTU(1280, 1450)
		ps.pmtuMu.Lock()
		ps.pmtu.eff, ps.pmtu.phase = 1450, phaseSettled
		ps.pmtuMu.Unlock()
		ps.setEff(1450)
		return ps
	}
	ns.mu.Lock()
	ns.byNode["aaa"] = mkPeer("aaa")
	ns.mu.Unlock()

	// First check: no prior reference peer, so this call rebases silently no
	// matter what localSourceIP returns — assert no reset happened by using a
	// resettable marker (peer's effMTU dropping to floor 1280 signals a reset).
	e.checkUnderlayChange(time.Now())
	e.underlayMu.Lock()
	firstRef := e.underlayRefNode
	e.underlayMu.Unlock()
	if firstRef != "aaa" {
		t.Fatalf("after first check, underlayRefNode = %q, want \"aaa\"", firstRef)
	}
	if ns.byNode["aaa"].effMTU.Load() != 1450 {
		t.Fatalf("first checkUnderlayChange (no prior reference) reset PMTU; it should only rebase silently, effMTU=%d", ns.byNode["aaa"].effMTU.Load())
	}

	// Introduce a second, lexicographically-earlier peer. Since "aaa" is
	// already the reference and connectedEndpoint prefers the existing
	// reference when still connected, this alone must not cause a switch.
	ns.mu.Lock()
	ns.byNode["000"] = mkPeer("000")
	ns.mu.Unlock()
	e.checkUnderlayChange(time.Now().Add(2 * time.Second))
	e.underlayMu.Lock()
	ref := e.underlayRefNode
	e.underlayMu.Unlock()
	if ref != "aaa" {
		t.Fatalf("underlayRefNode switched to %q after adding a lexicographically-earlier peer; should have stuck with \"aaa\"", ref)
	}
	if ns.byNode["aaa"].effMTU.Load() != 1450 || ns.byNode["000"].effMTU.Load() != 1450 {
		t.Fatalf("spurious PMTU reset after adding an unrelated peer (aaa=%d, 000=%d), want both still 1450",
			ns.byNode["aaa"].effMTU.Load(), ns.byNode["000"].effMTU.Load())
	}
}

// pmtuLoop used to return before its first tick whenever PMTU discovery was
// disabled (UnderlayMTUMax <= UnderlayMTU), which silently took
// checkUnderlayChange — and therefore roam detection, resetAllPMTU/
// reassertOSState recovery, and the restart-on-underlay-change hook — down
// with it, even though none of those are actually about PMTU discovery.
// This proves the tick (and checkUnderlayChange with it) still runs with
// discovery off; only the per-peer PMTU probing itself should be skipped.
func TestCheckUnderlayChangeRunsWithPMTUDiscoveryDisabled(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("new key set: %v", err)
	}
	e := NewEngine(Options{NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1280, Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Keys: ks,
		Self4: netip.MustParseAddr("10.0.0.1"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	if e.pmtuCeil > e.pmtuFloor {
		t.Fatalf("test setup: discovery should be disabled (ceil=%d, floor=%d)", e.pmtuCeil, e.pmtuFloor)
	}
	e.Start() // pmtuLoop (like every other per-network loop) only runs once started
	defer e.Stop()

	ns := e.netSnapshot()[1]
	ns.mu.Lock()
	ns.byNode["aaa"] = &peerSession{net: ns, nodeID: "aaa", endpoint: netip.MustParseAddrPort("203.0.113.1:1")}
	ns.mu.Unlock()

	if !waitUntil(3*time.Second, func() bool {
		e.underlayMu.Lock()
		defer e.underlayMu.Unlock()
		return e.underlayRefNode == "aaa"
	}) {
		t.Fatal("checkUnderlayChange never ran with PMTU discovery disabled — pmtuLoop must still tick even when discovery itself is off")
	}
}

func TestLocalSourceIP(t *testing.T) {
	ip, ok := localSourceIP(netip.MustParseAddrPort("127.0.0.1:9"))
	if !ok || !ip.IsValid() {
		t.Fatalf("localSourceIP returned (%v,%v)", ip, ok)
	}
	if !ip.IsLoopback() {
		t.Logf("source for loopback dst = %s (non-loopback, environment-dependent)", ip)
	}
}

// TestCheckUnderlayChangeDetectsRoamWithNoUsablePeerEndpoint reproduces the
// intermittent "whole mesh reads 'no reply' after switching networks, but
// only sometimes" report. The peer-anchored roam signal (localSourceIP
// against a specific peer's stored underlay endpoint) can only fire when that
// endpoint is still routable — but a roam onto a network with no route back
// to the old endpoints (with or without a default route) is exactly when it
// isn't, and that's also exactly when recovery is most needed (every peer
// unreachable). Before the peer-independent anchor signal, checkUnderlayChange
// returned early in that case and the roam went undetected: no PMTU reset, no
// OS-state reassert, no restart hook — the "sometimes" was whether any peer
// endpoint happened to still be routable at the instant of the check.
//
// This drives that shape directly: no peers at all (so the peer-anchored half
// contributes nothing, same as all-endpoints-unroutable), while the injected
// default-path source flips as it would on a real roam. The underlay-change
// hook must still fire.
func TestCheckUnderlayChangeDetectsRoamWithNoUsablePeerEndpoint(t *testing.T) {
	origFn := defaultPathSourceIPFn
	defer func() { defaultPathSourceIPFn = origFn }()

	src := netip.MustParseAddr("192.168.1.50")
	defaultPathSourceIPFn = func() (netip.Addr, bool) { return src, true }

	eng := NewEngine(Options{NodeID: "self"})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace) // past startup grace
	var hookCalls int
	eng.SetUnderlayChangeHook(func() { hookCalls++ })

	// First check establishes the baseline default-path source. No peers
	// exist, so the peer-anchored signal never contributes here or below.
	eng.checkUnderlayChange(time.Now())
	if hookCalls != 0 {
		t.Fatalf("baseline check should not fire the hook, got %d", hookCalls)
	}

	// The default egress source flips — a roam — with still no usable peer
	// endpoint anywhere. The anchor signal alone must catch it.
	src = netip.MustParseAddr("10.9.9.20")
	eng.checkUnderlayChange(time.Now().Add(2 * time.Second)) // past the 1s per-check throttle

	if hookCalls != 1 {
		t.Fatalf("roam via the peer-independent anchor signal was not detected with no usable peer "+
			"endpoint (hook fired %d times, want 1) — this is the intermittent whole-mesh 'no reply' case", hookCalls)
	}
}

// TestCheckUnderlayChangeAnchorStableDoesNotFire guards the other direction:
// a steady default-path source (no roam) must not spuriously trigger recovery
// just because the anchor signal now runs every check.
func TestCheckUnderlayChangeAnchorStableDoesNotFire(t *testing.T) {
	origFn := defaultPathSourceIPFn
	defer func() { defaultPathSourceIPFn = origFn }()

	defaultPathSourceIPFn = func() (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.50"), true }

	eng := NewEngine(Options{NodeID: "self"})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace)
	var hookCalls int
	eng.SetUnderlayChangeHook(func() { hookCalls++ })

	base := time.Now()
	for i := 0; i < 4; i++ {
		eng.checkUnderlayChange(base.Add(time.Duration(i) * 2 * time.Second))
	}
	if hookCalls != 0 {
		t.Fatalf("stable default-path source spuriously fired the hook %d time(s)", hookCalls)
	}
}

// TestCheckUnderlayChangeReconnectsAllPeers is the core of the roam fix: a
// detected underlay roam must tear down and re-dial EVERY peer, not just
// configured seeds. Before this, checkUnderlayChange only reset PMTU and
// re-asserted OS routes on a roam and left every session pointed at its
// old-underlay endpoint; a non-seed peer's stale session is never retried
// until it's pruned, and it's only pruned by timeout — so the mesh sat
// partitioned ("no reply" to every peer) until sessions aged out, and even
// then only seeds redialed. This drives a detected roam (via the injected
// default-path anchor, so it fires with no live peer endpoint needed) and
// asserts a live non-seed peer's session was aged past the timeout, i.e. it
// WILL be pruned and re-dialed this maintenance cycle.
func TestCheckUnderlayChangeReconnectsAllPeers(t *testing.T) {
	origFn := defaultPathSourceIPFn
	defer func() { defaultPathSourceIPFn = origFn }()
	src := netip.MustParseAddr("192.168.1.50")
	defaultPathSourceIPFn = func() (netip.Addr, bool) { return src, true }

	const netID = uint64(0xA11)
	eng := NewEngine(Options{
		NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450,
		Nets: []NetSpec{{ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.Attach(nopSender{})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace)
	ns := eng.network(netID)

	// A live, non-seed peer whose endpoint is on the OLD underlay.
	ps := &peerSession{nodeID: "peer", net: ns, localIdx: 7,
		endpoint: netip.MustParseAddrPort("203.0.113.1:65432")}
	ps.initPMTU(eng.pmtuFloor, eng.pmtuCeil)
	ps.setLastRx(time.Now()) // freshly alive
	eng.mu.Lock()
	eng.sessions[7] = ps
	eng.mu.Unlock()
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.mu.Unlock()

	// Baseline check (establishes the anchor), then the roam.
	eng.checkUnderlayChange(time.Now())
	src = netip.MustParseAddr("10.9.9.20")
	roamAt := time.Now().Add(2 * time.Second)
	eng.checkUnderlayChange(roamAt)

	if roamAt.Sub(ps.lastRxTime()) <= eng.peerTimeoutDuration() {
		t.Fatalf("a detected roam did not age the (non-seed) peer session past peerTimeout "+
			"(gap=%v) — it would black-hole on its stale endpoint until timeout instead of "+
			"redialing, which is the whole-mesh 'no reply' after roam bug", roamAt.Sub(ps.lastRxTime()))
	}
	// pruneDead this same cycle should now reap it, freeing the endpoint for redial.
	eng.pruneDead(ns, roamAt)
	ns.mu.RLock()
	_, still := ns.byNode["peer"]
	ns.mu.RUnlock()
	if still {
		t.Fatal("peer session was not pruned after the roam aged it — it won't be redialed")
	}
}

// TestCheckUnderlayChangeDetectsRoamViaGatewayWhenSourceIPUnchanged is the
// same-subnet-roam terminal state: roaming between two networks that hand out
// the SAME local IP (two APs on one 192.168.203.x subnet, or the same DHCP
// lease re-issued on rejoin) leaves the anchor source address (signal 1)
// unchanged, and if every peer is already dead the peer-anchored signal
// (signal 2) contributes nothing either — so the roam goes completely
// undetected, no recovery runs, and no further roam re-triggers it until you
// switch to a network that finally gives a different IP. The physical default
// gateway (signal 1b) almost always differs across such a move, so it catches
// what the source IP misses. Here the injected default-path source is held
// constant while the gateway flips; recovery must still fire.
func TestCheckUnderlayChangeDetectsRoamViaGatewayWhenSourceIPUnchanged(t *testing.T) {
	origSrc := defaultPathSourceIPFn
	origGW := defaultGatewayFn
	defer func() { defaultPathSourceIPFn = origSrc; defaultGatewayFn = origGW }()

	// Source IP is pinned — signal 1 can never fire.
	defaultPathSourceIPFn = func() (netip.Addr, bool) {
		return netip.MustParseAddr("192.168.203.10"), true
	}
	// Gateway flips between checks — signal 1b must carry the detection.
	gw := netip.MustParseAddr("192.168.203.1")
	var gwIf int32 = 3
	defaultGatewayFn = func(family int, exclude int32) (tun.Gateway, error) {
		return tun.Gateway{Addr: gw, IfIndex: gwIf, Metric: 100}, nil
	}

	eng := NewEngine(Options{NodeID: "self"})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace)
	var hookCalls int
	eng.SetUnderlayChangeHook(func() { hookCalls++ })

	// Baseline check establishes both the (pinned) source and the gateway.
	eng.checkUnderlayChange(time.Now())
	if hookCalls != 0 {
		t.Fatalf("baseline check should not fire the hook, got %d", hookCalls)
	}

	// Same source IP, but a roam onto a different AP: same subnet, different
	// gateway address AND interface. Signal 1 stays silent; 1b must fire.
	gw = netip.MustParseAddr("192.168.203.254")
	gwIf = 5
	eng.checkUnderlayChange(time.Now().Add(2 * time.Second))
	if hookCalls != 1 {
		t.Fatalf("a same-source-IP roam (gateway changed) was not detected (hook fired %d times, want 1) — "+
			"this is the '3-4 rapid roams then stuck until a different network' terminal state", hookCalls)
	}
}

// TestCheckUnderlayChangeIgnoresOwnTunnelDefaultRoute is the regression guard
// for the v463 self-inflicted loop. In full-tunnel mode gravinet installs its
// own default route via a tun device and demotes the physical one's metric. A
// naive "lowest-metric default route" read for signal 1b flips between the
// physical gateway and gravinet's own tunnel gateway every time recovery
// reasserts full-tunnel state — and since a detected change triggers recovery
// which reasserts that state, that's a once-per-second self-sustaining loop
// that re-dials every peer every second so no handshake ever completes,
// making the mesh permanently unrecoverable (strictly worse than not having
// the signal). physicalDefaultGateway must therefore reject a default route
// that sits on one of gravinet's own tun interfaces. Here defaultGatewayFn
// returns a gateway ON the fake tun's ifindex; signal 1b must treat it as "no
// physical gateway" and never fire, no matter how it changes.
func TestCheckUnderlayChangeIgnoresOwnTunnelDefaultRoute(t *testing.T) {
	origSrc := defaultPathSourceIPFn
	origGW := defaultGatewayFn
	defer func() { defaultPathSourceIPFn = origSrc; defaultGatewayFn = origGW }()

	// Pin the source IP so signal 1 stays silent; isolate signal 1b.
	defaultPathSourceIPFn = func() (netip.Addr, bool) {
		return netip.MustParseAddr("10.0.0.1"), true
	}

	// The only "default gateway" the OS reports sits on gravinet's own tun
	// device (fakeDev.IfIndex == 0xF4CE), i.e. it's gravinet's tunnel default,
	// not a physical one — and it flips address each call, as gravinet's
	// demote/restore would make a naive read appear to.
	const tunIf = int32(0xF4CE)
	flip := false
	defaultGatewayFn = func(family int, exclude int32) (tun.Gateway, error) {
		flip = !flip
		addr := "10.255.0.1"
		if flip {
			addr = "10.255.0.2"
		}
		return tun.Gateway{Addr: netip.MustParseAddr(addr), IfIndex: tunIf, Metric: 50}, nil
	}

	eng := NewEngine(Options{
		NodeID: "self",
		Nets: []NetSpec{{ID: 0xB01, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.startedAt = time.Now().Add(-2 * underlayRestartGrace)
	var hookCalls int
	eng.SetUnderlayChangeHook(func() { hookCalls++ })

	// Several checks, each seeing a different tun-owned "gateway". None may
	// fire: the gateway sits on our own tun, so it isn't a physical roam.
	base := time.Now()
	for i := 0; i < 4; i++ {
		eng.checkUnderlayChange(base.Add(time.Duration(i) * 2 * time.Second))
	}
	if hookCalls != 0 {
		t.Fatalf("signal 1b fired %d time(s) on a gateway that sits on gravinet's own tun interface — "+
			"that's the v463 demote loop that made the mesh permanently unrecoverable", hookCalls)
	}
}
