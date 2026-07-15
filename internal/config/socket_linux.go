//go:build linux

package config

// DefaultControlSocket is the local IPC endpoint used by the CLI. /run is a
// tmpfs-backed, FHS-standard runtime directory present on every mainstream
// Linux distribution (systemd or not), so a bare path here is safe.
var DefaultControlSocket = "/run/gravinet.sock"
