package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// TestManagedPeersSortedByHostname verifies ManagedPeers orders its result by
// hostname (case-insensitively), not by the map-iteration order of its
// internal `best` accumulator — which Go deliberately randomizes on every
// call, and which is what previously made the header's node picker and the
// speedtest source/target pickers reshuffle on their own between polls even
// when the managed-peer set hadn't changed. A peer with no known hostname
// yet falls back to sorting by its node id.
func TestManagedPeersSortedByHostname(t *testing.T) {
	e := NewEngine(Options{
		NodeID: "self",
		Nets:   []NetSpec{{ID: 1, Name: "n1", Dev: newFakeDev("a")}},
	})
	now := time.Now()
	snap := e.netSnapshot()
	// Insert deliberately out of alphabetical order, with mixed case and one
	// peer that has no hostname yet, to make sure the sort isn't accidentally
	// passing just because insertion order happened to match.
	entries := []*nodeInfo{
		{nodeID: "id-zebra", hostname: "zebra", managed: true, overlay4: netip.MustParseAddr("10.1.0.1"), webPort: 8443, lastSeen: now},
		{nodeID: "id-apple", hostname: "Apple", managed: true, overlay4: netip.MustParseAddr("10.1.0.2"), webPort: 8443, lastSeen: now}, // mixed case
		{nodeID: "id-mango", hostname: "mango", managed: true, overlay4: netip.MustParseAddr("10.1.0.3"), webPort: 8443, lastSeen: now},
		{nodeID: "id-noname", hostname: "", managed: true, overlay4: netip.MustParseAddr("10.1.0.4"), webPort: 8443, lastSeen: now}, // no hostname yet
	}
	for _, ni := range entries {
		snap[1].nodes[ni.nodeID] = ni
	}

	want := []string{"Apple", "id-noname", "mango", "zebra"} // unified case-insensitive sort of (hostname, or id when absent)
	for i := 0; i < 5; i++ {                                 // repeat: catches a fix that only "happens" to sort once
		got := e.ManagedPeers(time.Minute)
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d peers, want %d", i, len(got), len(want))
		}
		for j, p := range got {
			label := p.Hostname
			if label == "" {
				label = p.NodeID
			}
			if label != want[j] {
				t.Fatalf("run %d: position %d = %q, want %q (full order: %v)", i, j, label, want[j], managedNamesOf(got))
			}
		}
	}
}

func managedNamesOf(ps []ManagedPeer) []string {
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
