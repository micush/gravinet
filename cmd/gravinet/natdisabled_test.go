package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestDisabledNATRuleNotInSpec verifies that a disabled NAT rule stays in config
// but is excluded from the runtime spec, while enabled rules flow through. This
// is the NAT analogue of the per-rule firewall enable/disable behaviour.
func TestDisabledNATRuleNotInSpec(t *testing.T) {
	n := config.Network{
		ID:      "1234",
		Name:    "lan",
		Enabled: true,
		NAT: config.NAT{
			Enabled: true,
			Rules: []config.NATRule{
				{Source: "10.0.0.0/24", Translate: "198.51.100.7", Enabled: true},
				{Source: "10.0.1.0/24", Translate: "198.51.100.8", Enabled: false}, // disabled
			},
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0, nil)

	if !spec.NATEnabled {
		t.Fatal("NATEnabled should be true")
	}
	if len(spec.NAT) != 1 {
		t.Fatalf("expected only the enabled NAT rule, got %d: %+v", len(spec.NAT), spec.NAT)
	}
	if spec.NAT[0].Source != "10.0.0.0/24" {
		t.Fatalf("wrong NAT rule survived: %+v", spec.NAT[0])
	}
}
