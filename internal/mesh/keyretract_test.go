package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestFloodKeyRelabelsExistingKey proves a key already known to a peer gets
// its label updated (and re-flooded once) when FloodKey is called again with
// a different label — the "rename a distributed key" path — while a repeat
// call with the *same* label is a true no-op (nothing to reconcile).
func TestFloodKeyRelabelsExistingKey(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xBEEF03)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.46.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.46.0.2"))
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

	sharedKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate shared key: %v", err)
	}
	if err := A.eng.FloodKey(netID, sharedKeyB64, "original-name", 0, -1); err != nil {
		t.Fatalf("FloodKey: %v", err)
	}
	labelOn := func(eng *Engine) string {
		for _, pk := range eng.PropagatedKeys(netID) {
			if pk.KeyB64 == sharedKeyB64 {
				return pk.Label
			}
		}
		return ""
	}
	if !waitUntil(10*time.Second, func() bool { return labelOn(B.eng) == "original-name" }) {
		t.Fatal("B never saw the key under its original label")
	}

	// Relabel: same key material, new label.
	if err := A.eng.FloodKey(netID, sharedKeyB64, "renamed", 0, -1); err != nil {
		t.Fatalf("FloodKey (relabel): %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { return labelOn(B.eng) == "renamed" }) {
		t.Fatal("B's label was not updated by the relabel")
	}
}

// TestFloodKeyUpdatesExpiryOnExistingKey mirrors
// TestFloodKeyRelabelsExistingKey for expiry: reproduces the reported gap
// where a key first distributed with no expiry, then given one afterward
// (label unchanged throughout, the same way an admin would set "never" ->
// a real date on an already-distributed key), must still propagate the new
// expiry to a peer already holding the key — not just a label change.
func TestFloodKeyUpdatesExpiryOnExistingKey(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xBEEF04)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.46.1.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.46.1.2"))
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

	sharedKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate shared key: %v", err)
	}
	// Distribute with no expiry ("never"), same as testkey7 originally was.
	if err := A.eng.FloodKey(netID, sharedKeyB64, "testkey7", 0, -1); err != nil {
		t.Fatalf("FloodKey: %v", err)
	}
	expiresOn := func(eng *Engine) string {
		for _, pk := range eng.PropagatedKeys(netID) {
			if pk.KeyB64 == sharedKeyB64 {
				return pk.Expires
			}
		}
		return "unset"
	}
	if !waitUntil(10*time.Second, func() bool { return expiresOn(B.eng) == "" }) {
		t.Fatalf("B never saw the key with no expiry, got %q", expiresOn(B.eng))
	}

	// Set an expiry after the fact, label unchanged — the exact scenario
	// reported: mcfed set testkey7's expiry to 2026-07-06T23:59:00Z after it
	// was already distributed and enabled on gn-bsd.
	newExpiry := time.Date(2026, 7, 6, 23, 59, 0, 0, time.UTC)
	if err := A.eng.FloodKey(netID, sharedKeyB64, "testkey7", newExpiry.UnixNano(), -1); err != nil {
		t.Fatalf("FloodKey (expiry change): %v", err)
	}
	want := newExpiry.Format(time.RFC3339)
	if !waitUntil(10*time.Second, func() bool { return expiresOn(B.eng) == want }) {
		t.Fatalf("B's expiry was not updated: got %q, want %q", expiresOn(B.eng), want)
	}
}

// TestRetractKeyPropagatesToHoldingPeer proves the wire half of retraction: a
// peer currently holding the key gets it marked in RetractedKeys (so the
// persist hook will remove it from config) after RetractKey floods a
// retraction for it.
func TestRetractKeyPropagatesToHoldingPeer(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xBEEF01)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.44.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.44.0.2"))
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

	extraKeyB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate extra key: %v", err)
	}
	raw, err := crypto.DecodeKey(extraKeyB64)
	if err != nil {
		t.Fatalf("decode extra key: %v", err)
	}
	id := crypto.DeriveKeyID(raw)

	if err := A.eng.FloodKey(netID, extraKeyB64, "extra", 0, -1); err != nil {
		t.Fatalf("FloodKey: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		_, ok := B.eng.network(netID).keys.Load().Lookup(id)
		return ok
	}) {
		t.Fatal("extra key did not propagate to B before the retraction test could begin")
	}

	if err := A.eng.RetractKey(netID, extraKeyB64); err != nil {
		t.Fatalf("RetractKey: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool {
		for _, got := range B.eng.RetractedKeys(netID) {
			if got == id {
				return true
			}
		}
		return false
	}) {
		t.Fatal("B was not marked to retract the key after RetractKey")
	}
}

// TestRetractKeyIgnoredByNonHoldingPeer proves the flood-termination half:
// a peer that never had the key isn't marked to retract it (nothing to
// retract), which is also what stops the flood from looping forever for a
// key that was never actually distributed anywhere.
func TestRetractKeyIgnoredByNonHoldingPeer(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const netID = uint64(0xBEEF02)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.45.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.45.0.2"))
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

	// A key B never received at all — retracting it should be a no-op on B.
	neverSharedB64, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := A.eng.RetractKey(netID, neverSharedB64); err != nil {
		t.Fatalf("RetractKey: %v", err)
	}
	// Give it a moment to (not) arrive, then confirm nothing was marked.
	time.Sleep(500 * time.Millisecond)
	if got := B.eng.RetractedKeys(netID); len(got) != 0 {
		t.Fatalf("RetractedKeys = %v, want none for a key B never held", got)
	}
}

// TestForgetAppliedRetractionsClearsOnceKeyGone proves the bookkeeping
// lifecycle: a pending retraction stays pending while the key set given to it
// still contains the key (config hasn't caught up / the persist hook refused
// it), and is forgotten once it doesn't (the persist hook's config.KeyDelete
// succeeded and a reload rebuilt the live set without it).
func TestForgetAppliedRetractionsClearsOnceKeyGone(t *testing.T) {
	raw := make([]byte, crypto.KeySize)
	raw[0] = 0x42
	id := crypto.DeriveKeyID(raw)

	ns := &netState{retractedKeys: map[crypto.KeyID]bool{id: true}}

	ns.mu.Lock()
	ns.forgetAppliedRetractions((&crypto.KeySet{}).With(raw))
	ns.mu.Unlock()
	if !ns.retractedKeys[id] {
		t.Fatal("should still be pending while the key set still contains it")
	}

	ns.mu.Lock()
	ns.forgetAppliedRetractions(&crypto.KeySet{}) // empty: key is gone
	ns.mu.Unlock()
	if ns.retractedKeys[id] {
		t.Fatal("should be forgotten once the key set no longer contains it")
	}
}

// TestForgetConfiguredKeysWaitsForLabelMatch proves the relabel-reconciliation
// lifecycle: a propagated key whose label config hasn't caught up with yet
// stays tracked (so the persist hook keeps retrying), and is only forgotten
// once the given label map agrees with what the mesh last said.
func TestForgetConfiguredKeysWaitsForLabelMatch(t *testing.T) {
	raw := make([]byte, crypto.KeySize)
	raw[0] = 0x99
	id := crypto.DeriveKeyID(raw)

	ns := &netState{propagatedKeys: map[crypto.KeyID]propagatedKey{id: {raw: raw, label: "new-label"}}}
	ks := (&crypto.KeySet{}).With(raw)
	noExpiry := map[crypto.KeyID]string{id: ""} // matches the zero-value expires above throughout

	ns.mu.Lock()
	ns.forgetConfiguredKeys(ks, map[crypto.KeyID]string{id: "old-label"}, noExpiry)
	ns.mu.Unlock()
	if _, ok := ns.propagatedKeys[id]; !ok {
		t.Fatal("entry should remain while config's label hasn't caught up")
	}

	ns.mu.Lock()
	ns.forgetConfiguredKeys(ks, map[crypto.KeyID]string{id: "new-label"}, noExpiry)
	ns.mu.Unlock()
	if _, ok := ns.propagatedKeys[id]; ok {
		t.Fatal("entry should be forgotten once config's label matches")
	}
}

// TestForgetConfiguredKeysWaitsForExpiryMatch mirrors the label test above for
// expiry: a propagated key whose expiry config hasn't caught up with yet
// stays tracked, and is only forgotten once the given expires map agrees with
// what the mesh last said — the same reconciliation lifecycle, now covering
// an expiry-only change (label matches throughout).
func TestForgetConfiguredKeysWaitsForExpiryMatch(t *testing.T) {
	raw := make([]byte, crypto.KeySize)
	raw[0] = 0x77
	id := crypto.DeriveKeyID(raw)

	newExpiryNano := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	ns := &netState{propagatedKeys: map[crypto.KeyID]propagatedKey{
		id: {raw: raw, label: "same-label", expires: newExpiryNano},
	}}
	ks := (&crypto.KeySet{}).With(raw)
	sameLabel := map[crypto.KeyID]string{id: "same-label"}

	ns.mu.Lock()
	ns.forgetConfiguredKeys(ks, sameLabel, map[crypto.KeyID]string{id: ""}) // config still says "never"
	ns.mu.Unlock()
	if _, ok := ns.propagatedKeys[id]; !ok {
		t.Fatal("entry should remain while config's expiry hasn't caught up")
	}

	ns.mu.Lock()
	ns.forgetConfiguredKeys(ks, sameLabel, map[crypto.KeyID]string{id: expiryRFC3339(newExpiryNano)})
	ns.mu.Unlock()
	if _, ok := ns.propagatedKeys[id]; ok {
		t.Fatal("entry should be forgotten once config's expiry matches")
	}
}
