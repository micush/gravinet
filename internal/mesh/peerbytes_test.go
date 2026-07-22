package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestPeerByteAndPacketCounters proves PeerInfo's TxBytes/RxBytes and
// TxPackets/RxPackets actually count wire traffic: two engines over real
// loopback UDP, a data packet each way, and both sides must report
// non-zero, plausibly-sized counters for the other.
func TestPeerByteAndPacketCounters(t *testing.T) {
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

	type counters struct{ tx, rx, txPkts, rxPkts uint64 }
	find := func(n *testNode, id string) counters {
		for _, pi := range n.eng.ListPeers(netID) {
			if pi.NodeID == id {
				return counters{pi.TxBytes, pi.RxBytes, pi.TxPackets, pi.RxPackets}
			}
		}
		t.Fatalf("peer %s not listed", id)
		return counters{}
	}
	a, b := find(A, "B"), find(B, "A")
	// Handshake traffic doesn't count (no session yet when it flows), but the
	// data packet plus keepalives do; the floor is the sealed data packet
	// (header + inner + tag > len(pkt)) and at least one datagram each way.
	if a.tx == 0 || a.rx == 0 || b.tx == 0 || b.rx == 0 {
		t.Fatalf("zero byte counter: A tx=%d rx=%d, B tx=%d rx=%d", a.tx, a.rx, b.tx, b.rx)
	}
	if a.tx < uint64(len(pkt)) || b.rx < uint64(len(pkt)) {
		t.Fatalf("byte counters smaller than one sealed data packet: A tx=%d, B rx=%d, pkt=%d", a.tx, b.rx, len(pkt))
	}
	if a.txPkts == 0 || a.rxPkts == 0 || b.txPkts == 0 || b.rxPkts == 0 {
		t.Fatalf("zero packet counter: A tx=%d rx=%d, B tx=%d rx=%d", a.txPkts, a.rxPkts, b.txPkts, b.rxPkts)
	}
	// The one overlay packet each way is a single unfragmented datagram (well
	// under any underlay MTU), so byte and packet counts should move together:
	// a byte count many datagrams' worth larger than its packet count would
	// mean something other than "one send == one packet" is happening.
	if a.tx/a.txPkts > 2000 {
		t.Fatalf("A's tx bytes-per-packet (%d/%d) implausible for an unfragmented test packet", a.tx, a.txPkts)
	}
	if b.rx/b.rxPkts > 2000 {
		t.Fatalf("B's rx bytes-per-packet (%d/%d) implausible for an unfragmented test packet", b.rx, b.rxPkts)
	}
}
