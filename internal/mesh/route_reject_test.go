package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestRejectRemovesRouteLive: rejecting a learned route must pull it from the OS
// table immediately (like a withdrawal), not leave it until restart.
func TestRejectRemovesRouteLive(t *testing.T) {
	const netID = uint64(0x90044)
	key, _ := crypto.GenerateKey()
	route := netip.MustParsePrefix("10.55.55.0/24")

	mk := func(name string, self netip.Addr, routes []netip.Prefix) *testNode {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self, Routes: routes}}})
		tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
		if err != nil {
			t.Fatal(err)
		}
		eng.Attach(tr)
		eng.Start()
		return &testNode{eng, tr, dev}
	}
	A := mk("A", netip.MustParseAddr("10.11.0.1"), []netip.Prefix{route})
	B := mk("B", netip.MustParseAddr("10.11.0.2"), nil)
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

	if !waitUntil(10*time.Second, func() bool { return B.dev.hasRoute(route) }) {
		t.Fatal("B did not install the learned route")
	}
	// Reject it on B, live (no restart).
	if err := B.eng.ReloadRuntime(netID, NetSpec{ID: netID, RouteReject: []RejectRule{{Prefix: route}}}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !waitUntil(5*time.Second, func() bool { return !B.dev.hasRoute(route) }) {
		t.Fatal("reject did not remove the route from the OS table live")
	}
	for _, ri := range B.eng.Routes(netID) {
		if ri.CIDR == route.String() {
			t.Fatal("rejected route still present in forwarding table")
		}
	}
	// A re-advertising must not bring it back while the reject stands.
	A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Routes: []netip.Prefix{route}, RouteMetric: map[netip.Prefix]int{route: 1}})
	time.Sleep(1 * time.Second)
	if B.dev.hasRoute(route) {
		t.Fatal("rejected route was re-installed by a re-advertisement")
	}
}
