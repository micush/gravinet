package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/crypto"
	"gravinet/internal/mesh"
)

func mustKeyB64(t *testing.T) string {
	t.Helper()
	k, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestFoldPropagatedKeysUsesSlotHintWhenFree proves a distributed key lands in
// the same slot as its originator when that slot is free — the actual feature
// this exists for (so a key generated into, say, slot 3 shows up as slot 3 on
// every peer that picks it up fresh, not scattered arbitrarily).
func TestFoldPropagatedKeysUsesSlotHintWhenFree(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	updated, unplaced, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "prod", SlotHint: 3},
	})
	if !changed || unplaced != 0 {
		t.Fatalf("changed=%v unplaced=%d, want changed=true unplaced=0", changed, unplaced)
	}
	if updated[3].Key != keyB64 || updated[3].Label != "prod" || !updated[3].Enabled || !updated[3].Distributed {
		t.Fatalf("slot 3 = %+v, want the distributed key placed there, enabled and marked Distributed", updated[3])
	}
	for i, k := range updated {
		if i != 3 && k.Key != "" {
			t.Fatalf("slot %d unexpectedly filled: %+v", i, k)
		}
	}
}

// TestFoldPropagatedKeysFallsBackWhenHintTaken: if the hinted slot is already
// occupied by something else, the key still gets placed — just in the first
// free slot instead, rather than being silently dropped.
func TestFoldPropagatedKeysFallsBackWhenHintTaken(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	keys[3] = config.KeySlot{Key: mustKeyB64(t), Label: "unrelated", Enabled: true}
	updated, unplaced, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "prod", SlotHint: 3},
	})
	if !changed || unplaced != 0 {
		t.Fatalf("changed=%v unplaced=%d, want changed=true unplaced=0", changed, unplaced)
	}
	if updated[0].Key != keyB64 {
		t.Fatalf("expected the key in the first free slot (0), got %+v", updated[0])
	}
	if updated[3].Label != "unrelated" {
		t.Fatalf("the slot already occupying the hint must be untouched, got %+v", updated[3])
	}
}

// TestFoldPropagatedKeysNoHintUsesFirstFree: SlotHint -1 (the plain rotation
// path, which has no natural originating slot) just uses the first free slot,
// matching the pre-slot-hint behavior.
func TestFoldPropagatedKeysNoHintUsesFirstFree(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	keys[0] = config.KeySlot{Key: mustKeyB64(t), Enabled: true}
	updated, _, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "rotated", SlotHint: -1},
	})
	if !changed {
		t.Fatal("expected a change")
	}
	if updated[1].Key != keyB64 {
		t.Fatalf("expected the key in slot 1 (first free), got %+v", updated[1])
	}
}

// TestFoldPropagatedKeysReconcilesLabelWithoutTouchingEnabled proves a relabel
// of an already-present distributed key updates only the label — Enabled (and
// everything else) is left exactly as the operator set it, so retiring a key
// by disabling its slot can never be silently undone by a later relabel.
func TestFoldPropagatedKeysReconcilesLabelWithoutTouchingEnabled(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	keys[2] = config.KeySlot{Key: keyB64, Label: "old-name", Enabled: false, Distributed: true}
	updated, unplaced, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "new-name", SlotHint: 2},
	})
	if !changed || unplaced != 0 {
		t.Fatalf("changed=%v unplaced=%d, want changed=true unplaced=0", changed, unplaced)
	}
	if updated[2].Label != "new-name" {
		t.Fatalf("label = %q, want %q", updated[2].Label, "new-name")
	}
	if updated[2].Enabled {
		t.Fatal("Enabled must not be flipped back on by a relabel of a disabled/retired slot")
	}
}

// TestFoldPropagatedKeysReconcilesExpiryWithoutTouchingEnabled mirrors the
// label test above for Expires: reproduces the reported gap where a key's
// expiry, set on the origin node after the key was already distributed and
// present elsewhere, must reconcile into an already-present slot the same
// way a relabel does — while Enabled is still left untouched.
func TestFoldPropagatedKeysReconcilesExpiryWithoutTouchingEnabled(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	keys[7] = config.KeySlot{Key: keyB64, Label: "testkey7", Enabled: true, Expires: "", Distributed: true}
	updated, unplaced, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "testkey7", Expires: "2026-07-06T23:59:00Z", SlotHint: 7},
	})
	if !changed || unplaced != 0 {
		t.Fatalf("changed=%v unplaced=%d, want changed=true unplaced=0", changed, unplaced)
	}
	if updated[7].Expires != "2026-07-06T23:59:00Z" {
		t.Fatalf("expires = %q, want %q", updated[7].Expires, "2026-07-06T23:59:00Z")
	}
	if !updated[7].Enabled {
		t.Fatal("Enabled must not be touched by an expiry reconciliation")
	}

	// And the reverse — clearing an expiry back to "never" — must also travel.
	keys2 := updated
	updated2, _, changed2 := foldPropagatedKeys(keys2, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "testkey7", Expires: "", SlotHint: 7},
	})
	if !changed2 {
		t.Fatal("clearing an expiry back to never should also count as a change")
	}
	if updated2[7].Expires != "" {
		t.Fatalf("expires = %q, want cleared", updated2[7].Expires)
	}
}

// TestFoldPropagatedKeysSameLabelIsNoop: nothing to reconcile → changed=false,
// so the caller doesn't do a pointless config write every persist cycle.
func TestFoldPropagatedKeysSameLabelIsNoop(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	keys[0] = config.KeySlot{Key: keyB64, Label: "same", Enabled: true, Distributed: true}
	_, unplaced, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "same", SlotHint: 0},
	})
	if changed || unplaced != 0 {
		t.Fatalf("changed=%v unplaced=%d, want both false/0 when nothing actually differs", changed, unplaced)
	}
}

// TestFoldPropagatedKeysNoFreeSlotReportsUnplaced: all 8 slots full → the key
// can't be persisted, and the caller is told so (to log it), not silently
// dropped.
func TestFoldPropagatedKeysNoFreeSlotReportsUnplaced(t *testing.T) {
	keyB64 := mustKeyB64(t)
	var keys [8]config.KeySlot
	for i := range keys {
		keys[i] = config.KeySlot{Key: mustKeyB64(t), Enabled: true}
	}
	updated, unplaced, changed := foldPropagatedKeys(keys, []mesh.PropagatedKeyInfo{
		{KeyB64: keyB64, Label: "no-room", SlotHint: -1},
	})
	if changed {
		t.Fatal("expected no change when there's no room")
	}
	if unplaced != 1 {
		t.Fatalf("unplaced = %d, want 1", unplaced)
	}
	if updated != keys {
		t.Fatal("keys must be untouched when a distributed key can't be placed")
	}
}

// TestApplyKeyRetractionsRemovesMatchingSlot: the ordinary case — the
// retracted key is present and isn't the only enabled key, so it's cleared.
func TestApplyKeyRetractionsRemovesMatchingSlot(t *testing.T) {
	keyB64 := mustKeyB64(t)
	raw, _ := crypto.DecodeKey(keyB64)
	id := crypto.DeriveKeyID(raw)
	var keys [8]config.KeySlot
	keys[0] = config.KeySlot{Key: mustKeyB64(t), Enabled: true} // some other enabled key
	keys[4] = config.KeySlot{Key: keyB64, Label: "retract-me", Enabled: true, Distributed: true}

	updated, refused, changed := applyKeyRetractions(keys, []crypto.KeyID{id})
	if !changed || len(refused) != 0 {
		t.Fatalf("changed=%v refused=%v, want changed=true, nothing refused", changed, refused)
	}
	if updated[4].Key != "" {
		t.Fatalf("slot 4 should be cleared, got %+v", updated[4])
	}
	if updated[0].Key == "" {
		t.Fatal("the unrelated slot must be untouched")
	}
}

// TestApplyKeyRetractionsRefusesLastEnabledKey proves the safety rail: a
// retraction can never brick this node's own ability to authenticate by
// removing its only enabled key — it's refused (and reported) instead.
func TestApplyKeyRetractionsRefusesLastEnabledKey(t *testing.T) {
	keyB64 := mustKeyB64(t)
	raw, _ := crypto.DecodeKey(keyB64)
	id := crypto.DeriveKeyID(raw)
	var keys [8]config.KeySlot
	keys[0] = config.KeySlot{Key: keyB64, Label: "only-key", Enabled: true, Distributed: true}

	updated, refused, changed := applyKeyRetractions(keys, []crypto.KeyID{id})
	if changed {
		t.Fatal("must not change anything when the retraction is refused")
	}
	if len(refused) != 1 || refused[0] != id {
		t.Fatalf("refused = %v, want exactly [%x]", refused, id)
	}
	if updated[0].Key != keyB64 {
		t.Fatal("the only enabled key must survive a refused retraction")
	}
}

// TestApplyKeyRetractionsIgnoresUnknownKey: retracting a key this node never
// had is simply a no-op, not an error.
func TestApplyKeyRetractionsIgnoresUnknownKey(t *testing.T) {
	var someID crypto.KeyID
	someID[0] = 0xAB
	var keys [8]config.KeySlot
	keys[0] = config.KeySlot{Key: mustKeyB64(t), Enabled: true}

	updated, refused, changed := applyKeyRetractions(keys, []crypto.KeyID{someID})
	if changed || len(refused) != 0 {
		t.Fatalf("changed=%v refused=%v, want no-op for an unknown key", changed, refused)
	}
	if updated != keys {
		t.Fatal("keys must be untouched")
	}
}

// TestApplyKeyRetractionsAllowsRemovalWhenAnotherKeyEnabled: retracting one of
// two enabled keys is fine — only the *last* enabled key is protected.
func TestApplyKeyRetractionsAllowsRemovalWhenAnotherKeyEnabled(t *testing.T) {
	keyB64 := mustKeyB64(t)
	raw, _ := crypto.DecodeKey(keyB64)
	id := crypto.DeriveKeyID(raw)
	var keys [8]config.KeySlot
	keys[0] = config.KeySlot{Key: keyB64, Enabled: true, Distributed: true}
	keys[1] = config.KeySlot{Key: mustKeyB64(t), Enabled: true}

	updated, refused, changed := applyKeyRetractions(keys, []crypto.KeyID{id})
	if !changed || len(refused) != 0 {
		t.Fatalf("changed=%v refused=%v, want the retraction to succeed", changed, refused)
	}
	if updated[0].Key != "" {
		t.Fatal("the retracted slot should be cleared")
	}
	if updated[1].Key == "" {
		t.Fatal("the other enabled key must be untouched")
	}
}
