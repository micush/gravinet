package mesh

import (
	"bytes"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// BenchmarkOutboundThroughput drives real packets through the full
// TUN-read -> firewall/NAT/route -> encrypt -> UDP-send pipeline (via
// fakeDev.in, exactly like a real TUN read) between two engines connected
// over real loopback UDP sockets. This exercises the actual code path
// tunLoop/processOutbound own, not a synthetic microbenchmark — the same
// benchmark file is run unmodified against the pre- and post-worker-pool
// trees for a direct before/after comparison.
func BenchmarkOutboundThroughput(b *testing.B) {
	key, err := crypto.GenerateKey()
	if err != nil {
		b.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		b.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0xB0B0)

	mk := func(name string, self netip.Addr) (*Engine, *fakeDev, *transport.Transport) {
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "m", Keys: ks, Dev: dev, Self4: self}},
		})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 4,
			Handler: eng.OnPacket,
		})
		if err != nil {
			b.Fatalf("open %s: %v", name, err)
		}
		eng.Attach(tr)
		eng.Start()
		return eng, dev, tr
	}

	A, devA, trA := mk("A", netip.MustParseAddr("10.9.0.1"))
	B, devB, trB := mk("B", netip.MustParseAddr("10.9.0.2"))
	defer func() {
		devA.Close()
		devB.Close()
		A.Stop()
		B.Stop()
		trA.Close()
		trB.Close()
	}()

	lo := netip.MustParseAddr("127.0.0.1")
	A.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trB.Port())))
	B.AddSeed(netID, netip.AddrPortFrom(lo, uint16(trA.Port())))

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if A.PeerCount(netID) >= 1 && B.PeerCount(netID) >= 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if A.PeerCount(netID) < 1 || B.PeerCount(netID) < 1 {
		b.Fatalf("peers did not connect: A=%d B=%d", A.PeerCount(netID), B.PeerCount(netID))
	}

	payload := bytes.Repeat([]byte{0xAB}, 1200)
	pkt := makeIPv4(netip.MustParseAddr("10.9.0.1"), netip.MustParseAddr("10.9.0.2"), payload)

	// Drain devB.out continuously so the pipeline's receive side never
	// becomes the bottleneck being measured — we're timing A's outbound
	// path, not B's.
	stopDrain := make(chan struct{})
	var received atomic.Int64
	go func() {
		for {
			select {
			case <-devB.out:
				received.Add(1)
			case <-stopDrain:
				return
			}
		}
	}()

	b.ResetTimer()
	b.SetBytes(int64(len(pkt)))
	for i := 0; i < b.N; i++ {
		devA.in <- pkt
	}
	b.StopTimer()
	close(stopDrain)
	b.Logf("received on B side: %d/%d", received.Load(), b.N)
}
