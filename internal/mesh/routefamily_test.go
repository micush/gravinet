package mesh

import (
	"net/netip"
	"testing"
	"time"
)

// deadFamilyPS builds a peerSession whose family-liveness state reports
// dead for whichever family probeSentAgo/goodAgo describe — used below to
// simulate "this origin's session is healthy, but the overlay path for one
// specific family has stopped answering pings" without waiting out
// familyDeadAfter (45s) in real time.
func deadFamilyPS(net *netState, nodeID string, overlay4, overlay6 netip.Addr, now time.Time) *peerSession {
	ps := &peerSession{net: net, nodeID: nodeID, overlay4: overlay4, overlay6: overlay6}
	// Probing started well over familyDeadAfter ago, and no reply has ever
	// been marked good (familyGood stays at its zero value) — per
	// familyLive's own logic, that's what "dead" looks like: not "never
	// checked" (probeSent == 0, which is deliberately optimistic).
	longAgo := now.Add(-2 * familyDeadAfter).UnixNano()
	ps.familyProbeSent4.Store(longAgo)
	ps.familyProbeSent6.Store(longAgo)
	return ps
}

// liveFamilyPS builds a peerSession reporting both families live: a recent
// probe with a recent good reply.
func liveFamilyPS(net *netState, nodeID string, overlay4, overlay6 netip.Addr, now time.Time) *peerSession {
	ps := &peerSession{net: net, nodeID: nodeID, overlay4: overlay4, overlay6: overlay6}
	recent := now.Add(-time.Second).UnixNano()
	ps.familyProbeSent4.Store(recent)
	ps.familyProbeSent6.Store(recent)
	ps.familyGood4.Store(recent)
	ps.familyGood6.Store(recent)
	return ps
}

// TestOnRouteAddRejectsDeadFamily proves a route can't be accepted for an
// address family that's already known undeliverable to its origin, even
// though the origin has a perfectly live session (the byNode check alone
// would let it through) — the acceptance-side counterpart to
// sweepDeadFamilyRoutes below, so a route is never let in only to be swept
// back out moments later.
func TestOnRouteAddRejectsDeadFamily(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]
	now := time.Now()

	origin4 := netip.MustParseAddr("10.9.9.1")
	origin6 := netip.MustParseAddr("fd00:9::1")
	ps := deadFamilyPS(ns, "origin", origin4, origin6, now)
	ns.mu.Lock()
	ns.byNode["origin"] = ps
	ns.mu.Unlock()

	v4 := netip.MustParsePrefix("10.40.0.0/24")
	v6 := netip.MustParsePrefix("fd40::/64")
	e.onRouteAdd(ps, encodeRouteAdd("origin", v4, 5)[1:])
	e.onRouteAdd(ps, encodeRouteAdd("origin", v6, 5)[1:])

	ns.mu.RLock()
	n := len(ns.redist)
	ns.mu.RUnlock()
	if n != 0 {
		t.Fatalf("redist = %d entries, want 0 (both families were dead at advertisement time)", n)
	}
	if dev.hasRoute(v4) || dev.hasRoute(v6) {
		t.Fatal("a route for a dead family was programmed into the OS table")
	}
}

// TestOnRouteAddAcceptsLiveFamilyEvenWithOtherFamilyDead proves the
// rejection above is genuinely per-family, not "origin has a problem,
// reject everything": a v4 route from an origin whose v6 path is dead but
// whose v4 path is live must still be accepted.
func TestOnRouteAddAcceptsLiveFamilyEvenWithOtherFamilyDead(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]
	now := time.Now()

	origin4 := netip.MustParseAddr("10.9.9.1")
	origin6 := netip.MustParseAddr("fd00:9::1")
	ps := &peerSession{net: ns, nodeID: "origin", overlay4: origin4, overlay6: origin6}
	longAgo := now.Add(-2 * familyDeadAfter).UnixNano()
	ps.familyProbeSent6.Store(longAgo) // v6 probed long ago, never good -> dead
	// v4 left at its zero value -> "never probed yet" -> optimistically live.
	ns.mu.Lock()
	ns.byNode["origin"] = ps
	ns.mu.Unlock()

	v4 := netip.MustParsePrefix("10.41.0.0/24")
	e.onRouteAdd(ps, encodeRouteAdd("origin", v4, 5)[1:])

	ns.mu.RLock()
	n := len(ns.redist)
	ns.mu.RUnlock()
	if n != 1 {
		t.Fatalf("redist = %d entries, want 1 (v4 route from an origin with only v6 dead should still be accepted)", n)
	}
	if !dev.hasRoute(v4) {
		t.Fatal("the live-family route was not programmed into the OS table")
	}
}

// TestSweepDeadFamilyRoutesWithdrawsOnlyDeadFamily is the withdrawal-side
// mirror of the two tests above: given one v4 and one v6 route already
// accepted from the same origin, and that origin's v6 path (only) going
// dead afterward, the sweep must withdraw the v6 route and leave the v4
// route alone — the same independence familyprobe.go and hostsync.go
// already hold to elsewhere.
func TestSweepDeadFamilyRoutesWithdrawsOnlyDeadFamily(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]
	now := time.Now()

	origin4 := netip.MustParseAddr("10.9.9.1")
	origin6 := netip.MustParseAddr("fd00:9::1")
	ps := &peerSession{net: ns, nodeID: "origin", overlay4: origin4, overlay6: origin6}
	// v4 currently healthy; v6 has gone dead.
	recent := now.Add(-time.Second).UnixNano()
	ps.familyProbeSent4.Store(recent)
	ps.familyGood4.Store(recent)
	longAgo := now.Add(-2 * familyDeadAfter).UnixNano()
	ps.familyProbeSent6.Store(longAgo)

	v4 := netip.MustParsePrefix("10.42.0.0/24")
	v6 := netip.MustParsePrefix("fd42::/64")
	ns.mu.Lock()
	ns.byNode["origin"] = ps
	ns.redist = append(ns.redist,
		routeEntry{origin: "origin", prefix: v4, metric: 5, lastSeen: now},
		routeEntry{origin: "origin", prefix: v6, metric: 5, lastSeen: now},
	)
	ns.knownRoute["origin|"+v4.String()] = true
	ns.knownRoute["origin|"+v6.String()] = true
	ns.mu.Unlock()
	e.syncRoute(ns, v4)
	e.syncRoute(ns, v6)
	if !dev.hasRoute(v4) || !dev.hasRoute(v6) {
		t.Fatal("setup: both routes should be installed in the OS table before the sweep")
	}

	e.sweepDeadFamilyRoutes(ns, now)

	if dev.hasRoute(v6) {
		t.Error("v6 route from the now-dead v6 family was not withdrawn")
	}
	if !dev.hasRoute(v4) {
		t.Error("v4 route was wrongly withdrawn even though its family is still live")
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if len(ns.redist) != 1 || ns.redist[0].prefix != v4 {
		t.Fatalf("redist after sweep = %+v, want only the v4 route", ns.redist)
	}
	if ns.knownRoute["origin|"+v6.String()] {
		t.Error("knownRoute still references the withdrawn v6 route")
	}
}

// TestSweepDeadFamilyRoutesLeavesSessionlessOriginAlone proves this sweep
// stays out of dropNodeRoutes' lane: a redist entry whose origin has no
// live session at all (already gone, or not yet reaped by
// pruneDead/sweepStuckKeepalive) must be left exactly as-is here, not
// withdrawn by this function too — teardownSessions -> dropNodeRoutes owns
// that case, and having two independent paths reach the same conclusion by
// different logic is a correctness risk, not redundancy worth having.
func TestSweepDeadFamilyRoutesLeavesSessionlessOriginAlone(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]
	now := time.Now()

	v4 := netip.MustParsePrefix("10.43.0.0/24")
	ns.mu.Lock()
	// Deliberately no ns.byNode["ghost"] entry.
	ns.redist = append(ns.redist, routeEntry{origin: "ghost", prefix: v4, metric: 5, lastSeen: now})
	ns.knownRoute["ghost|"+v4.String()] = true
	ns.mu.Unlock()
	e.syncRoute(ns, v4)

	e.sweepDeadFamilyRoutes(ns, now)

	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if len(ns.redist) != 1 {
		t.Fatalf("redist after sweep = %+v, want the sessionless entry left untouched", ns.redist)
	}
	if !dev.hasRoute(v4) {
		t.Error("route from a sessionless origin was withdrawn by sweepDeadFamilyRoutes, not left for dropNodeRoutes")
	}
}
