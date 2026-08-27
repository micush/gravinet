//go:build !linux

package dhcrelay

import (
	"fmt"
	"net"
	"net/netip"
)

// Not implemented off Linux, and refused rather than approximated.
//
// The relay depends on binding a wildcard socket and then confining it to one
// interface, which is SO_BINDTODEVICE. The BSDs spell the equivalent
// differently (IP_RECVIF plus a per-packet source lookup) and Windows differently
// again, and a relay that guessed the arrival link would stamp the wrong giaddr
// — which does not fail, it hands clients addresses from another LAN's subnet.
// Refusing to start says so; guessing would not.
func listenClient(ifName string) (net.PacketConn, error) {
	return nil, fmt.Errorf("the DHCP relay is implemented on Linux only")
}

// Unreachable in practice, since listenClient is called first and refuses, but
// present so this file matches the interface the Linux build provides.
func listenServer(_ netip.Addr) (net.PacketConn, error) {
	return nil, fmt.Errorf("the DHCP relay is implemented on Linux only")
}
