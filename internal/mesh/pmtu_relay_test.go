package mesh

import (
	"io"
	"testing"
	"time"

	"gravinet/internal/logx"
	"gravinet/internal/protocol"
)

// relayEnvelopeOverheadFor is the test's own independent restatement of what
// deliver() adds when it sends to a relayed peer: encodeRelay's two
// length-prefixed node IDs, wrapped in one more sealed datagram on the relay's
// own session. Deliberately not calling the production helper — a test that
// asks the code under test to define the number it is checking proves only
// that the code agrees with itself.
func relayEnvelopeOverheadFor(selfID, peerID string) int {
	sealed := protocol.DataHeaderLen + 1 + protocol.GCMOverhead // header + innerType + AEAD tag
	return sealed + 2 + len(selfID) + len(peerID)               // + two 1-byte length prefixes
}

// TestRelayedPMTUCeilingAccountsForRelayEnvelope covers the field failure that
// motivated it: a relayed session climbing its path-MTU search all the way to
// the same ceiling a direct session uses, with no allowance for the relay
// envelope it will be wrapped in.
//
// What that produced, from the logs: gn-openbsd, reached over a three-hop chain,
// would come up and walk 5140 → 7070 → 8035 → ... → 9000 in thirteen seconds,
// converging on exactly the figure the direct sessions carried. The probes ack
// because the relay fragments them — an acked probe proves the bytes arrive,
// not that they arrived in one piece end to end — so nothing in the search ever
// pushes back. Real traffic then rode permanently fragmented, and the peer table
// showed it: the relayed peers carried the only non-zero fragment counters in
// the mesh, and were the only sessions that ever got reaped and rebuilt.
//
// The invariant: a relayed session's outer datagram must fit inside its relay's
// own outer datagram, with room for the envelope. Chains come out right for
// free, because a relay that is itself relayed has already had this applied to
// its own effMTU, so capping against it composes.
func TestRelayedPMTUCeilingAccountsForRelayEnvelope(t *testing.T) {
	const (
		selfID   = "gn-mcfed"
		peerID   = "gn-openbsd"
		relayID  = "gn-rocky"
		ceiling  = 9000 // config underlay_mtu_max, as in the field case
		flooring = 1280
	)

	e := &Engine{nodeID: selfID, log: logx.New(io.Discard, logx.LevelDebug)}

	relay := &peerSession{nodeID: relayID}
	relay.setEff(ceiling) // relay itself reaches us at the full underlay MTU

	ps := &peerSession{nodeID: peerID, relay: relay}
	ps.initPMTU(flooring, ceiling)

	want := ceiling - relayEnvelopeOverheadFor(selfID, peerID)

	got := e.relayMTUCap(ps)
	if got != want {
		t.Fatalf("relayMTUCap = %d, want %d (relay eff %d minus envelope %d)", got, want, ceiling, relayEnvelopeOverheadFor(selfID, peerID))
	}

	// The cap has to actually reach the search, not merely be computable.
	e.applyRelayMTUCap(ps)
	ps.pmtuMu.Lock()
	gotCeil := ps.pmtu.ceil
	ps.pmtuMu.Unlock()
	if gotCeil != want {
		t.Fatalf("search ceiling = %d, want %d: the discovery state machine is still free to climb to the unrelayed ceiling", gotCeil, want)
	}

	// A direct session must be left entirely alone — this fix must not shrink
	// the MTU of the peers that are currently working.
	direct := &peerSession{nodeID: "gn-ionos2"}
	direct.initPMTU(flooring, ceiling)
	if cap := e.relayMTUCap(direct); cap != 0 {
		t.Fatalf("relayMTUCap on a direct session = %d, want 0 (no relay, no cap)", cap)
	}
	e.applyRelayMTUCap(direct)
	direct.pmtuMu.Lock()
	gotDirectCeil := direct.pmtu.ceil
	direct.pmtuMu.Unlock()
	if gotDirectCeil != ceiling {
		t.Fatalf("direct session ceiling = %d, want %d untouched", gotDirectCeil, ceiling)
	}
}

// TestPMTUTickAppliesRelayCap exists because the two tests above call
// applyRelayMTUCap directly, and so would both still pass if the cap were
// computed correctly but never wired into the discovery loop — which is the
// only way it has any effect in production. Verified by removing the call from
// pmtuTick: the other two stay green, this one fails.
//
// The session is parked in phaseSettled with revalidation far off so step()
// issues no probe, letting the tick be driven without a transport underneath.
func TestPMTUTickAppliesRelayCap(t *testing.T) {
	const (
		selfID  = "gn-mcfed"
		peerID  = "gn-openbsd"
		ceiling = 9000
		floor   = 1280
	)

	e := &Engine{nodeID: selfID, log: logx.New(io.Discard, logx.LevelDebug)}
	relay := &peerSession{nodeID: "gn-rocky"}
	relay.setEff(ceiling)

	ps := &peerSession{nodeID: peerID, relay: relay}
	ps.initPMTU(floor, ceiling)
	ps.pmtuMu.Lock()
	ps.pmtu.eff, ps.pmtu.low, ps.pmtu.high = ceiling, ceiling, ceiling
	ps.pmtu.phase = phaseSettled
	ps.pmtu.revalAt = time.Now().Add(time.Hour)
	ps.pmtuMu.Unlock()
	ps.setEff(ceiling)

	e.pmtuTick(ps)

	want := ceiling - relayEnvelopeOverheadFor(selfID, peerID)
	ps.pmtuMu.Lock()
	gotCeil, gotEff := ps.pmtu.ceil, ps.pmtu.eff
	ps.pmtuMu.Unlock()
	if gotCeil != want {
		t.Fatalf("after pmtuTick, ceiling = %d, want %d: the cap is not reaching the discovery loop", gotCeil, want)
	}
	if gotEff > want || int(ps.effMTU.Load()) > want {
		t.Fatalf("after pmtuTick, eff = %d / published %d, want <= %d", gotEff, ps.effMTU.Load(), want)
	}
}

// which the field logs show is the one that matters: sessions do not start
// relayed and stay that way. Relay assignments move (gn-debian went cush2 →
// cush1, gn-openbsd cush2 → rocky, within fifteen minutes), and a session that
// converged at 9000 while direct must be pulled back down when it becomes
// relayed, rather than keeping an estimate that is now a hop too large.
func TestRelayedPMTUCapClampsAnAlreadyInflatedSession(t *testing.T) {
	const (
		selfID  = "gn-mcfed"
		peerID  = "gn-win11"
		ceiling = 9000
		floor   = 1280
	)

	e := &Engine{nodeID: selfID, log: logx.New(io.Discard, logx.LevelDebug)}
	ps := &peerSession{nodeID: peerID}
	ps.initPMTU(floor, ceiling)

	// Converged at the full ceiling while the peer was reached directly.
	ps.pmtuMu.Lock()
	ps.pmtu.eff = ceiling
	ps.pmtu.low = ceiling
	ps.pmtu.high = ceiling
	ps.pmtu.phase = phaseSettled
	ps.pmtuMu.Unlock()
	ps.setEff(ceiling)

	// Now it goes relayed.
	relay := &peerSession{nodeID: "gn-rocky"}
	relay.setEff(ceiling)
	ps.mu.Lock()
	ps.relay = relay
	ps.mu.Unlock()

	e.applyRelayMTUCap(ps)

	want := ceiling - relayEnvelopeOverheadFor(selfID, peerID)
	if got := int(ps.effMTU.Load()); got > want {
		t.Fatalf("published effMTU = %d after going relayed, want <= %d: every full-size packet is now one relay envelope too big", got, want)
	}
	ps.pmtuMu.Lock()
	gotEff, gotLow, gotHigh := ps.pmtu.eff, ps.pmtu.low, ps.pmtu.high
	ps.pmtuMu.Unlock()
	for _, c := range []struct {
		name string
		val  int
	}{{"eff", gotEff}, {"low", gotLow}, {"high", gotHigh}} {
		if c.val > want {
			t.Errorf("pmtu.%s = %d, want <= %d: search state left above the relayed ceiling will re-inflate on the next tick", c.name, c.val, want)
		}
	}
}

// TestPMTUCeilingRestoredOnUpgradeToDirect guards the regression the cap nearly
// introduced. Sessions are not statically relayed: when a direct path appears
// the session is upgraded (TestRelayedConnectionUpgradesToDirect), and an
// implementation that only ever lowers the ceiling would leave such a session
// pinned at its old relayed limit forever — quietly capping a peer that no
// longer has a relay in front of it, which is the same bug in the other
// direction, just slower to notice.
func TestPMTUCeilingRestoredOnUpgradeToDirect(t *testing.T) {
	const (
		selfID  = "gn-mcfed"
		peerID  = "gn-openbsd"
		ceiling = 9000
		floor   = 1280
	)

	e := &Engine{
		nodeID:    selfID,
		log:       logx.New(io.Discard, logx.LevelDebug),
		pmtuFloor: floor,
		pmtuCeil:  ceiling,
	}

	relay := &peerSession{nodeID: "gn-rocky"}
	relay.setEff(ceiling)
	ps := &peerSession{nodeID: peerID, relay: relay}
	ps.initPMTU(floor, ceiling)

	e.applyRelayMTUCap(ps)
	capped := ceiling - relayEnvelopeOverheadFor(selfID, peerID)
	ps.pmtuMu.Lock()
	got := ps.pmtu.ceil
	ps.pmtuMu.Unlock()
	if got != capped {
		t.Fatalf("relayed ceiling = %d, want %d", got, capped)
	}

	// A direct path is found and the relay dropped.
	ps.mu.Lock()
	ps.relay = nil
	ps.mu.Unlock()

	e.applyRelayMTUCap(ps)
	ps.pmtuMu.Lock()
	got = ps.pmtu.ceil
	ps.pmtuMu.Unlock()
	if got != ceiling {
		t.Fatalf("ceiling after upgrade to direct = %d, want %d restored: the session is stuck at its old relayed limit", got, ceiling)
	}
}
