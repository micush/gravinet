package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func TestTokenBucket(t *testing.T) {
	var now time.Time
	tb := newTokenBucket(10, 5) // 10/s, burst 5
	tb.now = func() time.Time { return now }
	now = time.Unix(1000, 0)
	tb.last = now

	for i := 0; i < 5; i++ {
		if !tb.allow() {
			t.Fatalf("burst token %d should be allowed", i)
		}
	}
	if tb.allow() {
		t.Fatal("token past burst should be denied while frozen")
	}
	// 1s later: +10 tokens but capped at burst (5).
	now = now.Add(time.Second)
	allowed := 0
	for i := 0; i < 20; i++ {
		if tb.allow() {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("after refill expected burst cap 5, got %d", allowed)
	}

	// pps<=0 means unlimited.
	ub := newTokenBucket(0, 0)
	for i := 0; i < 1000; i++ {
		if !ub.allow() {
			t.Fatal("unlimited bucket denied a packet")
		}
	}
}

func TestClassify(t *testing.T) {
	ns := &netState{subnet4: netip.MustParsePrefix("10.42.0.0/16")}
	cases := []struct {
		addr string
		want dstKind
	}{
		{"10.42.0.5", kindUnicast},
		{"255.255.255.255", kindBroadcast},
		{"10.42.255.255", kindBroadcast}, // subnet broadcast
		{"224.0.0.1", kindMulticast},
		{"239.1.2.3", kindMulticast},
		{"ff02::1", kindMulticast},
		{"fd00::1", kindUnicast},
		{"2001:db8::1", kindUnicast},
	}
	for _, c := range cases {
		if got := ns.classify(netip.MustParseAddr(c.addr)); got != c.want {
			t.Errorf("classify(%s) = %d, want %d", c.addr, got, c.want)
		}
	}
}

// spinNodeStorm builds a node with explicit storm-control limits.
func spinNodeStorm(t *testing.T, name string, netID uint64, key string, self netip.Addr, bpps, burst int) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self,
			BroadcastPPS: bpps, MulticastPPS: bpps, StormBurst: burst,
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

func TestBroadcastDelivery(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xBCA5)

	A := spinNode(t, "A", netID, key, netip.MustParseAddr("10.8.0.1"))
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.8.0.2"))
	C := spinNode(t, "C", netID, key, netip.MustParseAddr("10.8.0.3"))
	all := []*testNode{A, B, C}
	defer func() {
		for _, n := range all {
			n.dev.Close()
			n.eng.Stop()
			n.tr.Close()
		}
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	A.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(B.tr.Port())))
	B.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))
	C.eng.AddSeed(netID, netip.AddrPortFrom(lo, uint16(A.tr.Port())))

	if !waitUntil(25*time.Second, func() bool {
		return A.eng.PeerCount(netID) == 2
	}) {
		t.Fatalf("mesh did not form: A=%d", A.eng.PeerCount(netID))
	}

	// A sends a limited broadcast; B and C should both receive it on their TUN.
	bc := makeIPv4(netip.MustParseAddr("10.8.0.1"), netip.MustParseAddr("255.255.255.255"), []byte("hello-mesh"))
	A.dev.in <- bc

	for _, n := range []*testNode{B, C} {
		select {
		case got := <-n.dev.out:
			if string(got) != string(bc) {
				t.Fatalf("%s got wrong broadcast packet", n.eng.nodeID)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not receive the broadcast", n.eng.nodeID)
		}
	}
}

func TestBroadcastStormControl(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xBCB0)

	A := spinNodeStorm(t, "A", netID, key, netip.MustParseAddr("10.9.0.1"), 3, 3) // 3 pps, burst 3
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.9.0.2"))
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
	if !waitUntil(15*time.Second, func() bool { return A.eng.PeerCount(netID) == 1 }) {
		t.Fatal("A-B did not connect")
	}

	// Freeze A's broadcast bucket so no tokens refill: exactly burst (3) pass.
	frozen := time.Unix(2000, 0)
	nsA := A.eng.network(netID)
	nsA.bcast.now = func() time.Time { return frozen }
	nsA.bcast.last = frozen
	nsA.bcast.tokens = 3

	for i := 0; i < 20; i++ {
		A.dev.in <- makeIPv4(netip.MustParseAddr("10.9.0.1"), netip.MustParseAddr("255.255.255.255"), []byte{byte(i)})
	}

	// Count what B receives within a window; storm control caps it at the burst.
	got := 0
	deadline := time.After(2 * time.Second)
loop:
	for {
		select {
		case <-B.dev.out:
			got++
		case <-deadline:
			break loop
		}
	}
	if got != 3 {
		t.Fatalf("storm control: expected exactly burst=3 delivered, got %d", got)
	}
}
