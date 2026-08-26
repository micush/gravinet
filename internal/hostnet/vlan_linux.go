//go:build linux

package hostnet

import (
	"fmt"
	"net"

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
		// caller against the host's own view (see VLANDevices): a name that
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

// VLANDevice is what the host says one tagged interface is: the device it
// rides on, the tag it carries, and whether that tag is the ordinary 802.1Q
// kind gravinet creates.
//
// Parent is empty when the lower link is in another network namespace and its
// ifindex resolves to nothing here. That is "not knowable", not "wrong", and a
// caller comparing it against a definition must not read it as a mismatch.
type VLANDevice struct {
	Parent   string
	ID       int
	QinQ     bool
	Protocol uint16
}

// VLANDevices reports every tagged interface on this host, keyed by device
// name. A name that is absent either does not exist or is not a VLAN — the
// caller distinguishes those with an interface lookup, because the difference
// only matters when a definition claims the name.
//
// Asked once for the whole set rather than per device: the callers are a page
// listing definitions against the host and the parent picker beside it, both
// of which want every device, and one dump means every row is answered from a
// single snapshot rather than from a host that may change between questions.
//
// A dump that fails yields no devices. Being unable to ask costs a label,
// which is what it cost before; what it must not do is fail the operation the
// caller was in the middle of.
func VLANDevices() map[string]VLANDevice {
	links, err := tun.VLANLinks()
	if err != nil {
		return nil
	}
	out := make(map[string]VLANDevice, len(links))
	for name, l := range links {
		out[name] = VLANDevice{
			Parent:   l.Parent,
			ID:       l.ID,
			QinQ:     l.Protocol == tun.EthPrt8021AD,
			Protocol: l.Protocol,
		}
	}
	return out
}
