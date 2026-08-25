//go:build linux

package webadmin

import (
	"os/exec"
	"strings"
)

// keaService drives the Kea DHCPv4 unit through systemd, mirroring raService
// and frrService. The timeout wrapper is the same defence: a wedged unit must
// not hold a request open indefinitely.
//
// The unit is named differently across distributions — Debian and Ubuntu ship
// kea-dhcp4-server, Fedora and Arch ship kea-dhcp4 — and unlike the binary,
// which pkgman's two candidate package names resolve, there is no lookup for
// a unit name. So both are tried and the first that works wins. An action that
// succeeds on neither is reported as failure by the caller, which is what
// turns a wrong guess here into a message rather than a silent no-op.
var keaUnits = []string{"kea-dhcp4-server", "kea-dhcp4"}

func keaService(action string) bool {
	for _, unit := range keaUnits {
		if exec.Command("timeout", "45", "systemctl", action, unit).Run() == nil {
			return true
		}
	}
	for _, unit := range keaUnits {
		if exec.Command("systemctl", action, unit).Run() == nil {
			return true
		}
	}
	return false
}

// keaUnit is the unit name actually present on this host, for the one place a
// name has to be shown rather than acted on: the message pointing an operator
// at journalctl. Naming the wrong one there sends somebody to a unit that does
// not exist on their distribution, which is what v944 did on Fedora — it
// hardcoded kea-dhcp4-server, and Fedora ships kea-dhcp4.
//
// Falls back to the first candidate when neither is installed. There is no
// right answer in that case and the message is about to say the server would
// not start anyway.
func keaUnit() string {
	for _, u := range keaUnits {
		if exec.Command("systemctl", "cat", u).Run() == nil {
			return u
		}
	}
	return keaUnits[0]
}

// keaTestConf runs Kea's own parser over a config file without starting the
// server, returning what it said and whether it accepted the file.
//
// This exists because "the service would not start" is not an answer. Kea
// reports precisely what is wrong with a config and precisely where, and
// through v946 all of that went to the journal while the operator got a
// sentence telling them to go and find it. Asking the parser directly puts the
// reason in the reply, on the page, next to the thing that caused it.
//
// A missing binary or an unrunnable one is reported as success: this is a
// better error message, not a gate, and refusing to apply because the test
// could not run would be worse than the situation it is meant to improve.
func keaTestConf(path string) (string, bool) {
	bin, err := exec.LookPath("kea-dhcp4")
	if err != nil {
		for _, p := range []string{"/usr/sbin/kea-dhcp4", "/usr/local/sbin/kea-dhcp4"} {
			if _, serr := exec.Command(p, "-v").CombinedOutput(); serr == nil {
				bin = p
				break
			}
		}
	}
	if bin == "" {
		return "", true
	}
	out, err := exec.Command("timeout", "20", bin, "-t", path).CombinedOutput()
	if err == nil {
		return "", true
	}
	// Kea prints the whole check to stdout; the last non-empty line is the
	// reason, the rest is progress.
	return strings.TrimSpace(lastLine(string(out))), false
}
