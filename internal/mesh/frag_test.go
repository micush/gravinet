package mesh

import (
	"bytes"
	"net/netip"
	"testing"
	"time"

	"gravinet/internal/crypto"
	"gravinet/internal/protocol"
	"gravinet/internal/transport"
)

func TestComputeMaxInnerFrag(t *testing.T) {
	// Default (0) -> 1280 underlay.
	got := computeMaxInnerFrag(0)
	want := 1280 - fragOverheadV6 - (protocol.DataHeaderLen + 1 + protocol.GCMOverhead) - fragHeaderLen
	if got != want {
		t.Fatalf("default maxInnerFrag = %d, want %d", got, want)
	}
	// A sealed fragment of max size must fit within the underlay cap.
	sealed := protocol.DataHeaderLen + 1 + fragHeaderLen + got + protocol.GCMOverhead
	if sealed+fragOverheadV6 > 1280 {
		t.Fatalf("sealed fragment %d + outer overhead exceeds underlay 1280", sealed)
	}
}

// TestOverlayFragmentationRoundTrip pushes an overlay packet far larger than the
// underlay datagram cap and confirms the peer reassembles it byte-for-byte, and
// that it really was split into multiple datagrams (not relying on IP frag).
func TestOverlayFragmentationRoundTrip(t *testing.T) {
	key, _ := crypto.GenerateKey()
	const netID = uint64(0xF00DF00D)
	addrA := netip.MustParseAddr("10.55.0.1")
	addrB := netip.MustParseAddr("10.55.0.2")

	mk := func(node string, dev *fakeDev, self netip.Addr) (*Engine, *transport.Transport) {
		ks, _ := crypto.NewKeySet([]string{key})
		eng := NewEngine(Options{
			NodeID: node, Hostname: node, UnderlayMTU: 600, // tiny cap -> forces many fragments
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
	devA.mtu, devB.mtu = 9216, 9216 // jumbo tunnel, like the real default
	engA, trA := mk("A", devA, addrA)
	engB, trB := mk("B", devB, addrB)
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
	// Drain any stray writes before measuring.
	time.Sleep(100 * time.Millisecond)

	// A ~6 KB overlay packet with a position-dependent payload so a mis-ordered
	// or dropped fragment would corrupt the result.
	payload := make([]byte, 6000)
	for i := range payload {
		payload[i] = byte(i*7 + 3)
	}
	pkt := makeIPv4(addrA, addrB, payload)

	_, txBefore := trA.Stats()
	devA.in <- pkt

	select {
	case got := <-devB.out:
		if !bytes.Equal(got, pkt) {
			t.Fatalf("reassembled packet differs: got %d bytes, want %d", len(got), len(pkt))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("large packet did not traverse the tunnel (fragmentation/reassembly broken)")
	}

	_, txAfter := trA.Stats()
	if d := txAfter - txBefore; d < 5 {
		// ~6 KB over a 600-byte underlay is ~12 fragments; anything <5 means it
		// wasn't really split.
		t.Fatalf("expected the packet to be split into many datagrams; tx delta=%d", d)
	} else {
		t.Logf("packet of %d bytes sent as %d datagrams", len(pkt), d)
	}

	// Control: a small packet must NOT fragment (one datagram, allowing at most
	// one stray background control packet in the measurement window).
	_, txB2 := trA.Stats()
	small := makeIPv4(addrA, addrB, []byte("small-one"))
	devA.in <- small
	select {
	case got := <-devB.out:
		if !bytes.Equal(got, small) {
			t.Fatal("small packet corrupted")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("small packet did not arrive")
	}
	if d := mustTxDelta(trA, txB2); d > 2 {
		t.Fatalf("small packet should be a single datagram, got %d", d)
	}
}

func mustTxDelta(tr *transport.Transport, before uint64) uint64 {
	// Small settle so the tx counter reflects the send.
	time.Sleep(50 * time.Millisecond)
	_, after := tr.Stats()
	return after - before
}
