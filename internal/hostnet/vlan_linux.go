//go:build linux

package hostnet

import (
	"fmt"
	"net"
	"os"
	"strings"

	"gravinet/internal/tun"
)

// VLANSupported reports whether this platform can create tagged interfaces.
//
// Linux only, and refused rather than approximated elsewhere — the same call
// v944 made for the DHCP relay. The BSDs spell this `ifconfig vlan0 create
// vlan 100 vlandev em0` and Windows does not really spell it at all: tagging
// there is a property of the NIC driver, exposed through per-vendor tooling
// with no common interface for gravinet to drive. A half-implementation would
// create a device on some hosts and silently not on others, which is worse on
// the page than an honest "not on this platform".
const VLANSupported = true

// EnsureVLAN creates a tagged interface if it is not already there, and brings
// it up. Idempotent, because it runs at every startup and every reload against
// a configuration that is usually already true of the host.
//
// Returns whether it created anything, so a reconciler can log the change and
// stay quiet about the ninety-nine reloads that changed nothing.
func EnsureVLAN(parent, name string, id int) (created bool, err error) {
	if existing, err := net.InterfaceByName(name); err == nil {
		// Already there. Whether it is the right device is checked by the
		// caller against the host's own view (see VLANInfo): a name that
		// belongs to something else is a collision to report, not something
		// to delete and recreate underneath whoever is using it.
		_ = existing
		if upErr := tun.SetLinkUp(name); upErr != nil {
			return false, fmt.Errorf("bringing %s up: %w", name, upErr)
		}
		return false, nil
	}
	if _, err := net.InterfaceByName(parent); err != nil {
		return false, fmt.Errorf("parent interface %s is not on this host", parent)
	}
	if err := tun.AddVLAN(parent, name, id); err != nil {
		return false, fmt.Errorf("creating %s as vlan %d on %s: %w", name, id, parent, err)
	}
	if err := tun.SetLinkUp(name); err != nil {
		// The device exists but is down. Left in place rather than rolled
		// back: a device an operator can see and bring up by hand is a better
		// outcome than one that vanished with an error message.
		return true, fmt.Errorf("created %s but could not bring it up: %w", name, err)
	}
	return true, nil
}

// DeleteVLAN removes a tagged interface. Absent is success: the caller wants
// the device gone, and it is.
func DeleteVLAN(name string) error {
	if _, err := net.InterfaceByName(name); err != nil {
		return nil
	}
	if err := tun.DelLink(name); err != nil {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// VLANInfo reports what the host thinks a device is: its parent and tag if it
// is a VLAN, and ok=false if it is not one or does not exist.
//
// Read from sysfs rather than from netlink. Getting this back over rtnetlink
// means a link dump and unpicking the same nested IFLA_LINKINFO that
// AddVLAN builds, and the only consumer is a page that wants to say "this row
// is vlan 100 on eth0". /sys/class/net/<dev>/ is a stable interface, and
// being unable to read it costs a label rather than a failed operation.
func VLANInfo(name string) (parent string, id int, ok bool) {
	// A VLAN device's lower link is exposed as a lower_<parent> symlink, and
	// its tag through the 8021q directory.
	var tag int
	if _, err := fmt.Sscanf(readSysNet(name, "8021q/vlan_id"), "%d", &tag); err != nil || tag <= 0 {
		return "", 0, false
	}
	entries, err := net.Interfaces()
	if err != nil {
		return "", tag, true
	}
	// iflink is the parent's ifindex; mapping it back to a name avoids
	// globbing for the lower_* symlink.
	var idx int
	if _, err := fmt.Sscanf(readSysNet(name, "iflink"), "%d", &idx); err != nil {
		return "", tag, true
	}
	for _, e := range entries {
		if e.Index == idx {
			return e.Name, tag, true
		}
	}
	return "", tag, true
}

func readSysNet(iface, rel string) string {
	// The name comes from the kernel's own device list rather than from
	// operator input, but it is checked anyway: one path separator is all it
	// would take to read somewhere else entirely, and the check costs nothing
	// next to the syscall it guards.
	if iface == "" || strings.ContainsAny(iface, "/\x00") {
		return ""
	}
	b, err := os.ReadFile("/sys/class/net/" + iface + "/" + rel)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
