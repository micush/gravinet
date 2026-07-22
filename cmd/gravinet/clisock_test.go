package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// The daemon binds cfg.ControlSocket and only falls back to the platform default
// when it's empty (cmdRun). The CLI must resolve the same way, or the two ends
// disagree and every control command dies with "connect: no such file or
// directory" naming a path nothing is listening on.
func TestDefaultControlSocketPrefersConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.ControlSocket = filepath.Join(dir, "custom.sock")
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("GRAVINET_CONFIG", path)

	if got, want := defaultControlSocket(), cfg.ControlSocket; got != want {
		t.Fatalf("defaultControlSocket() = %q, want the config's control_socket %q", got, want)
	}
}

// An empty control_socket means "use the platform default" — that's what the
// daemon does, so the CLI has to agree.
func TestDefaultControlSocketFallsBackWhenUnset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	cfg := config.Default()
	cfg.ControlSocket = ""
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("SaveTo: %v", err)
	}
	t.Setenv("GRAVINET_CONFIG", path)

	if got, want := defaultControlSocket(), config.DefaultControlSocket; got != want {
		t.Fatalf("defaultControlSocket() = %q, want platform default %q", got, want)
	}
}

// A missing or unreadable config must not break commands whose whole job is to
// talk to a running daemon: fall back to the platform default, don't fail.
func TestDefaultControlSocketMissingConfig(t *testing.T) {
	t.Setenv("GRAVINET_CONFIG", filepath.Join(t.TempDir(), "nope.json"))

	if got, want := defaultControlSocket(), config.DefaultControlSocket; got != want {
		t.Fatalf("defaultControlSocket() = %q, want platform default %q", got, want)
	}
}

// The bare syscall error only names a path; the hint has to name both causes
// (daemon down, or listening elsewhere) and point at the config that decides it.
func TestControlDialHintMentionsBothCauses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("GRAVINET_CONFIG", path)

	hint := controlDialHint()
	for _, want := range []string{"isn't running", "control_socket", "-sock", path} {
		if !strings.Contains(hint, want) {
			t.Errorf("controlDialHint() missing %q:\n%s", want, hint)
		}
	}
}

// Guard the value this whole bug got misdiagnosed as: /run is a Linux
// (systemd/FHS) convention and does not exist on a stock FreeBSD, macOS or
// OpenBSD, where the traditional location is /var/run. Serve() would fail at
// daemon startup if this ever regressed to /run.
func TestBSDDefaultSocketIsVarRun(t *testing.T) {
	if os.Getenv("GRAVINET_SKIP_PLATFORM") != "" {
		t.Skip("platform check skipped")
	}
	switch runtime.GOOS {
	case "freebsd", "darwin", "openbsd":
		if got := config.DefaultControlSocket; got != "/var/run/gravinet.sock" {
			t.Fatalf("BSD default control socket = %q, want /var/run/gravinet.sock (/run does not exist on a stock BSD)", got)
		}
	}
}
