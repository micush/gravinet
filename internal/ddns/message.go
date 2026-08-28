// Package ddns registers this host's own name in DNS with dynamic updates.
//
// RFC 2136 UPDATE, signed with TSIG (RFC 8945) when a key is configured, over
// a hand-rolled DNS message encoder. No external dependency, matching the rest
// of this project — internal/dhcrelay builds DHCP frames the same way and for
// the same reason, and internal/mesh's firewall already carries a hand-rolled
// DNS response *parser* for its wildcard fqdn objects. This is the write half
// of the same wire format.
//
// # Why this exists
//
// A node whose address comes from DHCP gets registered by whatever hands out
// the lease. A gateway does not: its addresses are static, so nothing on the
// network ever announces them, and its name resolves only if somebody typed it
// into a zone by hand and remembered to change it afterwards. This closes that
// gap from the node's own side, which is also the only side that knows what
// addresses it currently has.
//
// # What it sends
//
// For each address, a two-record update: delete every existing record of that
// type at the name, then add the current one. That is the standard idiom and
// it is what makes this convergent rather than additive — a node that changed
// address does not end up with two A records, one of which is a lie. The pair
// travels in one message, so there is no window where the name resolves to
// nothing.
//
// Nothing is sent when the record already says the right thing. The check is a
// query to the authoritative server before each update, which costs one packet
// and saves an update on every tick after the first — on a node whose address
// is stable, which is the ordinary case, this is a query and nothing else.
package ddns

import (
	"encoding/binary"
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
)

// DNS wire constants. Only what this package sends or reads.
const (
	classIN   = 1
	classAny  = 255 // used by UPDATE to mean "delete every record of this type"
	classNone = 254

	typeA    = 1
	typeSOA  = 6
	typePTR  = 12
	typeAAAA = 28
	typeTSIG = 250
	typeAny  = 255

	opcodeQuery  = 0
	opcodeUpdate = 5
)

// rcodes worth naming, because the whole point of this package is telling an
// operator why their name did not appear.
const (
	rcodeNoError  = 0
	rcodeFormErr  = 1
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeNotImp   = 4
	rcodeRefused  = 5
	rcodeNotAuth  = 9
)

// rcodeText names a response code. The interesting ones here are REFUSED and
// NOTAUTH: both mean the server understood perfectly well and declined, which
// is a configuration answer rather than a network one, and an operator reading
// "update failed" learns nothing while "refused" sends them to the right page.
func rcodeText(rc int) string {
	switch rc {
	case rcodeNoError:
		return "NOERROR"
	case rcodeFormErr:
		return "FORMERR"
	case rcodeServFail:
		return "SERVFAIL"
	case rcodeNXDomain:
		return "NXDOMAIN"
	case rcodeNotImp:
		return "NOTIMP"
	case rcodeRefused:
		return "REFUSED (the server understood the update and declined it — check that this zone allows dynamic updates from this node's address, and that a TSIG key is configured if the zone requires one)"
	case rcodeNotAuth:
		// Two quite different faults share this code, and naming only the
		// signature one sent operators to the TSIG settings for what was
		// usually a zone that the server does not serve under that name.
		return "NOTAUTH (the server is not authoritative for the zone this update names, or it rejected the signature — check that the zone exists on that server under exactly that name, then the TSIG key name, secret and algorithm and this node's clock)"
	case 16:
		return "BADSIG (the TSIG signature did not verify — the secret or algorithm does not match the server's)"
	case 17:
		return "BADKEY (the server does not know this TSIG key name)"
	case 18:
		return "BADTIME (the signature timestamp is outside the server's fudge window — this node's clock is wrong)"
	}
	return fmt.Sprintf("rcode %d", rc)
}

// encodeName writes a domain name in wire format: each label length-prefixed,
// terminated by a zero byte.
//
// Never compressed. Compression pointers are legal in a question and in most
// rdata, but a TSIG MAC is computed over the bytes actually sent, and a name
// this code compressed differently from how it measured would sign one message
// and send another. Uncompressed costs a few bytes in a packet that is already
// far inside one datagram.
func encodeName(name string) ([]byte, error) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	if name == "" {
		return []byte{0}, nil
	}
	out := make([]byte, 0, len(name)+2)
	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return nil, fmt.Errorf("name %q has an empty label", name)
		}
		if len(label) > 63 {
			return nil, fmt.Errorf("name %q has a label longer than 63 bytes", name)
		}
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	if len(out) > 255 {
		return nil, fmt.Errorf("name %q is longer than 255 bytes on the wire", name)
	}
	return out, nil
}

// decodeName reads a name at off, following compression pointers. Returns the
// name and the offset just past it in the *original* stream, which is not
// where the name ended if a pointer was followed.
func decodeName(msg []byte, off int) (string, int, bool) {
	var labels []string
	next := -1
	for hops := 0; ; hops++ {
		if hops > 50 || off < 0 || off >= len(msg) {
			return "", 0, false
		}
		n := int(msg[off])
		if n == 0 {
			off++
			if next < 0 {
				next = off
			}
			return strings.Join(labels, "."), next, true
		}
		if n&0xC0 == 0xC0 {
			if off+1 >= len(msg) {
				return "", 0, false
			}
			ptr := int(binary.BigEndian.Uint16(msg[off:off+2]) & 0x3FFF)
			if next < 0 {
				next = off + 2
			}
			off = ptr
			continue
		}
		if n > 63 || off+1+n > len(msg) {
			return "", 0, false
		}
		labels = append(labels, string(msg[off+1:off+1+n]))
		off += 1 + n
	}
}

// header is a DNS message header.
type header struct {
	id                                 uint16
	flags                              uint16
	qdcount, ancount, nscount, arcount uint16
}

func (h header) marshal() []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[0:], h.id)
	binary.BigEndian.PutUint16(b[2:], h.flags)
	binary.BigEndian.PutUint16(b[4:], h.qdcount)
	binary.BigEndian.PutUint16(b[6:], h.ancount)
	binary.BigEndian.PutUint16(b[8:], h.nscount)
	binary.BigEndian.PutUint16(b[10:], h.arcount)
	return b
}

func newID() uint16 { return uint16(rand.Intn(1 << 16)) }

// buildQuery builds an ordinary question. Used to find a zone's authoritative
// server and to read back the record currently published for a name.
//
// Recursion desired is set only for the SOA lookup path, where the configured
// resolver may not be authoritative for the zone; the read-back goes straight
// to the authoritative server, where recursion is meaningless and asking for
// it is noise.
func buildQuery(name string, qtype int, recursion bool) (uint16, []byte, error) {
	qname, err := encodeName(name)
	if err != nil {
		return 0, nil, err
	}
	h := header{id: newID(), qdcount: 1}
	if recursion {
		h.flags = 0x0100
	}
	msg := append(h.marshal(), qname...)
	msg = binary.BigEndian.AppendUint16(msg, uint16(qtype))
	msg = binary.BigEndian.AppendUint16(msg, classIN)
	return h.id, msg, nil
}

// rr is one record in an update's update-section.
type rr struct {
	name  string // fully qualified, no trailing dot needed
	rtype int
	class int
	ttl   uint32
	rdata []byte
}

func (r rr) marshal() ([]byte, error) {
	name, err := encodeName(r.name)
	if err != nil {
		return nil, err
	}
	out := append([]byte{}, name...)
	out = binary.BigEndian.AppendUint16(out, uint16(r.rtype))
	out = binary.BigEndian.AppendUint16(out, uint16(r.class))
	out = binary.BigEndian.AppendUint32(out, r.ttl)
	out = binary.BigEndian.AppendUint16(out, uint16(len(r.rdata)))
	return append(out, r.rdata...), nil
}

// deleteRRset is "remove every record of this type at this name", the first
// half of the replace idiom. Class ANY with no rdata, per RFC 2136 §2.5.2.
//
// Deleting the set rather than the specific old value is deliberate: this node
// does not reliably know what it published last — it may have been restarted,
// restored, or renumbered since — and deleting only the value it remembers
// would leave a stale record behind precisely when the address changed, which
// is the one case this exists to handle.
func deleteRRset(name string, rtype int) rr {
	return rr{name: name, rtype: rtype, class: classAny}
}

// addRecord is the second half: the value that should be there now.
func addRecord(name string, rtype int, ttl uint32, rdata []byte) rr {
	return rr{name: name, rtype: rtype, class: classIN, ttl: ttl, rdata: rdata}
}

// rdataAddr encodes an address as A or AAAA rdata, and reports which type it is.
func rdataAddr(a netip.Addr) (int, []byte) {
	if a.Is4() {
		b := a.As4()
		return typeA, b[:]
	}
	b := a.As16()
	return typeAAAA, b[:]
}

// buildUpdate assembles an UPDATE message for one zone.
//
// The zone section names the zone and carries type SOA, which is what tells the
// server which of its zones this update is against. Everything in updates goes
// in the update section; there are no prerequisites, deliberately — a
// prerequisite would make this fail rather than converge when the zone does not
// hold what we expected, and converging is the entire job.
func buildUpdate(zone string, updates []rr) (uint16, []byte, error) {
	zname, err := encodeName(zone)
	if err != nil {
		return 0, nil, fmt.Errorf("zone: %w", err)
	}
	h := header{
		id:      newID(),
		flags:   opcodeUpdate << 11,
		qdcount: 1,                    // ZOCOUNT
		nscount: uint16(len(updates)), // UPCOUNT
	}
	msg := append(h.marshal(), zname...)
	msg = binary.BigEndian.AppendUint16(msg, typeSOA)
	msg = binary.BigEndian.AppendUint16(msg, classIN)
	for _, u := range updates {
		b, err := u.marshal()
		if err != nil {
			return 0, nil, err
		}
		msg = append(msg, b...)
	}
	return h.id, msg, nil
}

// answer is one parsed record from a response.
type answer struct {
	name  string
	rtype int
	rdata []byte
	// text is the rdata rendered for the types this package reads back: an
	// address for A/AAAA, a name for SOA MNAME and PTR.
	text string
}

// parseResponse reads a response far enough to answer the two questions this
// package asks of one: what did it say, and did it work.
func parseResponse(msg []byte) (rcode int, answers []answer, ok bool) {
	if len(msg) < 12 {
		return 0, nil, false
	}
	flags := binary.BigEndian.Uint16(msg[2:4])
	rcode = int(flags & 0x000F)
	qd := int(binary.BigEndian.Uint16(msg[4:6]))
	an := int(binary.BigEndian.Uint16(msg[6:8]))
	ns := int(binary.BigEndian.Uint16(msg[8:10]))

	off := 12
	for i := 0; i < qd; i++ {
		_, next, good := decodeName(msg, off)
		if !good || next+4 > len(msg) {
			return rcode, nil, false
		}
		off = next + 4
	}
	// Answer, then authority. A SOA lookup against a recursive resolver for a
	// name that does not exist puts the zone's SOA in the authority section
	// rather than the answer section, and that record is exactly what this
	// package is looking for — so both are read.
	for i := 0; i < an+ns; i++ {
		name, next, good := decodeName(msg, off)
		if !good || next+10 > len(msg) {
			return rcode, answers, true
		}
		rtype := int(binary.BigEndian.Uint16(msg[next : next+2]))
		rdlen := int(binary.BigEndian.Uint16(msg[next+8 : next+10]))
		rstart := next + 10
		if rstart+rdlen > len(msg) {
			return rcode, answers, true
		}
		a := answer{name: name, rtype: rtype, rdata: msg[rstart : rstart+rdlen]}
		switch rtype {
		case typeA:
			if rdlen == 4 {
				a.text = netip.AddrFrom4([4]byte(a.rdata)).String()
			}
		case typeAAAA:
			if rdlen == 16 {
				a.text = netip.AddrFrom16([16]byte(a.rdata)).String()
			}
		case typeSOA, typePTR:
			// SOA's MNAME is the first name in its rdata, which is the primary
			// master — the server an update has to be sent to.
			if n, _, good := decodeName(msg, rstart); good {
				a.text = n
			}
		}
		answers = append(answers, a)
		off = rstart + rdlen
	}
	return rcode, answers, true
}
