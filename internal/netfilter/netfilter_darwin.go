//go:build darwin

package netfilter

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// darwinAnchor lives under macOS's built-in "com.apple/*" wildcard, which
// /etc/pf.conf hooks with nat-anchor/rdr-anchor/anchor lines by default on
// every stock macOS install. Loading rules here (via `pfctl -a <anchor> -f
// -`) makes them evaluate immediately, with no edit to /etc/pf.conf and no
// risk of colliding with the operator's own rules or Application Firewall's
// own com.apple/250.ApplicationFirewall anchor.
const darwinAnchor = "com.apple/gravinet"

// tokenRe pulls the reference token out of `pfctl -E`'s output, e.g.
// "Token : 4149728214". The token is opaque and only ever fed back into
// `pfctl -X <token>`, so we don't need to parse anything else about it.
var tokenRe = regexp.MustCompile(`(?i)token\s*:\s*(\d+)`)

// New enables pf via the reference-counted -E (so we never disable it out
// from under Application Firewall, Internet Sharing, or anything else that
// separately enabled it — see pfctl(8)'s -E/-X, which exist only on macOS's
// pfctl), and returns a Manager whose Clear releases exactly that reference.
func New() (*Manager, error) {
	if _, err := exec.LookPath("pfctl"); err != nil {
		return nil, fmt.Errorf("pfctl not found on PATH")
	}
	out, err := exec.Command("pfctl", "-E").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("pfctl -E: %w: %s", err, strings.TrimSpace(string(out)))
	}
	token := ""
	if m := tokenRe.FindSubmatch(out); m != nil {
		token = string(m[1])
	}
	release := func() {
		if token != "" {
			_ = exec.Command("pfctl", "-X", token).Run()
		}
		// If we couldn't parse a token, leave pf enabled rather than risk an
		// unqualified `pfctl -d` disabling it out from under something else
		// that also depends on it — best-effort, matching this codebase's
		// other platform-tuning fallbacks (see internal/ipfwd).
	}
	return &Manager{anchor: darwinAnchor, release: release}, nil
}
