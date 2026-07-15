package resolver

import (
	"net/netip"
	"reflect"
	"testing"
)

func addrsFor(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	out := make([]netip.Addr, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParseAddr(s)
	}
	return out
}

func TestLinuxRoutingArgsUnionsServersAcrossDomains(t *testing.T) {
	entries := []Entry{
		{Domain: "domain2", Servers: addrsFor(t, "3.3.3.3", "4.4.4.4")},
		{Domain: "domain1", Servers: addrsFor(t, "1.1.1.1", "2.2.2.2")},
	}
	domains, servers := linuxRoutingArgs(entries)

	wantDomains := []string{"~domain1", "~domain2"}
	if !reflect.DeepEqual(domains, wantDomains) {
		t.Fatalf("domains = %v, want %v", domains, wantDomains)
	}
	wantServers := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"}
	if !reflect.DeepEqual(servers, wantServers) {
		t.Fatalf("servers = %v, want %v (Linux takes one shared server set per link, so this is a deliberate union, not a bug)", servers, wantServers)
	}
}

func TestNormalizeDomain(t *testing.T) {
	tests := []struct{ in, want string }{
		{"example.com", "example.com"},
		{"Example.com", "example.com"},
		{"EXAMPLE.COM", "example.com"},
		{"example.com.", "example.com"}, // trailing root dot stripped
		{"  Example.Com  ", "example.com"},
		{"", ""},
		{".", ""},
	}
	for _, tc := range tests {
		if got := normalizeDomain(tc.in); got != tc.want {
			t.Errorf("normalizeDomain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLinuxRoutingArgsDedupesAndSkipsEmptyDomain(t *testing.T) {
	entries := []Entry{
		{Domain: "domain1.", Servers: addrsFor(t, "1.1.1.1")}, // trailing dot stripped
		{Domain: "domain1", Servers: addrsFor(t, "1.1.1.1")},  // exact duplicate collapses
		{Domain: "Domain1", Servers: addrsFor(t, "1.1.1.1")},  // case-variant duplicate also collapses
		{Domain: "  ", Servers: addrsFor(t, "9.9.9.9")},       // blank domain skipped entirely
	}
	domains, servers := linuxRoutingArgs(entries)

	if !reflect.DeepEqual(domains, []string{"~domain1"}) {
		t.Fatalf("domains = %v, want [~domain1] (dedup + trailing-dot + case normalization)", domains)
	}
	if !reflect.DeepEqual(servers, []string{"1.1.1.1"}) {
		t.Fatalf("servers = %v, want [1.1.1.1] (blank-domain entry's server must not leak in)", servers)
	}
}

func TestLinuxRoutingArgsEmpty(t *testing.T) {
	domains, servers := linuxRoutingArgs(nil)
	if len(domains) != 0 || len(servers) != 0 {
		t.Fatalf("expected empty results for no entries, got domains=%v servers=%v", domains, servers)
	}
}

func TestSearchDomainArgsDedupesNormalizesAndSorts(t *testing.T) {
	got := searchDomainArgs([]string{"Mesh.Internal", "corp.internal.", "mesh.internal", "  ", "corp.internal"})
	want := []string{"corp.internal", "mesh.internal"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("searchDomainArgs = %v, want %v (dedup + trailing-dot + case normalization, sorted)", got, want)
	}
}

func TestSearchDomainArgsCarriesNoTildePrefix(t *testing.T) {
	// The whole point of a search domain (as opposed to a routing domain) is
	// that it's bare — no "~" — so systemd-resolved treats it as a search,
	// not routing, entry.
	got := searchDomainArgs([]string{"corp.internal"})
	if len(got) != 1 || got[0] != "corp.internal" {
		t.Fatalf("searchDomainArgs = %v, want [corp.internal] with no ~ prefix", got)
	}
}

func TestSearchDomainArgsEmpty(t *testing.T) {
	if got := searchDomainArgs(nil); len(got) != 0 {
		t.Fatalf("expected empty result for no domains, got %v", got)
	}
}
