package main

import (
	"fmt"
	"os"

	"gravinet/internal/config"
)

// defaultControlSocket returns the path the CLI should dial when -sock isn't
// given explicitly.
//
// The daemon binds cfg.ControlSocket from the config file and only falls back
// to config.DefaultControlSocket when that field is empty (see cmdRun). The CLI
// subcommands, however, used to default straight to config.DefaultControlSocket
// without ever opening the config — so the moment anyone set "control_socket"
// to anything other than the platform default, the daemon listened on one path
// and every CLI command dialed another, failing with
//
//	control: dial unix <platform default>: connect: no such file or directory
//
// which reads exactly like "the daemon isn't running" and sent people off
// changing the platform default instead of noticing the mismatch. Resolving the
// same way the daemon does — config first, platform default second — keeps the
// two ends in agreement by construction, on every platform.
//
// GRAVINET_CONFIG overrides the config path for CLI commands that (unlike run
// and the config-editing commands) take no -config flag of their own.
//
// A missing/unreadable/invalid config is not an error here: those commands are
// about talking to a running daemon, not about validating config, and the
// platform default is the right guess in that case anyway.
func defaultControlSocket() string {
	path := os.Getenv("GRAVINET_CONFIG")
	if path == "" {
		path = platformDefaultConfigPath()
	}
	configured := ""
	if cfg, err := config.Load(path); err == nil {
		configured = cfg.ControlSocket
	}
	// Same normalizer the daemon runs on the same value (see cmdRun), so both ends
	// land on the same endpoint even when the config holds a stale one.
	endpoint, _ := config.NormalizeControlSocket(configured)
	return endpoint
}

// controlDialHint explains the two things that actually cause a failed dial of
// the control socket, since the raw syscall error ("no such file or directory")
// only ever names the path and never says which of them it was.
func controlDialHint() string {
	path := os.Getenv("GRAVINET_CONFIG")
	if path == "" {
		path = platformDefaultConfigPath()
	}
	return fmt.Sprintf(`
The daemon creates this socket at startup, so a missing socket means either:
  - the daemon isn't running   -> check the service (e.g. "service gravinet onestatus",
    "systemctl status gravinet", "launchctl print system/com.gravinet.daemon"), and see
    the log path from your config for why it exited; or
  - it's listening elsewhere   -> "control_socket" in %s (or $GRAVINET_CONFIG) sets the
    path for both ends; pass -sock PATH to dial a different one.`, path)
}
