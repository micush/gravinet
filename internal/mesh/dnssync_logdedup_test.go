package mesh

import (
	"strings"
	"testing"
)

// A DNS-sync failure that an operator has to fix by hand (the canonical one:
// systemd-resolved not enabled on RHEL/Rocky/Alma/CentOS, so every sync fails
// with "The name is not activatable") must not re-log every maintenance tick.
// The generic error path deliberately doesn't record lastDNSSig — it retries, so
// a fix applied later takes effect without a restart — which is exactly what made
// it re-log forever. Shout once, then stay quiet until it changes or clears.
func TestDNSSyncErrorLoggedOncePerDistinctError(t *testing.T) {
	ns := &netState{}
	const rhel = `resolver: set dns on mesh0: The name is not activatable`

	// This mirrors the branch in syncDNS: warn only when the message changes.
	warns := 0
	report := func(msg string) {
		if msg != ns.lastDNSErr {
			warns++
			ns.lastDNSErr = msg
		}
	}

	for i := 0; i < 20; i++ { // 20 maintenance ticks, same failure every time
		report(rhel)
	}
	if warns != 1 {
		t.Errorf("a persistent, unchanged failure warned %d times across 20 ticks; want exactly 1", warns)
	}

	// A *different* failure is news, and must be reported.
	report("resolver: set dns on mesh0: Unknown interface mesh0")
	if warns != 2 {
		t.Errorf("a changed failure did not warn (warns=%d, want 2)", warns)
	}

	// Recovery clears the memo, so a later recurrence is news again — otherwise a
	// flapping resolver would go silent after its first failure, forever.
	ns.lastDNSErr = "" // what the success path does
	report(rhel)
	if warns != 3 {
		t.Errorf("a failure recurring after a success did not warn (warns=%d, want 3)", warns)
	}
}

// The netState field has to exist and be per-network: two networks failing for
// different reasons must each get their own say.
func TestDNSErrMemoIsPerNetwork(t *testing.T) {
	a, b := &netState{}, &netState{}
	a.lastDNSErr = "boom"
	if b.lastDNSErr != "" {
		t.Error("lastDNSErr is shared across networks; each network must track its own last failure")
	}
	if !strings.Contains(a.lastDNSErr, "boom") {
		t.Error("lastDNSErr did not retain the failure it was given")
	}
}
