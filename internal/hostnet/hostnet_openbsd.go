//go:build openbsd

package hostnet

import (
	"os"
	"strings"
)

// OpenBSD keeps per-interface configuration in /etc/hostname.<if> and the
// default gateway in /etc/mygate. Both are plain files with no supported
// editor, so gravinet writes them whole — and refuses to replace one it did
// not write.
func detect() *Backend {
	if _, err := os.Stat("/etc"); err != nil {
		return nil
	}
	return &Backend{Name: "hostname.if", Persist: persistHostnameIf}
}

func persistHostnameIf(s Spec) error {
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	if mode6 == ModeDHCP6 {
		return errNoDHCPv6("OpenBSD", "use slaac, which is what an OpenBSD host on a v6 network normally wants")
	}
	path := "/etc/hostname." + s.Iface
	var lines []string
	lines = append(lines, Marker)
	// OpenBSD spells both non-static modes "autoconf", per family: `inet
	// autoconf` runs dhcpleased, `inet6 autoconf` turns on RA handling and
	// slaacd. The two are independent lines in the same file, which is exactly
	// the per-family split this feature is built around — this backend needed
	// the least persuading of any of them.
	if mode4 == ModeDHCP {
		lines = append(lines, "inet autoconf")
	}
	if mode6 == ModeSLAAC {
		lines = append(lines, "inet6 autoconf")
	}
	for _, p := range s.StaticAddrs(s.Addrs) {
		fam := "inet"
		if p.Addr().Is6() {
			fam = "inet6"
		}
		// hostname.if takes the prefix length directly for both families.
		lines = append(lines, fam+" "+p.Addr().String()+" "+maskOrBits(p))
	}
	if s.MTU > 0 {
		lines = append(lines, "mtu "+itoa(s.MTU))
	}
	if err := writeOwnedFile(path, joinLines(lines...), 0o640); err != nil {
		return err
	}
	// mygate carries both families, one per line, and is not per-interface —
	// so it is only rewritten when a gateway was actually given. A non-static
	// family never has one: the lease or the advertisement carries the route,
	// and a line here would compete with it on every boot.
	var gws []string
	if s.GW4.IsValid() && mode4 == ModeStatic {
		gws = append(gws, s.GW4.String())
	}
	if s.GW6.IsValid() && mode6 == ModeStatic {
		gws = append(gws, s.GW6.String())
	}
	if len(gws) == 0 {
		return nil
	}
	return writeOwnedFile("/etc/mygate", joinLines(append([]string{Marker}, gws...)...), 0o644)
}

// maskOrBits renders the netmask in the form hostname.if expects: a dotted
// mask for IPv4, a prefix length for IPv6.
func maskOrBits(p netipPrefix) string {
	if p.Addr().Is6() {
		return itoa(p.Bits())
	}
	return dottedMask(p)
}

func writeOwnedFile(path, content string, mode os.FileMode) error {
	if b, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(b), Marker) {
		return errNotOurs(path)
	}
	return os.WriteFile(path, []byte(content), mode)
}
