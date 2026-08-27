package dhcrelay

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

// Logf is the relay's log sink. A plain function rather than an interface so
// the caller can hand it logx.Warnf directly and tests can hand it nothing.
type Logf func(format string, args ...any)

// Config is what the relay needs to run. Built from config.DHCPConfig by the
// caller, so this package does not depend on the config schema and can be
// tested without one.
type Config struct {
	// Links are the client-facing interfaces to listen on, each with its own
	// upstream servers and hop limit.
	Links []Link
}

// Link is one client-facing interface and where requests arriving on it go.
//
// Servers and MaxHops belong to the link rather than to the relay as a whole,
// because the socket already does: there is one per interface, bound to that
// interface's own address, and it was only ever the form above that made the
// two links share a destination.
type Link struct {
	// Iface is the client-facing interface to listen on.
	Iface string
	// Servers are the upstream DHCP servers for this link. Each gets a copy
	// of every forwarded request; the client uses whichever answer arrives
	// first and ignores the rest, which is how a relay does redundancy —
	// there is no failover to sequence.
	Servers []netip.Addr
	// MaxHops bounds the relay count. 0 means RFC 1542's default of 4.
	MaxHops int
}

// Relay is a running relay agent. Start returns one; Stop shuts it down.
type Relay struct {
	cfg  Config
	log  Logf
	mu   sync.Mutex
	pcs  []net.PacketConn
	live []string // links that actually bound, for Listening
	done chan struct{}
	wg   sync.WaitGroup
}

// link is one link's running state: the two sockets it needs and the config
// they serve. Paired in a struct because the two are useless apart — a request
// read on client is forwarded from server, and the reply read on server is
// returned from client.
type link struct {
	cfg    Link
	self   netip.Addr
	client net.PacketConn // wildcard, confined to cfg.Iface: hears clients
	server net.PacketConn // bound to self: talks to the upstream servers
}

// Listening reports the links this relay actually bound, which is not always
// the links it was configured with: an interface that cannot be bound is
// logged and skipped so one bad NIC does not take the other LANs down with it.
//
// Exists so the page can say what is running rather than what was asked for.
// Those two drift for ordinary reasons — a link with no address yet, a NIC
// renamed by a kernel upgrade — and a page that only ever reflects the request
// reports a relay running on interfaces it never bound.
func (r *Relay) Listening() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.live...)
}

// Start binds each configured interface's sockets and begins forwarding.
//
// Two sockets per interface rather than one, for reasons set out at length in
// listen_linux.go: the client-facing side must hear broadcasts and know which
// link they came from, the server-facing side must reach a server that is by
// definition elsewhere, and no single socket does both.
//
// An interface that cannot be bound is logged and skipped rather than failing
// the whole relay. A node relaying for three LANs should not lose all three
// because one NIC has no address yet. Both of a link's sockets must bind for
// the link to count: a link with only one is not half-working, it is a link
// that accepts requests and silently never answers them, which is worse than
// one that is plainly absent from Listening.
func Start(cfg Config, log Logf) (*Relay, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	if len(cfg.Links) == 0 {
		return nil, errors.New("no client-facing interfaces configured")
	}
	// A link with nowhere to forward to is a configuration error rather than
	// a host condition, so it fails here instead of being logged and skipped
	// the way an unbindable interface is.
	for _, l := range cfg.Links {
		if len(l.Servers) == 0 {
			return nil, fmt.Errorf("%s: no upstream DHCP servers configured", l.Iface)
		}
	}
	r := &Relay{cfg: cfg, log: log, done: make(chan struct{})}
	for _, l := range cfg.Links {
		self, err := ifaceAddr4(l.Iface)
		if err != nil {
			log("dhcp relay: %s: %v", l.Iface, err)
			continue
		}
		client, err := listenClient(l.Iface)
		if err != nil {
			log("dhcp relay: %s: %v", l.Iface, err)
			continue
		}
		server, err := listenServer(self)
		if err != nil {
			// Half a link is not a working link, so give the first socket
			// back rather than leaving it bound and deaf.
			_ = client.Close()
			log("dhcp relay: %s: %v", l.Iface, err)
			continue
		}
		lk := &link{cfg: l, self: self, client: client, server: server}
		r.mu.Lock()
		r.pcs = append(r.pcs, client, server)
		r.live = append(r.live, l.Iface)
		r.mu.Unlock()
		r.wg.Add(2)
		go r.serve(lk, client, fromClient)
		go r.serve(lk, server, fromServer)
	}
	r.mu.Lock()
	n := len(r.live)
	r.mu.Unlock()
	if n == 0 {
		close(r.done)
		return nil, fmt.Errorf("no configured interface could be listened on")
	}
	log("dhcp relay: listening on %d interface(s)", n)
	return r, nil
}

// side says which of a link's two sockets a datagram was read on. Direction is
// decided by this rather than by the op code alone: a reply read on the
// client-facing socket is another server answering on the client's LAN, and
// forwarding it is not this relay's job.
type side int

const (
	fromClient side = iota
	fromServer
)

// Stop closes every listener and waits for the readers to finish. Safe to call
// twice, because a reload that changes nothing still asks for a stop.
func (r *Relay) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	pcs := r.pcs
	r.pcs, r.live = nil, nil
	r.mu.Unlock()
	for _, pc := range pcs {
		_ = pc.Close()
	}
	r.wg.Wait()
}

// serve reads one of a link's sockets until it is closed.
//
// The link travels with the socket rather than being looked up per packet:
// which servers a request goes to and how many hops it may already have
// crossed are properties of the link it arrived on, and this is the one place
// that pairing is known for certain.
func (r *Relay) serve(lk *link, pc net.PacketConn, s side) {
	defer r.wg.Done()
	buf := make([]byte, maxLen)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-r.done:
			default:
				r.log("dhcp relay: %s: read: %v", lk.cfg.Iface, err)
			}
			return
		}
		// The buffer is reused, so everything downstream either acts on it
		// before the next read or copies. handle does the former.
		r.handle(lk, s, buf[:n])
	}
}

// handle relays one datagram across a link, in the direction its arrival
// socket implies.
//
// Every rejection is silent past the debug log. These sockets receive every
// broadcast on the link, so malformed and irrelevant traffic is the normal
// case rather than an error worth reporting to an operator — a relay that
// logged a line per stray packet would bury the one line that mattered.
func (r *Relay) handle(lk *link, s side, b []byte) {
	m, err := parse(b)
	if err != nil {
		return
	}
	switch {
	case s == fromClient && m.op() == opRequest:
		// A client's request, read on the LAN. Stamp it and send it out the
		// server-facing socket, whose source address is the giaddr just
		// stamped — which is how the reply finds its way back here.
		if err := prepareRequest(m, lk.self, lk.cfg.MaxHops); err != nil {
			return
		}
		for _, srv := range lk.cfg.Servers {
			if err := sendTo(lk.server, b, netip.AddrPortFrom(srv, ServerPort)); err != nil {
				r.log("dhcp relay: %s -> %s: %v", lk.cfg.Iface, srv, err)
			}
		}
	case s == fromServer && m.op() == opReply:
		// The server's answer, addressed to this link's giaddr and delivered
		// regardless of which interface it arrived on. It goes back out the
		// client-facing socket, because that is the one confined to the LAN
		// the client is on.
		to, err := replyTarget(m, lk.self)
		if err != nil {
			return
		}
		if err := sendTo(lk.client, b, netip.AddrPortFrom(to, ClientPort)); err != nil {
			r.log("dhcp relay: %s: returning reply to %s: %v", lk.cfg.Iface, to, err)
		}
	}
	// Anything else is traffic this relay has no part in: a reply on the LAN
	// is another server answering the client directly, and a request on the
	// server-facing socket is somebody else's relay pointed at this address.
}

// ifaceAddr4 returns the interface's own IPv4 address, which becomes the
// giaddr for everything relayed off that link.
//
// An interface with no IPv4 address cannot relay: there is nothing to put in
// giaddr, so the server would have no way to choose a subnet and no way to
// address its reply. Reported rather than defaulted to zero, because a zero
// giaddr means "not relayed" and would make the server answer as though the
// client were on its own link.
func ifaceAddr4(name string) (netip.Addr, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("no interface by that name on this host")
	}
	if ifi.Flags&net.FlagUp == 0 {
		return netip.Addr{}, fmt.Errorf("interface is down")
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		addr, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if addr.Is4() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() {
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("interface has no IPv4 address to use as the relay address (giaddr)")
}

func sendTo(pc net.PacketConn, b []byte, to netip.AddrPort) error {
	_, err := pc.WriteTo(b, net.UDPAddrFromAddrPort(to))
	return err
}
