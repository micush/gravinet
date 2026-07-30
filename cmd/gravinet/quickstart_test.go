package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gravinet/internal/config"
)

// TestParseQuickstartArgs locks in the invocation-shape validation that
// cmdQuickstart relies on to never index rest[0]/rest[1] unsafely. An
// earlier version indexed unconditionally: "gravinet quickstart -config
// PATH" (no NAME/TOKEN) reduces rest to empty once -config/-no-service are
// stripped, and rest[0] panicked with an index-out-of-range. Testing the
// pure function directly, rather than cmdQuickstart itself, means this
// never touches fatal()'s os.Exit.
func TestParseQuickstartArgs(t *testing.T) {
	cases := []struct {
		name        string
		rest        []string
		wantJoining bool
		wantValue   string
		wantErr     bool
	}{
		{"empty (the original panic case)", nil, false, "", true},
		{"create form", []string{"corp"}, false, "corp", false},
		{"create form with extra keyword args", []string{"corp", "subnet", "10.50.0.0/16"}, false, "corp", false},
		{"join form", []string{"join", "grav1.abc"}, true, "grav1.abc", false},
		{"join with no token", []string{"join"}, false, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			joining, value, err := parseQuickstartArgs(c.rest)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if err != nil {
				return // value/joining are unspecified on error
			}
			if joining != c.wantJoining {
				t.Errorf("joining = %v, want %v", joining, c.wantJoining)
			}
			if value != c.wantValue {
				t.Errorf("value = %q, want %q", value, c.wantValue)
			}
		})
	}
}

// TestQuickstartCreateThenJoin exercises the two real flows end to end, in
// process: create a fresh mesh from nothing (no pre-existing config, no
// pre-existing directory), then feed the token it prints into the join form
// on a second, independent config. Both runs pass -no-service so this never
// touches the host's real service manager. Neither input is malformed, so
// this never reaches a fatal()/os.Exit path — safe to call cmdQuickstart
// directly and just capture stdout for the printed token.
func TestQuickstartCreateThenJoin(t *testing.T) {
	base := t.TempDir()
	path1 := filepath.Join(base, "node1", "config.json")

	createOut := captureStdout(t, func() {
		cmdQuickstart([]string{
			"corp", "subnet", "10.90.0.0/16", "addr", "198.51.100.7",
			"-config", path1, "-no-service",
		})
	})
	cfg1, err := config.Load(path1)
	if err != nil {
		t.Fatalf("create: config did not load: %v", err)
	}
	if len(cfg1.Networks) != 1 || cfg1.Networks[0].Name != "corp" || cfg1.Networks[0].Subnet4 != "10.90.0.0/16" {
		t.Fatalf("create: unexpected network state: %+v", cfg1.Networks)
	}
	token := regexp.MustCompile(`grav1\.[A-Za-z0-9_-]+`).FindString(createOut)
	if token == "" {
		t.Fatalf("create: no join token found in output:\n%s", createOut)
	}

	path2 := filepath.Join(base, "node2", "config.json")
	cmdQuickstart([]string{"join", token, "-config", path2, "-no-service"})
	cfg2, err := config.Load(path2)
	if err != nil {
		t.Fatalf("join: config did not load: %v", err)
	}
	if len(cfg2.Networks) != 1 {
		t.Fatalf("join: expected exactly 1 network, got %+v", cfg2.Networks)
	}

	n1, n2 := cfg1.Networks[0], cfg2.Networks[0]
	if n1.ID != n2.ID {
		t.Errorf("network id mismatch: node1=%s node2=%s", n1.ID, n2.ID)
	}
	if n1.Keys[0].Key != n2.Keys[0].Key {
		t.Errorf("key0 mismatch — join did not inherit the network key from the token")
	}
	if n2.Subnet4 != "10.90.0.0/16" {
		t.Errorf("node2 subnet4 = %q, want the subnet learned from the token (10.90.0.0/16)", n2.Subnet4)
	}
	if len(n2.Seeds) != 1 || n2.Seeds[0].Address != "198.51.100.7" {
		t.Errorf("node2 seeds = %+v, want the addr embedded in the token (198.51.100.7)", n2.Seeds)
	}
	// A per-node identity, not part of the network the token describes — the
	// two nodes must NOT end up sharing one.
	if cfg1.NodeID == cfg2.NodeID {
		t.Errorf("node1 and node2 ended up with the same node_id %q — each quickstart run must mint its own", cfg1.NodeID)
	}
}

// TestQuickstartDuplicateNetworkNameFails confirms cmdQuickstart surfaces
// Config.NetworkAdd's own "already exists" error (via fatal(), exit 1)
// rather than the macro doing anything surprising with it. Runs as a
// subprocess (see runQuickstartSubprocess) since the second call is
// expected to hit fatal()/os.Exit.
func TestQuickstartDuplicateNetworkNameFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if code, out := runQuickstartSubprocess(t, "corp", "-config", path, "-no-service"); code != 0 {
		t.Fatalf("first create: exit code = %d, output:\n%s", code, out)
	}
	code, out := runQuickstartSubprocess(t, "corp", "-config", path, "-no-service")
	if code != 1 {
		t.Fatalf("duplicate create: exit code = %d, want 1, output:\n%s", code, out)
	}
	if !strings.Contains(out, "already exists") {
		t.Errorf("duplicate create: expected an \"already exists\" message, got:\n%s", out)
	}
}

// TestQuickstartFlagsOnlyLeavesNoTrace is the subprocess-level regression
// test for the original bug: "gravinet quickstart -config PATH" (no
// NAME/TOKEN) must fail cleanly with exit 1 — not panic — and must not
// create the config file or even its parent directory, since
// parseQuickstartArgs is checked before any filesystem write.
func TestQuickstartFlagsOnlyLeavesNoTrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.json") // parent must not get created either
	code, out := runQuickstartSubprocess(t, "-config", path)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (fatal), output:\n%s", code, out)
	}
	if strings.Contains(out, "panic") {
		t.Fatalf("output contains a panic trace — the original bug regressed:\n%s", out)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("parent directory %s exists after a validation failure — expected no filesystem side effects, got stat err=%v", filepath.Dir(path), err)
	}
}

// TestHelperProcess isn't a real test — it's the subprocess entry point
// runQuickstartSubprocess re-execs the already-built test binary as, so a
// call into cmdQuickstart that hits fatal()/os.Exit(1) (or, if a bug
// regressed, panics) can be observed as a normal exit code/output from the
// parent test instead of killing it. Standard library tests use this same
// pattern for exec.Command-adjacent code (see os/exec's own tests) — gated
// on GRAVINET_QUICKSTART_TEST so it's an inert no-op under a plain `go test`.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GRAVINET_QUICKSTART_TEST") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	cmdQuickstart(args)
	os.Exit(0) // cmdQuickstart returned normally: a success path.
}

// runQuickstartSubprocess re-execs this test binary with TestHelperProcess
// selected, passing args through to cmdQuickstart in that subprocess. See
// TestHelperProcess's doc comment for why.
func runQuickstartSubprocess(t *testing.T, args ...string) (exitCode int, output string) {
	t.Helper()
	cs := append([]string{"-test.run=TestHelperProcess", "--"}, args...)
	cmd := exec.Command(os.Args[0], cs...)
	cmd.Env = append(os.Environ(), "GRAVINET_QUICKSTART_TEST=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), string(out)
	}
	t.Fatalf("subprocess failed to run at all: %v\noutput:\n%s", err, out)
	return -1, string(out)
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it, restoring the original afterward. cmdQuickstart
// writes its progress with fmt.Println/Printf directly to os.Stdout, so this
// is what lets a test assert on that output (e.g. extracting a printed join
// token) without cmdQuickstart itself needing an io.Writer parameter.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, err := r.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if err != nil {
				break
			}
		}
		done <- string(buf)
	}()
	fn()
	os.Stdout = orig
	w.Close()
	return <-done
}
