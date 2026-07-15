package mesh

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// hsPayload is the plaintext exchanged inside a handshake (sealed under the
// pre-shared key). It carries the sender's X25519 ephemeral, the session index
// the peer must use when sending data back, a timestamp for anti-replay, the
// sender's advertised overlay addresses, node id, and hostname.
type hsPayload struct {
	Index     uint32
	TimeNano  int64
	Ephemeral []byte // 32 bytes, X25519 public
	Overlay4  netip.Addr
	Overlay6  netip.Addr
	NodeID    string
	Hostname  string
	Subnet4   netip.Prefix // advertised so a joiner can self-assign
	Subnet6   netip.Prefix
	Name      string // network name, advertised so a joiner inherits it
	Managed   bool   // node opts into remote management ("managed" cluster mode)
	Manager   bool   // node opts into managing other Managed peers (see config.Config.Manager)
	WebPort   uint16 // web-admin port, advertised so a manager can reach it over the overlay
	TCPPort   uint16 // TCP/TLS fallback port, advertised so peers can dial it when UDP fails
	// ExtraTCPPorts/ExtraUDPPorts are additional listen ports (config
	// extra_tcp_listen_ports/extra_listen_ports) beyond the primary ones
	// above, advertised so a peer can try them too — the primary UDP port
	// needs no equivalent field since it's learned automatically (a peer
	// observes which port a handshake packet actually arrived from); extra
	// UDP ports have no such signal, so they only have any value if
	// explicitly advertised the same way TCP ports already are.
	ExtraTCPPorts []uint16
	ExtraUDPPorts []uint16
	// AllowRelay mirrors this node's config.AllowRelay (NetSpec.AllowRelay):
	// whether it is willing to forward other peers' traffic as an
	// intermediary (see onRelay). Advertised in the handshake so a peer can
	// tell, *before* committing to us as its relay, whether we would actually
	// carry the traffic — bestRelay had no way to know this, so it would
	// happily pick a node with allow_relay disabled, whose onRelay silently
	// drops every forwarded packet, and keep re-picking it forever.
	//
	// RelayKnown distinguishes "this peer says it will not relay" from "this
	// peer is too old to say" — see flagRelayKnown. It is not itself a wire
	// field; it's derived on decode from whether the advertising bit was
	// present at all.
	AllowRelay bool
	RelayKnown bool
	// LocalEndpoints are this node's own underlay interface addresses paired
	// with its primary UDP port — "host candidates" in ICE terms. Until these
	// existed, a node's endpoint could ONLY ever be learned by observation:
	// the source address a packet arrived from, or a third party gossiping
	// what *it* observed. That works fine across the internet and fails
	// completely for two nodes behind the same NAT: every outside observer
	// sees both at the one shared public address, so the only candidate either
	// ever learns for the other is that public address, and dialing it from
	// inside requires NAT hairpin, which plenty of gateways simply don't do.
	// Two machines on the same switch, with no firewall between them, would
	// therefore fail every direct handshake and fall back to relaying through
	// a node on the far side of the internet — while the LAN address that
	// would have worked instantly was known to nobody but themselves, because
	// nothing ever advertised it. Self-declared, so it needs no observer;
	// propagated onward by neighbors in the gossip peer list exactly the way
	// ExtraUDPPorts already is (see buildPeerList), which is what lets two
	// nodes that have never spoken discover each other's LAN address through
	// a mutual peer.
	LocalEndpoints []netip.AddrPort
}

const ephemeralLen = 32

const (
	flagHasV4 = 1 << 0
	flagHasV6 = 1 << 1
	// flagManaged marks a gossiped peer entry as remotely manageable; when set,
	// the entry carries a trailing 2-byte web-admin port. Only used in the peer
	// list (gossip), not the handshake (which advertises managed separately).
	flagManaged = 1 << 2
	// flagManager marks a gossiped peer entry as itself able to manage other
	// Managed peers (see config.Config.Manager). Carries no extra data — unlike
	// flagManaged it never gates a trailing field — so it's a plain bit. Same
	// gossip-only scope as flagManaged; the handshake advertises Manager
	// separately (see the "Manager-cluster advertisement" trailing field below).
	flagManager = 1 << 3
	// flagRelayKnown / flagAllowRelay advertise hsPayload.AllowRelay. Two bits,
	// not one, and the reason is backward compatibility: a node predating this
	// field sets neither bit, and a single "allow" bit would make it
	// indistinguishable from a node explicitly refusing to relay — so every
	// upgraded node would immediately stop relaying through every
	// not-yet-upgraded one, breaking relay across a rolling upgrade for
	// exactly as long as the mesh is mixed. With flagRelayKnown, "no bits set"
	// decodes as unknown, and unknown keeps today's optimistic behavior (assume
	// willing, try it, same as before this change). Only an explicit
	// known-and-refusing peer is skipped as a relay candidate. Both bits are
	// free in the peer-list encoding too (which only uses bits 0–3), so nothing
	// collides. Adding bits changes no field lengths, so an older decoder that
	// masks for the bits it knows simply ignores these.
	flagRelayKnown = 1 << 4
	flagAllowRelay = 1 << 5
)

var errBadPayload = errors.New("mesh: malformed handshake payload")

// encodeHSPayload serializes a payload to bytes.
//
//	[idx:4][ts:8][eph:32][flags:1]([v4:4])([v6:16])[nidLen:1][nid][hnLen:1][hn]
func encodeHSPayload(p hsPayload) []byte {
	var flags byte
	if p.Overlay4.Is4() {
		flags |= flagHasV4
	}
	if p.Overlay6.Is6() && !p.Overlay6.Is4In6() {
		flags |= flagHasV6
	}
	// Always advertise that we're new enough to have an opinion (flagRelayKnown),
	// then the opinion itself. See the flag constants for why both bits exist.
	flags |= flagRelayKnown
	if p.AllowRelay {
		flags |= flagAllowRelay
	}
	nid := []byte(p.NodeID)
	hn := []byte(p.Hostname)
	if len(nid) > 255 {
		nid = nid[:255]
	}
	if len(hn) > 255 {
		hn = hn[:255]
	}

	size := 4 + 8 + ephemeralLen + 1 + 1 + len(nid) + 1 + len(hn)
	if flags&flagHasV4 != 0 {
		size += 4
	}
	if flags&flagHasV6 != 0 {
		size += 16
	}
	b := make([]byte, 0, size)

	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], p.Index)
	b = append(b, u32[:]...)
	var u64 [8]byte
	binary.BigEndian.PutUint64(u64[:], uint64(p.TimeNano))
	b = append(b, u64[:]...)
	b = append(b, p.Ephemeral...)
	b = append(b, flags)
	if flags&flagHasV4 != 0 {
		a := p.Overlay4.As4()
		b = append(b, a[:]...)
	}
	if flags&flagHasV6 != 0 {
		a := p.Overlay6.As16()
		b = append(b, a[:]...)
	}
	b = append(b, byte(len(nid)))
	b = append(b, nid...)
	b = append(b, byte(len(hn)))
	b = append(b, hn...)

	// Subnet advertisement: [subFlags:1]([s4:4][bits:1])([s6:16][bits:1])
	var subFlags byte
	if p.Subnet4.IsValid() && p.Subnet4.Addr().Is4() {
		subFlags |= flagHasV4
	}
	if p.Subnet6.IsValid() && p.Subnet6.Addr().Is6() && !p.Subnet6.Addr().Is4In6() {
		subFlags |= flagHasV6
	}
	b = append(b, subFlags)
	if subFlags&flagHasV4 != 0 {
		a := p.Subnet4.Addr().As4()
		b = append(b, a[:]...)
		b = append(b, byte(p.Subnet4.Bits()))
	}
	if subFlags&flagHasV6 != 0 {
		a := p.Subnet6.Addr().As16()
		b = append(b, a[:]...)
		b = append(b, byte(p.Subnet6.Bits()))
	}

	// Network name advertisement (optional trailing field): [nameLen:1][name]
	nm := []byte(p.Name)
	if len(nm) > 255 {
		nm = nm[:255]
	}
	b = append(b, byte(len(nm)))
	b = append(b, nm...)

	// Managed-cluster advertisement (optional trailing field): [mflag:1][webPort:2].
	// mflag bit 0 is Managed, bit 1 is Manager (see config.Config.Manager) —
	// packed into the same byte rather than a new trailing field since both are
	// simple booleans and this keeps the wire format from growing per-flag.
	var mflag byte
	if p.Managed {
		mflag |= 1
	}
	if p.Manager {
		mflag |= 2
	}
	b = append(b, mflag)
	var wp [2]byte
	binary.BigEndian.PutUint16(wp[:], p.WebPort)
	b = append(b, wp[:]...)
	// TCP/TLS fallback port (optional trailing field): [tcpPort:2].
	var tp [2]byte
	binary.BigEndian.PutUint16(tp[:], p.TCPPort)
	b = append(b, tp[:]...)
	// Extra TCP/UDP listen ports (further optional trailing fields, each
	// count-prefixed since the list length varies):
	// [tcpCount:1][tcpPort:2]*N [udpCount:1][udpPort:2]*M
	b = appendPortList(b, p.ExtraTCPPorts)
	b = appendPortList(b, p.ExtraUDPPorts)
	// Local (host) endpoint candidates — a further optional trailing field,
	// count-prefixed like the port lists above. See hsPayload.LocalEndpoints.
	b = appendEndpointList(b, p.LocalEndpoints)
	return b
}

// maxLocalEndpoints caps how many host candidates a node advertises. A
// multi-homed box (several NICs, VPNs, docker/bridge interfaces, v4+v6 on
// each) can enumerate a surprising number of addresses, and every one of
// them becomes a seed the receiver will dial. Bounding it keeps the
// handshake small and stops a pathological host from making every peer
// spray handshakes at a dozen dead-end addresses forever.
const maxLocalEndpoints = 8

// appendEndpointList writes a count-prefixed list of endpoints:
// [n:1] then appendEndpoint each. Clamped to maxLocalEndpoints.
func appendEndpointList(b []byte, eps []netip.AddrPort) []byte {
	n := len(eps)
	if n > maxLocalEndpoints {
		n = maxLocalEndpoints
	}
	b = append(b, byte(n))
	for _, ep := range eps[:n] {
		b = appendEndpoint(b, ep)
	}
	return b
}

// readEndpointList reads a list written by appendEndpointList. ok is false
// only when the field is absent entirely (an older peer's shorter payload),
// which callers treat as "no candidates advertised," not as an error.
func readEndpointList(r *reader) ([]netip.AddrPort, bool) {
	n, ok := r.byte()
	if !ok {
		return nil, false
	}
	if n > maxLocalEndpoints {
		return nil, false // malformed or hostile: don't allocate on its say-so
	}
	out := make([]netip.AddrPort, 0, n)
	for i := 0; i < int(n); i++ {
		ep, ok := r.endpoint()
		if !ok {
			return nil, false
		}
		if ep.IsValid() {
			out = append(out, ep)
		}
	}
	return out, true
}

// appendPortList writes a count-prefixed list of ports: [n:1][port:2]*n.
// Clamped to 255 entries (a single length byte, like NodeID/Hostname/Name
// above) — no realistic config approaches that many extra ports, so this
// is a defensive cap, not an expected limit.
func appendPortList(b []byte, ports []uint16) []byte {
	n := len(ports)
	if n > 255 {
		n = 255
	}
	b = append(b, byte(n))
	for _, port := range ports[:n] {
		var pb [2]byte
		binary.BigEndian.PutUint16(pb[:], port)
		b = append(b, pb[:]...)
	}
	return b
}

// decodeHSPayload parses a payload, validating bounds.
func decodeHSPayload(b []byte) (hsPayload, error) {
	var p hsPayload
	r := reader{b: b}
	idx, ok := r.u32()
	if !ok {
		return p, errBadPayload
	}
	p.Index = idx
	ts, ok := r.u64()
	if !ok {
		return p, errBadPayload
	}
	p.TimeNano = int64(ts)
	eph, ok := r.take(ephemeralLen)
	if !ok {
		return p, errBadPayload
	}
	p.Ephemeral = append([]byte(nil), eph...)
	flags, ok := r.byte()
	if !ok {
		return p, errBadPayload
	}
	// A peer predating this field sets neither bit: RelayKnown stays false and
	// callers treat it optimistically (assume willing), preserving pre-change
	// behavior across a mixed-version mesh. See flagRelayKnown.
	p.RelayKnown = flags&flagRelayKnown != 0
	p.AllowRelay = flags&flagAllowRelay != 0
	if flags&flagHasV4 != 0 {
		v4, ok := r.take(4)
		if !ok {
			return p, errBadPayload
		}
		p.Overlay4 = netip.AddrFrom4([4]byte{v4[0], v4[1], v4[2], v4[3]})
	}
	if flags&flagHasV6 != 0 {
		v6, ok := r.take(16)
		if !ok {
			return p, errBadPayload
		}
		var a16 [16]byte
		copy(a16[:], v6)
		p.Overlay6 = netip.AddrFrom16(a16)
	}
	nidLen, ok := r.byte()
	if !ok {
		return p, errBadPayload
	}
	nid, ok := r.take(int(nidLen))
	if !ok {
		return p, errBadPayload
	}
	p.NodeID = string(nid)
	hnLen, ok := r.byte()
	if !ok {
		return p, errBadPayload
	}
	hn, ok := r.take(int(hnLen))
	if !ok {
		return p, errBadPayload
	}
	p.Hostname = string(hn)

	// Subnet advertisement is optional (older peers omit it).
	if subFlags, ok := r.byte(); ok {
		if subFlags&flagHasV4 != 0 {
			a, ok := r.take(4)
			if !ok {
				return p, errBadPayload
			}
			bits, ok := r.byte()
			if !ok {
				return p, errBadPayload
			}
			if pfx, err := netip.AddrFrom4([4]byte{a[0], a[1], a[2], a[3]}).Prefix(int(bits)); err == nil {
				p.Subnet4 = pfx
			}
		}
		if subFlags&flagHasV6 != 0 {
			a, ok := r.take(16)
			if !ok {
				return p, errBadPayload
			}
			bits, ok := r.byte()
			if !ok {
				return p, errBadPayload
			}
			var a16 [16]byte
			copy(a16[:], a)
			if pfx, err := netip.AddrFrom16(a16).Prefix(int(bits)); err == nil {
				p.Subnet6 = pfx
			}
		}
	}

	// Network name is an optional trailing field (older peers omit it).
	if nmLen, ok := r.byte(); ok {
		if nm, ok := r.take(int(nmLen)); ok {
			p.Name = string(nm)
		}
	}

	// Managed-cluster advertisement is an optional trailing field.
	if mflag, ok := r.byte(); ok {
		p.Managed = mflag&1 != 0
		p.Manager = mflag&2 != 0
		if wp, ok := r.take(2); ok {
			p.WebPort = binary.BigEndian.Uint16(wp)
		}
		// TCP/TLS fallback port is a further optional trailing field.
		if tp, ok := r.take(2); ok {
			p.TCPPort = binary.BigEndian.Uint16(tp)
			// Extra TCP/UDP listen ports are further optional trailing fields,
			// each count-prefixed. Nested the same way as everything above:
			// only attempted if the field before it was present, so an older
			// peer's shorter payload just leaves these at nil rather than
			// erroring — see appendPortList's doc comment for the format.
			if extraTCP, ok := readPortList(&r); ok {
				p.ExtraTCPPorts = extraTCP
				if extraUDP, ok := readPortList(&r); ok {
					p.ExtraUDPPorts = extraUDP
					// Local endpoint candidates, same nesting rule: absent on
					// an older peer's payload, which just leaves this nil.
					if local, ok := readEndpointList(&r); ok {
						p.LocalEndpoints = local
					}
				}
			}
		}
	}
	return p, nil
}

// readPortList reads a count-prefixed port list written by appendPortList.
// Returns ok=false if the count byte is missing (the normal case for an
// older peer that simply omits this field entirely) *or* if fewer ports
// follow than the count declared. That second case can only mean malformed
// or truncated input — a genuine older peer never emits a count it doesn't
// follow through on — and it leaves the reader's cursor at a position that
// no longer means anything, so every caller here treats ok=false as "stop
// trusting this decode from this point on," not "this one field is just
// absent."
func readPortList(r *reader) ([]uint16, bool) {
	n, ok := r.byte()
	if !ok {
		return nil, false
	}
	ports := make([]uint16, 0, n)
	for i := 0; i < int(n); i++ {
		pb, ok := r.take(2)
		if !ok {
			return ports, false
		}
		ports = append(ports, binary.BigEndian.Uint16(pb))
	}
	return ports, true
}

// reader is a tiny bounds-checked byte cursor.
type reader struct {
	b   []byte
	off int
}

func (r *reader) take(n int) ([]byte, bool) {
	if n < 0 || r.off+n > len(r.b) {
		return nil, false
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s, true
}
func (r *reader) byte() (byte, bool) {
	s, ok := r.take(1)
	if !ok {
		return 0, false
	}
	return s[0], true
}
func (r *reader) u32() (uint32, bool) {
	s, ok := r.take(4)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint32(s), true
}
func (r *reader) u64() (uint64, bool) {
	s, ok := r.take(8)
	if !ok {
		return 0, false
	}
	return binary.BigEndian.Uint64(s), true
}
