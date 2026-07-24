package service

// Scheduled host OS package updates — the backend for System > Upgrade's
// "OS updates" section. Deliberately a different concern from the rest of
// that page: gravinet's own upgrade machinery (internal/upgrade) rebuilds
// and replaces the gravinet binary from source; this instead runs whatever
// the host's own package manager considers "update everything installed" —
// security patches, library bumps, all of it, the same as an operator
// running `apt upgrade` by hand. The two share a page because both answer
// "how does this host stay current," not because they're the same
// mechanism.
//
// Deliberately never reboots on its own, even when an update implies one
// would be worthwhile (a new kernel, a libc bump, ...) — that's a separate,
// consequential decision an operator makes from System > Power, not
// something this feature should do unattended to a mesh node.
//
// Windows is not supported: every platform below has one well-known,
// scriptable "update everything" command; Windows Update has no equivalent
// without pulling in an extra module (PSWindowsUpdate) that isn't part of a
// stock install, a meaningfully different kind of gap than "didn't get to
// it yet" — see OSUpdatesSupported.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"gravinet/internal/config"
)

// osUpdateRunning tracks whether an update pass is currently in flight, so
// the System > Upgrade page can show that (and the handler can decline to
// start a second one on top of it) rather than only ever finding out after
// the fact via the state breadcrumb. A run can take minutes; the HTTP
// handler that starts one returns immediately rather than blocking for
// that whole time, so this is the only way anything else knows "still
// going" versus "done."
var osUpdateRunning atomic.Bool

// OSUpdateRunning reports whether an update pass is currently in flight.
func OSUpdateRunning() bool { return osUpdateRunning.Load() }

// osUpdatePackageManager returns the name of the first supported package
// manager found on this host, or "" if none is. Linux checks in a fixed
// priority order rather than picking arbitrarily, since a host can
// plausibly have more than one manager's binary present (a base install's
// own plus something added later) — apt-get first because it's also the
// most common, not for any deeper reason.
func osUpdatePackageManager() string {
	switch runtime.GOOS {
	case "linux":
		for _, m := range []string{"apt-get", "dnf", "yum", "zypper", "pacman"} {
			if _, err := exec.LookPath(m); err == nil {
				return m
			}
		}
	case "freebsd":
		if _, err := exec.LookPath("pkg"); err == nil {
			return "pkg"
		}
	case "openbsd":
		if _, err := exec.LookPath("pkg_add"); err == nil {
			return "pkg_add"
		}
	case "darwin":
		if _, err := exec.LookPath("softwareupdate"); err == nil {
			return "softwareupdate"
		}
	}
	return ""
}

// OSUpdatesSupported reports whether this host can actually run a scheduled
// update at all.
func OSUpdatesSupported() (bool, string) {
	if runtime.GOOS == "windows" {
		return false, "gravinet doesn't drive Windows Update — there's no simple, dependency-free way to script it the way there is for a package manager"
	}
	if osUpdatePackageManager() == "" {
		return false, "no supported package manager (apt/dnf/yum/zypper/pacman/pkg/pkg_add/softwareupdate) found on this host"
	}
	return true, ""
}

// osUpdateCommands returns the sequence of commands mgr's "update
// everything installed" pass runs — more than one step for managers that
// separate "refresh the package index" from "apply upgrades" (apt,
// zypper, pkg), a single step for the ones that don't (dnf, yum, pacman,
// pkg_add, softwareupdate).
func osUpdateCommands(mgr string) [][]string {
	switch mgr {
	case "apt-get":
		return [][]string{{"apt-get", "update"}, {"apt-get", "-y", "upgrade"}}
	case "dnf":
		return [][]string{{"dnf", "-y", "upgrade"}}
	case "yum":
		return [][]string{{"yum", "-y", "update"}}
	case "zypper":
		return [][]string{{"zypper", "--non-interactive", "refresh"}, {"zypper", "--non-interactive", "update"}}
	case "pacman":
		return [][]string{{"pacman", "-Syu", "--noconfirm"}}
	case "pkg":
		return [][]string{{"pkg", "update"}, {"pkg", "upgrade", "-y"}}
	case "pkg_add":
		return [][]string{{"pkg_add", "-u"}}
	case "softwareupdate":
		return [][]string{{"softwareupdate", "-i", "-a"}}
	default:
		return nil
	}
}

// ApplyOSUpdates runs a full update pass via whatever package manager this
// host has, returning (ok, output). output is the commands' own combined
// output on success (informational — what actually happened) or a
// one-line failure summary on error, not the same thing twice. Synchronous
// and can take minutes for a real update run; callers that don't want to
// block (a scheduler tick, an HTTP handler) should run this in their own
// goroutine, the same way gravinet's own upgrade-apply already handles a
// long-running operation.
func ApplyOSUpdates() (bool, string) {
	mgr := osUpdatePackageManager()
	if mgr == "" {
		_, hint := OSUpdatesSupported()
		return false, hint
	}
	env := os.Environ()
	if mgr == "apt-get" {
		env = append(env, "DEBIAN_FRONTEND=noninteractive")
	}
	var combined strings.Builder
	for _, c := range osUpdateCommands(mgr) {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		combined.Write(out)
		combined.WriteByte('\n')
		if err != nil {
			return false, mgr + " " + strings.Join(c[1:], " ") + " failed: " + lastNonEmptyLine(combined.String())
		}
	}
	return true, truncateOSUpdateOutput(combined.String())
}

// truncateOSUpdateOutput caps stored/returned command output to a
// reasonable length — a full `apt upgrade` on a host with a lot of pending
// packages can run to many kilobytes, more than useful to keep around or
// show on a settings page.
func truncateOSUpdateOutput(s string) string {
	const max = 4000
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… (truncated)"
}

// scheduledSlotStart returns the instant, on now's own date, that cfg's
// cadence says an update should run — or the zero Time if today isn't a
// scheduled day for this cadence at all (the wrong weekday for "weekly",
// the wrong day-of-month for "monthly"). Pure — no clock reads beyond the
// now/cfg already passed in — so it's directly testable against any date.
func scheduledSlotStart(cfg config.OSUpdateConfig, now time.Time) time.Time {
	switch cfg.Cadence {
	case "daily":
		// every day qualifies
	case "weekly":
		if int(now.Weekday()) != cfg.Weekday {
			return time.Time{}
		}
	case "monthly":
		if now.Day() != cfg.DayOfMonth {
			return time.Time{}
		}
	default:
		return time.Time{}
	}
	return time.Date(now.Year(), now.Month(), now.Day(), cfg.Hour, cfg.Minute, 0, 0, now.Location())
}

// OSUpdateDue reports whether cfg says an update should run right now,
// given when the last one actually happened (lastRun's zero value means
// never). Comparing against the *start of today's scheduled slot* — not
// simply "was the last run within the last N hours" — means a daemon that
// was down over its exact scheduled minute still catches up once it's back
// (the slot already started, lastRun is still before it), without firing a
// second time if checked again a few minutes later in the same slot
// (lastRun is now after the slot start).
func OSUpdateDue(cfg config.OSUpdateConfig, lastRun, now time.Time) bool {
	if !cfg.Enabled {
		return false
	}
	slot := scheduledSlotStart(cfg, now)
	if slot.IsZero() || now.Before(slot) {
		return false
	}
	return lastRun.Before(slot)
}

// NextOSUpdateRun searches forward from after (exclusive) for the next
// instant cfg's cadence would run, up to 32 days out — comfortably more
// than a month, so a monthly cadence whose day has already passed this
// month still finds next month's occurrence. Returns (zero, false) when
// disabled or (in principle only) if nothing was found in that window.
// For the System > Upgrade page's own "next run" display, not the
// scheduler itself, which uses OSUpdateDue directly.
func NextOSUpdateRun(cfg config.OSUpdateConfig, after time.Time) (time.Time, bool) {
	if !cfg.Enabled {
		return time.Time{}, false
	}
	for d := 0; d <= 32; d++ {
		day := after.AddDate(0, 0, d)
		slot := scheduledSlotStart(cfg, day)
		if !slot.IsZero() && slot.After(after) {
			return slot, true
		}
	}
	return time.Time{}, false
}

// OSUpdateState is the small on-disk breadcrumb recording the last update
// run's outcome — read by the System > Upgrade page (so "last run" survives
// a gravinet restart, and a browser refresh) and by the scheduler itself
// (so OSUpdateDue has a real lastRun to compare against instead of treating
// every restart as "never ran").
type OSUpdateState struct {
	LastRun    time.Time `json:"last_run,omitempty"`
	LastOK     bool      `json:"last_ok"`
	LastOutput string    `json:"last_output,omitempty"`
	// LastTriggeredBy: "schedule" or "manual" — the page shows this so an
	// operator can tell a run they just clicked apart from one that fired
	// on its own overnight.
	LastTriggeredBy string `json:"last_triggered_by,omitempty"`
}

// OSUpdateStatePath returns where the state breadcrumb lives, under the
// same directory the upgrade guard's own state file already uses
// (upgradeStateDir — pass cfg.UpgradeStateDir()) rather than a directory of
// its own: this feature lives on the same System > Upgrade page, sharing
// its state directory keeps every "how does this host stay current"
// breadcrumb in one place.
func OSUpdateStatePath(upgradeStateDir string) string {
	return filepath.Join(upgradeStateDir, "os-update-state.json")
}

// LoadOSUpdateState reads the breadcrumb, or returns the zero value
// (LastRun never happened) if it doesn't exist yet or can't be parsed —
// never an error a caller has to handle specially, since "no state yet" is
// the ordinary case for a node that's never had this feature turned on.
func LoadOSUpdateState(path string) OSUpdateState {
	b, err := os.ReadFile(path)
	if err != nil {
		return OSUpdateState{}
	}
	var st OSUpdateState
	if err := json.Unmarshal(b, &st); err != nil {
		return OSUpdateState{}
	}
	return st
}

// SaveOSUpdateState writes the breadcrumb via a temp file + rename, so a
// concurrent read (the page can poll this while a long update is running)
// never sees a half-written file.
func SaveOSUpdateState(path string, st OSUpdateState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// RunOSUpdateNow runs ApplyOSUpdates and records the outcome to the state
// breadcrumb at path, tagging it with triggeredBy ("schedule" or
// "manual"). The one function both the scheduler ticker and the manual
// "run now" HTTP action call, so the two can never disagree about what
// "last run" means or how it's recorded. Guarded by osUpdateRunning for
// the whole call, so a caller can check OSUpdateRunning() first to avoid
// starting a second pass on top of one already in flight — this function
// itself doesn't refuse to run concurrently (that check belongs at the
// call site, which has more context about why it's being called), but it
// does keep the flag accurate for whoever is watching it.
func RunOSUpdateNow(path, triggeredBy string) (bool, string) {
	osUpdateRunning.Store(true)
	defer osUpdateRunning.Store(false)
	ok, output := ApplyOSUpdates()
	SaveOSUpdateState(path, OSUpdateState{
		LastRun: time.Now(), LastOK: ok, LastOutput: output, LastTriggeredBy: triggeredBy,
	})
	return ok, output
}
