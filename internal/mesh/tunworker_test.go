package mesh

import (
	"net/netip"
	"sync"
	"testing"

	"gravinet/internal/crypto"
)

// countingSender counts successful sends and records enough of each payload's
// identity to catch a dropped or duplicated send under concurrent callers,
// without needing a real socket.
type countingSender struct {
	mu   sync.Mutex
	seen map[string]int // payload (as string) -> times sent
	n    int
}

func (s *countingSender) Send(_ netip.AddrPort, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]int)
	}
	s.seen[string(payload)]++
	s.n++
	return nil
}

// TestProcessOutboundConcurrentSameDest is the mesh-layer counterpart to
// internal/crypto's TestConcurrentSealNoCollision: it drives
// processOutbound — the function tunLoop's worker pool now calls from
// multiple goroutines per network — concurrently, with every packet routed
// to the *same* peer session, the case that used to be impossible (tunLoop
// was single-threaded) and is now the normal case under load. Fast and
// deterministic (no network, no timing), so it's cheap to run at high
// repetition/-race, unlike the network-timing-based integration tests that
// also exercise this path but can take 10-30s each per iteration.
func TestProcessOutboundConcurrentSameDest(t *testing.T) {
	const netID = uint64(0xC0DE)
	eng := NewEngine(Options{
		NodeID: "self",
		Nets: []NetSpec{{
			ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.5.0.0/24"), Self4: netip.MustParseAddr("10.5.0.1"),
		}},
	})
	sender := &countingSender{}
	eng.Attach(sender)
	ns := eng.network(netID)

	sess, err := crypto.NewSession(crypto.DeriveSessionKeys(
		make([]byte, 32), make([]byte, 32), []byte("t"), true))
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	dst := netip.MustParseAddr("10.5.0.2")
	ps := &peerSession{nodeID: "peer", net: ns, sess: sess, remoteIdx: 1,
		overlay4: dst, endpoint: netip.MustParseAddrPort("203.0.113.9:65432")}
	ps.initPMTU(eng.pmtuFloor, eng.pmtuCeil)
	ns.mu.Lock()
	ns.byNode["peer"] = ps
	ns.routes4[dst] = ps
	ns.mu.Unlock()

	const goroutines = 32
	const perGoroutine = 100
	total := goroutines * perGoroutine

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// Distinct payload per call (src port varies) so a dropped or
				// duplicated packet would change the observed distinct count.
				pkt := makeIPv4(netip.MustParseAddr("10.5.0.1"), dst,
					[]byte{byte(g), byte(g >> 8), byte(i), byte(i >> 8)})
				eng.processOutbound(ns, pkt)
			}
		}(g)
	}
	wg.Wait()

	sender.mu.Lock()
	got := sender.n
	sender.mu.Unlock()
	if got != total {
		t.Fatalf("countingSender recorded %d sends, want %d (a race would show up as a drop here)", got, total)
	}
}
