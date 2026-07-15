package transport

import (
	"errors"
	"net/netip"
)

// Dual combines the UDP underlay with the TCP/TLS fallback behind a single
// Sender. A send prefers an existing TLS connection to the destination — which
// exists when the peer reached us over TCP, or when we dialed the fallback
// because UDP couldn't get through — and otherwise goes over UDP. Inbound
// datagrams from both transports are delivered to the same Handler, so the
// engine never has to know which underlay carried a packet.
//
// TLS may be nil when the fallback listener failed to bind (e.g. port 443 in
// use or unprivileged); in that case Dual is just the UDP transport. UDP may
// also be nil — the operator turned it off entirely (see config.PrimaryPort's
// 0-means-disabled convention) — in which case Dual is just the TLS fallback;
// Config.Validate refuses to let both be nil at once, so at least one is
// always live.
type Dual struct {
	UDP *Transport
	TLS *TLSTransport
}

// Send routes to TLS when a live connection to `to` exists, else to UDP. When
// UDP is disabled and there's no live TLS connection yet either, this errors
// rather than dialing — same as any other UDP send that hasn't found its
// peer yet — and the caller's normal fallback-on-failure handling (see
// mesh.Engine.ensureFallback) is what actually dials TLS.
func (d Dual) Send(to netip.AddrPort, payload []byte) error {
	if d.TLS != nil && d.TLS.HasConn(to) {
		return d.TLS.Send(to, payload)
	}
	if d.UDP == nil {
		return errNoUDP
	}
	return d.UDP.Send(to, payload)
}

// DialFallback opens a TCP/TLS connection to a peer's fallback endpoint so that
// subsequent sends to that endpoint go over TLS. The engine calls this when UDP
// can't reach a peer. Returns an error if the fallback is unavailable.
func (d Dual) DialFallback(to netip.AddrPort) error {
	if d.TLS == nil {
		return errNoFallback
	}
	return d.TLS.Dial(to)
}

// HasFallback reports whether a live TLS connection to the endpoint exists.
func (d Dual) HasFallback(to netip.AddrPort) bool {
	return d.TLS != nil && d.TLS.HasConn(to)
}

var errNoFallback = errors.New("transport: TCP/TLS fallback not available")
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
