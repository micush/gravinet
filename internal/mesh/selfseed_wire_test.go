package mesh

import "testing"

// TestHSPayloadRoundTripsSelfSeed: the advertisement survives encode/decode
// in both states. Unlike AllowRelay, there's no separate "known" bit needed
// — SelfSeed is a plain declaration, not an opinion that needs distinguishing
// from "too old to have one"; an old peer simply decodes false, which is
// exactly the right default (ManagedPeers falls back to its address-based
// checks for such a peer, same as it always has).
func TestHSPayloadRoundTripsSelfSeed(t *testing.T) {
	for _, self := range []bool{true, false} {
		in := hsPayload{Ephemeral: make([]byte, ephemeralLen), NodeID: "n", SelfSeed: self}
		out, err := decodeHSPayload(encodeHSPayload(in))
		if err != nil {
			t.Fatalf("decode (self=%v): %v", self, err)
		}
		if out.SelfSeed != self {
			t.Errorf("self=%v: round-tripped SelfSeed=%v", self, out.SelfSeed)
		}
	}
}

// SelfSeed shares the mflag byte with Managed and Manager (bits 0/1/2) rather
// than getting its own trailing field — this proves all three bits are
// independent and none clobbers another, in every one of the 8 combinations.
func TestHSPayloadSelfSeedIndependentOfManagedManager(t *testing.T) {
	for _, managed := range []bool{true, false} {
		for _, manager := range []bool{true, false} {
			for _, self := range []bool{true, false} {
				in := hsPayload{
					Ephemeral: make([]byte, ephemeralLen), NodeID: "n",
					Managed: managed, Manager: manager, SelfSeed: self,
				}
				out, err := decodeHSPayload(encodeHSPayload(in))
				if err != nil {
					t.Fatalf("decode (managed=%v manager=%v self=%v): %v", managed, manager, self, err)
				}
				if out.Managed != managed || out.Manager != manager || out.SelfSeed != self {
					t.Errorf("in={managed=%v manager=%v self=%v} out={managed=%v manager=%v self=%v}",
						managed, manager, self, out.Managed, out.Manager, out.SelfSeed)
				}
			}
		}
	}
}
