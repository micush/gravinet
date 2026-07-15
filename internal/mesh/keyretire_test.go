package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func spinNodeKeys(t *testing.T, name string, netID uint64, keys []string, self netip.Addr) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet(keys)
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID: name, Hostname: name,
		Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self}},
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

func keyID(k string) crypto.KeyID {
	ks, _ := crypto.NewKeySet([]string{k})
	return ks.Order()[0]
}

func sessionKeyID(n *testNode, netID uint64, node string) crypto.KeyID {
	ns := n.eng.network(netID)
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	if ps := ns.byNode[node]; ps != nil {
		return ps.keyID
	}
	return crypto.KeyID{}
}

// TestKeyDisableReconnects: disabling the key a session rides on tears it down,
// and the peers re-handshake on another shared key with no restart.
func TestKeyDisableReconnects(t *testing.T) {
	const netID = uint64(0x5EE5)
	k1, _ := crypto.GenerateKey()
	k2, _ := crypto.GenerateKey()

	A := spinNodeKeys(t, "A", netID, []string{k1, k2}, netip.MustParseAddr("10.5.0.1"))
	B := spinNodeKeys(t, "B", netID, []string{k1, k2}, netip.MustParseAddr("10.5.0.2"))
	defer func() {
		for _, n := range []*testNode{A, B} {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()
	lo := netip.MustParseAddr("127.0.0.1")
	port := func(n *testNode) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }
	A.eng.AddSeed(netID, port(B))
	B.eng.AddSeed(netID, port(A))

	if !waitUntil(8*time.Second, func() bool { return A.eng.connectedToNode(A.eng.network(netID), "B") }) {
		t.Fatal("A and B never connected")
	}
	if sessionKeyID(A, netID, "B") != keyID(k1) {
		t.Skip("session didn't form on k1 (key ordering); skip")
	}

	// Disable k1 on A: reload its set down to {k2} only.
	ksK2, _ := crypto.NewKeySet([]string{k2})
	if err := A.eng.ReloadRuntime(netID, NetSpec{ID: netID, Keys: ksK2}); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// A must reconnect to B on k2 without a restart.
	if !waitUntil(10*time.Second, func() bool {
		return A.eng.connectedToNode(A.eng.network(netID), "B") && sessionKeyID(A, netID, "B") == keyID(k2)
	}) {
		t.Fatal("after disabling k1, A did not re-handshake B on k2")
	}
}
