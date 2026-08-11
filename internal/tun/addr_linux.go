//go:build linux

package tun

// Host address configuration over rtnetlink: adding and removing addresses on
// an interface, and replacing a default route. The write counterpart to
// routeread_linux.go, using the same raw-netlink approach as the rest of this
// package — no `ip` binary, no cgo.
//
// These change host networking immediately and without confirmation. That is
// the intended behaviour: `ip addr` does not ask either, and an admin tool
// that second-guesses an operator editing an address is a tool they will stop
// using. The consequence worth knowing is the obvious one — change the
// address you are connected over and the connection goes with it.

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// ifaddrmsg is 8 bytes: family, prefixlen, flags, scope, index.
const sizeofIfAddrMsg = 8

// rtnetlink attribute types for addresses.
const (
	ifaAddress = 1
	ifaLocal   = 2
)

// AddAddr adds an address to an interface. Replaces an existing entry for the
// same prefix rather than erroring, so re-applying an unchanged value is a
// no-op instead of a failure — which is what makes an edit that only changes
// one of several addresses safe to submit whole.
func AddAddr(ifName string, p netip.Prefix) error {
	return addrMsg(syscall.RTM_NEWADDR,
		syscall.NLM_F_REQUEST|syscall.NLM_F_CREATE|syscall.NLM_F_REPLACE|syscall.NLM_F_ACK,
		ifName, p)
}

// DelAddr removes an address from an interface.
func DelAddr(ifName string, p netip.Prefix) error {
	return addrMsg(syscall.RTM_DELADDR, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, ifName, p)
}

func addrMsg(msgType, flags int, ifName string, p netip.Prefix) error {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s: %w", ifName, err)
	}
	addr := p.Addr().Unmap()
	family := syscall.AF_INET
	if addr.Is6() {
		family = syscall.AF_INET6
	}

	ifa := make([]byte, sizeofIfAddrMsg)
	ifa[0] = byte(family)
	ifa[1] = byte(p.Bits())
	ifa[3] = 0 // RT_SCOPE_UNIVERSE; the kernel narrows this for link-locals
	binary.NativeEndian.PutUint32(ifa[4:8], uint32(ifi.Index))

	raw := addr.AsSlice()
	body := append(ifa, rtattr(ifaLocal, raw)...)
	// IFA_ADDRESS is the peer address on a point-to-point link and the same
	// as IFA_LOCAL everywhere else. Sending both keeps this correct on
	// ordinary interfaces without special-casing.
	body = append(body, rtattr(ifaAddress, raw)...)

	return netlinkExec(msgType, flags, body)
}

// ReplaceDefaultRoute points the default route for a family at a new gateway
// on a given interface, removing whatever default was there first.
//
// Delete-then-add rather than NLM_F_REPLACE: a host can carry several default
// routes at different metrics, and replacing one of them would leave the
// others in place and the effective default unchanged — which looks, from the
// outside, exactly like the change not having worked.
func ReplaceDefaultRoute(family int, gw netip.Addr, ifName string) error {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return fmt.Errorf("interface %s: %w", ifName, err)
	}
	var zero netip.Addr
	if family == syscall.AF_INET {
		zero = netip.AddrFrom4([4]byte{})
	} else {
		zero = netip.AddrFrom16([16]byte{})
	}
	def := netip.PrefixFrom(zero, 0)

	// Every existing default is cleared, whichever interface it leaves by,
	// so a stale route cannot outrank the new one.
	for _, r := range allDefaultRoutes(family) {
		_ = DelGatewayRoute(def, r.gw, r.oif, r.metric)
	}
	return AddGatewayRoute(def, gw, int32(ifi.Index), 0)
}

type defRoute struct {
	gw     netip.Addr
	oif    int32
	metric int
}

// allDefaultRoutes lists every default route for a family, whichever
// interface it leaves by.
func allDefaultRoutes(family int) []defRoute {
	rs, err := dumpDefaultRoutes(family)
	if err != nil {
		return nil
	}
	out := make([]defRoute, 0, len(rs))
	for _, r := range rs {
		if r.gateway.IsValid() {
			out = append(out, defRoute{gw: r.gateway, oif: r.oif, metric: r.metric})
		}
	}
	return out
}

// netlinkExec sends one request and waits for its acknowledgement, returning
// the kernel's error if it refused. Unlike netlinkDump this expects exactly
// one reply: an address change either takes or does not.
func netlinkExec(msgType, flags int, body []byte) error {
	s, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return fmt.Errorf("netlink socket: %w", err)
	}
	defer syscall.Close(s)
	if err := syscall.Bind(s, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink bind: %w", err)
	}

	total := syscall.SizeofNlMsghdr + len(body)
	req := make([]byte, syscall.SizeofNlMsghdr, total)
	binary.NativeEndian.PutUint32(req[0:4], uint32(total))
	binary.NativeEndian.PutUint16(req[4:6], uint16(msgType))
	binary.NativeEndian.PutUint16(req[6:8], uint16(flags))
	binary.NativeEndian.PutUint32(req[8:12], 1)
	req = append(req, body...)

	if err := syscall.Sendto(s, req, 0, &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink send: %w", err)
	}
	buf := make([]byte, 8192)
	n, _, err := syscall.Recvfrom(s, buf, 0)
	if err != nil {
		return fmt.Errorf("netlink recv: %w", err)
	}
	msgs, err := syscall.ParseNetlinkMessage(buf[:n])
	if err != nil {
		return fmt.Errorf("netlink parse: %w", err)
	}
	for _, m := range msgs {
		if m.Header.Type == syscall.NLMSG_ERROR && len(m.Data) >= 4 {
			if e := int32(binary.NativeEndian.Uint32(m.Data[0:4])); e != 0 {
				return syscall.Errno(-e)
			}
		}
	}
	return nil
}

// SetLinkMTU sets an interface's MTU via RTM_SETLINK.
func SetLinkMTU(ifIndex int32, mtu int) error {
	// struct ifinfomsg is 16 bytes: family, pad, type, index, flags, change.
	ifi := make([]byte, 16)
	binary.NativeEndian.PutUint32(ifi[4:8], uint32(ifIndex))
	mtuBuf := make([]byte, 4)
	binary.NativeEndian.PutUint32(mtuBuf, uint32(mtu))
	body := append(ifi, rtattr(syscall.IFLA_MTU, mtuBuf)...)
	return netlinkExec(syscall.RTM_SETLINK, syscall.NLM_F_REQUEST|syscall.NLM_F_ACK, body)
}
