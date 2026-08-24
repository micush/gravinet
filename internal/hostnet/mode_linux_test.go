//go:build linux

package hostnet

import (
	"net"
	"strings"
	"testing"
)

// TestSysctlPathRejectsNonComponents is the regression test for the CodeQL
// finding. The interface name is a whole path component here, unlike the
// three places Persist embeds it in the middle of a filename, so a name that
// is not a component must not reach the concatenation.
func TestSysctlPathRejectsNonComponents(t *testing.T) {
	for _, bad := range []string{
		"",
		"..",         // admitted by safeIface: one level up, out of conf/
		"../..",      // and the general case, if a separator ever got through
		"..foo",      // the guidance's check is deliberately broader than ".."
		"eth0/../..", // separator plus traversal
		`eth0\..`,    // the other separator spelling
		"a/b",
		"/etc/passwd",
	} {
		got, err := ifaceSysctlPath6(bad, "accept_ra")
		if err == nil {
			t.Errorf("ifaceSysctlPath6(%q) = %q, nil; want a refusal", bad, got)
		}
	}
}

// TestSysctlPathAcceptsRealInterfaceNames guards the other direction. The
// check exists to keep the value a single component, not to have opinions
// about interface naming, so every shape Linux actually produces must still
// build a path — VLAN sub-interfaces and bridge members especially, since
// those contain the dot that the ".." test is looking for.
func TestSysctlPathAcceptsRealInterfaceNames(t *testing.T) {
	for _, ok := range []string{
		"eth0", "lo", "eno1", "enp3s0", "wlp2s0",
		"eth0.100",  // VLAN: a single dot must not read as traversal
		"br-lan",    // bridge
		"veth1a2b3", // container veth
		"tun0", "gravinet0",
		"bond0.4094",
		"eth0:0", // alias; safeIface admits the colon, so this must too
	} {
		got, err := ifaceSysctlPath6(ok, "accept_ra")
		if err != nil {
			t.Errorf("ifaceSysctlPath6(%q) returned %v; want a path", ok, err)
			continue
		}
		want := "/proc/sys/net/ipv6/conf/" + ok + "/accept_ra"
		if got != want {
			t.Errorf("ifaceSysctlPath6(%q) = %q; want %q", ok, got, want)
		}
	}
}

// TestSysctlPathStaysUnderConfDir is the property the concatenation is
// supposed to have, stated directly: whatever comes out addresses something
// inside the per-interface directory, one level down, and nowhere else.
func TestSysctlPathStaysUnderConfDir(t *testing.T) {
	for _, name := range []string{"eth0", "eth0.100", "br-lan", "eth0:0"} {
		got, err := ifaceSysctlPath6(name, "autoconf")
		if err != nil {
			t.Fatalf("ifaceSysctlPath6(%q): %v", name, err)
		}
		if !strings.HasPrefix(got, sysctlConfDir6) {
			t.Errorf("ifaceSysctlPath6(%q) = %q, which is outside %s", name, got, sysctlConfDir6)
		}
		if rest := strings.TrimPrefix(got, sysctlConfDir6); strings.Count(rest, "/") != 1 {
			t.Errorf("ifaceSysctlPath6(%q) = %q; want exactly one directory below %s", name, got, sysctlConfDir6)
		}
	}
}

// TestApplyRefusesPseudoInterfaces is the test for the bug the alert led to
// rather than the alert itself.
//
// "all" and "default" are directories in /proc/sys/net/ipv6/conf but are not
// interfaces. Writing accept_ra or autoconf under "all" changes the setting
// for every interface on the host; under "default", for every interface
// created later. Both used to be reachable from the interface-edit endpoint,
// because Apply ran applyMode before it discovered there was no such
// interface — so the request failed *after* the kernel had been changed.
//
// safeIface admits both, correctly: they are well-formed names. What rules
// them out is that the kernel has no link by either name.
func TestApplyRefusesPseudoInterfaces(t *testing.T) {
	for _, name := range []string{"all", "default"} {
		if _, err := net.InterfaceByName(name); err == nil {
			t.Skipf("this host really has an interface named %q", name)
		}
		_, _, err := Apply(Spec{Iface: name, Mode6: ModeStatic})
		if err == nil {
			t.Fatalf("Apply(Iface: %q) succeeded; it must refuse a name that is not an interface", name)
		}
		if !strings.Contains(err.Error(), "no interface named") {
			t.Errorf("Apply(Iface: %q) failed with %v; want the refusal to come from the existence check, before applyMode writes to /proc", name, err)
		}
	}
}

// TestApplyChecksExistenceBeforeWriting pins the ordering rather than the two
// names. Any name the kernel does not know must be turned away by the
// existence check, not by GlobalAddrs further down — the whole point is that
// applyMode does not get to run first.
func TestApplyChecksExistenceBeforeWriting(t *testing.T) {
	for _, name := range []string{"nosuchiface0", "conf", "..", "eth0.nonexistent"} {
		if _, err := net.InterfaceByName(name); err == nil {
			continue // vanishingly unlikely, but do not fail on a real NIC
		}
		_, _, err := Apply(Spec{Iface: name, Mode6: ModeStatic})
		if err == nil {
			t.Errorf("Apply(Iface: %q) succeeded; want a refusal", name)
			continue
		}
		if !strings.Contains(err.Error(), "no interface named") &&
			!strings.Contains(err.Error(), "refusing to configure") {
			t.Errorf("Apply(Iface: %q) failed with %v; want the existence or safeIface check, not something further down", name, err)
		}
	}
}
