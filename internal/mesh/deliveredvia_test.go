package mesh

import (
	"net/netip"
	"testing"
)

// Which underlay carried a peer's traffic is a fact only the transport that
// read the packet knows. Everything downstream used to infer it from the
// address — "is there a TLS connection to this endpoint" — which is right only
// while one address means one peer.
//
// Two peers behind one NAT gateway break that, and the configuration is
// entirely ordinary: TCP/65432 forwarded to one host, UDP/65432 to another, on
// one external IP. Two independent forward rules. The TCP peer's connection
// made the UDP peer's endpoint answer HasTCP true, so the UDP peer was
// reported with transport "tcp" it never used, and its traffic was handed to
// the other peer's socket — leaving, counted as sent, arriving nowhere.

// A session records the underlay its packets actually arrive on, and defaults
// to UDP before anything has arrived: a session with no traffic yet has no TCP
// connection worth preferring either.
func TestSessionRecordsDeliveringUnderlay(t *testing.T) {
	ps := &peerSession{nodeID: "p", endpoint: netip.MustParseAddrPort("174.64.247.165:65432")}
	if got := ps.via(); got != ProtoUDP {
		t.Fatalf("fresh session via = %v, want udp", got)
	}
	ps.deliveredVia.Store(uint32(ProtoTCP))
	if got := ps.via(); got != ProtoTCP {
		t.Fatalf("via = %v, want tcp after a TCP delivery", got)
	}
	ps.deliveredVia.Store(uint32(ProtoUDP))
	if got := ps.via(); got != ProtoUDP {
		t.Fatalf("via = %v, want udp — a peer that moves back to UDP must be followed", got)
	}
}

// sendVia must steer by the session's recorded underlay, not by the address.
// recordingSender implements protoSender, so the engine is expected to use it.
type recordingSender struct {
	plain []netip.AddrPort
	via   []Proto
}

func (r *recordingSender) Send(to netip.AddrPort, b []byte) error {
	r.plain = append(r.plain, to)
	return nil
}

func (r *recordingSender) SendVia(to netip.AddrPort, b []byte, p Proto) error {
	r.via = append(r.via, p)
	return nil
}

func TestEngineSendsOverTheSessionsUnderlay(t *testing.T) {
	e := &Engine{}
	rs := &recordingSender{}
	e.Attach(rs)
	to := netip.MustParseAddrPort("174.64.247.165:65432")

	if err := e.sendVia(to, []byte("x"), ProtoUDP); err != nil {
		t.Fatalf("sendVia: %v", err)
	}
	if err := e.sendVia(to, []byte("x"), ProtoTCP); err != nil {
		t.Fatalf("sendVia: %v", err)
	}
	if len(rs.plain) != 0 {
		t.Errorf("fell back to address-based Send %d times; the transport offers SendVia", len(rs.plain))
	}
	if len(rs.via) != 2 || rs.via[0] != ProtoUDP || rs.via[1] != ProtoTCP {
		t.Fatalf("protocols sent = %v, want [udp tcp]", rs.via)
	}
}

// A transport without the protocol-aware form (UDP-only builds, test doubles)
// must still work rather than dropping traffic.
type plainSender struct{ n int }

func (p *plainSender) Send(netip.AddrPort, []byte) error { p.n++; return nil }

func TestEngineFallsBackToPlainSend(t *testing.T) {
	e := &Engine{}
	ps := &plainSender{}
	e.Attach(ps)
	if err := e.sendVia(netip.MustParseAddrPort("198.51.100.7:65432"), []byte("x"), ProtoTCP); err != nil {
		t.Fatalf("sendVia: %v", err)
	}
	if ps.n != 1 {
		t.Fatalf("plain sends = %d, want 1 — a transport without SendVia must still carry traffic", ps.n)
	}
}
