package tun

import (
	"net"
	"net/netip"
)

// ptpIfconfigArgs builds the "ifconfig <if> inet <args...>" arguments for
// assigning a point-to-point overlay address with an explicit netmask, shared
// by AddIPv4 on darwin (and mirroring what tun_freebsd.go does inline).
//
// Split out into its own file with no _darwin build-tag suffix — unlike the
// rest of tun_darwin.go — specifically so it can be unit-tested here without
// an actual Mac to run ifconfig on: the actual bug this fixed (AddIPv4 on
// darwin silently discarding its prefixLen argument, so the address got
// applied with no netmask at all) was invisible from reading the function
// signature and only obvious once you looked at what string of arguments
// actually reached ifconfig. A test asserting on those arguments directly
// catches a regression back to that the same way running the real command
// would, without needing root or a real interface.
func ptpIfconfigArgs(addr netip.Addr, prefixLen int) []string {
	a := addr.String()
	mask := net.CIDRMask(prefixLen, 32)
	return []string{"inet", a, a, "netmask", net.IP(mask).String()}
}
