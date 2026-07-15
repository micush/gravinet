package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestPrunedNodeRoutesRemoved: when a route-advertising node goes silent and its
// session is pruned, the peer must drop the learned route from its OS table —
// not leave it lingering until restart.
func TestPrunedNodeRoutesRemoved(t *testing.T) {
	const netID = uint64(0x70022)
	key, _ := crypto.GenerateKey()
	route := netip.MustParsePrefix("10.30.30.0/24")

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
	A := mk("A", netip.MustParseAddr("10.8.0.1"), []netip.Prefix{route})
	B := mk("B", netip.MustParseAddr("10.8.0.2"), nil)
	aStopped := false
	defer func() {
		if !aStopped {
			A.dev.Close()
			A.eng.Stop()
			A.tr.Close()
		}
		B.dev.Close()
		B.eng.Stop()
		B.tr.Close()
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	if !waitUntil(10*time.Second, func() bool { return B.dev.hasRoute(route) }) {
		t.Fatal("B did not install the route")
	}
	// Kill A's transport so it goes silent, then force B to prune dead sessions.
	A.eng.Stop()
	A.tr.Close()
	aStopped = true
	ns := B.eng.network(netID)
	if !waitUntil(20*time.Second, func() bool {
		B.eng.pruneDead(ns, time.Now().Add(2*time.Minute)) // jump past peerTimeout
		return !B.dev.hasRoute(route)
	}) {
		t.Fatal("B did not drop the route after A's session was pruned")
	}
}
