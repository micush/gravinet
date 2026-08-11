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
			return &Backend{Name: "NetworkManager", Persist: persistNM, Reapply: reapplyNM}
		}
	}
	if _, err := exec.LookPath("netplan"); err == nil {
		if entries, err := os.ReadDir("/etc/netplan"); err == nil && len(entries) >= 0 {
			return &Backend{Name: "netplan", Persist: persistNetplan, Reapply: reapplyNetplan}
		}
	}
	if serviceActive("systemd-networkd") {
		return &Backend{Name: "systemd-networkd", Persist: persistNetworkd, Reapply: reapplyNetworkd}
	}
	// ifupdown has no per-interface reload that is not `ifdown && ifup`, which
	// takes the link down — including the one carrying the request. Left nil
	// so the operator is told, rather than surprised.
	if interfacesDSourced() {
		return &Backend{Name: "ifupdown", Persist: persistIfupdown}
	}
	return nil
}

// reapplyNetworkd reconfigures one interface through networkctl.
func reapplyNetworkd(s Spec) error {
	if out, err := exec.Command("networkctl", "reconfigure", s.Iface).CombinedOutput(); err != nil {
		return fmt.Errorf("networkctl reconfigure %s: %v (%s)", s.Iface, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// reapplyNetplan is the awkward one. `netplan apply` is the documented way in
// and it reconfigures every interface on the host, which is more than an edit to
// one of them asked for and can bounce links that had nothing to do with it —
// the same reason persistNetplan runs `generate` rather than `apply`.
//
// So: netplan renders to networkd or to NM, and where the renderer offers a
// per-interface reload, that is used instead. Where it does not, the operator is
// told the mode is written and waiting rather than having the host's networking
// bounced underneath them.
func reapplyNetplan(s Spec) error {
	if serviceActive("systemd-networkd") {
		return reapplyNetworkd(s)
	}
	if serviceActive("NetworkManager") {
		if _, err := exec.LookPath("nmcli"); err == nil {
			return reapplyNM(s)
		}
	}
	return ErrNoReapply
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
	if out, err := exec.Command("nmcli", nmModifyArgs(con, s)...).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection modify: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// nmModifyArgs builds the nmcli argv for one interface.
//
// Split out from persistNM so it can be checked without NetworkManager
// present. Every other backend in this package is a render function with a
// test over its output; this one assembled an argv and shelled out, which is
// why an index error in it went unseen.
//
// The methods are decided before the slice is built rather than patched into
// it afterwards. Writing back into a flat argv by position means the correct
// index is a fact about a literal several lines away, and getting it wrong
// produces a command that is still well-formed Go.
func nmModifyArgs(con string, s Spec) []string {
	v4, v6 := v4v6(s.Addrs)
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	// A non-static family has no static addresses to write, and any it still
	// carries are stale. Cleared rather than left in place: an address sitting
	// in an NM profile under method auto is applied alongside the lease at the
	// next boot, which is an interface holding an address nothing manages.
	if mode4 != ModeStatic {
		v4 = nil
	}
	if mode6 != ModeStatic {
		v6 = nil
	}

	// NM's own vocabulary. "auto" means DHCP for IPv4 and RA/SLAAC for IPv6;
	// "dhcp" exists only for IPv6 and is DHCPv6 with the route still from the
	// RA, which is exactly ModeDHCP6.
	//
	// An empty address list with method manual is refused by NM, so a static
	// family left with no addresses goes to disabled rather than to a
	// half-state NM will not accept. That reading is unchanged from before
	// modes existed, and it is now the only thing "disabled" means here —
	// previously it was also standing in for "get one from the network",
	// which it could not do.
	meth4 := "manual"
	switch {
	case mode4 == ModeDHCP:
		meth4 = "auto"
	case len(v4) == 0:
		meth4 = "disabled"
	}
	meth6 := "manual"
	switch {
	case mode6 == ModeSLAAC:
		meth6 = "auto"
	case mode6 == ModeDHCP6:
		meth6 = "dhcp"
	case len(v6) == 0:
		meth6 = "disabled"
	}

	args := []string{"connection", "modify", con,
		"ipv4.method", meth4,
		"ipv6.method", meth6,
		"ipv4.addresses", strings.Join(cidrStrings(v4), ","),
		"ipv6.addresses", strings.Join(cidrStrings(v6), ","),
	}
	// A gateway is only meaningful under manual. Under auto or dhcp the route
	// comes from the lease or the advertisement, and NM would treat one set
	// here as an override of it.
	if s.GW4.IsValid() && mode4 == ModeStatic {
		args = append(args, "ipv4.gateway", s.GW4.String())
	}
	if s.GW6.IsValid() && mode6 == ModeStatic {
		args = append(args, "ipv6.gateway", s.GW6.String())
	}
	if s.MTU > 0 {
		args = append(args, "802-3-ethernet.mtu", strconv.Itoa(s.MTU))
	}
	return args
}

// reapplyNM asks NM to reconfigure one device, which is what starts or stops
// its DHCP client. `device reapply` rather than `connection up`: reapply acts
// on the device already using the connection and leaves the rest of the host
// alone, where `connection up` on a connection bound elsewhere can move it.
func reapplyNM(s Spec) error {
	if out, err := exec.Command("nmcli", "device", "reapply", s.Iface).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli device reapply %s: %v (%s)", s.Iface, err, strings.TrimSpace(string(out)))
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
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	var b strings.Builder
	b.WriteString(Marker + "\nnetwork:\n  version: 2\n  ethernets:\n    " + s.Iface + ":\n")
	b.WriteString("      dhcp4: " + yesNo(mode4 == ModeDHCP) + "\n")
	b.WriteString("      dhcp6: " + yesNo(mode6 == ModeDHCP6) + "\n")
	// accept-ra is written even though netplan's default is true. An implicit
	// default is the wrong thing to rely on for a setting whose whole purpose
	// is to be the difference between two of the three IPv6 modes: static has
	// to say no, and saying nothing would have meant yes.
	b.WriteString("      accept-ra: " + yesNo(mode6.AcceptsRA()) + "\n")
	if s.MTU > 0 {
		b.WriteString("      mtu: " + strconv.Itoa(s.MTU) + "\n")
	}
	if addrs := cidrStrings(s.StaticAddrs(s.Addrs)); len(addrs) > 0 {
		b.WriteString("      addresses:\n")
		for _, a := range addrs {
			b.WriteString("        - " + a + "\n")
		}
	}
	// routes:, not the deprecated gateway4/gateway6, which current netplan
	// warns about and will eventually reject. Only for a static family: under
	// dhcp4 or accept-ra the default route arrives with the address, and a
	// second one written here would compete with it.
	var routes []string
	if s.GW4.IsValid() && mode4 == ModeStatic {
		routes = append(routes, "        - to: default\n          via: "+s.GW4.String())
	}
	if s.GW6.IsValid() && mode6 == ModeStatic {
		routes = append(routes, "        - to: default\n          via: "+s.GW6.String())
	}
	if len(routes) > 0 {
		b.WriteString("      routes:\n" + strings.Join(routes, "\n") + "\n")
	}
	return b.String()
}

// yesNo renders a YAML boolean. Spelled true/false rather than yes/no: netplan
// accepts both, and YAML 1.1's yes/no is the pair that trips people reading the
// file later.
func yesNo(v bool) string {
	if v {
		return "true"
	}
	return "false"
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
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	b.WriteString("[Network]\n")
	// networkd folds both families into one DHCP= key, so the four
	// combinations have to be spelled out rather than written per family.
	b.WriteString("DHCP=" + networkdDHCP(mode4 == ModeDHCP, mode6 == ModeDHCP6) + "\n")
	// IPv6AcceptRA is what makes SLAAC work, and also what gets the default
	// route under DHCPv6 — a DHCPv6 server does not supply one.
	//
	// One asymmetry worth naming: networkd starts a DHCPv6 client of its own
	// accord when an accepted RA carries the managed-address flag. So under
	// slaac this file says "do not ask for DHCPv6", not "refuse it if the
	// router insists". Overriding that would mean fighting the router from a
	// config file, which is not what an operator picking SLAAC is asking for.
	b.WriteString("IPv6AcceptRA=" + yesNo(mode6.AcceptsRA()) + "\n")
	for _, a := range cidrStrings(s.StaticAddrs(s.Addrs)) {
		b.WriteString("Address=" + a + "\n")
	}
	if s.GW4.IsValid() && mode4 == ModeStatic {
		b.WriteString("Gateway=" + s.GW4.String() + "\n")
	}
	if s.GW6.IsValid() && mode6 == ModeStatic {
		b.WriteString("Gateway=" + s.GW6.String() + "\n")
	}
	return b.String()
}

func networkdDHCP(v4, v6 bool) string {
	switch {
	case v4 && v6:
		return "yes"
	case v4:
		return "ipv4"
	case v6:
		return "ipv6"
	}
	return "no"
}

// ---------------------------------------------------------------------------
// ifupdown
// ---------------------------------------------------------------------------

func persistIfupdown(s Spec) error {
	return writeOwned(filepath.Join("/etc/network/interfaces.d", "gravinet-"+s.Iface),
		renderIfupdown(s), 0o644)
}

func renderIfupdown(s Spec) string {
	v4, v6 := v4v6(s.StaticAddrs(s.Addrs))
	mode4, mode6 := s.Mode4.Or(ModeStatic), s.Mode6.Or(ModeStatic)
	var b strings.Builder
	b.WriteString(Marker + "\n")

	if mode4 == ModeDHCP {
		// One stanza, no address: ifupdown runs the host's DHCP client for
		// this interface itself. The MTU still belongs here — it is a link
		// property, not part of the lease.
		b.WriteString("\nauto " + s.Iface + "\niface " + s.Iface + " inet dhcp\n")
		if s.MTU > 0 {
			b.WriteString("    mtu " + strconv.Itoa(s.MTU) + "\n")
		}
	}
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

	switch mode6 {
	case ModeSLAAC:
		// `inet6 auto` is ifupdown's spelling for kernel autoconfiguration,
		// and it sets accept_ra itself. The explicit accept_ra line is there
		// because a distribution that ships accept_ra defaulted to 0 turns
		// `auto` into a stanza that configures nothing.
		b.WriteString("\niface " + s.Iface + " inet6 auto\n    accept_ra 1\n")
	case ModeDHCP6:
		// dhcp for the address, accept_ra for the default route the server
		// will not be providing.
		b.WriteString("\niface " + s.Iface + " inet6 dhcp\n    accept_ra 1\n")
	default:
		for i, p := range v6 {
			b.WriteString("\niface " + s.Iface + " inet6 static\n")
			b.WriteString("    address " + p.String() + "\n")
			if i == 0 && s.GW6.IsValid() {
				b.WriteString("    gateway " + s.GW6.String() + "\n")
			}
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
