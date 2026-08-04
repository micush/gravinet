package transport

import (
	"errors"
	"net/netip"
)

// Dual carries the mesh over two transports, UDP and TCP/TLS, behind a single
// Sender. A send prefers an existing TLS connection to the destination — which
// exists when the peer reached us over TCP, or when we dialed TCP because UDP
// couldn't get through — and otherwise goes over UDP. Inbound datagrams from
// both are delivered to the same Handler, so the engine never has to know
// which underlay carried a packet.
//
// Neither is a fallback for the other. They are two ways to reach a peer, and
// which one works is a fact about that peer and the network between, not a
// tier to descend. UDP is preferred only because it is cheaper to set up.
//
// Either may be nil: TLS when no TCP port bound, UDP when the operator emptied
// udp_ports. Config.Validate refuses both empty, so at least one is live.
type Dual struct {
	UDP *Transport
	TLS *TLSTransport
}

// Send routes to TLS when a live connection to `to` exists, else to UDP. When
// UDP is off and there's no live TLS connection yet either, this errors rather
// than dialing — same as any other send that hasn't found its peer yet — and
// the engine's candidate dialing (see mesh.Engine.ensureFallback) is what
// actually opens one.
func (d Dual) Send(to netip.AddrPort, payload []byte) error {
	if d.TLS != nil && d.TLS.HasConn(to) {
		return d.TLS.Send(to, payload)
	}
	if d.UDP == nil {
		return errNoUDP
	}
	return d.UDP.Send(to, payload)
}

// DialTCP opens a TCP/TLS connection to a candidate endpoint so that
// subsequent sends there go over TLS. Errors if this node has no TCP
// transport at all (tcp_ports empty, or no port bound).
func (d Dual) DialTCP(to netip.AddrPort) error {
	if d.TLS == nil {
		return errNoTCP
	}
	return d.TLS.Dial(to)
}

// HasTCP reports whether a live TLS connection to the endpoint exists.
func (d Dual) HasTCP(to netip.AddrPort) bool {
	return d.TLS != nil && d.TLS.HasConn(to)
}

var errNoTCP = errors.New("transport: no TCP/TLS transport on this node")
var errNoUDP = errors.New("transport: UDP underlay is disabled")

// Close tears down both underlays.
func (d Dual) Close() error {
	var err error
	if d.TLS != nil {
		err = d.TLS.Close()
	}
	if d.UDP != nil {
		if e := d.UDP.Close(); e != nil {
			err = e
		}
	}
	return err
}
