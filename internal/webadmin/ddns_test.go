package webadmin

import (
	"strings"
	"testing"
)

// The settings panel reports; it does not act.
//
// A "register now" button shipped there in v992 and came out in v996. The
// distinction it violated is worth a guard rather than a memory: everything
// else on Settings is a preference that takes effect when something else next
// runs, and a control that reaches out to another organisation's DNS server on
// click is a different kind of thing in the same visual row as a dark-mode
// switch.
func TestSettingsHasNoRegisterAction(t *testing.T) {
	sec := between(t, indexHTML, "function secSettingsGeneral(c)", "\nfunction secSettingsSecurity")
	for _, gone := range []string{"register now", "op:'register'", `op: 'register'`} {
		if strings.Contains(sec, gone) {
			t.Errorf("the settings panel carries %q again; triggering a registration is not a preference", gone)
		}
	}
	// And the report the button sat next to went the same way in v1000, for
	// the same reason carried one step further: a settings page says what this
	// node is configured to do, not what it did. The outcome of every run is
	// in the log.
	for _, gone := range []string{"b.last", "Last run ", "ddnsLast"} {
		if strings.Contains(sec, gone) {
			t.Errorf("the settings panel reports run outcomes again (%q); that belongs in the log", gone)
		}
	}
}

// The log is now the only place a run's outcome is written, so every outcome
// has to reach it — including the failures that were previously visible on the
// page and would otherwise have nowhere left to go.
func TestEveryRunOutcomeIsLogged(t *testing.T) {
	loop := between(t, mustRead("ddns.go"), "res, err := RunDDNSOnce(cfg, skip)", "\n\t\t\t}")
	for _, want := range []string{
		"case err != nil:",          // could not run at all
		"case len(res.Errors) > 0:", // ran, something failed — the PTR case
		"case res.Updated > 0:",     // ran, something changed
		"default:",                  // ran, nothing to do
	} {
		if !strings.Contains(loop, want) {
			t.Errorf("a run outcome has no log branch: %s", want)
		}
	}
	// Failures must carry the server's own reason, which is the whole value of
	// the line — "ddns failed" sends somebody to a packet capture.
	if !strings.Contains(loop, "strings.Join(res.Errors,") {
		t.Error("the failure log line does not include what actually failed")
	}
}

// And the endpoint no longer takes the op, so a stale page cannot still fire
// one at a server that has moved on.
func TestDDNSEndpointHasNoRegisterOp(t *testing.T) {
	src := mustRead("ddns.go")
	if strings.Contains(src, `req.Op == "register"`) {
		t.Error("/api/ddns still accepts an on-demand register")
	}
	if !strings.Contains(src, "RunDDNSOnce") {
		t.Error("RunDDNSOnce is gone; the timer loop needs it")
	}
}
