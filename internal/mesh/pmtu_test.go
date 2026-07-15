package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// driveSearch runs the state machine to convergence, answering each probe via
// works(size): true => the path carries that size (ack), false => it's dropped
// (no ack, so the candidate must time out after pmtuMaxTries+1 steps).
func driveSearch(p *pmtuState, works func(int) bool, maxSteps int) int {
	now := time.Now()
	var seq uint32
	next := func() uint32 { seq++; return seq }
	for i := 0; i < maxSteps; i++ {
		size, id, send := p.step(now, next)
		if send {
			if works(size) {
				p.ack(id, now)
			}
			// else: leave it awaiting; advancing `now` past the deadline on the
			// next iterations drives the timeout/retry path.
		}
		now = now.Add(pmtuProbeTimeout + time.Millisecond)
		if p.phase == phaseSettled {
			return p.eff
		}
	}
	return p.eff
}

func TestPMTUClimbsToCeiling(t *testing.T) {
	p := newPMTUState(1280, 9000, time.Now())
	// The whole range works -> discovery should reach the ceiling exactly.
	eff := driveSearch(p, func(int) bool { return true }, 200)
	if eff != 9000 {
		t.Fatalf("clean path: eff=%d, want 9000", eff)
	}
}

func TestPMTUFindsIntermediate(t *testing.T) {
	p := newPMTUState(1280, 9000, time.Now())
	// Path carries up to 1500 (e.g. typical ethernet) and drops anything bigger.
	eff := driveSearch(p, func(s int) bool { return s <= 1500 }, 200)
	if eff != 1500 {
		t.Fatalf("eth path: eff=%d, want 1500 (largest working size)", eff)
	}
}

func TestPMTUBlackHoleFallsBackToFloor(t *testing.T) {
	p := newPMTUState(1280, 9000, time.Now())
	// Nothing above the floor gets through (ICMP black hole / 5G dropping big
	// datagrams). eff must stay at the floor.
	eff := driveSearch(p, func(s int) bool { return s <= 1280 }, 200)
	if eff != 1280 {
		t.Fatalf("black hole: eff=%d, want floor 1280", eff)
	}
}

func TestPMTUDisabledWhenCeilingNotAboveFloor(t *testing.T) {
	p := newPMTUState(1280, 1280, time.Now())
	if _, _, send := p.step(time.Now(), func() uint32 { return 1 }); send {
		t.Fatal("discovery should be inert when ceil<=floor")
	}
	if p.eff != 1280 {
		t.Fatalf("eff=%d, want 1280", p.eff)
	}
}

func TestPMTUValidateDegradeReturnsToFloor(t *testing.T) {
	now := time.Now()
	var seq uint32
	next := func() uint32 { seq++; return seq }
	p := newPMTUState(1280, 4000, now)

	// Converge on a clean path first.
	if eff := driveSearch(p, func(int) bool { return true }, 200); eff != 4000 {
		t.Fatalf("initial converge eff=%d, want 4000", eff)
	}
	if p.phase != phaseSettled {
		t.Fatalf("phase=%d, want settled", p.phase)
	}

	// Jump to the revalidation time; the path has since collapsed (nothing above
	// the floor works). Validation of the current size must fail and drop eff to
	// the floor, then rediscovery confirms only the floor.
	now = p.revalAt.Add(time.Millisecond)
	for i := 0; i < 200; i++ {
		size, id, send := p.step(now, next)
		if send && size <= 1280 { // only the floor still works
			p.ack(id, now)
		}
		now = now.Add(pmtuProbeTimeout + time.Millisecond)
		if p.phase == phaseSettled {
			break
		}
	}
	if p.eff != 1280 {
		t.Fatalf("after path collapse eff=%d, want floor 1280", p.eff)
	}
}

// TestPMTULiveClimbsOverLoopback runs two real engines; loopback carries large
// datagrams, so discovery should raise the peer's effective MTU above the floor.
func TestPMTULiveClimbsOverLoopback(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0x9000F00D)
	mk := func(node string, dev *fakeDev, self netip.Addr) (*Engine, *transport.Transport) {
		ks, _ := crypto.NewKeySet([]string{key})
		eng := NewEngine(Options{
			NodeID: node, Hostname: node, UnderlayMTU: 1280, UnderlayMTUMax: 8000,
			Nets: []NetSpec{{ID: netID, Name: "t", Keys: ks, Dev: dev, Self4: self}},
		})
		tr, err := transport.Open(transport.Options{BindAddr: "127.0.0.1", PrimaryPort: 0, EnableV4: true, Workers: 1, Handler: eng.OnPacket})
		if err != nil {
			t.Fatalf("transport: %v", err)
		}
		eng.Attach(tr)
		eng.Start()
		return eng, tr
	}
	devA, devB := newFakeDev("a"), newFakeDev("b")
	devA.mtu, devB.mtu = 9216, 9216
	engA, trA := mk("A", devA, netip.MustParseAddr("10.66.0.1"))
	engB, trB := mk("B", devB, netip.MustParseAddr("10.66.0.2"))
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
	if !waitUntil(8*time.Second, func() bool { return engA.SessionCount() > 0 && engB.SessionCount() > 0 }) {
		t.Fatal("handshake did not complete")
	}

	// Find A's session to B and watch its discovered MTU climb above the floor.
	climbed := waitUntil(30*time.Second, func() bool {
		var eff int32
		engA.mu.RLock()
		for _, ps := range engA.sessions {
			if v := ps.effMTU.Load(); v > eff {
				eff = v
			}
		}
		engA.mu.RUnlock()
		return eff >= 8000 // loopback carries everything; should reach the ceiling
	})
	if !climbed {
		var eff int32
		engA.mu.RLock()
		for _, ps := range engA.sessions {
			if v := ps.effMTU.Load(); v > eff {
				eff = v
			}
		}
		engA.mu.RUnlock()
		t.Fatalf("path MTU did not climb to the ceiling over loopback; eff=%d", eff)
	}

	// With discovery active and the MTU climbed, a large overlay packet must still
	// traverse the tunnel intact (it now rides as fewer/no fragments).
	for len(devB.out) > 0 {
		<-devB.out
	}
	payload := make([]byte, 5000)
	for i := range payload {
		payload[i] = byte(i*5 + 1)
	}
	pkt := makeIPv4(netip.MustParseAddr("10.66.0.1"), netip.MustParseAddr("10.66.0.2"), payload)
	devA.in <- pkt
	select {
	case got := <-devB.out:
		if len(got) != len(pkt) {
			t.Fatalf("post-discovery packet truncated: got %d, want %d", len(got), len(pkt))
		}
		for i := range got {
			if got[i] != pkt[i] {
				t.Fatalf("post-discovery packet corrupted at byte %d", i)
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatal("large packet did not traverse the tunnel after discovery")
	}
}
