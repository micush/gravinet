package mesh

import (
	"net/netip"
	"testing"
)

// TestListPeersSortedByHostname verifies ListPeers orders its result by
// hostname (case-insensitively), not by the insertion/map-iteration order of
// the underlying ns.byNode map — which Go deliberately randomizes on every
// call, and which is what previously made the peers list in the admin UI and
// `gravinet list` look like it reshuffled on its own between polls even when
// the peer set hadn't changed. A peer with no known hostname yet falls back
// to sorting by its node id.
func TestListPeersSortedByHostname(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID: 1, Name: "n", Dev: newFakeDev("d"), Subnet4: netip.MustParsePrefix("10.0.0.0/24"),
	}}})
	ns := e.netSnapshot()[1]
	// Insert deliberately out of alphabetical order, with mixed case and one
	// peer that has no hostname yet, to make sure the sort isn't accidentally
	// passing just because insertion order happened to match.
	peers := []*peerSession{
		{net: ns, nodeID: "id-zebra", hostname: "zebra", endpoint: netip.MustParseAddrPort("203.0.113.1:1")},
		{net: ns, nodeID: "id-apple", hostname: "Apple", endpoint: netip.MustParseAddrPort("203.0.113.2:1")}, // mixed case
		{net: ns, nodeID: "id-mango", hostname: "mango", endpoint: netip.MustParseAddrPort("203.0.113.3:1")},
		{net: ns, nodeID: "id-noname", hostname: "", endpoint: netip.MustParseAddrPort("203.0.113.4:1")}, // no hostname yet
	}
	ns.mu.Lock()
	for _, p := range peers {
		ns.byNode[p.nodeID] = p
	}
	ns.mu.Unlock()

	want := []string{"Apple", "id-noname", "mango", "zebra"} // unified case-insensitive sort of (hostname, or id when absent)
	for i := 0; i < 5; i++ {                                 // repeat: catches a fix that only "happens" to sort once
		got := e.ListPeers(1)
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d peers, want %d", i, len(got), len(want))
		}
		for j, p := range got {
			label := p.Hostname
			if label == "" {
				label = p.NodeID
			}
			if label != want[j] {
				t.Fatalf("run %d: position %d = %q, want %q (full order: %v)", i, j, label, want[j], namesOf(got))
			}
		}
	}
}

func namesOf(ps []PeerInfo) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		if p.Hostname != "" {
			out[i] = p.Hostname
		} else {
			out[i] = p.NodeID
		}
	}
	return out
}
