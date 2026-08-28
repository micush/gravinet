package ddns

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// The reverse zone a PTR belongs in is discovered, not derived.
//
// This is the one part of the package that could not be pinned by reading the
// source, because the fault was not in the control flow — the PTR was attempted
// on every run, exactly as intended — but in a name computed from arithmetic
// that only matches sites which delegate their reverse zones on a /24 or a /64.
// Everyone else sent a well-formed update naming a zone their server had never
// heard of, got NOTAUTH, and never saw a single PTR while their A and AAAA
// records published normally.
//
// So there is a server here. It is small and it fakes only what these tests
// ask of it, but the thing under test is which zone name goes on the wire, and
// that is only observable from the other end.

// fakeDNS is an authoritative server for a fixed set of zones.
type fakeDNS struct {
	zones []string
	// mnameUnresolvable makes the server answer no address for its SOA's
	// MNAME, the way a zone written with `@ IN SOA @ ...` does.
	mnameUnresolvable bool

	mu       sync.Mutex
	accepted []update // updates it served
	refused  []string // zone names it was asked about and does not hold
}

type update struct {
	zone    string
	records []string
}

// zoneFor is the longest zone this server holds that encloses name, or "".
func (f *fakeDNS) zoneFor(name string) string {
	name = strings.ToLower(strings.Trim(name, "."))
	best := ""
	for _, z := range f.zones {
		if name == z || strings.HasSuffix(name, "."+z) {
			if len(z) > len(best) {
				best = z
			}
		}
	}
	return best
}

func (f *fakeDNS) took() ([]update, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]update(nil), f.accepted...), append([]string(nil), f.refused...)
}

// start listens on loopback and serves until the test ends.
//
// Port 53 specifically: the package resolves an SOA's MNAME to an address and
// then dials it on the standard port, which is correct behaviour and leaves a
// test no say in the matter.
func (f *fakeDNS) start(t *testing.T) string {
	t.Helper()
	// Port 53 specifically, because a resolved MNAME is dialled there and a
	// test has no say in it. The exception is a server whose MNAME resolves to
	// nothing: there is no second dial to aim anywhere, so it can take any
	// port, which is also the only way two fakes can be up at once.
	addr := "127.0.0.1:53"
	if f.mnameUnresolvable {
		addr = "127.0.0.1:0"
	}
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		t.Skipf("cannot bind %s (%v)", addr, err)
	}
	t.Cleanup(func() { pc.Close() })

	go func() {
		buf := make([]byte, 4096)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := f.respond(append([]byte{}, buf[:n]...))
			if resp != nil {
				pc.WriteTo(resp, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func (f *fakeDNS) respond(msg []byte) []byte {
	if len(msg) < 12 {
		return nil
	}
	id := binary.BigEndian.Uint16(msg[0:2])
	op := (binary.BigEndian.Uint16(msg[2:4]) >> 11) & 0xF
	qname, next, ok := decodeName(msg, 12)
	if !ok || next+4 > len(msg) {
		return nil
	}
	qtype := int(binary.BigEndian.Uint16(msg[next : next+2]))

	reply := func(rcode, ancount, nscount int, body []byte) []byte {
		out := append([]byte{}, msg[:next+4]...)
		binary.BigEndian.PutUint16(out[0:2], id)
		binary.BigEndian.PutUint16(out[2:4], uint16(op)<<11|0x8000|uint16(rcode))
		binary.BigEndian.PutUint16(out[4:6], 1)
		binary.BigEndian.PutUint16(out[6:8], uint16(ancount))
		binary.BigEndian.PutUint16(out[8:10], uint16(nscount))
		binary.BigEndian.PutUint16(out[10:12], 0)
		return append(out, body...)
	}

	// An SOA owned by apex, naming ns.example as its master.
	soa := func(apex string) []byte {
		owner, _ := encodeName(apex)
		mname, _ := encodeName("ns.example")
		rname, _ := encodeName("hostmaster.example")
		rdata := append(append([]byte{}, mname...), rname...)
		rdata = append(rdata, make([]byte, 20)...) // serial and the timers
		b := append([]byte{}, owner...)
		b = binary.BigEndian.AppendUint16(b, typeSOA)
		b = binary.BigEndian.AppendUint16(b, classIN)
		b = binary.BigEndian.AppendUint32(b, 3600)
		b = binary.BigEndian.AppendUint16(b, uint16(len(rdata)))
		return append(b, rdata...)
	}

	if op == opcodeUpdate {
		apex := f.zoneFor(qname)
		if apex != strings.ToLower(strings.Trim(qname, ".")) {
			// A server refuses an update whose zone section names something
			// that is not one of its zones. This is the behaviour the bug ran
			// into on every site that does not delegate on a /24.
			f.mu.Lock()
			f.refused = append(f.refused, qname)
			f.mu.Unlock()
			return reply(rcodeNotAuth, 0, 0, nil)
		}
		u := update{zone: qname}
		off := next + 4
		for i := 0; i < int(binary.BigEndian.Uint16(msg[8:10])); i++ {
			nm, nx, ok := decodeName(msg, off)
			if !ok || nx+10 > len(msg) {
				break
			}
			rtype := int(binary.BigEndian.Uint16(msg[nx : nx+2]))
			rdlen := int(binary.BigEndian.Uint16(msg[nx+8 : nx+10]))
			u.records = append(u.records, nm+" "+recordTypeName(rtype))
			off = nx + 10 + rdlen
		}
		f.mu.Lock()
		f.accepted = append(f.accepted, u)
		f.mu.Unlock()
		return reply(rcodeNoError, 0, 0, nil)
	}

	apex := f.zoneFor(qname)
	switch {
	case strings.EqualFold(strings.Trim(qname, "."), "ns.example") && qtype == typeA && !f.mnameUnresolvable:
		owner, _ := encodeName(qname)
		b := append([]byte{}, owner...)
		b = binary.BigEndian.AppendUint16(b, typeA)
		b = binary.BigEndian.AppendUint16(b, classIN)
		b = binary.BigEndian.AppendUint32(b, 3600)
		b = binary.BigEndian.AppendUint16(b, 4)
		return reply(rcodeNoError, 1, 0, append(b, 127, 0, 0, 1))

	case apex == "":
		// Not ours and nothing above it either.
		return reply(rcodeNXDomain, 0, 0, nil)

	case qtype == typeSOA && apex == strings.ToLower(strings.Trim(qname, ".")):
		// The apex itself: the SOA is a positive answer.
		return reply(rcodeNoError, 1, 0, soa(apex))

	default:
		// A name inside a zone we hold, with no record of the type asked for.
		// The zone's SOA goes in the authority section — which is exactly how
		// a server tells a client what the enclosing zone is called.
		return reply(rcodeNXDomain, 0, 1, soa(apex))
	}
}

// hostAddrs is what collectRecords will find, so a test can say what it expects
// without hard-coding this machine's addressing.
func hostAddrs(t *testing.T) []netip.Addr {
	t.Helper()
	recs, err := collectRecords("node7", "corp.internal", nil)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	var out []netip.Addr
	for _, rs := range recs {
		if rs.primary && len(rs.addrs) > 0 {
			out = append(out, rs.addrs[0])
		}
	}
	if len(out) == 0 {
		t.Skip("this machine has no address worth publishing")
	}
	return out
}

// A reverse zone delegated on a /16 gets its PTR. Through v1000 it got nothing:
// the update named the /24, which that server does not serve.
func TestPTRGoesToTheZoneTheServerActuallyHolds(t *testing.T) {
	var revZones []string
	for _, a := range hostAddrs(t) {
		if a.Is4() {
			revZones = append(revZones, shortReverseZone(a))
		}
	}
	if len(revZones) == 0 {
		t.Skip("no IPv4 address on this machine")
	}

	f := &fakeDNS{zones: append([]string{"corp.internal"}, revZones...)}
	server := f.start(t)

	res, err := Register(Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		TTL:      900,
		Reverse:  true,
	}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	accepted, refused := f.took()
	if len(refused) > 0 {
		t.Errorf("the server refused updates naming %v — the zone is being derived rather than discovered", refused)
	}
	if len(res.Errors) > 0 {
		t.Errorf("errors: %v", res.Errors)
	}

	var ptrZones []string
	for _, u := range accepted {
		for _, r := range u.records {
			if strings.HasSuffix(r, " PTR") {
				ptrZones = append(ptrZones, u.zone)
				break
			}
		}
	}
	if len(ptrZones) == 0 {
		t.Fatal("no PTR update was sent at all")
	}
	for _, z := range ptrZones {
		if f.zoneFor(z) != strings.ToLower(z) {
			t.Errorf("PTR update named zone %q, which the server does not hold", z)
		}
	}
}

// shortReverseZone is the /16-style reverse zone for an IPv4 address: one label
// shorter than the /24 the old code assumed, which is the common case this
// package used to fail on.
func shortReverseZone(a netip.Addr) string {
	b := a.As4()
	return fmt.Sprintf("%d.%d.in-addr.arpa", b[1], b[0])
}

// When no reverse zone is served, the note says so rather than blaming TSIG.
func TestNoReverseZoneIsReportedAsAMissingDelegation(t *testing.T) {
	addrs := hostAddrs(t)
	f := &fakeDNS{zones: []string{"corp.internal", "in-addr.arpa", "ip6.arpa"}}
	server := f.start(t)

	res, err := Register(Params{
		Hostname: "node7",
		Domain:   "corp.internal",
		Servers:  []string{server},
		TTL:      900,
		Reverse:  true,
	}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(res.Errors) == 0 {
		t.Fatalf("no note about the missing reverse zone for %v", addrs)
	}
	joined := strings.Join(res.Errors, " ")
	if !strings.Contains(joined, "reverse zone") {
		t.Errorf("the note does not mention the reverse zone: %s", joined)
	}
	if _, refused := f.took(); len(refused) > 0 {
		t.Errorf("an update was sent to the arpa apex anyway: %v", refused)
	}
}

// zoneFromSOA takes the apex from the SOA's owner, and refuses to believe an
// owner that does not enclose the name asked about.
func TestZoneFromSOA(t *testing.T) {
	for _, tc := range []struct{ owner, queried, want string }{
		{"0.192.in-addr.arpa", "2.2.0.192.in-addr.arpa", "0.192.in-addr.arpa"},
		{"corp.internal", "corp.internal", "corp.internal"},
		{"CORP.INTERNAL.", "node7.corp.internal", "corp.internal"},
		{"corp.internal", "node7.eng.corp.internal", "corp.internal"},
		// Not a suffix: an answer this code cannot build an update from, so it
		// keeps the name it asked about instead of inventing a zone.
		{"example.net", "node7.corp.internal", "node7.corp.internal"},
		{"", "corp.internal", "corp.internal"},
		// A sibling that merely ends in the same letters is not an enclosing
		// zone; the boundary is a label, not a substring.
		{"orp.internal", "node7.corp.internal", "node7.corp.internal"},
	} {
		if got := zoneFromSOA(tc.owner, tc.queried); got != tc.want {
			t.Errorf("zoneFromSOA(%q, %q) = %q, want %q", tc.owner, tc.queried, got, tc.want)
		}
	}
}

// The arpa apexes are real zones and would otherwise pass every other check,
// while an update naming one is refused by everything.
func TestUsableReverseZone(t *testing.T) {
	rev := "2.2.0.192.in-addr.arpa"
	for _, bad := range []string{"", "arpa", "in-addr.arpa", "ip6.arpa", "IN-ADDR.ARPA.", "example.net"} {
		if usableReverseZone(bad, rev) {
			t.Errorf("usableReverseZone(%q) = true", bad)
		}
	}
	for _, good := range []string{"0.192.in-addr.arpa", "2.0.192.in-addr.arpa", "192.in-addr.arpa"} {
		if !usableReverseZone(good, rev) {
			t.Errorf("usableReverseZone(%q) = false", good)
		}
	}
}

// The forward update names the zone the server holds too, which is the same
// bug in the half that happened to work: a host in a subdomain of its zone was
// naming its search domain and being refused.
func TestForwardUpdateNamesTheDiscoveredApex(t *testing.T) {
	f := &fakeDNS{zones: []string{"corp.internal"}}
	server := f.start(t)

	res, err := Register(Params{
		Hostname: "node7",
		Domain:   "eng.corp.internal", // a search domain that is not a zone
		Servers:  []string{server},
		TTL:      900,
	}, nil)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("errors: %v", res.Errors)
	}
	accepted, refused := f.took()
	if len(refused) > 0 {
		t.Errorf("refused updates naming %v; the zone came from the search domain rather than the SOA", refused)
	}
	for _, u := range accepted {
		if !strings.EqualFold(u.zone, "corp.internal") {
			t.Errorf("update named zone %q, want corp.internal", u.zone)
		}
		for _, r := range u.records {
			if !strings.Contains(r, "eng.corp.internal") {
				t.Errorf("record %q is not under the search domain", r)
			}
		}
	}
}
