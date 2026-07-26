package mesh

import (
	"net/netip"
	"testing"
)

// tcpPacket builds a minimal well-formed IPv4 TCP packet with the given
// endpoints, enough for parseSrc/parseDst/parseL4 — which is all flowIndex
// reads.
func tcpPacket(src, dst netip.Addr, sport, dport uint16) []byte {
	p := make([]byte, 40)
	p[0] = 0x45
	total := uint16(len(p))
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64 // TTL
	p[9] = 6  // TCP
	s, d := src.As4(), dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	p[20], p[21] = byte(sport>>8), byte(sport)
	p[22], p[23] = byte(dport>>8), byte(dport)
	p[32] = 5 << 4 // data offset
	return p
}

// The property the outbound worker pool now depends on: every packet of one
// connection hashes to the same worker, so that connection's packets are
// processed — and therefore sealed and sent — in the order they were read.
//
// This is what stops the receiver seeing a scrambled stream. Before per-flow
// dispatch, any free worker could take any packet: measured on a live link
// that produced ~315k out-of-order segments per run and 3,210 DSACK'd
// (spurious) retransmits out of 4,186.
func TestFlowIndexPinsAConnectionToOneWorker(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	for _, workers := range []int{2, 3, 4, 7, 8, 16} {
		want := flowIndex(tcpPacket(a, b, 4001, 80), workers)
		// Same 5-tuple, many times: the index must never move. (Payload
		// length varies in reality; flowIndex only reads the headers, but
		// assert across several packets anyway.)
		for i := 0; i < 100; i++ {
			if got := flowIndex(tcpPacket(a, b, 4001, 80), workers); got != want {
				t.Fatalf("workers=%d: same flow hashed to %d then %d", workers, want, got)
			}
		}
	}
}

// Different connections must still spread, or per-flow pinning would have
// bought ordering at the cost of the parallelism the pool exists for. Eight
// concurrent streams is exactly the iperf3 -P 8 case.
func TestFlowIndexSpreadsDistinctConnections(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	const workers = 7
	seen := make(map[int]int)
	for port := uint16(4000); port < 4064; port++ {
		seen[flowIndex(tcpPacket(a, b, port, 5201), workers)]++
	}
	if len(seen) != workers {
		t.Fatalf("64 distinct connections used only %d of %d workers: %v", len(seen), workers, seen)
	}
	// No worker should be carrying a wildly disproportionate share; with 64
	// flows over 7 workers the mean is ~9, so allow generous slack but catch
	// a hash that collapses everything onto one.
	for w, n := range seen {
		if n > 32 {
			t.Fatalf("worker %d got %d of 64 flows — hash is not spreading: %v", w, n, seen)
		}
	}
}

// The index must always be a usable queue subscript. A packet flowIndex can't
// parse at all (truncated, not IP) still has to land somewhere in range rather
// than panicking the reader goroutine, since tunLoopPooled indexes a slice
// with it directly.
func TestFlowIndexAlwaysInRange(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	cases := [][]byte{
		tcpPacket(a, b, 1, 2),
		ipv4From(a, b),  // no L4 ports parseable
		{},              // empty
		{0x45},          // truncated IPv4
		{0x60, 0, 0, 0}, // truncated IPv6
		make([]byte, 20),
	}
	for _, workers := range []int{1, 2, 7, 8} {
		for i, pkt := range cases {
			got := flowIndex(pkt, workers)
			if got < 0 || got >= workers {
				t.Fatalf("case %d with workers=%d: index %d out of range", i, workers, got)
			}
		}
	}
}

// Both directions of one connection are separate flows here (the 5-tuple
// differs), which is correct and intended: this node only ever originates one
// direction on its own TUN, and pinning is per-direction.
func TestFlowIndexTreatsReverseDirectionIndependently(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	b := netip.MustParseAddr("10.0.0.2")
	const workers = 8
	fwd := flowIndex(tcpPacket(a, b, 4001, 80), workers)
	rev := flowIndex(tcpPacket(b, a, 80, 4001), workers)
	// They may or may not collide; what matters is that each is stable.
	if fwd != flowIndex(tcpPacket(a, b, 4001, 80), workers) {
		t.Fatal("forward direction is not stable")
	}
	if rev != flowIndex(tcpPacket(b, a, 80, 4001), workers) {
		t.Fatal("reverse direction is not stable")
	}
}
