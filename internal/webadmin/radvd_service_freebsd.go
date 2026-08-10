//go:build freebsd

package webadmin

import "os/exec"

// raService drives rtadvd through FreeBSD's service(8). Note the daemon
// differs from Linux's: only the radvd config format is rendered today, so a
// FreeBSD host will start rtadvd against a file it does not understand. That
// gap is real and named in the changelog; this exists so the service plumbing
// is not the thing missing when the rtadvd renderer lands.
func raService(action string) bool {
	if action == "enable" {
		return exec.Command("sysrc", "rtadvd_enable=YES").Run() == nil
	}
	if err := exec.Command("timeout", "45", "service", "rtadvd", action).Run(); err == nil {
		return true
	}
	return exec.Command("service", "rtadvd", action).Run() == nil
}
