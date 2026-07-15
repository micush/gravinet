package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestNetworkReset proves that ResetNetwork drops a network's current peer
// sessions right away and that the pair reconnects on their own shortly after
// (the seed and learned endpoint survive the reset), and that it errors for
// an unknown network id instead of silently doing nothing.
func TestNetworkReset(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0x2E5E7)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.31.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.31.0.2"))
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
	if !waitUntil(15*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A-B did not connect")
	}

	// Resetting an unknown network id must error, not silently no-op.
	if err := A.eng.ResetNetwork(0xDEADBEEF); err == nil {
		t.Fatal("ResetNetwork on an unknown network id should return an error")
	}

	// Reset drops A's session to B immediately...
	if err := A.eng.ResetNetwork(netID); err != nil {
		t.Fatalf("ResetNetwork: %v", err)
	}
	if pc := A.eng.PeerCount(netID); pc != 0 {
		t.Fatalf("A still shows %d peer(s) immediately after reset", pc)
	}

	// ...and the two sides reconnect on their own shortly after, since the
	// seed and B's learned endpoint are retained across the reset.
	if !waitUntil(15*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A-B did not reconnect after reset")
	}
}
