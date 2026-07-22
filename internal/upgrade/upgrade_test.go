package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeBinary writes an executable stand-in for a gravinet build: a script whose
// `version` output is the real, parseable line main.go prints. Everything the
// preflight does to a candidate — exec it, read its version, hand it a config —
// works on this, which is the point: the checks are about what a binary *does*
// when run, so a test binary that runs is a faithful test binary.
func fakeBinary(t *testing.T, dir, name, version string, pam bool, selftestOK bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shebang scripts are not executable on Windows; the swap logic is exercised on Unix")
	}
	pamS := "no"
	if pam {
		pamS = "yes"
	}
	path := filepath.Join(dir, name)
	fail := ""
	if !selftestOK {
		fail = `if [ "$1" = "selftest" ]; then echo "config: unknown field \"managed\"" >&2; exit 1; fi`
	}
	script := fmt.Sprintf("#!/bin/sh\n%s\necho \"gravinet %s (testbuild) %s/%s pam=%s\"\nexit 0\n",
		fail, version, runtime.GOOS, runtime.GOARCH, pamS)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func probeVersion(t *testing.T, path string) string {
	t.Helper()
	p, err := ProbeBinary(context.Background(), path)
	if err != nil {
		t.Fatalf("probing %s: %v", path, err)
	}
	return p.Version
}

func TestVersionLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"398", "399", true},
		{"399", "398", false},
		{"399", "399", false},
		{"99", "100", true}, // numeric, not lexical
		// Non-numeric versions fall back to lexical order: stable and total,
		// but explicitly not semver-aware. Asserting the lexical result here
		// rather than a semver one keeps the test honest about the contract.
		{"1.10.0", "1.2.0", true},
		{"", "399", true},
	}
	for _, c := range cases {
		if got := VersionLess(c.a, c.b); got != c.want {
			t.Errorf("VersionLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// --- preflight ---

// These are the failures that actually brick nodes. Every one of them is
// invisible to a content digest — the digest of the *wrong* binary is perfectly
// valid — which is why the candidate is executed rather than merely hashed.
func TestPreflightCatchesTheThingsThatBrickNodes(t *testing.T) {
	ctx := context.Background()

	t.Run("binary does not run", func(t *testing.T) {
		junk := filepath.Join(t.TempDir(), "truncated")
		os.WriteFile(junk, []byte("\x7fELF not really"), 0o755)
		if _, err := Preflight(ctx, junk, Options{}); err == nil {
			t.Fatal("a binary that cannot execute passed preflight — this is the truncated-build case")
		}
	})

	t.Run("binary prints nothing parseable", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "quiet")
		os.WriteFile(path, []byte("#!/bin/sh\necho hello\nexit 0\n"), 0o755)
		if _, err := Preflight(ctx, path, Options{}); err == nil ||
			!strings.Contains(err.Error(), "unrecognized version line") {
			t.Fatalf("a binary that is not gravinet was accepted: %v", err)
		}
	})

	t.Run("silent PAM downgrade", func(t *testing.T) {
		binNoPAM := fakeBinary(t, t.TempDir(), "gravinet-nopam", "399", false, true)
		_, err := Preflight(ctx, binNoPAM, Options{RunningPAM: true})
		if err == nil || !strings.Contains(err.Error(), "PAM") {
			t.Fatalf("replacing a PAM build with a non-PAM one was allowed silently: %v", err)
		}
		if _, err := Preflight(ctx, binNoPAM, Options{RunningPAM: true, AllowPAMDowngrade: true}); err != nil {
			t.Fatalf("the explicit override should permit it: %v", err)
		}
	})

	t.Run("new binary rejects this node's config", func(t *testing.T) {
		binBad := fakeBinary(t, t.TempDir(), "gravinet-badcfg", "399", true, false)
		cfg := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(cfg, []byte("{}"), 0o600)
		_, err := Preflight(ctx, binBad, Options{ConfigPath: cfg})
		if err == nil || !strings.Contains(err.Error(), "rejected this node's config") {
			t.Fatalf("a binary that refuses this node's config passed preflight: %v", err)
		}
	})

	t.Run("downgrade", func(t *testing.T) {
		oldBin := fakeBinary(t, t.TempDir(), "gravinet-old", "397", true, true)
		if _, err := Preflight(ctx, oldBin, Options{RunningVersion: "398"}); err == nil {
			t.Fatal("a downgrade was accepted without the explicit flag")
		}
		if _, err := Preflight(ctx, oldBin, Options{RunningVersion: "398", AllowDowngrade: true}); err != nil {
			t.Fatalf("an explicitly allowed downgrade was refused: %v", err)
		}
	})
}

// --- apply ---

func TestApplySwapsAndRevertRestores(t *testing.T) {
	candidate := fakeBinary(t, t.TempDir(), "gravinet-built", "399", false, true)
	installDir := t.TempDir()
	target := fakeBinary(t, installDir, "gravinet", "398", false, true)
	before, _ := os.ReadFile(target)

	p, err := Apply(context.Background(), candidate, Options{Target: target, RunningVersion: "398"})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if p.Version != "399" {
		t.Fatalf("probe reported %q", p.Version)
	}
	// The installed path now runs the new binary...
	if got := probeVersion(t, target); got != "399" {
		t.Fatalf("target reports %q after apply, want 399", got)
	}
	// ...the old one is right next to it, byte for byte...
	prev, err := os.ReadFile(BackupPath(target))
	if err != nil {
		t.Fatalf("no backup was kept: %v", err)
	}
	if string(prev) != string(before) {
		t.Fatal("the backup is not the binary that was replaced")
	}
	// ...and no staging file was left lying around next to it.
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Fatal("a .new file survived a successful apply")
	}

	if err := Revert(target); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("target reports %q after revert, want 398", got)
	}
	if _, err := os.Stat(target + ".failed"); err != nil {
		t.Fatal("the backed-out binary should be kept for forensics")
	}
}

// A failed preflight must not touch the installed binary at all.
func TestApplyLeavesTargetAloneWhenPreflightFails(t *testing.T) {
	candidate := fakeBinary(t, t.TempDir(), "gravinet-built", "399", true, false /* rejects config */)
	installDir := t.TempDir()
	target := fakeBinary(t, installDir, "gravinet", "398", true, true)
	cfg := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(cfg, []byte("{}"), 0o600)

	if _, err := Apply(context.Background(), candidate, Options{Target: target, ConfigPath: cfg}); err == nil {
		t.Fatal("apply succeeded despite a failing preflight")
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("the installed binary changed to %q despite a failed preflight", got)
	}
	if _, err := os.Stat(BackupPath(target)); err == nil {
		t.Fatal("a backup was taken even though nothing was replaced")
	}
	if _, err := os.Stat(target + ".new"); err == nil {
		t.Fatal("the staged candidate was left next to the target after a failed preflight")
	}
}

// --- source archives ---

// tgz builds a gzip-compressed tar from a name->content map. Entries whose
// content is nil are directories.
func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func goodTree() map[string]string {
	return map[string]string{
		"gravinet/go.mod":                "module gravinet\n\ngo 1.22\n",
		"gravinet/cmd/gravinet/main.go":  "package main\n\nconst (\n\tversion = \"401\"\n)\n",
		"gravinet/internal/mesh/mesh.go": "package mesh\n",
	}
}

func TestExtractAcceptsATgzAndFindsTheModuleRoot(t *testing.T) {
	dest := t.TempDir()
	root, err := extractSourceArchive(bytes.NewReader(tgz(t, goodTree())), dest)
	if err != nil {
		t.Fatalf("a well-formed source archive was rejected: %v", err)
	}
	if filepath.Base(root) != "gravinet" {
		t.Fatalf("module root resolved to %q, want the directory holding go.mod", root)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "gravinet", "main.go")); err != nil {
		t.Fatalf("the tree was not actually written out: %v", err)
	}
}

// The format is decided by content, not by filename — the upload arrives with
// no name attached at all, and an extension would be a claim rather than a fact.
func TestExtractDetectsZipByContent(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range goodTree() {
		f, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	zw.Close()

	root, err := extractSourceArchive(bytes.NewReader(buf.Bytes()), t.TempDir())
	if err != nil {
		t.Fatalf("a zip source archive was rejected: %v", err)
	}
	if filepath.Base(root) != "gravinet" {
		t.Fatalf("module root resolved to %q", root)
	}
}

func TestExtractRejectsJunk(t *testing.T) {
	if _, err := extractSourceArchive(strings.NewReader("this is not an archive"), t.TempDir()); err == nil {
		t.Fatal("a non-archive upload was accepted")
	}
}

// A tar entry that escapes the extraction directory must be refused outright.
// The hazard does not require a hostile uploader — a build tool's own quirk can
// put "../" in a stream — but the check is the boundary either way.
func TestExtractRefusesPathTraversal(t *testing.T) {
	dest := t.TempDir()
	evil := map[string]string{
		"gravinet/go.mod":               "module gravinet\n",
		"gravinet/cmd/gravinet/main.go": "package main\n",
		"../escaped.txt":                "pwned",
	}
	if _, err := extractSourceArchive(bytes.NewReader(tgz(t, evil)), dest); err == nil {
		t.Fatal("an entry escaping the extraction directory was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escaped.txt")); err == nil {
		t.Fatal("the escaping entry was actually written outside the destination")
	}
}

// A symlink is how a tar entry escapes a path check that only inspects the
// entry's own name, so they are refused rather than resolved.
func TestExtractRefusesSymlinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "gravinet/go.mod", Mode: 0o644, Size: 16, Typeflag: tar.TypeReg})
	tw.Write([]byte("module gravinet\n"))
	tw.WriteHeader(&tar.Header{Name: "gravinet/link", Mode: 0o777, Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()

	if _, err := extractSourceArchive(bytes.NewReader(buf.Bytes()), t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlink in a source upload was accepted: %v", err)
	}
}

// An archive that is a valid tarball but not this project is refused before any
// build is attempted — ten minutes of compiling is an expensive way to discover
// someone uploaded the wrong file.
func TestExtractRefusesSomethingThatIsNotGravinet(t *testing.T) {
	notUs := map[string]string{"other/go.mod": "module other\n", "other/main.go": "package main\n"}
	_, err := extractSourceArchive(bytes.NewReader(tgz(t, notUs)), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "cmd/gravinet") {
		t.Fatalf("a go module that is not gravinet was accepted: %v", err)
	}

	noMod := map[string]string{"stuff/readme.txt": "hello"}
	_, err = extractSourceArchive(bytes.NewReader(tgz(t, noMod)), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("an archive with no go.mod was accepted: %v", err)
	}
}

// SourceVersion answers "what am I about to install?" before committing to a
// build — the same line install-linux.sh's source_version() greps for.
func TestSourceVersionReadsTheTreeWithoutBuilding(t *testing.T) {
	dest := t.TempDir()
	root, err := extractSourceArchive(bytes.NewReader(tgz(t, goodTree())), dest)
	if err != nil {
		t.Fatal(err)
	}
	if got := SourceVersion(root); got != "401" {
		t.Fatalf("SourceVersion = %q, want 401", got)
	}
	if got := SourceVersion(t.TempDir()); got != "" {
		t.Fatalf("SourceVersion on a non-tree = %q, want empty", got)
	}
}

// --- guard ---

// The crash-loop case: the new binary starts, but never reaches health, over and
// over. Nobody is coming to help — a manager cannot reach a node that is not on
// the mesh — so the node has to back itself out.
func TestGuardRevertsAfterBootLoop(t *testing.T) {
	candidate := fakeBinary(t, t.TempDir(), "gravinet-built", "399", false, true)
	installDir := t.TempDir()
	target := fakeBinary(t, installDir, "gravinet", "398", false, true)

	restarts := 0
	g := NewGuard(t.TempDir(), func() error { restarts++; return nil }, t.Logf)
	if err := g.Arm(State{Target: target, From: "398", To: "399", PrePeers: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), candidate, Options{Target: target, RunningVersion: "398"}); err != nil {
		t.Fatal(err)
	}

	// Each call is one start of the new binary that never confirms.
	for i := 1; i <= MaxBoots; i++ {
		act, s := g.OnBoot()
		if act != BootNormal {
			t.Fatalf("boot %d: reverted early (allowance is %d)", i, MaxBoots)
		}
		if s.Boots != i {
			t.Fatalf("boot %d: counted %d", i, s.Boots)
		}
		if got := probeVersion(t, target); got != "399" {
			t.Fatalf("boot %d: binary is %q, should still be the new one", i, got)
		}
	}
	act, s := g.OnBoot()
	if act != BootReverted {
		t.Fatalf("the boot after the allowance was spent should revert, got %v", act)
	}
	if s.Phase != PhaseReverted {
		t.Fatalf("phase is %q, want reverted", s.Phase)
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("after revert the installed binary is %q, want the old 398", got)
	}
	if restarts != 1 {
		t.Fatalf("the restored binary was not put back into service (restarts=%d)", restarts)
	}
	// The restored (old) binary now boots. It must not count itself against the
	// failed upgrade and try to revert the revert.
	if act, _ := g.OnBoot(); act != BootNormal {
		t.Fatal("the rescued binary tried to rescue itself")
	}
}

// The other failure: the new binary runs fine but cannot rejoin the mesh. The
// process is healthy; the node is not.
func TestGuardWatchRevertsWhenHealthNeverArrives(t *testing.T) {
	candidate := fakeBinary(t, t.TempDir(), "gravinet-built", "399", false, true)
	target := fakeBinary(t, t.TempDir(), "gravinet", "398", false, true)

	restarted := make(chan struct{}, 1)
	g := NewGuard(t.TempDir(), func() error { restarted <- struct{}{}; return nil }, t.Logf)
	// A one-second window, so the test does not take ninety.
	if err := g.Arm(State{Target: target, From: "398", To: "399", PrePeers: 4, ConfirmSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(context.Background(), candidate, Options{Target: target}); err != nil {
		t.Fatal(err)
	}
	g.OnBoot()
	g.Watch(func() (bool, string) { return false, "0 of 4 peers reconnected" })

	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the watchdog never fired; a node that came up without the mesh stayed on the new binary")
	}
	s := g.Load()
	if s.Phase != PhaseReverted {
		t.Fatalf("phase %q, want reverted", s.Phase)
	}
	if !strings.Contains(s.LastError, "peers reconnected") {
		t.Fatalf("the recorded reason (%q) should be the health check's own, not a generic one", s.LastError)
	}
	if got := probeVersion(t, target); got != "398" {
		t.Fatalf("installed binary is %q after the watchdog reverted, want 398", got)
	}
}

func TestGuardWatchCommitsWhenHealthy(t *testing.T) {
	candidate := fakeBinary(t, t.TempDir(), "gravinet-built", "399", false, true)
	target := fakeBinary(t, t.TempDir(), "gravinet", "398", false, true)

	g := NewGuard(t.TempDir(), func() error { t.Error("a healthy node must not be restarted"); return nil }, t.Logf)
	g.Arm(State{Target: target, From: "398", To: "399", PrePeers: 4, ConfirmSeconds: 1})
	Apply(context.Background(), candidate, Options{Target: target})
	g.OnBoot()
	g.Watch(func() (bool, string) { return true, "" })

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if g.Load().Phase == PhaseCommitted {
			// The backup is deliberately kept even after a commit: `upgrade
			// rollback` is for regressions that health checks do not see.
			if _, err := os.Stat(BackupPath(target)); err != nil {
				t.Fatal("the backup was deleted on commit, leaving no way back")
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("never committed; phase is %q", g.Load().Phase)
}
