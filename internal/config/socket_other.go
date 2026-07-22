//go:build !linux && !darwin && !freebsd && !windows && !openbsd

package config

// DefaultControlSocket is the local IPC endpoint used by the CLI. /tmp exists
// on essentially every Unix-like system, unlike /run (Linux-specific) or
// /var/run (not guaranteed here), so it's the safest generic fallback for a
// platform gravinet doesn't otherwise special-case.
var DefaultControlSocket = "/tmp/gravinet.sock"
