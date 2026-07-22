package mesh

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"

	"gravinet/internal/crypto"
)

func mkFrag(id uint32, idx, count byte, payload []byte) []byte {
	b := make([]byte, fragHeaderLen+len(payload))
	binary.BigEndian.PutUint32(b[0:4], id)
	b[4] = idx
	b[5] = count
	copy(b[fragHeaderLen:], payload)
	return b
}

func TestFragReasmCounters(t *testing.T) {
	const netID = uint64(0xF00D)
	eng := NewEngine(Options{
		NodeID: "self", UnderlayMTU: 1280, UnderlayMTUMax: 1450,
		Nets: []NetSpec{{ID: netID, Name: "n", Dev: newFakeDev("d"),
			Subnet4: netip.MustParsePrefix("10.0.0.0/24")}},
	})
	eng.Attach(nopSender{})
	ns := eng.network(netID)

	// Receive: a complete 2-fragment group → fragsRcvd=2, reasmOK=1.
	rx := &peerSession{nodeID: "p", net: ns}
	eng.onFragment(rx, mkFrag(1, 0, 2, bytes.Repeat([]byte{0xAA}, 100)))
	eng.onFragment(rx, mkFrag(1, 1, 2, bytes.Repeat([]byte{0xBB}, 100)))
	if rx.fragsRcvd.Load() != 2 || rx.reasmOK.Load() != 1 {
		t.Fatalf("rx counters: rcvd=%d ok=%d (want 2,1)", rx.fragsRcvd.Load(), rx.reasmOK.Load())
	}
	// An inconsistent count for an in-flight group is a drop.
	eng.onFragment(rx, mkFrag(2, 0, 2, bytes.Repeat([]byte{0x11}, 50)))
	eng.onFragment(rx, mkFrag(2, 0, 3, bytes.Repeat([]byte{0x22}, 50))) // count mismatch
	if rx.reasmDrop.Load() < 1 {
		t.Fatalf("inconsistent fragment did not count as a reasm drop: %d", rx.reasmDrop.Load())
	}

	// Send: a 3000-byte packet fragments into several datagrams → fragsSent>1.
	sess, err := crypto.NewSession(crypto.DeriveSessionKeys(bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32), []byte("t"), true))
	if err != nil {
		t.Fatal(err)
	}
	tx := &peerSession{nodeID: "q", net: ns, sess: sess, remoteIdx: 1,
		endpoint: netip.MustParseAddrPort("203.0.113.5:65432")}
	tx.initPMTU(eng.pmtuFloor, eng.pmtuCeil)
	eng.sendData(tx, make([]byte, 3000))
	if tx.fragsSent.Load() < 2 {
		t.Fatalf("expected multiple fragments sent, got %d", tx.fragsSent.Load())
	}

	// And the counters surface through ListPeers.
	ns.mu.Lock()
	ns.byNode["q"] = tx
	ns.mu.Unlock()
	found := false
	for _, pi := range eng.ListPeers(netID) {
		if pi.NodeID == "q" {
			found = true
			if pi.FragsSent != tx.fragsSent.Load() || pi.PathMTU != int(tx.effMTU.Load()) {
				t.Fatalf("ListPeers diagnostics mismatch: %+v", pi)
			}
			if pi.Transport != "udp" { // nopSender has no TCP fallback
				t.Fatalf("Transport = %q, want udp", pi.Transport)
			}
		}
	}
	if !found {
		t.Fatal("peer q not surfaced by ListPeers")
	}
}
