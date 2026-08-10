package mesh

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"gravinet/internal/config"
)

// interfaceAddr returns the first usable address of the requested family on
// the named interface, used as the masquerade source address.
//
// For IPv6 "usable" excludes link-local as well as loopback: fe80::/10 is not
// routable off-link, so masquerading to it produces a source address the
// far end cannot reply to. IPv4 has no equivalent exclusion here — a
// 169.254/16 address is a misconfiguration rather than a normal coexisting
// address the way fe80:: is.
func interfaceAddr(name string, want6 bool) (netip.Addr, bool) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Addr{}, false
	}
	addrs, err := ifc.Addrs()
	if err != nil {
		return netip.Addr{}, false
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		na, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		na = na.Unmap()
		if na.Is4() == want6 {
			continue
		}
		if na.IsLoopback() {
			continue
		}
		if want6 && na.IsLinkLocalUnicast() {
			continue
		}
		return na, true
	}
	return netip.Addr{}, false
}

// NAT translates addresses on the overlay data path with connection tracking so
// replies are reverse-translated automatically.
//
//   - SNAT (masquerade): rewrite the source of egress packets to a single
//     address, with port translation so many internal hosts share it. Replies
//     arriving on ingress have their destination restored.
//   - DNAT (port-forward): rewrite the destination of ingress packets to an
//     internal host. Replies leaving on egress have their source restored.
//
// Which of the two a rule runs is carried by Translate itself (see
// NATRuleSpec's doc comment) rather than a separate direction/mode field —
// earlier versions had one, but it only ever expressed these same two
// behaviors under three confusingly-overlapping labels (one of the three
// was fully unimplemented dead weight), and Translate already has to name
// the rewrite target regardless, so folding the choice in there removes a
// whole field instead of just relabeling it.
//
// Both address families are translated. A rule's family comes from its
// translation target and it only ever matches packets of that family, so a
// dual-stack overlay needs one rule per family (config enforces that a single
// rule cannot mix them).
//
// What differs between the two is only what has to be recomputed afterwards:
// IPv4 has a header checksum and IPv6 has none, and ICMPv6's checksum covers
// the pseudo-header where ICMPv4's does not. See fixChecksums. Other L4
// protocols are translated at the IP layer only (ICMP is tracked by address
// pair). IPv6 packets carrying AH, ESP, or an unterminated extension-header
// chain are passed through untranslated — see ipv6Fields.
//
// This is the overlay path: it translates between overlay peers. The kernel
// path (internal/netfilter) translates overlay traffic being forwarded out a
// physical interface. Both consume the same configured rule list, from
// different ends, and both now handle IPv4 and IPv6.

type natAction uint8

const (
	snatAction natAction = iota
	dnatAction
)

type natRule struct {
	action natAction
	// is6 is the rule's address family, taken from its translation target.
	// A rule only ever applies to packets of its own family: an "any" source
	// or dest prefix is invalid rather than family-specific, so prefixMatch
	// alone would let a v4 rule claim v6 packets and rewrite their addresses
	// with As4 garbage.
	is6 bool
	src netip.Prefix // match source (invalid = any)
	dst netip.Prefix // match dest (invalid = any)
	// srcNeg/dstNeg invert their prefix's match — the rule fires for
	// everything OUTSIDE the prefix. Mirrors fwRule.srcNegate/dstNegate.
	// Only meaningful alongside a valid prefix; an invalid (any) prefix
	// with negation on would match nothing at all, which config.Validate
	// rejects rather than letting a rule silently never fire.
	srcNeg, dstNeg bool
	proto          uint8      // 0 = any
	to             netip.Addr // translation target
	// dportLo/dportHi scope a DNAT rule to a specific destination port or
	// range on the original (pre-translation) packet; 0,0 = any port.
	// Unused for SNAT rules (see config.NATRule.DestPort's doc comment for
	// why a rewritten *source* port has no equivalent match).
	dportLo, dportHi uint16
	// toPort, if nonzero, remaps the destination port to this fixed value
	// on a matched DNAT packet instead of preserving the original port —
	// port address translation (PAT). Only ever set alongside dportLo ==
	// dportHi (a single matched port); see buildNATRule/toRule for why a
	// range can't remap this way.
	toPort uint16
}

type natKey struct {
	proto uint8
	sip   netip.Addr
	dip   netip.Addr
	sport uint16
	dport uint16
}

type natConn struct {
	oSrc, oDst     netip.Addr
	oSport, oDport uint16
	tSrc, tDst     netip.Addr
	tSport, tDport uint16
	proto          uint8
	lastSeen       time.Time
}

type natTable struct {
	mu       sync.Mutex
	snat     []natRule
	dnat     []natRule
	snatFwd  map[natKey]*natConn
	snatRev  map[natKey]*natConn
	dnatFwd  map[natKey]*natConn
	dnatRev  map[natKey]*natConn
	nextPort uint16
	ttl      time.Duration // idle lifetime of a tracked connection ("state")
}

const natConnTTL = 120 * time.Second

func newNATTable(rules []natRule, ttl time.Duration) *natTable {
	if ttl <= 0 {
		ttl = natConnTTL
	}
	t := &natTable{
		snatFwd:  map[natKey]*natConn{},
		snatRev:  map[natKey]*natConn{},
		dnatFwd:  map[natKey]*natConn{},
		dnatRev:  map[natKey]*natConn{},
		nextPort: 20000,
		ttl:      ttl,
	}
	for _, r := range rules {
		if r.action == snatAction {
			t.snat = append(t.snat, r)
		} else {
			t.dnat = append(t.dnat, r)
		}
	}
	return t
}

// ---- packet field helpers (IPv4) ----

// Upper-layer protocol numbers this file cares about.
const (
	protoTCP    uint8 = 6
	protoUDP    uint8 = 17
	protoICMPv6 uint8 = 58
)

// ipHdr is one packet's parsed addressing: enough to match it against a rule
// and to rewrite it afterwards, for either address family.
//
// l4off replaces the IPv4-only "ihl" this used to pass around. For IPv4 the
// two are the same number, but for IPv6 the upper-layer header sits after a
// fixed 40-byte header plus however many extension headers the sender chose
// to chain, so the offset has to be walked to rather than computed.
type ipHdr struct {
	is6      bool
	l4off    int   // offset of the upper-layer header
	proto    uint8 // upper-layer protocol (after any IPv6 extension headers)
	src, dst netip.Addr
	sport    uint16 // 0 when not TCP/UDP, or not in this fragment
	dport    uint16
}

// ipFields parses an IPv4 or IPv6 packet's addressing. ok is false for
// anything this file must not rewrite — a truncated header, an unknown
// version, or an IPv6 chain it cannot see through (see ipv6Fields).
func ipFields(pkt []byte) (ipHdr, bool) {
	if len(pkt) < 20 {
		return ipHdr{}, false
	}
	switch pkt[0] >> 4 {
	case 4:
		return ipv4Fields2(pkt)
	case 6:
		return ipv6Fields(pkt)
	}
	return ipHdr{}, false
}

func ipv4Fields2(pkt []byte) (ipHdr, bool) {
	ihl := int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return ipHdr{}, false
	}
	var s, d [4]byte
	copy(s[:], pkt[12:16])
	copy(d[:], pkt[16:20])
	h := ipHdr{l4off: ihl, proto: pkt[9], src: netip.AddrFrom4(s), dst: netip.AddrFrom4(d)}
	// Ports exist only in the first fragment; in a later one those bytes are
	// payload that happens to sit where a port would be. Reading them anyway
	// invents a flow that was never there.
	if fragOff := (uint16(pkt[6])<<8 | uint16(pkt[7])) & 0x1fff; fragOff == 0 {
		h.sport, h.dport = l4Ports(pkt, ihl, h.proto)
	}
	return h, true
}

// ipv6Fields walks the extension-header chain to find the upper-layer header.
//
// Three chains are refused outright rather than translated. AH (51)
// authenticates the addresses being rewritten, so any translation invalidates
// it by construction. ESP (50) and "no next header" (59) leave nothing this
// code can locate a checksum in. Refusing means the packet passes through
// untranslated, which is the safe direction: a NAT that declines to act
// drops connectivity for that flow, while one that rewrites blind corrupts
// packets that would otherwise have worked.
func ipv6Fields(pkt []byte) (ipHdr, bool) {
	const fixedHdr = 40
	if len(pkt) < fixedHdr {
		return ipHdr{}, false
	}
	var s, d [16]byte
	copy(s[:], pkt[8:24])
	copy(d[:], pkt[24:40])
	h := ipHdr{is6: true, src: netip.AddrFrom16(s), dst: netip.AddrFrom16(d)}
	next := pkt[6]
	off := fixedHdr
	firstFragment := true
	// RFC 8200 sets no limit on chain length, so bound it here rather than
	// trusting a crafted packet to terminate the walk.
	for i := 0; i < 8; i++ {
		switch next {
		case 0, 43, 60: // hop-by-hop, routing, destination options
			if len(pkt) < off+2 {
				return ipHdr{}, false
			}
			l := (int(pkt[off+1]) + 1) * 8
			if l <= 0 || len(pkt) < off+l {
				return ipHdr{}, false
			}
			next = pkt[off]
			off += l
		case 44: // fragment header: fixed 8 bytes
			if len(pkt) < off+8 {
				return ipHdr{}, false
			}
			if (uint16(pkt[off+2])<<8|uint16(pkt[off+3]))>>3 != 0 {
				firstFragment = false
			}
			next = pkt[off]
			off += 8
		case 50, 51, 59: // ESP, AH, no-next-header — see doc comment
			return ipHdr{}, false
		default:
			if off > len(pkt) {
				return ipHdr{}, false
			}
			h.proto, h.l4off = next, off
			if firstFragment {
				h.sport, h.dport = l4Ports(pkt, off, next)
			}
			return h, true
		}
	}
	return ipHdr{}, false
}

func l4Ports(pkt []byte, off int, proto uint8) (sport, dport uint16) {
	if proto != protoTCP && proto != protoUDP {
		return 0, 0
	}
	if len(pkt) < off+4 {
		return 0, 0
	}
	return uint16(pkt[off])<<8 | uint16(pkt[off+1]), uint16(pkt[off+2])<<8 | uint16(pkt[off+3])
}

// ipv4Fields is the original IPv4-only parser's signature, retained for the
// existing tests and the fuzz target. New code uses ipFields.
func ipv4Fields(pkt []byte) (ihl int, proto uint8, src, dst netip.Addr, sport, dport uint16, ok bool) {
	h, k := ipFields(pkt)
	if !k || h.is6 {
		return 0, 0, netip.Addr{}, netip.Addr{}, 0, 0, false
	}
	return h.l4off, h.proto, h.src, h.dst, h.sport, h.dport, true
}

func setSrc(pkt []byte, h ipHdr, a netip.Addr, port uint16) {
	if h.is6 {
		b := a.As16()
		copy(pkt[8:24], b[:])
	} else {
		b := a.As4()
		copy(pkt[12:16], b[:])
	}
	if (h.proto == protoTCP || h.proto == protoUDP) && port != 0 && len(pkt) >= h.l4off+2 {
		pkt[h.l4off] = byte(port >> 8)
		pkt[h.l4off+1] = byte(port)
	}
}

func setDst(pkt []byte, h ipHdr, a netip.Addr, port uint16) {
	if h.is6 {
		b := a.As16()
		copy(pkt[24:40], b[:])
	} else {
		b := a.As4()
		copy(pkt[16:20], b[:])
	}
	if (h.proto == protoTCP || h.proto == protoUDP) && port != 0 && len(pkt) >= h.l4off+4 {
		pkt[h.l4off+2] = byte(port >> 8)
		pkt[h.l4off+3] = byte(port)
	}
}

func ones(b []byte, initial uint32) uint16 {
	sum := initial
	i := 0
	for ; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if i < len(b) {
		sum += uint32(b[i]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// pseudoSum accumulates the transport pseudo-header: the address pair, the
// upper-layer protocol, and the upper-layer length. IPv6 spells the last two
// as 32-bit fields where IPv4 uses 8 and 16 bits, but a ones-complement sum
// folds the zero high halves away, so one expression serves both.
func pseudoSum(pkt []byte, h ipHdr, l4len int) uint32 {
	var sum uint32
	lo, hi := 12, 20
	if h.is6 {
		lo, hi = 8, 40
	}
	for i := lo; i < hi; i += 2 {
		sum += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	return sum + uint32(h.proto) + uint32(l4len)
}

// fixChecksums recomputes every checksum invalidated by rewriting an address
// or port.
//
// The families differ in two ways. IPv4 has a header checksum and IPv6 has
// none. And ICMPv6's checksum covers the pseudo-header — so unlike ICMPv4,
// whose checksum spans only the ICMP message and survives address rewriting
// untouched, an ICMPv6 packet must have its checksum redone. Missing that
// would produce packets the receiving stack silently discards, with the NAT
// itself looking correct from both ends.
func fixChecksums(pkt []byte, h ipHdr) {
	if !h.is6 {
		pkt[10], pkt[11] = 0, 0
		c := ones(pkt[:h.l4off], 0)
		pkt[10], pkt[11] = byte(c>>8), byte(c)
	}
	var off int
	switch h.proto {
	case protoTCP:
		off = 16
	case protoUDP:
		off = 6
	case protoICMPv6:
		if !h.is6 {
			return // protocol 58 over IPv4 is not ICMPv6; leave it alone
		}
		off = 2
	default:
		return
	}
	if h.l4off > len(pkt) {
		return
	}
	l4 := pkt[h.l4off:]
	if len(l4) < off+2 {
		return
	}
	pseudo := pseudoSum(pkt, h, len(l4))
	l4[off], l4[off+1] = 0, 0
	cc := ones(l4, pseudo)
	if h.proto == protoUDP && cc == 0 {
		cc = 0xffff // 0 means "no checksum" in UDP/IPv4; RFC 768
	}
	l4[off], l4[off+1] = byte(cc>>8), byte(cc)
}

func prefixMatch(p netip.Prefix, a netip.Addr) bool {
	return !p.IsValid() || p.Contains(a)
}

// prefixMatchNeg is prefixMatch with an inversion flag. A blank (invalid)
// prefix still means "any" and stays true regardless of neg, so a negated
// blank can never turn into "match nothing" here — that combination is
// refused at save time, and this keeps a stale config from behaving
// surprisingly if one ever reaches the data plane.
func prefixMatchNeg(p netip.Prefix, a netip.Addr, neg bool) bool {
	if !p.IsValid() {
		return true
	}
	return p.Contains(a) != neg
}

// translateOut applies NAT to an egress (TUN->mesh) packet, rewriting in place.
func (t *natTable) translateOut(pkt []byte) {
	if t == nil {
		return
	}
	h, ok := ipFields(pkt)
	if !ok {
		return
	}
	proto, src, dst, sport, dport := h.proto, h.src, h.dst, h.sport, h.dport
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Reverse an inbound DNAT: reply from internal host -> restore source.
	if c := t.dnatRev[natKey{proto, src, dst, sport, dport}]; c != nil {
		c.lastSeen = now
		setSrc(pkt, h, c.oDst, c.oDport)
		fixChecksums(pkt, h)
		return
	}

	// 2. SNAT rules.
	for _, r := range t.snat {
		if r.is6 != h.is6 {
			continue
		}
		if r.proto != 0 && r.proto != proto {
			continue
		}
		if !prefixMatchNeg(r.src, src, r.srcNeg) || !prefixMatchNeg(r.dst, dst, r.dstNeg) {
			continue
		}
		fwd := natKey{proto, src, dst, sport, dport}
		c := t.snatFwd[fwd]
		if c == nil {
			tport := sport
			if proto == 6 || proto == 17 {
				for {
					if _, clash := t.snatRev[natKey{proto, dst, r.to, dport, tport}]; !clash {
						break
					}
					tport = t.allocPort()
				}
			}
			c = &natConn{
				oSrc: src, oDst: dst, oSport: sport, oDport: dport,
				tSrc: r.to, tDst: dst, tSport: tport, tDport: dport,
				proto: proto,
			}
			t.snatFwd[fwd] = c
			t.snatRev[natKey{proto, dst, r.to, dport, tport}] = c
		}
		c.lastSeen = now
		setSrc(pkt, h, c.tSrc, c.tSport)
		fixChecksums(pkt, h)
		return
	}
}

// translateIn applies NAT to an ingress (mesh->TUN) packet, rewriting in place.
func (t *natTable) translateIn(pkt []byte) {
	if t == nil {
		return
	}
	h, ok := ipFields(pkt)
	if !ok {
		return
	}
	proto, src, dst, sport, dport := h.proto, h.src, h.dst, h.sport, h.dport
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Reverse an outbound SNAT: reply to translated address -> restore dest.
	if c := t.snatRev[natKey{proto, src, dst, sport, dport}]; c != nil {
		c.lastSeen = now
		setDst(pkt, h, c.oSrc, c.oSport)
		fixChecksums(pkt, h)
		return
	}

	// 2. DNAT rules.
	for _, r := range t.dnat {
		if r.is6 != h.is6 {
			continue
		}
		if r.proto != 0 && r.proto != proto {
			continue
		}
		if !prefixMatchNeg(r.src, src, r.srcNeg) || !prefixMatchNeg(r.dst, dst, r.dstNeg) {
			continue
		}
		if r.dportLo != 0 && (dport < r.dportLo || dport > r.dportHi) {
			continue
		}
		tport := dport
		if r.toPort != 0 {
			tport = r.toPort
		}
		fwd := natKey{proto, src, dst, sport, dport}
		c := t.dnatFwd[fwd]
		if c == nil {
			c = &natConn{
				oSrc: src, oDst: dst, oSport: sport, oDport: dport,
				tSrc: src, tDst: r.to, tSport: sport, tDport: tport,
				proto: proto,
			}
			t.dnatFwd[fwd] = c
			// Reply will be src=internal host (r.to:tport) -> us (src:sport).
			t.dnatRev[natKey{proto, r.to, src, tport, sport}] = c
		}
		c.lastSeen = now
		setDst(pkt, h, c.tDst, c.tDport)
		fixChecksums(pkt, h)
		return
	}
}

func (t *natTable) allocPort() uint16 {
	if t.nextPort < 20000 {
		t.nextPort = 20000
	}
	p := t.nextPort
	t.nextPort++
	if t.nextPort == 0 {
		t.nextPort = 20000
	}
	return p
}

func (t *natTable) sweep(now time.Time) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, m := range []map[natKey]*natConn{t.snatFwd, t.snatRev, t.dnatFwd, t.dnatRev} {
		for k, c := range m {
			if now.Sub(c.lastSeen) > t.ttl {
				delete(m, k)
			}
		}
	}
}

// ---- exported config form ----

// NATRuleSpec is the config-facing NAT rule.
type NATRuleSpec struct {
	Source string // CIDR/host or empty=any
	Dest   string
	// SourceNegate/DestNegate invert their field's match — see
	// config.NATRule's doc comment.
	SourceNegate bool
	DestNegate   bool
	// DestPort/Proto scope a port-forward rule to a specific destination
	// port or range — see config.NATRule.DestPort's doc comment. Both
	// blank (the pre-PAT default) matches every port, same as before
	// these fields existed.
	DestPort string
	Proto    string
	// Translate names both the rewrite target and which mode the rule runs:
	//   - "masquerade", or blank with Interface set: SNAT, rewrite source to
	//     Interface's address.
	//   - a literal IPv4: SNAT, rewrite source to that fixed address.
	//   - "port-forward:<ipv4>": DNAT, rewrite destination to that address.
	//   - "port-forward:<ipv4>:<port>": same, and also rewrite the
	//     destination port to <port> (PAT) — only valid with a
	//     single-value DestPort.
	// See toRule for the parsing.
	Translate string
	Interface string // egress interface; its IPv4 is used when masquerading
}

// portForwardPrefix marks a Translate value as DNAT — see NATRuleSpec's doc
// comment. Matched case-insensitively so "Port-Forward:" from a config an
// operator hand-edited works the same as the lowercase form the admin UI
// and CLI always write.
const portForwardPrefix = "port-forward:"

func (s NATRuleSpec) toRule() (natRule, bool) {
	translate := strings.TrimSpace(s.Translate)
	action := snatAction
	var toPort uint16
	src, err := parsePrefixField(s.Source)
	if err != nil {
		return natRule{}, false
	}
	dst, err := parsePrefixField(s.Dest)
	if err != nil {
		return natRule{}, false
	}
	if rest, ok := cutPrefixFold(translate, portForwardPrefix); ok {
		action = dnatAction
		// config.SplitNATTarget, not a strings.Cut on the first colon: an
		// IPv6 target is mostly colons, and cutting on the first one leaves
		// "fd00" as the address with "203::5" offered as its port. Shared
		// with the config and kernel-NAT paths precisely so all three agree
		// on what a stored target means.
		addr, portStr, hasPort, perr := config.SplitNATTarget(rest)
		if perr != nil {
			return natRule{}, false
		}
		translate = addr
		if hasPort {
			p, cerr := strconv.Atoi(strings.TrimSpace(portStr))
			if cerr != nil || p < 1 || p > 65535 {
				return natRule{}, false
			}
			toPort = uint16(p)
		}
	} else if (translate == "" || strings.EqualFold(translate, "masquerade")) && s.Interface != "" {
		// Masquerade has no target address of its own, so the rule's family
		// comes from its source prefix and the egress interface supplies a
		// matching address. A blank source means IPv4 — config.buildNATRule
		// refuses to save that combination without saying so, so an operator
		// who wants IPv6 masqueraded has already been told to name a prefix.
		want6 := src.IsValid() && !src.Addr().Is4()
		ip, ok := interfaceAddr(s.Interface, want6)
		if !ok {
			return natRule{}, false
		}
		translate = ip.String()
	}
	to, err := netip.ParseAddr(translate)
	if err != nil || to.Is4In6() {
		return natRule{}, false
	}
	is6 := !to.Is4()
	// Every prefix in a rule must belong to the rule's own family. config
	// rejects a mixed rule at save time; this is the same check for a config
	// that arrived some other way (hand-edited file, older release), and it
	// matters because the translate paths dispatch on is6 alone — a v4 prefix
	// on a v6 rule would silently never match.
	if src.IsValid() && (!src.Addr().Is4()) != is6 {
		return natRule{}, false
	}
	if dst.IsValid() && (!dst.Addr().Is4()) != is6 {
		return natRule{}, false
	}
	var dpLo, dpHi uint16
	if dp := strings.TrimSpace(s.DestPort); dp != "" {
		lo, hi, perr := parsePortSpec(dp)
		if perr != nil {
			return natRule{}, false
		}
		dpLo, dpHi = lo, hi
	}
	return natRule{
		src: src, dst: dst, srcNeg: s.SourceNegate, dstNeg: s.DestNegate,
		to: to, action: action, proto: protoNum(s.Proto),
		dportLo: dpLo, dportHi: dpHi, toPort: toPort, is6: is6,
	}, true
}

// parsePortSpec parses "N" or "N-M" into inclusive bounds (a single port has
// lo == hi). Mirrors config.validNATPortSpec; kept as mesh's own copy for
// the same "packages intentionally don't depend on each other, they just
// happen to agree on the same small grammar" reason portForwardPrefix does.
func parsePortSpec(s string) (lo, hi uint16, err error) {
	a, b, isRange := strings.Cut(s, "-")
	if !isRange {
		p, perr := strconv.Atoi(a)
		if perr != nil || p < 1 || p > 65535 {
			return 0, 0, fmt.Errorf("bad port %q", s)
		}
		return uint16(p), uint16(p), nil
	}
	lo64, lerr := strconv.Atoi(a)
	hi64, herr := strconv.Atoi(b)
	if lerr != nil || herr != nil || lo64 < 1 || lo64 > 65535 || hi64 < 1 || hi64 > 65535 || lo64 > hi64 {
		return 0, 0, fmt.Errorf("bad port range %q", s)
	}
	return uint16(lo64), uint16(hi64), nil
}

// cutPrefixFold is strings.CutPrefix's case-insensitive counterpart — Go's
// standard library doesn't have one. Only used for the short, fixed
// portForwardPrefix keyword, so a simple length-bounded EqualFold on the
// leading slice is all this needs, no need to pull in unicode-aware
// case-folding machinery for something this narrow.
func cutPrefixFold(s, prefix string) (rest string, ok bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return s, false
	}
	return s[len(prefix):], true
}
