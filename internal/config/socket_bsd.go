//go:build darwin || freebsd || openbsd

package config

// DefaultControlSocket is the local IPC endpoint used by the CLI.
//
// Neither macOS nor FreeBSD has a "/run" directory by default — that's a
// Linux (systemd/FHS) convention. Using it here meant Serve (internal/control)
// silently failed at daemon startup with "no such file or directory" on both
// BSD-family platforms, disabling every control-socket CLI command (status,
// ban, managed, network, ...) without the daemon itself failing to start, so
// the first visible symptom was the CLI, much later, saying the same thing:
// "dial unix /run/gravinet.sock: connect: no such file or directory".
//
// /var/run is the traditional BSD/Darwin equivalent of Linux's /run (and
// exists out of the box on both), so use that instead. This mirrors the
// per-platform handling already done for the config file's default path in
// cmd/gravinet/defaultpath_freebsd.go and defaultpath_other.go.
var DefaultControlSocket = "/var/run/gravinet.sock"
