//go:build freebsd

package transport

import "syscall"

// reusePort is false off Linux; we open one socket per family and spread the
// worker goroutines across it instead.
const reusePort = false

// control sets socket options before bind. network is "udp4"/"udp6". FreeBSD
// exports real IP_DONTFRAG/IPV6_DONTFRAG socket options (unlike macOS's
// documented-but-silently-broken pair — see internal/mesh/pmtu_cap_darwin.go
// — and unlike OpenBSD, which has no IPv4 equivalent at all — see
// control_openbsd.go), so, like Linux, an oversized probe genuinely fails
// with EMSGSIZE here instead of being silently IP-fragmented, which is what
// lets a successful probe in pmtu.go mean "this exact datagram crossed the
// path as one piece."
func control(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		if network == "udp6" {
			// Keep v4 and v6 sockets independent so both can hold the same port.
			if e := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1); e != nil {
				sockErr = e
				return
			}
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_DONTFRAG, 1) // best-effort
		} else {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_DONTFRAG, 1) // best-effort
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
