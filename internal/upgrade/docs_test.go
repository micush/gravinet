package upgrade

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestSyncInstalledDocsCopiesWhatExists builds a minimal fake source tree
// (just the four doc files, no go.mod/no real repo) and checks that
// SyncInstalledDocs copies exactly the ones present, into the same relative
// location config.resolveDocPath's own read-side search checks first — the
// two sides of this contract are tested separately, in different packages,
// so this pins the write side's path formula against a literal expectation
// rather than trusting it agrees with the read side by construction.
func TestSyncInstalledDocsCopiesWhatExists(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "README.md"), "# readme\n")
	mustWrite(t, filepath.Join(src, "LICENSE"), "license text\n")
	// Deliberately omit getting-started.md, to check a missing file is
	// skipped rather than treated as an error.
	mustWrite(t, filepath.Join(src, "docs", "API.md"), "# api\n")

	// target sits at <prefix>/bin/gravinet, mirroring a real install.
	prefix := t.TempDir()
	target := filepath.Join(prefix, "bin", "gravinet")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}

	synced := SyncInstalledDocs(src, target)
	wantSynced := map[string]bool{"README.md": true, "LICENSE": true, "API.md": true}
	if len(synced) != len(wantSynced) {
		t.Fatalf("synced = %v, want exactly %v", synced, wantSynced)
	}
	for _, name := range synced {
		if !wantSynced[name] {
			t.Errorf("unexpected file reported synced: %s", name)
		}
	}

	var dstDir string
	if runtime.GOOS == "windows" {
		dstDir = filepath.Dir(target)
	} else {
		dstDir = filepath.Join(prefix, "share", "doc", "gravinet")
	}
	for name, want := range map[string]string{"README.md": "# readme\n", "LICENSE": "license text\n", "API.md": "# api\n"} {
		got, err := os.ReadFile(filepath.Join(dstDir, name))
		if err != nil {
			t.Errorf("%s not written to %s: %v", name, dstDir, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dstDir, "getting-started.md")); err == nil {
		t.Error("getting-started.md was not in the source tree but got written anyway")
	}
}

// TestSyncInstalledDocsNeverFailsUpgrade checks the empty/missing-input
// cases are quiet no-ops, not panics or errors a caller would have to
// handle specially — SyncInstalledDocs's whole contract is that a doc-sync
// problem can never be allowed to look like an upgrade failure.
func TestSyncInstalledDocsNeverFailsUpgrade(t *testing.T) {
	if got := SyncInstalledDocs("", "/some/target"); got != nil {
		t.Errorf("empty moduleRoot: got %v, want nil", got)
	}
	if got := SyncInstalledDocs("/some/root", ""); got != nil {
		t.Errorf("empty target: got %v, want nil", got)
	}
	// A moduleRoot that doesn't exist at all (e.g. already cleaned up) —
	// every file is simply absent, so this degrades to an empty result,
	// not an error.
	if got := SyncInstalledDocs(filepath.Join(t.TempDir(), "gone"), filepath.Join(t.TempDir(), "bin", "gravinet")); got != nil {
		t.Errorf("nonexistent moduleRoot: got %v, want nil", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
