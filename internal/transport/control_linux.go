//go:build linux

package transport

import "syscall"

// reusePort is true on Linux, enabling one socket per worker on the same port
// for lock-free, kernel-balanced parallel receive.
const reusePort = true

// soReusePort is the Linux value of SO_REUSEPORT (not exported by the stdlib
// syscall package on all versions, so defined here).
const soReusePort = 15

// Don't-fragment / kernel PMTU-discovery socket options (Linux values, not all
// exported by the stdlib syscall package). Setting the discover mode to
// IP_PMTUDISC_DO sets the DF bit on every datagram: an oversized one is dropped
// (the send returns EMSGSIZE) instead of being silently IP-fragmented. That is
// what keeps each application-layer fragment to a single un-fragmented underlay
// datagram — making overlay->underlay fragmentation reliable under load — and
// makes our probe-based path-MTU discovery honest (an oversized probe fails
// instead of succeeding via in-kernel fragment/reassembly).
const (
	ipMTUDiscover   = 10 // IP_MTU_DISCOVER
	ipv6MTUDiscover = 23 // IPV6_MTU_DISCOVER
	pmtuDiscDo      = 2  // IP_PMTUDISC_DO / IPV6_PMTUDISC_DO
)

// control sets socket options before bind. network is "udp4"/"udp6".
func control(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1); e != nil {
			sockErr = e
			return
		}
		if network == "udp6" {
			// Keep v4 and v6 sockets independent so both can hold the same port.
			if e := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1); e != nil {
				sockErr = e
				return
			}
			// Best-effort: set DF + PMTU discovery on the v6 socket.
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6MTUDiscover, pmtuDiscDo)
		} else {
			// Best-effort: set DF + PMTU discovery on the v4 socket. Non-fatal —
			// without it we fall back to the kernel's default (may IP-fragment).
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipMTUDiscover, pmtuDiscDo)
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
