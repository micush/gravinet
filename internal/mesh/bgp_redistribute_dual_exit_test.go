package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestSelfBGPRedistributionShadowsSiblingGossip covers a redundant-exit-node
// setup: two nodes each learn the same external prefix over their own BGP
// session and redistribute it into mesh gossip at different metrics, so
// every *other* mesh node can pick the better of the two. The node doing the
// redistributing is a different story — see bestRedistMetric's own doc
// comment for the full mechanism this guards against: without it, each exit
// node would treat its sibling's gossiped copy as just another route,
// install it as a plain kernel route, and since FRR treats any pre-existing
// kernel route as distance 0 (always ahead of a BGP-learned one, regardless
// of BGP's own distance), silently loop its own transit traffic for the
// prefix back out over the mesh instead of out its own working BGP session.
func TestSelfBGPRedistributionShadowsSiblingGossip(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xC0511)
	prefix := netip.MustParsePrefix("192.168.5.0/24")

	cush1 := spinNode(t, "gn-cush1", netID, key, netip.MustParseAddr("10.60.0.1"))
	cush2 := spinNode(t, "gn-cush2", netID, key, netip.MustParseAddr("10.60.0.2"))
	defer func() {
		for _, n := range []*testNode{cush1, cush2} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	cush1.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(cush2.tr.Port())))
	cush2.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(cush1.tr.Port())))
	if !waitUntil(15*time.Second, func() bool {
		return cush1.eng.PeerCount(netID) == 1 && cush2.eng.PeerCount(netID) == 1
	}) {
		t.Fatal("gn-cush1/gn-cush2 did not connect")
	}

	// gn-cush2 redistributes its BGP-learned copy first, at metric 250 —
	// standing in for "gn-cush2 already had its BGP session up". With
	// gn-cush1 not yet redistributing the same prefix itself, it has no
	// better path of its own, so it correctly installs gn-cush2's gossiped
	// copy — this is the ordinary, correct fallback behavior for any node
	// that isn't itself an exit for the prefix, and must keep working.
	if ok := cush2.eng.SetBGPRoutes(netID, []netip.Prefix{prefix}, 250); !ok {
		t.Fatal("SetBGPRoutes on gn-cush2 reported networkID not found")
	}
	if !waitUntil(10*time.Second, func() bool {
		return cush1.dev.hasRoute(prefix) && cush1.dev.metricOf(prefix) == 250+MeshRouteMetricFloor
	}) {
		t.Fatalf("gn-cush1 should have installed gn-cush2's gossiped route at metric %d; hasRoute=%v metric=%d",
			250+MeshRouteMetricFloor, cush1.dev.hasRoute(prefix), cush1.dev.metricOf(prefix))
	}

	// gn-cush1 now also redistributes the same prefix from its own BGP RIB
	// (metric 200 — the "better" of the two, though the metric shouldn't
	// even matter here). It must drop the mesh-sourced route it borrowed
	// from gn-cush2 and defer entirely to its own BGP session instead of
	// shadowing it.
	if ok := cush1.eng.SetBGPRoutes(netID, []netip.Prefix{prefix}, 200); !ok {
		t.Fatal("SetBGPRoutes on gn-cush1 reported networkID not found")
	}
	if !waitUntil(10*time.Second, func() bool { return !cush1.dev.hasRoute(prefix) }) {
		t.Fatalf("gn-cush1 should have dropped gn-cush2's gossiped route once it redistributed the prefix itself; metric=%d",
			cush1.dev.metricOf(prefix))
	}
	// gn-cush2's advertisement must still be tracked (it's still gossiped to
	// the rest of the mesh, and gn-cush1 still needs it as a fallback) — only
	// the OS install decision changes, not the redistributed-route bookkeeping.
	if !routeKnown(cush1, netID, prefix.String()) {
		t.Fatal("gn-cush1 should still know about gn-cush2's advertisement even though it no longer installs it")
	}

	// gn-cush1 stops redistributing the prefix itself (BGP session dropped,
	// or the operator unchecked it) — gn-cush2's gossiped copy must come
	// back immediately as the fallback, not wait on some unrelated event.
	if ok := cush1.eng.SetBGPRoutes(netID, nil, 0); !ok {
		t.Fatal("SetBGPRoutes withdrawal on gn-cush1 reported networkID not found")
	}
	if !waitUntil(10*time.Second, func() bool {
		return cush1.dev.hasRoute(prefix) && cush1.dev.metricOf(prefix) == 250+MeshRouteMetricFloor
	}) {
		t.Fatalf("gn-cush1 should have fallen back to gn-cush2's gossiped route at metric %d; hasRoute=%v metric=%d",
			250+MeshRouteMetricFloor, cush1.dev.hasRoute(prefix), cush1.dev.metricOf(prefix))
	}
}
