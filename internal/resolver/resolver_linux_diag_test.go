//go:build linux

package resolver

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The exact error a RHEL/Rocky/Alma/CentOS host produces, where resolvectl exists
// (it ships in the base systemd package) but systemd-resolved isn't running.
const rhelErr = `resolvectl dns mesh0 192.168.168.168 192.168.168.169: exit status 1: ` +
	`Failed to set DNS configuration: The name is not activatable`

func TestResolvedDownRecognisesTheDBusFailure(t *testing.T) {
	down := []string{
		rhelErr,
		"Failed to activate service 'org.freedesktop.resolve1': timed out",
		"Could not activate remote peer",
	}
	for _, s := range down {
		if !resolvedDown(errors.New(s)) {
			t.Errorf("resolvedDown(%q) = false, want true — this is systemd-resolved not running", s)
		}
	}

	// A genuine per-link failure must NOT be mistaken for a missing service, or
	// the real error gets buried under advice about a service that's working fine.
	notDown := []string{
		"resolvectl dns mesh0 10.0.0.1: exit status 1: Unknown interface mesh0",
		"resolvectl domain mesh0 ~corp.example: exit status 1: Invalid domain name",
		"exit status 1: Access denied",
	}
	for _, s := range notDown {
		if resolvedDown(errors.New(s)) {
			t.Errorf("resolvedDown(%q) = true, want false — that's a real error, not a missing service", s)
		}
	}
	if resolvedDown(nil) {
		t.Error("resolvedDown(nil) = true, want false")
	}
}

// The point of the wrapper: the raw D-Bus string names neither the cause nor the
// cure. The wrapped error must name systemd-resolved, say RHEL doesn't enable it,
// and give the commands — while still preserving the original error.
func TestExplainResolvedNamesCauseAndCure(t *testing.T) {
	orig := fmt.Errorf("resolver: set dns on mesh0: %w", errors.New(rhelErr))
	got := explainResolved(orig)

	if !errors.Is(got, orig) {
		t.Error("explainResolved must wrap the original error, not replace it")
	}
	msg := got.Error()
	for _, want := range []string{
		"systemd-resolved is not running",         // the cause, in words
		"org.freedesktop.resolve1",                // what "the name" actually was
		"RHEL, Rocky, Alma and CentOS",            // why it's these distros
		"systemctl enable --now systemd-resolved", // the cure
		"stub-resolv.conf",                        // ...and the part people forget
		"dns=systemd-resolved",                    // ...and the NetworkManager part
		"turn off DNS forwarding",                 // the opt-out, for hosts that shouldn't change
		"The name is not activatable",             // the original text is still there
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("explained error missing %q:\n%s", want, msg)
		}
	}
}

// A real error passes through untouched.
func TestExplainResolvedLeavesRealErrorsAlone(t *testing.T) {
	orig := errors.New("resolver: set dns on mesh0: Unknown interface mesh0")
	if got := explainResolved(orig); got.Error() != orig.Error() {
		t.Errorf("explainResolved rewrote a genuine per-link error:\n%s", got)
	}
}
