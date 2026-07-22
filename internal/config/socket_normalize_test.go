package config

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The bug this guards: install-*.sh scaffolds config.json via "run -init", and
// Default() used to write the then-current DefaultControlSocket into it. Any box
// installed before the /run -> /var/run correction therefore has the stale value
// frozen in its config file, where it beats the corrected code default forever.
// A freshly scaffolded config must not name a socket path at all.
func TestDefaultDoesNotFreezeControlSocket(t *testing.T) {
	cfg := Default()
	if cfg.ControlSocket != "" {
		t.Fatalf("Default().ControlSocket = %q, want empty so the platform default is followed at runtime", cfg.ControlSocket)
	}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "control_socket") {
		t.Errorf("scaffolded config serializes control_socket; it must be omitted so a save can't freeze today's default:\n%s", b)
	}
}

// Empty means "follow the platform default" — the daemon's long-standing
// behavior, now shared with the CLI.
func TestNormalizeEmptyUsesPlatformDefault(t *testing.T) {
	got, note := NormalizeControlSocket("")
	if got != DefaultControlSocket {
		t.Errorf("NormalizeControlSocket(\"\") = %q, want %q", got, DefaultControlSocket)
	}
	if note != "" {
		t.Errorf("unexpected note for an unset socket: %q", note)
	}
}

// The migration itself: the stale Linux-only value must not be honoured on a
// platform that has no /run. On Linux it IS the platform default and must be
// left exactly as-is.
func TestNormalizeLegacyRunPath(t *testing.T) {
	got, note := NormalizeControlSocket(legacyControlSocket)

	if runtime.GOOS == "linux" {
		if got != legacyControlSocket {
			t.Fatalf("on linux /run is the real default; got %q, want it honoured verbatim", got)
		}
		if note != "" {
			t.Errorf("no migration note expected on linux, got %q", note)
		}
		return
	}

	if got == legacyControlSocket {
		t.Fatalf("stale %q was honoured on %s, where /run does not exist — this is the bug", legacyControlSocket, runtime.GOOS)
	}
	if got != DefaultControlSocket {
		t.Errorf("NormalizeControlSocket(legacy) = %q, want the platform default %q", got, DefaultControlSocket)
	}
	if note == "" {
		t.Error("a rewritten config value must come with a note explaining why")
	}
}

// Migration must be surgical: it rewrites exactly one known-stale value. A path
// the operator actually chose — including another path under /run, or a TCP
// endpoint — is theirs, and is honoured verbatim on every platform.
func TestNormalizeHonoursDeliberatePaths(t *testing.T) {
	for _, want := range []string{
		"/var/run/gravinet.sock",
		"/run/custom-name.sock",
		filepath.Join(t.TempDir(), "gravinet.sock"),
		"127.0.0.1:9099",
	} {
		got, note := NormalizeControlSocket(want)
		if got != want {
			t.Errorf("NormalizeControlSocket(%q) = %q, want it honoured verbatim", want, got)
		}
		if note != "" {
			t.Errorf("NormalizeControlSocket(%q) unexpectedly rewrote the value: %s", want, note)
		}
	}
}

// Both ends resolve through this one function, so whatever a config holds, the
// binder and the dialer land on the same endpoint. That agreement is the whole
// point — the original failure was the two disagreeing.
func TestNormalizeIsDeterministic(t *testing.T) {
	for _, in := range []string{"", legacyControlSocket, "/var/run/gravinet.sock", "127.0.0.1:9099"} {
		a, _ := NormalizeControlSocket(in)
		b, _ := NormalizeControlSocket(in)
		if a != b {
			t.Errorf("NormalizeControlSocket(%q) is not deterministic: %q vs %q", in, a, b)
		}
	}
}
