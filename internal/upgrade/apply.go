package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// PreflightTimeout bounds each probe of a candidate binary. A binary that
// cannot print its own version in ten seconds is not one we are going to make
// this machine's only copy of gravinet.
const PreflightTimeout = 10 * time.Second

// versionLine matches what `gravinet version` prints. main.go's own comment
// notes that this line is deliberately stable and parseable because the install
// scripts grep it; this is the second consumer, and for the same reason — the
// only component that reliably knows what a binary was built with is that
// binary. Anything else (ldd, file(1), the manifest's own claims) is a guess
// about a file, and we are about to make that file the init system's argv[0].
//
//	gravinet 399 (abc1234) linux/amd64 pam=yes
var versionLine = regexp.MustCompile(`^gravinet (\S+) \((\S+)\) (\w+)/(\w+) pam=(yes|no)`)

// Probe is what a candidate binary says about itself when asked.
type Probe struct {
	Version string
	Commit  string
	OS      string
	Arch    string
	PAM     bool
}

// ProbeBinary executes `path version` and parses the result.
//
// This is the single most valuable check in the package, and it is worth being
// explicit about why: the overwhelming majority of real upgrade bricks are not
// subtle logic regressions, they are "the arm64 build went to the amd64 boxes",
// "the artifact was truncated at 4 MiB by a proxy", or "the new binary is
// dynamically linked against a libpam this host doesn't have". Every one of
// those is a binary that cannot successfully print its own version — and every
// one of them is invisible to a digest check, because the digest of the *wrong*
// binary is perfectly valid. Actually running the thing, before it becomes the
// thing that runs, is what separates a rollout from a fleet-wide outage.
func ProbeBinary(ctx context.Context, path string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil {
		return Probe{}, fmt.Errorf("upgrade: candidate binary failed to run (%v): %s", err, firstLine(out))
	}
	mm := versionLine.FindStringSubmatch(strings.TrimSpace(firstLine(out)))
	if mm == nil {
		return Probe{}, fmt.Errorf("upgrade: candidate binary printed an unrecognized version line: %q", firstLine(out))
	}
	return Probe{Version: mm[1], Commit: mm[2], OS: mm[3], Arch: mm[4], PAM: mm[5] == "yes"}, nil
}

// SelfTest runs `path selftest -config <cfg>`: the candidate loads and validates
// this node's actual config file and exits.
//
// The failure this catches is the nastiest one in the set, because it is the one
// that passes every other gate: a new version that tightened config validation,
// renamed a field, or dropped a deprecated key. The binary is genuine, correctly
// signed, right architecture, prints its version happily — and then refuses to
// start on *this* node's config, in a crash loop, on a node whose only management
// path was the mesh the daemon isn't joining. Handing the candidate the real
// config before the swap turns that from an outage into an error message.
func SelfTest(ctx context.Context, path, configPath string) error {
	if configPath == "" {
		return nil // nothing to test against; ProbeBinary is still the gate
	}
	ctx, cancel := context.WithTimeout(ctx, PreflightTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "selftest", "-config", configPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("upgrade: candidate rejected this node's config: %s", firstLine(out))
	}
	return nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// Options controls one apply.
type Options struct {
	// Target is the path to replace: the binary the service manager re-executes
	// on restart, which is not necessarily os.Args[0] and not necessarily the
	// running process's own path (a daemon started from /tmp during a test, or
	// via a symlink, must still upgrade the installed copy).
	Target string
	// ConfigPath is passed to the candidate's selftest.
	ConfigPath string
	// RunningPAM is whether *this* process has PAM compiled in, for the
	// silent-auth-downgrade check below.
	RunningPAM bool
	// AllowPAMDowngrade permits replacing a pam=yes binary with a pam=no one.
	AllowPAMDowngrade bool
	// AllowDowngrade permits replacing a binary with an older version.
	AllowDowngrade bool
	// RunningVersion is this node's current version, for the downgrade check.
	RunningVersion string
}

// BackupPath is where the outgoing binary is kept. Deliberately a sibling of
// the target rather than a copy in the store: rollback must work when the store
// is unreadable, the disk is nearly full, or the config that names the store has
// itself been mangled. It is also on the same filesystem, which is what makes
// both halves of the swap a rename rather than a copy — and therefore atomic.
func BackupPath(target string) string { return target + ".prev" }

// Preflight runs every check that can be run *without* touching the installed
// binary, and returns what the candidate says it is. Split out from Apply so a
// caller can dry-run — "would this land?" — before it changes anything.
//
// There is no manifest to cross-check against here, and deliberately so: the
// candidate was compiled on this node moments ago, so the binary's own report
// of itself is not one claim among several to be reconciled, it is the only
// claim there is. What a manifest used to add — a second, independent
// assertion about a file that arrived from elsewhere — has no counterpart when
// nothing arrived from elsewhere. The os/arch check below therefore compares
// the probe against this node's own runtime rather than against a document.
func Preflight(ctx context.Context, candidate string, o Options) (Probe, error) {
	p, err := ProbeBinary(ctx, candidate)
	if err != nil {
		return Probe{}, err
	}
	// A locally-compiled binary is native by construction, so this should be
	// unreachable — keep it anyway. It costs one comparison, and the failure it
	// guards (a cross-compiling GOOS/GOARCH leaking in from the service
	// manager's environment) produces a binary that cannot execute at all,
	// which is the single most expensive mistake this package exists to
	// prevent.
	if p.OS != runtime.GOOS || p.Arch != runtime.GOARCH {
		return Probe{}, fmt.Errorf("upgrade: candidate is %s/%s, this node is %s/%s", p.OS, p.Arch, runtime.GOOS, runtime.GOARCH)
	}
	if !o.AllowDowngrade && o.RunningVersion != "" && VersionLess(p.Version, o.RunningVersion) {
		return Probe{}, fmt.Errorf("upgrade: %s is older than the running %s (pass -allow-downgrade to force)", p.Version, o.RunningVersion)
	}
	if o.RunningPAM && !p.PAM && !o.AllowPAMDowngrade {
		return Probe{}, errors.New("upgrade: this node authenticates the web admin against PAM and the new binary has no PAM support — " +
			"it would start cleanly and then be unable to log anyone in (pass -allow-pam-downgrade if that is intended)")
	}
	if err := SelfTest(ctx, candidate, o.ConfigPath); err != nil {
		return Probe{}, err
	}
	return p, nil
}

// Apply copies the freshly-built candidate next to the target, preflights it
// there (so what is tested is byte-for-byte what will be executed, on the
// target's own filesystem and mount flags — a build directory on a noexec
// mount would otherwise pass a preflight run from somewhere else and then fail
// to start), and swaps it in.
//
// The swap is two renames on one filesystem:
//
//	target      -> target.prev     (keep the way back)
//	target.new  -> target          (install)
//
// Neither can leave the target missing: after the first, the installed path is
// briefly absent, and if the second fails the first is undone immediately. A
// crash in that window leaves target.prev on disk, which is exactly what the
// guard's boot path looks for. There is no window in which a *partially written*
// binary occupies the target path, which is the failure mode a naive
// "open target for writing and copy" has and which no amount of retrying fixes.
//
// Apply does not restart anything. Deciding when a node may drop off the mesh
// is the caller's call.
func Apply(ctx context.Context, candidate string, o Options) (Probe, error) {
	if o.Target == "" {
		return Probe{}, errors.New("upgrade: no target path")
	}
	if _, err := os.Stat(candidate); err != nil {
		return Probe{}, fmt.Errorf("upgrade: candidate binary %s: %w", candidate, err)
	}

	staged := o.Target + ".new"
	if err := copyFile(candidate, staged, 0o755); err != nil {
		return Probe{}, err
	}
	// Any failure from here to the final rename must not leave .new behind: a
	// stale, unverified .new file next to the binary is a loaded gun for
	// whatever runs next.
	ok := false
	defer func() {
		if !ok {
			os.Remove(staged)
		}
	}()

	p, err := Preflight(ctx, staged, o)
	if err != nil {
		return Probe{}, err
	}

	backup := BackupPath(o.Target)
	os.Remove(backup) // last upgrade's backup; the current one supersedes it
	if err := os.Rename(o.Target, backup); err != nil {
		return Probe{}, fmt.Errorf("upgrade: backing up the current binary: %w", err)
	}
	if err := os.Rename(staged, o.Target); err != nil {
		// Put it back. If this also fails the node has no binary at its
		// installed path, which is unrecoverable-in-place — say so loudly
		// rather than returning a vague error, because the operator's next
		// move is a manual `mv gravinet.prev gravinet` over a shell that may
		// itself be about to disappear.
		if rerr := os.Rename(backup, o.Target); rerr != nil {
			return Probe{}, fmt.Errorf("upgrade: FAILED TO INSTALL (%v) AND FAILED TO RESTORE (%v): "+
				"the previous binary is at %s and must be renamed back to %s by hand", err, rerr, backup, o.Target)
		}
		return Probe{}, fmt.Errorf("upgrade: installing the new binary (previous one restored): %w", err)
	}
	ok = true
	return p, nil
}

// Revert undoes Apply by swapping the backup back over the target. It is the
// same two-rename dance in reverse, and it is deliberately dumb: no probes, no
// verification, no config parsing. It runs on a node that is, by hypothesis, in
// trouble — possibly mid-crash-loop, possibly with the mesh down — and the
// binary it is restoring is the one that was demonstrably working ten minutes
// ago. Adding checks here adds ways for the rescue path to fail.
func Revert(target string) error {
	backup := BackupPath(target)
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("upgrade: no backup to revert to at %s: %w", backup, err)
	}
	// Keep the binary we're backing out, named so it is obviously not in use.
	// Forensics on a node that crash-looped is a lot easier with the binary
	// still in hand, and the build directory it came from is long gone.
	failed := target + ".failed"
	os.Remove(failed)
	if err := os.Rename(target, failed); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("upgrade: setting aside the failed binary: %w", err)
	}
	if err := os.Rename(backup, target); err != nil {
		return fmt.Errorf("upgrade: restoring %s from %s: %w", target, backup, err)
	}
	return nil
}

// ResolveTarget picks the path an upgrade should replace: the caller's explicit
// choice, else the running executable with symlinks resolved. The symlink
// resolution matters — replacing a symlink with a regular file is how a node
// ends up with a /usr/local/bin/gravinet that is no longer whatever the package
// manager thinks it is.
func ResolveTarget(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("upgrade: locating the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// copyFile writes src to dst with mode, syncing before close. Not a rename:
// src is in a build directory and dst must be a sibling of the target binary,
// which is very often a different filesystem (/tmp vs /usr/local/bin).
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("upgrade: reading built binary: %w", err)
	}
	defer in.Close()
	// O_EXCL: if a .new is already sitting there, something else is mid-apply
	// (or a previous one died). Fail rather than race with it.
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		if os.IsExist(err) {
			// A leftover from a crashed apply — the deferred cleanup in Apply
			// removes it on every path except a hard kill. Reclaim it rather
			// than wedging every future upgrade on this node forever.
			if rmErr := os.Remove(dst); rmErr != nil {
				return fmt.Errorf("upgrade: a stale %s is in the way and could not be removed: %w", dst, rmErr)
			}
			out, err = os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		}
		if err != nil {
			return fmt.Errorf("upgrade: staging next to the target binary: %w", err)
		}
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("upgrade: copying the new binary into place: %w", err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("upgrade: syncing the new binary: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	// Chmod explicitly: the mode passed to OpenFile is masked by umask, and a
	// binary that lands 0644 because the daemon inherited umask 022 from a
	// service manager is a node that will not restart.
	if err := os.Chmod(dst, mode); err != nil {
		os.Remove(dst)
		return fmt.Errorf("upgrade: chmod staged binary: %w", err)
	}
	return nil
}

// VersionLess orders gravinet version strings. They are a monotonically
// increasing integer counter ("398", "399"), so the fast path is numeric — but
// this must not *assume* that, because a hand-built binary carrying "1.2.0-rc1"
// or a git describe string would otherwise sort nonsensically and, worse, defeat
// the downgrade guard in rollout.go (which asks exactly this question before it
// agrees to replace a binary). Numeric when both sides are numeric; lexical
// otherwise, which is at least stable and total.
func VersionLess(a, b string) bool {
	an, aok := atoiStrict(a)
	bn, bok := atoiStrict(b)
	if aok && bok {
		return an < bn
	}
	return a < b
}

func atoiStrict(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}
