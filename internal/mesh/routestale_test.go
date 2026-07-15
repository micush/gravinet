package mesh

import (
	"net/netip"
	"testing"
	"time"
)

func TestRouteTTL(t *testing.T) {
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: newFakeDev("d")}}})
	// Default cadence (10s) -> 20s hold, which is the target withdrawal time.
	if got := e.routeTTL(); got != 20*time.Second {
		t.Fatalf("default routeTTL = %s, want 20s", got)
	}
	// Scales with the live cadence.
	e.SetRouteAdvInterval(30 * time.Second)
	if got := e.routeTTL(); got != 60*time.Second {
		t.Fatalf("routeTTL at 30s cadence = %s, want 60s", got)
	}
	// Floored so a tiny cadence can't flap on jitter.
	e.SetRouteAdvInterval(time.Second)
	if got := e.routeTTL(); got != minRouteHold {
		t.Fatalf("routeTTL floor = %s, want %s", got, minRouteHold)
	}
}

func TestSweepStaleRoutesWithdrawsAndKeeps(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]
	now := time.Now()
	ttl := e.routeTTL()

	stale := netip.MustParsePrefix("10.9.0.0/24")
	fresh := netip.MustParsePrefix("10.8.0.0/24")
	add := func(origin string, p netip.Prefix, seen time.Time) {
		ns.mu.Lock()
		ns.redist = append(ns.redist, routeEntry{origin: origin, prefix: p, metric: 5, lastSeen: seen})
		ns.knownRoute[origin+"|"+p.String()] = true
		ns.mu.Unlock()
		e.syncRoute(ns, p) // program the OS route
	}
	// One route whose origin went silent well past the TTL, one just refreshed.
	add("offline", stale, now.Add(-ttl-10*time.Second))
	add("alive", fresh, now)
	if !dev.hasRoute(stale) || !dev.hasRoute(fresh) {
		t.Fatal("routes were not installed in the OS table")
	}

	e.sweepStaleRoutes(ns, now)

	if dev.hasRoute(stale) {
		t.Fatal("stale route (origin offline) was not withdrawn from the OS table")
	}
	if !dev.hasRoute(fresh) {
		t.Fatal("freshly-advertised route was wrongly withdrawn")
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if len(ns.redist) != 1 || ns.redist[0].origin != "alive" {
		t.Fatalf("redist after sweep = %+v, want only the fresh route", ns.redist)
	}
	if ns.knownRoute["offline|"+stale.String()] {
		t.Fatal("knownRoute still references the withdrawn route")
	}
}

// TestReadvertiseRefreshesRoute confirms a re-advertisement of a known route
// resets its freshness, so a live origin's routes are never swept.
func TestReadvertiseRefreshesRoute(t *testing.T) {
	dev := newFakeDev("d")
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{ID: 1, Name: "n", Dev: dev}}})
	ns := e.netSnapshot()[1]
	ps := &peerSession{net: ns, nodeID: "peer"}
	p := netip.MustParsePrefix("10.7.0.0/24")
	body := encodeRouteAdd("origin", p, 5)[1:] // strip the leading ctrl byte

	e.onRouteAdd(ps, body) // learn
	// Age it as though the origin had gone quiet.
	ns.mu.Lock()
	for i := range ns.redist {
		if ns.redist[i].prefix == p {
			ns.redist[i].lastSeen = time.Now().Add(-time.Hour)
		}
	}
	ns.mu.Unlock()

	e.onRouteAdd(ps, body)             // re-advertisement must refresh it
	e.sweepStaleRoutes(ns, time.Now()) // ...so this must not withdraw it

	ns.mu.RLock()
	n := len(ns.redist)
	ns.mu.RUnlock()
	if n != 1 {
		t.Fatalf("re-advertised route was wrongly swept; redist len=%d", n)
	}
}
