package mesh

import (
	"io"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// Relay scoring used to compare only this node's RTT to the candidate, which
// is one half of the path, and it made that comparison exactly once — a
// relayed session lives in ns.byNode, so tryRelays never looked at it again.
// Together those two facts let a node sit permanently on the slowest relay
// available, chosen arbitrarily at startup before any keepalive had completed.
//
// These tests cover the two halves of the fix: the far leg is now gossiped
// (peerEntry.rttMillis, relayCost) and the choice is now revisited under
// hysteresis (rescoreRelays).

// advertise makes cand report a round trip of rtt to target, as if it had
// arrived in a gossiped peer list.
func advertise(cand *peerSession, target string, rtt time.Duration) {
	cand.noteReportedRTT([]peerEntry{{
		nodeID:    target,
		rttMillis: uint16(rtt / time.Millisecond),
	}})
}

// ---- wire format ----

func TestPeerListRoundTripsRTT(t *testing.T) {
	in := []peerEntry{
		{nodeID: "a", rttMillis: 42},
		{nodeID: "b", rttMillis: 0}, // unknown: must stay unknown
		{nodeID: "c", rttMillis: 65535},
	}
	out, err := decodePeerList(encodePeerList(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != len(in) {
		t.Fatalf("got %d entries, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i].rttMillis != in[i].rttMillis {
			t.Errorf("entry %q: rttMillis = %d, want %d", in[i].nodeID, out[i].rttMillis, in[i].rttMillis)
		}
	}
}

// The block must not be emitted when nobody has a measurement, so a mesh where
// no keepalive round trip has completed pays nothing for the feature — the same
// property every other optional block here has.
func TestPeerListOmitsRTTBlockWhenNoneMeasured(t *testing.T) {
	none := encodePeerList([]peerEntry{{nodeID: "a"}, {nodeID: "b"}})
	some := encodePeerList([]peerEntry{{nodeID: "a"}, {nodeID: "b", rttMillis: 5}})
	if len(some) <= len(none) {
		t.Fatalf("encoding with a measurement (%d bytes) should be longer than without (%d)", len(some), len(none))
	}
	for _, b := range none {
		if b == peerListRTTBlock {
			// Not conclusive on its own (0x07 could be a payload byte), so
			// pair it with the length check above rather than relying on it.
			t.Log("note: 0x07 appears in the no-RTT encoding as payload, which is fine")
		}
	}
}

// The RTT block is emitted last, after every block that predates it, because a
// decoder that predates a marker stops at it — so anything ordered *before* the
// new block would become invisible to this build's own decoder if the order
// were wrong. Asserted by round-tripping an entry that populates every block at
// once and checking nothing earlier was lost.
func TestPeerListRTTBlockDoesNotShadowEarlierBlocks(t *testing.T) {
	in := []peerEntry{{
		nodeID:         "a",
		tcpPort:        4443,
		extraTCPPorts:  []uint16{1, 2},
		extraUDPPorts:  []uint16{3},
		localEndpoints: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:51820")},
		version:        "796",
		selfSeed:       true,
		rttMillis:      77,
	}}
	out, err := decodePeerList(encodePeerList(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out[0]
	if got.tcpPort != 4443 || len(got.extraTCPPorts) != 2 || len(got.extraUDPPorts) != 1 ||
		len(got.localEndpoints) != 1 || got.version != "796" || !got.selfSeed {
		t.Errorf("an earlier block was lost: %+v", got)
	}
	if got.rttMillis != 77 {
		t.Errorf("rttMillis = %d, want 77", got.rttMillis)
	}
}

// An older peer sends no RTT block at all. That must read back as "unknown"
// rather than as zero latency, which would score as an unbeatably fast far leg.
func TestMissingRTTBlockIsUnknownNotZero(t *testing.T) {
	out, err := decodePeerList(encodePeerList([]peerEntry{{nodeID: "a", tcpPort: 1}}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out[0].rttMillis != 0 {
		t.Fatalf("rttMillis = %d, want 0", out[0].rttMillis)
	}
	cand := mkCandidate("cand", false, 10)
	cand.noteReportedRTT(out)
	if _, ok := cand.reportedRTTFor("a"); ok {
		t.Fatal("a zero rttMillis was stored as a measurement; it means 'not measured'")
	}
}

func TestRTTMillisOfClampsAndDistinguishesUnmeasured(t *testing.T) {
	unmeasured := &peerSession{}
	if got := rttMillisOf(unmeasured); got != 0 {
		t.Errorf("unmeasured: got %d, want 0", got)
	}
	sub := &peerSession{}
	sub.rttNanos.Store(int64(100 * time.Microsecond))
	if got := rttMillisOf(sub); got != 1 {
		t.Errorf("sub-millisecond but measured: got %d, want 1 (0 would read as unmeasured)", got)
	}
	huge := &peerSession{}
	huge.rttNanos.Store(int64(120 * time.Second))
	if got := rttMillisOf(huge); got != 65535 {
		t.Errorf("absurdly slow: got %d, want the clamp 65535 (wrapping would look fastest)", got)
	}
}

// ---- cost model ----

func TestRelayCostAddsGossipedFarLeg(t *testing.T) {
	cand := mkCandidate("cand", false, 50)
	if cost, full, known := relayCost(cand, "target"); !known || full || cost != 50*time.Millisecond {
		t.Fatalf("no gossiped far leg: cost=%v full=%v known=%v, want 50ms/false/true", cost, full, known)
	}
	advertise(cand, "target", 20*time.Millisecond)
	cost, full, known := relayCost(cand, "target")
	if !known || !full {
		t.Fatalf("with a gossiped far leg: full=%v known=%v, want true/true", full, known)
	}
	if cost != 70*time.Millisecond {
		t.Fatalf("cost = %v, want 70ms (50 near + 20 far)", cost)
	}
}

// The headline case: a nearby relay with a long onward leg is worse than a
// distant one that is close to the target. Scoring the near leg alone gets this
// exactly backwards, which is the bug.
func TestRelayBetterUsesEndToEndNotJustTheNearLeg(t *testing.T) {
	near := mkCandidate("near", false, 20)     // close to us...
	far := mkCandidate("far", false, 150)      // ...much further from us
	advertise(near, "t", 400*time.Millisecond) // ...but a long way from the target
	advertise(far, "t", 10*time.Millisecond)   // ...and nearly on top of it

	if !relayBetter(far, near, "t") {
		t.Fatal("160ms end to end should beat 420ms, even though the loser's near leg is closer")
	}
	if relayBetter(near, far, "t") {
		t.Fatal("the nearer candidate should not win on its near leg alone")
	}
}

// A known total is not comparable to a half-known one: a fast near leg with an
// unadvertised far leg could hide anything.
func TestRelayBetterPrefersFullyKnownCostOverPartial(t *testing.T) {
	known := mkCandidate("known", false, 60)
	advertise(known, "t", 30*time.Millisecond)   // 90ms, known
	partial := mkCandidate("partial", false, 20) // 20ms near leg, far leg unknown

	if !relayBetter(known, partial, "t") {
		t.Fatal("a known 90ms total should beat an unknown total behind a 20ms near leg")
	}
	if relayBetter(partial, known, "t") {
		t.Fatal("a partial cost should not beat a fully known one")
	}
}

// Stale gossip must not be trusted: it would be believed strongly enough to
// move a working path onto a guess.
func TestReportedRTTExpires(t *testing.T) {
	cand := mkCandidate("cand", false, 10)
	advertise(cand, "t", 5*time.Millisecond)
	if _, ok := cand.reportedRTTFor("t"); !ok {
		t.Fatal("a fresh observation should be usable")
	}
	cand.reportedMu.Lock()
	obs := cand.reportedRTT["t"]
	obs.at = time.Now().Add(-reportedRTTTTL - time.Second)
	cand.reportedRTT["t"] = obs
	cand.reportedMu.Unlock()

	if _, ok := cand.reportedRTTFor("t"); ok {
		t.Fatal("an observation older than reportedRTTTTL should read as unknown")
	}
	cand.reportedMu.Lock()
	_, still := cand.reportedRTT["t"]
	cand.reportedMu.Unlock()
	if still {
		t.Error("a stale observation should be dropped, not left to accumulate")
	}
}

// The TTL has to outlast the worst case gap between refreshes. RTT is
// deliberately not in peerListSig, so on a quiet mesh the only thing that
// refreshes it is gossipFullRefresh.
func TestReportedRTTTTLOutlastsGossipFullRefresh(t *testing.T) {
	if reportedRTTTTL <= gossipFullRefresh {
		t.Fatalf("reportedRTTTTL (%v) must exceed gossipFullRefresh (%v), or a quiet mesh "+
			"spends most of its time treating every far leg as unknown", reportedRTTTTL, gossipFullRefresh)
	}
}

// RTT must stay out of the gossip signature. If it got in, the signature would
// differ on essentially every keepalive and re-flood the full peer list every
// tick — the exact O(N^2) cost that signature exists to avoid.
func TestPeerListSigIgnoresRTT(t *testing.T) {
	ns := &netState{byNode: map[string]*peerSession{}}
	ps := &peerSession{nodeID: "a", hostname: "a"}
	ns.byNode["a"] = ps

	ps.rttNanos.Store(int64(10 * time.Millisecond))
	before := ns.peerListSig()
	ps.rttNanos.Store(int64(4500 * time.Millisecond))
	if after := ns.peerListSig(); after != before {
		t.Fatal("peerListSig changed when only RTT moved; that re-floods the peer list every tick")
	}
}

// ---- re-scoring ----

// relayed builds an incumbent relayed session established dwell ago, with an
// end-to-end RTT of rtt, reached via the session named by relay.
func relayedIncumbent(target, relay string, rtt, age time.Duration) *peerSession {
	ps := &peerSession{nodeID: target, established: time.Now().Add(-age)}
	ps.relay = &peerSession{nodeID: relay}
	ps.rttNanos.Store(int64(rtt))
	return ps
}

// candidateFor builds a relay candidate that reports knowing target.
func candidateFor(id, target string, near, far time.Duration) *peerSession {
	ps := &peerSession{nodeID: id}
	ps.rttNanos.Store(int64(near))
	ps.markReported([]string{target})
	if far > 0 {
		advertise(ps, target, far)
	}
	return ps
}

// rescoreState wires a netState holding one incumbent session plus candidate
// relays. Kept separate from rescoreFixture so the two tests that need to seed
// pending/relayRescored themselves can reuse it.
func rescoreState(inc *peerSession, cands ...*peerSession) (*Engine, *netState) {
	e := &Engine{nodeID: "self", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{
		byNode:        map[string]*peerSession{inc.nodeID: inc},
		pending:       map[uint32]*pendingHS{},
		nodes:         map[string]*nodeInfo{},
		relayRescored: map[string]time.Time{},
	}
	ns.spec.ID = 0x1234
	for _, c := range cands {
		ns.byNode[c.nodeID] = c
	}
	return e, ns
}

// rescoreFixture returns the targets relaySwitches decided to move.
//
// Deliberately exercises relaySwitches rather than rescoreRelays: every bound
// under test here is a decision, and rescoreRelays' only other job is to
// perform the handshake, which needs a key set and a socket this fixture has no
// business standing up. That separation is why relaySwitches exists.
func rescoreFixture(t *testing.T, inc *peerSession, cands ...*peerSession) []string {
	t.Helper()
	e, ns := rescoreState(inc, cands...)
	var moved []string
	for _, sw := range e.relaySwitches(ns, time.Now()) {
		moved = append(moved, sw.target)
	}
	return moved
}

func TestRescoreMovesPathToAMateriallyBetterRelay(t *testing.T) {
	// The screenshot case: reaching the target costs 363ms through the
	// incumbent, and a candidate offers 60ms end to end.
	inc := relayedIncumbent("target", "slow-relay", 363*time.Millisecond, relayRescoreDwell+time.Minute)
	good := candidateFor("fast-relay", "target", 50*time.Millisecond, 10*time.Millisecond)

	e, ns := rescoreState(inc, good)
	switches := e.relaySwitches(ns, time.Now())
	if len(switches) != 1 {
		t.Fatalf("got %d switches, want 1: 60ms should displace a measured 363ms", len(switches))
	}
	sw := switches[0]
	if sw.target != "target" || sw.from != "slow-relay" || sw.to.nodeID != "fast-relay" {
		t.Errorf("switch = %+v, want target→fast-relay away from slow-relay", sw)
	}
	// The reported figures are what the log line carries, so an operator can
	// see why the path moved rather than just that it did.
	if sw.was != 363*time.Millisecond {
		t.Errorf("was = %v, want the incumbent's measured 363ms", sw.was)
	}
	if sw.now != 60*time.Millisecond {
		t.Errorf("now = %v, want the challenger's estimated 60ms (50 near + 10 far)", sw.now)
	}
}

func TestRescoreLeavesPathAloneWithinMargin(t *testing.T) {
	// 90ms vs a measured 100ms: a real improvement, but well inside the
	// margin, and a handshake plus a brief interruption costs more than 10ms.
	inc := relayedIncumbent("target", "incumbent", 100*time.Millisecond, relayRescoreDwell+time.Minute)
	marginal := candidateFor("marginal", "target", 60*time.Millisecond, 30*time.Millisecond)

	if moved := rescoreFixture(t, inc, marginal); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: a 10ms gain is inside relayRescoreMargin", moved)
	}
}

// The absolute floor exists for fast paths, where the ratio alone would chase
// jitter: 3ms is 50% better than 6ms and completely meaningless.
func TestRescoreRequiresAbsoluteGainNotJustRatio(t *testing.T) {
	inc := relayedIncumbent("target", "incumbent", 6*time.Millisecond, relayRescoreDwell+time.Minute)
	jitter := candidateFor("jitter", "target", 2*time.Millisecond, time.Millisecond)

	if moved := rescoreFixture(t, inc, jitter); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: 3ms off 6ms clears the ratio but not relayRescoreMinGain", moved)
	}
}

// A freshly established path has one RTT sample or none. Moving it on that
// basis reintroduces the arbitrary-pick problem with extra steps.
func TestRescoreRespectsDwell(t *testing.T) {
	inc := relayedIncumbent("target", "slow-relay", 363*time.Millisecond, relayRescoreDwell/2)
	good := candidateFor("fast-relay", "target", 50*time.Millisecond, 10*time.Millisecond)

	if moved := rescoreFixture(t, inc, good); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: the incumbent is younger than relayRescoreDwell", moved)
	}
}

// Trading a measured path for one whose far leg is a guess is not an
// improvement; it is a coin flip with extra latency.
func TestRescoreIgnoresCandidateWithUnknownFarLeg(t *testing.T) {
	inc := relayedIncumbent("target", "slow-relay", 363*time.Millisecond, relayRescoreDwell+time.Minute)
	unknown := candidateFor("unadvertised", "target", 20*time.Millisecond, 0) // near leg only

	if moved := rescoreFixture(t, inc, unknown); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: the candidate's far leg is unknown", moved)
	}
}

// A direct session is not a relayed one and must never be moved onto a relay,
// however fast the relay looks.
func TestRescoreNeverTouchesDirectSessions(t *testing.T) {
	direct := &peerSession{nodeID: "target", established: time.Now().Add(-time.Hour)}
	direct.rttNanos.Store(int64(500 * time.Millisecond)) // slow, but direct
	good := candidateFor("fast-relay", "target", 5*time.Millisecond, 5*time.Millisecond)

	if moved := rescoreFixture(t, direct, good); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: a direct path is never moved onto a relay", moved)
	}
}

// No end-to-end measurement means nothing to compare a challenger against.
func TestRescoreSkipsIncumbentWithNoMeasurement(t *testing.T) {
	inc := &peerSession{nodeID: "target", established: time.Now().Add(-time.Hour)}
	inc.relay = &peerSession{nodeID: "slow-relay"}
	// rttNanos deliberately left at 0.
	good := candidateFor("fast-relay", "target", 5*time.Millisecond, 5*time.Millisecond)

	if moved := rescoreFixture(t, inc, good); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: the incumbent has no measured cost", moved)
	}
}

// Re-picking the relay already in use would be a pointless handshake.
func TestRescoreDoesNotMoveToTheIncumbentRelay(t *testing.T) {
	inc := relayedIncumbent("target", "same-relay", 363*time.Millisecond, relayRescoreDwell+time.Minute)
	same := candidateFor("same-relay", "target", 5*time.Millisecond, 5*time.Millisecond)

	if moved := rescoreFixture(t, inc, same); len(moved) != 0 {
		t.Fatalf("moved = %v, want none: that is the relay already in use", moved)
	}
}

// Two candidates either side of the margin could otherwise hand a path back and
// forth every dwell period.
func TestRescoreThrottlesRepeatMoves(t *testing.T) {
	inc := relayedIncumbent("target", "slow-relay", 363*time.Millisecond, relayRescoreDwell+time.Minute)
	good := candidateFor("fast-relay", "target", 50*time.Millisecond, 10*time.Millisecond)

	e, ns := rescoreState(inc, good)
	ns.relayRescored["target"] = time.Now() // just moved

	if got := e.relaySwitches(ns, time.Now()); len(got) != 0 {
		t.Fatalf("got %d switches, want 0: this target moved within relayRescoreInterval", len(got))
	}
	// ...and once the interval has elapsed it becomes eligible again, so the
	// throttle is a delay rather than a permanent pin.
	later := time.Now().Add(relayRescoreInterval + time.Second)
	if got := e.relaySwitches(ns, later); len(got) != 1 {
		t.Fatalf("got %d switches after relayRescoreInterval elapsed, want 1", len(got))
	}
}

// A handshake already in flight for this target means a switch is redundant.
func TestRescoreSkipsTargetWithHandshakeInFlight(t *testing.T) {
	inc := relayedIncumbent("target", "slow-relay", 363*time.Millisecond, relayRescoreDwell+time.Minute)
	good := candidateFor("fast-relay", "target", 50*time.Millisecond, 10*time.Millisecond)

	e, ns := rescoreState(inc, good)
	ns.pending[1] = &pendingHS{targetNode: "target"}

	if got := e.relaySwitches(ns, time.Now()); len(got) != 0 {
		t.Fatalf("got %d switches, want 0: a handshake for this target is already in flight", len(got))
	}
}

// The margin and dwell constants are the whole reason this is safe to run every
// maintenance tick. Pin them so a later edit has to be deliberate.
func TestRescoreHysteresisBoundsAreSane(t *testing.T) {
	if relayRescoreMargin >= 1 {
		t.Errorf("relayRescoreMargin = %v: at or above 1 any challenger displaces the incumbent", relayRescoreMargin)
	}
	if relayRescoreDwell < 3*defaultKeepaliveInterval {
		t.Errorf("relayRescoreDwell (%v) should span several keepalives (%v) so the incumbent's "+
			"RTT is more than one sample", relayRescoreDwell, defaultKeepaliveInterval)
	}
	if relayRescoreInterval <= relayRescoreDwell {
		t.Errorf("relayRescoreInterval (%v) should exceed relayRescoreDwell (%v), or a path can be "+
			"handed between two candidates as fast as it settles", relayRescoreInterval, relayRescoreDwell)
	}
	if relayRescoreMinGain <= 0 {
		t.Error("relayRescoreMinGain must be positive, or the ratio alone governs fast paths")
	}
}
