package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildCompilesThisActualTree is the end-to-end check the unit tests above
// deliberately cannot be: it tars up this very repository, hands the archive to
// Build exactly as an upload or a Manager push would, and requires a real
// binary to come out that reports the version baked into the source.
//
// Everything else in this file stubs the compiler out, which is right for
// testing gates but leaves the actual claim — "a node can turn a source archive
// into a runnable gravinet" — untested. That claim is the entire feature, and
// it is the one that breaks silently: a build flag that stops working, a probe
// regex that drifts from main.go's output, an extraction that produces a tree
// `go build` won't accept. None of those show up in a gate test.
//
// Skipped under -short and when no Go toolchain is reachable, since it is a
// genuine multi-minute compile.
func TestBuildCompilesThisActualTree(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the whole tree; skipped under -short")
	}
	if _, err := locateGo(); err != nil {
		t.Skipf("no Go toolchain reachable: %v", err)
	}

	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	want := SourceVersion(repo)
	if want == "" {
		t.Fatal("could not read this tree's own version")
	}

	archive := tarRepo(t, repo)
	t.Logf("archived this tree: %d bytes, version %s", len(archive), want)

	bin, moduleRoot, probe, cleanup, err := Build(context.Background(), bytes.NewReader(archive), t.TempDir())
	if err != nil {
		t.Fatalf("Build failed on this project's own source: %v", err)
	}
	defer cleanup()

	if moduleRoot == "" {
		t.Error("Build returned an empty moduleRoot")
	}
	if fi, err := os.Stat(filepath.Join(moduleRoot, "go.mod")); err != nil || fi.IsDir() {
		t.Errorf("moduleRoot %s does not contain go.mod: %v", moduleRoot, err)
	}
	if fi, err := os.Stat(filepath.Join(moduleRoot, "docs", "API.md")); err != nil || fi.IsDir() {
		t.Errorf("moduleRoot %s does not contain docs/API.md, so SyncInstalledDocs would silently skip it: %v", moduleRoot, err)
	}

	if probe.Version != want {
		t.Errorf("built binary reports version %q, source says %q", probe.Version, want)
	}
	if fi, err := os.Stat(bin); err != nil || fi.Size() == 0 {
		t.Fatalf("no usable binary at %s: %v", bin, err)
	}

	// Preflight has to accept its own output. A build that produces something
	// Preflight then refuses would be a self-inconsistency no gate test could
	// surface.
	if _, err := Preflight(context.Background(), bin, Options{}); err != nil {
		t.Fatalf("Preflight rejected a binary this package just built: %v", err)
	}
	t.Logf("built and preflighted gravinet %s (%s/%s, pam=%v)", probe.Version, probe.OS, probe.Arch, probe.PAM)
}

// tarRepo builds a .tgz of the repository in the shape the installers ship and
// the extractor expects: everything under a single top-level directory. Build
// output, VCS metadata and any prior upgrade state are left out, the same way a
// release archive would leave them out.
func tarRepo(t *testing.T, repo string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	skip := func(rel string) bool {
		for _, p := range []string{".git", "upgrades", ".goenv"} {
			if rel == p || strings.HasPrefix(rel, p+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}

	err := filepath.Walk(repo, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil || rel == "." {
			return err
		}
		if skip(rel) {
			if fi.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks are refused by the extractor by design; skip rather than
		// emit one and fail the test on our own fixture.
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(filepath.Join("gravinet", rel))
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
