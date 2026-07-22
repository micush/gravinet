package mesh

import (
	"net/netip"
	"testing"
)

// TestAnnouncePeerChangeQueuesResends checks the reliability mechanism
// itself: since ctrlPeerAdd rides over UDP with no ACK or retry of its own, a
// changed peer must get peerAddResends extra redundant attempts queued, and
// flushPendingPeerAdds must count them down to exactly zero and then stop —
// not resend forever, and not silently drop to zero early.
func TestAnnouncePeerChangeQueuesResends(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", hostname: "h1"}
	ns.mu.Unlock()

	e.announcePeerChange(ns, ns.byNode["peer1"])

	ns.mu.RLock()
	remaining, queued := ns.pendingPeerAdds["peer1"]
	ns.mu.RUnlock()
	if !queued || remaining != peerAddResends {
		t.Fatalf("right after announcePeerChange: want queued with %d remaining, got queued=%v remaining=%d",
			peerAddResends, queued, remaining)
	}

	for i := 1; i <= peerAddResends; i++ {
		e.flushPendingPeerAdds(ns)
		ns.mu.RLock()
		remaining, queued := ns.pendingPeerAdds["peer1"]
		ns.mu.RUnlock()
		if i < peerAddResends {
			if !queued || remaining != peerAddResends-i {
				t.Fatalf("after flush #%d: want queued with %d remaining, got queued=%v remaining=%d",
					i, peerAddResends-i, queued, remaining)
			}
		} else if queued {
			t.Fatalf("after final flush #%d: entry should be gone, still queued with remaining=%d", i, remaining)
		}
	}
}

// TestFlushPendingPeerAddsDropsDisconnectedPeer checks the other half of
// reliability: a peer that disconnects while a resend is still pending must
// be cleaned out of pendingPeerAdds on the next flush, rather than resending
// stale data forever or leaking the map entry.
func TestFlushPendingPeerAddsDropsDisconnectedPeer(t *testing.T) {
	e, ns := testEngineWithNet(t)
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{net: ns, nodeID: "peer1", hostname: "h1"}
	ns.mu.Unlock()

	e.announcePeerChange(ns, ns.byNode["peer1"])

	ns.mu.Lock()
	delete(ns.byNode, "peer1") // disconnects before its resends are exhausted
	ns.mu.Unlock()

	e.flushPendingPeerAdds(ns)

	ns.mu.RLock()
	_, queued := ns.pendingPeerAdds["peer1"]
	ns.mu.RUnlock()
	if queued {
		t.Fatal("flushPendingPeerAdds should drop a disconnected peer instead of continuing to resend it")
	}
}

// TestPeerListSigStableAcrossCalls guards the core assumption
// broadcastGossip's change-gating relies on: calling peerListSig twice with
// nothing changed in between must return identical output, or every
// maintenance tick would look like a change and the optimization would do
// nothing.
func TestPeerListSigStableAcrossCalls(t *testing.T) {
	_, ns := testEngineWithNet(t)
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{nodeID: "peer1", hostname: "h1",
		overlay4: netip.MustParseAddr("10.0.0.5"), endpoint: netip.MustParseAddrPort("1.2.3.4:5")}
	ns.byNode["peer2"] = &peerSession{nodeID: "peer2", hostname: "h2",
		overlay4: netip.MustParseAddr("10.0.0.6"), endpoint: netip.MustParseAddrPort("1.2.3.4:6"),
		managed: true, webPort: 8443}
	ns.mu.Unlock()

	a := ns.peerListSig()
	b := ns.peerListSig()
	if a != b {
		t.Fatalf("peerListSig is not stable across calls with nothing changed:\na=%q\nb=%q", a, b)
	}
	if a == "" {
		t.Fatal("peerListSig returned empty for a non-empty peer set")
	}
}

// TestPeerListSigChangesOnEndpointRoam checks the case that actually matters
// most in practice: a peer's underlay endpoint changing (NAT roam) must
// change the signature, or a roamed peer's new address would never get
// re-gossiped to peers who aren't its direct neighbour.
func TestPeerListSigChangesOnEndpointRoam(t *testing.T) {
	_, ns := testEngineWithNet(t)
	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{nodeID: "peer1", hostname: "h1",
		overlay4: netip.MustParseAddr("10.0.0.5"), endpoint: netip.MustParseAddrPort("1.2.3.4:5")}
	ns.mu.Unlock()
	before := ns.peerListSig()

	ns.mu.Lock()
	ns.byNode["peer1"].endpoint = netip.MustParseAddrPort("1.2.3.4:9999") // roamed to a new port
	ns.mu.Unlock()
	after := ns.peerListSig()

	if before == after {
		t.Fatal("peerListSig did not change when a peer's endpoint roamed")
	}
}

// TestPeerListSigChangesOnMembershipChange checks the other case that
// matters: a peer joining or leaving ns.byNode must change the signature, so
// broadcastGossip doesn't skip telling everyone else about it.
func TestPeerListSigChangesOnMembershipChange(t *testing.T) {
	_, ns := testEngineWithNet(t)
	empty := ns.peerListSig()

	ns.mu.Lock()
	ns.byNode["peer1"] = &peerSession{nodeID: "peer1", hostname: "h1"}
	ns.mu.Unlock()
	withPeer := ns.peerListSig()

	if empty == withPeer {
		t.Fatal("peerListSig did not change when a peer joined")
	}

	ns.mu.Lock()
	delete(ns.byNode, "peer1")
	ns.mu.Unlock()
	afterLeave := ns.peerListSig()

	if afterLeave != empty {
		t.Fatalf("peerListSig after the only peer left should match the empty signature: got %q, want %q", afterLeave, empty)
	}
}

// TestInstallGossipsPeerListToNewPeerImmediately checks the bootstrap path
// that makes the change-gating in broadcastGossip safe: a newly-installed
// session must receive the peer list itself right away, since it has no
// prior state and can't rely on the periodic broadcast (which may now
// legitimately skip ticks where nothing has changed from the perspective of
// peers who are already up to date).
func TestInstallGossipsPeerListToNewPeerImmediately(t *testing.T) {
	e, ns := testEngineWithNet(t)

	// An existing peer, so the peer list the newcomer receives is non-empty,
	// and so announcePeerChange's flood to "everyone else" has a real
	// recipient to exercise (it needs sess too, for the same reason).
	ns.mu.Lock()
	ns.byNode["existing"] = &peerSession{nodeID: "existing", hostname: "h",
		overlay4: netip.MustParseAddr("10.0.0.9"), sess: testSession(t)}
	ns.mu.Unlock()

	newcomer := &peerSession{net: ns, nodeID: "newcomer",
		endpoint: netip.MustParseAddrPort("203.0.113.1:443"), sess: testSession(t)}
	e.install(ns, newcomer)

	got, err := decodePeerList(e.buildPeerList(ns, "__nobody__")[1:])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, en := range got {
		if en.nodeID == "existing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("sanity check failed: buildPeerList should list the existing peer, got %+v", got)
	}
	// The regression this guards against: install() reaching
	// sendControl/sealAndSend for the newcomer's own session without
	// panicking. That path is only exercised at all because install() now
	// gossips to a brand-new peer immediately instead of relying solely on
	// the periodic broadcast, which is change-gated and could otherwise leave
	// a newcomer waiting up to gossipFullRefresh for its first peer list.
}
