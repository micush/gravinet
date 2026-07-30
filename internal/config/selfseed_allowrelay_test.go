package config

import (
	"encoding/json"
	"testing"
)

// TestNetworkAllowRelayBackfillOnMissingKey checks that a network JSON with
// no "allow_relay" key at all (any config predating this field) loads with
// AllowRelay defaulted on — not silently disabled at encoding/json's zero
// value, which is indistinguishable from a deliberate choice and, once
// re-saved, becomes one. Same reasoning, same mechanism, as DNSSync's
// backfill (TestNetworkDNSSyncBackfillOnMissingKey) — added alongside it in
// Network.UnmarshalJSON rather than as a separate check.
func TestNetworkAllowRelayBackfillOnMissingKey(t *testing.T) {
	raw := `{"id":"1","name":"n"}`
	var n Network
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := NewNetworkDefaults().AllowRelay
	if !want {
		t.Fatal("test assumption broken: NewNetworkDefaults().AllowRelay is no longer true")
	}
	if n.AllowRelay != want {
		t.Fatalf("AllowRelay = %v, want default %v", n.AllowRelay, want)
	}
}

// TestNetworkAllowRelayExplicitValueRespected checks the backfill never
// overrides an "allow_relay" key that's actually present — including an
// explicit false, which is a valid, deliberate configuration and must be
// left alone, not "corrected" back to the default.
func TestNetworkAllowRelayExplicitValueRespected(t *testing.T) {
	explicitFalse := `{"id":"1","name":"n","allow_relay":false}`
	var n Network
	if err := json.Unmarshal([]byte(explicitFalse), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.AllowRelay {
		t.Fatal("explicit \"allow_relay\":false must be respected verbatim, got true")
	}

	explicitTrue := `{"id":"1","name":"n","allow_relay":true}`
	var n2 Network
	if err := json.Unmarshal([]byte(explicitTrue), &n2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !n2.AllowRelay {
		t.Fatal("explicit \"allow_relay\":true must be respected verbatim, got false")
	}
}

// SelfSeed has no backfill (unlike AllowRelay/DNSSync, it doesn't predate
// having a default — it's new, and false is the correct default for
// something the operator hasn't declared), so a missing key must simply
// decode to false, not panic or pick up some other value.
func TestNetworkSelfSeedMissingKeyDefaultsFalse(t *testing.T) {
	raw := `{"id":"1","name":"n"}`
	var n Network
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if n.SelfSeed {
		t.Fatal("SelfSeed with no \"self_seed\" key at all: got true, want false")
	}
}

func TestNetworkSetSelfSeed(t *testing.T) {
	c := &Config{Networks: []Network{{ID: "1", Name: "n"}}}
	if err := c.NetworkSetSelfSeed("n", true); err != nil {
		t.Fatalf("NetworkSetSelfSeed(true): %v", err)
	}
	if !c.Networks[0].SelfSeed {
		t.Fatal("SelfSeed not set to true")
	}
	if err := c.NetworkSetSelfSeed("n", false); err != nil {
		t.Fatalf("NetworkSetSelfSeed(false): %v", err)
	}
	if c.Networks[0].SelfSeed {
		t.Fatal("SelfSeed not set back to false")
	}
	if err := c.NetworkSetSelfSeed("nonexistent", true); err == nil {
		t.Fatal("NetworkSetSelfSeed on a nonexistent network: want an error, got nil")
	}
}

func TestNetworkSetAllowRelay(t *testing.T) {
	c := &Config{Networks: []Network{{ID: "1", Name: "n", AllowRelay: true}}}
	if err := c.NetworkSetAllowRelay("n", false); err != nil {
		t.Fatalf("NetworkSetAllowRelay(false): %v", err)
	}
	if c.Networks[0].AllowRelay {
		t.Fatal("AllowRelay not set to false")
	}
	if err := c.NetworkSetAllowRelay("n", true); err != nil {
		t.Fatalf("NetworkSetAllowRelay(true): %v", err)
	}
	if !c.Networks[0].AllowRelay {
		t.Fatal("AllowRelay not set back to true")
	}
	if err := c.NetworkSetAllowRelay("nonexistent", true); err == nil {
		t.Fatal("NetworkSetAllowRelay on a nonexistent network: want an error, got nil")
	}
}
