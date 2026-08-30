package tui

import (
	"errors"
	"testing"

	"gravinet/internal/config"
)

// fakeGravinet installs a stand-in for runGravinet for the duration of one
// test, recording every call it received and returning canned answers in
// order. t.Cleanup restores the real one, so a forgotten restore can never
// leak a fake into a later test.
type fakeGravinet struct {
	calls [][]string
	// answers is consumed one per call, in order; once exhausted, calls
	// return an empty success so a test that doesn't care about the exact
	// number of calls doesn't have to pad this out.
	answers []fakeAnswer
}

type fakeAnswer struct {
	output string
	code   int
	err    error
}

func installFakeGravinet(t *testing.T) *fakeGravinet {
	t.Helper()
	f := &fakeGravinet{}
	old := runGravinet
	runGravinet = func(args ...string) (string, int, error) {
		f.calls = append(f.calls, append([]string{}, args...))
		if len(f.answers) == 0 {
			return "ok", 0, nil
		}
		a := f.answers[0]
		f.answers = f.answers[1:]
		return a.output, a.code, a.err
	}
	t.Cleanup(func() { runGravinet = old })
	return f
}

func TestRunLeafSuccess(t *testing.T) {
	f := installFakeGravinet(t)
	f.answers = []fakeAnswer{{output: "added network corp\n", code: 0}}
	res := runLeaf("network", "add", "corp")
	if !res.ok {
		t.Fatalf("expected ok, got %+v", res)
	}
	if res.detail != "added network corp" {
		t.Errorf("detail = %q", res.detail)
	}
	if len(f.calls) != 1 || f.calls[0][0] != "network" {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestRunLeafNonZeroExitIsAFailureNotAGoError(t *testing.T) {
	// This is the ordinary "the CLI itself said no" case — a bad flag, a
	// name that doesn't exist — and must come back as ok:false with the
	// CLI's own message, not as a mutationResult that looks like this
	// package couldn't even run the command.
	f := installFakeGravinet(t)
	f.answers = []fakeAnswer{{output: "no network named \"nope\"\n", code: 1}}
	res := runLeaf("network", "delete", "nope")
	if res.ok {
		t.Fatal("a nonzero exit must not be reported as success")
	}
	if res.detail != "no network named \"nope\"" {
		t.Errorf("detail = %q", res.detail)
	}
}

func TestRunLeafNonZeroExitWithNoOutputStillExplainsItself(t *testing.T) {
	f := installFakeGravinet(t)
	f.answers = []fakeAnswer{{output: "", code: 2}}
	res := runLeaf("network", "add", "x")
	if res.ok {
		t.Fatal("expected failure")
	}
	if res.detail == "" {
		t.Error("a failure with no CLI output must still say something, not show a blank result screen")
	}
}

func TestRunLeafCouldNotStartTheProcess(t *testing.T) {
	f := installFakeGravinet(t)
	f.answers = []fakeAnswer{{output: "", code: -1, err: errors.New("could not locate the running gravinet binary: stat: no such file")}}
	res := runLeaf("network", "add", "x")
	if res.ok {
		t.Fatal("expected failure")
	}
	if res.detail == "" {
		t.Error("the error should be visible in the result, not swallowed")
	}
}

func TestCliArgsAppendsOnlyConfigPath(t *testing.T) {
	// cliArgs is for config-file-editing leaves, which parse arguments with
	// extractOpt/openCfg — manual scanning that recognizes -config and
	// nothing else. It must never append -sock: these leaves resolve the
	// control socket to reload from cfg.ControlSocket (the value already in
	// the file they just loaded), never from a command-line flag, and on a
	// leaf using a real flag.FlagSet an unrecognized -sock is an immediate
	// parse error.
	m := &model{cfgPath: "/etc/gravinet/config.json", sockPath: "/run/gravinet/control.sock"}
	got := m.cliArgs("network", "add", "corp")
	want := []string{"network", "add", "corp", "-config", "/etc/gravinet/config.json"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCliArgsSockAppendsOnlySockPath(t *testing.T) {
	// cliArgsSock is for control-socket-only leaves (ban, unban, fw, upgrade)
	// — confirmed per leaf by reading its own flag.NewFlagSet call, every
	// one of which registers -sock and never -config, because none of them
	// call config.Load. Appending -config there is the same class of bug in
	// the other direction.
	m := &model{cfgPath: "/etc/gravinet/config.json", sockPath: "/run/gravinet/control.sock"}
	got := m.cliArgsSock("ban", "deadbeef")
	want := []string{"ban", "deadbeef", "-sock", "/run/gravinet/control.sock"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCliArgsBareAppendsNeither(t *testing.T) {
	// cliArgsBare is for leaves that touch neither a config file nor the
	// control socket (System > Resolver/Time/Syslog/Users/Power) — each
	// registers its own tiny flag.FlagSet with only the fields that one
	// operation needs, so either flag would be an immediate parse error.
	m := &model{cfgPath: "/etc/gravinet/config.json", sockPath: "/run/gravinet/control.sock"}
	got := m.cliArgsBare("system", "resolver", "hostname", "gn1")
	want := []string{"system", "resolver", "hostname", "gn1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCliArgsOmitsPathsWhenUnset(t *testing.T) {
	// A test model with no paths configured must not append empty -config/
	// -sock flags — that would make the subprocess try to open a config at
	// the empty path instead of falling back to its own default.
	m := &model{}
	got := m.cliArgs("version")
	if len(got) != 1 || got[0] != "version" {
		t.Errorf("got %v, want just [version]", got)
	}
}

func TestCliArgsDoesNotMutateItsInput(t *testing.T) {
	// A subtle append() footgun: if cliArgs shared backing storage with a
	// slice the caller reuses, a second call could corrupt the first's
	// arguments. Every call site here builds a fresh []string literal, but
	// this pins the guarantee so that stays true if one doesn't.
	m := &model{cfgPath: "/c"}
	base := []string{"network", "add"}
	got1 := m.cliArgs(base...)
	got2 := m.cliArgs(append(base, "other")...)
	if got1[len(got1)-3] != "add" {
		t.Errorf("first call's args were altered by the second: %v", got1)
	}
	_ = got2
}

// ---- commitConfig ---------------------------------------------------------

func TestCommitConfigRefusesAnInvalidResult(t *testing.T) {
	// A network with no ID is invalid; Validate() should catch it before
	// anything is written, the same guarantee cmd/gravinet's commitCfg
	// gives — a rejected value must never touch disk.
	cfg := &config.Config{Networks: []config.Network{{Name: "x"}}}
	res := commitConfig(cfg, "/nonexistent/dir/config.json", "")
	if res.ok {
		t.Fatal("an invalid config must not be reported as committed")
	}
}

func TestCommitConfigSavesAndReportsWhenNoDaemonIsReachable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	cfg := config.Default()
	cfg.NodeID = "abc"
	res := commitConfig(cfg, path, "/nonexistent.sock")
	if !res.ok {
		t.Fatalf("save itself should succeed even with no daemon: %+v", res)
	}
	if res.detail == "" {
		t.Error("should say something about the daemon not being reachable")
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("the file was not actually written: %v", err)
	}
	if loaded.NodeID != "abc" {
		t.Errorf("loaded config doesn't match what was saved: %+v", loaded)
	}
}
