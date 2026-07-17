package config

import (
	"encoding/json"
	"testing"
)

func baseValid() *Config {
	return &Config{PrimaryPort: 65432, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
}

// Enabling BGP without a local AS is a clear config error (matches the FRR
// renderer's own enabled+asn gate); disabled BGP with no AS is fine.
func TestBGPValidateRequiresASN(t *testing.T) {
	c := baseValid()
	c.BGP = BGPConfig{Enabled: true, ASN: 0}
	if err := c.Validate(); err == nil {
		t.Fatal("enabling BGP with asn=0 should fail validation")
	}

	c.BGP = BGPConfig{Enabled: true, ASN: 65001}
	if err := c.Validate(); err != nil {
		t.Fatalf("enabled BGP with a valid AS should pass: %v", err)
	}

	c.BGP = BGPConfig{Enabled: false, ASN: 0}
	if err := c.Validate(); err != nil {
		t.Fatalf("disabled BGP with no AS should pass: %v", err)
	}
}

// Session timers: unset fields resolve to gravinet's 4s/12s default, an
// explicit pair is kept, and combinations FRR would reject (out of range, a
// sub-3s hold, or a keepalive exceeding the hold) fail validation — including
// the case where a large keepalive is left against the *default* hold.
func TestBGPValidateTimers(t *testing.T) {
	// Defaults when unset.
	def := BGPConfig{}
	if def.EffectiveKeepAlive() != DefaultBGPKeepAlive || def.EffectiveHoldTime() != DefaultBGPHoldTime {
		t.Errorf("unset timers should resolve to %d/%d, got %d/%d",
			DefaultBGPKeepAlive, DefaultBGPHoldTime, def.EffectiveKeepAlive(), def.EffectiveHoldTime())
	}
	// Explicit values are preserved.
	set := BGPConfig{KeepAlive: 10, HoldTime: 30}
	if set.EffectiveKeepAlive() != 10 || set.EffectiveHoldTime() != 30 {
		t.Errorf("explicit timers not preserved: %d/%d", set.EffectiveKeepAlive(), set.EffectiveHoldTime())
	}

	ok := func(b BGPConfig) {
		t.Helper()
		c := baseValid()
		c.BGP = b
		if err := c.Validate(); err != nil {
			t.Errorf("expected valid, got %v for %+v", err, b)
		}
	}
	bad := func(b BGPConfig) {
		t.Helper()
		c := baseValid()
		c.BGP = b
		if err := c.Validate(); err == nil {
			t.Errorf("expected invalid, got nil for %+v", b)
		}
	}

	ok(BGPConfig{Enabled: true, ASN: 65001})                              // defaults (4/12)
	ok(BGPConfig{Enabled: true, ASN: 65001, KeepAlive: 3, HoldTime: 9})   // classic 3/9
	ok(BGPConfig{Enabled: true, ASN: 65001, KeepAlive: 60, HoldTime: 180}) // FRR traditional
	bad(BGPConfig{Enabled: true, ASN: 65001, HoldTime: 2})               // hold < 3
	bad(BGPConfig{Enabled: true, ASN: 65001, KeepAlive: 20})             // 20 vs default 12 hold
	bad(BGPConfig{Enabled: true, ASN: 65001, KeepAlive: 30, HoldTime: 20}) // keepalive > hold
	bad(BGPConfig{Enabled: true, ASN: 65001, HoldTime: 70000})           // out of range
}

// The BGP config survives a JSON save/load round-trip intact, including
// neighbors and their BFD/password flags.
func TestBGPRoundTrip(t *testing.T) {
	in := BGPConfig{
		Enabled: true, ASN: 65001, RouterID: "10.0.0.1", KeepAlive: 5, HoldTime: 15, BFD: true,
		Neighbors: []BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002, Description: "core", Password: "s3cr3t", BFD: false},
			{Peer: "fd00::2", RemoteAS: 65010, BFD: true},
		},
		Networks:              []string{"10.0.0.0/24"},
		RedistributeConnected: true,
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out BGPConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ASN != 65001 || out.RouterID != "10.0.0.1" || !out.BFD || !out.RedistributeConnected {
		t.Errorf("scalar round-trip mismatch: %+v", out)
	}
	if out.KeepAlive != 5 || out.HoldTime != 15 {
		t.Errorf("timer round-trip mismatch: keepalive=%d hold=%d", out.KeepAlive, out.HoldTime)
	}
	if len(out.Neighbors) != 2 || out.Neighbors[0].Password != "s3cr3t" || !out.Neighbors[1].BFD {
		t.Errorf("neighbor round-trip mismatch: %+v", out.Neighbors)
	}
	if len(out.Networks) != 1 || out.Networks[0] != "10.0.0.0/24" {
		t.Errorf("networks round-trip mismatch: %+v", out.Networks)
	}
}
