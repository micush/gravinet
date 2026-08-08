package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// mcfed's failure, reduced. Its node id (0916a3a70b1d5f4c) sorted below every
// other node in the mesh, so onHSInit's tie-break always took the "ours wins"
// branch — it never once took the self-healing branch that defers to the peer.
// Combined with a pending that outlived the paths that delete it, that meant
// permanently ignoring those peers' handshake inits.
//
// The symptom was one-directional and quiet: mcfed's own session stayed up and
// answered keepalives (rtt measured fine, 47 rx packets of pure keepalive)
// while the peer re-initiated every few seconds because from its side nothing
// had completed. It landed on a different arbitrary subset of peers on each
// boot — whichever addresses happened to leave an orphan behind — so a restart
// moved the outage instead of clearing it.
func TestStalePendingDoesNotSuppressInboundForever(t *testing.T) {
	_, ns := testEngineWithNet(t)
	ep := netip.MustParseAddrPort("192.168.5.105:65432")

	ns.mu.Lock()
	ns.pending[1] = &pendingHS{idxI: 1, endpoint: ep, started: time.Now()}
	ns.mu.Unlock()

	// Fresh: the tie-break is entitled to hold, and must, or a genuine
	// simultaneous open would install two sessions for one pair.
	if stale := pendingStaleForTest(ns, 1); stale {
		t.Fatal("a just-started handshake was called stale; a real attempt would lose ties it should win")
	}

	// Aged past the window: our attempt is not going to complete, and holding
	// the tie open only blocks the peer's.
	ns.mu.Lock()
	ns.pending[1].started = time.Now().Add(-handshakeStaleForTieBreak - time.Second)
	ns.mu.Unlock()

	if stale := pendingStaleForTest(ns, 1); !stale {
		t.Fatalf("a pending older than %v still counted as in flight — this is the state that suppressed "+
			"every inbound init from a peer indefinitely", handshakeStaleForTieBreak)
	}
}

func pendingStaleForTest(ns *netState, idx uint32) bool {
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	p := ns.pending[idx]
	if p == nil {
		return false
	}
	return time.Since(p.started) > handshakeStaleForTieBreak
}

// The window has to comfortably exceed a full planHandshake cycle, or a
// legitimate attempt working through the key order would be declared stale and
// lose a tie it was about to win.
func TestStaleWindowExceedsAFullHandshakeCycle(t *testing.T) {
	// planHandshake re-sends once per handshakeRetry per key. Four keys is
	// more than any configured order in this tree.
	const keys = 4
	if cycle := handshakeRetry * keys; handshakeStaleForTieBreak <= cycle {
		t.Fatalf("handshakeStaleForTieBreak (%v) does not cover a %d-key cycle (%v); real attempts would be "+
			"cut off mid-flight", handshakeStaleForTieBreak, keys, cycle)
	}
}

// tryRelays swept only relayed pendings. A direct pending has no other
// time-based exit: onHSResp deletes it when a response arrives, and
// planHandshake deletes it after exhausting the key order — but planHandshake
// finds it only by scanning for pp.endpoint == seed, so it must be called
// again for that same address. When an address stops being dialed, nothing
// ever removes the entry.
func TestTryRelaysSweepsStaleDirectPendings(t *testing.T) {
	e, ns := testEngineWithNet(t)
	fresh := netip.MustParseAddrPort("192.168.5.105:65432")
	orphan := netip.MustParseAddrPort("192.168.5.108:65432")

	ns.mu.Lock()
	ns.pending[1] = &pendingHS{idxI: 1, endpoint: fresh, started: time.Now()}
	// Direct (relay == nil) and long past the TTL: the leaked shape.
	ns.pending[2] = &pendingHS{idxI: 2, endpoint: orphan, started: time.Now().Add(-relayPendingTTL - time.Minute)}
	ns.mu.Unlock()

	e.tryRelays(ns)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if _, ok := ns.pending[2]; ok {
		t.Fatal("a stale direct pending survived the sweep; it will keep winning the tie-break and " +
			"silently suppress that peer's handshakes")
	}
	if _, ok := ns.pending[1]; !ok {
		t.Fatal("the sweep took a live pending with it; planHandshake was still managing that attempt")
	}
}

// Relayed pendings must still be swept — that was the sweep's original job and
// widening it must not have narrowed it.
func TestTryRelaysStillSweepsStaleRelayPendings(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ns.mu.Lock()
	ns.pending[7] = &pendingHS{
		idxI:       7,
		targetNode: "somepeer",
		relay:      &peerSession{nodeID: "somerelay"},
		started:    time.Now().Add(-relayPendingTTL - time.Second),
	}
	ns.mu.Unlock()

	e.tryRelays(ns)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if _, ok := ns.pending[7]; ok {
		t.Fatal("stale relayed pending survived; startRelayHandshake will refuse to retry this target")
	}
}
