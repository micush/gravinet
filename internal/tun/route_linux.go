//go:build linux

package tun

// Route installation programs the host routing table so the kernel hands packets
// for a redistributed prefix to this TUN ("<prefix> dev <name>"), or — via
// AddGatewayRoute/DelGatewayRoute — a specific prefix to a specific physical
// gateway/interface instead (used for full-tunnel peer-bypass host routes;
// see gateway_linux.go). It speaks rtnetlink directly — no `ip`/`route`
// binary, no cgo — matching the ioctl-based interface setup in tun_linux.go.

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"
)

// AddRoute installs a route for prefix via this interface with the given metric
// (route priority), equivalent to `ip route replace <prefix> dev <name> metric
// <m>`. Idempotent (replaces an existing one at the same metric).
func (d *Device) AddRoute(p netip.Prefix, metric int) error {
	return d.routeMsg(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE, p, metric)
}

// DelRoute removes the route for prefix on this interface. The metric must match
// the one the route was installed with, because a route's kernel identity
// includes its priority. A missing route is not treated as an error.
func (d *Device) DelRoute(p netip.Prefix, metric int) error {
	return d.routeMsg(syscall.RTM_DELROUTE, 0, p, metric)
}

// IfIndex returns this TUN's kernel interface index — e.g. for passing as
// DefaultGateway's excludeIfIndex, so a physical-gateway lookup never
// mistakes gravinet's own tunnel-routed default for the real one.
func (d *Device) IfIndex() (int32, error) {
	s, err := ctlSocket(syscall.AF_INET)
	if err != nil {
		return 0, err
	}
	defer syscall.Close(s)
	req := d.ifreqWithName()
	if err := ioctl(uintptr(s), cSIOCGIFINDEX, unsafe.Pointer(&req[0])); err != nil {
		return 0, fmt.Errorf("get ifindex: %w", err)
	}
	return int32(binary.NativeEndian.Uint32(req[ifnameSize:])), nil
}

func rtaAlign(n int) int { return (n + 3) &^ 3 }

// rtattr serializes a single rtnetlink attribute (header + payload, padded to a
// 4-byte boundary).
func rtattr(typ int, data []byte) []byte {
	l := syscall.SizeofRtAttr + len(data) // 4 + len
	b := make([]byte, rtaAlign(l))
	binary.NativeEndian.PutUint16(b[0:2], uint16(l))
	binary.NativeEndian.PutUint16(b[2:4], uint16(typ))
	copy(b[4:], data)
	return b
}

func (d *Device) routeMsg(msgType int, extraFlags int, p netip.Prefix, metric int) error {
	p = p.Masked()
	idx, err := d.IfIndex()
	if err != nil {
		return err
	}
	addr := p.Addr()
	family := syscall.AF_INET
	scope := byte(syscall.RT_SCOPE_LINK) // on-link dev route (v4)
	var dst []byte
	if addr.Is4() {
		a := addr.As4()
		dst = a[:]
	} else {
		family = syscall.AF_INET6
		scope = byte(syscall.RT_SCOPE_UNIVERSE) // v6 dev routes are global-scope
		a := addr.As16()
		dst = a[:]
	}
	return sendRouteMsg(msgType, extraFlags, family, dst, p.Bits(), syscall.RT_TABLE_MAIN, scope, idx, nil, metric)
}

// AddGatewayRoute installs "<prefix> via <gateway> dev <ifIndex>" into the
// main table — the shape a full-tunnel peer-bypass host route needs, unlike
// AddRoute's on-link "<prefix> dev <this-tun>": the mesh's own underlay
// traffic to a peer has to keep leaving via the real physical
// gateway/interface, not this tun, once a full-tunnel default route is
// active on it (see internal/mesh/routes.go and this package's
// DefaultGateway, which supplies ifIndex/gateway). prefix and gateway must
// be the same address family. Idempotent (replaces an existing one at the
// same metric) — not a Device method, since it has nothing to do with this
// process's own tun device; it targets whatever physical interface ifIndex
// names.
func AddGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return gatewayRouteMsg(syscall.RTM_NEWROUTE, syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE, p, gateway, ifIndex, metric)
}

// DelGatewayRoute removes a route previously installed by AddGatewayRoute.
// The metric must match the one it was installed with — same reason as
// DelRoute's: a route's kernel identity includes its priority. A missing
// route is not treated as an error.
func DelGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return gatewayRouteMsg(syscall.RTM_DELROUTE, 0, p, gateway, ifIndex, metric)
}

func gatewayRouteMsg(msgType, extraFlags int, p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	p = p.Masked()
	addr := p.Addr()
	if addr.Is4() != gateway.Is4() {
		return fmt.Errorf("gateway route: prefix %s and gateway %s are different address families", p, gateway)
	}
	family := syscall.AF_INET
	var dst, gw []byte
	if addr.Is4() {
		a := addr.As4()
		dst = a[:]
		g := gateway.As4()
		gw = g[:]
	} else {
		family = syscall.AF_INET6
		a := addr.As16()
		dst = a[:]
		g := gateway.As16()
		gw = g[:]
	}
	// A route reached via a gateway is never on-link by definition, so scope
	// is universal regardless of family — unlike routeMsg's on-link routes,
	// which use RT_SCOPE_LINK for v4 specifically because those are on-link.
	return sendRouteMsg(msgType, extraFlags, family, dst, p.Bits(), syscall.RT_TABLE_MAIN, syscall.RT_SCOPE_UNIVERSE, ifIndex, gw, metric)
}

// sendRouteMsg is the shared rtnetlink plumbing behind both an on-link route
// (routeMsg's "<prefix> dev <tun>") and a gateway route (gatewayRouteMsg's
// "<prefix> via <gateway> dev <if>"): build one RTM_NEWROUTE/RTM_DELROUTE
// request, send it, and parse the ACK. gateway is nil for an on-link route.
//
// rtm_protocol (rtm[5]) is set to RTPROT_BOOT on an add/replace, matching
// what `ip route` itself defaults to for a manually-installed route, but
// deliberately left at RTPROT_UNSPEC (0, the zero value — not set at all)
// on a delete. This isn't cosmetic: the kernel's route-delete matching
// (fib_table_delete, both IPv4 and IPv6) treats rtm_protocol as an exact-
// match selector whenever it's nonzero — "delete the route matching this
// dst/table/scope/type/oif/gateway/priority *and* installed by protocol
// exactly this" — and skips that check entirely when it's zero. Every
// route gravinet ever adds itself uses RTPROT_BOOT on both the add and the
// matching delete, so this made no observable difference there — add and
// delete stayed symmetric by construction. It broke the one caller that
// isn't deleting a route gravinet itself installed:
// gateway_linux.go's DemoteDefaultRoute deletes the pre-existing *physical*
// default route once its demoted-metric replacement is in place, and that
// route was never gravinet's to begin with — installed by a DHCP client
// (RTPROT_DHCP), NetworkManager, systemd-networkd, or a static config, each
// with its own real protocol value, essentially never RTPROT_BOOT. A
// delete that hardcoded RTPROT_BOOT there didn't error — the kernel
// returns ESRCH for "no route matches this exact selector," which
// sendRouteMsg's own error handling below already treats as "already
// gone," i.e. success — but silently deleted nothing: the real
// DHCP-installed route stayed in the table at its original metric,
// alongside gravinet's own demoted-metric copy of it, alongside gravinet's
// own full-tunnel default. Three default routes where there should be two,
// discovered from a live `ip route` a demotion had run against.
func sendRouteMsg(msgType, extraFlags int, family int, dst []byte, dstBits int, table, scope byte, oif int32, gateway []byte, metric int) error {
	// struct rtmsg (12 bytes)
	rtm := make([]byte, syscall.SizeofRtMsg)
	rtm[0] = byte(family)  // rtm_family
	rtm[1] = byte(dstBits) // rtm_dst_len
	rtm[4] = table
	if msgType != syscall.RTM_DELROUTE {
		rtm[5] = syscall.RTPROT_BOOT // rtm_protocol; left 0 (RTPROT_UNSPEC) on delete — see doc comment above
	}
	rtm[6] = scope
	rtm[7] = syscall.RTN_UNICAST

	oifb := make([]byte, 4)
	binary.NativeEndian.PutUint32(oifb, uint32(oif))

	body := append([]byte{}, rtm...)
	body = append(body, rtattr(syscall.RTA_DST, dst)...)
	body = append(body, rtattr(syscall.RTA_OIF, oifb)...)
	if gateway != nil {
		body = append(body, rtattr(syscall.RTA_GATEWAY, gateway)...)
	}
	// RTA_PRIORITY carries the route metric. Only emit it when non-zero: a metric
	// of 0 is the kernel default, and on delete the absence of RTA_PRIORITY
	// matches that default — keeping add/delete symmetric so the right route is
	// removed. (RTA_PRIORITY == 6 in the rtnetlink attribute enum.)
	if metric > 0 {
		const rtaPriority = 6
		prio := make([]byte, 4)
		binary.NativeEndian.PutUint32(prio, uint32(metric))
		body = append(body, rtattr(rtaPriority, prio)...)
	}

	// struct nlmsghdr (16 bytes) + body
	total := syscall.SizeofNlMsghdr + len(body)
	msg := make([]byte, syscall.SizeofNlMsghdr)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(total))
	binary.NativeEndian.PutUint16(msg[4:6], uint16(msgType))
	binary.NativeEndian.PutUint16(msg[6:8], uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|extraFlags))
	binary.NativeEndian.PutUint32(msg[8:12], 1) // seq
	msg = append(msg, body...)

	// SOCK_CLOEXEC: same reasoning as dumpDefaultRoutes in gateway_linux.go —
	// closed well before returning, but atomic-at-open closes the narrow
	// concurrent-exec race a deferred close alone can't.
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Bind(fd, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}
	if err := syscall.Sendto(fd, msg, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return err
	}

	resp := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, resp, 0)
	if err != nil {
		return err
	}
	if n < syscall.SizeofNlMsghdr+4 {
		return nil // no error payload to parse; assume success
	}
	if binary.NativeEndian.Uint16(resp[4:6]) == syscall.NLMSG_ERROR {
		errno := int32(binary.NativeEndian.Uint32(resp[syscall.SizeofNlMsghdr : syscall.SizeofNlMsghdr+4]))
		if errno == 0 {
			return nil // ack: success
		}
		e := syscall.Errno(-errno)
		// Deleting a route that isn't there is fine (e.g. already withdrawn).
		if msgType == syscall.RTM_DELROUTE && (e == syscall.ESRCH || e == syscall.ENOENT) {
			return nil
		}
		return fmt.Errorf("rtnetlink route op: %w", e)
	}
	return nil
}
