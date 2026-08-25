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

func VLANInfo(name string) (string, int, bool) { return "", 0, false }
