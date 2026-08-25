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
//
// The rulebase is node-global from v957, so it arrives as its own argument
// rather than hanging off the network.
func TestDisabledFirewallKeepsRules(t *testing.T) {
	n := config.Network{ID: "1111", Name: "lan"}
	fw := config.Firewall{Enabled: false} // firewall OFF
	rules := []config.FirewallRule{
		{ID: 1, Action: "deny", Direction: "in", Proto: "tcp", DstPortMin: 22, DstPortMax: 22},
		{ID: 2, Action: "allow", Direction: "in", Proto: "udp", DstPortMin: 53, DstPortMax: 53},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0, nil, config.BGPConfig{}, config.NAT{}, config.QoS{}, config.Throttle{}, fw, rules)
	if spec.FirewallEnabled {
		t.Fatal("FirewallEnabled should be false")
	}
	if len(spec.FirewallRules) != 2 {
		t.Fatalf("disabled firewall must still carry its 2 rules into the spec; got %d", len(spec.FirewallRules))
	}
	// The config-assigned id has to reach the engine, or it mints its own and
	// the ids the UI holds stop matching after a reload.
	for i, want := range []uint64{1, 2} {
		if spec.FirewallRules[i].ID != want {
			t.Errorf("rule %d: id %d did not survive into the spec, want %d", i, spec.FirewallRules[i].ID, want)
		}
	}
}
