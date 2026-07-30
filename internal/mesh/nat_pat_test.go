package mesh

import (
	"net/netip"
	"testing"
)

// TestNATRuleSpecToRulePAT covers toRule()'s parsing of the PAT-specific
// fields: DestPort/Proto (matching) and the optional ":port" suffix on a
// port-forward Translate (remapping) — the extension to
// TestNATRuleSpecToRuleDetectsMode's coverage that came with PAT.
func TestNATRuleSpecToRulePAT(t *testing.T) {
	cases := []struct {
		name           string
		spec           NATRuleSpec
		wantOK         bool
		wantLo, wantHi uint16
		wantToPort     uint16
		wantProto      uint8
	}{
		{"single port, no remap", NATRuleSpec{Translate: "port-forward:10.0.0.5", DestPort: "32400", Proto: "tcp"}, true, 32400, 32400, 0, 6},
		{"range, no remap", NATRuleSpec{Translate: "port-forward:10.0.0.5", DestPort: "8000-8010", Proto: "udp"}, true, 8000, 8010, 0, 17},
		{"single port with remap", NATRuleSpec{Translate: "port-forward:10.0.0.5:443", DestPort: "8443", Proto: "tcp"}, true, 8443, 8443, 443, 6},
		{"no dest-port at all: address-only, unchanged from pre-PAT", NATRuleSpec{Translate: "port-forward:10.0.0.5"}, true, 0, 0, 0, 0},
		{"proto only, no dest-port", NATRuleSpec{Translate: "port-forward:10.0.0.5", Proto: "tcp"}, true, 0, 0, 0, 6},
		{"bad dest-port", NATRuleSpec{Translate: "port-forward:10.0.0.5", DestPort: "not-a-port", Proto: "tcp"}, false, 0, 0, 0, 0},
		{"bad remap port", NATRuleSpec{Translate: "port-forward:10.0.0.5:notaport"}, false, 0, 0, 0, 0},
		{"remap port out of range", NATRuleSpec{Translate: "port-forward:10.0.0.5:99999"}, false, 0, 0, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, ok := c.spec.toRule()
			if ok != c.wantOK {
				t.Fatalf("toRule() ok = %v, want %v (rule=%+v)", ok, c.wantOK, r)
			}
			if !ok {
				return
			}
			if r.dportLo != c.wantLo || r.dportHi != c.wantHi {
				t.Errorf("dportLo/Hi = %d/%d, want %d/%d", r.dportLo, r.dportHi, c.wantLo, c.wantHi)
			}
			if r.toPort != c.wantToPort {
				t.Errorf("toPort = %d, want %d", r.toPort, c.wantToPort)
			}
			if r.proto != c.wantProto {
				t.Errorf("proto = %d, want %d", r.proto, c.wantProto)
			}
		})
	}
}

// TestDNATPortMatchOnly checks a DNAT rule scoped to a single dest-port only
// rewrites packets addressed to that port — a packet to a different port on
// the same matched address must pass through completely untouched (no rule
// in the table matches, so translateIn is a no-op for it). This is the
// behavior the whole feature exists for: a seed's public IP can host other
// services alongside one forwarded port.
func TestDNATPortMatchOnly(t *testing.T) {
	a := netip.MustParseAddr
	nt := newNATTable([]natRule{{
		action: dnatAction, dst: netip.MustParsePrefix("203.0.113.5/32"),
		proto: 17, dportLo: 32400, dportHi: 32400, to: a("10.0.0.5"),
	}}, 0)

	// Matching port: rewritten.
	match := makeUDP(a("198.51.100.1"), a("203.0.113.5"), 5000, 32400, []byte("hit"))
	nt.translateIn(match)
	if _, _, _, dst, _, dport, _ := ipv4Fields(match); dst != a("10.0.0.5") || dport != 32400 {
		t.Fatalf("matching port should be DNAT'd: dst=%s dport=%d", dst, dport)
	}
	if !ipValid(match) || !udpValid(match) {
		t.Fatal("checksums invalid after DNAT")
	}

	// Different port, same address: must pass through untouched.
	other := makeUDP(a("198.51.100.1"), a("203.0.113.5"), 5000, 22, []byte("ssh"))
	before := append([]byte(nil), other...)
	nt.translateIn(other)
	if _, _, _, dst, _, dport, _ := ipv4Fields(other); dst != a("203.0.113.5") || dport != 22 {
		t.Fatalf("non-matching port should be left alone: dst=%s dport=%d", dst, dport)
	}
	for i := range other {
		if other[i] != before[i] {
			t.Fatal("packet bytes changed even though no rule matched")
		}
	}
}

// TestDNATPortRangeMatch checks a range (DestPort "8000-8010") matches every
// port within it and nothing outside it.
func TestDNATPortRangeMatch(t *testing.T) {
	a := netip.MustParseAddr
	nt := newNATTable([]natRule{{
		action: dnatAction, dst: netip.MustParsePrefix("203.0.113.5/32"),
		proto: 17, dportLo: 8000, dportHi: 8010, to: a("10.0.0.5"),
	}}, 0)

	for _, p := range []uint16{8000, 8005, 8010} {
		pkt := makeUDP(a("198.51.100.1"), a("203.0.113.5"), 5000, p, nil)
		nt.translateIn(pkt)
		if _, _, _, dst, _, _, _ := ipv4Fields(pkt); dst != a("10.0.0.5") {
			t.Errorf("port %d should be inside the range and DNAT'd, dst=%s", p, dst)
		}
	}
	for _, p := range []uint16{7999, 8011} {
		pkt := makeUDP(a("198.51.100.1"), a("203.0.113.5"), 5000, p, nil)
		nt.translateIn(pkt)
		if _, _, _, dst, _, _, _ := ipv4Fields(pkt); dst != a("203.0.113.5") {
			t.Errorf("port %d should be outside the range and untouched, dst=%s", p, dst)
		}
	}
}

// TestDNATPortRemapRoundTrip is the full PAT round trip: a packet arrives
// for the external port (8443), gets DNAT'd to the internal host on the
// *remapped* port (443); the internal host's reply (sourced from 443) must
// have its source rewritten back to look like it came from the original
// external address and port (8443), not the internal ones — otherwise the
// original sender would see a reply from a port it never talked to and
// silently drop it as unsolicited.
func TestDNATPortRemapRoundTrip(t *testing.T) {
	a := netip.MustParseAddr
	nt := newNATTable([]natRule{{
		action: dnatAction, dst: netip.MustParsePrefix("203.0.113.5/32"),
		proto: 6, dportLo: 8443, dportHi: 8443, toPort: 443, to: a("10.0.0.5"),
	}}, 0)

	req := makeTCP(a("198.51.100.1"), a("203.0.113.5"), 40000, 8443)
	nt.translateIn(req)
	_, _, _, dst, _, dport, _ := ipv4Fields(req)
	if dst != a("10.0.0.5") || dport != 443 {
		t.Fatalf("expected remap to 10.0.0.5:443, got dst=%s dport=%d", dst, dport)
	}

	// Internal host replies from its real address:port (443) back to the
	// original sender. The reply's source must be rewritten to look like
	// it came from the externally-visible 203.0.113.5:8443, not
	// 10.0.0.5:443 — the original sender only ever talked to the former.
	reply := makeTCP(a("10.0.0.5"), a("198.51.100.1"), 443, 40000)
	nt.translateOut(reply)
	_, _, src, _, sport, _, _ := ipv4Fields(reply)
	if src != a("203.0.113.5") || sport != 8443 {
		t.Fatalf("expected reply source rewritten to 203.0.113.5:8443, got src=%s sport=%d", src, sport)
	}
	if !ipValid(reply) || !tcpValid(reply) {
		t.Fatal("checksums invalid after reverse DNAT with remap")
	}
}

// makeTCP builds a minimal (header-only, no payload/flags-don't-matter-here)
// IPv4/TCP packet with valid checksums, the TCP analog of nat_test.go's
// makeUDP — needed here since PAT's most realistic use (forwarding a
// specific application port) is almost always TCP, and proto must be an
// exact match (6, not 17) for these tests to exercise the right code path.
func makeTCP(src, dst netip.Addr, sport, dport uint16) []byte {
	ihl := 20
	p := make([]byte, ihl+20) // bare TCP header, no options/payload
	p[0] = 0x45
	total := uint16(len(p))
	p[2], p[3] = byte(total>>8), byte(total)
	p[8] = 64
	p[9] = 6 // TCP
	s, d := src.As4(), dst.As4()
	copy(p[12:16], s[:])
	copy(p[16:20], d[:])
	p[ihl], p[ihl+1] = byte(sport>>8), byte(sport)
	p[ihl+2], p[ihl+3] = byte(dport>>8), byte(dport)
	p[ihl+12] = 5 << 4 // data offset: 5 words (20 bytes), no options
	fixChecksums(p, ihl)
	return p
}

// tcpValid mirrors nat_test.go's udpValid for TCP's checksum offset (16,
// not UDP's 6).
func tcpValid(pkt []byte) bool {
	ihl := int(pkt[0]&0x0f) * 4
	l4 := pkt[ihl:]
	var pseudo uint32
	for i := 12; i < 20; i += 2 {
		pseudo += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	pseudo += uint32(pkt[9]) + uint32(len(l4))
	return ones(l4, pseudo) == 0
}
