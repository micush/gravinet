//go:build windows

package hostnet

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
)

// Host addressing on Windows, through netsh — the same tool internal/tun
// already uses to configure the overlay device here.

func netshFamily(a netip.Addr) string {
	if a.Is6() {
		return "ipv6"
	}
	return "ipv4"
}

func addAddr(ifName string, p netip.Prefix) error {
	fam := netshFamily(p.Addr())
	spec := p.Addr().String() + "/" + strconv.Itoa(p.Bits())
	return netshRun("interface", fam, "add", "address", "interface="+ifName, "address="+spec)
}

func delAddr(ifName string, p netip.Prefix) error {
	fam := netshFamily(p.Addr())
	return netshRun("interface", fam, "delete", "address", "interface="+ifName, "address="+p.Addr().String())
}

func setGateway(gw netip.Addr, ifName string) error {
	fam := netshFamily(gw)
	def := "0.0.0.0/0"
	if gw.Is6() {
		def = "::/0"
	}
	// As elsewhere: clear any existing default on this interface before
	// adding, so the new one is the effective route rather than one of
	// several. The delete is best-effort.
	_ = exec.Command("netsh", "interface", fam, "delete", "route", def, "interface="+ifName).Run()
	return netshRun("interface", fam, "add", "route", def, "interface="+ifName, "nexthop="+gw.String())
}

func netshRun(args ...string) error {
	out, err := exec.Command("netsh", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("netsh %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// setMTU goes through netsh's subinterface form, which is where Windows keeps
// the MTU. store=persistent is explicit here rather than relied on as the
// default, because unlike address it is the one netsh setting whose default
// store has differed between releases.
func setMTU(ifName string, mtu int) error {
	return netshRun("interface", "ipv4", "set", "subinterface", ifName,
		"mtu="+strconv.Itoa(mtu), "store=persistent")
}
