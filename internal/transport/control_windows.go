//go:build windows

package transport

import "syscall"

// reusePort is false off Linux; we open one socket per family and spread the
// worker goroutines across it instead.
const reusePort = false

// IP_DONTFRAGMENT / IPV6_DONTFRAG (Winsock values, not exported by Go's
// syscall package for windows). Sourced from Microsoft's own SDK header,
// ws2ipdef.h — "#define IP_DONTFRAGMENT 14" / "#define IPV6_DONTFRAG 14"
// (same numeric value, but at different setsockopt levels: IPPROTO_IP vs
// IPPROTO_IPV6, so this isn't a collision). Setting either makes an
// oversized datagram fail (WSAEMSGSIZE) rather than be silently
// IP-fragmented, the same guarantee control_linux.go/control_darwin.go/
// control_freebsd.go establish on their platforms — see pmtu_cap_darwin.go
// for why that matters to path-MTU discovery.
const (
	ipDontFragment = 14 // IPPROTO_IP level
	ipv6DontFrag   = 14 // IPPROTO_IPV6 level
)

// control sets socket options before bind. network is "udp4"/"udp6".
func control(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		h := syscall.Handle(fd)
		if e := syscall.SetsockoptInt(h, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		if network == "udp6" {
			// Keep v4 and v6 sockets independent so both can hold the same port.
			if e := syscall.SetsockoptInt(h, syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1); e != nil {
				sockErr = e
				return
			}
			_ = syscall.SetsockoptInt(h, syscall.IPPROTO_IPV6, ipv6DontFrag, 1) // best-effort
		} else {
			_ = syscall.SetsockoptInt(h, syscall.IPPROTO_IP, ipDontFragment, 1) // best-effort
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
