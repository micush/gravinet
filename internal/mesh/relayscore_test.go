package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// mkCandidate builds a bare peerSession for scoring tests: relayed=true gives
// it a non-nil relay (itself reached via a relay, so a chain would stack),
// rttMs<0 leaves rttNanos at its zero value (unmeasured).
func mkCandidate(id string, relayed bool, rttMs int) *peerSession {
	ps := &peerSession{nodeID: id}
	if relayed {
		ps.relay = &peerSession{nodeID: id + "-upstream-relay"}
	}
	if rttMs >= 0 {
		ps.rttNanos.Store(int64(rttMs) * int64(time.Millisecond))
	}
	return ps
}

func TestRelayBetterPrefersDirectOverRelayedRegardlessOfRTT(t *testing.T) {
	direct := mkCandidate("direct", false, 500) // slow, but direct
	relayed := mkCandidate("relayed", true, 10) // fast, but itself relayed
	if !relayBetter(direct, relayed) {
		t.Fatalf("a slow direct candidate should beat a fast already-relayed one (avoids stacking hops)")
	}
	if relayBetter(relayed, direct) {
		t.Fatalf("relayed should not beat direct in the reverse comparison")
	}
}

func TestRelayBetterPrefersLowerRTTWithinSameTier(t *testing.T) {
	fast := mkCandidate("fast", false, 20)
	slow := mkCandidate("slow", false, 200)
	if !relayBetter(fast, slow) {
		t.Fatalf("lower RTT should win among two direct candidates")
	}
	if relayBetter(slow, fast) {
		t.Fatalf("higher RTT should not win")
	}

	// Same comparison, both already relayed (same tier, still compared by RTT).
	fastR := mkCandidate("fastR", true, 20)
	slowR := mkCandidate("slowR", true, 200)
	if !relayBetter(fastR, slowR) {
		t.Fatalf("lower RTT should win among two already-relayed candidates too")
	}
}

func TestRelayBetterUnmeasuredNeverWins(t *testing.T) {
	measured := mkCandidate("measured", false, 900) // slow, but a real sample
	unmeasured := mkCandidate("unmeasured", false, -1)
	if relayBetter(unmeasured, measured) {
		t.Fatalf("an unmeasured candidate should not beat a measured one, even a slow one")
	}
	if !relayBetter(measured, unmeasured) {
		t.Fatalf("a measured candidate should beat an unmeasured one")
	}
	// Two unmeasured: neither beats the other (bestRelay keeps first-seen).
	otherUnmeasured := mkCandidate("other-unmeasured", false, -1)
	if relayBetter(unmeasured, otherUnmeasured) || relayBetter(otherUnmeasured, unmeasured) {
		t.Fatalf("neither of two unmeasured candidates should claim to beat the other")
	}
}

func TestBestRelayPicksLowestRTTAmongReportingCandidates(t *testing.T) {
	target := "target-node"
	a := mkCandidate("A", false, 150)
	b := mkCandidate("B", false, 30) // should win: direct + fastest
	c := mkCandidate("C", true, 5)   // fastest overall, but already relayed
	for _, ps := range []*peerSession{a, b, c} {
		ps.markReported([]string{target})
	}
	// D is connected but never reported knowing the target — must be ignored.
	d := mkCandidate("D", false, 1)
	got, _ := bestRelay([]*peerSession{a, b, c, d}, target)
	if got != b {
		t.Fatalf("bestRelay() = %v, want B (direct, lowest RTT among direct candidates)", got.nodeID)
	}
}

func TestBestRelayIgnoresTargetItselfAndNonReporters(t *testing.T) {
	target := "target-node"
	self := mkCandidate(target, false, 1) // shouldn't relay to ourself via "ourself"
	self.markReported([]string{target})
	stranger := mkCandidate("stranger", false, 1) // never reported knowing target
	if got, _ := bestRelay([]*peerSession{self, stranger}, target); got != nil {
		t.Fatalf("bestRelay() = %v, want nil (no eligible candidate)", got)
	}
}

func TestBestRelayReturnsNilWithNoCandidates(t *testing.T) {
	if got, _ := bestRelay(nil, "whoever"); got != nil {
		t.Fatalf("bestRelay(nil, ...) = %v, want nil", got)
	}
}

// TestKeepaliveRTTCapture exercises the actual send/receive path that feeds
// relay scoring: sendKeepalive stamps pingSentNanos, and onControl's ctrlPong
// case turns that into a stored rttNanos sample — without a real transport,
// so the measured delta is small and deterministic-enough to sanity-check
// (just "recorded, and roughly matches a manually-inserted sleep") rather than
// exact.
func TestKeepaliveRTTCapture(t *testing.T) {
	const netID = uint64(0xE77E)
	sess, err := crypto.NewSession(crypto.DeriveSessionKeys(
		make([]byte, 32), make([]byte, 32), []byte("t"), true))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	eng := NewEngine(Options{
		NodeID: "self",
		Nets: []NetSpec{{ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.Attach(nopSender{})
	ns := eng.network(netID)

	ps := &peerSession{nodeID: "peer", net: ns, sess: sess, remoteIdx: 1,
		endpoint: netip.MustParseAddrPort("203.0.113.9:65432")}
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.mu.Unlock()

	if got := ps.rttNanos.Load(); got != 0 {
		t.Fatalf("rttNanos before any keepalive = %d, want 0", got)
	}

	eng.sendKeepalive(ns) // stamps ps.pingSentNanos and sends ctrlPing (dropped by nopSender)
	if ps.pingSentNanos.Load() == 0 {
		t.Fatalf("sendKeepalive did not stamp pingSentNanos")
	}

	time.Sleep(5 * time.Millisecond) // manufacture a measurable, known-positive gap
	eng.onControl(ps, []byte{ctrlPong})

	rtt := ps.rttNanos.Load()
	if rtt <= 0 {
		t.Fatalf("rttNanos after pong = %d, want > 0", rtt)
	}
	if rtt < int64(4*time.Millisecond) {
		t.Fatalf("rttNanos = %v, want roughly >= the manufactured 5ms gap", time.Duration(rtt))
	}

	// A pong with no prior ping (pingSentNanos reset to 0) must not record
	// a bogus sample.
	ps.pingSentNanos.Store(0)
	ps.rttNanos.Store(0)
	eng.onControl(ps, []byte{ctrlPong})
	if got := ps.rttNanos.Load(); got != 0 {
		t.Fatalf("rttNanos after a pong with no matching ping = %d, want 0", got)
	}
}
