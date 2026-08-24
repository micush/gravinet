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
	// Interfaces are the client-facing links to listen on.
	Interfaces []string
	// Servers are the upstream DHCP servers. Each gets a copy of every
	// forwarded request; the client uses whichever answer arrives first and
	// ignores the rest, which is how a relay does redundancy — there is no
	// failover to sequence.
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
	done chan struct{}
	wg   sync.WaitGroup
}

// Start binds a listener on each configured interface and begins forwarding.
//
// One socket per interface, bound to that interface's own address, rather than
// a single wildcard socket. Two reasons, and the second is the one that
// matters: the address bound is the giaddr this relay stamps on requests from
// that link, so the server picks the right subnet to lease from and addresses
// its reply somewhere this node can hear it. A wildcard socket would have to
// guess which link a broadcast arrived on and which of this host's addresses
// to claim, and it would guess wrong on any node with more than one LAN —
// which is every node worth putting a relay on.
//
// An interface that cannot be bound is logged and skipped rather than failing
// the whole relay. A node relaying for three LANs should not lose all three
// because one NIC has no address yet.
func Start(cfg Config, log Logf) (*Relay, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	if len(cfg.Servers) == 0 {
		return nil, errors.New("no upstream DHCP servers configured")
	}
	if len(cfg.Interfaces) == 0 {
		return nil, errors.New("no client-facing interfaces configured")
	}
	r := &Relay{cfg: cfg, log: log, done: make(chan struct{})}
	for _, name := range cfg.Interfaces {
		self, err := ifaceAddr4(name)
		if err != nil {
			log("dhcp relay: %s: %v", name, err)
			continue
		}
		pc, err := listen(name, self)
		if err != nil {
			log("dhcp relay: %s: %v", name, err)
			continue
		}
		r.mu.Lock()
		r.pcs = append(r.pcs, pc)
		r.mu.Unlock()
		r.wg.Add(1)
		go r.serve(pc, name, self)
	}
	r.mu.Lock()
	n := len(r.pcs)
	r.mu.Unlock()
	if n == 0 {
		close(r.done)
		return nil, fmt.Errorf("no configured interface could be listened on")
	}
	log("dhcp relay: listening on %d interface(s), forwarding to %d server(s)", n, len(cfg.Servers))
	return r, nil
}

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
	r.pcs = nil
	r.mu.Unlock()
	for _, pc := range pcs {
		_ = pc.Close()
	}
	r.wg.Wait()
}

// serve reads one interface's socket until it is closed.
func (r *Relay) serve(pc net.PacketConn, name string, self netip.Addr) {
	defer r.wg.Done()
	buf := make([]byte, maxLen)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-r.done:
			default:
				r.log("dhcp relay: %s: read: %v", name, err)
			}
			return
		}
		// The buffer is reused, so everything downstream either acts on it
		// before the next read or copies. handle does the former.
		r.handle(pc, name, self, buf[:n], from)
	}
}

// handle relays one datagram in whichever direction its op code says.
//
// Every rejection is silent past the debug log. This socket receives every
// broadcast on the link, so malformed and irrelevant traffic is the normal
// case rather than an error worth reporting to an operator — a relay that
// logged a line per stray packet would bury the one line that mattered.
func (r *Relay) handle(pc net.PacketConn, name string, self netip.Addr, b []byte, from net.Addr) {
	m, err := parse(b)
	if err != nil {
		return
	}
	switch m.op() {
	case opRequest:
		if err := prepareRequest(m, self, r.cfg.MaxHops); err != nil {
			return
		}
		for _, srv := range r.cfg.Servers {
			if err := sendTo(pc, b, netip.AddrPortFrom(srv, ServerPort)); err != nil {
				r.log("dhcp relay: %s -> %s: %v", name, srv, err)
			}
		}
	case opReply:
		// A reply arriving on the client-facing socket is the server's answer
		// coming back — the relay sent the request from this socket, so this
		// is where the response lands.
		to, err := replyTarget(m, self)
		if err != nil {
			return
		}
		if err := sendTo(pc, b, netip.AddrPortFrom(to, ClientPort)); err != nil {
			r.log("dhcp relay: %s: returning reply to %s: %v", name, to, err)
		}
	}
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
