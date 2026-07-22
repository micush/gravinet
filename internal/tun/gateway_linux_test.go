//go:build linux

package tun

import (
	"bufio"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
)

// buildRTMNewRoute assembles a synthetic RTM_NEWROUTE dump message with the
// given family/dstLen/table plus RTA_OIF, RTA_GATEWAY (v4), and RTA_PRIORITY
// attributes, for exercising parseDefaultRoute without any real netlink
// socket or privilege.
func buildRTMNewRoute(t *testing.T, family, dstLen, table byte, oif int32, gw netip.Addr, metric int) []byte {
	t.Helper()
	rtm := make([]byte, syscall.SizeofRtMsg)
	rtm[0] = family
	rtm[1] = dstLen
	rtm[4] = table

	body := append([]byte{}, rtm...)

	oifb := make([]byte, 4)
	binary.NativeEndian.PutUint32(oifb, uint32(oif))
	body = append(body, rtattr(syscall.RTA_OIF, oifb)...)

	if gw.IsValid() {
		if gw.Is4() {
			a := gw.As4()
			body = append(body, rtattr(syscall.RTA_GATEWAY, a[:])...)
		} else {
			a := gw.As16()
			body = append(body, rtattr(syscall.RTA_GATEWAY, a[:])...)
		}
	}

	prio := make([]byte, 4)
	binary.NativeEndian.PutUint32(prio, uint32(metric))
	body = append(body, rtattr(syscall.RTA_PRIORITY, prio)...)

	hdr := make([]byte, syscall.SizeofNlMsghdr)
	binary.NativeEndian.PutUint32(hdr[0:4], uint32(syscall.SizeofNlMsghdr+len(body)))
	binary.NativeEndian.PutUint16(hdr[4:6], uint16(syscall.RTM_NEWROUTE))
	return append(hdr, body...)
}

func TestParseDefaultRoute(t *testing.T) {
	gw := netip.MustParseAddr("192.0.2.1")

	t.Run("main table default route parses", func(t *testing.T) {
		m := buildRTMNewRoute(t, syscall.AF_INET, 0, syscall.RT_TABLE_MAIN, 3, gw, 100)
		e, ok := parseDefaultRoute(m)
		if !ok {
			t.Fatal("expected ok=true for a main-table default route")
		}
		if e.oif != 3 || e.gateway != gw || e.metric != 100 {
			t.Fatalf("got %+v, want oif=3 gateway=%s metric=100", e, gw)
		}
	})

	t.Run("non-default prefix is skipped", func(t *testing.T) {
		m := buildRTMNewRoute(t, syscall.AF_INET, 24, syscall.RT_TABLE_MAIN, 3, gw, 100)
		if _, ok := parseDefaultRoute(m); ok {
			t.Fatal("expected ok=false for a /24 (dst_len=24), not a default route")
		}
	})

	t.Run("non-main table is skipped", func(t *testing.T) {
		m := buildRTMNewRoute(t, syscall.AF_INET, 0, 51, 3, gw, 100)
		if _, ok := parseDefaultRoute(m); ok {
			t.Fatal("expected ok=false for a non-main-table route")
		}
	})

	t.Run("truncated message is rejected, not misparsed", func(t *testing.T) {
		m := buildRTMNewRoute(t, syscall.AF_INET, 0, syscall.RT_TABLE_MAIN, 3, gw, 100)
		if _, ok := parseDefaultRoute(m[:syscall.SizeofNlMsghdr+4]); ok {
			t.Fatal("expected ok=false for a truncated message")
		}
	})

	t.Run("ipv6 gateway parses", func(t *testing.T) {
		gw6 := netip.MustParseAddr("fe80::1")
		m := buildRTMNewRoute(t, syscall.AF_INET6, 0, syscall.RT_TABLE_MAIN, 7, gw6, 50)
		e, ok := parseDefaultRoute(m)
		if !ok || e.gateway != gw6 {
			t.Fatalf("got ok=%v gateway=%s, want %s", ok, e.gateway, gw6)
		}
	})
}

// procNetRouteDefaultGateway independently reads /proc/net/route for the
// kernel's own idea of the default gateway, as a cross-check against
// DefaultGateway that doesn't share any code with it.
func procNetRouteDefaultGateway(t *testing.T) (netip.Addr, bool) {
	t.Helper()
	f, err := os.Open("/proc/net/route")
	if err != nil {
		t.Skipf("no /proc/net/route here: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[1] != "00000000" {
			continue // not a default (0.0.0.0) destination
		}
		var raw uint32
		fmtSscanHex(fields[2], &raw) // gateway column, little-endian hex
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, raw)
		addr, ok := netip.AddrFromSlice(b)
		if !ok || !addr.IsValid() || addr.IsUnspecified() {
			continue
		}
		return addr, true
	}
	return netip.Addr{}, false
}

func TestDefaultGatewayLinux(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_NET_ADMIN to open an AF_NETLINK/NETLINK_ROUTE socket")
	}
	want, ok := procNetRouteDefaultGateway(t)
	if !ok {
		t.Skip("no IPv4 default route present in this environment to compare against")
	}

	got, err := DefaultGateway(syscall.AF_INET, 0)
	if err != nil {
		t.Fatalf("DefaultGateway: %v", err)
	}
	if got.Addr != want {
		t.Fatalf("DefaultGateway returned %s, /proc/net/route says %s", got.Addr, want)
	}

	// A bogus excludeIfIndex that can't match anything real must not change
	// the result — only the real tun ifindex should ever filter a route out.
	got2, err := DefaultGateway(syscall.AF_INET, 999999)
	if err != nil || got2.Addr != want {
		t.Fatalf("DefaultGateway with a non-matching exclude changed the result: %+v, err=%v", got2, err)
	}

	// Excluding the gateway's own real outgoing interface must make it
	// unfindable (simulating "this is gravinet's own tunnel route").
	if _, err := DefaultGateway(syscall.AF_INET, got.IfIndex); err == nil {
		t.Fatal("expected an error once the real default route's own interface is excluded")
	}
}

// TestAddDelGatewayRouteLinux chains DefaultGateway with
// AddGatewayRoute/DelGatewayRoute end to end: discover the real physical
// gateway, install a /32 peer-bypass-style route through it for a throwaway
// TEST-NET-3 destination, confirm the real kernel table shows a gateway
// route (not an on-link one) through the right interface, then remove it.
func TestAddDelGatewayRouteLinux(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_NET_ADMIN")
	}
	gw, err := DefaultGateway(syscall.AF_INET, 0)
	if err != nil {
		t.Skipf("no physical default gateway to test against here: %v", err)
	}
	iface, err := net.InterfaceByIndex(int(gw.IfIndex))
	if err != nil {
		t.Fatalf("resolve ifindex %d to a name: %v", gw.IfIndex, err)
	}

	dst := netip.MustParsePrefix("203.0.113.77/32") // TEST-NET-3, never a real peer
	const metric = 55
	if err := AddGatewayRoute(dst, gw.Addr, gw.IfIndex, metric); err != nil {
		t.Fatalf("AddGatewayRoute: %v", err)
	}
	t.Cleanup(func() { _ = DelGatewayRoute(dst, gw.Addr, gw.IfIndex, metric) })

	gotGW, gotIface, ok := procNetRouteEntry(t, dst.Addr())
	if !ok {
		t.Fatal("203.0.113.77/32 not present in /proc/net/route after AddGatewayRoute")
	}
	if gotIface != iface.Name {
		t.Fatalf("route installed on interface %q, want %q", gotIface, iface.Name)
	}
	if gotGW != gw.Addr {
		t.Fatalf("route gateway is %s, want %s (an on-link/no-gateway route would show 0.0.0.0)", gotGW, gw.Addr)
	}

	if err := DelGatewayRoute(dst, gw.Addr, gw.IfIndex, metric); err != nil {
		t.Fatalf("DelGatewayRoute: %v", err)
	}
	if _, _, ok := procNetRouteEntry(t, dst.Addr()); ok {
		t.Fatal("203.0.113.77/32 still present in /proc/net/route after DelGatewayRoute")
	}
}

// sendForeignProtoRoute installs p via gateway/ifIndex/metric with an
// explicit rtm_protocol other than RTPROT_BOOT — simulating a route
// installed by something other than gravinet itself (a DHCP client,
// NetworkManager, a static config, ...), which is exactly what
// DemoteDefaultRoute (gateway_linux.go) has to be able to remove: the
// pre-existing physical default route, never installed by gravinet, so
// never RTPROT_BOOT. Deliberately doesn't go through sendRouteMsg (which
// only ever emits RTPROT_BOOT on an add) — this needs to control that
// field directly to set up the scenario sendRouteMsg's now-fixed delete
// path has to handle.
func sendForeignProtoRoute(t *testing.T, p netip.Prefix, gateway netip.Addr, ifIndex int32, metric int, protocol byte) error {
	t.Helper()
	rtm := make([]byte, syscall.SizeofRtMsg)
	rtm[0] = syscall.AF_INET
	rtm[1] = byte(p.Bits())
	rtm[4] = syscall.RT_TABLE_MAIN
	rtm[5] = protocol
	rtm[6] = syscall.RT_SCOPE_UNIVERSE
	rtm[7] = syscall.RTN_UNICAST

	dstB := p.Addr().As4()
	gwB := gateway.As4()
	oifb := make([]byte, 4)
	binary.NativeEndian.PutUint32(oifb, uint32(ifIndex))
	prio := make([]byte, 4)
	binary.NativeEndian.PutUint32(prio, uint32(metric))

	body := append([]byte{}, rtm...)
	body = append(body, rtattr(syscall.RTA_DST, dstB[:])...)
	body = append(body, rtattr(syscall.RTA_OIF, oifb)...)
	body = append(body, rtattr(syscall.RTA_GATEWAY, gwB[:])...)
	const rtaPriority = 6
	body = append(body, rtattr(rtaPriority, prio)...)

	total := syscall.SizeofNlMsghdr + len(body)
	msg := make([]byte, syscall.SizeofNlMsghdr)
	binary.NativeEndian.PutUint32(msg[0:4], uint32(total))
	binary.NativeEndian.PutUint16(msg[4:6], uint16(syscall.RTM_NEWROUTE))
	binary.NativeEndian.PutUint16(msg[6:8], uint16(syscall.NLM_F_REQUEST|syscall.NLM_F_ACK|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE))
	binary.NativeEndian.PutUint32(msg[8:12], 1)
	msg = append(msg, body...)

	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
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
	if n >= syscall.SizeofNlMsghdr+4 && binary.NativeEndian.Uint16(resp[4:6]) == syscall.NLMSG_ERROR {
		errno := int32(binary.NativeEndian.Uint32(resp[syscall.SizeofNlMsghdr : syscall.SizeofNlMsghdr+4]))
		if errno != 0 {
			return syscall.Errno(-errno)
		}
	}
	return nil
}

// TestDelGatewayRouteMatchesRegardlessOfProtocol is the regression test for
// the exact bug a live `ip route` surfaced: DemoteDefaultRoute deletes the
// pre-existing *physical* default route once its demoted-metric
// replacement is installed, and that route is never one gravinet itself
// added — a DHCP client's RTPROT_DHCP, in the field report, but the same
// problem exists for RTPROT_STATIC, NetworkManager's own value, or
// anything else that isn't RTPROT_BOOT. sendRouteMsg used to hardcode
// RTPROT_BOOT into every delete request too, and the kernel's
// fib_table_delete treats a nonzero rtm_protocol as an exact-match
// selector — so a delete for a same-dst/gateway/oif/metric route installed
// under any other protocol quietly matched nothing, returned ESRCH, and
// sendRouteMsg's own "deleting an already-gone route is fine" handling
// swallowed that as success. Net effect: DemoteDefaultRoute's delete step
// "succeeded" while leaving the original route fully intact — confirmed
// against a real, physical DHCP-installed default route on a live machine,
// which is exactly why this test installs a foreign-protocol route first
// rather than only checking the happy path.
func TestDelGatewayRouteMatchesRegardlessOfProtocol(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root / CAP_NET_ADMIN")
	}
	gw, err := DefaultGateway(syscall.AF_INET, 0)
	if err != nil {
		t.Skipf("no physical default gateway to test against here: %v", err)
	}

	dst := netip.MustParsePrefix("203.0.113.88/32") // TEST-NET-3, never a real peer
	const metric = 66
	const foreignProto = syscall.RTPROT_STATIC // anything other than RTPROT_BOOT

	if err := sendForeignProtoRoute(t, dst, gw.Addr, gw.IfIndex, metric, foreignProto); err != nil {
		t.Fatalf("install throwaway route with a foreign protocol: %v", err)
	}
	t.Cleanup(func() { _ = DelGatewayRoute(dst, gw.Addr, gw.IfIndex, metric) })

	if _, _, ok := procNetRouteEntry(t, dst.Addr()); !ok {
		t.Fatal("throwaway route not present in /proc/net/route after install")
	}

	if err := DelGatewayRoute(dst, gw.Addr, gw.IfIndex, metric); err != nil {
		t.Fatalf("DelGatewayRoute: %v", err)
	}
	if _, _, ok := procNetRouteEntry(t, dst.Addr()); ok {
		t.Fatal("throwaway route (installed with a foreign protocol) still present after DelGatewayRoute — the exact bug this test guards against")
	}
}

// procNetRouteEntry reads /proc/net/route for addr's exact host entry
// (assumed to appear with a /32 destination mask, as AddGatewayRoute
// installs it) and returns its gateway and interface name.
func procNetRouteEntry(t *testing.T, addr netip.Addr) (gateway netip.Addr, iface string, ok bool) {
	t.Helper()
	f, err := os.Open("/proc/net/route")
	if err != nil {
		t.Skipf("no /proc/net/route here: %v", err)
	}
	defer f.Close()
	a := addr.As4()
	wantDest := binary.LittleEndian.Uint32(a[:])
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		var dest uint32
		fmtSscanHex(fields[1], &dest)
		if dest != wantDest {
			continue
		}
		var raw uint32
		fmtSscanHex(fields[2], &raw)
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, raw)
		gwAddr, _ := netip.AddrFromSlice(b)
		return gwAddr, fields[0], true
	}
	return netip.Addr{}, "", false
}
