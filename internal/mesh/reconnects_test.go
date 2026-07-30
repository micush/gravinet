package mesh

import (
	"testing"
	"time"
)

// A session dying and being torn down records exactly one reconnect against
// the node it belonged to, with the reason and a fresh timestamp.
func TestTeardownSessionsRecordsReconnect(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]

	ns.nodes["a"] = &nodeInfo{nodeID: "a", hostname: "a", managed: true}
	ps := &peerSession{net: ns, nodeID: "a"}
	ns.byNode["a"] = ps

	e.teardownSessions(ns, []*peerSession{ps}, "pruned dead session")

	ni := ns.nodes["a"]
	if ni.reconnects != 1 {
		t.Errorf("reconnects = %d, want 1", ni.reconnects)
	}
	if ni.lastReconnectReason != "pruned dead session" {
		t.Errorf("lastReconnectReason = %q, want %q", ni.lastReconnectReason, "pruned dead session")
	}
	if time.Since(ni.lastReconnectAt) > 2*time.Second {
		t.Errorf("lastReconnectAt = %v, want just now", ni.lastReconnectAt)
	}
}

// The real scenario this was built to handle correctly: a multi-candidate-
// port pile-up (see NetSpec.ConfiguredSeeds' doc comment for why one exists
// at all) leaves several simultaneous sessions for one node in e.sessions,
// only one of which is ever ns.byNode's actual current session. When a sweep
// reaps several of them for the same node in one batch, only the one that
// was actually current should count — otherwise one churn burst reports as
// N flaps instead of the one that actually mattered.
func TestTeardownSessionsDoesNotInflateOnDuplicateSessions(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]

	ns.nodes["a"] = &nodeInfo{nodeID: "a", hostname: "a", managed: true}
	current := &peerSession{net: ns, nodeID: "a"}
	superseded := &peerSession{net: ns, nodeID: "a"} // a duplicate that never became ns.byNode's entry
	ns.byNode["a"] = current

	e.teardownSessions(ns, []*peerSession{superseded, current}, "pruned dead session")

	if got := ns.nodes["a"].reconnects; got != 1 {
		t.Errorf("reconnects = %d, want 1 (only the session that was actually current should count)", got)
	}
}

// Two separate teardown events accumulate, not overwrite -- the whole point
// of a durable counter is answering "how many times has this happened",
// not just "did it happen at least once".
func TestTeardownSessionsAccumulatesAcrossEvents(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]

	ns.nodes["a"] = &nodeInfo{nodeID: "a", hostname: "a", managed: true}
	ps1 := &peerSession{net: ns, nodeID: "a"}
	ns.byNode["a"] = ps1
	e.teardownSessions(ns, []*peerSession{ps1}, "pruned dead session")

	ps2 := &peerSession{net: ns, nodeID: "a"}
	ns.byNode["a"] = ps2
	e.teardownSessions(ns, []*peerSession{ps2}, "reconnecting stuck session (keepalive stopped completing)")

	ni := ns.nodes["a"]
	if ni.reconnects != 2 {
		t.Errorf("reconnects = %d, want 2", ni.reconnects)
	}
	if ni.lastReconnectReason != "reconnecting stuck session (keepalive stopped completing)" {
		t.Errorf("lastReconnectReason = %q, want the most recent reason", ni.lastReconnectReason)
	}
}

// ListPeers surfaces the counter for a node with a reconnect history, and
// omits it (zero value) for one that's never had one -- a healthy peer's
// payload shouldn't grow fields that are always empty.
func TestListPeersSurfacesReconnects(t *testing.T) {
	e := reflexiveEngine()
	ns := e.netSnapshot()[1]

	ns.nodes["a"] = &nodeInfo{nodeID: "a", hostname: "a", managed: true, lastSeen: time.Now()}
	psA := &peerSession{net: ns, nodeID: "a", hostname: "a"}
	ns.byNode["a"] = psA
	e.teardownSessions(ns, []*peerSession{psA}, "pruned dead session")
	// The real, current session for "a" now, distinct from the one that was
	// just torn down -- reconnects is a property of the node, not tied to
	// any one session object.
	psA2 := &peerSession{net: ns, nodeID: "a", hostname: "a"}
	ns.byNode["a"] = psA2

	ns.nodes["b"] = &nodeInfo{nodeID: "b", hostname: "b", managed: true, lastSeen: time.Now()}
	ns.byNode["b"] = &peerSession{net: ns, nodeID: "b", hostname: "b"}

	peers := e.ListPeers(1)
	got := map[string]PeerInfo{}
	for _, p := range peers {
		got[p.NodeID] = p
	}
	if got["a"].Reconnects != 1 {
		t.Errorf("a: Reconnects = %d, want 1", got["a"].Reconnects)
	}
	if got["a"].LastReconnectReason != "pruned dead session" {
		t.Errorf("a: LastReconnectReason = %q, want %q", got["a"].LastReconnectReason, "pruned dead session")
	}
	if got["b"].Reconnects != 0 || got["b"].LastReconnectReason != "" {
		t.Errorf("b (never reconnected): Reconnects=%d LastReconnectReason=%q, want zero values", got["b"].Reconnects, got["b"].LastReconnectReason)
	}
}
