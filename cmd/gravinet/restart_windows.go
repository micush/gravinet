//go:build windows

package main

import "fmt"

// selfRestart is not yet implemented on Windows. Two things make the Unix
// approach (syscall.Exec, a true in-place process-image replacement) not
// translate directly:
//
//   - Windows has no equivalent primitive; the alternative is spawning a
//     detached child process and exiting, but internal/service's SCM
//     integration (run_windows.go) always reports SERVICE_STOPPED with exit
//     code 0 when the service function returns, regardless of why — the
//     Service Control Manager sees a clean, intentional stop either way, not
//     a failure, so even a service configured with a "restart on failure"
//     recovery action wouldn't trigger from that report.
//   - A spawned child process also wouldn't inherit the original's
//     registration with the SCM, so `services.msc`/`sc query` would show
//     the service as Stopped while an orphaned process kept the mesh running
//     unmanaged underneath it — worse than doing nothing, since the two
//     would disagree about whether gravinet is running at all.
//
// Fixing this properly needs run_windows.go to support reporting a
// service-specific non-zero exit code so a configured recovery action can
// take over, which is more than this one file. Until then, the caller falls
// back to the existing in-process best-effort recovery (onResume) alone on
// Windows, same as before this change, and logs that a manual restart is
// the reliable path after a sleep/resume-related connectivity problem.
func selfRestart() error {
	return fmt.Errorf("automatic restart after sleep/resume is not yet supported on Windows; restart the gravinet service manually if connectivity looks stuck")
}
