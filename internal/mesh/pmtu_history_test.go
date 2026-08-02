package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestSeedLinkCapClampsSearchBelowWhatWouldOtherwiseBeReached proves the
// core mechanism: seeding a fresh pmtuState with a remembered link cap
// bounds discovery to that cap even on a path that would otherwise support
// more — the whole point being to never re-probe a size already known to
// be refused.
func TestSeedLinkCapClampsSearchBelowWhatWouldOtherwiseBeReached(t *testing.T) {
	ps := &peerSession{}
	ps.initPMTU(1280, 9000)
	ps.seedLinkCap(1400)

	// A path that actually supports the full ceiling — without the seed,
	// TestPMTUClimbsToCeiling already proves this converges to 9000.
	eff := driveSearch(ps.pmtu, func(int) bool { return true }, 200)
	if eff != 1400 {
		t.Fatalf("eff=%d, want 1400 (the seeded cap), even though every candidate would have acked", eff)
	}
	ps.pmtuMu.Lock()
	lc := ps.pmtu.linkCap
	ps.pmtuMu.Unlock()
	if lc != 1400 {
		t.Errorf("linkCap = %d, want 1400 (seeded value recorded the same way tooBig would)", lc)
	}
}

// TestSeedLinkCapIgnoredWhenBelowFloor proves a stale seed that's now below
// the configured floor is ignored rather than clamping a healthy path down
// to a size nothing has actually refused — see seedLinkCap's own doc
// comment for why this specific case can only arise from the floor having
// grown since the value was recorded, not from tooBig ever producing it.
func TestSeedLinkCapIgnoredWhenBelowFloor(t *testing.T) {
	ps := &peerSession{}
	ps.initPMTU(1400, 9000) // floor raised above the stale seed below
	ps.seedLinkCap(1280)

	eff := driveSearch(ps.pmtu, func(int) bool { return true }, 200)
	if eff != 9000 {
		t.Fatalf("eff=%d, want 9000 (a below-floor seed should have been ignored entirely)", eff)
	}
}

// TestSeedLinkCapNoopForNonPositive proves a zero or negative cap (no
// remembered value, or a defensive bad input) touches nothing.
func TestSeedLinkCapNoopForNonPositive(t *testing.T) {
	for _, cap := range []int{0, -1} {
		ps := &peerSession{}
		ps.initPMTU(1280, 9000)
		ps.seedLinkCap(cap)
		ps.pmtuMu.Lock()
		ceil, lc := ps.pmtu.ceil, ps.pmtu.linkCap
		ps.pmtuMu.Unlock()
		if ceil != 9000 || lc != 0 {
			t.Errorf("seedLinkCap(%d): ceil=%d linkCap=%d, want unchanged (9000, 0)", cap, ceil, lc)
		}
	}
}

// TestCurrentLinkCapReadsWhatTooBigRecorded proves the read-side accessor
// used at teardown actually reflects real EMSGSIZE-derived state, not just
// what was seeded — the round trip this whole feature depends on.
func TestCurrentLinkCapReadsWhatTooBigRecorded(t *testing.T) {
	ps := &peerSession{}
	ps.initPMTU(1280, 9000)
	if got := ps.currentLinkCap(); got != 0 {
		t.Fatalf("currentLinkCap before any rejection = %d, want 0", got)
	}
	ps.pmtuMu.Lock()
	ps.pmtu.tooBig(1401, time.Now())
	ps.pmtuMu.Unlock()
	if got := ps.currentLinkCap(); got != 1400 {
		t.Errorf("currentLinkCap after tooBig(1401) = %d, want 1400", got)
	}
}

// TestTeardownSessionsRemembersLinkCap is the write-side integration test:
// a session that discovered a real constraint has it preserved in nodeInfo
// when the session is torn down, surviving exactly the moment peerSession
// itself is discarded.
func TestTeardownSessionsRemembersLinkCap(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]

	ps := &peerSession{net: ns, nodeID: "peer"}
	ps.initPMTU(1280, 9000)
	ps.pmtuMu.Lock()
	ps.pmtu.tooBig(1401, time.Now())
	ps.pmtuMu.Unlock()

	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.nodes["peer"] = &nodeInfo{nodeID: "peer"}
	ns.mu.Unlock()

	e.teardownSessions(ns, []*peerSession{ps}, "test teardown")

	ns.mu.RLock()
	ni := ns.nodes["peer"]
	ns.mu.RUnlock()
	if ni == nil {
		t.Fatal("nodeInfo for peer disappeared")
	}
	if ni.lastLinkCap != 1400 {
		t.Errorf("lastLinkCap = %d, want 1400", ni.lastLinkCap)
	}
	if ni.lastLinkCapAt.IsZero() {
		t.Error("lastLinkCapAt was not recorded")
	}
}

// TestTeardownSessionsLeavesExistingLinkCapAloneWhenNothingNewLearned
// proves a session that never triggered a fresh EMSGSIZE doesn't clear an
// existing remembered value — see teardownSessions' own comment: no new
// rejection isn't evidence an old constraint went away, especially since
// that old value is often exactly why this session never got probed high
// enough to re-trigger it.
func TestTeardownSessionsLeavesExistingLinkCapAloneWhenNothingNewLearned(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]

	ps := &peerSession{net: ns, nodeID: "peer"}
	ps.initPMTU(1280, 9000) // never triggers tooBig this session

	old := time.Now().Add(-time.Minute)
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.nodes["peer"] = &nodeInfo{nodeID: "peer", lastLinkCap: 1400, lastLinkCapAt: old}
	ns.mu.Unlock()

	e.teardownSessions(ns, []*peerSession{ps}, "test teardown")

	ns.mu.RLock()
	ni := ns.nodes["peer"]
	ns.mu.RUnlock()
	if ni.lastLinkCap != 1400 || !ni.lastLinkCapAt.Equal(old) {
		t.Errorf("existing remembered value was overwritten: lastLinkCap=%d lastLinkCapAt=%v, want untouched (1400, %v)",
			ni.lastLinkCap, ni.lastLinkCapAt, old)
	}
}

// TestSeedPeerPMTUFromHistoryUsesRecentNodeInfo is the read-side integration
// test: a fresh session for a node with a recently-remembered link cap
// starts already bounded to it, instead of climbing the full ceiling from
// scratch. Calls seedPeerPMTUFromHistory directly — the piece install()
// delegates to — rather than all of install(), which also gossips over a
// real crypto session a bare test fixture doesn't have.
func TestSeedPeerPMTUFromHistoryUsesRecentNodeInfo(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]

	ns.mu.Lock()
	ns.nodes["peer"] = &nodeInfo{nodeID: "peer", lastLinkCap: 1400, lastLinkCapAt: time.Now()}
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer", overlay4: netip.MustParseAddr("10.9.0.2")}
	ps.initPMTU(e.pmtuFloor, e.pmtuCeil)
	e.seedPeerPMTUFromHistory(ns, ps)

	ps.pmtuMu.Lock()
	ceil, lc := ps.pmtu.ceil, ps.pmtu.linkCap
	ps.pmtuMu.Unlock()
	if lc != 1400 {
		t.Errorf("linkCap after seedPeerPMTUFromHistory = %d, want 1400 (seeded from nodeInfo)", lc)
	}
	if ceil > 1400 {
		t.Errorf("ceil after seedPeerPMTUFromHistory = %d, want <= 1400", ceil)
	}
}

// TestSeedPeerPMTUFromHistoryIgnoresExpiredNodeInfo proves a remembered
// value older than linkCapMemoryTTL is not applied — the peer gets a
// genuine, unconstrained fresh search, on the theory that a link stable
// enough to go this long without reconnecting has earned the chance to
// prove any old constraint no longer applies.
func TestSeedPeerPMTUFromHistoryIgnoresExpiredNodeInfo(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]

	ns.mu.Lock()
	ns.nodes["peer"] = &nodeInfo{
		nodeID: "peer", lastLinkCap: 1400,
		lastLinkCapAt: time.Now().Add(-linkCapMemoryTTL - time.Minute),
	}
	ns.mu.Unlock()

	ps := &peerSession{net: ns, nodeID: "peer", overlay4: netip.MustParseAddr("10.9.0.3")}
	ps.initPMTU(e.pmtuFloor, e.pmtuCeil)
	e.seedPeerPMTUFromHistory(ns, ps)

	ps.pmtuMu.Lock()
	ceil, lc := ps.pmtu.ceil, ps.pmtu.linkCap
	ps.pmtuMu.Unlock()
	if lc != 0 {
		t.Errorf("linkCap after seedPeerPMTUFromHistory with an expired memory = %d, want 0 (untouched)", lc)
	}
	if ceil != e.pmtuCeil {
		t.Errorf("ceil after seedPeerPMTUFromHistory with an expired memory = %d, want the engine default %d", ceil, e.pmtuCeil)
	}
}
