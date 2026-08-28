//go:build linux

package dhcrelay

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// rawSender puts a reply on the client link addressed to a hardware address,
// bypassing neighbour resolution entirely. See frame.go for why that is
// necessary at all.
type rawSender struct {
	fd    int
	index int
	name  string
}

// newRawSender opens the packet socket for one link.
//
// SOCK_DGRAM rather than SOCK_RAW: with SOCK_DGRAM the kernel builds the
// Ethernet header from the destination address given to each Sendto, so this
// supplies the IP header downwards and nothing above it. SOCK_RAW would mean
// building the Ethernet header here too, including this interface's own
// source address, for no gain.
//
// The protocol argument is 0, not ETH_P_IP. It selects what the socket
// *receives*, and this socket only ever sends — binding it to ETH_P_IP would
// queue a copy of every IP frame on the link into a receive buffer nothing
// reads. The protocol that matters is the one in each Sendto's sockaddr,
// which is set there.
func newRawSender(ifName string) (directSender, error) {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return nil, fmt.Errorf("no interface by that name on this host")
	}
	fd, err := syscall.Socket(syscall.AF_PACKET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("opening a packet socket (needs root/CAP_NET_RAW): %w", err)
	}
	syscall.CloseOnExec(fd)
	return &rawSender{fd: fd, index: ifi.Index, name: ifName}, nil
}

// sendDirect frames payload as IPv4/UDP from srcIP to dstIP and puts it on the
// link addressed to dstMAC.
func (s *rawSender) sendDirect(dstMAC net.HardwareAddr, srcIP, dstIP netip.Addr, payload []byte) error {
	if len(dstMAC) != ethAddrLen {
		return fmt.Errorf("hardware address is %d bytes, need %d", len(dstMAC), ethAddrLen)
	}
	if !srcIP.Is4() || !dstIP.Is4() {
		return fmt.Errorf("direct delivery is IPv4 only")
	}
	sa := &syscall.SockaddrLinklayer{
		Protocol: htons(syscall.ETH_P_IP),
		Ifindex:  s.index,
		Halen:    ethAddrLen,
	}
	copy(sa.Addr[:], dstMAC)
	return syscall.Sendto(s.fd, ip4udp(srcIP, dstIP, ServerPort, ClientPort, payload), 0, sa)
}

func (s *rawSender) Close() error { return syscall.Close(s.fd) }
