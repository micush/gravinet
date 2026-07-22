package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestDisabledHostNotAdvertised verifies that a disabled host record stays in
// config but is withheld from the runtime advertise spec, while enabled records
// (including legacy ones with no disabled field set) still flow through. This is
// the host analogue of the per-rule firewall enable/disable behaviour.
func TestDisabledHostNotAdvertised(t *testing.T) {
	n := config.Network{
		ID:      "1234",
		Name:    "lan",
		Enabled: true,
		HostsAdvertise: []config.HostRecord{
			{Name: "web.local", IP: "10.0.0.5"},                    // enabled (zero value)
			{Name: "db.local", IP: "10.0.0.6", Disabled: true},     // disabled
			{Name: "cache.local", IP: "10.0.0.7", Disabled: false}, // enabled explicitly
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0, nil)

	got := map[string]bool{}
	for _, h := range spec.AdvHosts {
		got[h.Name] = true
	}
	if len(spec.AdvHosts) != 2 {
		t.Fatalf("expected 2 advertised hosts, got %d: %+v", len(spec.AdvHosts), spec.AdvHosts)
	}
	if !got["web.local"] || !got["cache.local"] {
		t.Errorf("enabled records missing from spec: %+v", spec.AdvHosts)
	}
	if got["db.local"] {
		t.Error("disabled record must not be advertised")
	}
}

// TestHostRejectSpec verifies that enabled host-reject entries flow into the
// runtime spec and disabled ones are excluded.
func TestHostRejectSpec(t *testing.T) {
	n := config.Network{
		ID: "1234", Name: "lan", Enabled: true,
		HostsReject: []config.HostReject{
			{Name: "bad.local"},
			{Name: "off.local", Disabled: true},
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0, nil)
	if len(spec.HostReject) != 1 || spec.HostReject[0] != "bad.local" {
		t.Fatalf("expected only the enabled reject in spec, got %+v", spec.HostReject)
	}
}
