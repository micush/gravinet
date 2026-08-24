//go:build linux

package dhcrelay

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// listen binds one interface's relay socket.
//
// The binding is the interesting part, and the obvious approach does not work.
// Binding to the interface's own unicast address would give a socket that
// never sees a single DHCP request: a client with no address broadcasts to
// 255.255.255.255, and a socket bound to a unicast address is not delivered
// broadcast traffic. Binding the wildcard instead receives the broadcasts but
// loses the one fact the relay exists to record — which link they arrived on,
// and therefore which subnet the server should lease from.
//
// SO_BINDTODEVICE is what resolves that: bind the wildcard so broadcasts are
// delivered, then confine the socket to one interface so everything it
// receives is known to have come from that link. The interface's own address
// is still what goes in giaddr; it is passed in rather than bound.
//
// SO_REUSEADDR and SO_REUSEPORT are both needed because a node relaying for
// several LANs opens one of these per LAN, all on port 67. SO_BROADCAST is
// needed to send the reply back to 255.255.255.255 for a client that has no
// address to unicast to yet.
func listen(ifName string, self netip.Addr) (net.PacketConn, error) {
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
				if ctlErr == nil {
					// Confining the socket to one device is what makes a
					// wildcard bind safe here — without it, every relay
					// socket on this host would see every other link's
					// broadcasts and stamp them with the wrong giaddr.
					if err := syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, unixSOBindToDevice, ifName); err != nil {
						ctlErr = fmt.Errorf("binding socket to %s (needs root/CAP_NET_RAW): %w", ifName, err)
					}
				}
			})
		},
	}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf(":%d", ServerPort))
	if err != nil {
		return nil, fmt.Errorf("listening on port %d: %w", ServerPort, err)
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
