package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestPeerEstablishedAt proves that ListPeers reports a stable "session
// established" timestamp for a connected peer: it's set once when the
// session comes up, stays put across repeated ListPeers calls while the
// session is alive, and jumps forward to a newer time after the session is
// torn down and re-established (e.g. via ResetNetwork).
func TestPeerEstablishedAt(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xE578115)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.32.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.32.0.2"))
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

	before := time.Now().UnixNano()
	peers := A.eng.ListPeers(netID)
	if len(peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(peers))
	}
	first := peers[0].EstablishedAt
	if first == 0 {
		t.Fatal("EstablishedAt was not set on a connected peer")
	}
	if first > before {
		t.Fatalf("EstablishedAt (%d) is after the check point (%d)", first, before)
	}

	// A moment later, with nothing having changed, the timestamp must be
	// unchanged — it's when the session came up, not "now".
	time.Sleep(1200 * time.Millisecond)
	again := A.eng.ListPeers(netID)[0].EstablishedAt
	if again != first {
		t.Fatalf("EstablishedAt drifted from %d to %d without a reconnect", first, again)
	}

	// Reset the network: the session is torn down and comes back up, so the
	// timestamp must advance to a newer value once reconnected.
	if err := A.eng.ResetNetwork(netID); err != nil {
		t.Fatalf("ResetNetwork: %v", err)
	}
	if !waitUntil(15*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A-B did not reconnect after reset")
	}
	after := A.eng.ListPeers(netID)[0].EstablishedAt
	if after <= first {
		t.Fatalf("EstablishedAt did not advance after reconnect: before=%d after=%d", first, after)
	}
}
