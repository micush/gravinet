package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestPeerLocalDisable proves that disabling a peer is local-only and applies
// live (no restart), in contrast to a mesh-wide ban:
//   - disabling peer B on node A disconnects B immediately and refuses its
//     reconnect attempts, while A keeps B's endpoint so it can come back;
//   - no ban record is created on A and nothing is flooded to B (B sees no ban);
//   - re-enabling B on A lets them reconnect automatically.
func TestPeerLocalDisable(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xD15AB1E)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.30.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.30.0.2"))
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

	// Disable peer B locally on A via the reload path (config-driven). Keys nil =
	// keep the current keyset; this only changes the disabled-peer set.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, DisabledPeers: []string{"B"}}); err != nil {
		t.Fatalf("disable reload: %v", err)
	}

	// A drops B right away.
	if !waitUntil(10*time.Second, func() bool { return A.eng.PeerCount(netID) == 0 }) {
		t.Fatal("A is still connected to a locally-disabled peer")
	}
	// And refuses B's reconnect attempts: A must stay at zero across several of
	// B's redial cycles.
	time.Sleep(4 * time.Second)
	if pc := A.eng.PeerCount(netID); pc != 0 {
		t.Fatalf("A reconnected to a disabled peer (PeerCount=%d)", pc)
	}

	// Local-only: no ban was created on A, and nothing was flooded to B.
	if n := len(A.eng.ListBans(netID)); n != 0 {
		t.Fatalf("disabling a peer created %d ban(s) on A; it must be local-only", n)
	}
	if n := len(B.eng.ListBans(netID)); n != 0 {
		t.Fatalf("disabling a peer on A flooded %d ban(s) to B; it must be local-only", n)
	}
	// A reports B in its disabled list.
	dp := A.eng.DisabledPeers(netID)
	if len(dp) != 1 || dp[0].NodeID != "B" {
		t.Fatalf("A's disabled-peer list = %+v, want [B]", dp)
	}

	// Re-enable B locally on A → they reconnect on their own.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID}); err != nil {
		t.Fatalf("enable reload: %v", err)
	}
	if !waitUntil(25*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("A-B did not reconnect after re-enabling the peer")
	}
	if n := len(A.eng.DisabledPeers(netID)); n != 0 {
		t.Fatalf("A still lists %d disabled peer(s) after re-enable", n)
	}
}
