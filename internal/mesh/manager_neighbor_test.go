package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestIsManagerNeighborAddrRequiresDirectSession is the trust anchor for the
// remote-upgrade path. IsManagerAddr accepts a manager known through gossip;
// IsManagerNeighborAddr must accept ONLY one this node holds a live direct
// session with. The two together are what lets the webadmin remote-apply gate
// refuse a mislabeled address that could otherwise run a binary as root.
func TestIsManagerNeighborAddrRequiresDirectSession(t *testing.T) {
	const netID = uint64(0x1234)
	eng, ns := newBanTestEngine(t, "self", netID, time.Hour)

	directAddr := netip.MustParseAddr("10.0.0.2")
	gossipAddr := netip.MustParseAddr("10.0.0.3")

	ns.mu.Lock()
	// A manager we hold a live direct session with: present in BOTH the node
	// registry (as manager) and byNode (as a live session).
	ns.nodes["mgr-direct"] = &nodeInfo{nodeID: "mgr-direct", manager: true, overlay4: directAddr}
	ns.byNode["mgr-direct"] = &peerSession{nodeID: "mgr-direct", net: ns}
	// A manager known ONLY through gossip: in the registry as a manager, but
	// with no live session in byNode.
	ns.nodes["mgr-gossip"] = &nodeInfo{nodeID: "mgr-gossip", manager: true, overlay4: gossipAddr}
	ns.mu.Unlock()

	// IsManagerAddr (the loose check) accepts both — that's its documented
	// behavior and what ordinary management relies on.
	if !eng.IsManagerAddr(directAddr) {
		t.Error("IsManagerAddr should accept the direct manager")
	}
	if !eng.IsManagerAddr(gossipAddr) {
		t.Error("IsManagerAddr should accept the gossip-only manager (its documented behavior)")
	}

	// IsManagerNeighborAddr (the strict check) accepts ONLY the direct one.
	if !eng.IsManagerNeighborAddr(directAddr) {
		t.Error("IsManagerNeighborAddr must accept a manager we hold a direct session with")
	}
	if eng.IsManagerNeighborAddr(gossipAddr) {
		t.Fatal("IsManagerNeighborAddr accepted a GOSSIP-ONLY manager — this is the exact spoofing gap the strict check exists to close")
	}

	// A manager address that matches an overlay but whose node is a live
	// session yet NOT flagged manager must also be rejected (belt and braces:
	// the manager flag and the session must coincide on the same node).
	nonMgr := netip.MustParseAddr("10.0.0.4")
	ns.mu.Lock()
	ns.nodes["plain-peer"] = &nodeInfo{nodeID: "plain-peer", manager: false, overlay4: nonMgr}
	ns.byNode["plain-peer"] = &peerSession{nodeID: "plain-peer", net: ns}
	ns.mu.Unlock()
	if eng.IsManagerNeighborAddr(nonMgr) {
		t.Error("IsManagerNeighborAddr accepted a direct session that is not a manager")
	}
}
