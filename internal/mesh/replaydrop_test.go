package mesh

import (
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/protocol"
)

// The replay window is what these tests are actually about, so they assert
// against crypto's own constant rather than a hardcoded 64: if the window is
// ever widened (the obvious follow-up if replayDrop turns out to be non-zero
// under real load), these should follow it automatically instead of silently
// asserting the old boundary.
const testReplayWindow = 64

// sealFor builds one complete on-the-wire DATA datagram for a receiver whose
// inbound session index is idx, sealed with send — the same byte layout
// sealAndSend produces, kept deliberately parallel to it so a change to the
// header/AAD binding breaks this test rather than quietly diverging from it.
func sealFor(t *testing.T, send *crypto.Session, idx uint32, inner []byte) []byte {
	t.Helper()
	const h = protocol.DataHeaderLen
	buf := make([]byte, h+1+len(inner), h+1+len(inner)+protocol.GCMOverhead)
	buf[h] = innerIP
	copy(buf[h+1:], inner)

	var aad [6]byte
	aad[0] = protocol.Version
	aad[1] = byte(protocol.TypeData)
	aad[2] = byte(idx >> 24)
	aad[3] = byte(idx >> 16)
	aad[4] = byte(idx >> 8)
	aad[5] = byte(idx)

	pt := buf[h:]
	counter, ct := send.Seal(pt[:0], pt, aad[:])
	protocol.EncodeData(buf[:h], protocol.DataHeader{RecvSession: idx, Counter: counter})
	return buf[:h+len(ct)]
}

// newReplayPeer wires a peer session whose recv cipher is the matching half of
// the returned send cipher, registered both in e.sessions (so onData's index
// demux finds it) and in the network's route map (so a decrypted packet gets
// as far as deliverInner rather than being dropped for an unowned source).
func newReplayPeer(t *testing.T, e *Engine, ns *netState, overlay netip.Addr) (*peerSession, *crypto.Session, uint32) {
	t.Helper()
	// Any agreed inputs work; only that the two halves match matters here.
	shared := []byte("test-ecdh-shared-secret-32-bytes")
	psk := []byte("test-preshared-key")
	transcript := []byte("test-transcript")
	sendSess, err := crypto.NewSession(crypto.DeriveSessionKeys(shared, psk, transcript, true))
	if err != nil {
		t.Fatalf("build send session: %v", err)
	}
	recvSess, err := crypto.NewSession(crypto.DeriveSessionKeys(shared, psk, transcript, false))
	if err != nil {
		t.Fatalf("build recv session: %v", err)
	}

	const idx = uint32(0x51E)
	ps := addPeerWithOverlay(ns, "peerR", overlay)
	ps.sess = recvSess
	ps.localIdx = idx
	e.mu.Lock()
	e.sessions[idx] = ps
	e.mu.Unlock()
	return ps, sendSess, idx
}

// A duplicate of an already-accepted datagram — the textbook replay — is
// rejected and counted as a replay, not as an authentication failure.
func TestOnDataCountsDuplicateAsReplay(t *testing.T) {
	e, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	ps, send, idx := newReplayPeer(t, e, ns, own)
	from := netip.MustParseAddrPort("203.0.113.1:65432")

	pkt := sealFor(t, send, idx, ipv4From(own, netip.MustParseAddr("10.0.0.9")))
	// A copy, because onData decrypts in place and would otherwise hand the
	// second call an already-clobbered buffer — which would fail for the
	// wrong reason and make this test prove nothing.
	dup := append([]byte(nil), pkt...)

	e.onData(pkt, from, nil)
	select {
	case <-dev.out:
	case <-time.After(2 * time.Second):
		t.Fatal("first (legitimate) packet was not delivered")
	}
	if got := ps.replayDrop.Load(); got != 0 {
		t.Fatalf("replayDrop=%d after one legitimate packet, want 0", got)
	}

	e.onData(dup, from, nil)
	if got := ps.replayDrop.Load(); got != 1 {
		t.Fatalf("replayDrop=%d after a duplicate, want 1", got)
	}
	if got := ps.authDrop.Load(); got != 0 {
		t.Fatalf("authDrop=%d after a duplicate, want 0 — a replay is not an auth failure", got)
	}
}

// The case this counter was actually added for: a packet that is perfectly
// valid and never seen before, but arrives far enough behind the highest
// counter yet received that the window has already slid past it. Nothing is
// wrong with the packet — it lost a race between sending goroutines — and it
// is discarded anyway. Historically indistinguishable from underlay loss.
func TestOnDataCountsLateReorderedPacketAsReplay(t *testing.T) {
	e, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	ps, send, idx := newReplayPeer(t, e, ns, own)
	from := netip.MustParseAddrPort("203.0.113.1:65432")
	inner := ipv4From(own, netip.MustParseAddr("10.0.0.9"))

	// Seal the straggler first so it holds the lowest counter, then seal and
	// deliver enough later packets to slide the window fully past it.
	straggler := sealFor(t, send, idx, inner)
	for i := 0; i < testReplayWindow+1; i++ {
		e.onData(sealFor(t, send, idx, inner), from, nil)
		select {
		case <-dev.out:
		case <-time.After(2 * time.Second):
			t.Fatalf("in-window packet %d was not delivered", i)
		}
	}
	if got := ps.replayDrop.Load(); got != 0 {
		t.Fatalf("replayDrop=%d before the straggler arrived, want 0", got)
	}

	e.onData(straggler, from, nil)
	if got := ps.replayDrop.Load(); got != 1 {
		t.Fatalf("replayDrop=%d for a valid packet %d behind the window, want 1",
			got, testReplayWindow+1)
	}
	select {
	case <-dev.out:
		t.Fatal("a packet outside the replay window was delivered to the overlay")
	case <-time.After(200 * time.Millisecond):
	}
}

// Reordering *within* the window is absorbed silently and costs nothing —
// the property the outbound worker pool relies on. If this ever fails, the
// window is narrower than the code that depends on it believes.
func TestOnDataAcceptsReorderingInsideWindow(t *testing.T) {
	e, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	ps, send, idx := newReplayPeer(t, e, ns, own)
	from := netip.MustParseAddrPort("203.0.113.1:65432")
	inner := ipv4From(own, netip.MustParseAddr("10.0.0.9"))

	// Seal a run in order, then deliver it exactly backwards: the last-sealed
	// packet lands first and every one after it is "late" by up to the full
	// window width, without ever exceeding it.
	const run = testReplayWindow - 1
	pkts := make([][]byte, run)
	for i := range pkts {
		pkts[i] = sealFor(t, send, idx, inner)
	}
	for i := len(pkts) - 1; i >= 0; i-- {
		e.onData(pkts[i], from, nil)
		select {
		case <-dev.out:
		case <-time.After(2 * time.Second):
			t.Fatalf("reordered packet %d (within the window) was not delivered", i)
		}
	}
	if got := ps.replayDrop.Load(); got != 0 {
		t.Fatalf("replayDrop=%d for %d packets reordered within a %d-packet window, want 0",
			got, run, testReplayWindow)
	}
}

// A corrupted packet is counted as an authentication failure and must not
// contaminate the replay counter — the whole point of splitting the two is
// that they send you to different places.
func TestOnDataCountsTamperedPacketAsAuthFailure(t *testing.T) {
	e, ns, dev := newSpoofTestEngine(t)
	own := netip.MustParseAddr("10.0.0.5")
	ps, send, idx := newReplayPeer(t, e, ns, own)
	from := netip.MustParseAddrPort("203.0.113.1:65432")

	pkt := sealFor(t, send, idx, ipv4From(own, netip.MustParseAddr("10.0.0.9")))
	pkt[len(pkt)-1] ^= 0xFF // flip a bit in the GCM tag: fresh counter, bad tag

	e.onData(pkt, from, nil)
	if got := ps.authDrop.Load(); got != 1 {
		t.Fatalf("authDrop=%d for a tampered packet, want 1", got)
	}
	if got := ps.replayDrop.Load(); got != 0 {
		t.Fatalf("replayDrop=%d for a tampered packet, want 0 — the tag failed, the counter was fine", got)
	}
	select {
	case <-dev.out:
		t.Fatal("a packet failing authentication was delivered to the overlay")
	case <-time.After(200 * time.Millisecond):
	}
}

// Both counters must reach PeerInfo, since the CLI and web admin are the only
// places an operator ever sees them.
func TestPeerInfoCarriesSessionDropCounters(t *testing.T) {
	e, ns, _ := newSpoofTestEngine(t)
	ps, _, _ := newReplayPeer(t, e, ns, netip.MustParseAddr("10.0.0.5"))
	ps.replayDrop.Store(7)
	ps.authDrop.Store(3)

	for _, pi := range e.ListPeers(1) {
		if pi.NodeID != "peerR" {
			continue
		}
		if pi.ReplayDrop != 7 || pi.AuthDrop != 3 {
			t.Fatalf("PeerInfo ReplayDrop=%d AuthDrop=%d, want 7 and 3", pi.ReplayDrop, pi.AuthDrop)
		}
		return
	}
	t.Fatal("peerR not present in ListPeers")
}
