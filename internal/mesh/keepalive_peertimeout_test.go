package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func TestKeepaliveIntervalDefaultAndSet(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	if got := e.keepaliveInterval(); got != defaultKeepaliveInterval {
		t.Fatalf("default keepaliveInterval() = %v, want %v", got, defaultKeepaliveInterval)
	}
	e.SetKeepaliveInterval(5 * time.Second)
	if got := e.keepaliveInterval(); got != 5*time.Second {
		t.Fatalf("after SetKeepaliveInterval(5s), keepaliveInterval() = %v, want 5s", got)
	}
	// Non-positive restores the default.
	e.SetKeepaliveInterval(0)
	if got := e.keepaliveInterval(); got != defaultKeepaliveInterval {
		t.Fatalf("after SetKeepaliveInterval(0), keepaliveInterval() = %v, want default %v", got, defaultKeepaliveInterval)
	}
	// Sub-second is floored to 1s, not rejected.
	e.SetKeepaliveInterval(200 * time.Millisecond)
	if got := e.keepaliveInterval(); got != time.Second {
		t.Fatalf("after SetKeepaliveInterval(200ms), keepaliveInterval() = %v, want floored to 1s", got)
	}
}

func TestPeerTimeoutDefaultAndSet(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	if got := e.peerTimeoutDuration(); got != defaultPeerTimeout {
		t.Fatalf("default peerTimeoutDuration() = %v, want %v", got, defaultPeerTimeout)
	}
	e.SetPeerTimeout(45 * time.Second)
	if got := e.peerTimeoutDuration(); got != 45*time.Second {
		t.Fatalf("after SetPeerTimeout(45s), peerTimeoutDuration() = %v, want 45s", got)
	}
	// Non-positive restores the default.
	e.SetPeerTimeout(0)
	if got := e.peerTimeoutDuration(); got != defaultPeerTimeout {
		t.Fatalf("after SetPeerTimeout(0), peerTimeoutDuration() = %v, want default %v", got, defaultPeerTimeout)
	}
}

// TestPeerTimeoutClampsToLiveKeepalive is the load-bearing live-set
// counterpart to the config package's TestPeerTimeoutDurationClampsToKeepalive:
// an explicit SetPeerTimeout below the *current* keepalive interval gets
// clamped up to it, using whatever keepaliveInterval() currently resolves to
// (not a snapshot from construction time) — set keepalive first, then set an
// even-lower peer timeout, and confirm the clamp uses the already-updated
// keepalive value.
func TestPeerTimeoutClampsToLiveKeepalive(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	e.SetKeepaliveInterval(20 * time.Second)
	e.SetPeerTimeout(5 * time.Second) // below the 20s keepalive just set
	if got := e.peerTimeoutDuration(); got != 20*time.Second {
		t.Fatalf("SetPeerTimeout(5s) with a 20s keepalive interval = %v, want clamped to 20s", got)
	}
	// A peer timeout at/above the keepalive interval is left alone.
	e.SetPeerTimeout(30 * time.Second)
	if got := e.peerTimeoutDuration(); got != 30*time.Second {
		t.Fatalf("SetPeerTimeout(30s) with a 20s keepalive interval = %v, want 30s (no clamp needed)", got)
	}
}

// TestLoweredPeerTimeoutPrunesFaster proves the live setting actually
// changes pruneDead's real behavior, not just that the getter returns the
// right number in isolation: with the default (20s) a session idle for 10s
// must survive; with an explicit 5s timeout, that same 10s-idle session must
// be reaped.
func TestLoweredPeerTimeoutPrunesFaster(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	e.Attach(nopSender{})
	ns := e.network(1)

	mkIdleSession := func(id string, idx uint32) *peerSession {
		ps := &peerSession{nodeID: id, net: ns, localIdx: idx,
			endpoint: netip.MustParseAddrPort("203.0.113.1:65432")}
		ps.setLastRx(time.Now().Add(-10 * time.Second)) // idle for 10s
		e.mu.Lock()
		e.sessions[idx] = ps
		e.mu.Unlock()
		ns.mu.Lock()
		ns.byNode[id] = ps
		ns.mu.Unlock()
		return ps
	}

	// Default (20s) peer timeout: a session idle for 10s must survive pruning.
	mkIdleSession("survivor", 1)
	e.pruneDead(ns, time.Now())
	ns.mu.RLock()
	_, alive := ns.byNode["survivor"]
	ns.mu.RUnlock()
	if !alive {
		t.Fatal("session idle for 10s was pruned under the default 20s peer timeout — should have survived")
	}

	// Lower the timeout below the idle duration: the same-shaped session
	// must now be reaped.
	e.SetPeerTimeout(5 * time.Second)
	mkIdleSession("victim", 2)
	e.pruneDead(ns, time.Now())
	ns.mu.RLock()
	_, stillThere := ns.byNode["victim"]
	ns.mu.RUnlock()
	if stillThere {
		t.Fatal("session idle for 10s was not pruned after lowering peer timeout to 5s")
	}
}
