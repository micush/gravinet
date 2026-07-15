package mesh

import (
	"net/netip"
	"testing"
)

// TestListPeersReportsRelayed verifies the NAT/reachability signal: a session
// reached through a relay is flagged Relayed (a peer behind a restrictive NAT),
// while a directly-connected peer is not.
func TestListPeersReportsRelayed(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	hop := &peerSession{net: ns, nodeID: "hop", endpoint: netip.MustParseAddrPort("203.0.113.9:51820")}
	direct := &peerSession{net: ns, nodeID: "direct", endpoint: netip.MustParseAddrPort("203.0.113.1:51820")}
	relayed := &peerSession{net: ns, nodeID: "relayed", endpoint: netip.MustParseAddrPort("203.0.113.2:51820"), relay: hop}
	ns.mu.Lock()
	ns.byNode["direct"] = direct
	ns.byNode["relayed"] = relayed
	ns.mu.Unlock()

	got := map[string]PeerInfo{}
	for _, p := range e.ListPeers(1) {
		got[p.NodeID] = p
	}
	if got["direct"].Relayed {
		t.Error("direct peer wrongly flagged relayed")
	}
	if !got["relayed"].Relayed {
		t.Error("relayed peer not flagged relayed")
	}
	// The observed endpoint (public mapping) is still reported for both.
	if got["direct"].Endpoint != "203.0.113.1:51820" {
		t.Errorf("direct endpoint = %q", got["direct"].Endpoint)
	}
}
