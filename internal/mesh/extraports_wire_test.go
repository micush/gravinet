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
	// Every field encodeHSPayload writes after the two extra-port lists has
	// to come off too, or this "truncate back to TCPPort" offset lands
	// somewhere inside the extra-TCP list instead — see
	// TestHSPayloadCarriesTCPPort's comment on the same hazard.
	const bgpASNFieldLen = 4
	afterExtras := endpointListEncodedLen(in.LocalEndpoints) + bgpASNFieldLen + lenStrEncodedLen(in.Version)
	tcpPortEnd := len(enc) - afterExtras -
		portListEncodedLen(in.ExtraTCPPorts) - portListEncodedLen(in.ExtraUDPPorts)
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
	tcpOnly := enc[:len(enc)-afterExtras-portListEncodedLen(in.ExtraUDPPorts)]
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

// lenStrEncodedLen returns how many bytes appendLenStr would produce for a
// given string — 1 length byte plus the (255-capped) body. Same reasoning as
// portListEncodedLen/endpointListEncodedLen: hsPayload.Version is a trailing
// optional field, so a truncation test has to cut at its exact boundary
// rather than a magic offset.
func lenStrEncodedLen(s string) int {
	n := len(s)
	if n > 255 {
		n = 255
	}
	return 1 + n
}

// endpointListEncodedLen returns how many bytes appendEndpointList would
// produce for a given list — appendEndpoint's own per-entry shape
// (1 family byte + 4 or 16 address bytes + 2 port bytes), same reasoning as
// portListEncodedLen: an exact field boundary for truncation tests, not a
// magic offset that silently drifts wrong the next time a trailing field is
// added after it (see TestHSPayloadCarriesTCPPort).
func endpointListEncodedLen(eps []netip.AddrPort) int {
	n := len(eps)
	if n > maxLocalEndpoints {
		n = maxLocalEndpoints
	}
	total := 1
	for _, ep := range eps[:n] {
		if ep.Addr().Unmap().Is6() {
			total += 1 + 16 + 2
		} else {
			total += 1 + 4 + 2
		}
	}
	return total
}

// TestHSPayloadCarriesVersion: the build version survives a handshake
// round-trip, and a payload from a peer that predates the field decodes to ""
// — one level further into the optional-field chain than BGPASN, same shape
// as TestHSPayloadCarriesExtraPorts.
func TestHSPayloadCarriesVersion(t *testing.T) {
	in := hsPayload{
		Index:     7,
		Ephemeral: make([]byte, ephemeralLen),
		NodeID:    "n",
		Hostname:  "h",
		TCPPort:   65432,
		BGPASN:    4200000042,
		Version:   "587",
	}
	enc := encodeHSPayload(in)
	out, err := decodeHSPayload(enc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Version != "587" {
		t.Fatalf("Version = %q, want \"587\"", out.Version)
	}
	if out.BGPASN != 4200000042 {
		t.Fatalf("BGPASN = %d, want 4200000042 (version field must not disturb it)", out.BGPASN)
	}

	// A peer predating the version field sends everything up to BGPASN and
	// stops: Version must come back empty, and BGPASN must still decode.
	old, err := decodeHSPayload(enc[:len(enc)-lenStrEncodedLen(in.Version)])
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if old.Version != "" {
		t.Fatalf("backward-compat Version = %q, want empty", old.Version)
	}
	if old.BGPASN != 4200000042 {
		t.Fatalf("backward-compat BGPASN = %d, want 4200000042", old.BGPASN)
	}
}

// TestPeerListCarriesVersion: the gossip list carries per-entry build
// versions in their own trailing block, a list whose entries have none omits
// the block entirely, and an older decoder that stops before it still reads
// every earlier field — mirroring TestPeerListCarriesExtraPorts.
func TestPeerListCarriesVersion(t *testing.T) {
	in := []peerEntry{
		{nodeID: "A", hostname: "a", overlay4: netip.MustParseAddr("10.0.0.1"),
			endpoint: netip.MustParseAddrPort("198.51.100.7:65432"), tcpPort: 65432, version: "587"},
		{nodeID: "B", hostname: "b", overlay4: netip.MustParseAddr("10.0.0.2"),
			endpoint: netip.MustParseAddrPort("198.51.100.8:65432"), tcpPort: 8443, version: "571"},
	}
	out, err := decodePeerList(encodePeerList(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].version != "587" || out[1].version != "571" {
		t.Fatalf("versions not carried: %+v", out)
	}

	// Entries with no version at all: the block is not emitted, so the
	// encoding is byte-identical to what it would have been before the
	// field existed (a mesh of older peers costs nothing).
	none := []peerEntry{
		{nodeID: "A", hostname: "a", endpoint: netip.MustParseAddrPort("198.51.100.7:1"), tcpPort: 1},
	}
	withVer := []peerEntry{
		{nodeID: "A", hostname: "a", endpoint: netip.MustParseAddrPort("198.51.100.7:1"), tcpPort: 1, version: "587"},
	}
	encNone, encVer := encodePeerList(none), encodePeerList(withVer)
	if len(encNone) != len(encVer)-lenStrEncodedLen("587")-1 { // -1 for the block marker
		t.Fatalf("empty-version encoding should omit the block: %d vs %d", len(encNone), len(encVer))
	}
	backNone, err := decodePeerList(encNone)
	if err != nil {
		t.Fatal(err)
	}
	if len(backNone) != 1 || backNone[0].version != "" || backNone[0].tcpPort != 1 {
		t.Fatalf("no-version round-trip wrong: %+v", backNone)
	}

	// An older decoder stops at the unrecognized version block; everything
	// before it must still have been read.
	trimmed := encVer[:len(encVer)-lenStrEncodedLen("587")-1]
	backOld, err := decodePeerList(trimmed)
	if err != nil {
		t.Fatalf("backward-compat decode failed: %v", err)
	}
	if len(backOld) != 1 || backOld[0].version != "" || backOld[0].tcpPort != 1 {
		t.Fatalf("backward-compat wrong: %+v", backOld)
	}
}
