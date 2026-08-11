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
	path := "/etc/hostname." + s.Iface
	var lines []string
	lines = append(lines, Marker)
	for _, p := range s.Addrs {
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
	// so it is only rewritten when a gateway was actually given.
	var gws []string
	if s.GW4.IsValid() {
		gws = append(gws, s.GW4.String())
	}
	if s.GW6.IsValid() {
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
