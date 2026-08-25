package webadmin

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gravinet/internal/config"
)

func suggestFor(t *testing.T, cidr string) dhcpSuggestion {
	t.Helper()
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		t.Fatalf("bad test prefix %q: %v", cidr, err)
	}
	s, ok := suggestDHCPSubnet(p)
	if !ok {
		t.Fatalf("no suggestion for %s", cidr)
	}
	return s
}

// The ordinary case: a gateway at .1 on a /24, and a pool that starts and ends
// a margin short of each edge.
func TestSuggestDHCPSubnetTypical(t *testing.T) {
	s := suggestFor(t, "10.1.1.1/24")
	if s.Subnet != "10.1.1.0/24" {
		t.Errorf("subnet = %q, want the interface's own network", s.Subnet)
	}
	if s.Router != "10.1.1.1" {
		t.Errorf("router = %q, want the interface's own address", s.Router)
	}
	if s.PoolStart != "10.1.1.10" || s.PoolEnd != "10.1.1.245" {
		t.Errorf("pool = %s-%s, want 10.1.1.10-10.1.1.245", s.PoolStart, s.PoolEnd)
	}
}

// An address with host bits set is what an interface actually carries; the
// subnet suggested from it is the network, not the address.
func TestSuggestDHCPSubnetMasks(t *testing.T) {
	if s := suggestFor(t, "192.168.7.34/22"); s.Subnet != "192.168.4.0/22" {
		t.Errorf("subnet = %q, want 192.168.4.0/22", s.Subnet)
	}
}

// The one that would otherwise produce a form the validator rejects: an
// interface addressed in the middle of its own subnet, where the naive pool
// contains the gateway. The pool has to move off it, keeping the wider side.
func TestSuggestDHCPSubnetRouterInsidePool(t *testing.T) {
	s := suggestFor(t, "10.1.1.50/24")
	if s.PoolStart != "10.1.1.60" || s.PoolEnd != "10.1.1.245" {
		t.Errorf("pool = %s-%s, want 10.1.1.60-10.1.1.245 (the wider side, clear of the gateway)",
			s.PoolStart, s.PoolEnd)
	}
	// And the narrower side when that is the one with room.
	s = suggestFor(t, "10.1.1.200/24")
	if s.PoolStart != "10.1.1.10" || s.PoolEnd != "10.1.1.190" {
		t.Errorf("pool = %s-%s, want 10.1.1.10-10.1.1.190", s.PoolStart, s.PoolEnd)
	}
}

// The margin shrinks on a subnet too small to spare ten at each end rather
// than giving up on a pool altogether.
func TestSuggestDHCPSubnetSmall(t *testing.T) {
	s := suggestFor(t, "10.0.0.9/29") // .8-.15
	if s.Subnet != "10.0.0.8/29" {
		t.Fatalf("subnet = %q", s.Subnet)
	}
	if s.PoolStart != "10.0.0.11" || s.PoolEnd != "10.0.0.12" {
		t.Errorf("pool = %s-%s, want 10.0.0.11-10.0.0.12", s.PoolStart, s.PoolEnd)
	}
}

// A prefix with no host range still fills in the subnet and the gateway. The
// pool is left blank rather than being invented, because there is no valid one
// to invent and an invalid suggestion is worse than none.
func TestSuggestDHCPSubnetNoHostRange(t *testing.T) {
	for _, cidr := range []string{"10.0.0.1/31", "10.0.0.1/32"} {
		s := suggestFor(t, cidr)
		if s.Router != "10.0.0.1" || s.Subnet == "" {
			t.Errorf("%s: subnet/router should still be filled in, got %+v", cidr, s)
		}
		if s.PoolStart != "" || s.PoolEnd != "" {
			t.Errorf("%s: suggested a pool %s-%s where there is no host range",
				cidr, s.PoolStart, s.PoolEnd)
		}
	}
}

// IPv6 is not suggested from at all: DHCP here is v4, and a v6 subnet would be
// rejected on save by DHCPSubnet.Validate.
func TestSuggestDHCPSubnetIgnoresV6(t *testing.T) {
	p := netip.MustParsePrefix("2001:db8::1/64")
	if _, ok := suggestDHCPSubnet(p); ok {
		t.Error("suggested a subnet from an IPv6 address")
	}
}

// The property that matters more than any individual boundary: whatever the
// page fills in must survive the validator that runs when it is saved. A
// prefill the form then refuses is the form arguing with itself, and it is the
// failure every off-by-one in the arithmetic above turns into.
func TestSuggestDHCPSubnetAlwaysValidates(t *testing.T) {
	cidrs := []string{
		"10.1.1.1/24", "10.1.1.50/24", "10.1.1.200/24", "10.1.1.254/24",
		"192.168.4.34/22", "172.16.0.1/16", "10.0.0.9/29", "10.0.0.1/30",
		"10.0.0.6/29", "203.0.113.1/25", "198.51.100.130/26", "10.255.255.250/24",
	}
	for _, cidr := range cidrs {
		s := suggestFor(t, cidr)
		if s.PoolStart == "" {
			continue // nothing to serve; the page leaves the pool to the operator
		}
		sub := config.DHCPSubnet{
			Iface: "eth1", Subnet: s.Subnet,
			PoolStart: s.PoolStart, PoolEnd: s.PoolEnd, Router: s.Router,
		}
		if err := sub.Validate(); err != nil {
			t.Errorf("%s suggested %+v, which does not validate: %v", cidr, s, err)
		}
	}
}

// Loopback is the entry that matters. A host resolving through 127.0.0.1 is
// pointing at something running on itself; handed to a client, the same
// address means the client's own loopback, so the setting that looks most like
// working DNS is the one guaranteed not to be.
func TestFilterDNSv4(t *testing.T) {
	got := filterDNSv4([]string{
		"127.0.0.53", "10.1.1.1", "2001:db8::1", "  9.9.9.9  ",
		"10.1.1.1", "0.0.0.0", "not-an-address", "224.0.0.1",
	})
	want := []string{"10.1.1.1", "9.9.9.9"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Never nil, so the field is filled with an empty string rather than the page
// having to tell "no resolvers" from "no reply".
func TestFilterDNSv4EmptyNotNil(t *testing.T) {
	if got := filterDNSv4(nil); got == nil {
		t.Error("filterDNSv4(nil) is nil; it should be an empty list")
	}
}

// The distinction the preflight branches on: addresses that were read and
// contain no usable IPv4 are an empty list, not a nil one. Conflating the two
// is what left "interface has no IPv4 address" unreachable through v944 —
// dhcpServerProblem and dhcpRelayProblem both return early on nil, so the
// message they were written to produce could never be reached.
func TestV4PrefixesEmptyNotNil(t *testing.T) {
	v6 := &net.IPNet{IP: net.ParseIP("2001:db8::1"), Mask: net.CIDRMask(64, 128)}
	lo := &net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)}
	ll := &net.IPNet{IP: net.ParseIP("169.254.3.4"), Mask: net.CIDRMask(16, 32)}
	got := v4Prefixes([]net.Addr{v6, lo, ll})
	if got == nil {
		t.Fatal("v4Prefixes returned nil for an interface whose addresses were read; " +
			"the preflight reads that as a failed lookup and stays quiet")
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none of these kept", got)
	}
	ok := &net.IPNet{IP: net.ParseIP("10.1.1.5"), Mask: net.CIDRMask(24, 32)}
	if p := v4Prefixes([]net.Addr{v6, ok}); len(p) != 1 || p[0].String() != "10.1.1.5/24" {
		t.Errorf("got %v, want [10.1.1.5/24]", p)
	}
}

// 'dhcp' is an acronym section key like nat/qos/dns/bgp/api/snmp/lldp, so
// label() has to uppercase it through that shared branch rather than
// title-casing it into "Dhcp" — which is what the rail, the page's own <h2>
// (sectionHeading returns label(s) for everything but ipv6ra) and the global
// search index all read through v945. Pinned on the ternary itself, the same
// way the lldp case is, because that branch is what renders all three.
func TestDHCPSectionLabelIsUppercased(t *testing.T) {
	upper := indexHTML[strings.Index(indexHTML, "return s==='nat'"):]
	upper = upper[:strings.Index(upper, "\n")]
	if !strings.Contains(upper, "s==='dhcp'") {
		t.Errorf("label()'s uppercase list is missing 'dhcp' — the page and rail would read %q:\n%s", "Dhcp", upper)
	}
	// And no override crept into sectionHeading to paper over it there while
	// leaving the rail and the search index still saying Dhcp.
	head := indexHTML[strings.Index(indexHTML, "function sectionHeading(s){"):]
	head = head[:strings.Index(head, "\n}")]
	if strings.Contains(head, "dhcp") {
		t.Errorf("sectionHeading special-cases dhcp; label() already uppercases it, and an override there fixes only the <h2>:\n%s", head)
	}
}

// Kea's grammar accepts exactly one key at the top level. v944 wrote the
// ownership marker beside Dhcp4, which is not ignored but rejected:
//
//	kea-dhcp4.conf:2.3-22: syntax error, unexpected constant string, expecting Dhcp4
//
// so every config gravinet rendered produced a server that would not start.
// The marker belongs in Dhcp4's user-context, which Kea defines as data it
// carries and does not interpret.
func TestRenderKeaTopLevelIsOnlyDhcp4(t *testing.T) {
	m := renderKeaMap(t, config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{dhcpSubnet()}})
	for k := range m {
		if k != "Dhcp4" {
			t.Errorf("rendered a top-level key %q; Kea rejects the file outright, expecting Dhcp4", k)
		}
	}
	d, _ := m["Dhcp4"].(map[string]any)
	uc, _ := d["user-context"].(map[string]any)
	if marked, _ := uc["gravinet-generated"].(bool); !marked {
		t.Errorf("no ownership marker in Dhcp4.user-context: %v", d["user-context"])
	}
}

// And the file gravinet writes has to be one it recognises as its own on the
// next apply, or it sets its own config aside as if it were the operator's.
func TestKeaOwnedRoundTripsWhatRenderKeaWrites(t *testing.T) {
	b, err := renderKea(config.DHCPConfig{Mode: config.DHCPServer, Subnets: []config.DHCPSubnet{dhcpSubnet()}})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	p := filepath.Join(t.TempDir(), "kea-dhcp4.conf")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if !keaOwned(p) {
		t.Error("keaOwned does not recognise the file renderKea just wrote")
	}
	// The v944 marker is still honoured, so an upgrade does not set aside a
	// file gravinet wrote itself.
	legacy := filepath.Join(t.TempDir(), "kea-dhcp4.conf")
	if err := os.WriteFile(legacy, []byte(`{"gravinet-generated":true,"Dhcp4":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !keaOwned(legacy) {
		t.Error("a v944-era config is no longer recognised as gravinet's")
	}
	// Somebody else's config is still left alone.
	theirs := filepath.Join(t.TempDir(), "kea-dhcp4.conf")
	if err := os.WriteFile(theirs, []byte(`{"Dhcp4":{"subnet4":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if keaOwned(theirs) {
		t.Error("claimed a config gravinet did not write")
	}
}
