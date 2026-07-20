package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func TestRouteMetricPropagatesAndUpdates(t *testing.T) {
	const netID = uint64(0x80033)
	key, _ := crypto.GenerateKey()
	route := netip.MustParsePrefix("10.40.40.0/24")

	mk := func(name string, self netip.Addr, routes []netip.Prefix, metric map[netip.Prefix]int) *testNode {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, Routes: routes, RouteMetric: metric}}})
		tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
		if err != nil {
			t.Fatal(err)
		}
		eng.Attach(tr)
		eng.Start()
		return &testNode{eng, tr, dev}
	}
	A := mk("A", netip.MustParseAddr("10.9.0.1"), []netip.Prefix{route}, map[netip.Prefix]int{route: 7})
	B := mk("B", netip.MustParseAddr("10.9.0.2"), nil, nil)
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))

	metricOnB := func() (int, bool) {
		for _, ri := range B.eng.Routes(netID) {
			if ri.CIDR == route.String() {
				return ri.Metric, true
			}
		}
		return 0, false
	}
	// B learns the route with metric 7.
	if !waitUntil(10*time.Second, func() bool { m, ok := metricOnB(); return ok && m == 7 }) {
		m, ok := metricOnB()
		t.Fatalf("B should learn metric 7; got %d ok=%v", m, ok)
	}
	// And the metric must reach B's OS route table (the device) — this is what
	// shows up in `ip route`, and is the whole point of installing it. It's
	// installed at the advertised value plus MeshRouteMetricFloor (see its own
	// doc comment) — a mesh-learned route must never outrank a locally-sourced
	// one for the same prefix — while the gossip-layer value above (what
	// Routes() reports, and what a peer re-advertising it forwards on) stays
	// the raw advertised metric; the floor is only ever added at the point a
	// route is actually programmed into this host's own OS table.
	if !waitUntil(5*time.Second, func() bool { return B.dev.hasRoute(route) && B.dev.metricOf(route) == 7+MeshRouteMetricFloor }) {
		t.Fatalf("B's device should hold route with metric %d; got metric %d hasRoute=%v",
			7+MeshRouteMetricFloor, B.dev.metricOf(route), B.dev.hasRoute(route))
	}
	// A changes the metric to 3 live; B must update.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Routes: []netip.Prefix{route}, RouteMetric: map[netip.Prefix]int{route: 3}}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { m, ok := metricOnB(); return ok && m == 3 }) {
		m, _ := metricOnB()
		t.Fatalf("B should update to metric 3 live; got %d", m)
	}
	// The OS route on B must be re-programmed to the new metric too.
	if !waitUntil(5*time.Second, func() bool { return B.dev.metricOf(route) == 3+MeshRouteMetricFloor }) {
		t.Fatalf("B's device metric should update to %d; got %d", 3+MeshRouteMetricFloor, B.dev.metricOf(route))
	}
}
