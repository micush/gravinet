//go:build darwin

package transport

import "syscall"

// reusePort is false off Linux; we open one socket per family and spread the
// worker goroutines across it instead.
const reusePort = false

// Don't-fragment socket options (macOS values). Setting them makes an oversized
// datagram fail (EMSGSIZE) rather than be silently IP-fragmented, so each
// application-layer fragment stays a single un-fragmented underlay datagram and
// probe-based path-MTU discovery stays honest.
const (
	ipDontFrag   = 28 // IP_DONTFRAG
	ipv6DontFrag = 62 // IPV6_DONTFRAG
)

// control sets socket options before bind. network is "udp4"/"udp6".
func control(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		if network == "udp6" {
			if e := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1); e != nil {
				sockErr = e
				return
			}
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, ipv6DontFrag, 1) // best-effort
		} else {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipDontFrag, 1) // best-effort
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
