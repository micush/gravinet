//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// selfRestart replaces the current process image with a fresh instance of the
// same binary, same arguments, same environment — the exact effect of a
// manual stop-then-start, triggered automatically instead of needing an
// operator (or an external wake-hook script — sleepwatcher on macOS, a
// systemd-sleep drop-in on Linux) to notice and do it themselves.
//
// syscall.Exec keeps the same PID: to systemd, launchd, or any other
// supervisor watching this process, it never appears to exit at all, so
// there's no restart-loop backoff, no "recovery action" configuration
// needed, and no window where the service looks stopped. The caller is
// expected to have already run the same graceful shutdown a SIGTERM would
// trigger (closing devices, transports, etc.) — Exec does not run deferred
// cleanup, so anything not already closed before this call leaks into the
// new process's fresh state (typically harmless for sockets/fds, since the
// OS reclaims them on close, but the explicit shutdown is what makes the new
// process's startup — which recreates the TUN device and rebinds the
// transport — collision-free rather than racing the old file descriptors'
// teardown).
func selfRestart() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}
