//go:build openbsd

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

// withFakeControlBin puts a fake unbound-control — a shell script with body
// script — at the front of PATH, so Sync/liveForwards exercise the real
// exec.CommandContext path (including controlTimeout) against a predictable
// stand-in instead of a real unbound install.
func withFakeControlBin(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "unbound-control")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestSyncProceedsWhenReachable proves a reachable unbound-control makes Sync
// exec it and succeed (no config-file gate on OpenBSD — see the backend doc).
func TestSyncProceedsWhenReachable(t *testing.T) {
	withTempStateDir(t)
	withFakeControlBin(t, `
case "$1" in
  list_forwards) exit 0 ;;
  forward_add) exit 0 ;;
  forward_remove) exit 0 ;;
esac`)
	err := Sync("deadbeef", "", []Entry{{Domain: "example.com", Servers: addrsFor(t, "1.1.1.1")}}, nil)
	if err != nil {
		t.Fatalf("Sync against a reachable unbound-control should succeed, got: %v", err)
	}
}

// TestSyncSurfacesEnableHintWhenUnreachable locks in the OpenBSD-specific
// behavior: a real forwarding request against an unreachable/remote-control-off
// unbound must return the actionable install-openbsd.sh --unbound hint rather
// than unbound-control's raw stderr.
func TestSyncSurfacesEnableHintWhenUnreachable(t *testing.T) {
	withTempStateDir(t)
	withFakeControlBin(t, "echo connection refused >&2; exit 1")
	err := Sync("deadbeef", "", []Entry{{Domain: "example.com", Servers: addrsFor(t, "1.1.1.1")}}, nil)
	if err == nil {
		t.Fatal("Sync with entries against an unreachable unbound should error, got nil")
	}
	if !strings.Contains(err.Error(), "install-openbsd.sh --unbound") {
		t.Fatalf("error = %v, want the actionable enable-unbound hint", err)
	}
}

// TestSyncToleratesUnreachableDaemonWhenClearing mirrors the FreeBSD/Linux
// tolerance: Clear (Sync with no entries) against an unreachable unbound must
// succeed, since there's nothing live to clear — the path clearStaleDNSForwards
// takes on every startup/shutdown, which must never itself become the hang.
func TestSyncToleratesUnreachableDaemonWhenClearing(t *testing.T) {
	withTempStateDir(t)
	withFakeControlBin(t, "exit 1")
	if err := Sync("deadbeef", "", nil, nil); err != nil {
		t.Fatalf("Sync with no entries should tolerate an unreachable daemon, got: %v", err)
	}
}

// TestSyncSearchDomainsUnsupportedButRoutingStillApplies checks the documented
// contract: unbound has no per-interface search-domain mechanism, so a
// non-empty searchDomains must produce a distinct error wrapping
// ErrSearchDomainsUnsupported — while routing domains still apply (proven by
// getting that specific error rather than a routing failure).
func TestSyncSearchDomainsUnsupportedButRoutingStillApplies(t *testing.T) {
	withTempStateDir(t)
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
		t.Fatalf("error should wrap ErrSearchDomainsUnsupported, got: %v", err)
	}
	if !strings.Contains(err.Error(), "search domain") {
		t.Fatalf("error should mention search domains (and thus that routing succeeded), got: %v", err)
	}
	if err := Sync("deadbeef", "", nil, nil); err != nil {
		t.Fatalf("Sync with nothing to apply should not error: %v", err)
	}
}

// TestRunTimesOutInsteadOfHangingForever locks in the bounded control call: a
// wedged socket must not block past controlTimeout, since Sync/Clear run from
// gravinet's shutdown path (see controlTimeout's doc).
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
