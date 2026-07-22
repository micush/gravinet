//go:build openbsd

package transport

import "syscall"

// reusePort is false off Linux; we open one socket per family and spread the
// worker goroutines across it instead.
const reusePort = false

// control sets socket options before bind. network is "udp4"/"udp6".
//
// Two things OpenBSD doesn't need or have, unlike every other platform here:
//   - IPV6_V6ONLY isn't set: on OpenBSD, IPv6 sockets are always v6-only
//     (there's no IPv4-mapped-address support at all), and the option is
//     documented as read-only there — setting it would just be a guaranteed
//     no-op syscall, not a real toggle.
//   - IP_DONTFRAG isn't set for v4: OpenBSD's IPv4 stack has no such option
//     (absent from ip(4)'s documented option list, and Go's syscall package
//     doesn't export the constant for this GOOS). IPV6_DONTFRAG does exist
//     and is set below. See internal/mesh/pmtu_cap_openbsd.go for how PMTU
//     discovery stays honest for v4 despite the missing guarantee.
func control(network, address string, c syscall.RawConn) error {
	var sockErr error
	err := c.Control(func(fd uintptr) {
		if e := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); e != nil {
			sockErr = e
			return
		}
		if network == "udp6" {
			_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_DONTFRAG, 1) // best-effort
		}
	})
	if err != nil {
		return err
	}
	return sockErr
}
