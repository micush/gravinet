//go:build freebsd || darwin || openbsd

package hostnet

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
)

// Host addressing on the BSDs and macOS, through ifconfig(8) and route(8).
//
// Shelling out rather than talking to a route socket, matching what
// internal/tun already does to configure the overlay device on these
// platforms: both ship in the base system, both are stable interfaces, and a
// hand-rolled SIOCAIFADDR would be a second implementation of something the
// base system already does correctly.

func ifconfigAddr(verb, ifName string, p netip.Prefix) error {
	fam := "inet"
	if p.Addr().Is6() {
		fam = "inet6"
	}
	// ifconfig takes addr/prefixlen for both families here, which avoids
	// converting a v4 prefix length into a dotted netmask.
	spec := p.Addr().String() + "/" + strconv.Itoa(p.Bits())
	out, err := exec.Command("ifconfig", ifName, fam, spec, verb).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s %s %s %s: %v (%s)", ifName, fam, spec, verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func addAddr(ifName string, p netip.Prefix) error {
	return ifconfigAddr("alias", ifName, p)
}

func delAddr(ifName string, p netip.Prefix) error {
	// OpenBSD spells removal "delete"; FreeBSD and macOS accept "-alias".
	// Try the portable-ish one first and fall back rather than branching on
	// GOOS, so a platform that accepts either keeps working.
	if err := ifconfigAddr("-alias", ifName, p); err == nil {
		return nil
	}
	return ifconfigAddr("delete", ifName, p)
}

func setGateway(gw netip.Addr, ifName string) error {
	fam := "-inet"
	if gw.Is6() {
		fam = "-inet6"
	}
	// Delete then add, for the same reason as the Linux path: a host can
	// carry more than one default and changing one of them can leave the
	// effective default untouched. A failing delete is ignored — there may
	// simply not be one — and only the add is allowed to fail the request.
	_ = exec.Command("route", "-n", "delete", fam, "default").Run()
	out, err := exec.Command("route", "-n", "add", fam, "default", gw.String()).CombinedOutput()
	if err != nil {
		return fmt.Errorf("route add %s default %s: %v (%s)", fam, gw, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func setMTU(ifName string, mtu int) error {
	out, err := exec.Command("ifconfig", ifName, "mtu", strconv.Itoa(mtu)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s mtu %d: %v (%s)", ifName, mtu, err, strings.TrimSpace(string(out)))
	}
	return nil
}
