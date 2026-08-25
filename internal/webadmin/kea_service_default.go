//go:build !linux

package webadmin

// gravinet drives Kea on Linux only (see dhcpSupported), so there is no
// service to talk to here. Stubs rather than build tags at the call site, so
// the apply path reads the same on every platform.
func keaService(string) bool { return false }

func keaUnit() string { return "kea-dhcp4" }

func keaTestConf(string) (string, bool) { return "", true }

func keaActive() bool { return false }
