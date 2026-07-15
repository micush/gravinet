package main

import (
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// TestSearchDomainsRequireValidServer verifies that spec.SearchDomains is
// gated on the same "at least one valid server" check spec.AdvDNS applies,
// so a domain with no valid server never becomes a search suffix even
// though it also never becomes a routing entry. Before this fix a typo'd or
// empty server list still produced a search domain with nothing behind it —
// an unqualified query would complete against a domain with no forward
// registered for it at all.
func TestSearchDomainsRequireValidServer(t *testing.T) {
	n := config.Network{
		ID: "1234", Name: "lan", Enabled: true,
		DNSAdvertise: []config.DNSForward{
			{Domain: "good.internal", Servers: []string{"10.0.0.1"}},                // valid
			{Domain: "bad.internal", Servers: []string{"not-an-ip"}},                // no valid server
			{Domain: "empty.internal", Servers: nil},                                // no servers at all
			{Domain: "off.internal", Servers: []string{"10.0.0.2"}, Disabled: true}, // disabled
		},
	}
	var spec mesh.NetSpec
	fillRuntimeSpec(&spec, n, nil, 0, nil)

	if len(spec.AdvDNS) != 1 || spec.AdvDNS[0].Domain != "good.internal" {
		t.Fatalf("expected only good.internal in AdvDNS, got %+v", spec.AdvDNS)
	}
	if len(spec.SearchDomains) != 1 || spec.SearchDomains[0] != "good.internal" {
		t.Fatalf("expected only good.internal in SearchDomains, got %+v", spec.SearchDomains)
	}
}
