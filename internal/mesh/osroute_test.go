package mesh

import (
	"net/netip"
	"testing"
)

// The reported failure, as a test: the kernel says "10.3.3.0/24 via
// 10.255.255.248 dev mesh0", 10.255.255.248 is a peer's overlay address, and
// the data plane must therefore forward to that peer — with nothing having
// been advertised into the mesh at all.
func TestOSRouteResolvesThroughPeerGateway(t *testing.T) {
	grav3 := &peerSession{nodeID: "grav3"}
	s := &fwdSnap{
		routes4: map[netip.Addr]*peerSession{
			netip.MustParseAddr("10.255.255.248"): grav3,
		},
		osRoutes: []OSRoute{{
			Prefix:  netip.MustParsePrefix("10.3.3.0/24"),
			Gateway: netip.MustParseAddr("10.255.255.248"),
		}},
	}
	if got := s.osRouteFlow(netip.MustParseAddr("10.3.3.1")); got != grav3 {
		t.Fatalf("10.3.3.1 should resolve to grav3, got %v", got)
	}
	// A destination no route covers stays unresolved.
	if got := s.osRouteFlow(netip.MustParseAddr("10.9.9.1")); got != nil {
		t.Errorf("uncovered destination should not resolve, got %v", got)
	}
}

// A gateway that isn't a peer resolves to nothing. A route out the mesh device
// via some other next-hop is not ours to carry, and guessing would send the
// packet to an unrelated peer.
func TestOSRouteIgnoresNonPeerGateway(t *testing.T) {
	s := &fwdSnap{
		routes4: map[netip.Addr]*peerSession{},
		osRoutes: []OSRoute{{
			Prefix:  netip.MustParsePrefix("10.3.3.0/24"),
			Gateway: netip.MustParseAddr("10.255.255.248"),
		}},
	}
	if got := s.osRouteFlow(netip.MustParseAddr("10.3.3.1")); got != nil {
		t.Fatalf("a gateway that is not a peer must not resolve, got %v", got)
	}
}

// Longest prefix wins, the same rule the kernel applied when it chose the
// route in the first place.
func TestOSRouteLongestPrefixWins(t *testing.T) {
	a := &peerSession{nodeID: "a"}
	b := &peerSession{nodeID: "b"}
	s := &fwdSnap{
		routes4: map[netip.Addr]*peerSession{
			netip.MustParseAddr("10.255.255.1"): a,
			netip.MustParseAddr("10.255.255.2"): b,
		},
		osRoutes: []OSRoute{
			{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Gateway: netip.MustParseAddr("10.255.255.1")},
			{Prefix: netip.MustParsePrefix("10.3.3.0/24"), Gateway: netip.MustParseAddr("10.255.255.2")},
		},
	}
	if got := s.osRouteFlow(netip.MustParseAddr("10.3.3.1")); got != b {
		t.Errorf("the /24 should win over the /8, got %v", got)
	}
	if got := s.osRouteFlow(netip.MustParseAddr("10.7.7.1")); got != a {
		t.Errorf("an address only the /8 covers should use it, got %v", got)
	}
}

// IPv6 resolves through the v6 overlay map, not the v4 one.
func TestOSRouteIPv6(t *testing.T) {
	p := &peerSession{nodeID: "p"}
	gw := netip.MustParseAddr("fdff:255::b5d3:5e4f:b4bd:bcc4")
	s := &fwdSnap{
		routes6:  map[netip.Addr]*peerSession{gw: p},
		osRoutes: []OSRoute{{Prefix: netip.MustParsePrefix("fd0a:3::/64"), Gateway: gw}},
	}
	if got := s.osRouteFlow(netip.MustParseAddr("fd0a:3::1")); got != p {
		t.Fatalf("v6 destination should resolve through the v6 map, got %v", got)
	}
}

func TestSameOSRoutes(t *testing.T) {
	r := []OSRoute{{Prefix: netip.MustParsePrefix("10.3.3.0/24"), Gateway: netip.MustParseAddr("10.255.255.248")}}
	if !sameOSRoutes(r, append([]OSRoute(nil), r...)) {
		t.Error("identical sets should compare equal")
	}
	if sameOSRoutes(r, nil) {
		t.Error("different lengths should not compare equal")
	}
	other := []OSRoute{{Prefix: r[0].Prefix, Gateway: netip.MustParseAddr("10.255.255.99")}}
	if sameOSRoutes(r, other) {
		t.Error("a changed gateway must be detected")
	}
}
