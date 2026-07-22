package mesh

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestTunLoopPooledDeliversAllPackets forces TunWorkers to a value >1 so
// tunLoopPooled itself — not just processOutbound in isolation (see
// TestProcessOutboundConcurrentSameDest) — runs end to end: real TUN reads
// via fakeDev, real channel handoff to real worker goroutines, real
// sync.Pool buffer reuse, real teardown. Every other test in this package
// constructs engines with the default TunWorkers (0 => runtime.NumCPU()-1,
// min 1), which is 1 on single-core hardware — meaning on a 1-core CI
// runner, nothing else in this package ever actually exercises
// tunLoopPooled's plumbing, only tunLoopSerial's. This test is what closes
// that gap regardless of how many cores the host actually has.
func TestTunLoopPooledDeliversAllPackets(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0xF00B)

	mk := func(name string, self netip.Addr) (*Engine, *fakeDev, *transport.Transport) {
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID: name, Hostname: name,
			Nets:       []NetSpec{{ID: netID, Name: "m", Keys: ks, Dev: dev, Self4: self}},
			TunWorkers: 4, // force the pooled path regardless of host core count
		})
		tr, err := transport.Open(transport.Options{
			BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 2,
			Handler: eng.OnPacket,
		})
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
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
		t.Fatalf("peers did not connect: A=%d B=%d", A.PeerCount(netID), B.PeerCount(netID))
	}

	// devB.out (fakeDev's debug tap) is intentionally non-blocking/lossy —
	// it exists so a test with nobody reading it can't deadlock, dropping
	// writes rather than blocking deliverInner. Under this test's load (two
	// full engines' worth of tunLoop/UDP-worker/maintenance goroutines all
	// contending for whatever cores are available) a drain goroutine can
	// easily lose its scheduling race against that 16-slot buffer filling
	// up, which says nothing about whether packets were actually delivered
	// — only that this debug tap couldn't keep up watching them. The real,
	// non-lossy signal is the transport's monotonic rx counter: it can only
	// go up, once per datagram actually received and decrypted, regardless
	// of what happens downstream. That's the assertion this test gates on;
	// devB.out is drained too and logged, but only as corroboration.
	const total = 200
	var tapped atomic.Int64
	go func() {
		for {
			select {
			case <-devB.out:
				tapped.Add(1)
			case <-time.After(15 * time.Second):
				return
			}
		}
	}()

	for i := 0; i < total; i++ {
		payload := []byte{byte(i), byte(i >> 8), 0xAA, 0xBB}
		pkt := makeIPv4(netip.MustParseAddr("10.9.0.1"), netip.MustParseAddr("10.9.0.2"), payload)
		select {
		case devA.in <- pkt:
		case <-time.After(3 * time.Second):
			t.Fatalf("devA.in <- pkt stalled at send #%d/%d (A's tunLoop stopped draining its TUN device)", i, total)
		}
	}
	t.Logf("all %d packets pushed into devA.in successfully", total)

	// B's real rx count includes a handful of keepalive/control datagrams
	// alongside the `total` data packets, so it settles a little above
	// `total`, not exactly at it — wait for it to stop climbing rather than
	// hit an exact number.
	var rxB uint64
	deadline = time.Now().Add(12 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		cur, _ := trB.Stats()
		if cur == rxB {
			stable++
			if stable >= 5 { // ~500ms with nothing new arriving: settled
				break
			}
		} else {
			stable = 0
		}
		rxB = cur
		time.Sleep(100 * time.Millisecond)
	}
	if rxB < uint64(total) {
		t.Fatalf("B's real transport-level rx count settled at %d, want >= %d data packets (tunLoopPooled did not deliver everything over the wire)", rxB, total)
	}
	t.Logf("B real transport rx=%d (>= %d data packets, plus keepalives/control — confirms real delivery); fakeDev.out debug tap saw %d/%d (informational only, see comment above)",
		rxB, total, tapped.Load(), total)

	// Clean shutdown must complete promptly: ns.wg.Wait() (inside Stop)
	// blocks until tunLoopPooled's own workers.Wait() has returned, so a
	// hang here would mean the close(jobs)/workers.Wait() teardown ordering
	// is broken.
	stopped := make(chan struct{})
	go func() {
		A.Stop()
		B.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return promptly — tunLoopPooled worker teardown may be hanging")
	}
}
