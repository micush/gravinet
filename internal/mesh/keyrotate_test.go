package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestDistributeKeyPropagates proves rotated-key propagation: a key distributed
// on one node reaches a connected peer over the mesh, lands in the peer's LIVE
// key set (so it can authenticate handshakes), is surfaced for persistence, and
// is de-duplicated (a second distribution of the same key is refused, and the
// flood does not loop).
func TestDistributeKeyPropagates(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xC0FFEE)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.40.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.40.0.2"))
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

	// A new key that neither node has yet.
	newKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate rotated key: %v", err)
	}
	rawNew, err := crypto.DecodeKey(newKeyB64)
	if err != nil {
		t.Fatalf("decode rotated key: %v", err)
	}
	newID := crypto.DeriveKeyID(rawNew)

	// Precondition: B does not have the new key in its live set.
	if _, ok := B.eng.network(netID).keys.Load().Lookup(newID); ok {
		t.Fatal("B already had the new key before distribution")
	}

	if err := A.eng.DistributeKey(netID, newKeyB64, "2026-rotation", 0); err != nil {
		t.Fatalf("DistributeKey: %v", err)
	}

	// A applied it locally immediately.
	if _, ok := A.eng.network(netID).keys.Load().Lookup(newID); !ok {
		t.Fatal("distributing node A did not add the key to its own live set")
	}

	// B receives it over the mesh and folds it into its LIVE key set.
	if !waitUntil(10*time.Second, func() bool {
		_, ok := B.eng.network(netID).keys.Load().Lookup(newID)
		return ok
	}) {
		t.Fatal("new key did not propagate into B's live key set")
	}

	// It's surfaced for persistence on B, with its label.
	found := false
	for _, pk := range B.eng.PropagatedKeys(netID) {
		if pk.KeyB64 == newKeyB64 {
			found = true
			if pk.Label != "2026-rotation" {
				t.Fatalf("propagated key label = %q, want %q", pk.Label, "2026-rotation")
			}
		}
	}
	if !found {
		t.Fatal("propagated key not surfaced by B.PropagatedKeys for persistence")
	}

	// De-dup: distributing the same key again is refused (and, since a node that
	// already holds the key stops relaying, the flood cannot loop).
	if err := A.eng.DistributeKey(netID, newKeyB64, "again", 0); err == nil {
		t.Fatal("second DistributeKey of the same key should have been refused")
	}
}

// TestFloodKeyPropagatesOwnedKey proves the web-UI/CLI "Distribute" primitive:
// unlike DistributeKey (for a brand-new key the mesh doesn't have yet, which
// refuses a key the initiator already owns), FloodKey is built for exactly that
// case — a key already sitting in a local slot — and can be called again later
// without being refused, e.g. once more peers have joined.
func TestFloodKeyPropagatesOwnedKey(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xF100D)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.41.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.41.0.2"))
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

	// Simulate the realistic ordering: A already owns this key (as if generated
	// into a slot and picked up on the next reload) *before* distributing it —
	// exactly the case DistributeKey refuses.
	ownKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate owned key: %v", err)
	}
	rawOwn, err := crypto.DecodeKey(ownKeyB64)
	if err != nil {
		t.Fatalf("decode owned key: %v", err)
	}
	ownID := crypto.DeriveKeyID(rawOwn)
	ns := A.eng.network(netID)
	ns.mu.Lock()
	ns.keys.Store(ns.keys.Load().With(rawOwn))
	ns.mu.Unlock()

	// DistributeKey refuses it outright, precisely because A already has it.
	if err := A.eng.DistributeKey(netID, ownKeyB64, "own", 0); err == nil {
		t.Fatal("DistributeKey should refuse a key A already owns")
	}

	// FloodKey doesn't: it floods A's own key straight to connected peers.
	if err := A.eng.FloodKey(netID, ownKeyB64, "own-key", 0, -1); err != nil {
		t.Fatalf("FloodKey: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		_, ok := B.eng.network(netID).keys.Load().Lookup(ownID)
		return ok
	}) {
		t.Fatal("owned key did not propagate into B's live key set via FloodKey")
	}

	// Unlike DistributeKey, calling FloodKey again on the same key is fine (e.g.
	// re-distributing after a new peer joins) — not refused as a duplicate.
	if err := A.eng.FloodKey(netID, ownKeyB64, "own-key", 0, -1); err != nil {
		t.Fatalf("second FloodKey call should not be refused: %v", err)
	}
}
