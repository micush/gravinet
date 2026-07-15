//go:build !windows

package service

// RunService is a no-op off Windows: there is no SCM, so the daemon always runs
// interactively (managed by systemd/launchd, which supervise it as a normal
// foreground process).
func RunService(name string, run func(stop <-chan struct{})) (bool, error) {
	return false, nil
}
