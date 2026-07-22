package mesh

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Wildcard fqdn objects ("*.example.com") can't be resolved by a direct DNS
// lookup — there is no query you can send that asks a resolver "give me
// every address under this domain". The only way to know what a wildcard
// pattern actually covers is to watch real DNS traffic go by and notice
// which answers happen to match it, the same approach gravinet's sibling
// project parapet uses. This file is that: a passive tap on DNS response
// packets already crossing this firewall's enforcement point (no proxy, no
// port redirection, nothing DNS-shaped needs to be reconfigured anywhere —
// see observeDNSResponse's call site in allow()), a minimal hand-rolled DNS
// message parser (no external dependency, matching the rest of this
// module), and a per-object address cache where each learned address
// expires on its own DNS record's TTL rather than a fixed poll interval.
//
// Literal (non-wildcard) fqdn entries are unaffected by any of this — they
// keep using the periodic resolver in firewall_fqdn.go exactly as before.
// An object mixing both kinds of entry (e.g. "example.com, *.example.com")
// gets contributions from both paths; see mergedFQDNLocked.

// ---- wildcard/literal pattern matching ----

// isWildcardFQDN reports whether an fqdn object's address entry is a
// subdomain wildcard ("*.example.com") rather than a literal name. Anything
// else — including a malformed glob like "*example.com" with no dot after
// the star, which has no meaning in real DNS traffic — is treated as a
// literal name and handled by the ordinary resolver, which will simply fail
// to resolve it. That's a deliberate non-choice: parsing "*foo.com" as some
// other kind of pattern would mean guessing at an intent DNS itself has no
// way to fulfil, and a resolve failure is already a safe, visible,
// self-correcting outcome for an entry that doesn't do what its author
// meant.
func isWildcardFQDN(pattern string) bool {
	return strings.HasPrefix(strings.TrimSpace(pattern), "*.")
}

// fqdnPatternMatch reports whether name (a name observed in a live DNS
// answer) matches pattern (one address entry from an fqdn object).
//
// "*.example.com" matches any subdomain of example.com (foo.example.com,
// a.b.example.com) *and* example.com itself — the wildcard is read as "this
// domain and everything under it", not strictly "everything under it
// excluding itself". A DNS wildcard label has no meaning to a real
// resolver, so "*.example.com" never appears as an answer name on the wire;
// the bare-domain case here matches on an observed answer for the literal
// name example.com, same as the subdomain case matches on an observed
// answer for e.g. foo.example.com — both are real names a resolver
// actually returns, just checked against the same pattern.
//
// A non-wildcard pattern matches only that exact name. Comparison is
// case-insensitive and ignores a trailing root dot on either side (DNS
// names are conventionally shown without one, but the wire form and some
// resolvers' string forms carry it).
func fqdnPatternMatch(pattern, name string) bool {
	pattern = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(pattern), "."))
	name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
	if pattern == "" || name == "" {
		return false
	}
	if strings.HasPrefix(pattern, "*.") {
		suffix := pattern[2:]
		if suffix == "" {
			return false
		}
		if name == suffix {
			return true
		}
		return strings.HasSuffix(name, suffix) &&
			len(name) > len(suffix)+1 &&
			name[len(name)-len(suffix)-1] == '.'
	}
	return pattern == name
}

// ---- minimal DNS response parsing (A/AAAA answers only) ----
//
// Just enough of RFC 1035 §4 to pull (name, address, TTL) out of every
// A/AAAA answer record in a response message: the 12-byte header, name
// decoding (labels and compression pointers), and walking the question and
// answer sections far enough to read each RR's type/ttl/rdlength/rdata.
// Nothing else in the message (authority/additional sections, other RR
// types, EDNS options) is touched. This parses bytes taken directly off the
// wire from whatever a peer happens to send on port 53 — every offset is
// bounds-checked and compression pointers are capped and required to point
// strictly backward, so a truncated, malformed, or deliberately hostile
// message can only ever fail to parse, never panic or loop.

// dnsMaxNamePointers bounds compression-pointer hops while decoding a
// single name. A real name resolves in at most a few hops; this is
// generous headroom against a malformed or adversarial pointer chain, not
// a realistic ceiling.
const dnsMaxNamePointers = 128

// dnsName decodes the name starting at off within msg, following
// compression pointers as needed, and returns the decoded, lowercased,
// dot-joined name (no trailing dot; the root name decodes to "") plus the
// offset immediately after this name's own encoding in the stream — i.e.
// where the next field after the name begins. That "next" offset is fixed
// the moment the first pointer (if any) is followed, regardless of how many
// further hops it takes to actually resolve the name: a pointer, once
// encountered, is always the last two bytes of the name's encoding at its
// original position, even though the label data it points to lives
// elsewhere in the message. ok is false on any malformed input; callers
// must stop parsing the message on that signal rather than guess at a
// resync point.
func dnsName(msg []byte, off int) (name string, next int, ok bool) {
	if off < 0 || off >= len(msg) {
		return "", 0, false
	}
	var b strings.Builder
	pos := off
	resumeAt := -1 // set once, on the first pointer followed; where the OUTER parse continues
	hops := 0
	for {
		if pos >= len(msg) {
			return "", 0, false
		}
		l := msg[pos]
		switch {
		case l == 0:
			pos++
			if resumeAt >= 0 {
				pos = resumeAt
			}
			return strings.ToLower(b.String()), pos, true
		case l&0xc0 == 0xc0: // compression pointer
			if pos+1 >= len(msg) {
				return "", 0, false
			}
			hops++
			if hops > dnsMaxNamePointers {
				return "", 0, false
			}
			ptr := int(l&0x3f)<<8 | int(msg[pos+1])
			if resumeAt < 0 {
				resumeAt = pos + 2
			}
			if ptr >= pos {
				// A well-formed message only ever points strictly backward
				// (into already-seen data, typically the question section).
				// A forward or self pointer is malformed at best, a
				// deliberate loop at worst — reject rather than risk it.
				return "", 0, false
			}
			pos = ptr
		case l&0xc0 != 0: // the two reserved length-byte patterns (01/10) — not valid in RFC 1035
			return "", 0, false
		default:
			ll := int(l)
			pos++
			if pos+ll > len(msg) {
				return "", 0, false
			}
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.Write(msg[pos : pos+ll])
			pos += ll
		}
	}
}

// dnsAnswer is one A/AAAA record pulled out of a parsed DNS response.
type dnsAnswer struct {
	name string
	addr netip.Addr
	ttl  time.Duration
}

// dnsParseResponse parses msg (a UDP/TCP DNS payload) and returns every
// A/AAAA answer record's own name, address, and TTL. Only response messages
// (the QR header bit set) are parsed; a query returns nil, not an error —
// the caller doesn't distinguish direction before calling this, so seeing a
// query here is the normal case for roughly half of all port-53 traffic.
//
// A record parsed cleanly before a later one fails is still returned: each
// dnsAnswer appended here has already passed every bounds check on its own,
// so a truncated or malformed record later in the same message doesn't
// retroactively make an earlier, fully-valid one wrong — only a header
// malformed enough that the message can't be trusted at all (too short, not
// a response, an implausible answer count) returns nil outright.
func dnsParseResponse(msg []byte) []dnsAnswer {
	if len(msg) < 12 {
		return nil
	}
	if msg[2]&0x80 == 0 { // QR: 0 = query
		return nil
	}
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	// A real response essentially always has exactly one question and a
	// modest answer count; either running into the thousands is not a
	// larger-than-usual answer, it's a corrupt or hostile count paired with
	// a much shorter actual buffer. Reject outright rather than let it drive
	// a long loop that will fail anyway once it runs past len(msg).
	if qd > 64 || an <= 0 || an > 4096 {
		return nil
	}
	off := 12
	for i := 0; i < qd; i++ {
		_, next, ok := dnsName(msg, off)
		if !ok {
			return nil
		}
		off = next + 4 // QTYPE(2) + QCLASS(2)
		if off > len(msg) {
			return nil
		}
	}
	out := make([]dnsAnswer, 0, an)
	for i := 0; i < an; i++ {
		name, next, ok := dnsName(msg, off)
		if !ok {
			return out
		}
		off = next
		if off+10 > len(msg) { // TYPE(2)+CLASS(2)+TTL(4)+RDLENGTH(2)
			return out
		}
		typ := binary.BigEndian.Uint16(msg[off : off+2])
		off += 2 + 2 // TYPE, CLASS (class is always IN in practice; not checked)
		ttl := binary.BigEndian.Uint32(msg[off : off+4])
		off += 4
		rdlen := int(binary.BigEndian.Uint16(msg[off : off+2]))
		off += 2
		if rdlen < 0 || off+rdlen > len(msg) {
			return out
		}
		rdata := msg[off : off+rdlen]
		off += rdlen
		switch typ {
		case 1: // A
			if rdlen == 4 {
				out = append(out, dnsAnswer{name: name, addr: netip.AddrFrom4([4]byte(rdata)), ttl: time.Duration(ttl) * time.Second})
			}
		case 28: // AAAA
			if rdlen == 16 {
				out = append(out, dnsAnswer{name: name, addr: netip.AddrFrom16([16]byte(rdata)), ttl: time.Duration(ttl) * time.Second})
			}
		}
	}
	return out
}

// ---- per-object TTL-based address cache ----

// wildcardFQDNCache holds sniffer-observed addresses for wildcard fqdn
// objects, per object name (lowercased), each address independently
// expiring at the time its own DNS answer's TTL indicated — the same idea
// as parapet's nft "flags timeout" sets, done here with an explicit map and
// a periodic sweep since gravinet's firewall enforcement is Go code, not a
// kernel ruleset with expiry built in.
//
// record (called from the hot packet path, via observeDNSResponse) only
// ever does a cheap map update under its own mutex — never firewall.mu,
// never a catalog rebuild. Only sweep (called from the maintenance tick,
// see sweepWildcardFQDN) drops expired entries and hands the caller
// whatever's still live, for promotion into the firewall's actual catalog.
// Splitting it this way keeps a burst of DNS traffic from turning into a
// burst of firewall recompiles.
type wildcardFQDNCache struct {
	mu      sync.Mutex
	entries map[string]map[netip.Addr]time.Time // object name -> addr -> expires-at
}

// record adds or refreshes one address's expiry for the named object. A
// later, later-expiring observation of the same address always wins over
// an earlier one (never shortens an existing entry's remaining life), since
// two records for the same name can legitimately carry different TTLs
// (e.g. a client re-querying before the first TTL elapsed) and the address
// is valid for as long as the longest-lived answer said it was.
func (c *wildcardFQDNCache) record(objName string, addr netip.Addr, ttl time.Duration) {
	if ttl <= 0 {
		ttl = time.Second // floor: keep a zero/negative TTL visible until at least the next sweep, rather than dropping it as if never seen
	}
	exp := time.Now().Add(ttl)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]map[netip.Addr]time.Time{}
	}
	m := c.entries[objName]
	if m == nil {
		m = map[netip.Addr]time.Time{}
		c.entries[objName] = m
	}
	if cur, ok := m[addr]; !ok || exp.After(cur) {
		m[addr] = exp
	}
}

// sweep drops every expired address and returns the current live set for
// every object that has (or, until this call, had) at least one entry —
// including an object whose set just became empty, so the caller can tell
// "this object's sniffed set is now empty" apart from "this object was
// never touched by the sniffer at all" and update the firewall accordingly
// either way.
func (c *wildcardFQDNCache) sweep(now time.Time) map[string][]netip.Prefix {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return nil
	}
	out := make(map[string][]netip.Prefix, len(c.entries))
	for name, m := range c.entries {
		var live []netip.Prefix
		for addr, exp := range m {
			if now.After(exp) {
				delete(m, addr)
				continue
			}
			live = append(live, netip.PrefixFrom(addr, addr.BitLen()))
		}
		if len(m) == 0 {
			delete(c.entries, name)
		}
		sortPrefixes(live)
		out[name] = live
	}
	return out
}

// ---- firewall integration ----

// observeDNSResponse is the hot-path hook called from allow() for every
// packet whose source port is 53. It bails immediately — a single pointer
// load, no locking — for any network with no wildcard fqdn objects
// configured, which is the common case. proto is passed through from the
// caller's own already-computed parseL4 result so this doesn't reparse the
// IP header.
func (f *firewall) observeDNSResponse(pkt []byte, proto uint8) {
	pats := f.wildcardPatterns.Load()
	if pats == nil || len(*pats) == 0 {
		return
	}
	payload := l4Payload(pkt, proto)
	if payload == nil {
		return
	}
	for _, a := range dnsParseResponse(payload) {
		for objName, patterns := range *pats {
			for _, pat := range patterns {
				if fqdnPatternMatch(pat, a.name) {
					f.wcCache.record(objName, a.addr, a.ttl)
					break
				}
			}
		}
	}
}

// sweepWildcardFQDN promotes the sniffer cache's current live (unexpired)
// per-object address sets into the firewall's actual fqdn-object catalog,
// recompiling only the objects whose set actually changed (setFQDNWildcard
// no-ops otherwise). Driven off the maintenance tick in control.go, the
// same 5-second cadence conntrack and NAT state sweeps already use — fine
// grained enough that even a short-TTL CDN record's address doesn't sit
// stale for long after it expires.
func (f *firewall) sweepWildcardFQDN(now time.Time) {
	live := f.wcCache.sweep(now)
	for name, prefixes := range live {
		f.setFQDNWildcard(name, prefixes)
	}
}

// l4Payload returns the UDP/TCP payload of an IPv4/IPv6 packet already
// known to carry the given protocol (as returned by parseL4), or nil if the
// packet is too short for its own declared header to fit — the same
// defensive bounds-checking parseL4 itself applies, extended one step
// further to the payload boundary parseL4 doesn't need and so doesn't
// compute. IPv6 extension headers are not walked, matching parseL4's own
// documented simplification.
func l4Payload(pkt []byte, proto uint8) []byte {
	if len(pkt) < 1 {
		return nil
	}
	switch pkt[0] >> 4 {
	case 4:
		if len(pkt) < 20 {
			return nil
		}
		ihl := int(pkt[0]&0x0f) * 4
		if ihl < 20 || len(pkt) < ihl {
			return nil
		}
		return l4PayloadFrom(pkt, ihl, proto)
	case 6:
		if len(pkt) < 40 {
			return nil
		}
		return l4PayloadFrom(pkt, 40, proto)
	}
	return nil
}

// l4PayloadFrom extracts the payload past the L4 header starting at
// hdrStart, given the L4 header itself begins there (IHL already applied
// for IPv4, fixed 40 for IPv6).
func l4PayloadFrom(pkt []byte, hdrStart int, proto uint8) []byte {
	switch proto {
	case 17: // UDP: 8-byte fixed header
		if len(pkt) < hdrStart+8 {
			return nil
		}
		return pkt[hdrStart+8:]
	case 6: // TCP: variable header, length from the data-offset nibble
		if len(pkt) < hdrStart+13 {
			return nil
		}
		dataOff := int(pkt[hdrStart+12]>>4) * 4
		if dataOff < 20 || len(pkt) < hdrStart+dataOff {
			return nil
		}
		return pkt[hdrStart+dataOff:]
	}
	return nil
}
