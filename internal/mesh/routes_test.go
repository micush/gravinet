package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// spinNodeRoutes is like spinNode but lets the caller set advertised routes and
// reject rules.
func spinNodeRoutes(t *testing.T, name string, netID uint64, key string, self netip.Addr,
	routes []netip.Prefix, reject []RejectRule) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self,
			Routes: routes, RouteReject: reject,
		}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}

func TestRouteRedistribution(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x52E)
	adv := netip.MustParsePrefix("100.64.0.0/24")

	// A advertises the route; B accepts it.
	A := spinNodeRoutes(t, "A", netID, key, netip.MustParseAddr("10.3.0.1"), []netip.Prefix{adv}, nil)
	B := spinNodeRoutes(t, "B", netID, key, netip.MustParseAddr("10.3.0.2"), nil, nil)
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

	if !waitUntil(10*time.Second, func() bool {
		return A.eng.PeerCount(netID) >= 1 && B.eng.PeerCount(netID) >= 1
	}) {
		t.Fatal("nodes did not connect")
	}

	// B should learn the route and resolve a packet in it to A's session.
	nsB := B.eng.network(netID)
	if !waitUntil(15*time.Second, func() bool {
		return B.eng.redistRoute(nsB, netip.MustParseAddr("100.64.0.5")) != nil
	}) {
		t.Fatalf("B did not learn redistributed route; routes=%v", B.eng.Routes(netID))
	}
	ps := B.eng.redistRoute(nsB, netip.MustParseAddr("100.64.0.5"))
	if ps == nil || ps.nodeID != "A" {
		t.Fatalf("route next-hop wrong: %+v", ps)
	}
}

func TestRouteReject(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x52F)
	adv := netip.MustParsePrefix("100.64.0.0/24")
	rej := netip.MustParsePrefix("100.64.0.0/16") // covers the advertised /24

	A := spinNodeRoutes(t, "A", netID, key, netip.MustParseAddr("10.4.0.1"), []netip.Prefix{adv}, nil)
	B := spinNodeRoutes(t, "B", netID, key, netip.MustParseAddr("10.4.0.2"), nil, []RejectRule{{Prefix: rej, Inclusive: true}})
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

	if !waitUntil(10*time.Second, func() bool {
		return B.eng.PeerCount(netID) >= 1
	}) {
		t.Fatal("nodes did not connect")
	}

	// Give A time to advertise; B must reject it.
	time.Sleep(3 * time.Second)
	if len(B.eng.Routes(netID)) != 0 {
		t.Fatalf("B should have rejected the route, got %v", B.eng.Routes(netID))
	}
}
