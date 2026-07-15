package mesh

import (
	"net/netip"
	"testing"
)

// TestHSPayloadCarriesExtraPorts: extra TCP/UDP ports survive a handshake
// round-trip alongside the primary TCP port, and an older payload without
// them decodes to nil — same shape as TestHSPayloadCarriesTCPPort, one level
// further into the optional-field chain.
func TestHSPayloadCarriesExtraPorts(t *testing.T) {
	in := hsPayload{
		Index:         7,
		Ephemeral:     make([]byte, ephemeralLen),
		NodeID:        "n",
		Hostname:      "h",
		TCPPort:       65432,
		ExtraTCPPorts: []uint16{443, 80, 21},
		ExtraUDPPorts: []uint16{123, 443, 53},
	}
	enc := encodeHSPayload(in)
	out, err := decodeHSPayload(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.TCPPort != 65432 {
		t.Fatalf("TCPPort = %d, want 65432", out.TCPPort)
	}
	if !equalUint16(out.ExtraTCPPorts, []uint16{443, 80, 21}) {
		t.Fatalf("ExtraTCPPorts = %v, want [443 80 21]", out.ExtraTCPPorts)
	}
	if !equalUint16(out.ExtraUDPPorts, []uint16{123, 443, 53}) {
		t.Fatalf("ExtraUDPPorts = %v, want [123 443 53]", out.ExtraUDPPorts)
	}

	// Simulate an older peer that doesn't send either extra-ports field
	// (strip everything after TCPPort): the primary fields must still
	// decode correctly, with both extra lists left nil.
	tcpPortEnd := len(enc) - portListEncodedLen(in.ExtraTCPPorts) - portListEncodedLen(in.ExtraUDPPorts)
	old, err := decodeHSPayload(enc[:tcpPortEnd])
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if old.TCPPort != 65432 {
		t.Fatalf("backward-compat TCPPort = %d, want 65432", old.TCPPort)
	}
	if len(old.ExtraTCPPorts) != 0 || len(old.ExtraUDPPorts) != 0 {
		t.Fatalf("backward-compat: expected nil extra ports, got tcp=%v udp=%v", old.ExtraTCPPorts, old.ExtraUDPPorts)
	}

	// Simulate a peer that sends ExtraTCPPorts but predates ExtraUDPPorts
	// (a hypothetical intermediate version) — only cut the UDP list.
	tcpOnly := enc[:len(enc)-portListEncodedLen(in.ExtraUDPPorts)]
	mid, err := decodeHSPayload(tcpOnly)
	if err != nil {
		t.Fatalf("partial backward-compat decode failed: %v", err)
	}
	if !equalUint16(mid.ExtraTCPPorts, []uint16{443, 80, 21}) {
		t.Fatalf("partial backward-compat ExtraTCPPorts = %v, want [443 80 21]", mid.ExtraTCPPorts)
	}
	if len(mid.ExtraUDPPorts) != 0 {
		t.Fatalf("partial backward-compat: expected nil ExtraUDPPorts, got %v", mid.ExtraUDPPorts)
	}
}

// TestPeerListCarriesExtraPorts: the gossip list carries per-entry extra
// TCP/UDP ports in their own trailing blocks, after the existing TCP-port
// block, and a list without them decodes as nil — mirroring
// TestPeerListCarriesTCPPort.
func TestPeerListCarriesExtraPorts(t *testing.T) {
	in := []peerEntry{
		{nodeID: "A", hostname: "a", overlay4: netip.MustParseAddr("10.0.0.1"),
			endpoint: netip.MustParseAddrPort("198.51.100.7:65432"), tcpPort: 65432,
			extraTCPPorts: []uint16{443, 80}, extraUDPPorts: []uint16{123}},
		{nodeID: "B", hostname: "b", overlay4: netip.MustParseAddr("10.0.0.2"),
			endpoint: netip.MustParseAddrPort("198.51.100.8:65432"), tcpPort: 8443},
	}
	enc := encodePeerList(in)
	out, err := decodePeerList(enc)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(out))
	}
	if !equalUint16(out[0].extraTCPPorts, []uint16{443, 80}) {
		t.Fatalf("entry A extraTCPPorts = %v, want [443 80]", out[0].extraTCPPorts)
	}
	if !equalUint16(out[0].extraUDPPorts, []uint16{123}) {
		t.Fatalf("entry A extraUDPPorts = %v, want [123]", out[0].extraUDPPorts)
	}
	if len(out[1].extraTCPPorts) != 0 || len(out[1].extraUDPPorts) != 0 {
		t.Fatalf("entry B expected no extra ports, got tcp=%v udp=%v", out[1].extraTCPPorts, out[1].extraUDPPorts)
	}
	if out[0].tcpPort != 65432 || out[1].tcpPort != 8443 {
		t.Fatalf("tcpPort regressed: %d, %d", out[0].tcpPort, out[1].tcpPort)
	}

	// An entry list with nobody using extra ports shouldn't even encode the
	// extra blocks (see peerListHasExtraTCP/UDP) — confirm decoding it still
	// works and produces nil, not just that it's shorter.
	noExtras := []peerEntry{
		{nodeID: "C", hostname: "c", endpoint: netip.MustParseAddrPort("198.51.100.9:65432"), tcpPort: 65432},
	}
	enc2 := encodePeerList(noExtras)
	out2, err := decodePeerList(enc2)
	if err != nil {
		t.Fatal(err)
	}
	if len(out2) != 1 || out2[0].tcpPort != 65432 || len(out2[0].extraTCPPorts) != 0 {
		t.Fatalf("no-extras round trip wrong: %+v", out2)
	}

	// Simulate an older decoder/peer that only understands the original TCP
	// block: strip the extra-TCP and extra-UDP trailing blocks from the end
	// (each is 1 marker byte + a per-entry count-prefixed list), leaving the
	// count, entries, and original TCP block intact.
	extraTCPBlockLen := 1 + portListEncodedLen(in[0].extraTCPPorts) + portListEncodedLen(in[1].extraTCPPorts)
	extraUDPBlockLen := 1 + portListEncodedLen(in[0].extraUDPPorts) + portListEncodedLen(in[1].extraUDPPorts)
	old, err := decodePeerList(enc[:len(enc)-extraTCPBlockLen-extraUDPBlockLen])
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if len(old) != 2 || old[0].tcpPort != 65432 || old[1].tcpPort != 8443 {
		t.Fatalf("backward-compat tcpPort wrong: %+v", old)
	}
	if len(old[0].extraTCPPorts) != 0 {
		t.Fatalf("backward-compat: expected nil extraTCPPorts, got %v", old[0].extraTCPPorts)
	}
}

func equalUint16(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// portListEncodedLen returns how many bytes appendPortList would produce for
// a given list — used by the tests above to truncate an encoded payload at
// an exact field boundary rather than a magic offset.
func portListEncodedLen(ports []uint16) int {
	n := len(ports)
	if n > 255 {
		n = 255
	}
	return 1 + 2*n
}
