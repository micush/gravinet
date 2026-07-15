package mesh

import (
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// interfaceIPv4 returns the first non-loopback IPv4 address configured on the
// named interface, used as the masquerade source address.
func interfaceIPv4(name string) (netip.Addr, bool) {
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
		if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
			if na, ok := netip.AddrFromSlice(ip4); ok {
				return na.Unmap(), true
			}
		}
	}
	return netip.Addr{}, false
}

// NAT translates addresses on the overlay data path with connection tracking so
// replies are reverse-translated automatically.
//
//   - SNAT (masquerade): rewrite the source of egress packets to a single
//     address, with port translation so many internal hosts share it. Replies
//     arriving on ingress have their destination restored. This is the
//     overlay→underlay / overlay→overlay case.
//   - DNAT (port-forward): rewrite the destination of ingress packets to an
//     internal host. Replies leaving on egress have their source restored. This
//     is the underlay→overlay case.
//
// NAT is IPv4-only (IPv6 NAT is unusual and out of scope). It rewrites and
// recomputes IPv4 + TCP/UDP checksums; other L4 protocols are translated at the
// IP layer only (ICMP is tracked by address pair).

type natAction uint8

const (
	snatAction natAction = iota
	dnatAction
)

type natRule struct {
	action natAction
	src    netip.Prefix // match source (invalid = any)
	dst    netip.Prefix // match dest (invalid = any)
	proto  uint8        // 0 = any
	to     netip.Addr   // translation target
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

func ipv4Fields(pkt []byte) (ihl int, proto uint8, src, dst netip.Addr, sport, dport uint16, ok bool) {
	if len(pkt) < 20 || pkt[0]>>4 != 4 {
		return 0, 0, netip.Addr{}, netip.Addr{}, 0, 0, false
	}
	ihl = int(pkt[0]&0x0f) * 4
	if ihl < 20 || len(pkt) < ihl {
		return 0, 0, netip.Addr{}, netip.Addr{}, 0, 0, false
	}
	proto = pkt[9]
	var s, d [4]byte
	copy(s[:], pkt[12:16])
	copy(d[:], pkt[16:20])
	src, dst = netip.AddrFrom4(s), netip.AddrFrom4(d)
	if (proto == 6 || proto == 17) && len(pkt) >= ihl+4 {
		sport = uint16(pkt[ihl])<<8 | uint16(pkt[ihl+1])
		dport = uint16(pkt[ihl+2])<<8 | uint16(pkt[ihl+3])
	}
	return ihl, proto, src, dst, sport, dport, true
}

func setSrc(pkt []byte, ihl int, a netip.Addr, port uint16) {
	b := a.As4()
	copy(pkt[12:16], b[:])
	if (pkt[9] == 6 || pkt[9] == 17) && port != 0 && len(pkt) >= ihl+2 {
		pkt[ihl] = byte(port >> 8)
		pkt[ihl+1] = byte(port)
	}
}

func setDst(pkt []byte, ihl int, a netip.Addr, port uint16) {
	b := a.As4()
	copy(pkt[16:20], b[:])
	if (pkt[9] == 6 || pkt[9] == 17) && port != 0 && len(pkt) >= ihl+4 {
		pkt[ihl+2] = byte(port >> 8)
		pkt[ihl+3] = byte(port)
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

func fixChecksums(pkt []byte, ihl int) {
	// IPv4 header checksum.
	pkt[10], pkt[11] = 0, 0
	c := ones(pkt[:ihl], 0)
	pkt[10], pkt[11] = byte(c>>8), byte(c)

	proto := pkt[9]
	var off int
	switch proto {
	case 6:
		off = 16 // TCP checksum offset
	case 17:
		off = 6 // UDP checksum offset
	default:
		return
	}
	l4 := pkt[ihl:]
	if len(l4) < off+2 {
		return
	}
	// Pseudo-header: src(4) dst(4) zero proto len.
	var pseudo uint32
	for i := 12; i < 20; i += 2 {
		pseudo += uint32(pkt[i])<<8 | uint32(pkt[i+1])
	}
	pseudo += uint32(proto)
	pseudo += uint32(len(l4))
	l4[off], l4[off+1] = 0, 0
	cc := ones(l4, pseudo)
	if proto == 17 && cc == 0 {
		cc = 0xffff
	}
	l4[off], l4[off+1] = byte(cc>>8), byte(cc)
}

func prefixMatch(p netip.Prefix, a netip.Addr) bool {
	return !p.IsValid() || p.Contains(a)
}

// translateOut applies NAT to an egress (TUN->mesh) packet, rewriting in place.
func (t *natTable) translateOut(pkt []byte) {
	if t == nil {
		return
	}
	ihl, proto, src, dst, sport, dport, ok := ipv4Fields(pkt)
	if !ok {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Reverse an inbound DNAT: reply from internal host -> restore source.
	if c := t.dnatRev[natKey{proto, src, dst, sport, dport}]; c != nil {
		c.lastSeen = now
		setSrc(pkt, ihl, c.oDst, c.oDport)
		fixChecksums(pkt, ihl)
		return
	}

	// 2. SNAT rules.
	for _, r := range t.snat {
		if r.proto != 0 && r.proto != proto {
			continue
		}
		if !prefixMatch(r.src, src) || !prefixMatch(r.dst, dst) {
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
		setSrc(pkt, ihl, c.tSrc, c.tSport)
		fixChecksums(pkt, ihl)
		return
	}
}

// translateIn applies NAT to an ingress (mesh->TUN) packet, rewriting in place.
func (t *natTable) translateIn(pkt []byte) {
	if t == nil {
		return
	}
	ihl, proto, src, dst, sport, dport, ok := ipv4Fields(pkt)
	if !ok {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()

	// 1. Reverse an outbound SNAT: reply to translated address -> restore dest.
	if c := t.snatRev[natKey{proto, src, dst, sport, dport}]; c != nil {
		c.lastSeen = now
		setDst(pkt, ihl, c.oSrc, c.oSport)
		fixChecksums(pkt, ihl)
		return
	}

	// 2. DNAT rules.
	for _, r := range t.dnat {
		if r.proto != 0 && r.proto != proto {
			continue
		}
		if !prefixMatch(r.src, src) || !prefixMatch(r.dst, dst) {
			continue
		}
		fwd := natKey{proto, src, dst, sport, dport}
		c := t.dnatFwd[fwd]
		if c == nil {
			c = &natConn{
				oSrc: src, oDst: dst, oSport: sport, oDport: dport,
				tSrc: src, tDst: r.to, tSport: sport, tDport: dport,
				proto: proto,
			}
			t.dnatFwd[fwd] = c
			// Reply will be src=internal host (r.to:dport) -> us (src:sport).
			t.dnatRev[natKey{proto, r.to, src, dport, sport}] = c
		}
		c.lastSeen = now
		setDst(pkt, ihl, c.tDst, c.tDport)
		fixChecksums(pkt, ihl)
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
	Direction string // overlay2underlay | underlay2overlay | overlay2overlay
	Source    string // CIDR/host or empty=any
	Dest      string
	Translate string // target address, or "masquerade"/empty with Interface set
	Interface string // egress interface; its IPv4 is used when masquerading
}

func (s NATRuleSpec) toRule() (natRule, bool) {
	translate := strings.TrimSpace(s.Translate)
	// Masquerade: when no explicit translate address is given (or it's the
	// keyword "masquerade"), use the egress interface's primary IPv4.
	if (translate == "" || strings.EqualFold(translate, "masquerade")) && s.Interface != "" {
		if ip, ok := interfaceIPv4(s.Interface); ok {
			translate = ip.String()
		} else {
			return natRule{}, false
		}
	}
	to, err := netip.ParseAddr(translate)
	if err != nil || !to.Is4() {
		return natRule{}, false
	}
	src, err := parsePrefixField(s.Source)
	if err != nil {
		return natRule{}, false
	}
	dst, err := parsePrefixField(s.Dest)
	if err != nil {
		return natRule{}, false
	}
	r := natRule{src: src, dst: dst, to: to}
	switch strings.ToLower(s.Direction) {
	case "underlay2overlay":
		r.action = dnatAction
	default: // overlay2underlay, overlay2overlay
		r.action = snatAction
	}
	return r, true
}
