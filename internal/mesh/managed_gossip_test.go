package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestPeerListCarriesManaged locks the wire format: managed status and web port
// survive a gossip encode/decode round-trip, so management can propagate beyond
// direct neighbours.
func TestPeerListCarriesManaged(t *testing.T) {
	entries := []peerEntry{
		{nodeID: "X", hostname: "hx", overlay4: netip.MustParseAddr("10.0.0.5"),
			endpoint: netip.MustParseAddrPort("1.2.3.4:5"), managed: true, webPort: 8443},
		{nodeID: "Y", hostname: "hy", overlay4: netip.MustParseAddr("10.0.0.6"),
			endpoint: netip.MustParseAddrPort("1.2.3.4:6"), managed: false},
	}
	dec, err := decodePeerList(encodePeerList(entries))
	if err != nil {
		t.Fatal(err)
	}
	if len(dec) != 2 {
		t.Fatalf("got %d entries, want 2", len(dec))
	}
	var x, y *peerEntry
	for i := range dec {
		switch dec[i].nodeID {
		case "X":
			x = &dec[i]
		case "Y":
			y = &dec[i]
		}
	}
	if x == nil || !x.managed || x.webPort != 8443 {
		t.Fatalf("managed/webPort not carried for X: %+v", x)
	}
	if y == nil || y.managed || y.webPort != 0 {
		t.Fatalf("Y should be unmanaged with no port: %+v", y)
	}
}

// TestManagedDiscoveryMeshWide checks that in a 3-node mesh a managed node is
// discoverable by every other node — not only its direct seed neighbour —
// carrying its web port, which is what makes a non-neighbour peer manageable.
func TestManagedDiscoveryMeshWide(t *testing.T) {
	const netID = uint64(0x3C0DE)
	key, _ := crypto.GenerateKey()
	// B is the hub; C is the managed node, seeded only to B.
	A := spinManaged(t, "A", netID, key, netip.MustParseAddr("10.9.0.1"), false, 0)
	B := spinManaged(t, "B", netID, key, netip.MustParseAddr("10.9.0.2"), false, 0)
	C := spinManaged(t, "C", netID, key, netip.MustParseAddr("10.9.0.3"), true, 9443)
	defer func() {
		for _, n := range []*testNode{A, B, C} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	ap := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }
	// A knows only B; C knows only B; B knows A and C. A and C are not seeded to
	// each other — A must learn C (and its managed/web port) through the mesh.
	A.eng.AddSeed(netID, ap(B))
	C.eng.AddSeed(netID, ap(B))
	B.eng.AddSeed(netID, ap(A))
	B.eng.AddSeed(netID, ap(C))

	seesCManaged := func(n *testNode) bool {
		for _, m := range n.eng.ManagedPeers(time.Minute) {
			if m.NodeID == "C" && m.WebPort == 9443 && (m.Overlay4.IsValid() || m.Overlay6.IsValid()) {
				return true
			}
		}
		return false
	}
	if !waitUntil(20*time.Second, func() bool { return seesCManaged(A) && seesCManaged(B) }) {
		t.Fatalf("not every node discovered managed C; A=%+v B=%+v", A.eng.ManagedPeers(time.Minute), B.eng.ManagedPeers(time.Minute))
	}
}

// TestManagedLearnedViaGossipOnly isolates the gossip path: A learns a managed
// node C purely from a gossiped peer list (C's endpoint is unreachable, so no
// direct session forms). Without managed status in gossip, A could never manage
// a peer it isn't directly connected to (e.g. a relayed or multi-hop peer).
func TestManagedLearnedViaGossipOnly(t *testing.T) {
	const netID = uint64(0x90551)
	key, _ := crypto.GenerateKey()
	A := spinManaged(t, "A", netID, key, netip.MustParseAddr("10.9.1.1"), false, 0)
	B := spinManaged(t, "B", netID, key, netip.MustParseAddr("10.9.1.2"), false, 0)
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}
	ns := A.eng.network(netID)
	ns.mu.RLock()
	sessB := ns.byNode["B"]
	ns.mu.RUnlock()
	if sessB == nil {
		t.Fatal("no session to B")
	}

	// Gossip advertising managed node C, which A is not connected to; the
	// endpoint is unroutable (TEST-NET-1) so A can't form a direct session.
	entries := []peerEntry{{
		nodeID: "C", hostname: "hc",
		overlay4: netip.MustParseAddr("10.9.1.9"),
		endpoint: netip.MustParseAddrPort("192.0.2.1:9999"),
		managed:  true, webPort: 9443,
	}}
	A.eng.learnPeers(sessB, entries)

	found := false
	for _, m := range A.eng.ManagedPeers(time.Minute) {
		if m.NodeID == "C" && m.WebPort == 9443 && m.Overlay4 == netip.MustParseAddr("10.9.1.9") {
			found = true
		}
	}
	if !found {
		t.Fatalf("A did not learn managed C from gossip; got %+v", A.eng.ManagedPeers(time.Minute))
	}
	ns.mu.RLock()
	_, direct := ns.byNode["C"]
	ns.mu.RUnlock()
	if direct {
		t.Fatal("test invalid: A formed a direct session to C (should be gossip-only)")
	}
}
