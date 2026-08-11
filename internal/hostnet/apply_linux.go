//go:build linux

package hostnet

import (
	"net"
	"net/netip"
	"syscall"

	"gravinet/internal/tun"
)

func addAddr(ifName string, p netip.Prefix) error { return tun.AddAddr(ifName, p) }
func delAddr(ifName string, p netip.Prefix) error { return tun.DelAddr(ifName, p) }

func setGateway(gw netip.Addr, ifName string) error {
	fam := syscall.AF_INET
	if gw.Is6() {
		fam = syscall.AF_INET6
	}
	return tun.ReplaceDefaultRoute(fam, gw, ifName)
}

// setMTU sets an interface's MTU over rtnetlink, matching the rest of this
// package's approach on Linux.
func setMTU(ifName string, mtu int) error {
	ifi, err := net.InterfaceByName(ifName)
	if err != nil {
		return err
	}
	return tun.SetLinkMTU(int32(ifi.Index), mtu)
}
