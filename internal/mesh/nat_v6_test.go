package mesh

import (
	"net/netip"
	"testing"
)

func a6(s string) netip.Addr { return netip.MustParseAddr(s) }

// udp6Packet builds an IPv6/UDP packet with a correct checksum.
func udp6Packet(src, dst netip.Addr, sport, dport uint16, payload []byte) []byte {
	const fixed = 40
	p := make([]byte, fixed+8+len(payload))
	p[0] = 6 << 4
	l4len := 8 + len(payload)
	p[4], p[5] = byte(l4len>>8), byte(l4len)
	p[6] = protoUDP
	p[7] = 64 // hop limit
	s, d := src.As16(), dst.As16()
	copy(p[8:24], s[:])
	copy(p[24:40], d[:])
	p[fixed], p[fixed+1] = byte(sport>>8), byte(sport)
	p[fixed+2], p[fixed+3] = byte(dport>>8), byte(dport)
	p[fixed+4], p[fixed+5] = byte(l4len>>8), byte(l4len)
	copy(p[fixed+8:], payload)
	h, ok := ipFields(p)
	if !ok {
		panic("built an unparseable v6 packet")
	}
	fixChecksums(p, h)
	return p
}

// icmp6Packet builds an IPv6/ICMPv6 echo request with a correct checksum.
func icmp6Packet(src, dst netip.Addr) []byte {
	const fixed = 40
	p := make([]byte, fixed+8)
	p[0] = 6 << 4
	p[4], p[5] = 0, 8
	p[6] = protoICMPv6
	p[7] = 64
	s, d := src.As16(), dst.As16()
	copy(p[8:24], s[:])
	copy(p[24:40], d[:])
	p[fixed] = 128 // echo request
	h, _ := ipFields(p)
	fixChecksums(p, h)
	return p
}

// l4ChecksumValid recomputes the upper-layer checksum over the packet as it
// stands. A correct checksum sums to zero when the stored value is included,
// so this is what a receiving stack does before accepting the packet.
func l4ChecksumValid(pkt []byte) bool {
	h, ok := ipFields(pkt)
	if !ok {
		return false
	}
	switch h.proto {
	case protoTCP, protoUDP, protoICMPv6:
	default:
		return true
	}
	l4 := pkt[h.l4off:]
	return ones(l4, pseudoSum(pkt, h, len(l4))) == 0
}

func v6HeaderIntact(pkt []byte) bool {
	return len(pkt) >= 40 && pkt[0]>>4 == 6
}

// The operator's case, on the overlay path: masquerade an IPv6 source. Before
// this, toRule rejected any non-IPv4 target outright, so a v6 rule produced no
// natRule at all and every v6 packet passed through untouched.
func TestIPv6SNATRewritesAndRestores(t *testing.T) {
	nt := newNATTable([]natRule{{
		action: snatAction, is6: true,
		src: netip.MustParsePrefix("fd00:203::/64"),
		to:  a6("2001:db8::1"),
	}}, 0)

	out := udp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"), 4321, 53, []byte("query"))
	nt.translateOut(out)

	h, ok := ipFields(out)
	if !ok {
		t.Fatal("packet stopped parsing after translation")
	}
	if h.src != a6("2001:db8::1") {
		t.Fatalf("source not masqueraded: got %v", h.src)
	}
	if !l4ChecksumValid(out) {
		t.Fatal("UDP checksum invalid after v6 SNAT — the receiver would discard this silently")
	}
	if !v6HeaderIntact(out) {
		t.Fatal("IPv6 header damaged by the rewrite")
	}

	// The reply comes back to the translated address and must be restored.
	reply := udp6Packet(a6("2001:db8:9::9"), h.src, 53, h.sport, []byte("answer"))
	nt.translateIn(reply)
	rh, _ := ipFields(reply)
	if rh.dst != a6("fd00:203::5") || rh.dport != 4321 {
		t.Fatalf("reply not reverse-translated: dst=%v dport=%d", rh.dst, rh.dport)
	}
	if !l4ChecksumValid(reply) {
		t.Fatal("UDP checksum invalid after reverse translation")
	}
}

func TestIPv6DNATRewritesAndRestores(t *testing.T) {
	nt := newNATTable([]natRule{{
		action: dnatAction, is6: true,
		dst: netip.MustParsePrefix("2001:db8::1/128"),
		to:  a6("fd00:203::5"),
	}}, 0)

	in := udp6Packet(a6("2001:db8:9::9"), a6("2001:db8::1"), 1234, 8080, []byte("req"))
	nt.translateIn(in)
	h, _ := ipFields(in)
	if h.dst != a6("fd00:203::5") {
		t.Fatalf("destination not translated: %v", h.dst)
	}
	if !l4ChecksumValid(in) {
		t.Fatal("UDP checksum invalid after v6 DNAT")
	}

	reply := udp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"), 8080, 1234, []byte("resp"))
	nt.translateOut(reply)
	rh, _ := ipFields(reply)
	if rh.src != a6("2001:db8::1") {
		t.Fatalf("reply source not restored: %v", rh.src)
	}
	if !l4ChecksumValid(reply) {
		t.Fatal("UDP checksum invalid after reverse DNAT")
	}
}

// ICMPv6 is the case an IPv4-shaped implementation gets wrong. ICMPv4's
// checksum spans only the ICMP message and survives an address rewrite;
// ICMPv6's covers the pseudo-header, so it must be recomputed. Skipping it
// produces packets every receiver drops while both ends look correct.
func TestICMPv6ChecksumRecomputedAfterTranslation(t *testing.T) {
	nt := newNATTable([]natRule{{
		action: snatAction, is6: true,
		src: netip.MustParsePrefix("fd00:203::/64"),
		to:  a6("2001:db8::1"),
	}}, 0)

	pkt := icmp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"))
	if !l4ChecksumValid(pkt) {
		t.Fatal("test packet built with a bad checksum")
	}
	nt.translateOut(pkt)

	h, _ := ipFields(pkt)
	if h.src != a6("2001:db8::1") {
		t.Fatalf("ICMPv6 source not translated: %v", h.src)
	}
	if !l4ChecksumValid(pkt) {
		t.Fatal("ICMPv6 checksum not recomputed after the address changed")
	}
}

// A rule matches only its own family. "any" prefixes are invalid rather than
// family-specific, so without the is6 gate a v4 rule with no source would
// claim v6 packets and rewrite them with As4 garbage.
func TestRuleFamilyGating(t *testing.T) {
	a := netip.MustParseAddr
	v4Only := newNATTable([]natRule{{action: snatAction, to: a("10.0.0.1")}}, 0)
	pkt := udp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"), 1111, 53, []byte("x"))
	before := append([]byte(nil), pkt...)
	v4Only.translateOut(pkt)
	if string(pkt) != string(before) {
		t.Fatal("an IPv4 rule translated an IPv6 packet")
	}

	v6Only := newNATTable([]natRule{{action: snatAction, is6: true, to: a6("2001:db8::1")}}, 0)
	p4 := makeUDP(a("192.168.1.5"), a("203.0.113.9"), 1111, 53, []byte("x"))
	before4 := append([]byte(nil), p4...)
	v6Only.translateOut(p4)
	if string(p4) != string(before4) {
		t.Fatal("an IPv6 rule translated an IPv4 packet")
	}
}

// Extension headers sit between the fixed header and the upper-layer header,
// so the L4 offset has to be walked to rather than assumed to be 40.
func TestIPv6ExtensionHeaderChainWalked(t *testing.T) {
	base := udp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"), 4321, 53, []byte("q"))
	// Splice a destination-options header (type 60, 8 bytes) in front of UDP.
	ext := make([]byte, 0, len(base)+8)
	ext = append(ext, base[:40]...)
	ext = append(ext, protoUDP, 0, 1, 4, 0, 0, 0, 0) // next=UDP, len=0 => 8 bytes
	ext = append(ext, base[40:]...)
	ext[6] = 60 // fixed header now points at destination options

	h, ok := ipFields(ext)
	if !ok {
		t.Fatal("extension-header chain not parsed")
	}
	if h.l4off != 48 {
		t.Fatalf("l4off = %d, want 48 (40 fixed + 8 ext)", h.l4off)
	}
	if h.proto != protoUDP || h.sport != 4321 || h.dport != 53 {
		t.Fatalf("upper layer misread: proto=%d sport=%d dport=%d", h.proto, h.sport, h.dport)
	}
}

// AH authenticates the very addresses NAT rewrites, and ESP leaves no
// locatable checksum. Both must pass through untranslated rather than be
// corrupted.
func TestIPv6AHAndESPRefused(t *testing.T) {
	for _, nh := range []byte{51, 50, 59} {
		pkt := udp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"), 1, 2, []byte("x"))
		pkt[6] = nh
		if _, ok := ipFields(pkt); ok {
			t.Errorf("next-header %d parsed; it must be refused so the packet is left alone", nh)
		}
	}
}

// A later fragment carries no ports — those bytes are payload. Reading them
// invents a flow that never existed.
func TestLaterFragmentPortsNotInvented(t *testing.T) {
	base := udp6Packet(a6("fd00:203::5"), a6("2001:db8:9::9"), 4321, 53, []byte("q"))
	frag := make([]byte, 0, len(base)+8)
	frag = append(frag, base[:40]...)
	// Fragment header (type 44): next=UDP, offset 8 octets (nonzero => later).
	frag = append(frag, protoUDP, 0, 0, 8, 0, 0, 0, 1)
	frag = append(frag, base[40:]...)
	frag[6] = 44

	h, ok := ipFields(frag)
	if !ok {
		t.Fatal("fragment chain not parsed")
	}
	if h.sport != 0 || h.dport != 0 {
		t.Fatalf("ports read from a later fragment: sport=%d dport=%d", h.sport, h.dport)
	}
}

// The stored config form has to survive the round trip into a runtime rule,
// brackets and all — this is the path that a first-colon split silently broke.
func TestIPv6PortForwardSpecBecomesRule(t *testing.T) {
	r, ok := NATRuleSpec{
		Dest:      "2001:db8::1/128",
		DestPort:  "8443",
		Proto:     "tcp",
		Translate: "port-forward:[fd00:203::5]:443",
	}.toRule()
	if !ok {
		t.Fatal("IPv6 port-forward spec did not become a rule")
	}
	if !r.is6 || r.to != a6("fd00:203::5") || r.toPort != 443 || r.action != dnatAction {
		t.Fatalf("rule wrong: %+v", r)
	}
}

// A hand-edited config could still name a v4 prefix on a v6 rule. The translate
// paths dispatch on is6 alone, so such a rule would never match anything —
// refuse it at load instead of installing something inert.
func TestMixedFamilySpecRefusedAtLoad(t *testing.T) {
	if _, ok := (NATRuleSpec{Source: "192.168.1.0/24", Translate: "fd00:203::1"}).toRule(); ok {
		t.Error("v4 source with v6 target accepted; the rule could never match")
	}
	if _, ok := (NATRuleSpec{Source: "fd00:203::/64", Translate: "10.0.0.1"}).toRule(); ok {
		t.Error("v6 source with v4 target accepted")
	}
}
