package webadmin

import (
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

// TestBoundedBGPImportTimesOut proves the fix for "stuck on Checking… forever":
// a fn that never returns (simulating a wedged FRR call that somehow evades
// runVtysh's own internal timeout) must still make boundedBGPImport return
// promptly once the outer deadline elapses, with ok=false and a reason that
// says so — never hang the caller.
func TestBoundedBGPImportTimesOut(t *testing.T) {
	blockForever := func(*logx.Logger) (config.BGPConfig, bool, bool, string) {
		select {} // never returns
	}
	start := time.Now()
	bgp, hasPw, ok, reason := boundedBGPImport(50*time.Millisecond, nil, blockForever)
	elapsed := time.Since(start)

	if ok {
		t.Errorf("expected ok=false on timeout, got true (bgp=%+v)", bgp)
	}
	if hasPw {
		t.Error("expected hasPw=false on timeout")
	}
	if reason == "" {
		t.Error("expected a non-empty reason explaining the timeout")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("boundedBGPImport took %s to return after a 50ms deadline — it did not actually bound the wait", elapsed)
	}
}

// TestBoundedBGPImportReturnsPromptly proves the common, working case isn't
// slowed down at all by the new wrapper — a fast fn's result comes straight
// through, well under the deadline.
func TestBoundedBGPImportReturnsPromptly(t *testing.T) {
	want := config.BGPConfig{Enabled: true, ASN: 42485736, RouterID: "10.1.1.1"}
	fast := func(*logx.Logger) (config.BGPConfig, bool, bool, string) {
		return want, false, true, "read from /etc/frr/frr.conf"
	}
	start := time.Now()
	bgp, hasPw, ok, reason := boundedBGPImport(5*time.Second, nil, fast)
	elapsed := time.Since(start)

	if !ok || bgp.ASN != want.ASN || bgp.RouterID != want.RouterID || hasPw {
		t.Errorf("unexpected result: bgp=%+v hasPw=%v ok=%v reason=%q", bgp, hasPw, ok, reason)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("fast path took %s — should return almost immediately", elapsed)
	}
}
