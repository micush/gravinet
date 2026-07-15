package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

func spinNodeThrottle(t *testing.T, name string, netID uint64, key string, self netip.Addr, up, down int) *testNode {
	t.Helper()
	ks, _ := crypto.NewKeySet([]string{key})
	dev := newFakeDev(name)
	eng := NewEngine(Options{
		NodeID:   name,
		Hostname: name,
		Nets: []NetSpec{{
			ID: netID, Name: "n", Keys: ks, Dev: dev, Self4: self,
			ThrottleUp: up, ThrottleDown: down,
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

func TestEgressThrottle(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x7401)

	A := spinNodeThrottle(t, "A", netID, key, netip.MustParseAddr("10.11.0.1"), 50000, 0) // 50 KB/s up
	B := spinNode(t, "B", netID, key, netip.MustParseAddr("10.11.0.2"))
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

	// Drain B's TUN output in the background, counting deliveries.
	got := make(chan int, 1)
	go func() {
		n := 0
		for range B.dev.out {
			n++
			if n == 40 {
				got <- n
				return
			}
		}
	}()

	const total = 40
	pkt := makeIPv4(netip.MustParseAddr("10.11.0.1"), netip.MustParseAddr("10.11.0.2"), make([]byte, 4000))
	start := time.Now()
	for i := 0; i < total; i++ {
		A.dev.in <- pkt
	}

	select {
	case <-got:
	case <-time.After(8 * time.Second):
		t.Fatal("throttled traffic did not all arrive")
	}
	elapsed := time.Since(start)

	// The fake device caps each packet at its 1400B MTU, so ~40×1528 ≈ 61KB on
	// the wire; at 50 KB/s that paces to ~1s. Unthrottled it would be near
	// instant, so require clear evidence of pacing.
	if elapsed < 600*time.Millisecond {
		t.Fatalf("egress throttle had no effect (%v for %d packets)", elapsed, total)
	}
	t.Logf("delivered %d packets in %v (~%.0f KB/s)", total, elapsed,
		float64(total*1528)/elapsed.Seconds()/1000)
}
