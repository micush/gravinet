//go:build freebsd

package tun

// FreeBSD's counterpart to routeread_linux.go: dump the kernel routing table
// over PF_ROUTE so gravinet can forward prefixes FRR (net/frr9) or any other
// daemon has pointed at a mesh peer, without those prefixes being restated to
// gravinet.
//
// Simpler than Linux in one respect that matters: FreeBSD has no nexthop
// objects, so every gatewayed route carries its gateway inline and there is no
// second dump to resolve. Fiddlier in another: the netmask arrives as a
// truncated sockaddr, so the prefix length has to be counted out of whatever
// bytes were actually sent rather than read from a field.

import (
	"fmt"
	"net/netip"
	"syscall"
)

// OSRoute is one kernel route whose traffic leaves via a given interface, with
// the gateway the kernel would have used.
type OSRoute struct {
	Prefix  netip.Prefix
	Gateway netip.Addr
}

// ListRoutesVia returns every routing-table entry for the given address family
// whose output interface is ifIndex and which has a gateway. family is an
// AF_INET/AF_INET6 constant, matching the Linux signature.
func ListRoutesVia(family int, ifIndex int32) ([]OSRoute, error) {
	rib, err := syscall.RouteRIB(syscall.NET_RT_DUMP, 0)
	if err != nil {
		return nil, fmt.Errorf("route dump: %w", err)
	}
	msgs, err := syscall.ParseRoutingMessage(rib)
	if err != nil {
		return nil, fmt.Errorf("route parse: %w", err)
	}
	var out []OSRoute
	for _, m := range msgs {
		rm, ok := m.(*syscall.RouteMessage)
		if !ok {
			continue
		}
		// RTF_GATEWAY is the filter that matters: a route without one is
		// on-link and says nothing about which peer owns the prefix.
		if rm.Header.Flags&syscall.RTF_UP == 0 || rm.Header.Flags&syscall.RTF_GATEWAY == 0 {
			continue
		}
		if int32(rm.Header.Index) != ifIndex {
			continue
		}
		addrs, err := syscall.ParseRoutingSockaddr(rm)
		if err != nil || len(addrs) <= syscall.RTAX_GATEWAY {
			continue
		}
		dst, ok := sockaddrToAddr(addrs[syscall.RTAX_DST], family)
		if !ok {
			continue
		}
		gw, ok := sockaddrToAddr(addrs[syscall.RTAX_GATEWAY], family)
		if !ok {
			continue // e.g. a link-layer gateway: not a peer overlay address
		}
		bits := dst.BitLen()
		if rm.Header.Flags&syscall.RTF_HOST == 0 && len(addrs) > syscall.RTAX_NETMASK {
			bits = maskBits(addrs[syscall.RTAX_NETMASK], dst.BitLen())
		}
		p := netip.PrefixFrom(dst, bits)
		if !p.IsValid() {
			continue
		}
		out = append(out, OSRoute{Prefix: p.Masked(), Gateway: gw})
	}
	return out, nil
}

// sockaddrToAddr converts a routing sockaddr to a netip.Addr of the requested
// family, reporting false for a nil entry or a family mismatch (the dump is
// not family-filtered, and a link-layer sockaddr appears where a gateway is an
// interface rather than an address).
func sockaddrToAddr(sa syscall.Sockaddr, family int) (netip.Addr, bool) {
	switch v := sa.(type) {
	case *syscall.SockaddrInet4:
		if family != syscall.AF_INET {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4(v.Addr), true
	case *syscall.SockaddrInet6:
		if family != syscall.AF_INET6 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom16(v.Addr), true
	}
	return netip.Addr{}, false
}

// maskBits counts the prefix length out of a netmask sockaddr.
//
// BSD truncates a netmask sockaddr to its significant bytes — a /8 arrives
// carrying one byte, not four — so trailing zero bytes are simply absent and
// the parser fills them in as zeros. Counting leading one-bits therefore gives
// the right answer whether the mask was sent whole or short, which reading a
// length field could not.
func maskBits(sa syscall.Sockaddr, max int) int {
	var b []byte
	switch v := sa.(type) {
	case *syscall.SockaddrInet4:
		b = v.Addr[:]
	case *syscall.SockaddrInet6:
		b = v.Addr[:]
	default:
		return max // no usable mask: treat as a host route
	}
	bits := 0
	for _, c := range b {
		if c == 0xff {
			bits += 8
			continue
		}
		for ; c&0x80 != 0; c <<= 1 {
			bits++
		}
		break
	}
	if bits > max {
		return max
	}
	return bits
}
