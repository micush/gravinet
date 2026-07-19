package webadmin

import (
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
)

func frrHas(t *testing.T, hay, needle string) {
	t.Helper()
	if !strings.Contains(hay, needle) {
		t.Errorf("expected to contain %q\n--- got ---\n%s", needle, hay)
	}
}

func frrLacks(t *testing.T, hay, needle string) {
	t.Helper()
	if strings.Contains(hay, needle) {
		t.Errorf("expected NOT to contain %q\n--- got ---\n%s", needle, hay)
	}
}

func daemonSet(b config.BGPConfig) map[string]bool {
	m := map[string]bool{}
	for _, d := range neededDaemons(b) {
		m[d] = true
	}
	return m
}

// gravinet always applies a fixed set of global BGP session-level knobs,
// unconditionally, whenever BGP is enabled — regardless of what else is
// configured. Since renderFRR fully regenerates frr.conf from scratch on
// every apply (rather than patching an existing file), there's no
// "if not present" branch to test separately: they're either in the output
// (enabled) or the whole router bgp block is absent (disabled).
func TestFRRGlobalBGPDirectives(t *testing.T) {
	always := []string{
		" bgp log-neighbor-changes\n",
		" no bgp ebgp-requires-policy\n",
		" bgp deterministic-med\n",
		" bgp bestpath as-path multipath-relax\n",
		" bgp conditional-advertisement timer 10\n",
	}
	// Present on a minimal config...
	c := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001})
	for _, line := range always {
		frrHas(t, c, line)
	}
	// ...and still present alongside a fully populated one — not crowded out
	// by router-id, timers, neighbors, or networks.
	full := renderFRR(config.BGPConfig{
		Enabled: true, ASN: 65001, RouterID: "1.2.3.4",
		KeepaliveTime: 4, HoldTime: 12,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}},
		Networks:  []string{"10.0.0.0/24"},
	})
	for _, line := range always {
		frrHas(t, full, line)
	}
	// Absent entirely when BGP is disabled — no router bgp block at all.
	d := renderFRR(config.BGPConfig{Enabled: false, ASN: 65001})
	for _, line := range always {
		frrLacks(t, d, line)
	}
}

// A minimal enabled BGP config must still emit a runnable `router bgp <asn>`
// block and request bgpd, even with nothing else filled in. Ported from
// parapet's bgp_minimal_block test.
func TestFRRBGPMinimalBlock(t *testing.T) {
	b := config.BGPConfig{Enabled: true, ASN: 65001}
	c := renderFRR(b)
	frrHas(t, c, "router bgp 65001\n")
	frrLacks(t, c, "bgp router-id") // no router-id line since field is empty
	if !daemonSet(b)["bgpd"] {
		t.Error("bgpd must be requested purely from enabled+asn")
	}
	// Disabled → no BGP block, bgpd not requested.
	d := renderFRR(config.BGPConfig{Enabled: false, ASN: 65001})
	frrLacks(t, d, "router bgp")
	if daemonSet(config.BGPConfig{Enabled: false, ASN: 65001})["bgpd"] {
		t.Error("bgpd must not be requested when disabled")
	}
}

// Password + per-neighbor BFD: an MD5 password is emitted, each neighbor's
// own BFD setting is independent (no global toggle to imply it), and bfdd is
// requested whenever any neighbor has it on. Ported from parapet's
// bgp_password_and_bfd test, adapted for gravinet's lack of a global toggle.
func TestFRRBGPPasswordAndBFD(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001, RouterID: "1.2.3.4",
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002, Password: "s3cr3t", BFD: false},
			{Peer: "10.0.0.3", RemoteAS: 65003, BFD: true},
		},
	}
	c := renderFRR(b)
	frrHas(t, c, " bgp router-id 1.2.3.4\n")
	frrHas(t, c, " neighbor 10.0.0.2 remote-as 65002\n")
	frrHas(t, c, " neighbor 10.0.0.2 password s3cr3t\n")
	// Each neighbor's own BFD setting, independently: on for .3, off for .2.
	frrLacks(t, c, "neighbor 10.0.0.2 bfd")
	frrHas(t, c, " neighbor 10.0.0.3 bfd\n")
	// address-family activation for each neighbor.
	frrHas(t, c, "  neighbor 10.0.0.2 activate\n")
	ds := daemonSet(b)
	if !ds["bgpd"] || !ds["bfdd"] {
		t.Errorf("expected bgpd+bfdd, got %v", ds)
	}
}

// A single neighbor with BFD still requests bfdd. Guards neededDaemons' only
// path to bfdd now that there's no global toggle.
func TestFRRPerNeighborBFDRequestsBfdd(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, BFD: true}},
	}
	if !daemonSet(b)["bfdd"] {
		t.Error("a single BFD neighbor must request bfdd")
	}
	// No BFD anywhere → no bfdd.
	nb := config.BGPConfig{Enabled: true, ASN: 65001, Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}}}
	if daemonSet(nb)["bfdd"] {
		t.Error("bfdd must not be requested with no BFD configured")
	}
}

// A neighbor with Shutdown set emits `neighbor <peer> shutdown`; one without
// it doesn't. Independent of BFD/password — a shut-down neighbor still gets
// its other directives (it stays fully configured, just held down).
func TestFRRNeighborShutdown(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002, Shutdown: true},
			{Peer: "10.0.0.3", RemoteAS: 65003},
		},
	}
	c := renderFRR(b)
	frrHas(t, c, " neighbor 10.0.0.2 shutdown\n")
	frrLacks(t, c, "10.0.0.3 shutdown")
	// Still activated in the address-family regardless of shutdown — shutdown
	// holds the session down administratively; it doesn't remove the neighbor
	// from AFI negotiation once lifted.
	frrHas(t, c, "  neighbor 10.0.0.2 activate\n")

	// Round-trips through the importer: a config file with the shutdown line
	// present sets Shutdown=true on that neighbor and only that neighbor.
	imported, _, ok := parseRunningConfigBGP(c)
	if !ok {
		t.Fatal("expected import to succeed")
	}
	if len(imported.Neighbors) != 2 {
		t.Fatalf("got %d neighbors, want 2", len(imported.Neighbors))
	}
	byPeer := map[string]config.BGPNeighbor{}
	for _, n := range imported.Neighbors {
		byPeer[n.Peer] = n
	}
	if !byPeer["10.0.0.2"].Shutdown {
		t.Error("expected 10.0.0.2 to import as Shutdown=true")
	}
	if byPeer["10.0.0.3"].Shutdown {
		t.Error("expected 10.0.0.3 to import as Shutdown=false")
	}
}

func TestFRRBGPPasswordWhitespaceStripped(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, Password: "bad pw"}},
	}
	frrHas(t, renderFRR(b), " neighbor 10.0.0.2 password badpw\n")
}

// Injection attempts in user-supplied tokens are filtered, never emitted.
// Ported from parapet's injection_is_filtered.
func TestFRRInjectionFiltered(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		// A network prefix carrying an injected directive must be rejected by
		// safeToken (it contains spaces and a ';'), so nothing leaks.
		Networks:  []string{"10.0.0.0/24 ; shutdown"},
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2 ; clear ip bgp", RemoteAS: 65002}},
	}
	c := renderFRR(b)
	frrLacks(t, c, "shutdown")
	frrLacks(t, c, "clear ip bgp")
	// A clean network on the same config still renders.
	b.Networks = append(b.Networks, "10.1.0.0/16")
	frrHas(t, renderFRR(b), "  network 10.1.0.0/16\n")
}

// A neighbor with a zero remote-as or empty peer is skipped entirely (matches
// parapet's render guards) rather than emitting a broken line.
func TestFRRSkipsInvalidNeighbors(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 0},     // zero AS — skip
			{Peer: "", RemoteAS: 65003},         // empty peer — skip
			{Peer: "10.0.0.4", RemoteAS: 65004}, // valid
		},
	}
	c := renderFRR(b)
	frrLacks(t, c, "neighbor 10.0.0.2")
	frrHas(t, c, " neighbor 10.0.0.4 remote-as 65004\n")
}

// An IPv6-addressed neighbor must be explicitly deactivated in
// `address-family ipv4 unicast` — FRR activates every neighbor there by
// default regardless of its own address family, so left alone a v6 peer
// would end up running an IPv4 unicast exchange over its v6 session — and
// activated instead in its own `address-family ipv6 unicast` block. An
// IPv4-addressed neighbor is unaffected: activated in ipv4 unicast, absent
// from the ipv6 block entirely.
func TestFRRIPv6NeighborDeactivatedInIPv4ActivatedInIPv6(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002},
			{Peer: "fd00::2", RemoteAS: 65003},
		},
	}
	c := renderFRR(b)
	v4Block, v6Block, found := strings.Cut(c, " address-family ipv6 unicast\n")
	if !found {
		t.Fatalf("expected an address-family ipv6 unicast block\n--- got ---\n%s", c)
	}
	frrHas(t, v4Block, "  neighbor 10.0.0.2 activate\n")
	frrHas(t, v4Block, "  no neighbor fd00::2 activate\n")
	frrLacks(t, v4Block, "  neighbor fd00::2 activate\n")
	frrHas(t, v6Block, "  neighbor fd00::2 activate\n")
	frrLacks(t, v6Block, "10.0.0.2")
}

// With no IPv6 neighbor and no IPv6 advertised network, no
// `address-family ipv6 unicast` block is emitted at all — there'd be
// nothing to activate in it.
func TestFRRNoIPv6AFBlockWhenUnneeded(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}},
		Networks:  []string{"10.0.0.0/24"},
	}
	c := renderFRR(b)
	frrLacks(t, c, "address-family ipv6 unicast")
}

// A v6-only advertised network (no v6 neighbor at all) still gets its own
// ipv6 unicast block — presence is keyed off having anything to carry, not
// specifically a v6 peer.
func TestFRRIPv6AFBlockFromNetworkAlone(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}},
		Networks:  []string{"fd00::/64"},
	}
	c := renderFRR(b)
	frrHas(t, c, " address-family ipv6 unicast\n")
	frrHas(t, c, "  network fd00::/64\n")
}

// Advertised networks are split by address family — a v6 prefix goes under
// ipv6 unicast only (FRR rejects a mismatched-family prefix), and vice
// versa. A redistribute selection with both v4 and v6 CIDRs produces a
// route-map reference under both address-family blocks (see
// renderRedistributeRouteMap) — the actual prefix-list/route-map contents
// are TestFRRRedistributeConnectedRouteMap's job; this test only checks
// which address-family blocks reference them.
func TestFRRNetworksSplitByFamily(t *testing.T) {
	b := config.BGPConfig{
		Enabled:                     true,
		ASN:                         65001,
		Neighbors:                   []config.BGPNeighbor{{Peer: "fd00::2", RemoteAS: 65002}},
		Networks:                    []string{"10.0.0.0/24", "fd00:1::/64"},
		RedistributeConnectedRoutes: []string{"172.16.0.0/24", "fd00:9::/64"},
		RedistributeStaticRoutes:    []string{"172.16.1.0/24", "fd00:10::/64"},
	}
	c := renderFRR(b)
	v4Block, v6Block, found := strings.Cut(c, " address-family ipv6 unicast\n")
	if !found {
		t.Fatalf("expected an address-family ipv6 unicast block\n--- got ---\n%s", c)
	}
	frrHas(t, v4Block, "  network 10.0.0.0/24\n")
	frrLacks(t, v4Block, "fd00:1::/64")
	frrHas(t, v6Block, "  network fd00:1::/64\n")
	frrLacks(t, v6Block, "10.0.0.0/24")
	frrHas(t, v4Block, "  redistribute connected route-map GRAVINET-REDIST-CONNECTED\n")
	frrHas(t, v4Block, "  redistribute static route-map GRAVINET-REDIST-STATIC\n")
	frrHas(t, v6Block, "  redistribute connected route-map GRAVINET-REDIST-CONNECTED\n")
	frrHas(t, v6Block, "  redistribute static route-map GRAVINET-REDIST-STATIC\n")
}

func TestFRRNetworksAndRedistribute(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Networks:                    []string{"10.0.0.0/24", "192.168.1.0/24"},
		RedistributeConnectedRoutes: []string{"172.16.0.0/24"},
		RedistributeStaticRoutes:    []string{"172.16.1.0/24"},
	}
	c := renderFRR(b)
	frrHas(t, c, "  network 10.0.0.0/24\n")
	frrHas(t, c, "  network 192.168.1.0/24\n")
	frrHas(t, c, "  redistribute connected route-map GRAVINET-REDIST-CONNECTED\n")
	frrHas(t, c, "  redistribute static route-map GRAVINET-REDIST-STATIC\n")
	frrHas(t, c, " address-family ipv4 unicast\n")
	frrHas(t, c, " exit-address-family\n")
}

// TestFRRRedistributeConnectedRouteMap covers renderRedistributeRouteMap's
// actual output: a permit prefix-list entry per selected CIDR (split into
// an `ip prefix-list` for v4 and an `ipv6 prefix-list` for v6 — FRR has no
// single keyword that accepts both), and a route-map with one sequence per
// family that's actually populated, each matching its own prefix-list.
// Rendered as top-level stanzas — siblings of `router bgp`, not nested
// inside it.
func TestFRRRedistributeConnectedRouteMap(t *testing.T) {
	b := config.BGPConfig{
		Enabled:                     true,
		ASN:                         65001,
		RedistributeConnectedRoutes: []string{"10.1.0.0/24", "10.2.0.0/24", "fd00:1::/64"},
	}
	c := renderFRR(b)
	frrHas(t, c, "ip prefix-list GRAVINET-REDIST-CONNECTED-V4 seq 5 permit 10.1.0.0/24\n")
	frrHas(t, c, "ip prefix-list GRAVINET-REDIST-CONNECTED-V4 seq 10 permit 10.2.0.0/24\n")
	frrHas(t, c, "ipv6 prefix-list GRAVINET-REDIST-CONNECTED-V6 seq 5 permit fd00:1::/64\n")
	frrHas(t, c, "route-map GRAVINET-REDIST-CONNECTED permit 10\n match ip address prefix-list GRAVINET-REDIST-CONNECTED-V4\n")
	frrHas(t, c, "route-map GRAVINET-REDIST-CONNECTED permit 20\n match ipv6 address prefix-list GRAVINET-REDIST-CONNECTED-V6\n")
	// The route-map stanza must appear before `router bgp` — it's a
	// top-level FRR construct, not something valid nested inside the BGP
	// stanza.
	if strings.Index(c, "route-map GRAVINET-REDIST-CONNECTED") > strings.Index(c, "router bgp 65001") {
		t.Error("route-map stanza appears after `router bgp` — it must be a sibling stanza, rendered before it")
	}
	// A v4-only selection must not touch the v6 prefix-list/route-map
	// sequence at all — no empty `ipv6 prefix-list` line, no seq 20.
	v4Only := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001, RedistributeConnectedRoutes: []string{"10.1.0.0/24"}})
	frrLacks(t, v4Only, "ipv6 prefix-list GRAVINET-REDIST-CONNECTED-V6")
	frrLacks(t, v4Only, "permit 20")
}

// A CIDR selected for redistribution but not currently a real
// connected/static route on the host isn't gravinet's concern to detect or
// prune here — renderFRR only ever renders what's selected; whether FRR's
// own zebra actually has a matching connected/static route to redistribute
// is between FRR and the kernel routing table, not something a prefix-list
// entry can be wrong about.
func TestFRRRedistributeEmptySelectionOmitsEverything(t *testing.T) {
	b := config.BGPConfig{Enabled: true, ASN: 65001}
	c := renderFRR(b)
	frrLacks(t, c, "redistribute connected")
	frrLacks(t, c, "redistribute static")
	frrLacks(t, c, "prefix-list")
	frrLacks(t, c, "route-map")
}

// bgpConfigRemovesSomething is the fix for a deleted neighbor not actually
// disappearing from FRR: it must report true whenever next drops a neighbor
// or network prev had, or tears down the whole BGP stanza, so applyBGP knows
// to force a restart rather than trust a reload (or the additive-only
// vtysh -b fallback) to notice the removal.
func TestBGPConfigRemovesSomething(t *testing.T) {
	base := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}, {Peer: "10.0.0.3", RemoteAS: 65003}},
		Networks:  []string{"10.0.0.0/24", "192.168.1.0/24"},
	}

	cases := []struct {
		name string
		next config.BGPConfig
		want bool
	}{
		{"identical config", base, false},
		{"neighbor removed", config.BGPConfig{
			Enabled: true, ASN: 65001,
			Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002}},
			Networks:  base.Networks,
		}, true},
		{"neighbor added, none removed", config.BGPConfig{
			Enabled: true, ASN: 65001,
			Neighbors: append(append([]config.BGPNeighbor{}, base.Neighbors...), config.BGPNeighbor{Peer: "10.0.0.4", RemoteAS: 65004}),
			Networks:  base.Networks,
		}, false},
		{"network removed", config.BGPConfig{
			Enabled: true, ASN: 65001,
			Neighbors: base.Neighbors,
			Networks:  []string{"10.0.0.0/24"},
		}, true},
		{"neighbor field edited, none removed/added", config.BGPConfig{
			Enabled: true, ASN: 65001,
			Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, Description: "edited"}, {Peer: "10.0.0.3", RemoteAS: 65003}},
			Networks:  base.Networks,
		}, false},
		{"BGP disabled entirely", config.BGPConfig{Enabled: false, ASN: 65001, Neighbors: base.Neighbors, Networks: base.Networks}, true},
		{"ASN changed", config.BGPConfig{Enabled: true, ASN: 65099, Neighbors: base.Neighbors, Networks: base.Networks}, true},
		{"first-ever save (zero prev)", base, false}, // prev is the zero value below, not base
	}

	for _, c := range cases {
		prev := base
		if c.name == "first-ever save (zero prev)" {
			prev = config.BGPConfig{}
		}
		if got := bgpConfigRemovesSomething(prev, c.next); got != c.want {
			t.Errorf("%s: bgpConfigRemovesSomething = %v, want %v", c.name, got, c.want)
		}
	}
}

// RedistributeMeshRoutes renders one `network` statement per selected CIDR
// that's also currently in meshRoutes, same mechanism as a manually-typed
// Networks entry — never FRR's `redistribute kernel`, which this feature
// exists specifically to avoid (see BGPConfig's doc comment): a mesh-learned
// route is just another kernel-table entry, so `redistribute kernel` would
// sweep in everything else on the host too.
func TestFRRRedistributeMesh(t *testing.T) {
	b := config.BGPConfig{Enabled: true, ASN: 65001, RedistributeMeshRoutes: []string{"10.10.0.0/24", "fd00:10::/64"}}
	c := renderFRR(b, "10.10.0.0/24", "fd00:10::/64")
	frrHas(t, c, "  network 10.10.0.0/24\n")
	frrLacks(t, c, "redistribute kernel")
	// The v6 mesh route lands in the ipv6 unicast block, not ipv4.
	if i4 := strings.Index(c, "address-family ipv4 unicast"); i4 >= 0 {
		if strings.Contains(c[:strings.Index(c, "exit-address-family")], "fd00:10::/64") {
			t.Error("v6 mesh route must not appear in the ipv4 unicast block")
		}
	}
	frrHas(t, c, "  network fd00:10::/64\n")

	// Off entirely when nothing is selected, even with routes passed in — the
	// caller (reconcileMeshRedistribute) always has a list handy, but it must
	// only be used for whatever the operator actually selected.
	off := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001}, "10.10.0.0/24")
	frrLacks(t, off, "10.10.0.0/24")

	// A partial selection — the whole point of this being a list rather than
	// a blanket toggle — must include only the selected CIDR, not every
	// route currently on the Mesh Routes page.
	partial := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001, RedistributeMeshRoutes: []string{"10.10.0.0/24"}}, "10.10.0.0/24", "10.20.0.0/24")
	frrHas(t, partial, "  network 10.10.0.0/24\n")
	frrLacks(t, partial, "10.20.0.0/24")

	// A selected CIDR that isn't (or is no longer) actually on the Mesh
	// Routes page contributes nothing — effectiveBGPNetworks intersects
	// against meshRoutes rather than trusting the selection outright.
	stale := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001, RedistributeMeshRoutes: []string{"10.30.0.0/24"}}, "10.10.0.0/24")
	frrLacks(t, stale, "10.30.0.0/24")
	frrLacks(t, stale, "10.10.0.0/24") // and the selection is empty of it, so nothing at all renders
}

// A CIDR that's both manually typed into Networks and selected via
// RedistributeMeshRoutes must not be emitted twice.
func TestFRRRedistributeMeshDedup(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001, RedistributeMeshRoutes: []string{"10.10.0.0/24", "10.20.0.0/24"},
		Networks: []string{"10.10.0.0/24"},
	}
	c := renderFRR(b, "10.10.0.0/24", "10.20.0.0/24")
	if n := strings.Count(c, "network 10.10.0.0/24\n"); n != 1 {
		t.Errorf("expected exactly one 'network 10.10.0.0/24' line, got %d in:\n%s", n, c)
	}
	frrHas(t, c, "  network 10.20.0.0/24\n")
}

// meshRedistributeRemovesSomething mirrors bgpConfigRemovesSomething for the
// mesh-derived side: it must catch a mesh route dropping off the Mesh Routes
// page or out of the selection, or BGP itself turning off, while leaving
// ordinary additions/no-ops alone.
func TestMeshRedistributeRemovesSomething(t *testing.T) {
	on := config.BGPConfig{Enabled: true, ASN: 65001, RedistributeMeshRoutes: []string{"10.0.0.0/24", "10.1.0.0/24"}}
	off := config.BGPConfig{Enabled: true, ASN: 65001}

	cases := []struct {
		name               string
		prev, next         config.BGPConfig
		prevMesh, nextMesh []string
		want               bool
	}{
		{"identical routes, nothing removed", on, on, []string{"10.0.0.0/24"}, []string{"10.0.0.0/24"}, false},
		{"route added, none removed", on, on, []string{"10.0.0.0/24"}, []string{"10.0.0.0/24", "10.1.0.0/24"}, false},
		{"route dropped", on, on, []string{"10.0.0.0/24", "10.1.0.0/24"}, []string{"10.0.0.0/24"}, true},
		{"toggle turned off with routes live", on, off, []string{"10.0.0.0/24"}, nil, true},
		{"toggle turned off, nothing was ever live", on, off, nil, nil, false},
		{"toggle was never on", off, off, nil, []string{"10.0.0.0/24"}, false},
		{"toggle just turned on", off, on, nil, []string{"10.0.0.0/24"}, false},
		{"BGP disabled entirely while routes were live", on, config.BGPConfig{Enabled: false, ASN: 65001, RedistributeMeshRoutes: on.RedistributeMeshRoutes}, []string{"10.0.0.0/24"}, nil, true},
	}
	for _, c := range cases {
		if got := meshRedistributeRemovesSomething(c.prev, c.next, c.prevMesh, c.nextMesh); got != c.want {
			t.Errorf("%s: meshRedistributeRemovesSomething = %v, want %v", c.name, got, c.want)
		}
	}
}

// syncDaemonsContent flips the managed daemons to match the wanted set and
// leaves unmanaged lines untouched. Ported from parapet's sync_daemons logic.
func TestSyncDaemonsContent(t *testing.T) {
	// A representative /etc/frr/daemons: bgpd/bfdd off, staticd on, plus a
	// comment and an unmanaged setting line that must be preserved verbatim.
	existing := "# frr daemons\n" +
		"bgpd=no\n" +
		"ospfd=no\n" +
		"bfdd=no\n" +
		"staticd=yes\n" +
		"vtysh_enable=yes\n"
	want := neededDaemons(config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, BFD: true}},
	}) // staticd, bgpd, bfdd

	body, changed := syncDaemonsContent(existing, want)
	if !changed {
		t.Fatal("expected a change (bgpd/bfdd flipped on)")
	}
	frrHas(t, body, "bgpd=yes\n")
	frrHas(t, body, "bfdd=yes\n")
	frrHas(t, body, "staticd=yes\n")
	frrHas(t, body, "ospfd=no\n") // wanted-out managed daemon stays off
	// Non-managed lines are preserved exactly.
	frrHas(t, body, "# frr daemons\n")
	frrHas(t, body, "vtysh_enable=yes\n")

	// Idempotent: re-running with the same want yields no change.
	body2, changed2 := syncDaemonsContent(body, want)
	if changed2 {
		t.Error("second sync with same want must be a no-op")
	}
	if body2 != body {
		t.Error("idempotent sync changed the body")
	}

	// Turning BGP off flips bgpd/bfdd back to =no.
	off := neededDaemons(config.BGPConfig{Enabled: false})
	body3, changed3 := syncDaemonsContent(body, off)
	if !changed3 {
		t.Error("disabling BGP should flip bgpd/bfdd off")
	}
	frrHas(t, body3, "bgpd=no\n")
	frrHas(t, body3, "bfdd=no\n")
	frrHas(t, body3, "staticd=yes\n") // staticd always wanted
}

// enableDaemonsContent flips bgpd/bfdd from =no to =yes on FRR detection while
// leaving every other line — including other managed daemons and unrelated
// settings — exactly as found, and never disables anything.
func TestEnableDaemonsContent(t *testing.T) {
	// A stock /etc/frr/daemons: everything off, plus a comment, an already-on
	// unrelated daemon, and an unmanaged setting line that must survive verbatim.
	existing := "# frr daemons\n" +
		"bgpd=no\n" +
		"ospfd=no\n" +
		"bfdd=no\n" +
		"staticd=yes\n" +
		"vtysh_enable=yes\n"

	// bgpd/bfdd, spelled out here rather than referencing frrBGPBFDDaemons
	// directly: that var now lives in frr_default.go (tagged !freebsd, since
	// FreeBSD's ensureFRRDaemonsEnabled has nothing analogous to enable — see
	// its doc comment), so this file needs to stand on its own to compile and
	// run on every platform, FreeBSD included.
	bgpBFD := []string{"bgpd", "bfdd"}

	body, changed := enableDaemonsContent(existing, bgpBFD)
	if !changed {
		t.Fatal("expected a change (bgpd/bfdd flipped on)")
	}
	frrHas(t, body, "bgpd=yes\n")
	frrHas(t, body, "bfdd=yes\n")
	// Only bgpd/bfdd are touched: ospfd stays off, and nothing else moves.
	frrHas(t, body, "ospfd=no\n")
	frrHas(t, body, "staticd=yes\n")
	frrHas(t, body, "# frr daemons\n")
	frrHas(t, body, "vtysh_enable=yes\n")

	// Idempotent: a second pass with them already on reports no change.
	body2, changed2 := enableDaemonsContent(body, bgpBFD)
	if changed2 {
		t.Error("second pass with bgpd/bfdd already on must be a no-op")
	}
	if body2 != body {
		t.Error("idempotent enable changed the body")
	}

	// One already on, one off: still a change, and it only enables (never the
	// reverse).
	mixed := "bgpd=yes\nbfdd=no\n"
	out, changed3 := enableDaemonsContent(mixed, bgpBFD)
	if !changed3 {
		t.Error("bfdd=no should flip to yes")
	}
	frrHas(t, out, "bgpd=yes\n")
	frrHas(t, out, "bfdd=yes\n")
	frrLacks(t, out, "bgpd=no")
	frrLacks(t, out, "bfdd=no")
}

// The exact bug: FRR has an existing BGP config with established peers, gravinet
// has none. parseRunningConfigBGP must reconstruct that config from FRR's
// `show running-config` output so the page reflects reality instead of empty.
func TestParseRunningConfigBGP(t *testing.T) {
	// Representative FRR running-config: a router bgp stanza with router-id, two
	// neighbors (one with description + password + bfd), an address-family with
	// networks/redistribute/activate, wrapped in surrounding unrelated config.
	rc := "frr version 8.4\n" +
		"!\n" +
		"router bgp 65001\n" +
		" bgp router-id 10.0.0.1\n" +
		" neighbor 10.0.0.2 remote-as 65002\n" +
		" neighbor 10.0.0.2 description core uplink\n" +
		" neighbor 10.0.0.2 password s3cr3t\n" +
		" neighbor 10.0.0.2 bfd\n" +
		" neighbor 10.0.0.3 remote-as 65003\n" +
		" !\n" +
		" address-family ipv4 unicast\n" +
		"  network 10.0.0.0/24\n" +
		"  network 192.168.5.0/24\n" +
		"  redistribute connected\n" +
		"  neighbor 10.0.0.2 activate\n" +
		"  neighbor 10.0.0.3 activate\n" +
		" exit-address-family\n" +
		"exit\n" +
		"!\n" +
		"line vty\n" +
		"!\n"

	cfg, hasPw, ok := parseRunningConfigBGP(rc)
	if !ok {
		t.Fatal("expected to find a BGP stanza")
	}
	if !cfg.Enabled || cfg.ASN != 65001 {
		t.Errorf("asn/enabled wrong: %+v", cfg)
	}
	if cfg.RouterID != "10.0.0.1" {
		t.Errorf("router-id = %q, want 10.0.0.1", cfg.RouterID)
	}
	// A bare "redistribute connected" (no route-map — an externally-authored
	// blanket directive) has no specific CIDRs to import into the new
	// selective fields, so it simply doesn't round-trip — see
	// parseRunningConfigBGP's doc comment.
	if len(cfg.RedistributeConnectedRoutes) != 0 || len(cfg.RedistributeStaticRoutes) != 0 {
		t.Errorf("redistribute selections should be empty on import: %+v / %+v", cfg.RedistributeConnectedRoutes, cfg.RedistributeStaticRoutes)
	}
	if len(cfg.Networks) != 2 || cfg.Networks[0] != "10.0.0.0/24" || cfg.Networks[1] != "192.168.5.0/24" {
		t.Errorf("networks wrong: %+v", cfg.Networks)
	}
	if len(cfg.Neighbors) != 2 {
		t.Fatalf("got %d neighbors, want 2 (activate lines must not create extras): %+v", len(cfg.Neighbors), cfg.Neighbors)
	}
	n0 := cfg.Neighbors[0]
	if n0.Peer != "10.0.0.2" || n0.RemoteAS != 65002 || n0.Description != "core uplink" || !n0.BFD {
		t.Errorf("neighbor[0] wrong: %+v", n0)
	}
	if n0.Password != "" {
		t.Errorf("password must not be imported, got %q", n0.Password)
	}
	if !hasPw {
		t.Error("hasPasswords should be true (one neighbor had a password line)")
	}
	if cfg.Neighbors[1].Peer != "10.0.0.3" || cfg.Neighbors[1].RemoteAS != 65003 {
		t.Errorf("neighbor[1] wrong: %+v", cfg.Neighbors[1])
	}
}

// No BGP stanza → ok=false, so the handler shows an empty editor rather than a
// bogus enabled config.
func TestParseRunningConfigBGPNone(t *testing.T) {
	for _, rc := range []string{"", "frr version 8.4\n!\nline vty\n!\n", "interface eth0\n ip address 10.0.0.1/24\n!\n"} {
		if _, _, ok := parseRunningConfigBGP(rc); ok {
			t.Errorf("expected no BGP stanza for %q", rc)
		}
	}
}

// The stanza must not bleed into following top-level config: a neighbor-like
// line after the stanza ends is ignored.
func TestParseRunningConfigBGPStanzaBoundary(t *testing.T) {
	rc := "router bgp 65001\n" +
		" neighbor 10.0.0.2 remote-as 65002\n" +
		"exit\n" +
		"router ospf\n" +
		" network 10.9.9.0/24 area 0\n" +
		"!\n"
	cfg, _, ok := parseRunningConfigBGP(rc)
	if !ok || cfg.ASN != 65001 {
		t.Fatalf("bgp parse failed: %+v", cfg)
	}
	if len(cfg.Neighbors) != 1 {
		t.Errorf("stanza boundary leaked: got %d neighbors, want 1", len(cfg.Neighbors))
	}
	for _, n := range cfg.Networks {
		if n == "10.9.9.0/24 area 0" || n == "10.9.9.0/24" {
			t.Error("OSPF network leaked into BGP import")
		}
	}
}

func TestRunVtyshAbsent(t *testing.T) {
	// With no vtysh present, runVtysh must return promptly with ok=false and
	// never spawn a process or block — this is what keeps the BGP endpoints
	// fast on hosts without FRR.
	withStatFile(t, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist })
	done := make(chan struct{})
	go func() {
		if out, ok := runVtysh("show running-config"); ok || out != nil {
			t.Errorf("runVtysh with no vtysh = (%v,%v), want (nil,false)", out, ok)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runVtysh did not return promptly when vtysh is absent")
	}
	// importBGPFromFRR builds on it and must also report nothing to import,
	// with a non-empty diagnostic reason.
	if _, _, ok, reason := importBGPFromFRR(nil); ok {
		t.Error("importBGPFromFRR should report ok=false when vtysh is absent")
	} else if reason == "" {
		t.Error("importBGPFromFRR should explain why nothing was imported")
	}
}

// The reported mismatch: FRR has a live BGP session (32-bit ASNs) but the
// editor showed empty. summaryToBGPConfig must rebuild the config from the same
// summary JSON the live-peers panel uses, so the editor matches. Values here
// mirror the screenshot: local AS 4216805503, router id 192.168.55.3, peer
// 192.168.55.1 / remote AS 4216825503.
// End-to-end regression for the reported bug: an existing /etc/frr/frr.conf
// (parapet-managed, with a neighbor whose session may be down) must import from
// the file alone — no dependency on vtysh/bgpd. Content is the operator's exact
// config, trailing spaces and all.
func TestImportBGPFromFRRFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/frr.conf"
	body := "! parapet-managed FRR configuration. Do not edit by hand. \n" +
		"frr defaults traditional \n! \n" +
		"router bgp 65001 \n" +
		" neighbor 10.1.1.1 remote-as 65003 \n" +
		" neighbor 10.1.1.1 password pass \n" +
		" neighbor 10.1.1.1 bfd \n" +
		" address-family ipv4 unicast \n" +
		"  network 10.1.1.0/24 \n" +
		"  network 10.1.2.0/24 \n" +
		"  neighbor 10.1.1.1 activate \n" +
		" exit-address-family \n!\n \n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := frrConfigPaths
	frrConfigPaths = []string{path}
	t.Cleanup(func() { frrConfigPaths = orig })
	// vtysh is absent in the test env (statFile seam), so this exercises the
	// file path with no daemon — exactly the failing scenario.
	withStatFile(t, func(string) (fs.FileInfo, error) { return nil, os.ErrNotExist })

	cfg, hasPw, ok, _ := importBGPFromFRR(nil)
	if !ok {
		t.Fatal("expected import to succeed from the config file alone")
	}
	if !cfg.Enabled || cfg.ASN != 65001 {
		t.Errorf("asn/enabled wrong: %+v", cfg)
	}
	if len(cfg.Neighbors) != 1 || cfg.Neighbors[0].Peer != "10.1.1.1" ||
		cfg.Neighbors[0].RemoteAS != 65003 || !cfg.Neighbors[0].BFD {
		t.Errorf("neighbor wrong: %+v", cfg.Neighbors)
	}
	if len(cfg.Networks) != 2 || cfg.Networks[0] != "10.1.1.0/24" || cfg.Networks[1] != "10.1.2.0/24" {
		t.Errorf("networks wrong: %+v", cfg.Networks)
	}
	if !hasPw {
		t.Error("expected password presence to be flagged")
	}
}

// A config that arrived with Windows (CRLF) line endings must still parse: the
// trailing \r on the ASN token used to break stanza detection and silently
// import nothing.
func TestParseRunningConfigBGPCRLF(t *testing.T) {
	rc := "router bgp 65001\r\n" +
		" bgp router-id 10.0.0.1\r\n" +
		" neighbor 10.0.0.2 remote-as 65002\r\n" +
		" address-family ipv4 unicast\r\n" +
		"  network 10.0.0.0/24\r\n" +
		"  neighbor 10.0.0.2 activate\r\n" +
		" exit-address-family\r\n"
	cfg, _, ok := parseRunningConfigBGP(rc)
	if !ok {
		t.Fatal("CRLF config should still parse (trailing \\r must not break the ASN)")
	}
	if cfg.ASN != 65001 || cfg.RouterID != "10.0.0.1" {
		t.Errorf("asn/router-id wrong from CRLF input: %+v", cfg)
	}
	if len(cfg.Neighbors) != 1 || cfg.Neighbors[0].Peer != "10.0.0.2" || cfg.Neighbors[0].RemoteAS != 65002 {
		t.Errorf("neighbor wrong from CRLF input: %+v", cfg.Neighbors)
	}
	if len(cfg.Networks) != 1 || cfg.Networks[0] != "10.0.0.0/24" {
		t.Errorf("networks wrong from CRLF input: %+v", cfg.Networks)
	}
}

func TestSummaryToBGPConfig(t *testing.T) {
	sum := []byte(`{
	  "ipv4Unicast": {
	    "routerId": "192.168.55.3",
	    "as": 4216805503,
	    "peers": {
	      "192.168.55.1": {"remoteAs": 4216825503, "state": "Established", "peerUptimeMsec": 85911000, "pfxRcd": 17}
	    }
	  }
	}`)
	cfg, at, ok := summaryToBGPConfig(sum)
	if !ok {
		t.Fatal("expected ok for a running BGP speaker")
	}
	if !cfg.Enabled {
		t.Error("imported config should be enabled")
	}
	if cfg.ASN != 4216805503 {
		t.Errorf("ASN = %d, want 4216805503 (32-bit ASN must not truncate)", cfg.ASN)
	}
	if cfg.RouterID != "192.168.55.3" {
		t.Errorf("router-id = %q, want 192.168.55.3", cfg.RouterID)
	}
	if len(cfg.Neighbors) != 1 {
		t.Fatalf("got %d neighbors, want 1: %+v", len(cfg.Neighbors), cfg.Neighbors)
	}
	if cfg.Neighbors[0].Peer != "192.168.55.1" || cfg.Neighbors[0].RemoteAS != 4216825503 {
		t.Errorf("neighbor = %+v, want 192.168.55.1/4216825503", cfg.Neighbors[0])
	}
	if at["192.168.55.1"] != 0 {
		t.Errorf("index map wrong: %+v", at)
	}
}

// A dual-stack peer present in both address families must appear once, and a
// speaker with no peers yet (just a local AS) still imports.
func TestSummaryToBGPConfigDedupAndEmpty(t *testing.T) {
	dual := []byte(`{
	  "ipv4Unicast": {"as": 65001, "routerId": "10.0.0.1", "peers": {"fd00::2": {"remoteAs": 65002, "state": "Established", "peerUptimeMsec": 1000}}},
	  "ipv6Unicast": {"as": 65001, "routerId": "10.0.0.1", "peers": {"fd00::2": {"remoteAs": 65002, "state": "Established", "peerUptimeMsec": 1000}}}
	}`)
	cfg, _, ok := summaryToBGPConfig(dual)
	if !ok || len(cfg.Neighbors) != 1 {
		t.Errorf("dual-stack peer should dedup to 1 neighbor, got %d (ok=%v)", len(cfg.Neighbors), ok)
	}

	// Local AS, no peers → still a valid import (an enabled speaker).
	solo := []byte(`{"ipv4Unicast": {"as": 65001, "routerId": "10.0.0.1", "peers": {}}}`)
	cfg2, _, ok2 := summaryToBGPConfig(solo)
	if !ok2 || cfg2.ASN != 65001 {
		t.Errorf("speaker with no peers should still import: ok=%v asn=%d", ok2, cfg2.ASN)
	}

	// Nothing at all → not ok.
	if _, _, ok3 := summaryToBGPConfig([]byte(`{}`)); ok3 {
		t.Error("empty summary should not import")
	}
}

// renderFRR's own output — including the new ipv6 unicast block — must
// round-trip cleanly through parseRunningConfigBGP: the parser doesn't
// branch on which address-family stanza a line is nested under, so both
// the v6 neighbor and the v6 network land back in cfg regardless. The
// `no neighbor fd00::2 activate` line in the v4 block is a negation and is
// correctly skipped rather than misread as clearing the neighbor.
func TestFRRIPv6RenderParseRoundTrip(t *testing.T) {
	b := config.BGPConfig{
		Enabled: true, ASN: 65001,
		Neighbors: []config.BGPNeighbor{
			{Peer: "10.0.0.2", RemoteAS: 65002},
			{Peer: "fd00::2", RemoteAS: 65003, Description: "v6 peer"},
		},
		Networks: []string{"10.0.0.0/24", "fd00:1::/64"},
	}
	rendered := renderFRR(b)
	cfg, _, ok := parseRunningConfigBGP(rendered)
	if !ok {
		t.Fatalf("round-trip parse failed\n--- rendered ---\n%s", rendered)
	}
	if len(cfg.Neighbors) != 2 {
		t.Fatalf("expected 2 neighbors back, got %d: %+v", len(cfg.Neighbors), cfg.Neighbors)
	}
	byPeer := map[string]config.BGPNeighbor{}
	for _, n := range cfg.Neighbors {
		byPeer[n.Peer] = n
	}
	if n, ok := byPeer["fd00::2"]; !ok || n.RemoteAS != 65003 || n.Description != "v6 peer" {
		t.Errorf("v6 neighbor didn't round-trip cleanly: %+v (present=%v)", n, ok)
	}
	if n, ok := byPeer["10.0.0.2"]; !ok || n.RemoteAS != 65002 {
		t.Errorf("v4 neighbor didn't round-trip cleanly: %+v (present=%v)", n, ok)
	}
	wantNets := map[string]bool{"10.0.0.0/24": true, "fd00:1::/64": true}
	if len(cfg.Networks) != 2 {
		t.Fatalf("expected 2 networks back, got %d: %v", len(cfg.Networks), cfg.Networks)
	}
	for _, net := range cfg.Networks {
		if !wantNets[net] {
			t.Errorf("unexpected network %q in round-trip result: %v", net, cfg.Networks)
		}
	}
}

func TestFRRTimersRenderAndParse(t *testing.T) {
	// Rendered when set; the 3:1 fast default the UI seeds new configs with.
	c := renderFRR(config.BGPConfig{Enabled: true, ASN: 65001, KeepaliveTime: 4, HoldTime: 12})
	frrHas(t, c, " timers bgp 4 12\n")
	// Omitted entirely when unset (0/0) → FRR uses its own defaults.
	frrLacks(t, renderFRR(config.BGPConfig{Enabled: true, ASN: 65001}), "timers bgp")

	// Round-trips through the running-config parser.
	rc := "router bgp 65001\n timers bgp 4 12\n neighbor 10.0.0.2 remote-as 65002\nexit\n!\n"
	cfg, _, ok := parseRunningConfigBGP(rc)
	if !ok || cfg.KeepaliveTime != 4 || cfg.HoldTime != 12 {
		t.Errorf("timer parse: ok=%v ka=%d hold=%d, want 4/12", ok, cfg.KeepaliveTime, cfg.HoldTime)
	}
}

func TestSafeToken(t *testing.T) {
	good := []string{"10.0.0.1", "fd00::2", "10.0.0.0/24", "eth0", "peer-1_a"}
	bad := []string{"", "has space", "semi;colon", "back`tick", "pipe|x", strings.Repeat("x", 65)}
	for _, g := range good {
		if !safeToken(g) {
			t.Errorf("safeToken(%q) = false, want true", g)
		}
	}
	for _, b := range bad {
		if safeToken(b) {
			t.Errorf("safeToken(%q) = true, want false", b)
		}
	}
}
