//go:build darwin || freebsd || openbsd

// Kernel NAT on the pf-based platforms (macOS, FreeBSD, OpenBSD). All three
// share pfctl and pf.conf(5) syntax, so the rule-loading and teardown logic
// here is common; only how each platform gets its anchor evaluated at all
// (New, in netfilter_darwin.go / netfilter_freebsd.go / netfilter_openbsd.go)
// differs, because pf's anchor-hooking conventions differ per platform (see
// those files for why).
//
// Every rule gravinet installs lives in one gravinet-owned anchor. Apply
// replaces that anchor's entire contents in one `pfctl -a <anchor> -f -`
// call (pf anchors, like nft tables, are loaded as a whole — there is no
// incremental "add one rule" primitive short of pfctl's -a/-f pair), so
// re-applying is idempotent and Clear only ever flushes what we own.
package netfilter

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Manager applies the gravinet NAT ruleset via pf. anchor is the pf anchor
// path gravinet owns (its evaluation is wired in differently per platform —
// see the platform-specific New constructors). release, when non-nil, is
// called by Clear to undo whatever New did to make pf/the anchor active
// (release pf's enable-reference on macOS, restore prior enabled state and
// leave the pf.conf hook in place on FreeBSD/OpenBSD).
type Manager struct {
	anchor  string
	release func()
}

// Backend reports the NAT backend in use.
func (m *Manager) Backend() string { return "pf" }

// Apply installs exactly the given rules into the gravinet anchor, replacing
// whatever was there before. Idempotent: calling it again with the same
// rules is a no-op in effect.
func (m *Manager) Apply(rules []Rule) error {
	cmd := exec.Command("pfctl", "-a", m.anchor, "-f", "-")
	cmd.Stdin = strings.NewReader(pfScript(rules))
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pfctl -a %s -f -: %w: %s", m.anchor, err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// Clear removes every rule gravinet installed and undoes whatever New did to
// make the anchor active in the first place (best-effort: this runs on the
// shutdown path, where a failure should not block exit).
func (m *Manager) Clear() error {
	_ = exec.Command("pfctl", "-a", m.anchor, "-F", "all").Run()
	if m.release != nil {
		m.release()
	}
	return nil
}
