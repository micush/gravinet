//go:build linux

package hostnet

import (
	"fmt"
	"os"
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

// writeIfaceSysctl6 sets one per-interface IPv6 sysctl.
//
// The interface name has already been through safeIface by the time Apply
// reaches here, so it cannot climb out of the directory; the check is repeated
// nowhere on the assumption that Apply is the only caller.
func writeIfaceSysctl6(iface, key, val string) error {
	path := "/proc/sys/net/ipv6/conf/" + iface + "/" + key
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
