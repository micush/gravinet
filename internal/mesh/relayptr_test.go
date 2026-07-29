package mesh

import (
	"io"
	"testing"

	"gravinet/internal/logx"
)

// peerSession.relay is a pointer to the relay's own session object. A relay
// re-handshakes far more often than it disconnects, and install() replaces the
// object in byNode rather than mutating it — so without repointing, every peer
// reached through that relay is left holding an orphan. deliver() goes on
// sealing to the orphan's keys and remoteIdx, which the relay node has already
// discarded, and the packets die there with no error, no counter and no log.
//
// Only relayed peers can be affected: a direct session has no such pointer.
// That asymmetry is the whole signature of the bug.

func TestRelayReplacementRepointsDependentSessions(t *testing.T) {
	e := &Engine{nodeID: "A", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{byNode: map[string]*peerSession{}}

	oldRelay := &peerSession{nodeID: "R", net: ns}
	viaRelay := &peerSession{nodeID: "B", net: ns, relay: oldRelay}
	direct := &peerSession{nodeID: "C", net: ns}
	ns.byNode["R"] = oldRelay
	ns.byNode["B"] = viaRelay
	ns.byNode["C"] = direct

	// R re-handshakes: a fresh object replaces it in byNode.
	newRelay := &peerSession{nodeID: "R", net: ns}
	ns.byNode["R"] = newRelay
	e.repointRelayUsers(ns, oldRelay, newRelay)

	if got := viaRelay.getRelay(); got != newRelay {
		which := "the orphaned session"
		if got == nil {
			which = "nil"
		}
		t.Fatalf("B still relays through %s after R re-handshaked: every packet to B is sealed to a session index R has discarded, and dies at R with no error, no counter and no log until B's own timeout reaps it", which)
	}
	if direct.getRelay() != nil {
		t.Fatal("a direct session acquired a relay pointer it never had")
	}
}

func TestRelayTeardownClearsDependentPointers(t *testing.T) {
	e := &Engine{nodeID: "A", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{byNode: map[string]*peerSession{}}

	relay := &peerSession{nodeID: "R", net: ns}
	viaRelay := &peerSession{nodeID: "B", net: ns, relay: relay}
	ns.byNode["B"] = viaRelay

	// R is gone entirely, not replaced.
	e.repointRelayUsers(ns, relay, nil)

	if viaRelay.getRelay() != nil {
		t.Fatal("B still points at a torn-down relay session; it will keep sealing traffic to a discarded index until its own timeout")
	}
}

// The helper must not disturb a peer relaying through a different relay.
func TestRepointLeavesOtherRelaysAlone(t *testing.T) {
	e := &Engine{nodeID: "A", log: logx.New(io.Discard, logx.LevelDebug)}
	ns := &netState{byNode: map[string]*peerSession{}}

	r1 := &peerSession{nodeID: "R1", net: ns}
	r2 := &peerSession{nodeID: "R2", net: ns}
	viaR1 := &peerSession{nodeID: "B", net: ns, relay: r1}
	viaR2 := &peerSession{nodeID: "D", net: ns, relay: r2}
	ns.byNode["B"] = viaR1
	ns.byNode["D"] = viaR2

	newR1 := &peerSession{nodeID: "R1", net: ns}
	e.repointRelayUsers(ns, r1, newR1)

	if viaR1.getRelay() != newR1 {
		t.Fatal("B was not moved onto R1's replacement")
	}
	if viaR2.getRelay() != r2 {
		t.Fatal("D was moved off R2, which was never replaced")
	}
}
