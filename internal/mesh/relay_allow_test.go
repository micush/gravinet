package mesh

import (
	"testing"
)

// TestHSPayloadRoundTripsAllowRelay: the advertisement survives encode/decode
// in both states, and RelayKnown is set either way — an upgraded node always
// has an opinion, even when the opinion is "no".
func TestHSPayloadRoundTripsAllowRelay(t *testing.T) {
	for _, allow := range []bool{true, false} {
		in := hsPayload{Ephemeral: make([]byte, ephemeralLen), NodeID: "n", AllowRelay: allow}
		out, err := decodeHSPayload(encodeHSPayload(in))
		if err != nil {
			t.Fatalf("decode (allow=%v): %v", allow, err)
		}
		if !out.RelayKnown {
			t.Errorf("allow=%v: RelayKnown should be set by any node new enough to encode the field", allow)
		}
		if out.AllowRelay != allow {
			t.Errorf("allow=%v: round-tripped AllowRelay=%v", allow, out.AllowRelay)
		}
	}
}

// TestWillRelayTreatsOldPeerAsWilling is the rolling-upgrade guard. A peer too
// old to advertise the field sets neither bit, so relayKnown is false. That
// must be read as "unknown, assume willing" — exactly the pre-change
// behavior — and NOT as a refusal, which would make every upgraded node stop
// relaying through every not-yet-upgraded one for the whole duration of a
// mixed-version mesh.
func TestWillRelayTreatsOldPeerAsWilling(t *testing.T) {
	old := &peerSession{nodeID: "old"} // relayKnown false, allowRelay false
	if !old.willRelay() {
		t.Fatal("a peer that predates the advertisement must be assumed willing to relay, not treated as refusing")
	}
	yes := &peerSession{nodeID: "yes", relayKnown: true, allowRelay: true}
	if !yes.willRelay() {
		t.Fatal("a peer advertising allow_relay=true should be usable as a relay")
	}
	no := &peerSession{nodeID: "no", relayKnown: true, allowRelay: false}
	if no.willRelay() {
		t.Fatal("a peer explicitly advertising allow_relay=false must not be used as a relay")
	}
}

// TestBestRelaySkipsRefusersAndCountsThem is the core of the fix: bestRelay
// used to happily pick a peer with allow_relay disabled, whose onRelay
// silently drops everything, and then keep re-picking it forever — the target
// never came up and nothing was logged anywhere. It must now skip refusers,
// prefer a willing peer even when a refuser scores better, and report how
// many it skipped so tryRelays can tell "nobody knows this target" apart from
// "everyone who knows it refuses" (see logRelayRefused).
func TestBestRelaySkipsRefusersAndCountsThem(t *testing.T) {
	target := "macmini"

	refuser := &peerSession{nodeID: "ionos1", relayKnown: true, allowRelay: false}
	refuser.markReported([]string{target})
	refuser.rttNanos.Store(1) // would win on RTT if it were eligible at all

	willing := &peerSession{nodeID: "ionos2", relayKnown: true, allowRelay: true}
	willing.markReported([]string{target})
	willing.rttNanos.Store(9999) // much worse RTT, but it will actually carry the traffic

	best, refused := bestRelay([]*peerSession{refuser, willing}, target)
	if best != willing {
		t.Fatalf("bestRelay should pick the willing peer over a better-scoring refuser; got %v", best)
	}
	if refused != 1 {
		t.Fatalf("refused count = %d, want 1", refused)
	}

	// Every candidate refuses: no relay, and the count is what makes the
	// difference visible to the operator instead of failing silently.
	best, refused = bestRelay([]*peerSession{refuser}, target)
	if best != nil {
		t.Fatalf("bestRelay should return nil when every candidate refuses; got %v", best)
	}
	if refused != 1 {
		t.Fatalf("refused count = %d, want 1", refused)
	}

	// A peer that simply doesn't know the target isn't a "refuser" — it's not
	// a candidate at all, and must not be counted as one (that would produce a
	// misleading "they all have allow_relay disabled" warning).
	stranger := &peerSession{nodeID: "gn-win11", relayKnown: true, allowRelay: false}
	best, refused = bestRelay([]*peerSession{stranger}, target)
	if best != nil || refused != 0 {
		t.Fatalf("a peer that doesn't report the target is not a refused candidate; best=%v refused=%d", best, refused)
	}
}
