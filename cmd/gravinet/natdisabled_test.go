package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestDisabledNATRuleNotInSpec verifies that a disabled NAT rule stays in config
// but is excluded from the runtime spec, while enabled rules flow through. This
// is the NAT analogue of the per-rule firewall enable/disable behaviour.
//
// NAT is node-global from v953, so the rules come from the config rather than
// from the network, and only those scoped to this network reach its overlay
// table — see natRuleInScope.
func TestDisabledNATRuleNotInSpec(t *testing.T) {
	n := config.Network{ID: "1234", Name: "lan", Enabled: true}
	nat := config.NAT{
		Enabled: true,
		Rules: []config.NATRule{
			{Source: "10.0.0.0/24", Translate: "198.51.100.7", Scope: "lan", Enabled: true},
			{Source: "10.0.1.0/24", Translate: "198.51.100.8", Scope: "lan", Enabled: false}, // disabled
			// Host-scoped: kernel only, never in an overlay table.
			{Source: "192.168.1.0/24", Translate: "masquerade", Interface: "eth0", Enabled: true},
			// Another network's rule: not this one's business.
			{Source: "10.9.0.0/24", Translate: "198.51.100.9", Scope: "other", Enabled: true},
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0, nil, config.BGPConfig{}, nat, config.QoS{}, config.Throttle{}, config.Firewall{}, nil)

	if !spec.NATEnabled {
		t.Fatal("NATEnabled should be true")
	}
	if len(spec.NAT) != 1 {
		t.Fatalf("expected only the enabled rule scoped to this network, got %d: %+v", len(spec.NAT), spec.NAT)
	}
	if spec.NAT[0].Source != "10.0.0.0/24" {
		t.Fatalf("wrong NAT rule survived: %+v", spec.NAT[0])
	}
}
