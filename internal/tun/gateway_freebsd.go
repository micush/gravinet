//go:build freebsd

package tun

// FreeBSD physical default-gateway detection and gateway-routed host
// routes, via the BSD routing socket family: a sysctl(CTL_NET, PF_ROUTE,
// ...) dump for reading the table (the standard mechanism — matching what
// `netstat -rn`/`route -n get` and golang.org/x/net/route's FetchRIB use;
// a routing *socket* write only returns one route per RTM_GET, which can't
// tell gravinet's own already-installed route apart from the physical one,
// same problem as a single Linux `ip route get` — see gateway_linux.go's
// doc comment), and RTM_ADD/RTM_DELETE messages written to an AF_ROUTE
// socket for mutating it.
//
// Every struct field layout and alignment constant below is copied
// verbatim from this Go toolchain's own stdlib source
// (src/syscall/ztypes_freebsd_amd64.go, zerrors_freebsd_amd64.go,
// route_bsd.go's rsaAlignOf) rather than derived from the C headers by
// hand — that source is generated directly from FreeBSD's own headers and
// is exactly what golang.org/x/net/route itself is built on, so matching
// it is far more trustworthy than re-deriving BSD struct padding through
// reasoning alone. Confirmed identical on freebsd/amd64 and freebsd/arm64
// (both LP64; the 32-bit variants use narrower RtMetrics fields and are
// out of scope — gravinet doesn't build for freebsd/386).
//
// GatewaySupported is true here: see gateway_linux.go's doc comment for
// why internal/mesh's syncFullTunnelRoute treats platform support as a
// hard, checked prerequisite for activating full-tunnel at all.

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"unsafe"
)

const GatewaySupported = true

// RouteDemotionNeeded is true here: FreeBSD's routing table has no
// Linux-style "keep both, prefer the lower metric" behavior for two default
// routes — see DemoteDefaultRoute's doc comment below for what that means in
// practice and why routes.go's syncFullTunnelRoute treats this flag as a
// prerequisite step, not an optional optimization.
const RouteDemotionNeeded = true

const (
	rsaAlign = 8 // sizeof(long) on freebsd/{amd64,arm64}; see route_bsd.go's rsaAlignOf

	rtaDst     = 0x1
	rtaGateway = 0x2

	rtmGet     = 4
	rtmAdd     = 1
	rtmDelete  = 2
	rtmVersion = 5

	rtfUp      = 0x1
	rtfGateway = 0x2
	rtfHost    = 0x4
	rtfStatic  = 0x800

	ctlNet    = 4
	pfRoute   = 0x11 // == AF_ROUTE; PF_ROUTE is #define'd to it in BSD headers
	netRTDump = 1
	sysSysctl = 202
)

// rtMsghdr mirrors syscall.RtMsghdr on freebsd/{amd64,arm64} field-for-field.
type rtMsghdr struct {
	msglen  uint16
	version uint8
	typ     uint8
	index   uint16
	_       [2]byte
	flags   int32
	addrs   int32
	pid     int32
	seq     int32
	errno   int32
	fmask   int32
	inits   uint64
	rmx     rtMetrics
}

// rtMetrics mirrors syscall.RtMetrics on freebsd/{amd64,arm64}.
type rtMetrics struct {
	locks, mtu, hopcount, expire, recvpipe, sendpipe,
	ssthresh, rtt, rttvar, pksent, weight uint64
	filler [3]uint64
}

// sockaddrIn4/6 mirror syscall.RawSockaddrInet4/6: BSD sockaddrs carry an
// explicit length prefix Linux's don't.
type sockaddrIn4 struct {
	length uint8
	family uint8
	port   uint16
	addr   [4]byte
	zero   [8]byte
}

type sockaddrIn6 struct {
	length   uint8
	family   uint8
	port     uint16
	flowinfo uint32
	addr     [16]byte
	scopeID  uint32
}

func roundUp(n int) int {
	if n == 0 {
		return rsaAlign
	}
	return (n + rsaAlign - 1) &^ (rsaAlign - 1)
}

// sysctlRaw is the two-call sysctl(2) idiom (size probe, then fetch) for a
// raw MIB, via the __sysctl syscall directly — no cgo, no
// golang.org/x/sys/unix.
func sysctlRaw(mib []int32) ([]byte, error) {
	var n uintptr
	_, _, errno := syscall.Syscall6(sysSysctl,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		0, uintptr(unsafe.Pointer(&n)), 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("sysctl (size probe): %w", errno)
	}
	if n == 0 {
		return nil, nil
	}
	// The table can grow between the size probe and the fetch; pad
	// generously and retry once on ENOMEM from a stale size, the same
	// slack strategy netstat/route itself use.
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, n+n/2+4096)
		got := uintptr(len(buf))
		_, _, errno := syscall.Syscall6(sysSysctl,
			uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&got)), 0, 0)
		if errno == 0 {
			return buf[:got], nil
		}
		if errno != syscall.ENOMEM {
			return nil, fmt.Errorf("sysctl (fetch): %w", errno)
		}
		n = got // kernel told us the table grew; retry with its new size
	}
	return nil, fmt.Errorf("sysctl: routing table kept growing across retries")
}

// parsedRoute is one dumped entry's fields relevant to DefaultGateway and
// DemoteDefaultRoute. metric is rmx.hopcount straight out of the dumped
// rt_msghdr — no extra parsing needed, unlike gateway/dst, since it lives in
// the fixed-size header rather than the trailing sockaddr array.
type parsedRoute struct {
	flags   int32
	index   int32
	gateway netip.Addr
	metric  int
}

// dumpDefaultRoutes fetches the whole routing table for family via sysctl
// and returns every default-route (RTF_HOST unset, RTF_GATEWAY set, i.e.
// the same shape a default route needs) entry.
func dumpDefaultRoutes(family int) ([]parsedRoute, error) {
	buf, err := sysctlRaw([]int32{ctlNet, pfRoute, 0, int32(family), netRTDump, 0})
	if err != nil {
		return nil, err
	}
	var out []parsedRoute
	hdrSize := int(unsafe.Sizeof(rtMsghdr{}))
	for len(buf) >= hdrSize {
		hdr := (*rtMsghdr)(unsafe.Pointer(&buf[0]))
		msglen := int(hdr.msglen)
		if msglen < hdrSize || msglen > len(buf) {
			break // malformed; stop rather than misparse the rest
		}
		if hdr.flags&rtfUp != 0 && hdr.flags&rtfHost == 0 && hdr.flags&rtfGateway != 0 {
			if gw, ok := parseGatewayAddr(buf[hdrSize:msglen], hdr.addrs, family); ok {
				out = append(out, parsedRoute{flags: hdr.flags, index: int32(hdr.index), gateway: gw, metric: int(hdr.rmx.hopcount)})
			}
		}
		buf = buf[msglen:]
	}
	return out, nil
}

// parseGatewayAddr walks the sockaddr array following a routing message
// header (selected by the addrs bitmask, RTAX_DST=bit0 then
// RTAX_GATEWAY=bit1, in that fixed order per <net/route.h>) and returns
// the gateway address, if present.
func parseGatewayAddr(b []byte, addrs int32, family int) (netip.Addr, bool) {
	if addrs&rtaDst != 0 {
		if len(b) < 1 {
			return netip.Addr{}, false
		}
		n := roundUp(int(b[0]))
		if n > len(b) {
			return netip.Addr{}, false
		}
		b = b[n:]
	}
	if addrs&rtaGateway == 0 || len(b) < 1 {
		return netip.Addr{}, false
	}
	switch family {
	case syscall.AF_INET:
		if len(b) < int(unsafe.Sizeof(sockaddrIn4{})) {
			return netip.Addr{}, false
		}
		sa := (*sockaddrIn4)(unsafe.Pointer(&b[0]))
		return netip.AddrFrom4(sa.addr), true
	default: // AF_INET6
		if len(b) < int(unsafe.Sizeof(sockaddrIn6{})) {
			return netip.Addr{}, false
		}
		sa := (*sockaddrIn6)(unsafe.Pointer(&b[0]))
		return netip.AddrFrom16(sa.addr), true
	}
}

// DefaultGateway returns the best physical (non-tunnel) default route for
// family (syscall.AF_INET or syscall.AF_INET6), ignoring any default route
// whose outgoing interface is excludeIfIndex.
func DefaultGateway(family int, excludeIfIndex int32) (Gateway, error) {
	routes, err := dumpDefaultRoutes(family)
	if err != nil {
		return Gateway{}, err
	}
	for _, r := range routes {
		if r.index == excludeIfIndex {
			continue
		}
		if !r.gateway.IsValid() || r.gateway.IsUnspecified() {
			continue
		}
		// FreeBSD's dump doesn't carry a routing-selection metric the way
		// Linux's RTA_PRIORITY does (rmx.hopcount, surfaced below, is
		// informational only — see DemoteDefaultRoute's doc comment); take
		// the first physical match. Multiple simultaneous non-tunnel
		// default routes are rare enough on a single host that this is an
		// acceptable simplification.
		return Gateway{Addr: r.gateway, IfIndex: r.index, Metric: r.metric}, nil
	}
	return Gateway{}, fmt.Errorf("no physical default route found (family %#x)", family)
}

// appendStruct appends a fixed-size struct's raw bytes (rounded up to
// rsaAlign) onto dst — the routing-socket convention every sockaddr in a
// message body follows.
func appendStruct[T any](dst []byte, v T) []byte {
	n := int(unsafe.Sizeof(v))
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&v)), n)
	padded := make([]byte, roundUp(n))
	copy(padded, raw)
	return append(dst, padded...)
}

func prefixMask4(bits int) [4]byte {
	var m [4]byte
	if bits > 0 {
		binary.BigEndian.PutUint32(m[:], ^uint32(0)<<uint(32-bits))
	}
	return m
}

func prefixMask6(bits int) [16]byte {
	var m [16]byte
	for i := 0; i < bits; i++ {
		m[i/8] |= 1 << uint(7-i%8)
	}
	return m
}

// isHostPrefix reports whether p is a full-length host route (/32 or
// /128) rather than a network route — the distinction sendRouteMsg needs
// to get RTF_HOST right (see its doc comment).
func isHostPrefix(p netip.Prefix) bool {
	return p.Bits() == p.Addr().BitLen()
}

// sendRouteMsg constructs an RTM_ADD/RTM_DELETE message for
// "<prefix> via gateway dev ifIndex" and writes it to an AF_ROUTE socket.
// RTF_HOST is set only when p is actually a host route (/32 or /128) — the
// shape every original caller (AddGatewayRoute/DelGatewayRoute, always a
// bypass route to one address) passes. DemoteDefaultRoute also calls this,
// for the physical default route (/0), which is a network route, not a
// host route; setting RTF_HOST there regardless of p made the FreeBSD
// kernel do a host-route lookup instead of honoring the supplied netmask,
// so the delete never matched the real (non-host) default route — it
// returned ESRCH, which this function's own delete-is-idempotent handling
// below (there for the legitimate case of deleting an already-withdrawn
// route) swallowed as success, silently no-op'ing the demotion.
func sendRouteMsg(msgType uint8, p netip.Prefix, gateway netip.Addr, ifIndex int32) error {
	if p.Addr().Is4() != gateway.Is4() {
		return fmt.Errorf("gateway route: prefix %s and gateway %s are different address families", p, gateway)
	}
	var body []byte
	var family uint8
	if p.Addr().Is4() {
		family = syscall.AF_INET
		dst := sockaddrIn4{length: uint8(unsafe.Sizeof(sockaddrIn4{})), family: family, addr: p.Addr().As4()}
		gw := sockaddrIn4{length: uint8(unsafe.Sizeof(sockaddrIn4{})), family: family, addr: gateway.As4()}
		mask := sockaddrIn4{length: uint8(unsafe.Sizeof(sockaddrIn4{})), family: family, addr: prefixMask4(p.Bits())}
		body = appendStruct(appendStruct(appendStruct(([]byte)(nil), dst), gw), mask)
	} else {
		family = syscall.AF_INET6
		dst := sockaddrIn6{length: uint8(unsafe.Sizeof(sockaddrIn6{})), family: family, addr: p.Addr().As16()}
		gw := sockaddrIn6{length: uint8(unsafe.Sizeof(sockaddrIn6{})), family: family, addr: gateway.As16()}
		mask := sockaddrIn6{length: uint8(unsafe.Sizeof(sockaddrIn6{})), family: family, addr: prefixMask6(p.Bits())}
		body = appendStruct(appendStruct(appendStruct(([]byte)(nil), dst), gw), mask)
	}

	flags := int32(rtfUp | rtfGateway | rtfStatic)
	if isHostPrefix(p) {
		flags |= rtfHost
	}
	hdr := rtMsghdr{
		version: rtmVersion,
		typ:     msgType,
		index:   uint16(ifIndex),
		flags:   flags,
		addrs:   rtaDst | rtaGateway | 0x4, // RTA_DST | RTA_GATEWAY | RTA_NETMASK
		pid:     int32(syscall.Getpid()),
		seq:     1,
	}
	hdrSize := int(unsafe.Sizeof(hdr))
	hdr.msglen = uint16(hdrSize + len(body))

	msg := make([]byte, hdrSize+len(body))
	*(*rtMsghdr)(unsafe.Pointer(&msg[0])) = hdr
	copy(msg[hdrSize:], body)

	// SOCK_CLOEXEC: closed (defer) well before returning either way, but
	// atomic-at-open closes the narrow concurrent-exec race a deferred close
	// alone can't — see tun_linux.go's New for the concrete bug this class of
	// leak caused elsewhere in this package.
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	if _, err := syscall.Write(fd, msg); err != nil {
		if msgType == rtmDelete && (err == syscall.ESRCH || err == syscall.ENOENT) {
			return nil // already gone
		}
		if msgType == rtmAdd && err == syscall.EEXIST {
			return nil // already there, at this exact key — idempotent, matching AddRoute elsewhere
		}
		return fmt.Errorf("write routing socket message: %w", err)
	}
	return nil
}

// AddGatewayRoute installs "<prefix> via <gateway> dev <ifIndex>" — the
// shape a full-tunnel peer-bypass host route needs, unlike AddRoute's
// on-link route via this process's own tun. Not a Device method, since it
// targets whatever physical interface ifIndex names.
func AddGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return sendRouteMsg(rtmAdd, p, gateway, ifIndex)
}

// DelGatewayRoute removes a route previously installed by AddGatewayRoute.
// A missing route is not treated as an error.
func DelGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return sendRouteMsg(rtmDelete, p, gateway, ifIndex)
}

// ifIndexByName resolves an interface name to its kernel index via Go's
// own stdlib net package — which on FreeBSD already does exactly this via
// the same routing-socket family, non-cgo, so there's no reason to
// duplicate that parsing here.
func ifIndexByName(name string) (int32, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return int32(ifc.Index), nil
}

// defaultPrefix returns 0.0.0.0/0 or ::/0 for family.
func defaultPrefix(family int) netip.Prefix {
	if family == syscall.AF_INET {
		return netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	}
	return netip.PrefixFrom(netip.IPv6Unspecified(), 0)
}

// savedPhysicalRoute is exactly what's needed to recreate a physical
// default route DemoteDefaultRoute removed: its gateway, its outgoing
// interface, and the metric it had, so restoring it later doesn't have to
// (and, once it's demoted, can't) rediscover any of that by looking at the
// routing table again.
type savedPhysicalRoute struct {
	gateway netip.Addr
	ifIndex int32
	metric  int
}

// demoteState holds, per address family, the physical default route
// DemoteDefaultRoute most recently removed and hasn't yet restored — see
// DemoteDefaultRoute's doc comment for how a single function serves both
// directions.
var demoteState = struct {
	mu    sync.Mutex
	saved map[int]savedPhysicalRoute
}{saved: make(map[int]savedPhysicalRoute)}

// DemoteDefaultRoute gets family's pre-existing physical (non-tunnel)
// default route out of the way of gravinet's own, and — called again later
// for the same family, once gravinet's own default route is withdrawn —
// puts it back. The same function serves both directions: whichever call
// finds nothing already stashed in demoteState does the removal; the next
// one (identified purely by finding something stashed, not by a separate
// parameter) does the restore. routes.go's syncFullTunnelRoute drives this
// by calling once to activate full-tunnel and once, later, to deactivate
// it, passing the metric the first call returned back in as newMetric —
// which is how the restore call recovers nothing more than "this was
// pending"; the gateway/interface/metric it actually restores come from
// demoteState, not from newMetric or a fresh routing-table lookup.
//
// "Out of the way" here means deleted, not deprioritized in place. Unlike
// Linux's FIB, which happily keeps two 0.0.0.0/0 (or ::/0) entries side by
// side and prefers whichever has the lower RTA_PRIORITY, FreeBSD's routing
// table holds exactly one route per destination/mask — a second RTM_ADD
// for the same destination collides with the one already there (EEXIST)
// rather than coexisting at a lower priority. An earlier version of this
// function tried to get away with an in-place RTM_CHANGE to the physical
// route's rmx.hopcount instead, on the theory that a "demoted" copy could
// just sit at 0.0.0.0/0 the way Linux's does. That never freed the
// destination for gravinet's own AddRoute to actually claim: hopcount
// isn't a FreeBSD forwarding-decision input to begin with, and — more to
// the point — no metric value changes the fact that the dst/mask key was
// still occupied by the physical route. The key itself has to come out of
// the table, which means an actual RTM_DELETE, which means the exact
// gateway/interface needed to put it back has to be captured before that
// delete goes out, since there's no way to ask the kernel for a route
// that's no longer there.
func DemoteDefaultRoute(family int, excludeIfIndex int32, newMetric int) (int, error) {
	p := defaultPrefix(family)

	demoteState.mu.Lock()
	saved, pending := demoteState.saved[family]
	demoteState.mu.Unlock()

	if pending {
		// Restore: put the exact route that was removed back, via the
		// gateway/interface captured at removal time — not a fresh
		// DefaultGateway-style lookup, which would find nothing (the
		// physical route has been gone since the removal call) or, worse,
		// gravinet's own now-withdrawn tun route if this races withdrawal.
		if err := sendRouteMsg(rtmAdd, p, saved.gateway, saved.ifIndex); err != nil {
			return 0, fmt.Errorf("restore physical default route: %w", err)
		}
		demoteState.mu.Lock()
		delete(demoteState.saved, family)
		demoteState.mu.Unlock()
		return saved.metric, nil
	}

	// Removal: the same physical-default lookup DefaultGateway performs,
	// excluding excludeIfIndex (gravinet's own tun) the same way.
	routes, err := dumpDefaultRoutes(family)
	if err != nil {
		return 0, err
	}
	for _, r := range routes {
		if r.index == excludeIfIndex {
			continue
		}
		if !r.gateway.IsValid() || r.gateway.IsUnspecified() {
			continue
		}
		if err := sendRouteMsg(rtmDelete, p, r.gateway, r.index); err != nil {
			return 0, fmt.Errorf("remove physical default route ahead of full-tunnel install: %w", err)
		}
		demoteState.mu.Lock()
		demoteState.saved[family] = savedPhysicalRoute{gateway: r.gateway, ifIndex: r.index, metric: r.metric}
		demoteState.mu.Unlock()
		return r.metric, nil
	}
	return 0, fmt.Errorf("no physical default route found to demote (family %#x)", family)
}
