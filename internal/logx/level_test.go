package logx

import "testing"

// TestLevelNameRoundTrips: the web admin reads the live level back through
// LevelName and writes it through config.LogLevel/ParseLevel, so the two must
// agree on spelling. If they drift, the settings dropdown silently shows the
// wrong current value while still applying changes correctly — the worst kind
// of UI bug, because it looks like the setting didn't take.
func TestLevelNameRoundTrips(t *testing.T) {
	for _, name := range []string{"error", "warn", "info", "debug"} {
		SetLevel(ParseLevel(name))
		if got := LevelName(); got != name {
			t.Errorf("SetLevel(ParseLevel(%q)) then LevelName() = %q, want %q", name, got, name)
		}
	}
	SetLevel(LevelInfo)
}

// TestDebugSuppressedAtInfo pins the behaviour that made this whole control
// necessary: at info, Debugf output is dropped. Most *rejection* paths in the
// mesh log only at debug (a replayed handshake, a clock-skew mismatch, a
// handshake claiming our own node id, a failed TLS dial), so at the default
// level a node rejecting every handshake it receives is indistinguishable in
// the log from a node receiving none at all.
func TestDebugSuppressedAtInfo(t *testing.T) {
	SetLevel(LevelInfo)
	if def.GetLevel() != LevelInfo {
		t.Fatalf("level = %v, want info", def.GetLevel())
	}
	if LevelDebug >= LevelInfo {
		t.Fatal("LevelDebug must sort below LevelInfo, or debug output would survive at info")
	}
	SetLevel(LevelDebug)
	if def.GetLevel() != LevelDebug {
		t.Fatal("raising to debug must take effect immediately — no restart")
	}
	SetLevel(LevelInfo)
}

// TestLevelCanBeSetBackToItsBootValue reproduces the bug that made the web
// admin's log-level selector look like it wasn't saving.
//
// reloadFn gated the apply on `newCfg.LogLevel != cfg.LogLevel`, where cfg is
// the config the daemon *booted* with and is never reassigned. Boot at info,
// select debug: applied, because debug != info. Now select info again — the test
// is info != info, false, so SetLevel never runs. The config file saved "info"
// correctly, but the running logger stayed on debug; and /api/config reports the
// live level (as it should), so the settings page snapped straight back to debug.
// The level could be changed away from its boot value exactly once, and never
// back.
//
// The invariant that fixes it, and that this pins: applying a level must depend
// only on what the logger is *currently* at, never on a snapshot. Round-tripping
// to any level and back must work, from any starting point.
func TestLevelCanBeSetBackToItsBootValue(t *testing.T) {
	for _, boot := range []string{"error", "warn", "info", "debug"} {
		SetLevel(ParseLevel(boot))

		for _, other := range []string{"error", "warn", "info", "debug"} {
			// Go somewhere else...
			apply(other)
			if got := LevelName(); got != other {
				t.Fatalf("boot=%s: setting %s gave %s", boot, other, got)
			}
			// ...and back to the boot value. This is the leg that used to fail.
			apply(boot)
			if got := LevelName(); got != boot {
				t.Fatalf("boot=%s: returning to %s after %s gave %s — the level must be settable back to where it started", boot, boot, other, got)
			}
		}
	}
	SetLevel(LevelInfo)
}

// apply mirrors what reloadFn does: compare the requested level against the
// LIVE logger level (never a config snapshot) and set it if it differs.
func apply(want string) {
	if w := ParseLevel(want); w != CurrentLevel() {
		SetLevel(w)
	}
}
