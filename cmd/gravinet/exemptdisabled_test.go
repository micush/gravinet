package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestDisabledExemptNotInSpec verifies that a disabled firewall allow-list entry
// stays in config but is excluded from the runtime spec, while enabled entries
// flow through. This is the allow-list analogue of the per-rule firewall
// enable/disable behaviour.
func TestDisabledExemptNotInSpec(t *testing.T) {
	exempts := []config.FirewallExempt{
		{Name: "BGP", Proto: "tcp", Port: 179},                  // enabled (zero value)
		{Name: "OSPF", Proto: "ospf", Disabled: true},           // disabled
		{Name: "RIP", Proto: "udp", Port: 520, Disabled: false}, // enabled explicitly
	}
	n := config.Network{ID: "1234", Name: "lan", Enabled: true}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, exempts, 0)

	got := map[string]bool{}
	for _, e := range spec.FirewallExempts {
		got[e.Name] = true
	}
	if len(spec.FirewallExempts) != 2 {
		t.Fatalf("expected 2 exemptions in spec, got %d: %+v", len(spec.FirewallExempts), spec.FirewallExempts)
	}
	if !got["BGP"] || !got["RIP"] {
		t.Errorf("enabled exemptions missing from spec: %+v", spec.FirewallExempts)
	}
	if got["OSPF"] {
		t.Error("disabled exemption must not be in the spec")
	}
}
