package mesh

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// ecmpNetState builds a bare engine + netState (no real device traffic) and
// stores a forwarding snapshot containing the given redistributed entries and
// a live session for every origin named in liveOrigins. It returns the
// netState ready for bestRedistOrigins / redistRouteFlow calls.
func ecmpNetState(t *testing.T, redist []routeEntry, liveOrigins ...string) (*Engine, *netState) {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{mustKey()})
	dev := newFakeDev("self")
	const netID = uint64(0xEC117)
	eng := NewEngine(Options{
		NodeID: "self", Hostname: "self",
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: netip.MustParseAddr("10.1.0.1")}},
	})
	eng.Attach(nopSender{})
	ns := eng.network(netID)

	byNode := map[string]*peerSession{}
	for _, id := range liveOrigins {
		byNode[id] = &peerSession{nodeID: id, net: ns}
	}
	ns.fwd.Store(&fwdSnap{
		routes4: map[netip.Addr]*peerSession{},
		routes6: map[netip.Addr]*peerSession{},
		byNode:  byNode,
		redist:  redist,
	})
	return eng, ns
}

// TestBestRedistOriginsCollectsEqualCostTies is the crux: two origins
// advertising the same prefix at the same metric must both be returned, in
// advertisement order, so the flow hash has more than one exit to choose from.
func TestBestRedistOriginsCollectsEqualCostTies(t *testing.T) {
	pfx := netip.MustParsePrefix("192.168.5.0/24")
	now := time.Now()
	_, ns := ecmpNetState(t,
		[]routeEntry{
			{origin: "exitA", prefix: pfx, metric: 200, lastSeen: now},
			{origin: "exitB", prefix: pfx, metric: 200, lastSeen: now},
		},
		"exitA", "exitB",
	)
	origins, snap := ns.bestRedistOrigins(netip.MustParseAddr("192.168.5.42"))
	if snap == nil || len(origins) != 2 {
		t.Fatalf("expected 2 tied origins, got %v", origins)
	}
	if origins[0] != "exitA" || origins[1] != "exitB" {
		t.Fatalf("tied origins not in advertisement order: %v", origins)
	}
}

// TestBestRedistOriginsMetricStillWins guards that ECMP did not turn into
// "always spread": a strictly better metric is a sole winner, no sharing.
func TestBestRedistOriginsMetricStillWins(t *testing.T) {
	pfx := netip.MustParsePrefix("192.168.5.0/24")
	now := time.Now()
	_, ns := ecmpNetState(t,
		[]routeEntry{
			{origin: "far", prefix: pfx, metric: 300, lastSeen: now},
			{origin: "near", prefix: pfx, metric: 100, lastSeen: now},
			{origin: "mid", prefix: pfx, metric: 200, lastSeen: now},
		},
		"far", "near", "mid",
	)
	origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("192.168.5.42"))
	if len(origins) != 1 || origins[0] != "near" {
		t.Fatalf("lowest metric should win alone, got %v", origins)
	}
}

// TestBestRedistOriginsLongestPrefixWins guards specificity precedence over
// metric: a more specific prefix wins even at a worse metric, and does not get
// pooled with less specific ties.
func TestBestRedistOriginsLongestPrefixWins(t *testing.T) {
	now := time.Now()
	_, ns := ecmpNetState(t,
		[]routeEntry{
			{origin: "broad", prefix: netip.MustParsePrefix("192.168.0.0/16"), metric: 10, lastSeen: now},
			{origin: "specific", prefix: netip.MustParsePrefix("192.168.5.0/24"), metric: 999, lastSeen: now},
		},
		"broad", "specific",
	)
	origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("192.168.5.42"))
	if len(origins) != 1 || origins[0] != "specific" {
		t.Fatalf("longest-prefix match should win alone, got %v", origins)
	}
}

// TestBestRedistOriginsSkipsDeadOrigins ensures an advertised route whose
// origin has no live session is not a candidate — it can neither win nor be a
// tie member, matching the old single-winner code's byNode nil check.
func TestBestRedistOriginsSkipsDeadOrigins(t *testing.T) {
	pfx := netip.MustParsePrefix("192.168.5.0/24")
	now := time.Now()
	_, ns := ecmpNetState(t,
		[]routeEntry{
			{origin: "dead", prefix: pfx, metric: 100, lastSeen: now}, // no session
			{origin: "live", prefix: pfx, metric: 200, lastSeen: now},
		},
		"live", // only "live" has a session
	)
	origins, _ := ns.bestRedistOrigins(netip.MustParseAddr("192.168.5.42"))
	if len(origins) != 1 || origins[0] != "live" {
		t.Fatalf("dead origin (better metric, no session) must be skipped; got %v", origins)
	}
}

// TestRedistRouteFlowSplitsAcrossSiblings drives the full flow selector: many
// distinct flows to the same tied prefix must resolve to both exits, and each
// individual flow must resolve to a single consistent exit.
func TestRedistRouteFlowSplitsAcrossSiblings(t *testing.T) {
	pfx := netip.MustParsePrefix("192.168.5.0/24")
	now := time.Now()
	eng, ns := ecmpNetState(t,
		[]routeEntry{
			{origin: "exitA", prefix: pfx, metric: 200, lastSeen: now},
			{origin: "exitB", prefix: pfx, metric: 200, lastSeen: now},
		},
		"exitA", "exitB",
	)
	dst := netip.MustParseAddr("192.168.5.42")
	hits := map[string]int{}
	for sport := 30000; sport < 30400; sport++ {
		pkt := buildV4Flow(netip.MustParseAddr("10.0.0.2"), dst, 6, uint16(sport), 443)
		ps := eng.redistRouteFlow(ns, dst, pkt)
		if ps == nil {
			t.Fatalf("no exit for flow sport=%d", sport)
		}
		hits[ps.nodeID]++
		// Re-resolving the same flow must give the same exit.
		if again := eng.redistRouteFlow(ns, dst, pkt); again.nodeID != ps.nodeID {
			t.Fatalf("flow sport=%d flapped exits: %s then %s", sport, ps.nodeID, again.nodeID)
		}
	}
	if len(hits) != 2 {
		t.Fatalf("expected traffic across both exits, got %v", hits)
	}
	t.Logf("400 flows split across exits: %v", hits)
}

// buildV4Flow makes a minimal IPv4 TCP/UDP packet with the given 5-tuple, for
// exercising flowIndex and the ECMP selector without a real device.
func buildV4Flow(src, dst netip.Addr, proto uint8, sport, dport uint16) []byte {
	p := make([]byte, 40)
	p[0] = 0x45 // v4, IHL 5
	p[9] = proto
	copy(p[12:16], src.AsSlice())
	copy(p[16:20], dst.AsSlice())
	binary.BigEndian.PutUint16(p[20:22], sport)
	binary.BigEndian.PutUint16(p[22:24], dport)
	return p
}

// TestFlowIndexStableAndBounded is the core safety property: the same flow
// always maps to the same bucket (no intra-connection reordering across
// exits), and the result is always in range.
func TestFlowIndexStableAndBounded(t *testing.T) {
	src := netip.MustParseAddr("10.0.0.9")
	dst := netip.MustParseAddr("192.168.5.7")
	pkt := buildV4Flow(src, dst, 6, 44321, 443)

	for n := 1; n <= 8; n++ {
		first := flowIndex(pkt, n)
		if first < 0 || first >= n {
			t.Fatalf("flowIndex out of range: got %d for n=%d", first, n)
		}
		for i := 0; i < 1000; i++ {
			if got := flowIndex(pkt, n); got != first {
				t.Fatalf("flowIndex not deterministic for n=%d: %d then %d", n, first, got)
			}
		}
	}
}

// TestFlowIndexDistinguishesFlows checks that different connections between
// the same host pair can land on different buckets — otherwise "load sharing"
// wouldn't share anything for a busy pair of hosts. We don't require any
// particular split, only that not every flow collapses to one bucket across a
// realistic spread of source ports.
func TestFlowIndexDistinguishesFlows(t *testing.T) {
	src := netip.MustParseAddr("10.0.0.9")
	dst := netip.MustParseAddr("192.168.5.7")
	const n = 2
	seen := map[int]int{}
	for sport := 20000; sport < 20500; sport++ {
		pkt := buildV4Flow(src, dst, 6, uint16(sport), 443)
		seen[flowIndex(pkt, n)]++
	}
	if len(seen) < 2 {
		t.Fatalf("all 500 flows hashed to one bucket; distribution=%v", seen)
	}
	// Sanity: neither side should be wildly starved (a hash this broken would
	// be a red flag). Allow generous slack; this is not a statistical test.
	for b, c := range seen {
		if c == 0 || c == 500 {
			t.Fatalf("bucket %d got %d/500 — degenerate split %v", b, c, seen)
		}
	}
	t.Logf("500 flows over 2 exits split %v", seen)
}

// TestFlowIndexNoPortsStillStable covers non-TCP/UDP (e.g. ICMP): no ports to
// mix, but address+proto must still yield a stable, in-range bucket.
func TestFlowIndexNoPortsStillStable(t *testing.T) {
	src := netip.MustParseAddr("10.0.0.9")
	dst := netip.MustParseAddr("192.168.5.7")
	pkt := buildV4Flow(src, dst, 1, 0, 0) // proto 1 = ICMP; parseL4 reads no ports
	first := flowIndex(pkt, 3)
	for i := 0; i < 100; i++ {
		if got := flowIndex(pkt, 3); got != first {
			t.Fatalf("ICMP flow not stable: %d then %d", first, got)
		}
	}
	if first < 0 || first >= 3 {
		t.Fatalf("ICMP bucket out of range: %d", first)
	}
}
