package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestPeerByteCounters proves PeerInfo's TxBytes/RxBytes actually count wire
// traffic: two engines over real loopback UDP, a data packet each way, and
// both sides must report non-zero, plausibly-sized counters for the other.
func TestPeerByteCounters(t *testing.T) {
	const netID = uint64(0xB17E5)
	key, _ := crypto.GenerateKey()
	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.5.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.5.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	// Seed one direction only. Seeding both makes A and B initiate
	// simultaneously (handshake glare): twin session pairs form, byNode keeps
	// whichever installed last on each side, and the peers can keep talking on
	// the twin byNode no longer points at — so each side's visible counters
	// describe a session the other side isn't using. That is established glare
	// behaviour (pruning converges it); this test wants the counters, not the
	// glare, so it forms the session the way most real peers do: one side
	// dials, the other learns the peer from the inbound handshake.
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	if !waitUntil(8*time.Second, func() bool {
		return A.eng.connectedToNode(A.eng.network(netID), "B") && B.eng.connectedToNode(B.eng.network(netID), "A")
	}) {
		t.Fatal("A and B never connected")
	}

	// One overlay packet A->B and one B->A.
	pkt := makeIPv4(netip.MustParseAddr("10.5.0.1"), netip.MustParseAddr("10.5.0.2"), []byte{1, 2, 3, 4})
	A.dev.in <- pkt
	select {
	case <-B.dev.out:
	case <-time.After(5 * time.Second):
		t.Fatal("A->B packet not delivered")
	}
	back := makeIPv4(netip.MustParseAddr("10.5.0.2"), netip.MustParseAddr("10.5.0.1"), []byte{5, 6, 7, 8})
	B.dev.in <- back
	select {
	case <-A.dev.out:
	case <-time.After(5 * time.Second):
		t.Fatal("B->A packet not delivered")
	}

	find := func(n *testNode, id string) (tx, rx uint64) {
		for _, pi := range n.eng.ListPeers(netID) {
			if pi.NodeID == id {
				return pi.TxBytes, pi.RxBytes
			}
		}
		t.Fatalf("peer %s not listed", id)
		return 0, 0
	}
	atx, arx := find(A, "B")
	btx, brx := find(B, "A")
	// Handshake bytes don't count (no session yet when they flow), but the
	// data packet plus keepalives do; the floor is the sealed data packet
	// (header + inner + tag > len(pkt)).
	if atx == 0 || arx == 0 || btx == 0 || brx == 0 {
		t.Fatalf("zero counter: A tx=%d rx=%d, B tx=%d rx=%d", atx, arx, btx, brx)
	}
	if atx < uint64(len(pkt)) || brx < uint64(len(pkt)) {
		t.Fatalf("counters smaller than one sealed data packet: A tx=%d, B rx=%d, pkt=%d", atx, brx, len(pkt))
	}
}
