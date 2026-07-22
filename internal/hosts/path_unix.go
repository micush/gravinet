//go:build !windows

package hosts

// DefaultPath is the OS hosts file. macOS and Linux both use /etc/hosts.
func DefaultPath() string { return "/etc/hosts" }
