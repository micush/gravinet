package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gravinet/internal/logx"
	"gravinet/internal/upgrade"
)

// minimalSourceTgz builds the smallest archive extractSourceArchive will
// accept as "gravinet": a go.mod and a cmd/gravinet/main.go that reports
// version on `path version` and otherwise just exits 0. It deliberately
// imports nothing from this module's own internal packages — it needs to
// compile standalone with `go build ./cmd/gravinet` run against a throwaway
// module root, not pull in gravinet's real dependency graph.
func minimalSourceTgz(t *testing.T, version string) []byte {
	t.Helper()
	// version sits alone on its own line, inside a var block — the same shape
	// cmd/gravinet/main.go's real `var ( version = "NNN" ... )` uses, and the
	// one sourceVersionRe (SourceVersion's `^\s*version\s*=...` regex) actually
	// requires: it anchors on the start of the line, so "const version = ..."
	// on a single line would NOT match, only "version = ..." with nothing but
	// whitespace before it.
	mainGo := "package main\n\n" +
		"import (\n\t\"fmt\"\n\t\"os\"\n\t\"runtime\"\n)\n\n" +
		"var (\n\tversion = \"" + version + "\"\n)\n\n" +
		"func main() {\n" +
		"\tif len(os.Args) > 1 && os.Args[1] == \"version\" {\n" +
		"\t\tfmt.Printf(\"gravinet %s (testbuild) %s/%s pam=no\\n\", version, runtime.GOOS, runtime.GOARCH)\n" +
		"\t\treturn\n" +
		"\t}\n" +
		"}\n"
	files := map[string]string{
		"gravinet/go.mod":               "module gravinet\n\ngo 1.22\n",
		"gravinet/cmd/gravinet/main.go": mainGo,
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// spoolArchive writes an archive to a file under dir, the shape controlOp's
// "apply" case expects (a src_path on disk, exactly what handleUpgradeSource
// and handleUpgradeRemoteApply both spool their uploads to before calling
// into this same op).
func spoolArchive(t *testing.T, dir string, b []byte) string {
	t.Helper()
	path := filepath.Join(dir, "src.tgz")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// testUpgradeSvc builds an upgradeSvc with no engine and no config path —
// safe for tests that never reach peersConnected() (the real-apply path's
// Arm call) or SelfTest (which no-ops on an empty ConfigPath). Both tests
// below stay within that boundary: the skip path returns before Build is
// even called, and the "differs" path uses dry_run, which stops after
// Preflight and never reaches Arm.
func testUpgradeSvc(t *testing.T, version string) *upgradeSvc {
	t.Helper()
	dir := t.TempDir()
	return &upgradeSvc{
		stateDir: dir,
		guard:    upgrade.NewGuard(dir, func() error { return nil }, logx.Infof),
		target:   filepath.Join(dir, "gravinet-target"),
		version:  version,
	}
}

// TestControlOpApplySkipsWhenAlreadyOnThisVersion is the behavior reported
// as missing: uploading a source archive whose baked-in version matches
// what this node already runs should not trigger a build, a preflight, an
// Arm, or a restart — there is nothing to change. Asserted two ways: the
// reply itself says skipped/already_on, and the guard's phase is left at
// PhaseIdle, proving Arm (the real-apply path's own state transition) was
// never reached.
func TestControlOpApplySkipsWhenAlreadyOnThisVersion(t *testing.T) {
	u := testUpgradeSvc(t, "628")
	archive := minimalSourceTgz(t, "628")
	src := spoolArchive(t, u.stateDir, archive)

	body, _ := json.Marshal(map[string]any{"src_path": src})
	out, err := u.controlOp("apply", body)
	if err != nil {
		t.Fatalf("controlOp(apply) on a same-version archive returned an error: %v", err)
	}

	var resp struct {
		OK         bool   `json:"ok"`
		Skipped    bool   `json:"skipped"`
		AlreadyOn  string `json:"already_on"`
		Applied    string `json:"applied"`
		Restarting bool   `json:"restarting"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("could not parse controlOp's reply %q: %v", out, err)
	}
	if !resp.OK || !resp.Skipped {
		t.Fatalf("controlOp(apply) reply = %s, want ok:true skipped:true", out)
	}
	if resp.AlreadyOn != "628" {
		t.Errorf("already_on = %q, want %q", resp.AlreadyOn, "628")
	}
	if resp.Applied != "" || resp.Restarting {
		t.Errorf("a skipped apply must not also claim applied/restarting: %s", out)
	}
	if st := u.guard.Load(); st.Phase != upgrade.PhaseIdle {
		t.Errorf("guard phase = %q after a skipped apply, want idle \u2014 Arm should never have run", st.Phase)
	}
}

// TestControlOpApplyBuildsWhenVersionDiffers is the same-version skip's
// negative case, and doubles as a regression test for the seek-back: the
// version check reads the uploaded archive once (via ExtractedVersion)
// before the real Build call reads it a second time, and if that rewind
// were missing or wrong, Build would see an empty (or partial) stream and
// fail every apply whose version genuinely differs \u2014 not just the skip
// path. dry_run is used so the flow stops after Preflight, before Arm,
// keeping this test's upgradeSvc (no mesh engine) valid throughout.
func TestControlOpApplyBuildsWhenVersionDiffers(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("no Go toolchain reachable: %v", err)
	}
	u := testUpgradeSvc(t, "628")
	archive := minimalSourceTgz(t, "629")
	src := spoolArchive(t, u.stateDir, archive)

	body, _ := json.Marshal(map[string]any{"src_path": src, "dry_run": true})
	out, err := u.controlOp("apply", body)
	if err != nil {
		t.Fatalf("controlOp(apply, dry_run) on a differing-version archive failed \u2014 possibly the post-version-check seek-back regressed: %v", err)
	}

	var resp struct {
		OK         bool   `json:"ok"`
		DryRun     bool   `json:"dry_run"`
		Skipped    bool   `json:"skipped"`
		WouldApply string `json:"would_apply"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("could not parse controlOp's reply %q: %v", out, err)
	}
	if resp.Skipped {
		t.Fatalf("a differing-version archive was reported as skipped: %s", out)
	}
	if !resp.OK || !resp.DryRun || resp.WouldApply != "629" {
		t.Fatalf("controlOp(apply, dry_run) reply = %s, want ok:true dry_run:true would_apply:629", out)
	}
}
