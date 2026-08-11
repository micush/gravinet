//go:build linux

package webadmin

import (
	"net"
	"syscall"

	"gravinet/internal/tun"
)

// readDefaultGateway asks the same netlink code the mesh uses to find the
// host's default route, rather than growing a second reader here.
func readDefaultGateway(v6 bool) gwInfo {
	fam := syscall.AF_INET
	if v6 {
		fam = syscall.AF_INET6
	}
	g, err := tun.DefaultGateway(fam, 0)
	if err != nil || !g.Addr.IsValid() {
		return gwInfo{}
	}
	name := ""
	if ifi, err := net.InterfaceByIndex(int(g.IfIndex)); err == nil {
		name = ifi.Name
	}
	return gwInfo{addr: g.Addr.String(), iface: name}
}
