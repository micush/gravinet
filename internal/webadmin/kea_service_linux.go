//go:build linux

package webadmin

import "os/exec"

// keaService drives the Kea DHCPv4 unit through systemd, mirroring raService
// and frrService. The timeout wrapper is the same defence: a wedged unit must
// not hold a request open indefinitely.
//
// The unit is named differently across distributions — Debian and Ubuntu ship
// kea-dhcp4-server, Fedora and Arch ship kea-dhcp4 — and unlike the binary,
// which pkgman's two candidate package names resolve, there is no lookup for
// a unit name. So both are tried and the first that works wins. An action that
// succeeds on neither is reported as failure by the caller, which is what
// turns a wrong guess here into a message rather than a silent no-op.
var keaUnits = []string{"kea-dhcp4-server", "kea-dhcp4"}

func keaService(action string) bool {
	for _, unit := range keaUnits {
		if exec.Command("timeout", "45", "systemctl", action, unit).Run() == nil {
			return true
		}
	}
	for _, unit := range keaUnits {
		if exec.Command("systemctl", action, unit).Run() == nil {
			return true
		}
	}
	return false
}
