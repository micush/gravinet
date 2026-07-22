//go:build openbsd

package tun

// OpenBSD physical default-gateway detection and gateway-routed host
// routes, via the BSD routing socket family — see gateway_freebsd.go's doc
// comment for the overall approach (sysctl dump for reads, AF_ROUTE socket
// writes for RTM_ADD/RTM_DELETE), which this file mirrors closely.
// OpenBSD's rt_msghdr has genuinely diverged from FreeBSD/Darwin's,
// growing Hdrlen, Tableid, Priority, and Mpls fields the others don't
// have, and dropping the Use field they do have — it is not simply "the
// same struct under a different name", so this file has its own
// independent struct definitions, checked against this Go toolchain's own
// stdlib source (src/syscall/ztypes_openbsd_amd64.go) rather than derived
// from the C headers by hand, same as every other platform in this
// package.
//
// The Hdrlen field specifically is a real OpenBSD-specific requirement,
// not just an extra field to zero: OpenBSD's route(4) needs rtm_hdrlen set
// to the actual header size on an outgoing message so the kernel knows
// where the header ends and the sockaddr array begins — FreeBSD/Darwin
// instead assume a fixed, well-known header size and have no equivalent
// field. sendRouteMsg below sets it explicitly for exactly this reason.
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

// RouteDemotionNeeded is true here — see gateway_freebsd.go's
// DemoteDefaultRoute doc comment for the underlying reason (shared across
// every BSD-family platform gravinet supports): a second RTM_ADD for
// 0.0.0.0/0 doesn't create a second, lower-priority default route the way
// it does on Linux, it collides with the physical one already there. The
// physical route has to actually be removed first, not just deprioritized
// in place — see this file's own DemoteDefaultRoute doc comment below,
// which (like gateway_freebsd.go's) used to try an in-place RTM_CHANGE to
// rmx.Hopcount instead and didn't actually work.
const RouteDemotionNeeded = true

const (
	obsdRsaAlign = 8 // sizeof(long) on openbsd/{amd64,arm64}; see route_bsd.go's rsaAlignOf — OpenBSD isn't Darwin's special case

	obsdRtaDst     = 0x1
	obsdRtaGateway = 0x2

	obsdRtmAdd     = 1
	obsdRtmDelete  = 2
	obsdRtmVersion = 5

	obsdRtfUp      = 0x1
	obsdRtfGateway = 0x2
	obsdRtfHost    = 0x4
	obsdRtfStatic  = 0x800

	obsdCtlNet    = 4
	obsdPfRoute   = 0x11 // == AF_ROUTE
	obsdNetRTDump = 1
	obsdSysSysctl = 202 // exported as sys_sysctl on OpenBSD, same number
)

// obsdRtMsghdr mirrors syscall.RtMsghdr on openbsd/{amd64,arm64}
// field-for-field — see package doc comment for how this genuinely differs
// from FreeBSD/Darwin's, not just superficially.
type obsdRtMsghdr struct {
	msglen   uint16
	version  uint8
	typ      uint8
	hdrlen   uint16
	index    uint16
	tableid  uint16
	priority uint8
	mpls     uint8
	addrs    int32
	flags    int32
	fmask    int32
	pid      int32
	seq      int32
	errno    int32
	inits    uint32
	rmx      obsdRtMetrics
}

// obsdRtMetrics mirrors syscall.RtMetrics on openbsd/{amd64,arm64}.
type obsdRtMetrics struct {
	pksent                                                                       uint64
	expire                                                                       int64
	locks, mtu, refcnt, hopcount, recvpipe, sendpipe, ssthresh, rtt, rttvar, pad uint32
}

// obsdSockaddrIn4/6 mirror syscall.RawSockaddrInet4/6 — same BSD sockaddr
// ABI as FreeBSD/Darwin (this part didn't diverge), kept independent of
// the other platforms' copies for the same self-containment reason
// gateway_darwin.go's does.
type obsdSockaddrIn4 struct {
	length uint8
	family uint8
	port   uint16
	addr   [4]byte
	zero   [8]byte
}

type obsdSockaddrIn6 struct {
	length   uint8
	family   uint8
	port     uint16
	flowinfo uint32
	addr     [16]byte
	scopeID  uint32
}

func obsdRoundUp(n int) int {
	if n == 0 {
		return obsdRsaAlign
	}
	return (n + obsdRsaAlign - 1) &^ (obsdRsaAlign - 1)
}

func obsdSysctlRaw(mib []int32) ([]byte, error) {
	var n uintptr
	_, _, errno := syscall.Syscall6(obsdSysSysctl,
		uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		0, uintptr(unsafe.Pointer(&n)), 0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("sysctl (size probe): %w", errno)
	}
	if n == 0 {
		return nil, nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		buf := make([]byte, n+n/2+4096)
		got := uintptr(len(buf))
		_, _, errno := syscall.Syscall6(obsdSysSysctl,
			uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&got)), 0, 0)
		if errno == 0 {
			return buf[:got], nil
		}
		if errno != syscall.ENOMEM {
			return nil, fmt.Errorf("sysctl (fetch): %w", errno)
		}
		n = got
	}
	return nil, fmt.Errorf("sysctl: routing table kept growing across retries")
}

type obsdParsedRoute struct {
	index   int32
	gateway netip.Addr
	metric  int // rmx.hopcount, straight out of the fixed-size header
}

func obsdDumpDefaultRoutes(family int) ([]obsdParsedRoute, error) {
	buf, err := obsdSysctlRaw([]int32{obsdCtlNet, obsdPfRoute, 0, int32(family), obsdNetRTDump, 0})
	if err != nil {
		return nil, err
	}
	var out []obsdParsedRoute
	hdrSize := int(unsafe.Sizeof(obsdRtMsghdr{}))
	for len(buf) >= hdrSize {
		hdr := (*obsdRtMsghdr)(unsafe.Pointer(&buf[0]))
		msglen := int(hdr.msglen)
		if msglen < hdrSize || msglen > len(buf) {
			break
		}
		// OpenBSD's own Hdrlen (not this package's hdrSize constant) tells
		// us exactly where this particular message's sockaddr array
		// starts — trust it over the fixed struct size, since a future
		// kernel could legitimately report a larger header than the one
		// this binary was built against, per OpenBSD's own extensibility
		// rationale for adding the field in the first place.
		addrStart := int(hdr.hdrlen)
		if addrStart < hdrSize || addrStart > msglen {
			addrStart = hdrSize
		}
		if hdr.flags&obsdRtfUp != 0 && hdr.flags&obsdRtfHost == 0 && hdr.flags&obsdRtfGateway != 0 {
			if gw, ok := obsdParseGatewayAddr(buf[addrStart:msglen], hdr.addrs, family); ok {
				out = append(out, obsdParsedRoute{index: int32(hdr.index), gateway: gw, metric: int(hdr.rmx.hopcount)})
			}
		}
		buf = buf[msglen:]
	}
	return out, nil
}

func obsdParseGatewayAddr(b []byte, addrs int32, family int) (netip.Addr, bool) {
	if addrs&obsdRtaDst != 0 {
		if len(b) < 1 {
			return netip.Addr{}, false
		}
		n := obsdRoundUp(int(b[0]))
		if n > len(b) {
			return netip.Addr{}, false
		}
		b = b[n:]
	}
	if addrs&obsdRtaGateway == 0 || len(b) < 1 {
		return netip.Addr{}, false
	}
	switch family {
	case syscall.AF_INET:
		if len(b) < int(unsafe.Sizeof(obsdSockaddrIn4{})) {
			return netip.Addr{}, false
		}
		sa := (*obsdSockaddrIn4)(unsafe.Pointer(&b[0]))
		return netip.AddrFrom4(sa.addr), true
	default:
		if len(b) < int(unsafe.Sizeof(obsdSockaddrIn6{})) {
			return netip.Addr{}, false
		}
		sa := (*obsdSockaddrIn6)(unsafe.Pointer(&b[0]))
		return netip.AddrFrom16(sa.addr), true
	}
}

// DefaultGateway returns the best physical (non-tunnel) default route for
// family (syscall.AF_INET or syscall.AF_INET6), ignoring any default route
// whose outgoing interface is excludeIfIndex.
func DefaultGateway(family int, excludeIfIndex int32) (Gateway, error) {
	routes, err := obsdDumpDefaultRoutes(family)
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
		return Gateway{Addr: r.gateway, IfIndex: r.index, Metric: r.metric}, nil
	}
	return Gateway{}, fmt.Errorf("no physical default route found (family %#x)", family)
}

func obsdAppendStruct[T any](dst []byte, v T) []byte {
	n := int(unsafe.Sizeof(v))
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&v)), n)
	padded := make([]byte, obsdRoundUp(n))
	copy(padded, raw)
	return append(dst, padded...)
}

func obsdPrefixMask4(bits int) [4]byte {
	var m [4]byte
	if bits > 0 {
		binary.BigEndian.PutUint32(m[:], ^uint32(0)<<uint(32-bits))
	}
	return m
}

func obsdPrefixMask6(bits int) [16]byte {
	var m [16]byte
	for i := 0; i < bits; i++ {
		m[i/8] |= 1 << uint(7-i%8)
	}
	return m
}

// obsdIsHostPrefix reports whether p is a full-length host route (/32 or
// /128) rather than a network route — see gateway_freebsd.go's
// isHostPrefix/sendRouteMsg doc comment for why obsdSendRouteMsg needs
// this distinction instead of hardcoding obsdRtfHost.
func obsdIsHostPrefix(p netip.Prefix) bool {
	return p.Bits() == p.Addr().BitLen()
}

func obsdSendRouteMsg(msgType uint8, p netip.Prefix, gateway netip.Addr, ifIndex int32) error {
	if p.Addr().Is4() != gateway.Is4() {
		return fmt.Errorf("gateway route: prefix %s and gateway %s are different address families", p, gateway)
	}
	var body []byte
	var family uint8
	if p.Addr().Is4() {
		family = syscall.AF_INET
		dst := obsdSockaddrIn4{length: uint8(unsafe.Sizeof(obsdSockaddrIn4{})), family: family, addr: p.Addr().As4()}
		gw := obsdSockaddrIn4{length: uint8(unsafe.Sizeof(obsdSockaddrIn4{})), family: family, addr: gateway.As4()}
		mask := obsdSockaddrIn4{length: uint8(unsafe.Sizeof(obsdSockaddrIn4{})), family: family, addr: obsdPrefixMask4(p.Bits())}
		body = obsdAppendStruct(obsdAppendStruct(obsdAppendStruct(([]byte)(nil), dst), gw), mask)
	} else {
		family = syscall.AF_INET6
		dst := obsdSockaddrIn6{length: uint8(unsafe.Sizeof(obsdSockaddrIn6{})), family: family, addr: p.Addr().As16()}
		gw := obsdSockaddrIn6{length: uint8(unsafe.Sizeof(obsdSockaddrIn6{})), family: family, addr: gateway.As16()}
		mask := obsdSockaddrIn6{length: uint8(unsafe.Sizeof(obsdSockaddrIn6{})), family: family, addr: obsdPrefixMask6(p.Bits())}
		body = obsdAppendStruct(obsdAppendStruct(obsdAppendStruct(([]byte)(nil), dst), gw), mask)
	}

	hdrSize := int(unsafe.Sizeof(obsdRtMsghdr{}))
	flags := int32(obsdRtfUp | obsdRtfGateway | obsdRtfStatic)
	if obsdIsHostPrefix(p) {
		flags |= obsdRtfHost
	}
	hdr := obsdRtMsghdr{
		version: obsdRtmVersion,
		typ:     msgType,
		hdrlen:  uint16(hdrSize), // see package doc comment: OpenBSD-specific, required
		index:   uint16(ifIndex),
		flags:   flags,
		addrs:   obsdRtaDst | obsdRtaGateway | 0x4, // RTA_DST | RTA_GATEWAY | RTA_NETMASK
		pid:     int32(syscall.Getpid()),
		seq:     1,
	}
	hdr.msglen = uint16(hdrSize + len(body))

	msg := make([]byte, hdrSize+len(body))
	*(*obsdRtMsghdr)(unsafe.Pointer(&msg[0])) = hdr
	copy(msg[hdrSize:], body)

	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	if _, err := syscall.Write(fd, msg); err != nil {
		if msgType == obsdRtmDelete && (err == syscall.ESRCH || err == syscall.ENOENT) {
			return nil
		}
		if msgType == obsdRtmAdd && err == syscall.EEXIST {
			return nil
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
	return obsdSendRouteMsg(obsdRtmAdd, p, gateway, ifIndex)
}

// DelGatewayRoute removes a route previously installed by AddGatewayRoute.
// A missing route is not treated as an error.
func DelGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return obsdSendRouteMsg(obsdRtmDelete, p, gateway, ifIndex)
}

// ifIndexByName resolves an interface name to its kernel index via Go's
// own stdlib net package, non-cgo — see gateway_freebsd.go's identically-
// named helper for why this is preferred over a hand-rolled parser.
func ifIndexByName(name string) (int32, error) {
	ifc, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return int32(ifc.Index), nil
}

// obsdDefaultPrefix returns 0.0.0.0/0 or ::/0 for family.
func obsdDefaultPrefix(family int) netip.Prefix {
	if family == syscall.AF_INET {
		return netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	}
	return netip.PrefixFrom(netip.IPv6Unspecified(), 0)
}

// obsdSavedPhysicalRoute is exactly what's needed to recreate a physical
// default route DemoteDefaultRoute removed: its gateway, its outgoing
// interface, and the metric it had.
type obsdSavedPhysicalRoute struct {
	gateway netip.Addr
	ifIndex int32
	metric  int
}

// obsdDemoteState holds, per address family, the physical default route
// DemoteDefaultRoute most recently removed and hasn't yet restored — see
// DemoteDefaultRoute's doc comment for how a single function serves both
// directions.
var obsdDemoteState = struct {
	mu    sync.Mutex
	saved map[int]obsdSavedPhysicalRoute
}{saved: make(map[int]obsdSavedPhysicalRoute)}

// DemoteDefaultRoute gets family's pre-existing physical (non-tunnel)
// default route out of the way of gravinet's own, and — called again later
// for the same family, once gravinet's own default route is withdrawn —
// puts it back. The same function serves both directions: whichever call
// finds nothing already stashed in obsdDemoteState does the removal; the
// next one (identified purely by finding something stashed, not by a
// separate parameter) does the restore. See gateway_freebsd.go's
// DemoteDefaultRoute doc comment for the full reasoning this mirrors —
// shared across every BSD-family platform gravinet supports: OpenBSD's
// routing table, like FreeBSD's, holds exactly one route per
// destination/mask, so a pre-existing physical default has to actually be
// removed (RTM_DELETE), not just reprogrammed in place, before gravinet's
// own can claim that destination. An earlier version of this function
// tried an in-place RTM_CHANGE to rmx.hopcount instead (mirroring
// gateway_freebsd.go's own former approach) and didn't actually work, for
// the same reason: no metric value frees up an occupied dst/mask key.
func DemoteDefaultRoute(family int, excludeIfIndex int32, newMetric int) (int, error) {
	p := obsdDefaultPrefix(family)

	obsdDemoteState.mu.Lock()
	saved, pending := obsdDemoteState.saved[family]
	obsdDemoteState.mu.Unlock()

	if pending {
		// Restore: put the exact route that was removed back, via the
		// gateway/interface captured at removal time.
		if err := obsdSendRouteMsg(obsdRtmAdd, p, saved.gateway, saved.ifIndex); err != nil {
			return 0, fmt.Errorf("restore physical default route: %w", err)
		}
		obsdDemoteState.mu.Lock()
		delete(obsdDemoteState.saved, family)
		obsdDemoteState.mu.Unlock()
		return saved.metric, nil
	}

	// Removal: the same physical-default lookup DefaultGateway performs,
	// excluding excludeIfIndex (gravinet's own tun) the same way.
	routes, err := obsdDumpDefaultRoutes(family)
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
		if err := obsdSendRouteMsg(obsdRtmDelete, p, r.gateway, r.index); err != nil {
			return 0, fmt.Errorf("remove physical default route ahead of full-tunnel install: %w", err)
		}
		obsdDemoteState.mu.Lock()
		obsdDemoteState.saved[family] = obsdSavedPhysicalRoute{gateway: r.gateway, ifIndex: r.index, metric: r.metric}
		obsdDemoteState.mu.Unlock()
		return r.metric, nil
	}
	return 0, fmt.Errorf("no physical default route found to demote (family %#x)", family)
}
