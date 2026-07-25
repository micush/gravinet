package mesh

import (
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/transport"
)

// TestTunQueueLoopDeliversFromExtraQueue proves tunQueueLoop itself — not
// just processOutbound in isolation — runs end to end: packets pushed into
// an *extra* queue (never the primary Device) still reach the peer over the
// wire. Modeled on TestTunLoopPooledDeliversAllPackets, which does the same
// proof for the outbound worker pool; this is the equivalent proof for
// multi-queue, since the two are independent mechanisms (see tunQueueLoop's
// doc comment) and neither test exercises the other's plumbing.
func TestTunQueueLoopDeliversFromExtraQueue(t *testing.T) {
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	ks, err := crypto.NewKeySet([]string{key})
	if err != nil {
		t.Fatalf("NewKeySet: %v", err)
	}
	const netID = uint64(0x00E1E) // arbitrary; "queue" pun doesn't survive hex, oh well

	mk := func(name string, self netip.Addr, extra []Queue) (*Engine, *fakeDev, *transport.Transport) {
		dev := newFakeDev(name)
		eng := NewEngine(Options{
			NodeID: name, Hostname: name,
			Nets: []NetSpec{{ID: netID, Name: "m", Keys: ks, Dev: dev, ExtraQueues: extra, Self4: self}},
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

	// A's primary device is never fed a single packet in this test — every
	// data packet goes in via extraA instead, so a passing test can only mean
	// tunQueueLoop (not tunLoop/tunLoopSerial) did the reading.
	extraA := newFakeDev("A-queue1")
	A, devA, trA := mk("A", netip.MustParseAddr("10.9.1.1"), []Queue{extraA})
	B, devB, trB := mk("B", netip.MustParseAddr("10.9.1.2"), nil)
	defer func() {
		devA.Close()
		extraA.Close()
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
		pkt := makeIPv4(netip.MustParseAddr("10.9.1.1"), netip.MustParseAddr("10.9.1.2"), payload)
		select {
		case extraA.in <- pkt:
		case <-time.After(3 * time.Second):
			t.Fatalf("extraA.in <- pkt stalled at send #%d/%d (tunQueueLoop stopped draining its queue)", i, total)
		}
	}
	t.Logf("all %d packets pushed into extraA.in (the *extra* queue, not A's primary device) successfully", total)

	var rxB uint64
	deadline = time.Now().Add(12 * time.Second)
	stable := 0
	for time.Now().Before(deadline) {
		cur, _ := trB.Stats()
		if cur == rxB {
			stable++
			if stable >= 5 {
				break
			}
		} else {
			stable = 0
		}
		rxB = cur
		time.Sleep(100 * time.Millisecond)
	}
	if rxB < uint64(total) {
		t.Fatalf("B's real transport-level rx count settled at %d, want >= %d data packets (tunQueueLoop did not deliver everything over the wire)", rxB, total)
	}
	t.Logf("B real transport rx=%d (>= %d data packets); fakeDev.out debug tap saw %d/%d (informational only)",
		rxB, total, tapped.Load(), total)

	stopped := make(chan struct{})
	go func() {
		A.Stop()
		B.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop() did not return promptly — tunQueueLoop teardown may be hanging")
	}
}

// TestAwaitQueueRebuildWakesExtraQueueReader is the narrower, non-network
// unit test for the rebuild-coordination path tunQueueLoop leans on: an
// extra-queue reader blocked in awaitQueueRebuild must wake, and see the new
// queue set, the moment rebuildOverlayDevice installs one — without ever
// calling NewDevice itself (only tunLoop, reading the primary Device, is
// allowed to do that; see dataplane.go's package doc on single-owner
// rebuilds).
func TestAwaitQueueRebuildWakesExtraQueueReader(t *testing.T) {
	d0 := newFakeDev("mq-test0")
	q0 := newFakeDev("mq-test0-q1") // doubles as a Queue: it already has Read
	d1 := newFakeDev("mq-test0")    // same name, as a real recreate would use
	q1 := newFakeDev("mq-test0-q1")

	var factoryCalls int
	e := NewEngine(Options{NodeID: "self", Nets: []NetSpec{{
		ID:          1,
		Name:        "n",
		Dev:         d0,
		ExtraQueues: []Queue{q0},
		Subnet4:     netip.MustParsePrefix("10.20.0.0/24"),
		Self4:       netip.MustParseAddr("10.20.0.5"),
		NewDevice: func() (Device, []Queue, error) {
			factoryCalls++
			return d1, []Queue{q1}, nil
		},
	}}})
	ns := e.netSnapshot()[1]

	if got := ns.queues(); len(got) != 1 || got[0] != Queue(q0) {
		t.Fatalf("precondition: extra-queue set should be the seeded one, got %v", got)
	}

	woke := make(chan bool, 1)
	go func() { woke <- e.awaitQueueRebuild(ns) }()

	// Give the waiter a moment to actually reach the select before the
	// rebuild fires, so this test can't pass by accident (rebuild happening
	// before anyone was waiting would still leave the channel closed and
	// waiter recv immediately — that's fine functionally, but give it a
	// beat regardless so a regression that made awaitQueueRebuild return
	// early for the wrong reason has a chance to show up as a timing issue
	// rather than being masked by ordering luck).
	time.Sleep(20 * time.Millisecond)

	if err := e.rebuildOverlayDevice(ns); err != nil {
		t.Fatalf("rebuildOverlayDevice: %v", err)
	}
	if factoryCalls != 1 {
		t.Fatalf("NewDevice called %d times, want exactly 1", factoryCalls)
	}

	select {
	case ok := <-woke:
		if !ok {
			t.Fatal("awaitQueueRebuild returned false (shutdown), want true (rebuild completed)")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("awaitQueueRebuild never woke after rebuildOverlayDevice succeeded")
	}

	if got := ns.queues(); len(got) != 1 || got[0] != Queue(q1) {
		t.Fatalf("post-rebuild extra-queue set should be the new one, got %v", got)
	}
	if ns.dev() != d1 {
		t.Fatal("post-rebuild live device should be the new one")
	}
}
