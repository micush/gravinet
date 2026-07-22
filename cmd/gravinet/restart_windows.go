//go:build windows

package main

import "fmt"

// selfRestart is not implemented on Windows: there is no in-place
// process-image replacement like Unix's syscall.Exec, and a detached child
// process wouldn't inherit the SCM registration (services.msc would show the
// service Stopped while an orphan kept the mesh running underneath — worse than
// nothing).
//
// The service case no longer relies on this at all: when a config change needs
// a full restart, the daemon takes the SCM-recovery path first (see
// service.RestartViaServiceManagerExit and run_windows.go) — it reports a
// failure exit and the SCM's configured recovery action restarts it cleanly.
// selfRestart is only reached on an interactive (non-service) Windows run,
// where there's no SCM to recover us; there it returns an error and the caller
// falls back to the platform service manager or, failing that, the operator.
func selfRestart() error {
	return fmt.Errorf("in-place restart is not supported on an interactive Windows run; restart the gravinet service to fully recover")
}
