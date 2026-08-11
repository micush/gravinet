//go:build linux

package hostnet

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux has four backends in wide use and no single answer. Detection order
// is by *which one is actually running or present*, most authoritative first:
//
//   - NetworkManager — Fedora, RHEL/Alma/Rocky, Manjaro, and desktop
//     Debian/Ubuntu. Driven through nmcli rather than by writing keyfiles,
//     because nmcli is the supported interface and reloads NM itself.
//   - netplan — Ubuntu server. A gravinet-owned YAML in /etc/netplan.
//     Netplan is a front-end that renders to networkd or NM, so it must be
//     checked before either of those or the change would be written to the
//     renderer and then overwritten by netplan on the next boot.
//   - systemd-networkd — a gravinet-owned .network unit.
//   - ifupdown — Debian server. A drop-in in /etc/network/interfaces.d,
//     which is only honoured if the main file sources that directory, so
//     that is checked rather than assumed.
//
// Whichever is chosen, gravinet writes one file per interface, marked, and
// never edits a file it did not write.

func detect() *Backend {
	if serviceActive("NetworkManager") {
		if _, err := exec.LookPath("nmcli"); err == nil {
			return &Backend{Name: "NetworkManager", Persist: persistNM}
		}
	}
	if _, err := exec.LookPath("netplan"); err == nil {
		if entries, err := os.ReadDir("/etc/netplan"); err == nil && len(entries) >= 0 {
			return &Backend{Name: "netplan", Persist: persistNetplan}
		}
	}
	if serviceActive("systemd-networkd") {
		return &Backend{Name: "systemd-networkd", Persist: persistNetworkd}
	}
	if interfacesDSourced() {
		return &Backend{Name: "ifupdown", Persist: persistIfupdown}
	}
	return nil
}

func serviceActive(unit string) bool {
	return exec.Command("systemctl", "is-active", "--quiet", unit).Run() == nil
}

// interfacesDSourced reports whether /etc/network/interfaces pulls in the
// drop-in directory. Writing a drop-in that nothing sources is exactly the
// silent-no-op this package exists to avoid.
func interfacesDSourced() bool {
	b, err := os.ReadFile("/etc/network/interfaces")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "source" || f[0] == "source-directory") &&
			strings.Contains(f[1], "interfaces.d") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// NetworkManager
// ---------------------------------------------------------------------------

// persistNM writes through nmcli. It edits the connection NM already has for
// the device rather than creating a second one: two connections for one
// interface race at boot, and which wins is not something an operator can
// reason about from this page.
func persistNM(s Spec) error {
	con, err := nmConnectionFor(s.Iface)
	if err != nil {
		return err
	}
	args := []string{"connection", "modify", con,
		"ipv4.method", "manual", "ipv6.method", "manual"}

	v4, v6 := v4v6(s.Addrs)
	args = append(args, "ipv4.addresses", strings.Join(cidrStrings(v4), ","))
	args = append(args, "ipv6.addresses", strings.Join(cidrStrings(v6), ","))
	if len(v4) == 0 {
		// An empty address list with method manual is refused by NM, so an
		// interface left with no v4 addresses goes back to disabled rather
		// than to a half-state NM will not accept.
		args[3] = "disabled"
	}
	if len(v6) == 0 {
		args[5] = "disabled"
	}
	if s.GW4.IsValid() {
		args = append(args, "ipv4.gateway", s.GW4.String())
	}
	if s.GW6.IsValid() {
		args = append(args, "ipv6.gateway", s.GW6.String())
	}
	if s.MTU > 0 {
		args = append(args, "802-3-ethernet.mtu", strconv.Itoa(s.MTU))
	}
	if out, err := exec.Command("nmcli", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection modify: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nmConnectionFor finds the connection bound to a device.
func nmConnectionFor(iface string) (string, error) {
	out, err := exec.Command("nmcli", "-t", "-f", "NAME,DEVICE", "connection", "show").Output()
	if err != nil {
		return "", fmt.Errorf("nmcli connection show: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		// -t escapes colons in values as \:, so split on the last unescaped
		// separator rather than the first.
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		if strings.TrimSpace(line[i+1:]) == iface {
			return strings.ReplaceAll(line[:i], "\\:", ":"), nil
		}
	}
	return "", fmt.Errorf("NetworkManager has no connection for %s; create one first (nmcli con add) and gravinet will edit it", iface)
}

// ---------------------------------------------------------------------------
// netplan
// ---------------------------------------------------------------------------

func persistNetplan(s Spec) error {
	path := filepath.Join("/etc/netplan", "99-gravinet-"+s.Iface+".yaml")
	if err := writeOwned(path, renderNetplan(s), 0o600); err != nil {
		return err
	}
	// generate rather than apply: apply reconfigures every interface on the
	// host, which is more than was asked for and can bounce links that had
	// nothing to do with this edit. The live change is already in place.
	if out, err := exec.Command("netplan", "generate").CombinedOutput(); err != nil {
		return fmt.Errorf("netplan generate: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func renderNetplan(s Spec) string {
	var b strings.Builder
	b.WriteString(Marker + "\nnetwork:\n  version: 2\n  ethernets:\n    " + s.Iface + ":\n")
	b.WriteString("      dhcp4: false\n      dhcp6: false\n")
	if s.MTU > 0 {
		b.WriteString("      mtu: " + strconv.Itoa(s.MTU) + "\n")
	}
	if len(s.Addrs) > 0 {
		b.WriteString("      addresses:\n")
		for _, a := range cidrStrings(s.Addrs) {
			b.WriteString("        - " + a + "\n")
		}
	}
	// routes:, not the deprecated gateway4/gateway6, which current netplan
	// warns about and will eventually reject.
	var routes []string
	if s.GW4.IsValid() {
		routes = append(routes, "        - to: default\n          via: "+s.GW4.String())
	}
	if s.GW6.IsValid() {
		routes = append(routes, "        - to: default\n          via: "+s.GW6.String())
	}
	if len(routes) > 0 {
		b.WriteString("      routes:\n" + strings.Join(routes, "\n") + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// systemd-networkd
// ---------------------------------------------------------------------------

func persistNetworkd(s Spec) error {
	return writeOwned(filepath.Join("/etc/systemd/network", "70-gravinet-"+s.Iface+".network"),
		renderNetworkd(s), 0o644)
}

func renderNetworkd(s Spec) string {
	var b strings.Builder
	b.WriteString(Marker + "\n[Match]\nName=" + s.Iface + "\n\n")
	if s.MTU > 0 {
		// MTUBytes belongs in [Link], not [Network]; networkd ignores it
		// under [Network] without complaining, which is the quiet kind of
		// wrong this whole package exists to avoid.
		b.WriteString("[Link]\nMTUBytes=" + strconv.Itoa(s.MTU) + "\n\n")
	}
	b.WriteString("[Network]\n")
	for _, a := range cidrStrings(s.Addrs) {
		b.WriteString("Address=" + a + "\n")
	}
	if s.GW4.IsValid() {
		b.WriteString("Gateway=" + s.GW4.String() + "\n")
	}
	if s.GW6.IsValid() {
		b.WriteString("Gateway=" + s.GW6.String() + "\n")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// ifupdown
// ---------------------------------------------------------------------------

func persistIfupdown(s Spec) error {
	return writeOwned(filepath.Join("/etc/network/interfaces.d", "gravinet-"+s.Iface),
		renderIfupdown(s), 0o644)
}

func renderIfupdown(s Spec) string {
	v4, v6 := v4v6(s.Addrs)
	var b strings.Builder
	b.WriteString(Marker + "\n")
	for i, p := range v4 {
		// ifupdown allows one address per stanza; extras become aliases.
		name := s.Iface
		if i > 0 {
			name = fmt.Sprintf("%s:%d", s.Iface, i)
		}
		b.WriteString("\nauto " + name + "\niface " + name + " inet static\n")
		b.WriteString("    address " + p.String() + "\n")
		if i == 0 && s.GW4.IsValid() {
			b.WriteString("    gateway " + s.GW4.String() + "\n")
		}
		if i == 0 && s.MTU > 0 {
			b.WriteString("    mtu " + strconv.Itoa(s.MTU) + "\n")
		}
	}
	for i, p := range v6 {
		b.WriteString("\niface " + s.Iface + " inet6 static\n")
		b.WriteString("    address " + p.String() + "\n")
		if i == 0 && s.GW6.IsValid() {
			b.WriteString("    gateway " + s.GW6.String() + "\n")
		}
	}
	return b.String()
}

// writeOwned writes a gravinet-managed file, refusing to replace one that
// does not carry the marker.
func writeOwned(path, content string, mode os.FileMode) error {
	if b, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(b), Marker) {
		return fmt.Errorf("%s exists and was not written by gravinet; move it aside first", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), mode)
}
