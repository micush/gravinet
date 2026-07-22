package transport

import (
	"net/netip"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// BenchmarkLoopbackThroughput drives datagrams between two real transports over
// loopback and reports packets/sec, so the batched and per-packet paths can be
// compared directly at the underlay without the mesh's crypto and routing on
// top (internal/mesh's BenchmarkOutboundThroughput covers the full pipeline).
//
// Set GRAVINET_NO_UDP_BATCH=1 to measure the per-packet path for comparison.
// Note that batching is a throughput optimisation for machines with more than
// one core: on a single-core box initBatch deliberately stays on the
// per-packet path, so both configurations measure the same code there.
func BenchmarkLoopbackThroughput(b *testing.B) {
	var rx atomic.Uint64
	recv, err := Open(Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 2,
		Handler: func(p []byte, from netip.AddrPort, fam Family) { rx.Add(1) },
	})
	if err != nil {
		b.Fatalf("open recv: %v", err)
	}
	defer recv.Close()

	send, err := Open(Options{
		BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1,
		Handler: func([]byte, netip.AddrPort, Family) {},
	})
	if err != nil {
		b.Fatalf("open send: %v", err)
	}
	defer send.Close()

	dst := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(recv.Port()))
	// A payload near a typical tunnelled packet, not a toy 8-byte datagram.
	payload := make([]byte, 1200)
	for i := range payload {
		payload[i] = byte(i)
	}

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	for i := 0; i < b.N; i++ {
		if err := send.Send(dst, payload); err != nil {
			b.Fatalf("send: %v", err)
		}
	}
	elapsed := time.Since(start)
	b.StopTimer()

	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "pkts/sec")
	b.ReportMetric(float64(rx.Load())/float64(b.N)*100, "%%delivered")
	_ = runtime.NumCPU()
}
