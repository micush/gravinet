package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func TestPeerListCodec(t *testing.T) {
	in := []peerEntry{
		{
			nodeID:   "node-A",
			hostname: "alpha",
			overlay4: netip.MustParseAddr("10.0.0.1"),
			overlay6: netip.MustParseAddr("fd00::1"),
			endpoint: netip.MustParseAddrPort("198.51.100.7:51820"),
		},
		{
			nodeID:   "node-B",
			hostname: "bravo",
			overlay4: netip.MustParseAddr("10.0.0.2"),
			endpoint: netip.MustParseAddrPort("[2001:db8::2]:4500"),
		},
	}
	out, err := decodePeerList(encodePeerList(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 entries, got %d", len(out))
	}
	if out[0].nodeID != "node-A" || out[0].hostname != "alpha" ||
		out[0].overlay4 != in[0].overlay4 || out[0].overlay6 != in[0].overlay6 ||
		out[0].endpoint != in[0].endpoint {
		t.Fatalf("entry 0 mismatch: %+v", out[0])
	}
	if out[1].endpoint != in[1].endpoint || out[1].overlay6.IsValid() {
		t.Fatalf("entry 1 mismatch: %+v", out[1])
	}
}

// TestMeshFormation starts three nodes where C is seeded only with A. Via
// gossip, C must discover B (and B discover C), converging to a full mesh
// where every node has two peers.
func TestMeshFormation(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x5151)

	type node struct {
		eng *Engine
		tr  *transport.Transport
		dev *fakeDev
	}
	mk := func(name string, self netip.Addr) *node {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name + ".mesh",
			Nets:     []NetSpec{{ID: netID, Name: "m", Keys: ks, Dev: dev, Self4: self}},
		})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1,
			Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		eng.Attach(tr)
		eng.Start()
		return &node{eng, tr, dev}
	}

	A := mk("A", netip.MustParseAddr("10.9.0.1"))
	B := mk("B", netip.MustParseAddr("10.9.0.2"))
	C := mk("C", netip.MustParseAddr("10.9.0.3"))
	nodes := []*node{A, B, C}
	defer func() {
		for _, n := range nodes {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	ap := func(n *node) netip.AddrPort { return netip.AddrPortFrom(lo, uint16(n.tr.Port())) }

	// A<->B are direct seeds; C only knows A. B<->C must form via gossip.
	A.eng.AddSeed(netID, ap(B))
	B.eng.AddSeed(netID, ap(A))
	C.eng.AddSeed(netID, ap(A))

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if A.eng.PeerCount(netID) >= 2 && B.eng.PeerCount(netID) >= 2 && C.eng.PeerCount(netID) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, n := range []struct {
		name string
		e    *Engine
	}{{"A", A.eng}, {"B", B.eng}, {"C", C.eng}} {
		if got := n.e.PeerCount(netID); got < 2 {
			t.Fatalf("%s did not reach full mesh: peers=%d", n.name, got)
		}
	}
	t.Logf("full mesh formed: A=%d B=%d C=%d peers",
		A.eng.PeerCount(netID), B.eng.PeerCount(netID), C.eng.PeerCount(netID))
}
