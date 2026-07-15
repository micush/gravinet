package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func TestRandomHostBounds(t *testing.T) {
	cases := []string{"10.42.0.0/16", "192.168.1.0/24", "10.0.0.0/30", "fd00:42::/64"}
	for _, c := range cases {
		pfx := netip.MustParsePrefix(c)
		for i := 0; i < 200; i++ {
			a := randomHost(pfx)
			if !a.IsValid() {
				t.Fatalf("%s: invalid address", c)
			}
			if !pfx.Contains(a) {
				t.Fatalf("%s: %s outside prefix", c, a)
			}
			if a == pfx.Masked().Addr() {
				t.Fatalf("%s: picked network address", c)
			}
		}
	}
}

// TestDynamicAddressing starts two nodes with NO static address. Node B has the
// subnet configured; node A learns it via the handshake. Both must self-assign
// distinct, in-subnet addresses through DAD.
// TestLoneNodeSelfAssigns guards the bootstrap case: the first/only node in a
// network (no peers) must still self-assign an overlay address. Previously the
// assignment was gated on having a peer, so a lone node stayed address-less.
func TestLoneNodeSelfAssigns(t *testing.T) {
	key, _ := crypto.GenerateKey()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev("solo")
	sub := netip.MustParsePrefix("10.70.0.0/16")
	eng := NewEngine(Options{
		NodeID:   "solo",
		Hostname: "solo",
		Nets:     []NetSpec{{ID: 0x7777, Name: "d", Keys: ks, Dev: dev, Subnet4: sub}},
	})
	tr, err := transport.Open(transport.Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1,
		Handler: eng.OnPacket,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	eng.Attach(tr)
	eng.Start() // no seeds, no peers
	defer func() { dev.Close(); eng.Stop(); tr.Close() }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if dev.addr4().IsValid() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	a4 := dev.addr4()
	if !a4.IsValid() {
		t.Fatalf("lone node did not self-assign an address")
	}
	if !sub.Contains(a4) {
		t.Fatalf("assigned address %s not in subnet %s", a4, sub)
	}
}

func TestDynamicAddressing(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xDDDD)
	subnet := netip.MustParsePrefix("10.55.0.0/16")

	devA := newFakeDev("a")
	devB := newFakeDev("b")

	mk := func(name string, dev *fakeDev, sub netip.Prefix) (*Engine, *transport.Transport) {
		ks, _ := crypto.NewKeySet([]string{key})
		eng := NewEngine(Options{
			NodeID:   name,
			Hostname: name,
			Nets:     []NetSpec{{ID: netID, Name: "d", Keys: ks, Dev: dev, Subnet4: sub}},
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
		return eng, tr
	}

	// A has no subnet configured; it must learn it from B during the handshake.
	engA, trA := mk("A", devA, netip.Prefix{})
	engB, trB := mk("B", devB, subnet)
	defer func() {
		devA.Close()
		devB.Close()
		engA.Stop()
		engB.Stop()
		trA.Close()
		trB.Close()
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	engA.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trB.Port())))
	engB.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trA.Port())))

	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if devA.addr4().IsValid() && devB.addr4().IsValid() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	a4, b4 := devA.addr4(), devB.addr4()
	if !a4.IsValid() || !b4.IsValid() {
		t.Fatalf("nodes did not self-assign: A=%s B=%s", a4, b4)
	}
	if !subnet.Contains(a4) || !subnet.Contains(b4) {
		t.Fatalf("assigned addresses out of subnet: A=%s B=%s", a4, b4)
	}
	if a4 == b4 {
		t.Fatalf("duplicate address assigned: %s", a4)
	}
	t.Logf("dynamic addresses: A=%s B=%s (A learned subnet %s from B)", a4, b4, subnet)
}
