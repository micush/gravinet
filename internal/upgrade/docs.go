package upgrade

import (
	"os"
	"path/filepath"
	"runtime"
)

// docFiles maps each doc file's location within a source tree (relative to
// moduleRoot) to the filename it's installed under. API.md lives under docs/
// in the repo, unlike the other three at the repo root — the same asymmetry
// every install script's own doc-copy loop already accounts for.
var docFiles = []struct{ src, dst string }{
	{"README.md", "README.md"},
	{"LICENSE", "LICENSE"},
	{"getting-started.md", "getting-started.md"},
	{filepath.Join("docs", "API.md"), "API.md"},
}

// SyncInstalledDocs copies README.md, LICENSE, getting-started.md, and
// docs/API.md from a freshly-extracted source tree (moduleRoot, as returned
// by Build — call this before invoking Build's cleanup) into this install's
// doc directory, so an in-place upgrade keeps those files current too, not
// just the binary.
//
// Without this, a node upgraded via the web admin or `gravinet upgrade
// apply` — rather than by re-running a platform installer — would silently
// keep serving whatever doc snapshot the last full install left behind
// forever. Worse, a doc file that didn't exist at install time (as API.md
// didn't, for any node installed before it was added) would never appear at
// all: the installer is the only thing that has ever populated that
// directory, and an in-place upgrade never touches it. This closes that gap
// generally, not just for API.md — the next doc file this project adds gets
// picked up the same way, automatically.
//
// Mirrors the install scripts' own doc-directory convention exactly, and
// deliberately duplicates it rather than importing internal/config for one
// path formula: unix installs docs at <prefix>/share/doc/gravinet/<file>,
// where <prefix> is one level above the target binary's directory (normally
// <prefix>/bin/gravinet); Windows installs them beside the binary itself,
// matching install-windows.ps1. This is also the very first candidate
// config.resolveDocPath's own read-side search checks, so anything written
// here is exactly what the Info pages will find on their next request — no
// restart needed, since those pages already read fresh from disk every time.
//
// Best-effort and silent-safe by design: a doc-sync failure (a source file
// missing from this particular archive, an unwritable destination) is never
// allowed to fail the upgrade itself — the binary swap is the one thing that
// must not be held hostage to a documentation file. Returns the destination
// filenames actually copied, for the caller to log; a shorter list than
// docFiles is not itself an error.
func SyncInstalledDocs(moduleRoot, target string) []string {
	if moduleRoot == "" || target == "" {
		return nil
	}
	exeDir := filepath.Dir(target)
	dstDir := filepath.Join(exeDir, "..", "share", "doc", "gravinet")
	if runtime.GOOS == "windows" {
		dstDir = exeDir
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return nil
	}
	var synced []string
	for _, f := range docFiles {
		src := filepath.Join(moduleRoot, f.src)
		if fi, err := os.Stat(src); err != nil || fi.IsDir() {
			continue // not present in this source tree; leave whatever's already installed alone
		}
		if err := copyDocFile(src, filepath.Join(dstDir, f.dst)); err == nil {
			synced = append(synced, f.dst)
		}
	}
	return synced
}

// copyDocFile copies a small text file via a temp file + rename in the
// destination directory, so a concurrent reader — the Info pages read these
// files fresh on every request, with no locking — never observes a
// half-written file mid-copy.
func copyDocFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".gravinet-tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
