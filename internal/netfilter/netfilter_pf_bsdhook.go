//go:build freebsd || openbsd

// FreeBSD and OpenBSD both use pf, but neither ships anything like macOS's
// default "com.apple/*" wildcard anchor, and neither has macOS's ref-counted
// pfctl -E/-X (confirmed: those flags are macOS-only additions to pfctl).
// pf's anchor evaluation rule is unconditional: a named anchor loaded via
// `pfctl -a <anchor> -f -` is only ever evaluated if the *currently active*
// main ruleset already references it via a nat-anchor/rdr-anchor (or plain
// anchor) line — loading rules into an unreferenced anchor populates it but
// pf never looks at it. So on these two platforms, gravinet has to ensure
// that hook exists in /etc/pf.conf itself; there is no way around editing
// the operator's pf.conf here the way there is on macOS.
//
// This is standard practice on these platforms, not something unusual to
// gravinet: the FreeBSD Handbook's own ftp-proxy(8) walkthrough requires the
// exact same treatment (add nat-anchor/rdr-anchor/anchor lines for
// "ftp-proxy/*" to /etc/pf.conf) for exactly this reason.
package netfilter

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	pfConfPath   = "/etc/pf.conf"
	hookBeginFmt = "# BEGIN gravinet nat anchor (managed by gravinet — do not edit within this block)\n"
	hookEndFmt   = "# END gravinet nat anchor\n"
)

// hookBlock is the exact text ensureAnchorHook looks for and appends. Kept as
// a function of the anchor name so tests can exercise it without a literal.
func hookBlock(anchor string) string {
	return hookBeginFmt +
		fmt.Sprintf("nat-anchor %q\n", anchor) +
		fmt.Sprintf("rdr-anchor %q\n", anchor) +
		hookEndFmt
}

// ensureAnchorHook makes sure /etc/pf.conf contains our nat-anchor/rdr-anchor
// hook for anchor, appending a clearly marked block if it's missing (or
// creating a minimal pf.conf if the file doesn't exist yet), and reloading
// the ruleset so the hook takes effect. It is idempotent: if the marker is
// already present, it does nothing. It never touches any other line in the
// file, and — deliberately — Clear() never removes this block again once
// added: the hook is inert when the anchor itself is empty, and removing it
// automatically would risk breaking something else that came to depend on
// pf being active in the meantime.
func ensureAnchorHook(anchor string) error {
	existing, err := os.ReadFile(pfConfPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("reading %s: %w", pfConfPath, err)
		}
		existing = nil // no pf.conf yet: we'll create a minimal one below
	}
	if strings.Contains(string(existing), "BEGIN gravinet nat anchor") {
		return nil // hook already present from a previous run
	}
	updated := string(existing)
	if len(updated) > 0 && !strings.HasSuffix(updated, "\n") {
		updated += "\n"
	}
	updated += hookBlock(anchor)
	if err := os.WriteFile(pfConfPath, []byte(updated), 0644); err != nil {
		return fmt.Errorf("writing %s: %w", pfConfPath, err)
	}
	if err := exec.Command("pfctl", "-f", pfConfPath).Run(); err != nil {
		return fmt.Errorf("pfctl -f %s: %w", pfConfPath, err)
	}
	return nil
}

var statusEnabledRe = regexp.MustCompile(`(?i)^Status:\s*Enabled`)

// pfIsEnabled reports whether pf is currently enabled, by parsing `pfctl -s
// info`'s first line ("Status: Enabled for ..." / "Status: Disabled").
func pfIsEnabled() bool {
	out, err := exec.Command("pfctl", "-s", "info").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if statusEnabledRe.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// enablePF turns pf on if it isn't already, reporting whether *this call*
// was the one that flipped it (so Clear only ever disables pf it enabled
// itself, never one the operator already had running for their own rules —
// the same "track and restore only what we changed" pattern internal/ipfwd
// uses for the forwarding sysctls).
func enablePF() (weEnabledIt bool) {
	if pfIsEnabled() {
		return false
	}
	_ = exec.Command("pfctl", "-e").Run()
	return true
}
