// Package hostnet persists host interface addressing across reboots.
//
// Applying an address is the easy half and lives in the caller. This is the
// half that decides whether the change survives, and it is harder for a
// reason that is not technical: boot-time addressing is owned by a different
// tool on every host, and gravinet is a guest on all of them.
//
// Two failure modes shape the whole design. Writing to a backend that is not
// in charge produces a file that looks authoritative and is ignored — the
// setting appears saved and silently is not, which is worse than not writing
// at all. Writing to the backend that *is* in charge means co-owning
// configuration another tool actively manages, which is how a host comes back
// from a reboot with no addresses.
//
// So: detect which backend actually manages the host, use that backend's own
// supported interface wherever one exists (nmcli, netplan, sysrc,
// networksetup, netsh) rather than editing its files behind its back, and own
// exactly one clearly-marked file per interface where no such interface
// exists. Never touch an interface the operator has not named.
package hostnet

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// Backend is one host network configuration system.
type Backend struct {
	// Name is what the operator is told, e.g. "NetworkManager" or "netplan".
	Name string
	// Persist writes the addressing for one interface so it survives a
	// reboot. It does not apply anything: the caller has already done that
	// live, and a backend that also applies would race with it.
	Persist func(Spec) error
}

// Spec is the intended persistent addressing for one interface.
type Spec struct {
	Iface string
	// Addrs are the interface's global addresses, in CIDR form. An empty
	// slice means "no static addresses" and is written as such rather than
	// treated as "leave alone" — an operator who removed the last address
	// meant it.
	Addrs []netip.Prefix
	// MTU is the interface MTU. 0 means "leave it alone", which is what an
	// edit that did not touch the field means — an interface's MTU is
	// usually set by the driver or the link, and treating "unspecified" as
	// "set it to zero" would break every interface gravinet manages.
	MTU int
	// Prune says whether addresses not in Addrs should be removed.
	//
	// True only from an operator editing the interface, where the submitted
	// list is the whole intended set and leaving one out means removing it.
	// False from the reconciler, which runs at every startup and reload
	// against a record that may be older than the interface: an address put
	// there by DHCP, by another tool, or by an edit gravinet never saw is
	// not gravinet's to delete, and deleting it on every reload is how a
	// node loses addressing it never asked gravinet to manage.
	Prune bool
	// GW4/GW6 are optional default gateways. Invalid means "do not write a
	// default route for this family", not "remove one" — a gateway belongs
	// to the host rather than to this interface, and clearing it because an
	// address was edited would be a surprise.
	GW4, GW6 netip.Addr
}

// Marker identifies files gravinet owns. Anything carrying it may be
// rewritten; anything not is left alone, so a hand-maintained config is never
// silently replaced.
const Marker = "# Managed by gravinet. Edits will be overwritten."

// ErrNoBackend is returned when nothing recognisable manages this host's
// networking. Reported to the operator rather than swallowed: a change that
// applied live but will not survive a reboot is a fact they need, and
// guessing a backend would be worse than saying so.
var ErrNoBackend = fmt.Errorf("no supported network configuration backend found on this host")

// v4v6 splits a prefix list by family, which every backend needs.
func v4v6(in []netip.Prefix) (v4, v6 []netip.Prefix) {
	for _, p := range in {
		if p.Addr().Is4() {
			v4 = append(v4, p)
		} else {
			v6 = append(v6, p)
		}
	}
	return
}

func cidrStrings(in []netip.Prefix) []string {
	out := make([]string, 0, len(in))
	for _, p := range in {
		out = append(out, p.String())
	}
	return out
}

// safeIface rejects an interface name that could break out of a config file
// or a command line. Names come from the host's own interface list, so this
// is a backstop rather than the primary defence.
func safeIface(n string) bool {
	if n == "" || len(n) > 64 {
		return false
	}
	for _, r := range n {
		if !(r == '.' || r == '_' || r == '-' || r == ':' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// Persist finds the backend managing this host and writes the spec through
// it, returning the backend's name for the operator.
func Persist(s Spec) (string, error) {
	if !safeIface(s.Iface) {
		return "", fmt.Errorf("refusing to write configuration for interface name %q", s.Iface)
	}
	b := detect()
	if b == nil {
		return "", ErrNoBackend
	}
	if err := b.Persist(s); err != nil {
		return b.Name, err
	}
	return b.Name, nil
}

// Describe reports which backend would be used, without writing anything, so
// the UI can say where a change will be recorded before one is made.
func Describe() string {
	if b := detect(); b != nil {
		return b.Name
	}
	return ""
}

// netipPrefix aliases netip.Prefix so the per-platform files can name it
// without each importing net/netip for one signature.
type netipPrefix = netip.Prefix

func itoa(n int) string { return strconv.Itoa(n) }

// errNotOurs refuses to replace a file gravinet did not write.
func errNotOurs(path string) error {
	return fmt.Errorf("%s exists and was not written by gravinet; move it aside first", path)
}

// dottedMask renders an IPv4 prefix length as a dotted netmask, which several
// backends want instead of a prefix length.
func dottedMask(p netip.Prefix) string {
	bits := p.Bits()
	var m [4]byte
	for i := 0; i < 4; i++ {
		n := bits - i*8
		switch {
		case n >= 8:
			m[i] = 0xff
		case n > 0:
			m[i] = byte(0xff << (8 - n))
		}
	}
	return fmt.Sprintf("%d.%d.%d.%d", m[0], m[1], m[2], m[3])
}

func joinLines(parts ...string) string {
	return strings.Join(parts, "\n") + "\n"
}
