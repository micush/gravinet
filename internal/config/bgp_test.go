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

// The BGP config survives a JSON save/load round-trip intact, including
// neighbors and their BFD/password flags.
func TestBGPRoundTrip(t *testing.T) {
	in := BGPConfig{
		Enabled: true, ASN: 65001, RouterID: "10.0.0.1", BFD: true,
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
	if len(out.Neighbors) != 2 || out.Neighbors[0].Password != "s3cr3t" || !out.Neighbors[1].BFD {
		t.Errorf("neighbor round-trip mismatch: %+v", out.Neighbors)
	}
	if len(out.Networks) != 1 || out.Networks[0] != "10.0.0.0/24" {
		t.Errorf("networks round-trip mismatch: %+v", out.Networks)
	}
}
