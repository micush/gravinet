//go:build linux

package hostnet

import (
	"fmt"
	"os"
	"strings"
)

// applyMode makes the live kernel settings match the spec's modes.
//
// IPv4 has nothing here, and that is not an omission. There is no kernel-side
// IPv4 autoconfiguration: an address either comes from a DHCP client — a
// daemon in userspace that this package does not own and will not start — or it
// is set statically. So switching IPv4 to DHCP is written by Persist and comes
// into effect when Reapply asks the backend to run its client. Faking it here
// by spawning dhclient would put a second, unmanaged DHCP client on a host that
// already has one, which is worse than waiting.
//
// IPv6 is the opposite: both non-static modes are kernel behaviour, reachable
// without any daemon, so they are applied here and take effect immediately.
func applyMode(s Spec) error {
	m6 := s.Mode6.Or(ModeStatic)
	// accept_ra governs whether router advertisements are honoured at all —
	// the default route as much as the prefix — and autoconf governs whether
	// an address is derived from an advertised prefix. The two knobs are
	// exactly the three modes, which is why the mode split is drawn where it
	// is.
	ra, autoconf := "0", "0"
	if m6.AcceptsRA() {
		ra = "1"
	}
	if m6.Autoconf() {
		autoconf = "1"
	}
	for _, kv := range []struct{ key, val string }{
		{"accept_ra", ra},
		{"autoconf", autoconf},
	} {
		if err := writeIfaceSysctl6(s.Iface, kv.key, kv.val); err != nil {
			return err
		}
	}
	return nil
}

// sysctlConfDir6 is the directory holding one subdirectory per interface. It
// also holds "all" and "default", which are not interfaces — see Apply, which
// is what keeps those two from getting here.
const sysctlConfDir6 = "/proc/sys/net/ipv6/conf/"

// ifaceSysctlPath6 builds the path of one per-interface IPv6 sysctl.
//
// The interface name is a single path component here, unprefixed and
// unsuffixed, which is what makes this different from the three places
// Persist puts the same name into a filename: there it lands in the middle of
// "99-gravinet-%s.yaml" and friends, where a lone ".." cannot become a
// component of its own. Here it can, so the check belongs here rather than
// being inherited from safeIface a package level away — the previous version
// of this function said in a comment that it relied on Apply being the only
// caller, and a property that depends on who calls you is one that stops
// holding the day someone else does.
//
// The test is the one CodeQL's own guidance gives for a value that must be a
// single component: no separator, in either spelling, and no "..". It is
// slightly broader than it needs to be — it would also turn away a NIC named
// "eth..0" — but no such interface exists, and the narrower version of this
// check is the kind that gets got by ".../...//".
//
// For the record, ".." was never a way out of here in practice: safeIface
// admits it, but it admits no "/", so it buys exactly one level up, to
// /proc/sys/net/ipv6, which has no accept_ra or autoconf to write. The write
// failed with ENOENT and was swallowed as "no IPv6 here". That is a fact
// about the kernel's /proc layout, not about this code, and it is not the
// sort of thing to keep depending on silently.
func ifaceSysctlPath6(iface, key string) (string, error) {
	if iface == "" ||
		strings.Contains(iface, "/") ||
		strings.Contains(iface, `\`) ||
		strings.Contains(iface, "..") {
		return "", fmt.Errorf("refusing to build a sysctl path from interface name %q", iface)
	}
	return sysctlConfDir6 + iface + "/" + key, nil
}

// writeIfaceSysctl6 sets one per-interface IPv6 sysctl.
func writeIfaceSysctl6(iface, key, val string) error {
	path, err := ifaceSysctlPath6(iface, key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(val+"\n"), 0o644); err != nil {
		// A kernel built without IPv6, or an interface with IPv6 disabled,
		// has no such file. Reporting that as a failure to change addressing
		// would be wrong in a way that matters: there is no IPv6 on this
		// interface to configure, and the IPv4 half of the same edit is fine.
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("setting IPv6 %s on %s: %w", key, iface, err)
	}
	return nil
}
