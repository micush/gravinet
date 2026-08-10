package main

import (
	"net/netip"
	"reflect"
	"testing"

	"gravinet/internal/config"
)

func pfx(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// The reported failure: BGP redistributes a LAN prefix, the kernel installs a
// route pointing at a peer's overlay next-hop, and the data plane drops the
// packet because nothing ever told the mesh which peer owns that prefix. The
// fix is that naming it under BGP is enough.
func TestAutoMeshRoutesFromBGPCoversRedistributedPrefixes(t *testing.T) {
	bgp := config.BGPConfig{
		RedistributeConnectedRoutes: []string{"10.1.1.0/24", "fd0a:1::/64"},
	}
	got := autoMeshRoutesFromBGP(config.Network{}, bgp)
	if want := pfx("10.1.1.0/24", "fd0a:1::/64"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A default route must never be flooded into the mesh: it would tell every
// peer to send this node all their unmatched traffic. The bundle that
// prompted this change had 0.0.0.0/0 configured, so this is the live case,
// not a hypothetical.
func TestAutoMeshRoutesRefusesDefaults(t *testing.T) {
	bgp := config.BGPConfig{
		Networks:                    []string{"0.0.0.0/0"}, // must be ignored entirely
		RedistributeConnectedRoutes: []string{"0.0.0.0/0", "::/0", "10.1.1.0/24"},
		RedistributeStaticRoutes:    []string{"0.0.0.0/0"},
	}
	got := autoMeshRoutesFromBGP(config.Network{}, bgp)
	if want := pfx("10.1.1.0/24"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want only the non-default prefix (%v)", got, want)
	}
}

// BGPConfig.Networks is the "originate into BGP" list and routinely holds
// aggregates this node cannot itself deliver. Advertising one into the mesh
// would be a claim this node can't honour.
func TestAutoMeshRoutesIgnoresBGPNetworks(t *testing.T) {
	bgp := config.BGPConfig{Networks: []string{"192.0.2.0/24"}}
	if got := autoMeshRoutesFromBGP(config.Network{}, bgp); len(got) != 0 {
		t.Fatalf("BGP network statements must not become mesh routes, got %v", got)
	}
}

// An explicit Mesh Routes entry always wins. The disabled case is the one
// that matters: silently resurrecting a route an operator switched off would
// be exactly the kind of surprise this whole area has already produced once.
func TestAutoMeshRoutesRespectsExplicitEntries(t *testing.T) {
	n := config.Network{Routes: []config.Route{
		{CIDR: "10.1.1.0/24", Enabled: true, Metric: 50},
		{CIDR: "10.9.9.0/24", Enabled: false},
	}}
	bgp := config.BGPConfig{RedistributeConnectedRoutes: []string{"10.1.1.0/24", "10.9.9.0/24", "10.2.2.0/24"}}

	got := autoMeshRoutesFromBGP(n, bgp)
	if want := pfx("10.2.2.0/24"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want only the prefix not already on the Routes page (%v)", got, want)
	}
}

// Duplicates across the two redistribute lists collapse, and an unmasked
// prefix still matches an explicit entry written in canonical form.
func TestAutoMeshRoutesDedupesAndMasks(t *testing.T) {
	bgp := config.BGPConfig{
		RedistributeConnectedRoutes: []string{"10.1.1.0/24", " 10.1.1.0/24 "},
		RedistributeStaticRoutes:    []string{"10.1.1.5/24", "not-a-prefix"},
	}
	got := autoMeshRoutesFromBGP(config.Network{}, bgp)
	if want := pfx("10.1.1.0/24"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
