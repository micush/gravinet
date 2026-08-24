//go:build !linux

package webadmin

// gravinet drives Kea on Linux only (see dhcpSupported), so there is no
// service to talk to here. A stub rather than a build tag at the call site,
// so the apply path reads the same on every platform.
func keaService(string) bool { return false }
