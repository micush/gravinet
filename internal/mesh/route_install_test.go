package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func spinWithRoutes(t *testing.T, name string, netID uint64, key string, self netip.Addr, routes []netip.Prefix) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID: name, Hostname: name,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, Routes: routes}},
	})
	tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	eng.Attach(tr)
	eng.Start()
	return &testNode{eng, tr, dev}
}

// TestRedistributedRouteInstalledInOS proves a learned redistributed route is
// pushed into the receiving node's OS routing table (here, the fake device that
// records AddRoute/DelRoute), and withdrawn when the advertiser stops. This is
// the missing step that left the route out of `ip route` on peers.
func TestRedistributedRouteInstalledInOS(t *testing.T) {
	const netID = uint64(0x60011)
	key, _ := crypto.GenerateKey()
	route := netip.MustParsePrefix("10.20.20.0/24")

	A := spinWithRoutes(t, "A", netID, key, netip.MustParseAddr("10.7.0.1"), []netip.Prefix{route})
	B := spinWithRoutes(t, "B", netID, key, netip.MustParseAddr("10.7.0.2"), nil)
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

	// B should learn A's route and install it in its OS table.
	if !waitUntil(10*time.Second, func() bool { return B.dev.hasRoute(route) }) {
		t.Fatal("B did not install the redistributed route 10.20.20.0/24 in its OS routing table")
	}
	// A must NOT install its own advertised route (it already reaches it directly).
	if A.dev.hasRoute(route) {
		t.Fatal("A should not install a route for a prefix it originates")
	}

	// A withdraws the route -> B should remove it from the OS table.
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Routes: nil}); err != nil {
		t.Fatalf("withdraw reload: %v", err)
	}
	if !waitUntil(10*time.Second, func() bool { return !B.dev.hasRoute(route) }) {
		t.Fatal("B did not remove the route from its OS table after withdrawal")
	}
}
