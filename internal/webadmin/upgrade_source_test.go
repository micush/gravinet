package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"gravinet/internal/upgrade"
)

func buildTgz(t *testing.T, entries map[string]string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range entries {
		hdr := &tar.Header{Name: name, Mode: 0644, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractSourceTarGzRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	tgz := buildTgz(t, map[string]string{
		"gravinet/go.mod":               "module gravinet\n",
		"gravinet/cmd/gravinet/main.go": "package main\nfunc main(){}\n",
		"../../etc/evil":                "pwned",
	})
	_, err := extractSourceTarGz(tgz, dir)
	if err == nil {
		t.Fatal("expected an error for a path-traversal entry, got nil")
	}
	t.Logf("correctly rejected: %v", err)
	// Confirm nothing escaped, regardless of error timing/ordering.
	if _, statErr := os.Stat(filepath.Join(dir, "..", "..", "etc", "evil")); statErr == nil {
		t.Fatal("path traversal entry was actually written outside destDir")
	}
}

func TestExtractSourceTarGzRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "gravinet/go.mod", Mode: 0644, Size: int64(len("module gravinet\n"))})
	tw.Write([]byte("module gravinet\n"))
	tw.WriteHeader(&tar.Header{Name: "gravinet/evil-link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"})
	tw.Close()
	gz.Close()
	_, err := extractSourceTarGz(&buf, dir)
	if err == nil {
		t.Fatal("expected an error for a symlink entry, got nil")
	}
	t.Logf("correctly rejected: %v", err)
}

func TestExtractSourceTarGzHappyPath(t *testing.T) {
	dir := t.TempDir()
	tgz := buildTgz(t, map[string]string{
		"gravinet/go.mod":               "module gravinet\n",
		"gravinet/cmd/gravinet/main.go": "package main\nfunc main(){}\n",
	})
	root, err := extractSourceTarGz(tgz, dir)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if filepath.Base(root) != "gravinet" {
		t.Fatalf("expected module root to be the 'gravinet' dir, got %q", root)
	}
}

// findRepoRoot locates the module root from this test file's own location
// (internal/webadmin/upgrade_source_test.go is always two directories below
// it), so the end-to-end build test packages gravinet's actual current
// source rather than a synthetic stand-in.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/webadmin/ -> internal/ -> repo root
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at %s: %v", root, err)
	}
	return root
}

// tarGzOfDir packages go.mod, cmd/, internal/, and third_party/ from srcDir
// into a gzip-compressed tar stream under a top-level prefix dir — the same
// shape extractSourceTarGz expects and the same shape the real gravinet
// source tarball the installers ship has. install/, docs/, and scripts/ are
// left out: irrelevant to `go build ./cmd/gravinet` and only slow the test
// down.
func tarGzOfDir(t *testing.T, srcDir, prefix string) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	include := []string{"go.mod", "cmd", "internal", "third_party"}
	for _, rel := range include {
		full := filepath.Join(srcDir, rel)
		walkErr := filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			relPath, err := filepath.Rel(srcDir, path)
			if err != nil {
				return err
			}
			name := filepath.ToSlash(filepath.Join(prefix, relPath))
			if info.IsDir() {
				hdr, err := tar.FileInfoHeader(info, "")
				if err != nil {
					return err
				}
				hdr.Name = name + "/"
				return tw.WriteHeader(hdr)
			}
			if !info.Mode().IsRegular() {
				return nil // no symlinks etc. expected here, skip defensively
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = name
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		})
		if walkErr != nil {
			t.Fatalf("packaging %s into test tarball: %v", rel, walkErr)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return &buf
}

// newTestStore is a bare, untrusted (no signing keys configured) upgrade
// store — exactly what unsigned/local-only mode uses, which is the only
// mode stageFromSource is ever reached from.
func newTestStore(t *testing.T) *upgrade.Store {
	t.Helper()
	st, err := upgrade.NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("creating test store: %v", err)
	}
	return st
}
// with, by asking the OS for wherever "go" would currently be found on
// PATH — done once, before any test clears PATH, so it reflects the
// environment's real installation rather than a hardcoded guess.
func realGoPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain available in this environment to test against")
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolving real go binary: %v", err)
	}
	return resolved
}

// TestLocateGoFindsToolchainOutsidePATH reproduces the bug report: a Go
// toolchain is genuinely installed at one of the well-known locations the
// platform installers use, but the process's PATH — standing in for a
// launchd/rc.d service's minimal inherited environment — doesn't include
// it. Before the fix, this failed with "no Go toolchain found" even though
// the toolchain was right there. Table-driven over both fallback slots:
// /usr/local/go/bin (the go.dev-tarball installers use) and /usr/local/bin
// (where FreeBSD's `pkg install go` / OpenBSD's `pkg_add go` put it
// instead — the case the first round of this fix missed, since FreeBSD's
// ensure_go() tries pkg *before* the tarball).
func TestLocateGoFindsToolchainOutsidePATH(t *testing.T) {
	for _, slot := range []string{"/usr/local/go/bin (tarball installer)", "/usr/local/bin (pkg/pkg_add installer)"} {
		t.Run(slot, func(t *testing.T) {
			real := realGoPath(t)

			fakeInstallDir := t.TempDir()
			if err := os.Symlink(real, filepath.Join(fakeInstallDir, "go")); err != nil {
				t.Fatalf("symlinking fake install: %v", err)
			}

			oldDirs := goInstallDirs
			goInstallDirs = []string{fakeInstallDir}
			defer func() { goInstallDirs = oldDirs }()

			t.Setenv("PATH", "") // simulate a service manager's minimal environment

			got, err := locateGo()
			if err != nil {
				t.Fatalf("locateGo failed with an empty PATH despite a toolchain at the well-known fallback location: %v", err)
			}
			if got != filepath.Join(fakeInstallDir, "go") {
				t.Fatalf("locateGo returned %q, want the fallback path", got)
			}
		})
	}
}

// TestGoInstallDirsCoversPkgInstallLocation locks in the actual production
// default (not a test double) against regressing back to checking only the
// tarball location. FreeBSD's installer tries `pkg install go` — which
// lands the binary in /usr/local/bin, the ports prefix's bin dir — before
// it ever falls back to unpacking the go.dev tarball into /usr/local/go;
// OpenBSD's pkg_add path does the same. A goInstallDirs that only knows
// about /usr/local/go/bin silently fails on exactly that (common) case.
func TestGoInstallDirsCoversPkgInstallLocation(t *testing.T) {
	want := map[string]bool{"/usr/local/go/bin": false, "/usr/local/bin": false}
	for _, d := range goInstallDirs {
		if _, ok := want[d]; ok {
			want[d] = true
		}
	}
	for dir, found := range want {
		if !found {
			t.Errorf("goInstallDirs is missing %q", dir)
		}
	}
}

// TestLocateGoErrorWhenTrulyMissing confirms the error path still fires
// correctly (and doesn't, say, silently fall back to something PATH-like)
// when Go genuinely isn't anywhere this code knows to look.
func TestLocateGoErrorWhenTrulyMissing(t *testing.T) {
	oldDirs := goInstallDirs
	goInstallDirs = []string{t.TempDir()} // empty directory, no "go" in it
	defer func() { goInstallDirs = oldDirs }()

	t.Setenv("PATH", "")

	if _, err := locateGo(); err == nil {
		t.Fatal("expected an error when no toolchain is reachable, got nil")
	}
}

// TestStageFromSourceBuildsWithGoOffPATH is the end-to-end version of the
// bug: build gravinet's own source tree via stageFromSource exactly as the
// web admin's upload handler would, with PATH cleared and Go only reachable
// through the installer's well-known fallback location. Before the fix this
// failed at the "no Go toolchain found" step; now it should build, probe,
// and ingest successfully.
func TestStageFromSourceBuildsWithGoOffPATH(t *testing.T) {
	real := realGoPath(t)
	repoRoot := findRepoRoot(t)

	fakeInstallDir := t.TempDir()
	if err := os.Symlink(real, filepath.Join(fakeInstallDir, "go")); err != nil {
		t.Fatalf("symlinking fake install: %v", err)
	}
	oldDirs := goInstallDirs
	goInstallDirs = []string{fakeInstallDir}
	defer func() { goInstallDirs = oldDirs }()
	t.Setenv("PATH", "")
	t.Setenv("CGO_ENABLED", "0") // no C toolchain guaranteed reachable with PATH cleared

	tgz := tarGzOfDir(t, repoRoot, "gravinet")
	st := newTestStore(t)

	m, err := stageFromSource(st, tgz)
	if err != nil {
		t.Fatalf("stageFromSource failed with Go reachable only via the fallback path: %v", err)
	}
	if m.Version == "" {
		t.Fatal("expected a non-empty version on the resulting manifest")
	}
}
