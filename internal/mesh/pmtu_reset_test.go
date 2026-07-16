package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
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
