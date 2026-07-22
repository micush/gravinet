//go:build freebsd

package resolver

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func withTempStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = old })
	return dir
}

// withFakeControlBin puts a fake local-unbound-control — a shell script with
// body script — at the front of PATH for the test, so Sync/liveForwards
// exercise the real exec.CommandContext path (including controlTimeout)
// against a predictable stand-in instead of a real local-unbound install.
func withFakeControlBin(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "local-unbound-control")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// withUnconfiguredUnbound points unboundConfigPath at a path that doesn't
// exist, simulating a stock FreeBSD host that has local-unbound-control on
// PATH (base system) but has never run local-unbound-setup — e.g. one that
// manages DNS via resolvconf(8) pointing straight at upstream servers.
func withUnconfiguredUnbound(t *testing.T) {
	t.Helper()
	old := unboundConfigPath
	unboundConfigPath = filepath.Join(t.TempDir(), "does-not-exist", "unbound.conf")
	t.Cleanup(func() { unboundConfigPath = old })
}

// withConfiguredUnbound points unboundConfigPath at a file that exists, the
// counterpart to withUnconfiguredUnbound for tests that need the "local-
// unbound has been set up" branch to proceed past the new guard.
func withConfiguredUnbound(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "unbound.conf")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	old := unboundConfigPath
	unboundConfigPath = path
	t.Cleanup(func() { unboundConfigPath = old })
}

// TestSyncNotConfiguredIsFriendlyNotRawStderr locks in the fix for the stock-
// FreeBSD case: local-unbound-control is on PATH but local-unbound was never
// set up (no /var/unbound/unbound.conf), e.g. a host managing DNS via
// resolvconf(8) straight to upstream servers. Sync must return the actionable
// "enable it: sysrc local_unbound_enable=YES ..." message instead of letting
// unbound-control's own "could not open ... unbound.conf" stderr bubble up
// unexplained, and must do so without ever shelling out (the fake binary
// would exit nonzero if called, so a passing test proves the guard short-
// circuited before exec).
func TestSyncNotConfiguredIsFriendlyNotRawStderr(t *testing.T) {
	withTempStateDir(t)
	withUnconfiguredUnbound(t)
	withFakeControlBin(t, "echo should not be invoked >&2; exit 1")
	err := Sync("deadbeef", "", []Entry{{Domain: "example.com", Servers: addrsFor(t, "1.1.1.1")}}, nil)
	if err == nil {
		t.Fatal("Sync against unconfigured local-unbound should error, got nil")
	}
	if !strings.Contains(err.Error(), "sysrc local_unbound_enable=YES") {
		t.Fatalf("error = %v, want the actionable enable-local-unbound hint", err)
	}
	if strings.Contains(err.Error(), "should not be invoked") {
		t.Fatalf("error = %v, want the guard to short-circuit before exec, not surface command stderr", err)
	}
}

// TestSyncToleratesUnconfiguredUnboundWhenClearing mirrors the unreachable-
// daemon tolerance: Clear (Sync with no entries) against local-unbound that
// was never set up must succeed, since there's nothing live to clear either
// way — this is the path every gravinet startup/shutdown takes via
// clearStaleDNSForwards, and it must not fail just because this host has
// never touched local-unbound at all.
func TestSyncToleratesUnconfiguredUnboundWhenClearing(t *testing.T) {
	withTempStateDir(t)
	withUnconfiguredUnbound(t)
	withFakeControlBin(t, "exit 1")
	if err := Sync("deadbeef", "", nil, nil); err != nil {
		t.Fatalf("Sync with no entries should tolerate unconfigured local-unbound, got: %v", err)
	}
}

// TestSyncProceedsWhenUnboundIsConfigured is the counterpart proving the
// guard doesn't false-positive: once unboundConfigPath exists, Sync proceeds
// to actually exec the control binary rather than short-circuiting.
func TestSyncProceedsWhenUnboundIsConfigured(t *testing.T) {
	withTempStateDir(t)
	withConfiguredUnbound(t)
	withFakeControlBin(t, `
case "$1" in
  list_forwards) exit 0 ;;
  forward_add) exit 0 ;;
  forward_remove) exit 0 ;;
esac`)
	err := Sync("deadbeef", "", []Entry{{Domain: "example.com", Servers: addrsFor(t, "1.1.1.1")}}, nil)
	if err != nil {
		t.Fatalf("Sync with local-unbound configured should reach the fake control binary and succeed, got: %v", err)
	}
}

// TestSyncSearchDomainsUnsupportedButRoutingStillApplies checks the contract
// documented on Sync: FreeBSD/local-unbound has no per-interface search-
// domain mechanism, so a non-empty searchDomains argument must produce a
// clear, distinct error — but routing domains (entries) must still be
// applied normally. Since syncRoutingDomains's own error (if any) is
// returned first and would not mention "search domain", getting that
// specific error back is itself proof routing succeeded.
func TestSyncSearchDomainsUnsupportedButRoutingStillApplies(t *testing.T) {
	withTempStateDir(t)
	withConfiguredUnbound(t)
	withFakeControlBin(t, `
case "$1" in
  list_forwards) exit 0 ;;
  forward_add) exit 0 ;;
  forward_remove) exit 0 ;;
esac`)
	err := Sync("deadbeef", "", []Entry{{Domain: "example.com", Servers: addrsFor(t, "1.1.1.1")}}, []string{"corp.internal"})
	if err == nil {
		t.Fatal("expected an error naming search domains as unsupported")
	}
	if !errors.Is(err, ErrSearchDomainsUnsupported) {
		t.Fatalf("error should wrap ErrSearchDomainsUnsupported (callers like dnssync.go rely on errors.Is to avoid retrying a permanent condition forever), got: %v", err)
	}
	if !strings.Contains(err.Error(), "search domain") {
		t.Fatalf("error should mention search domains (and thus that routing succeeded independently), got: %v", err)
	}

	// No search domains and no entries: no error either.
	if err := Sync("deadbeef", "", nil, nil); err != nil {
		t.Fatalf("Sync with nothing to apply should not error: %v", err)
	}
}
// TestRunTimesOutInsteadOfHangingForever locks in the fix for the FreeBSD
// install hang: a wedged control socket (e.g. local-unbound installed but
// never enabled) must not block its caller past controlTimeout, since Sync/
// Clear run from gravinet's own shutdown path — see controlTimeout's doc
// comment for the full chain from there to `service gravinet stop` hanging.
func TestRunTimesOutInsteadOfHangingForever(t *testing.T) {
	withFakeControlBin(t, "sleep 30")
	start := time.Now()
	_, err := run("list_forwards")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error from a wedged control socket, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout error", err)
	}
	if elapsed > controlTimeout+2*time.Second {
		t.Fatalf("run took %s, want bounded near controlTimeout (%s)", elapsed, controlTimeout)
	}
}

// TestSyncToleratesUnreachableDaemonWhenClearing mirrors resolver_linux.go's
// Clear tolerating a missing resolvectl: Clear (Sync with no entries) against
// an unreachable local-unbound must succeed, since there's nothing live to
// clear anyway — this is exactly the path clearStaleDNSForwards takes on
// every startup and shutdown, so it must never itself become the hang.
func TestSyncToleratesUnreachableDaemonWhenClearing(t *testing.T) {
	withTempStateDir(t)
	withConfiguredUnbound(t) // isolate from the not-configured guard; this test is about the daemon being unreachable specifically
	withFakeControlBin(t, "exit 1") // binary present, daemon unreachable
	if err := Sync("deadbeef", "", nil, nil); err != nil {
		t.Fatalf("Sync with no entries should tolerate an unreachable daemon, got: %v", err)
	}
}

// TestSyncSurfacesErrorWhenDaemonUnreachableAndEntriesRequested checks the
// other half of that tolerance: a real Sync request (entries to apply)
// against an unreachable daemon is a genuine failure and must be reported,
// not silently swallowed the way an empty Clear-style Sync is.
func TestSyncSurfacesErrorWhenDaemonUnreachableAndEntriesRequested(t *testing.T) {
	withTempStateDir(t)
	withConfiguredUnbound(t) // isolate from the not-configured guard; this test is about the daemon being unreachable specifically
	withFakeControlBin(t, "exit 1")
	err := Sync("deadbeef", "", []Entry{{Domain: "example.com", Servers: addrsFor(t, "1.1.1.1")}}, nil)
	if err == nil {
		t.Fatal("Sync with entries against an unreachable daemon should return an error, not silently succeed")
	}
}

// TestStateRoundTrip locks in that saveState/loadState survive a "restart" —
// loadState is called fresh, with nothing cached in memory, exactly like a
// gravinet daemon restart would see it (see the backend's package doc on why
// a state file satisfies restart-safety even without an embedded marker).
func TestStateRoundTrip(t *testing.T) {
	withTempStateDir(t)
	tag := "deadbeef"

	want := map[string]struct{}{"domain1": {}, "domain2": {}}
	if err := saveState(tag, want); err != nil {
		t.Fatalf("saveState: %v", err)
	}

	got, err := loadState(tag)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("loadState = %v, want %v", got, want)
	}
}

// TestLoadStateMissingFileIsEmptyNotError covers both a tag that's never been
// synced and a state file lost some other way: either way Sync should start
// clean rather than fail.
func TestLoadStateMissingFileIsEmptyNotError(t *testing.T) {
	withTempStateDir(t)
	got, err := loadState("never-synced")
	if err != nil {
		t.Fatalf("loadState on missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("loadState on missing file = %v, want empty", got)
	}
}

// TestLoadStateCorruptFileIsEmptyNotError mirrors the above for a state file
// that exists but isn't valid JSON, so a partial write (e.g. from a crash
// mid-save, before the atomic rename lands) can't wedge every future Sync.
func TestLoadStateCorruptFileIsEmptyNotError(t *testing.T) {
	dir := withTempStateDir(t)
	tag := "deadbeef"
	if err := os.WriteFile(filepath.Join(dir, tag+".json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := loadState(tag)
	if err != nil {
		t.Fatalf("loadState on corrupt file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("loadState on corrupt file = %v, want empty", got)
	}
}

func TestStateRoundTripEmptySet(t *testing.T) {
	withTempStateDir(t)
	tag := "deadbeef"
	if err := saveState(tag, map[string]struct{}{}); err != nil {
		t.Fatalf("saveState: %v", err)
	}
	got, err := loadState(tag)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("loadState = %v, want empty", got)
	}
}

// TestParseListForwardsSingleZone matches the documented list_forwards output
// shape for one forward zone: "<zone>. IN forward <addr> [addr ...]".
func TestParseListForwardsSingleZone(t *testing.T) {
	out := "example.com. IN forward 1.1.1.1\n"
	got := parseListForwards(out)
	want := map[string][]string{"example.com": {"1.1.1.1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListForwards = %v, want %v", got, want)
	}
}

func TestParseListForwardsMultipleZonesAndServers(t *testing.T) {
	out := "domain1. IN forward 1.1.1.1 2.2.2.2\ndomain2. IN forward 3.3.3.3\n"
	got := parseListForwards(out)
	want := map[string][]string{
		"domain1": {"1.1.1.1", "2.2.2.2"},
		"domain2": {"3.3.3.3"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListForwards = %v, want %v", got, want)
	}
}

// TestParseListForwardsSkipsUnrecognizedLines covers blank output (no
// forwards configured) and any line that isn't a "<zone> IN forward ..."
// entry, so an unexpected line degrades gracefully instead of erroring.
func TestParseListForwardsSkipsUnrecognizedLines(t *testing.T) {
	out := "\nsomething unexpected\ndomain1. IN forward 1.1.1.1\n"
	got := parseListForwards(out)
	want := map[string][]string{"domain1": {"1.1.1.1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListForwards = %v, want %v", got, want)
	}
}

func TestParseListForwardsEmpty(t *testing.T) {
	got := parseListForwards("")
	if len(got) != 0 {
		t.Fatalf("parseListForwards(\"\") = %v, want empty", got)
	}
}
