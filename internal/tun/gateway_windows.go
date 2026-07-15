//go:build windows

package tun

// Windows physical default-gateway detection and gateway-routed host
// routes, via the IP Helper API's "2" route functions (netioapi.h,
// exported from iphlpapi.dll): GetIpForwardTable2, CreateIpForwardEntry2,
// DeleteIpForwardEntry2. No golang.org/x/sys/windows dependency — raw
// syscall.NewLazyDLL/.NewProc, matching this package's existing Windows
// convention (see tun_windows.go's WinTun bindings and pty_windows.go's
// ConPTY ones).
//
// The MIB_IPFORWARD_ROW2 struct layout below is not derived from the C
// header by hand — it's checked directly against WireGuard's own Windows
// client (golang.zx2c4.com/wireguard/windows/tunnel/winipcfg), which solves
// this identical problem and is running in production on a large number of
// machines; matching a known-correct, battle-tested Go struct layout is far
// more trustworthy here than re-deriving Win32 struct padding from the C
// header by reasoning alone. This file is independently implemented (no
// dependency on that package — it's not vendored, just used as the
// reference for getting the memory layout right), following this package's
// zero-dependency convention.
//
// GatewaySupported is true here: see gateway_linux.go's doc comment for why
// internal/mesh's syncFullTunnelRoute treats platform support as a hard,
// checked prerequisite for activating full-tunnel at all, not just a
// capability note — the /0 (or the old /1 split, before v314) is the
// dangerous half, installable on every platform via the ordinary AddRoute;
// only the bypass routes these functions back are the safety net.

import (
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"
)

const GatewaySupported = true

// RouteDemotionNeeded is true here: unlike Linux's FIB, Windows doesn't
// reliably let a manually-installed low-metric route on gravinet's own
// adapter win over the physical adapter's own default route just because
// its Metric field is numerically lower — Windows computes an automatic
// interface metric per adapter (interface speed, link state, ...) that
// combines with a route's own metric, and that combination isn't something
// CreateIpForwardEntry2 alone can be relied on to lose against. See
// DemoteDefaultRoute below for the actual fix: deprioritize the physical
// adapter's own default route first, the same shape gateway_freebsd.go's
// DemoteDefaultRoute doc comment describes for the BSD family, even though
// the underlying reason differs (routing-table collision there, automatic
// metric composition here).
const RouteDemotionNeeded = true

var (
	iphlpapi                        = syscall.NewLazyDLL("iphlpapi.dll")
	procGetIPForwardTable2          = iphlpapi.NewProc("GetIpForwardTable2")
	procFreeMibTable                = iphlpapi.NewProc("FreeMibTable")
	procInitializeIPForwardEntry    = iphlpapi.NewProc("InitializeIpForwardEntry")
	procCreateIPForwardEntry2       = iphlpapi.NewProc("CreateIpForwardEntry2")
	procSetIPForwardEntry2          = iphlpapi.NewProc("SetIpForwardEntry2")
	procDeleteIPForwardEntry2       = iphlpapi.NewProc("DeleteIpForwardEntry2")
	procConvertInterfaceLUIDToIndex = iphlpapi.NewProc("ConvertInterfaceLuidToIndex")
)

const (
	winAFInet  = 2  // AF_INET
	winAFInet6 = 23 // AF_INET6, Windows-specific value (differs from Linux's 10)
)

// sockaddrInet mirrors SOCKADDR_INET (ws2ipdef.h): a union big enough for a
// sockaddr_in or sockaddr_in6, tagged by Family. Field layout confirmed
// against winipcfg's RawSockaddrInet.
type sockaddrInet struct {
	family uint16
	data   [26]byte // holds port+addr(+flowinfo+scope_id for v6); interpreted by family
}

func (s *sockaddrInet) setAddr(a netip.Addr) {
	*s = sockaddrInet{}
	if a.Is4() {
		s.family = winAFInet
		b := a.As4()
		copy(s.data[2:6], b[:]) // sockaddr_in: family(2) port(2) addr(4) zero(8)
	} else {
		s.family = winAFInet6
		b := a.As16()
		copy(s.data[6:22], b[:]) // sockaddr_in6: family(2) port(2) flowinfo(4) addr(16) scope_id(4)
	}
}

func (s *sockaddrInet) addr() netip.Addr {
	switch s.family {
	case winAFInet:
		var b [4]byte
		copy(b[:], s.data[2:6])
		return netip.AddrFrom4(b)
	case winAFInet6:
		var b [16]byte
		copy(b[:], s.data[6:22])
		return netip.AddrFrom16(b)
	}
	return netip.Addr{}
}

// ipAddressPrefix mirrors IP_ADDRESS_PREFIX (netioapi.h). Explicit trailing
// padding to 32 bytes total, matching winipcfg's IPAddressPrefix (which
// pads with 2 explicit bytes plus one more Go inserts implicitly for
// 2-byte alignment — written out explicitly here instead so the total size
// doesn't depend on that implicit step).
type ipAddressPrefix struct {
	prefix       sockaddrInet // 28 bytes
	prefixLength uint8
	_            [3]byte // pad to 32
}

// mibIPforwardRow2 mirrors MIB_IPFORWARD_ROW2 (netioapi.h) field-for-field,
// including relying on Go's own struct alignment to insert the same padding
// a C compiler would (e.g. 3 bytes before ValidLifetime, after the single
// SitePrefixLength byte) — deliberately not written out by hand, matching
// winipcfg's own struct, so as not to introduce a mismatch by guessing
// wrong about where padding belongs.
type mibIPforwardRow2 struct {
	interfaceLUID      uint64 // NET_LUID: an 8-byte union, opaque here — always left zero (see addGatewayRoute's comment on why InterfaceIndex is used instead)
	interfaceIndex     uint32
	destinationPrefix  ipAddressPrefix
	nextHop            sockaddrInet
	sitePrefixLength   uint8
	validLifetime      uint32
	preferredLifetime  uint32
	metric             uint32
	protocol           uint32
	loopback           uint8
	autoconfigAddress  uint8
	publish            uint8
	immortal           uint8
	age                uint32
	origin             uint32
}

// mibIPforwardTable2Header mirrors just the fixed-size head of
// MIB_IPFORWARD_TABLE2 (a ULONG NumEntries followed by the Table[] array) —
// enough to read NumEntries and compute where the array starts via
// unsafe.Pointer arithmetic; the array itself is read directly out of the
// buffer GetIpForwardTable2 allocated; see dumpForwardTable2 below.
type mibIPforwardTable2Header struct {
	numEntries uint32
	_          [4]byte // pad to 8-byte alignment: mibIPforwardRow2's first field is a uint64
}

func convertInterfaceLUIDToIndex(luid uint64) (int32, error) {
	var idx uint32
	r, _, _ := procConvertInterfaceLUIDToIndex.Call(uintptr(unsafe.Pointer(&luid)), uintptr(unsafe.Pointer(&idx)))
	if r != 0 {
		return 0, fmt.Errorf("ConvertInterfaceLuidToIndex: %w", syscall.Errno(r))
	}
	return int32(idx), nil
}

// dumpForwardTable2 calls GetIpForwardTable2 for family and returns every
// row as a Go slice, copied out of the API-allocated buffer before it's
// freed (FreeMibTable) — no pointers into that buffer escape this function.
func dumpForwardTable2(family uint16) ([]mibIPforwardRow2, error) {
	var tablePtr uintptr
	r, _, _ := procGetIPForwardTable2.Call(uintptr(family), uintptr(unsafe.Pointer(&tablePtr)))
	if r != 0 {
		return nil, fmt.Errorf("GetIpForwardTable2: %w", syscall.Errno(r))
	}
	defer procFreeMibTable.Call(tablePtr)

	hdr := (*mibIPforwardTable2Header)(unsafe.Pointer(tablePtr))
	n := int(hdr.numEntries)
	if n == 0 {
		return nil, nil
	}
	first := unsafe.Add(unsafe.Pointer(tablePtr), unsafe.Sizeof(mibIPforwardTable2Header{}))
	rows := unsafe.Slice((*mibIPforwardRow2)(first), n)
	out := make([]mibIPforwardRow2, n)
	copy(out, rows)
	return out, nil
}

// DefaultGateway returns the best physical (non-tunnel) default route for
// family (winAFInet or winAFInet6), ignoring any default route whose
// outgoing interface is excludeIfIndex. See gateway_linux.go's doc comment
// for why this dumps the whole table and filters, rather than asking for a
// single route decision: once gravinet's own full-tunnel route already
// exists, a single lookup can't distinguish it from the genuine physical
// default any more.
func DefaultGateway(family int, excludeIfIndex int32) (Gateway, error) {
	rows, err := dumpForwardTable2(uint16(family))
	if err != nil {
		return Gateway{}, err
	}
	var best Gateway
	found := false
	for _, r := range rows {
		if r.destinationPrefix.prefixLength != 0 {
			continue // not a default route
		}
		if int32(r.interfaceIndex) == excludeIfIndex {
			continue
		}
		gw := r.nextHop.addr()
		if !gw.IsValid() || gw.IsUnspecified() {
			continue // on-link default with no real gateway isn't useful here
		}
		if !found || int(r.metric) < best.Metric {
			best = Gateway{Addr: gw, IfIndex: int32(r.interfaceIndex), Metric: int(r.metric)}
			found = true
		}
	}
	if !found {
		return Gateway{}, fmt.Errorf("no physical default route found (family %#x)", family)
	}
	return best, nil
}

// DemoteDefaultRoute finds the current physical (non-tunnel) default route
// for family — the same lookup DefaultGateway performs, excluding
// excludeIfIndex the same way — and reprograms just its Metric field to
// newMetric via SetIpForwardEntry2, leaving every other field (NextHop,
// InterfaceIndex, InterfaceLuid, ...) exactly as GetIpForwardTable2 reported
// it: the row read out of the table is mutated in place and passed straight
// back, rather than rebuilt field-by-field the way buildForwardRow does for
// a brand new route, so there's no chance of this dropping or
// mis-populating a field CreateIpForwardEntry2 never needed but
// SetIpForwardEntry2's matching logic does. Returns the route's previous
// metric so the caller can restore it later — see RouteDemotionNeeded's
// doc comment above for why Windows needs this step at all.
func DemoteDefaultRoute(family int, excludeIfIndex int32, newMetric int) (int, error) {
	rows, err := dumpForwardTable2(uint16(family))
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.destinationPrefix.prefixLength != 0 {
			continue // not a default route
		}
		if int32(r.interfaceIndex) == excludeIfIndex {
			continue
		}
		gw := r.nextHop.addr()
		if !gw.IsValid() || gw.IsUnspecified() {
			continue // on-link default with no real gateway isn't the physical one
		}
		old := int(r.metric)
		r.metric = uint32(newMetric)
		rr, _, _ := procSetIPForwardEntry2.Call(uintptr(unsafe.Pointer(&r)))
		if rr != 0 {
			return 0, fmt.Errorf("SetIpForwardEntry2: %w", syscall.Errno(rr))
		}
		return old, nil
	}
	return 0, fmt.Errorf("no physical default route found to demote (family %#x)", family)
}

// buildForwardRow populates a mibIPforwardRow2 for p/gateway/ifIndex/metric,
// first zeroing and default-initializing it via InitializeIpForwardEntry —
// the documented, recommended pattern (matching winipcfg's own Init() +
// field-set + Create() sequence) rather than relying on a Go zero value
// alone matching whatever defaults the network stack expects.
func buildForwardRow(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) (mibIPforwardRow2, error) {
	if p.Addr().Is4() != gateway.Is4() {
		return mibIPforwardRow2{}, fmt.Errorf("gateway route: prefix %s and gateway %s are different address families", p, gateway)
	}
	var row mibIPforwardRow2
	procInitializeIPForwardEntry.Call(uintptr(unsafe.Pointer(&row)))
	row.destinationPrefix.prefix.setAddr(p.Addr())
	row.destinationPrefix.prefixLength = uint8(p.Bits())
	row.nextHop.setAddr(gateway)
	row.interfaceIndex = uint32(ifIndex)
	row.metric = uint32(metric)
	return row, nil
}

// AddGatewayRoute installs "<prefix> via <gateway> dev <ifIndex>" — the
// shape a full-tunnel peer-bypass host route needs, as opposed to
// Device.AddRoute's on-link route via this process's own tun. Not a Device
// method, since it targets whatever physical interface ifIndex names, not
// this adapter. Uses InterfaceIndex, not InterfaceLuid — CreateIpForwardEntry2's
// own docs say InterfaceLuid is preferred when set, but leaving it zero and
// setting only InterfaceIndex is documented as falling back correctly, and
// gravinet only ever has an ifindex in hand (from DefaultGateway or
// IfIndex), never a LUID for an arbitrary interface — resolving one just to
// leave the other unset would be pure overhead.
func AddGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	row, err := buildForwardRow(p, gateway, ifIndex, metric)
	if err != nil {
		return err
	}
	r, _, _ := procCreateIPForwardEntry2.Call(uintptr(unsafe.Pointer(&row)))
	if r != 0 {
		// ERROR_OBJECT_ALREADY_EXISTS (5010): treat a route that's already
		// there, at the same key, as success — matches AddRoute's own
		// idempotent-replace behavior on the other platforms.
		if syscall.Errno(r) == 5010 {
			return nil
		}
		return fmt.Errorf("CreateIpForwardEntry2: %w", syscall.Errno(r))
	}
	return nil
}

// DelGatewayRoute removes a route previously installed by AddGatewayRoute.
// A missing route is not treated as an error.
func DelGatewayRoute(p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int) error {
	row, err := buildForwardRow(p, gateway, ifIndex, metric)
	if err != nil {
		return err
	}
	r, _, _ := procDeleteIPForwardEntry2.Call(uintptr(unsafe.Pointer(&row)))
	if r != 0 {
		// ERROR_NOT_FOUND (1168): already gone is fine.
		if syscall.Errno(r) == 1168 {
			return nil
		}
		return fmt.Errorf("DeleteIpForwardEntry2: %w", syscall.Errno(r))
	}
	return nil
}
