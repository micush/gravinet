package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestDisabledFirewallKeepsRules guards against the regression where disabling a
// firewall dropped its rules from the live engine view: fillRuntimeSpec must load
// the rulebase regardless of the enabled flag (the flag only gates enforcement),
// otherwise the UI — which reads the engine's rules — shows none while disabled
// and a persist firing while off could wipe them from config.
func TestDisabledFirewallKeepsRules(t *testing.T) {
	n := config.Network{
		Firewall: config.Firewall{
			Enabled: false, // firewall OFF
			Rules: []config.FirewallRule{
				{Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 22, DstPortMax: 22},
				{Action: "allow", Direction: "in", Proto: "udp", DstPortMin: 53, DstPortMax: 53},
			},
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0)
	if spec.FirewallEnabled {
		t.Fatal("FirewallEnabled should be false")
	}
	if len(spec.FirewallRules) != 2 {
		t.Fatalf("disabled firewall must still carry its 2 rules into the spec; got %d", len(spec.FirewallRules))
	}
}
