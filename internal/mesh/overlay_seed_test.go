package mesh

import (
	"net/netip"
	"testing"
)

func twoNetEngine() *Engine {
	return NewEngine(Options{NodeID: "self", Nets: []NetSpec{
		{ID: 1, Name: "net1", Dev: newFakeDev("d1"), Subnet4: netip.MustParsePrefix("10.1.0.0/16")},
		{ID: 2, Name: "net2", Dev: newFakeDev("d2"), Subnet4: netip.MustParsePrefix("10.2.0.0/16")},
	}})
}

// A gossip-learned endpoint that is actually another network's overlay (mesh)
// address must not be dialed as an underlay seed.
func TestAddSeedIgnoresOverlayAddr(t *testing.T) {
	e := twoNetEngine()
	ns2 := e.netSnapshot()[2]
	overlay := netip.MustParseAddrPort("10.1.0.5:51820") // net1's mesh0 address
	underlay := netip.MustParseAddrPort("203.0.113.5:51820")
	e.AddSeed(2, overlay)
	e.AddSeed(2, underlay)
	ns2.mu.RLock()
	got := append([]netip.AddrPort(nil), ns2.seeds...)
	ns2.mu.RUnlock()
	for _, s := range got {
		if s == overlay {
			t.Fatalf("overlay address %s was added as a seed on net2", overlay)
		}
	}
	found := false
	for _, s := range got {
		if s == underlay {
			found = true
		}
	}
	if !found {
		t.Fatalf("underlay seed %s was not added; seeds=%v", underlay, got)
	}
}

// peer_cache (fed by PeerEndpoints) must never contain an overlay address, even
// if a session somehow holds one — this is the network1-mesh0-in-network2-cache
// leak.
func TestPeerEndpointsExcludesOverlay(t *testing.T) {
	e := twoNetEngine()
	ns2 := e.netSnapshot()[2]
	ns2.mu.Lock()
	ns2.byNode["bleed"] = &peerSession{net: ns2, nodeID: "bleed", endpoint: netip.MustParseAddrPort("10.1.0.5:51820")}
	ns2.byNode["real"] = &peerSession{net: ns2, nodeID: "real", endpoint: netip.MustParseAddrPort("203.0.113.9:51820")}
	ns2.mu.Unlock()

	eps := e.PeerEndpoints(2)
	if len(eps) != 1 || eps[0] != netip.MustParseAddrPort("203.0.113.9:51820") {
		t.Fatalf("PeerEndpoints(net2) = %v, want only the underlay 203.0.113.9:51820", eps)
	}
}
