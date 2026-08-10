//go:build !freebsd

package webadmin

import "os/exec"

// raService drives the radvd unit through systemd, mirroring frrService.
// The timeout wrapper is the same defence: a wedged unit must not hold a
// request open indefinitely.
func raService(action string) bool {
	if err := exec.Command("timeout", "45", "systemctl", action, "radvd").Run(); err == nil {
		return true
	}
	return exec.Command("systemctl", action, "radvd").Run() == nil
}
