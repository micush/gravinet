package webadmin

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
)

// sendRelayedDiscover emits a DHCPDISCOVER shaped as a relay agent would
// forward it: hops already counted, giaddr stamped with the relay's address,
// and sent as a plain unicast to the server rather than broadcast.
//
// Built and sent with an explicit IP header so the source can be the relay's
// address rather than this host's. That is what makes the test faithful: the
// server must answer a request whose source and giaddr belong to a subnet it
// has no interface on, arriving over a path it knows nothing about.
func sendRelayedDiscover(giaddr, server string) error {
	src, dst := net.ParseIP(giaddr).To4(), net.ParseIP(server).To4()
	if src == nil || dst == nil {
		return fmt.Errorf("bad addresses %q -> %q", giaddr, server)
	}

	bootp := make([]byte, 236)
	bootp[0], bootp[1], bootp[2] = 1, 1, 6 // BOOTREQUEST, ethernet, 6-byte mac
	bootp[3] = 1                           // hops: one relay already in the path
	binary.BigEndian.PutUint32(bootp[4:], 0xC0FFEE)
	copy(bootp[24:28], src)                                     // giaddr
	copy(bootp[28:34], []byte{0x0c, 0xa5, 0x3b, 0xab, 0, 0x99}) // chaddr
	payload := append(bootp, 99, 130, 83, 99)                   // magic cookie
	payload = append(payload, 53, 1, 1, 255)                    // DISCOVER, end

	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[0:], 67)
	binary.BigEndian.PutUint16(udp[2:], 67)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(payload)))
	copy(udp[8:], payload)
	ph := make([]byte, 0, 12+len(udp))
	ph = append(ph, src...)
	ph = append(ph, dst...)
	ph = append(ph, 0, 17, byte(len(udp)>>8), byte(len(udp)))
	binary.BigEndian.PutUint16(udp[6:], checksum(append(ph, udp...)))

	ip := make([]byte, 20)
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(20+len(udp)))
	binary.BigEndian.PutUint16(ip[4:], 0x1234)
	copy(ip[12:16], src)
	copy(ip[16:20], dst)
	binary.BigEndian.PutUint16(ip[10:], checksum(ip))

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 1); err != nil {
		return err
	}
	var to syscall.SockaddrInet4
	copy(to.Addr[:], dst)
	return syscall.Sendto(fd, append(ip, udp...), 0, &to)
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum >> 16) + (sum & 0xFFFF)
	}
	return ^uint16(sum)
}
