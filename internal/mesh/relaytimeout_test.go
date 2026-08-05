package mesh

import (
	"testing"
	"time"
)

// A relayed session's keepalives cross the relay hop in addition to their own,
// so every hop's loss multiplies and it has strictly less margin than a direct
// session at the same per-link loss rate. With the default 30s timeout against a
// 10s keepalive cadence a session gets three attempts; at 25% loss on the relay
// leg that is a ~1.5% chance of a spurious teardown per window, which over a
// 75-second window is roughly one in six. That is the rate at which
// TestRelayedSessionSurvivesLossOnTheRelayLeg failed, and the field symptom was
// relayed sessions being reaped while every direct session on the node held.
func TestRelayedSessionsGetMoreTimeoutMargin(t *testing.T) {
	e := &Engine{}
	direct := &peerSession{nodeID: "d"}
	relayed := &peerSession{nodeID: "r"}
	relayed.relay = &peerSession{nodeID: "via"}

	base := e.peerTimeoutDuration()
	if got := e.sessionTimeoutFor(direct); got != base {
		t.Errorf("direct session timeout = %v, want the configured %v", got, base)
	}
	if got := e.sessionTimeoutFor(relayed); got != base*relayedTimeoutFactor {
		t.Errorf("relayed session timeout = %v, want %v", got, base*relayedTimeoutFactor)
	}
	// The margin is only meaningful as a count of keepalive attempts; assert
	// that rather than the raw duration, since either constant could move.
	attempts := e.sessionTimeoutFor(relayed) / defaultKeepaliveInterval
	if attempts < 5 {
		t.Errorf("a relayed session gets only %d keepalive attempts before being reaped; "+
			"three was demonstrably too few on a lossy relay leg", attempts)
	}
}

// A live SetPeerTimeout must still be respected — the factor multiplies the
// configured value, it does not replace it.
func TestRelayedTimeoutFollowsConfiguredPeerTimeout(t *testing.T) {
	e := &Engine{}
	e.SetPeerTimeout(90 * time.Second)
	relayed := &peerSession{nodeID: "r"}
	relayed.relay = &peerSession{nodeID: "via"}
	if got, want := e.sessionTimeoutFor(relayed), 90*time.Second*relayedTimeoutFactor; got != want {
		t.Errorf("got %v, want %v: the factor multiplies the configured timeout", got, want)
	}
}

// Detection latency for a dead relayed peer is the cost being paid here. Pin it
// so the tradeoff stays visible and bounded rather than drifting upward.
func TestRelayedTimeoutCostIsBounded(t *testing.T) {
	e := &Engine{}
	if d := e.sessionTimeoutFor(&peerSession{relay: &peerSession{}}); d > time.Minute {
		t.Errorf("a dead relayed peer would take %v to notice; that is too long a price for margin", d)
	}
}
