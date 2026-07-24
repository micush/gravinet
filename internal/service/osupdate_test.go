package service

import (
	"path/filepath"
	"testing"
	"time"

	"gravinet/internal/config"
)

// This file deliberately never calls ApplyOSUpdates, osUpdatePackageManager,
// or OSUpdatesSupported: all three either shell out to a real package
// manager or report on one actually being present, and the whole point of
// this suite is to never risk a real `apt-get upgrade` (or any other
// manager's equivalent) running as a side effect of the test suite — the
// same discipline sysusers_test.go/groups_test.go/systemsnmp_handler_test.go
// already established for their own real-mutation-capable operations. Only
// the pure scheduling math and the state-file round-trip are covered here.

func mustParse(t *testing.T, layout, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(layout, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestScheduledSlotStart(t *testing.T) {
	// A Wednesday (2026-07-22 is a Wednesday) at some arbitrary time.
	wed := mustParse(t, "2006-01-02 15:04", "2026-07-22 14:30")

	cases := []struct {
		name string
		cfg  config.OSUpdateConfig
		want string // "" means expect the zero Time
	}{
		{"daily always matches", config.OSUpdateConfig{Cadence: "daily", Hour: 3, Minute: 0}, "2026-07-22 03:00"},
		{"weekly matching weekday", config.OSUpdateConfig{Cadence: "weekly", Weekday: 3, Hour: 4, Minute: 15}, "2026-07-22 04:15"}, // 3 = Wednesday
		{"weekly wrong weekday", config.OSUpdateConfig{Cadence: "weekly", Weekday: 1, Hour: 4, Minute: 15}, ""},                    // 1 = Monday
		{"monthly matching day", config.OSUpdateConfig{Cadence: "monthly", DayOfMonth: 22, Hour: 2, Minute: 0}, "2026-07-22 02:00"},
		{"monthly wrong day", config.OSUpdateConfig{Cadence: "monthly", DayOfMonth: 1, Hour: 2, Minute: 0}, ""},
		{"unknown cadence", config.OSUpdateConfig{Cadence: "hourly"}, ""},
		{"empty cadence", config.OSUpdateConfig{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scheduledSlotStart(c.cfg, wed)
			if c.want == "" {
				if !got.IsZero() {
					t.Errorf("got %v, want zero", got)
				}
				return
			}
			want := mustParse(t, "2006-01-02 15:04", c.want)
			if !got.Equal(want) {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestOSUpdateDue(t *testing.T) {
	daily3am := config.OSUpdateConfig{Enabled: true, Cadence: "daily", Hour: 3, Minute: 0}

	cases := []struct {
		name    string
		cfg     config.OSUpdateConfig
		lastRun string // "" means zero (never run)
		now     string
		want    bool
	}{
		{"disabled never due", config.OSUpdateConfig{Cadence: "daily", Hour: 3}, "", "2026-07-22 03:05", false},
		{"never run, past the slot: due", daily3am, "", "2026-07-22 03:05", true},
		{"never run, before the slot: not yet", daily3am, "", "2026-07-22 02:55", false},
		{"already ran after the slot today: not due again", daily3am, "2026-07-22 03:01", "2026-07-22 10:00", false},
		{"last run was yesterday's slot: due today", daily3am, "2026-07-21 03:01", "2026-07-22 03:05", true},
		{"last run was today but before the slot (clock oddity): still due", daily3am, "2026-07-22 01:00", "2026-07-22 03:05", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var lastRun time.Time
			if c.lastRun != "" {
				lastRun = mustParse(t, "2006-01-02 15:04", c.lastRun)
			}
			now := mustParse(t, "2006-01-02 15:04", c.now)
			if got := OSUpdateDue(c.cfg, lastRun, now); got != c.want {
				t.Errorf("OSUpdateDue() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestOSUpdateDueDoesNotDoubleFireAcrossRestarts is the scenario this whole
// design exists for: the daemon is down across the exact scheduled minute,
// comes back later the same day, must still catch up exactly once — not
// zero times (missed entirely) and not repeatedly (fired on every tick
// after catching up).
func TestOSUpdateDueDoesNotDoubleFireAcrossRestarts(t *testing.T) {
	cfg := config.OSUpdateConfig{Enabled: true, Cadence: "daily", Hour: 3, Minute: 0}
	var lastRun time.Time // never run yet

	// Daemon was down from before 3am until 9am; first tick after restart.
	firstCheck := mustParse(t, "2006-01-02 15:04", "2026-07-22 09:00")
	if !OSUpdateDue(cfg, lastRun, firstCheck) {
		t.Fatal("expected due on the first tick after missing the exact slot")
	}
	// Simulate the run actually happening and being recorded.
	lastRun = firstCheck

	// A later tick the same day must not fire again.
	secondCheck := mustParse(t, "2006-01-02 15:04", "2026-07-22 15:00")
	if OSUpdateDue(cfg, lastRun, secondCheck) {
		t.Error("should not be due again the same day after already catching up")
	}

	// But the next day's slot must fire.
	nextDay := mustParse(t, "2006-01-02 15:04", "2026-07-23 03:05")
	if !OSUpdateDue(cfg, lastRun, nextDay) {
		t.Error("expected due again the next day's slot")
	}
}

func TestNextOSUpdateRun(t *testing.T) {
	after := mustParse(t, "2006-01-02 15:04", "2026-07-22 14:30") // Wednesday afternoon

	t.Run("disabled", func(t *testing.T) {
		if _, ok := NextOSUpdateRun(config.OSUpdateConfig{Cadence: "daily"}, after); ok {
			t.Error("disabled config should never have a next run")
		}
	})

	t.Run("daily finds tomorrow (today's slot already passed)", func(t *testing.T) {
		cfg := config.OSUpdateConfig{Enabled: true, Cadence: "daily", Hour: 3, Minute: 0}
		got, ok := NextOSUpdateRun(cfg, after)
		if !ok {
			t.Fatal("expected a next run")
		}
		want := mustParse(t, "2006-01-02 15:04", "2026-07-23 03:00")
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("daily finds later today if the slot hasn't passed", func(t *testing.T) {
		cfg := config.OSUpdateConfig{Enabled: true, Cadence: "daily", Hour: 23, Minute: 0}
		got, ok := NextOSUpdateRun(cfg, after)
		if !ok {
			t.Fatal("expected a next run")
		}
		want := mustParse(t, "2006-01-02 15:04", "2026-07-22 23:00")
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("monthly rolls into next month when this month's day has passed", func(t *testing.T) {
		cfg := config.OSUpdateConfig{Enabled: true, Cadence: "monthly", DayOfMonth: 1, Hour: 3, Minute: 0}
		got, ok := NextOSUpdateRun(cfg, after)
		if !ok {
			t.Fatal("expected a next run")
		}
		want := mustParse(t, "2006-01-02 15:04", "2026-08-01 03:00")
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("weekly finds the next matching weekday", func(t *testing.T) {
		cfg := config.OSUpdateConfig{Enabled: true, Cadence: "weekly", Weekday: 5, Hour: 3, Minute: 0} // Friday
		got, ok := NextOSUpdateRun(cfg, after)
		if !ok {
			t.Fatal("expected a next run")
		}
		want := mustParse(t, "2006-01-02 15:04", "2026-07-24 03:00")
		if !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})
}

func TestTruncateOSUpdateOutput(t *testing.T) {
	short := "all good, 3 packages upgraded"
	if got := truncateOSUpdateOutput(short); got != short {
		t.Errorf("short output should pass through unchanged: got %q", got)
	}
	long := make([]byte, 5000)
	for i := range long {
		long[i] = 'x'
	}
	got := truncateOSUpdateOutput(string(long))
	if len(got) >= 5000 {
		t.Errorf("expected truncation, got length %d", len(got))
	}
	if got[len(got)-len("(truncated)"):] != "(truncated)" {
		t.Errorf("truncated output should end with a truncation marker, got %q", got[len(got)-30:])
	}
}

func TestOSUpdateStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := OSUpdateStatePath(dir)

	// No file yet: zero value, not an error.
	got := LoadOSUpdateState(path)
	if !got.LastRun.IsZero() || got.LastOK {
		t.Errorf("expected zero-value state before any save, got %+v", got)
	}

	want := OSUpdateState{
		LastRun: time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC),
		LastOK:  true, LastOutput: "3 packages upgraded", LastTriggeredBy: "schedule",
	}
	if err := SaveOSUpdateState(path, want); err != nil {
		t.Fatal(err)
	}
	got = LoadOSUpdateState(path)
	if !got.LastRun.Equal(want.LastRun) || got.LastOK != want.LastOK ||
		got.LastOutput != want.LastOutput || got.LastTriggeredBy != want.LastTriggeredBy {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestOSUpdateStatePathSharesUpgradeStateDir(t *testing.T) {
	got := OSUpdateStatePath("/some/upgrade/statedir")
	want := filepath.Join("/some/upgrade/statedir", "os-update-state.json")
	if got != want {
		t.Errorf("OSUpdateStatePath = %q, want %q", got, want)
	}
}
