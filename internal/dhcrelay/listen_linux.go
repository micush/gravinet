//go:build linux

package dhcrelay

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// A relay needs two sockets per link, not one, and they want opposite things.
//
// The client-facing socket must receive broadcasts and must know which link
// they arrived on. Binding the interface's own unicast address would satisfy
// the second and fail the first: a client with no address broadcasts to
// 255.255.255.255, which is not delivered to a socket bound to a unicast
// address. So it binds the wildcard and is confined with SO_BINDTODEVICE —
// everything it receives is then known to have come from that link, which is
// what makes the giaddr stamp trustworthy.
//
// The server-facing socket must reach a server that is, by the entire premise
// of relaying, somewhere else. It cannot be device-confined: the route to the
// server usually leaves by a different interface than the one the clients are
// on — an uplink, a tunnel, an overlay — and SO_BINDTODEVICE constrains both
// egress and delivery, so a confined socket would send the request out the
// wrong interface and then not be delivered the reply when it arrived on the
// right one. It binds the link's own address instead, which is the giaddr the
// server addresses its reply to, and which receives that reply no matter which
// interface it arrives on.
//
// Using one socket for both, as this package did until now, works only when
// the server is on the client's link — the one topology where a relay is not
// needed.
//
// SO_REUSEADDR and SO_REUSEPORT are needed on both because a node relaying for
// several LANs opens a pair of these per LAN, all on port 67. SO_BROADCAST is
// needed on the client-facing socket to reply to 255.255.255.255 for a client
// that has no address to unicast to yet.

// listenClient binds the client-facing socket for one link: the limited
// broadcast address, so client broadcasts arrive, confined to ifName, so their
// link is known.
//
// 255.255.255.255 rather than the wildcard, which is what this bound before
// and would be the obvious choice. Both receive the broadcasts a relay exists
// to hear, and the narrower one is what lets this run beside a DHCP server on
// the same host. Kea takes <iface-addr>:67 whichever socket type it is
// configured for, and sets no SO_REUSEADDR, so a wildcard bind next to it
// fails outright with EADDRINUSE — first come, first served, in whichever
// order the two happened to start. Binding the broadcast address is a
// different tuple, so the collision does not arise.
//
// Nothing is given up by narrowing it. The traffic a relay must catch is
// exactly the traffic from clients that have no address yet, and RFC 2131
// §4.1 has those sent to 255.255.255.255. A client that already holds a lease
// unicasts its renewal to the server itself, routed, with no relay in the
// path — so a relay that never sees it is a relay behaving correctly.
func listenClient(ifName string) (net.PacketConn, error) {
	pc, err := bind(netip.AddrPortFrom(bcast, ServerPort).String(), func(fd uintptr) error {
		// Confining the socket to one device is what makes a wildcard bind
		// safe here — without it, every relay socket on this host would see
		// every other link's broadcasts and stamp them with the wrong giaddr.
		if err := syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, unixSOBindToDevice, ifName); err != nil {
			return fmt.Errorf("binding socket to %s (needs root/CAP_NET_RAW): %w", ifName, err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("client-facing socket: %w", err)
	}
	return pc, nil
}

// listenServer binds the server-facing socket for one link: the link's own
// address, so the source of forwarded requests is the giaddr the server
// replies to, and so that reply is delivered whichever interface it arrives
// on. Deliberately not confined to a device.
func listenServer(self netip.Addr) (net.PacketConn, error) {
	pc, err := bind(netip.AddrPortFrom(self, ServerPort).String(), nil)
	if err != nil {
		return nil, fmt.Errorf("server-facing socket on %s: %w", self, err)
	}
	return pc, nil
}

// bind opens a udp4 socket at addr with the options every relay socket needs,
// plus whatever extra the caller wants set before the bind.
func bind(addr string, extra func(fd uintptr) error) (net.PacketConn, error) {
	var ctlErr error
	lc := net.ListenConfig{
		Control: func(_, _ string, c syscall.RawConn) error {
			return c.Control(func(fd uintptr) {
				set := func(opt int, name string) {
					if ctlErr != nil {
						return
					}
					if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, opt, 1); err != nil {
						ctlErr = fmt.Errorf("setting %s: %w", name, err)
					}
				}
				set(syscall.SO_REUSEADDR, "SO_REUSEADDR")
				set(unixSOReusePort, "SO_REUSEPORT")
				set(syscall.SO_BROADCAST, "SO_BROADCAST")
				if ctlErr == nil && extra != nil {
					ctlErr = extra(fd)
				}
			})
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", addr, err)
	}
	if ctlErr != nil {
		_ = pc.Close()
		return nil, ctlErr
	}
	return pc, nil
}

// Not in the syscall package's exported set on every architecture it builds
// for, and the values are ABI-stable across all of them.
const (
	unixSOReusePort    = 15
	unixSOBindToDevice = 25
)
