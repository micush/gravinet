package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestBanCarriesHostname proves a ban record carries the banned node's *hostname*
// (captured at ban time, since applyBan drops the node from the registry), not
// merely its id — here the two differ deliberately.
func TestBanCarriesHostname(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xB055)

	mk := func(id, host string, self netip.Addr) *testNode {
		ks, _ := crypto.NewKeySet([]string{key})
		dev := newFakeDev(id)
		eng := NewEngine(Options{NodeID: id, Hostname: host,
			Nets: []NetSpec{{ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self}}})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
		if err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
		eng.Attach(tr)
		eng.Start()
		return &testNode{eng, tr, dev}
	}

	A := mk("node-a", "alpha-box", netip.MustParseAddr("10.9.1.1"))
	B := mk("node-b", "bravo-box", netip.MustParseAddr("10.9.1.2"))
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

	if !waitUntil(25*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 1 && B.eng.PeerCount(netID) == 1
	}) {
		t.Fatalf("mesh did not form: A=%d B=%d", A.eng.PeerCount(netID), B.eng.PeerCount(netID))
	}

	if err := A.eng.BanNode(netID, "node-b", "test"); err != nil {
		t.Fatalf("ban: %v", err)
	}
	found := false
	for _, b := range A.eng.ListBans(netID) {
		if b.Target == "node-b" {
			found = true
			if b.Hostname != "bravo-box" {
				t.Fatalf("ban hostname = %q, want %q (the learned hostname, not the id)", b.Hostname, "bravo-box")
			}
			// A issued the ban, so the origin hostname is A's own hostname.
			if b.Origin != "node-a" {
				t.Fatalf("ban origin = %q, want %q", b.Origin, "node-a")
			}
			if b.OriginHostname != "alpha-box" {
				t.Fatalf("ban origin hostname = %q, want %q (the issuing node's hostname)", b.OriginHostname, "alpha-box")
			}
		}
	}
	if !found {
		t.Fatalf("ban not recorded: %v", A.eng.ListBans(netID))
	}
}
