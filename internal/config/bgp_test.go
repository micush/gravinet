package config

import (
	"encoding/json"
	"testing"
)

func baseValid() *Config {
	return &Config{UDPPorts: []int{65432}, EnableIPv4: true,
		Networks: []Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
}

// Enabling BGP without a local AS is a clear config error (matches the FRR
// renderer's own enabled+asn gate); disabled BGP with no AS is fine.
// BGP timers: hold must exceed keepalive and clear FRR's 3s floor; 0/0 is fine.
func TestBGPValidateTimers(t *testing.T) {
	c := baseValid()

	c.BGP = BGPConfig{Enabled: true, ASN: 65001, KeepaliveTime: 4, HoldTime: 12}
	if err := c.Validate(); err != nil {
		t.Errorf("4/12 should be valid: %v", err)
	}

	c.BGP = BGPConfig{Enabled: true, ASN: 65001, KeepaliveTime: 12, HoldTime: 12}
	if err := c.Validate(); err == nil {
		t.Error("hold == keepalive should fail")
	}

	c.BGP = BGPConfig{Enabled: true, ASN: 65001, HoldTime: 2}
	if err := c.Validate(); err == nil {
		t.Error("hold below 3s floor should fail")
	}

	c.BGP = BGPConfig{Enabled: true, ASN: 65001} // 0/0 → FRR defaults
	if err := c.Validate(); err != nil {
		t.Errorf("unset timers should be valid: %v", err)
	}
}

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
		Enabled: true, ASN: 65001, RouterID: "10.0.0.1",
		Neighbors: []BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002, Description: "core", Password: "s3cr3t", BFD: false,
				FilterIn: []string{"10.9.0.0/24"}, FilterOut: []string{"10.8.0.0/24", "fd00:8::/64"}},
			{Peer: "fd00::2", RemoteAS: 65010, BFD: true},
		},
		Networks:                    []string{"10.0.0.0/24"},
		RedistributeConnectedRoutes: []string{"10.0.0.0/24"},
		RedistributeMeshRoutes:      []string{"10.1.0.0/24"},
	}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out BGPConfig
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.ASN != 65001 || out.RouterID != "10.0.0.1" || len(out.RedistributeConnectedRoutes) != 1 || out.RedistributeConnectedRoutes[0] != "10.0.0.0/24" || len(out.RedistributeMeshRoutes) != 1 || out.RedistributeMeshRoutes[0] != "10.1.0.0/24" {
		t.Errorf("scalar round-trip mismatch: %+v", out)
	}
	if len(out.Neighbors) != 2 || out.Neighbors[0].Password != "s3cr3t" || !out.Neighbors[1].BFD {
		t.Errorf("neighbor round-trip mismatch: %+v", out.Neighbors)
	}
	if got := out.Neighbors[0].FilterIn; len(got) != 1 || got[0] != "10.9.0.0/24" {
		t.Errorf("neighbor filter_in round-trip mismatch: %+v", got)
	}
	if got := out.Neighbors[0].FilterOut; len(got) != 2 || got[0] != "10.8.0.0/24" || got[1] != "fd00:8::/64" {
		t.Errorf("neighbor filter_out round-trip mismatch: %+v", got)
	}
	if len(out.Neighbors[1].FilterIn) != 0 || len(out.Neighbors[1].FilterOut) != 0 {
		t.Errorf("neighbor with no filters set should round-trip empty, got: %+v", out.Neighbors[1])
	}
	// omitempty: a neighbor with no filters shouldn't put filter_in/filter_out
	// keys in the marshaled JSON at all.
	var rawNeighbors []map[string]any
	var rawTop map[string]any
	if err := json.Unmarshal(raw, &rawTop); err != nil {
		t.Fatal(err)
	}
	nb, _ := json.Marshal(rawTop["neighbors"])
	if err := json.Unmarshal(nb, &rawNeighbors); err != nil {
		t.Fatal(err)
	}
	if len(rawNeighbors) != 2 {
		t.Fatalf("expected 2 raw neighbor objects, got %d", len(rawNeighbors))
	}
	if _, ok := rawNeighbors[0]["filter_in"]; !ok {
		t.Errorf("expected filter_in key present for the first neighbor: %v", rawNeighbors[0])
	}
	if _, ok := rawNeighbors[1]["filter_in"]; ok {
		t.Errorf("neighbor with no filters set should omit filter_in from JSON: %v", rawNeighbors[1])
	}
	if _, ok := rawNeighbors[1]["filter_out"]; ok {
		t.Errorf("neighbor with no filters set should omit filter_out from JSON: %v", rawNeighbors[1])
	}
	if len(out.Networks) != 1 || out.Networks[0] != "10.0.0.0/24" {
		t.Errorf("networks round-trip mismatch: %+v", out.Networks)
	}
}
