package main

import (
	"net/netip"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestDisabledRoutesNotInSpec verifies that disabled advertised routes and
// disabled reject entries stay in config but are excluded from the runtime
// spec, while enabled ones flow through. This is the route analogue of the
// per-rule firewall enable/disable behaviour.
func TestDisabledRoutesNotInSpec(t *testing.T) {
	n := config.Network{
		ID:      "1234",
		Name:    "lan",
		Enabled: true,
		Routes: []config.Route{
			{CIDR: "192.168.1.0/24", Enabled: true},
			{CIDR: "192.168.2.0/24", Enabled: false}, // disabled advertise
		},
		RouteRej: []config.RejectRoute{
			{CIDR: "10.0.0.0/8"},                    // enabled reject
			{CIDR: "172.16.0.0/12", Disabled: true}, // disabled reject
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0)

	// Advertised routes: only the enabled one reaches the spec.
	if len(spec.Routes) != 1 || spec.Routes[0] != netip.MustParsePrefix("192.168.1.0/24") {
		t.Fatalf("expected only the enabled advertised route, got %+v", spec.Routes)
	}

	// Reject entries: only the enabled one reaches the spec.
	if len(spec.RouteReject) != 1 {
		t.Fatalf("expected 1 reject rule, got %d: %+v", len(spec.RouteReject), spec.RouteReject)
	}
	if spec.RouteReject[0].Prefix != netip.MustParsePrefix("10.0.0.0/8") {
		t.Fatalf("wrong reject prefix survived: %+v", spec.RouteReject[0])
	}
}
