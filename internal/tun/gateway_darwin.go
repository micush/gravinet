//go:build darwin

package tun

// macOS physical default-gateway detection and gateway-routed host routes,
// via the BSD routing socket family — see gateway_freebsd.go's doc comment
// for the overall approach (sysctl dump for reads, AF_ROUTE socket writes
// for RTM_ADD/RTM_DELETE), which this file mirrors closely. Darwin's own
// struct layout genuinely differs from FreeBSD's despite the shared BSD
// ancestry — different rt_msghdr fields (Use instead of Fmask, a 32-bit
// Inits instead of FreeBSD's 64-bit one) and, critically, a different
// sockaddr alignment: Darwin routing facilities require 32-bit-aligned
// access even on a 64-bit kernel, where FreeBSD/OpenBSD use the native
// 64-bit alignment. That's confirmed straight from this Go toolchain's own
// stdlib source (src/syscall/route_bsd.go's rsaAlignOf: "Darwin kernels
// require 32-bit aligned access to routing facilities") — not something
// that would have been obvious from reasoning about "modern 64-bit BSD"
// alone, which is exactly why every struct/constant here is checked
// against that source rather than derived from the C headers by hand.
// Confirmed identical on darwin/amd64 and darwin/arm64.
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
// which (like gateway_freebsd.go's former approach) used to try an
// in-place RTM_CHANGE to rmx.hopcount instead, and didn't actually work.
const RouteDemotionNeeded = true

const (
	darwinRsaAlign = 4 // NOT sizeof(long)/8 — see package doc comment above

	darwinRtaDst     = 0x1
	darwinRtaGateway = 0x2

	darwinRtmAdd     = 1
	darwinRtmDelete  = 2
	darwinRtmVersion = 5

	darwinRtfUp      = 0x1
	darwinRtfGateway = 0x2
	darwinRtfHost    = 0x4
	darwinRtfStatic  = 0x800

	darwinCtlNet    = 4
	darwinPfRoute   = 0x11 // == AF_ROUTE
	darwinNetRTDump = 1
	darwinSysSysctl = 202
)

// darwinRtMsghdr mirrors syscall.RtMsghdr on darwin/{amd64,arm64}
// field-for-field — note Use where FreeBSD has Fmask, and a 32-bit Inits
// where FreeBSD's is 64-bit; these are genuinely different structs, not
// the same one under a different name.
type darwinRtMsghdr struct {
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
	use     int32
	inits   uint32
	rmx     darwinRtMetrics
}

// darwinRtMetrics mirrors syscall.RtMetrics on darwin/{amd64,arm64}: all
// 32-bit fields, unlike FreeBSD's 64-bit ones.
type darwinRtMetrics struct {
	locks, mtu, hopcount                              uint32
	expire                                            int32
	recvpipe, sendpipe, ssthresh, rtt, rttvar, pksent uint32
	filler                                            [4]uint32
}

// darwinSockaddrIn4/6 mirror syscall.RawSockaddrInet4/6 — identical layout
// to FreeBSD's (this part of the BSD sockaddr ABI didn't diverge), kept as
// separate types in this file so gateway_darwin.go has no cross-file
// dependency on gateway_freebsd.go (the two are only ever compiled one at a
// time anyway, by build tag, but keeping them independently self-contained
// makes each easier to audit against its own platform's source on its own).
type darwinSockaddrIn4 struct {
	length uint8
	family uint8
	port   uint16
	addr   [4]byte
	zero   [8]byte
}

type darwinSockaddrIn6 struct {
	length   uint8
	family   uint8
	port     uint16
	flowinfo uint32
	addr     [16]byte
	scopeID  uint32
}

func darwinRoundUp(n int) int {
	if n == 0 {
		return darwinRsaAlign
	}
	return (n + darwinRsaAlign - 1) &^ (darwinRsaAlign - 1)
}

func darwinSysctlRaw(mib []int32) ([]byte, error) {
	var n uintptr
	_, _, errno := syscall.Syscall6(darwinSysSysctl,
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
		_, _, errno := syscall.Syscall6(darwinSysSysctl,
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

type darwinParsedRoute struct {
	index   int32
	gateway netip.Addr
	metric  int // rmx.hopcount, straight out of the fixed-size header
}

func darwinDumpDefaultRoutes(family int) ([]darwinParsedRoute, error) {
	buf, err := darwinSysctlRaw([]int32{darwinCtlNet, darwinPfRoute, 0, int32(family), darwinNetRTDump, 0})
	if err != nil {
		return nil, err
	}
	var out []darwinParsedRoute
	hdrSize := int(unsafe.Sizeof(darwinRtMsghdr{}))
	for len(buf) >= hdrSize {
		hdr := (*darwinRtMsghdr)(unsafe.Pointer(&buf[0]))
		msglen := int(hdr.msglen)
		if msglen < hdrSize || msglen > len(buf) {
			break
		}
		if hdr.flags&darwinRtfUp != 0 && hdr.flags&darwinRtfHost == 0 && hdr.flags&darwinRtfGateway != 0 {
			if gw, ok := darwinParseGatewayAddr(buf[hdrSize:msglen], hdr.addrs, family); ok {
				out = append(out, darwinParsedRoute{index: int32(hdr.index), gateway: gw, metric: int(hdr.rmx.hopcount)})
			}
		}
		buf = buf[msglen:]
	}
	return out, nil
}

func darwinParseGatewayAddr(b []byte, addrs int32, family int) (netip.Addr, bool) {
	if addrs&darwinRtaDst != 0 {
		if len(b) < 1 {
			return netip.Addr{}, false
		}
		n := darwinRoundUp(int(b[0]))
		if n > len(b) {
			return netip.Addr{}, false
		}
		b = b[n:]
	}
	if addrs&darwinRtaGateway == 0 || len(b) < 1 {
		return netip.Addr{}, false
	}
	switch family {
	case syscall.AF_INET:
		if len(b) < int(unsafe.Sizeof(darwinSockaddrIn4{})) {
			return netip.Addr{}, false
		}
		sa := (*darwinSockaddrIn4)(unsafe.Pointer(&b[0]))
		return netip.AddrFrom4(sa.addr), true
	default:
		if len(b) < int(unsafe.Sizeof(darwinSockaddrIn6{})) {
			return netip.Addr{}, false
		}
		sa := (*darwinSockaddrIn6)(unsafe.Pointer(&b[0]))
		return netip.AddrFrom16(sa.addr), true
	}
}

// DefaultGateway returns the best physical (non-tunnel) default route for
// family (syscall.AF_INET or syscall.AF_INET6), ignoring any default route
// whose outgoing interface is excludeIfIndex.
func DefaultGateway(family int, excludeIfIndex int32) (Gateway, error) {
	routes, err := darwinDumpDefaultRoutes(family)
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

func darwinAppendStruct[T any](dst []byte, v T) []byte {
	n := int(unsafe.Sizeof(v))
	raw := unsafe.Slice((*byte)(unsafe.Pointer(&v)), n)
	padded := make([]byte, darwinRoundUp(n))
	copy(padded, raw)
	return append(dst, padded...)
}

func darwinPrefixMask4(bits int) [4]byte {
	var m [4]byte
	if bits > 0 {
		binary.BigEndian.PutUint32(m[:], ^uint32(0)<<uint(32-bits))
	}
	return m
}

func darwinPrefixMask6(bits int) [16]byte {
	var m [16]byte
	for i := 0; i < bits; i++ {
		m[i/8] |= 1 << uint(7-i%8)
	}
	return m
}

// darwinIsHostPrefix reports whether p is a full-length host route (/32 or
// /128) rather than a network route — see gateway_freebsd.go's
// isHostPrefix/sendRouteMsg doc comment for why darwinSendRouteMsg needs
// this distinction instead of hardcoding darwinRtfHost.
func darwinIsHostPrefix(p netip.Prefix) bool {
	return p.Bits() == p.Addr().BitLen()
}

func darwinSendRouteMsg(msgType uint8, p netip.Prefix, gateway netip.Addr, ifIndex int32) error {
	if p.Addr().Is4() != gateway.Is4() {
		return fmt.Errorf("gateway route: prefix %s and gateway %s are different address families", p, gateway)
	}
	var body []byte
	var family uint8
	if p.Addr().Is4() {
		family = syscall.AF_INET
		dst := darwinSockaddrIn4{length: uint8(unsafe.Sizeof(darwinSockaddrIn4{})), family: family, addr: p.Addr().As4()}
		gw := darwinSockaddrIn4{length: uint8(unsafe.Sizeof(darwinSockaddrIn4{})), family: family, addr: gateway.As4()}
		mask := darwinSockaddrIn4{length: uint8(unsafe.Sizeof(darwinSockaddrIn4{})), family: family, addr: darwinPrefixMask4(p.Bits())}
		body = darwinAppendStruct(darwinAppendStruct(darwinAppendStruct(([]byte)(nil), dst), gw), mask)
	} else {
		family = syscall.AF_INET6
		dst := darwinSockaddrIn6{length: uint8(unsafe.Sizeof(darwinSockaddrIn6{})), family: family, addr: p.Addr().As16()}
		gw := darwinSockaddrIn6{length: uint8(unsafe.Sizeof(darwinSockaddrIn6{})), family: family, addr: gateway.As16()}
		mask := darwinSockaddrIn6{length: uint8(unsafe.Sizeof(darwinSockaddrIn6{})), family: family, addr: darwinPrefixMask6(p.Bits())}
		body = darwinAppendStruct(darwinAppendStruct(darwinAppendStruct(([]byte)(nil), dst), gw), mask)
	}

	flags := int32(darwinRtfUp | darwinRtfGateway | darwinRtfStatic)
	if darwinIsHostPrefix(p) {
		flags |= darwinRtfHost
	}
	hdr := darwinRtMsghdr{
		version: darwinRtmVersion,
		typ:     msgType,
		index:   uint16(ifIndex),
		flags:   flags,
		addrs:   darwinRtaDst | darwinRtaGateway | 0x4, // RTA_DST | RTA_GATEWAY | RTA_NETMASK
		pid:     int32(syscall.Getpid()),
		seq:     1,
	}
	hdrSize := int(unsafe.Sizeof(hdr))
	hdr.msglen = uint16(hdrSize + len(body))

	msg := make([]byte, hdrSize+len(body))
	*(*darwinRtMsghdr)(unsafe.Pointer(&msg[0])) = hdr
	copy(msg[hdrSize:], body)

	// Darwin has no atomic SOCK_CLOEXEC (unlike Linux/FreeBSD/OpenBSD's
	// SOCK_RAW|SOCK_CLOEXEC elsewhere in this package), so this takes
	// syscall.ForkLock the same way tun_darwin.go's New and Go's own
	// os.OpenFile do — held across the open-then-mark pair so a concurrent
	// exec.Command on another goroutine can't fork in the gap and inherit
	// this fd, even though it's closed (defer) well before returning anyway.
	syscall.ForkLock.RLock()
	fd, err := syscall.Socket(syscall.AF_ROUTE, syscall.SOCK_RAW, 0)
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	if _, err := syscall.Write(fd, msg); err != nil {
		if msgType == darwinRtmDelete && (err == syscall.ESRCH || err == syscall.ENOENT) {
			return nil
		}
		if msgType == darwinRtmAdd && err == syscall.EEXIST {
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
	return darwinSendRouteMsg(darwinRtmAdd, p, gateway, ifIndex)
}

// DelGatewayRoute removes a route previously installed by AddGatewayRoute.
// A missing route is not treated as an error.
func DelGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	return darwinSendRouteMsg(darwinRtmDelete, p, gateway, ifIndex)
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

// darwinDefaultPrefix returns 0.0.0.0/0 or ::/0 for family.
func darwinDefaultPrefix(family int) netip.Prefix {
	if family == syscall.AF_INET {
		return netip.PrefixFrom(netip.IPv4Unspecified(), 0)
	}
	return netip.PrefixFrom(netip.IPv6Unspecified(), 0)
}

// darwinSavedPhysicalRoute is exactly what's needed to recreate a physical
// default route DemoteDefaultRoute removed: its gateway, its outgoing
// interface, and the metric it had.
type darwinSavedPhysicalRoute struct {
	gateway netip.Addr
	ifIndex int32
	metric  int
}

// darwinDemoteState holds, per address family, the physical default route
// DemoteDefaultRoute most recently removed and hasn't yet restored — see
// DemoteDefaultRoute's doc comment for how a single function serves both
// directions.
var darwinDemoteState = struct {
	mu    sync.Mutex
	saved map[int]darwinSavedPhysicalRoute
}{saved: make(map[int]darwinSavedPhysicalRoute)}

// DemoteDefaultRoute gets family's pre-existing physical (non-tunnel)
// default route out of the way of gravinet's own, and — called again later
// for the same family, once gravinet's own default route is withdrawn —
// puts it back. See gateway_freebsd.go's DemoteDefaultRoute doc comment for
// the full reasoning this mirrors exactly: macOS's routing table, like
// FreeBSD's and OpenBSD's, holds exactly one route per destination/mask, so
// the pre-existing physical default has to actually be removed
// (RTM_DELETE), not just reprogrammed in place, before gravinet's own can
// claim that destination. An earlier version of this function tried an
// in-place RTM_CHANGE to rmx.hopcount instead — the same approach
// FreeBSD/OpenBSD's own earlier versions used — and, for the identical
// reason, never actually freed the destination/mask key: a field report
// confirmed it directly, `netstat -rn` showing the physical default route
// (`UGScg`, unchanged) still present after full-tunnel "activated," with no
// second `default` line for gravinet's own tun anywhere in the table.
func DemoteDefaultRoute(family int, excludeIfIndex int32, newMetric int) (int, error) {
	p := darwinDefaultPrefix(family)

	darwinDemoteState.mu.Lock()
	saved, pending := darwinDemoteState.saved[family]
	darwinDemoteState.mu.Unlock()

	if pending {
		// Restore: put the exact route that was removed back, via the
		// gateway/interface captured at removal time.
		if err := darwinSendRouteMsg(darwinRtmAdd, p, saved.gateway, saved.ifIndex); err != nil {
			return 0, fmt.Errorf("restore physical default route: %w", err)
		}
		darwinDemoteState.mu.Lock()
		delete(darwinDemoteState.saved, family)
		darwinDemoteState.mu.Unlock()
		return saved.metric, nil
	}

	// Removal: the same physical-default lookup DefaultGateway performs,
	// excluding excludeIfIndex (gravinet's own tun) the same way.
	routes, err := darwinDumpDefaultRoutes(family)
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
		if err := darwinSendRouteMsg(darwinRtmDelete, p, r.gateway, r.index); err != nil {
			return 0, fmt.Errorf("remove physical default route ahead of full-tunnel install: %w", err)
		}
		darwinDemoteState.mu.Lock()
		darwinDemoteState.saved[family] = darwinSavedPhysicalRoute{gateway: r.gateway, ifIndex: r.index, metric: r.metric}
		darwinDemoteState.mu.Unlock()
		return r.metric, nil
	}
	return 0, fmt.Errorf("no physical default route found to demote (family %#x)", family)
}
