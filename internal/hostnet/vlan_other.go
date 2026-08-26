//go:build !linux

package hostnet

import "fmt"

// Tagged interfaces are Linux-only here. See vlan_linux.go for why this is
// refused rather than approximated: the BSDs create a VLAN through
// ifconfig(8) and Windows leaves it to per-vendor NIC tooling, and a feature
// that works on some hosts and silently does nothing on others is worse on
// the page than one that says where it works.
const VLANSupported = false

var errNoVLAN = fmt.Errorf("gravinet can only create tagged interfaces on Linux")

func EnsureVLAN(parent, name string, id int) (bool, error) { return false, errNoVLAN }

func DeleteVLAN(name string) error { return errNoVLAN }

// VLANDevice mirrors the Linux type so callers compile everywhere. See
// vlan_linux.go for what the fields mean.
type VLANDevice struct {
	Parent   string
	ID       int
	QinQ     bool
	Protocol uint16
}

// VLANDevices reports nothing, because nothing here creates a tagged
// interface. A nil map reads the same way an empty one does, so a caller
// looking a device up gets a miss rather than a special case — and every such
// caller is already behind a VLANSupported check, because "gravinet did not
// make this" and "gravinet cannot make this" are different things to say.
func VLANDevices() map[string]VLANDevice { return nil }
