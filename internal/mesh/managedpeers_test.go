package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func TestBetterManagedPrefersReachable(t *testing.T) {
	now := time.Now()
	reachableOld := ManagedPeer{Overlay4: netip.MustParseAddr("10.2.0.5"), WebPort: 8443, LastSeen: now.Add(-time.Minute)}
	unreachableNew := ManagedPeer{Overlay4: netip.MustParseAddr("10.1.0.5"), WebPort: 0, LastSeen: now}
	if !betterManaged(reachableOld, unreachableNew) {
		t.Fatal("a reachable (manageable) entry must beat a newer unreachable one")
	}
	if betterManaged(unreachableNew, reachableOld) {
		t.Fatal("an unreachable entry must not beat a reachable one even if newer")
	}
	// Among two reachable entries, the connected/most-recent one wins.
	a := ManagedPeer{Overlay4: netip.MustParseAddr("10.1.0.1"), WebPort: 1, LastSeen: now, Connected: true}
	b := ManagedPeer{Overlay4: netip.MustParseAddr("10.2.0.1"), WebPort: 1, LastSeen: now.Add(time.Hour), Connected: false}
	if !betterManaged(a, b) {
		t.Fatal("connected should beat disconnected among manageable entries")
	}
}

// TestManagedPeersSurfacesManagerFlag locks in that ManagedPeers reports each
// peer's own advertised Manager state (not just whether it's Managed/
// reachable) — this is what lets the Speedtest "from" picker in the web UI
// tell apart a peer that can only be a target from one that can also be the
// client. Regression test: before this field existed, a Managed-but-not-
// Manager peer looked identical to a Manager one in this API, and picking it
// as the client failed downstream with a 401 the picker had no way to
// prevent.
func TestManagedPeersSurfacesManagerFlag(t *testing.T) {
	e := NewEngine(Options{
		NodeID: "self",
		Nets:   []NetSpec{{ID: 1, Name: "n1", Dev: newFakeDev("a")}},
	})
	now := time.Now()
	snap := e.netSnapshot()
	snap[1].nodes["mgr"] = &nodeInfo{nodeID: "mgr", hostname: "manager-node", managed: true, manager: true,
		overlay4: netip.MustParseAddr("10.1.0.5"), webPort: 8443, lastSeen: now}
	snap[1].nodes["plain"] = &nodeInfo{nodeID: "plain", hostname: "plain-node", managed: true, manager: false,
		overlay4: netip.MustParseAddr("10.1.0.6"), webPort: 8443, lastSeen: now}

	got := map[string]ManagedPeer{}
	for _, p := range e.ManagedPeers(time.Minute) {
		got[p.NodeID] = p
	}
	if mgr, ok := got["mgr"]; !ok || !mgr.Manager {
		t.Fatalf("manager node should have Manager=true, got %+v ok=%v", mgr, ok)
	}
	if plain, ok := got["plain"]; !ok || plain.Manager {
		t.Fatalf("non-manager node should have Manager=false, got %+v ok=%v", plain, ok)
	}
}

// TestManagedPeersReachableAcrossNetworks reproduces the dropdown half of the
// multi-network bug: a node that joins two networks under one id, reachable only
// via the second network, must still appear (and as manageable) even when its
// first-network entry is newer but unreachable.
func TestManagedPeersReachableAcrossNetworks(t *testing.T) {
	e := NewEngine(Options{
		NodeID: "self",
		Nets: []NetSpec{
			{ID: 1, Name: "n1", Dev: newFakeDev("a")},
			{ID: 2, Name: "n2", Dev: newFakeDev("b")},
		},
	})
	now := time.Now()
	snap := e.netSnapshot()
	// Node X: present on both networks, reachable only on net2; net1 entry newer.
	snap[1].nodes["X"] = &nodeInfo{nodeID: "X", hostname: "node-x", managed: true,
		overlay4: netip.MustParseAddr("10.1.0.5"), webPort: 0, lastSeen: now}
	snap[2].nodes["X"] = &nodeInfo{nodeID: "X", hostname: "node-x", managed: true,
		overlay4: netip.MustParseAddr("10.2.0.5"), webPort: 8443, lastSeen: now.Add(-10 * time.Second)}
	// Node Y: only on net2, reachable — must also show up.
	snap[2].nodes["Y"] = &nodeInfo{nodeID: "Y", hostname: "node-y", managed: true,
		overlay4: netip.MustParseAddr("10.2.0.6"), webPort: 8443, lastSeen: now}

	peers := e.ManagedPeers(time.Minute)
	got := map[string]ManagedPeer{}
	for _, p := range peers {
		got[p.NodeID] = p
	}
	if len(peers) != 2 {
		t.Fatalf("expected 2 unique nodes, got %d: %+v", len(peers), peers)
	}
	x, ok := got["X"]
	if !ok {
		t.Fatal("node X missing from managed peers")
	}
	if !x.manageable() || x.Network != 2 {
		t.Fatalf("node X should be represented by its reachable net2 entry, got %+v", x)
	}
	if y, ok := got["Y"]; !ok || !y.manageable() {
		t.Fatalf("net2-only node Y should appear and be manageable, got %+v ok=%v", y, ok)
	}
}
