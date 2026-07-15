package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
)

// TestLiveNetworkAddRemove proves a network can be brought up and torn down at
// runtime — no engine restart — while another network keeps running.
func TestLiveNetworkAddRemove(t *testing.T) {
	const net1 = uint64(0x1111)
	const net2 = uint64(0x2222)
	key, _ := crypto.GenerateKey()

	A := spinNode(t, "A", net1, key, netip.MustParseAddr("10.1.0.1"))
	B := spinNode(t, "B", net1, key, netip.MustParseAddr("10.1.0.2"))
	devA, devB := newFakeDev("A2"), newFakeDev("B2")
	defer func() {
		// Close every TUN first so each tunLoop's blocking Read returns, then Stop.
		devA.Close()
		devB.Close()
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	port := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }

	// Baseline: connect on net1.
	A.eng.AddSeed(net1, port(B))
	B.eng.AddSeed(net1, port(A))
	if !waitUntil(8*time.Second, func() bool { return len(A.eng.PeerEndpoints(net1)) > 0 }) {
		t.Fatal("net1 never connected")
	}

	// Add net2 live to both nodes (shared transport, fresh TUN + same key).
	ks, _ := crypto.NewKeySet([]string{key})
	if err := A.eng.AddNetwork(NetSpec{ID: net2, Name: "n2", Keys: ks, Dev: devA, Self4: netip.MustParseAddr("10.2.0.1")}); err != nil {
		t.Fatalf("A add net2: %v", err)
	}
	if err := B.eng.AddNetwork(NetSpec{ID: net2, Name: "n2", Keys: ks, Dev: devB, Self4: netip.MustParseAddr("10.2.0.2")}); err != nil {
		t.Fatalf("B add net2: %v", err)
	}
	A.eng.AddSeed(net2, port(B))
	B.eng.AddSeed(net2, port(A))

	// net2 must come up WITHOUT any restart.
	if !waitUntil(8*time.Second, func() bool {
		return len(A.eng.PeerEndpoints(net2)) > 0 && len(B.eng.PeerEndpoints(net2)) > 0
	}) {
		t.Fatal("net2 added live but never connected")
	}
	if A.eng.network(net2) == nil {
		t.Fatal("net2 missing from A after add")
	}

	// Remove net2 from A live; net1 must keep working and the teardown must not hang.
	done := make(chan struct{})
	go func() { A.eng.RemoveNetwork(net2); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RemoveNetwork hung (goroutine teardown deadlock)")
	}
	if A.eng.network(net2) != nil {
		t.Fatal("net2 still present after RemoveNetwork")
	}
	if len(A.eng.PeerEndpoints(net1)) == 0 {
		t.Fatal("removing net2 disturbed net1")
	}
}
