package mesh

import (
	"net/netip"
	"testing"
)

func rejectNS(t *testing.T, rules ...RejectRule) *netState {
	t.Helper()
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), RouteReject: rules,
	}}})
	return e.netSnapshot()[1]
}

func exactRej(cidr string) RejectRule {
	return RejectRule{Prefix: netip.MustParsePrefix(cidr)}
}
func inclRej(cidr string) RejectRule {
	return RejectRule{Prefix: netip.MustParsePrefix(cidr), Inclusive: true}
}

// A plain reject matches ONLY the exact advertised prefix — not the more-specific
// networks it contains.
func TestRejectExactByDefault(t *testing.T) {
	ns := rejectNS(t, exactRej("10.129.16.0/21"))
	p := netip.MustParsePrefix
	if !ns.rejected(p("10.129.16.0/21")) {
		t.Fatal("exact reject should block the identical prefix")
	}
	for _, ok := range []string{"10.129.17.0/24", "10.129.16.0/24", "10.129.23.0/24", "10.0.0.0/8", "0.0.0.0/0"} {
		if ns.rejected(p(ok)) {
			t.Fatalf("exact reject of 10.129.16.0/21 wrongly blocked %s", ok)
		}
	}
}

// An inclusive reject also blocks every more-specific network inside the prefix,
// but nothing outside it.
func TestRejectInclusiveBlocksContained(t *testing.T) {
	ns := rejectNS(t, inclRej("10.129.16.0/21"))
	p := netip.MustParsePrefix
	for _, blocked := range []string{"10.129.16.0/21", "10.129.17.0/24", "10.129.16.128/25", "10.129.23.0/24"} {
		if !ns.rejected(p(blocked)) {
			t.Fatalf("inclusive reject of 10.129.16.0/21 should block contained %s", blocked)
		}
	}
	for _, ok := range []string{"10.129.24.0/24", "10.130.0.0/16", "192.168.0.0/16", "0.0.0.0/0"} {
		if ns.rejected(p(ok)) {
			t.Fatalf("inclusive reject of 10.129.16.0/21 wrongly blocked outside prefix %s", ok)
		}
	}
}

// The default-route reject (the shipped default, non-inclusive 0.0.0.0/0) blocks
// only a learned default route, never the routes it nominally contains.
func TestRejectDefaultRouteOnlyBlocksDefault(t *testing.T) {
	ns := rejectNS(t, exactRej("0.0.0.0/0"))
	p := netip.MustParsePrefix
	if !ns.rejected(p("0.0.0.0/0")) {
		t.Fatal("0.0.0.0/0 reject should block a learned default route")
	}
	for _, ok := range []string{"10.0.0.0/8", "192.168.1.0/24", "172.16.5.0/24"} {
		if ns.rejected(p(ok)) {
			t.Fatalf("0.0.0.0/0 reject wrongly blocked %s (would suppress all routing)", ok)
		}
	}
}

// An unmasked advertised prefix still matches an exact reject of its network.
func TestRejectExactMatchesUnmasked(t *testing.T) {
	ns := rejectNS(t, exactRej("10.129.16.0/21"))
	if !ns.rejected(netip.MustParsePrefix("10.129.20.5/21")) {
		t.Fatal("exact reject should match the same network regardless of host bits")
	}
}
