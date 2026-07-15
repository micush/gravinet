package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestSeedNotesForEndpoint covers the same scenarios the web UI's JS
// seedNotesForAddr was checked against (see the v348 changelog entry), plus
// the two failure modes reported directly from a live peers table: a peer
// reached over an address that shares no host with any seed at all (a
// LAN-discovered direct path), and a peer sitting on the exact port
// configured for a *different* seed on the same host.
func TestSeedNotesForEndpoint(t *testing.T) {
	seeds := []config.Seed{
		{Address: "98.191.177.78:443", Notes: "cox gn-cush1 nat phoenix"},
		{Address: "98.191.177.78:65432", Notes: "cox gn-cush2 nat phoenix"},
	}

	if got := seedNotesForEndpoint(seeds, "98.191.177.78:443"); got != "cox gn-cush1 nat phoenix" {
		t.Errorf("cush1 exact port: got %q", got)
	}
	if got := seedNotesForEndpoint(seeds, "98.191.177.78:65432"); got != "cox gn-cush2 nat phoenix" {
		t.Errorf("cush2 exact port: got %q", got)
	}
	// A third, unseeded port on the shared host is genuinely ambiguous
	// between the two boxes — must not guess.
	if got := seedNotesForEndpoint(seeds, "98.191.177.78:51234"); got != "" {
		t.Errorf("ambiguous same-host port: got %q, want empty", got)
	}
	// A completely different host (e.g. cush1 reached over a LAN-local path
	// instead of its public seed) shares nothing with either seed at all.
	if got := seedNotesForEndpoint(seeds, "192.168.55.3:65432"); got != "" {
		t.Errorf("unrelated host: got %q, want empty", got)
	}

	// A genuinely unambiguous host-only case (one box, two seed rows for
	// it, same note) must still resolve even when the live port matches
	// neither seed exactly.
	sameBox := []config.Seed{
		{Address: "203.0.113.5", Notes: "office uplink"},
		{Address: "203.0.113.5:443", Notes: "office uplink"},
	}
	if got := seedNotesForEndpoint(sameBox, "203.0.113.5:9999"); got != "office uplink" {
		t.Errorf("unambiguous host-only match: got %q, want %q", got, "office uplink")
	}

	// No seeds, no endpoint, or an endpoint that isn't host:port shaped at
	// all: no match, no panic.
	if got := seedNotesForEndpoint(nil, "1.2.3.4:5"); got != "" {
		t.Errorf("nil seeds: got %q", got)
	}
	if got := seedNotesForEndpoint(seeds, ""); got != "" {
		t.Errorf("empty endpoint: got %q", got)
	}
}

// TestApplySeedNoteInheritance drives the decision logic directly against a
// constructed config and mesh.PeerInfo values — the same peers table shape
// reported live (gn-cush1 on a LAN-local path unrelated to any seed,
// gn-cush2 sitting on gn-cush1's exact seeded port).
func TestApplySeedNoteInheritance(t *testing.T) {
	cfg := &config.Config{Networks: []config.Network{{
		ID:   "0000000000000001",
		Name: "cush1",
		Seeds: []config.Seed{
			{Address: "98.191.177.78:443", Notes: "cox gn-cush1 nat phoenix"},
			{Address: "98.191.177.78:65432", Notes: "cox gn-cush2 nat phoenix"},
		},
	}}}
	peers := map[uint64][]mesh.PeerInfo{
		1: {
			{NodeID: "cush1id", Endpoint: "192.168.55.3:65432"},    // LAN-local, no seed match
			{NodeID: "cush2id", Endpoint: "98.191.177.78:443"},     // sitting on cush1's own port
			{NodeID: "freebsdid", Endpoint: "192.168.5.104:65432"}, // no seed at all for this host
		},
	}

	changed := applySeedNoteInheritance(cfg, peers)
	if !changed {
		t.Fatal("expected a change: cush2id sits on cush1's exact seeded port, an unambiguous match")
	}
	// cush1's own host doesn't match any seed at all (LAN-local path);
	// cush2's exact-port match is unambiguous by construction and should be
	// inherited; freebsd has no seed on this network at all. Exactly one
	// note should have been written.
	got := cfg.Networks[0].PeerNotes
	if got["cush2id"] != "cox gn-cush1 nat phoenix" {
		t.Errorf("cush2id notes = %q, want %q (it's on cush1's seeded port)", got["cush2id"], "cox gn-cush1 nat phoenix")
	}
	if _, ok := got["cush1id"]; ok {
		t.Errorf("cush1id should not have inherited a note (LAN-local path matches no seed): got %q", got["cush1id"])
	}
	if _, ok := got["freebsdid"]; ok {
		t.Errorf("freebsdid should not have inherited a note (no matching seed at all): got %q", got["freebsdid"])
	}

	// Never overwrites an existing note.
	cfg2 := &config.Config{Networks: []config.Network{{
		ID:        "0000000000000001",
		Name:      "cush1",
		Seeds:     []config.Seed{{Address: "203.0.113.5:443", Notes: "seed note"}},
		PeerNotes: map[string]string{"already-noted": "operator's own note"},
	}}}
	peers2 := map[uint64][]mesh.PeerInfo{1: {{NodeID: "already-noted", Endpoint: "203.0.113.5:443"}}}
	if applySeedNoteInheritance(cfg2, peers2) {
		t.Error("should not report a change when the only candidate already has its own note")
	}
	if got := cfg2.Networks[0].PeerNotes["already-noted"]; got != "operator's own note" {
		t.Errorf("existing note was overwritten: got %q", got)
	}

	// A network with no seeds at all is a fast no-op, not a panic on a nil
	// Seeds slice.
	cfg3 := &config.Config{Networks: []config.Network{{ID: "0000000000000001", Name: "bare"}}}
	if applySeedNoteInheritance(cfg3, map[uint64][]mesh.PeerInfo{1: {{NodeID: "x", Endpoint: "1.2.3.4:5"}}}) {
		t.Error("network with no seeds should never report a change")
	}

	// A peer with no node id (shouldn't happen in practice, but nothing
	// here should assume it can't) is skipped rather than keyed on "".
	cfg4 := &config.Config{Networks: []config.Network{{
		ID: "0000000000000001", Name: "cush1",
		Seeds: []config.Seed{{Address: "203.0.113.5:443", Notes: "seed note"}},
	}}}
	if applySeedNoteInheritance(cfg4, map[uint64][]mesh.PeerInfo{1: {{NodeID: "", Endpoint: "203.0.113.5:443"}}}) {
		t.Error("a peer with no node id must not be keyed on an empty string")
	}
}
