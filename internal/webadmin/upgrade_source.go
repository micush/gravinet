package webadmin

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gravinet/internal/upgrade"
)

// maxSourceUploadSize caps a source archive upload — a gzip-compressed tar
// (.tgz/.tar.gz) or a zip (.zip), detected by content, not extension (see
// extractSourceArchive). gravinet's own source tree (no vendor/, no build
// output, no git history in the archive the installers or GitHub's own
// "Download ZIP" button produce) is a few MiB; 128 MiB is generous headroom
// without being large enough to be a meaningful disk-fill vector on a small
// box.
const maxSourceUploadSize = 128 << 20

// maxSourceExtractedSize caps the *decompressed* total a tgz may expand to —
// a gzip bomb is a handful of KiB on the wire and gigabytes off it, and the
// wire-side cap above does nothing to stop that. Enforced per-file and
// cumulatively as bytes are copied, not read from the tar header (which is
// attacker-controlled and never verified against the actual stream).
const maxSourceExtractedSize = 512 << 20

// buildTimeout bounds a full `go build`, including any module/toolchain
// fetch it triggers. Generous, matching what the old peer-fetch machinery
// used for the same reason: this is a local, infrequent, operator-initiated
// action, not something on a hot path.
const buildTimeout = 10 * time.Minute

// extractSourceArchive safely unpacks r — a gzip-compressed tar (.tgz/
// .tar.gz) or a zip (.zip) — under destDir, auto-detecting which of the two
// it is by content (the leading bytes: 0x1f 0x8b for gzip, "PK" plus a
// third byte of 0x03, 0x05, or 0x07 for zip's few defined first-record
// types), not by filename or extension: this handler's caller posts a raw
// body with no filename attached at all, and even where one exists,
// trusting an extension to say what a file actually contains is exactly
// the kind of check the rest of this file is careful not to rely on.
//
// zip support exists because GitHub's own "Download ZIP" button — and
// most anything else that hands out a repo snapshot without requiring a
// git client — produces a .zip, not a .tgz; before this, that download
// couldn't be used as an upgrade source at all despite being the single
// most likely way an operator without a local clone would actually get
// this project's source.
//
// Every format archive/zip can parse needs io.ReaderAt plus a known
// length — its central directory lives at the end of the stream and has
// to be located and read before anything else, which a forward-only
// io.Reader (what an HTTP request body is) can't support. So r is always
// spooled to a temp file first, even for the gzip/tar case where that
// isn't strictly required, both to keep one code path instead of two and
// to make detecting the format (which likewise needs to look at the
// stream before committing to either parser) simple: read once, sniff,
// then hand the same seekable file to whichever extractor applies. Capped
// at maxSourceUploadSize+1 bytes independent of whatever the caller's own
// reader may or may not already limit (the one real caller wraps its
// request body in http.MaxBytesReader, but this stays self-contained
// rather than trusting that).
func extractSourceArchive(r io.Reader, destDir string) (moduleRoot string, err error) {
	tmp, err := os.CreateTemp(filepath.Dir(destDir), ".upload-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	n, copyErr := io.Copy(tmp, io.LimitReader(r, maxSourceUploadSize+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > maxSourceUploadSize {
		return "", fmt.Errorf("upload exceeds the %d-byte size ceiling", int64(maxSourceUploadSize))
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var sig [4]byte
	if _, err := io.ReadFull(f, sig[:]); err != nil {
		return "", errors.New("upload is too small to be a valid .tgz/.tar.gz or .zip source archive")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	switch {
	case sig[0] == 0x1f && sig[1] == 0x8b:
		return extractSourceTarGz(f, destDir)
	case sig[0] == 'P' && sig[1] == 'K' && (sig[2] == 0x03 || sig[2] == 0x05 || sig[2] == 0x07):
		return extractSourceZip(f, n, destDir)
	default:
		return "", errors.New("not a valid .tgz/.tar.gz or .zip source archive (unrecognized file signature)")
	}
}

// extractSourceTarGz safely unpacks r (a gzip-compressed tar stream) under
// destDir, and returns the directory that actually contains go.mod — the
// archive this project's installers ship, and the one this handler
// expects (whichever of the two formats extractSourceArchive determined
// r to be), wraps everything in a single top-level directory (e.g.
// "gravinet/go.mod", "gravinet/cmd/..."), so the module root is a
// subdirectory of destDir, not destDir itself.
//
// "Safely" here means the standard tar-extraction hazards are closed, not
// that the uploader is assumed hostile — the same care applies to a tarball
// the operator built themselves, since a symlink or a ".." entry can end up
// in a tar stream through nothing more adversarial than a build tool's own
// quirk. Every entry's target path is resolved and checked to still be
// inside destDir before anything is written; symlinks and hardlinks are
// rejected outright (a source tree has no legitimate need for either, and a
// symlink is exactly how a tar entry escapes a path check that only looks at
// the entry's own name); and total decompressed bytes are counted against
// maxSourceExtractedSize as they're written, not trusted from the header.
func extractSourceTarGz(r io.Reader, destDir string) (moduleRoot string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("not a valid gzip-compressed tar archive (.tgz/.tar.gz): %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var total int64
	var foundGoMod string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("reading tar stream: %w", err)
		}

		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeReg:
			// handled below
		case tar.TypeSymlink, tar.TypeLink:
			return "", fmt.Errorf("refusing to extract %q: a source upload may not contain symlinks or hard links", hdr.Name)
		default:
			// Device nodes, FIFOs, etc. have no business in a source tree;
			// skip rather than fail, the same way most tar readers do for
			// entry types they don't understand.
			continue
		}

		// Clean, reject absolute paths and any ".." component, then confirm
		// the resolved path is still inside destDir. filepath.Clean alone is
		// not enough — "../x" cleans to itself — so the prefix check after
		// joining is the actual boundary, not the cleaning.
		name := filepath.Clean(hdr.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("refusing to extract %q: escapes the upload directory", hdr.Name)
		}
		target := filepath.Join(destDir, name)
		if target != destDir && !strings.HasPrefix(target, destDir+string(filepath.Separator)) {
			return "", fmt.Errorf("refusing to extract %q: escapes the upload directory", hdr.Name)
		}

		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			return "", err
		}
		n, err := io.Copy(f, io.LimitReader(tr, hdr.Size+1))
		f.Close()
		if err != nil {
			return "", err
		}
		if n != hdr.Size {
			return "", fmt.Errorf("%q: tar header claimed %d bytes, stream had at least %d", hdr.Name, hdr.Size, n)
		}
		total += n
		if total > maxSourceExtractedSize {
			return "", fmt.Errorf("upload expands past the %d-byte extraction ceiling", int64(maxSourceExtractedSize))
		}

		if foundGoMod == "" && filepath.Base(target) == "go.mod" {
			foundGoMod = filepath.Dir(target)
		}
	}

	if foundGoMod == "" {
		return "", errors.New("no go.mod found anywhere in the upload — this doesn't look like a gravinet source tree")
	}
	if _, err := os.Stat(filepath.Join(foundGoMod, "cmd", "gravinet")); err != nil {
		return "", errors.New("found a go.mod, but no cmd/gravinet under it — this doesn't look like a gravinet source tree")
	}
	return foundGoMod, nil
}

// extractSourceZip is extractSourceTarGz's zip-format counterpart, same
// contract and the same safety checks (path-escape rejection, symlink
// rejection, per-entry and cumulative size enforced by counting bytes
// actually written rather than trusting the header) — see its doc comment
// for the reasoning behind each; nothing here is that comment's mirror by
// coincidence, it's the same hazards under a different archive format,
// notably including GitHub's own "Download ZIP" output (which wraps a repo
// in a single top-level "<repo>-<branch>/..." directory, the same shape
// extractSourceTarGz already expects from a .tgz).
//
// r must support io.ReaderAt — zip's central directory is read from the end
// of the stream, so it can't be parsed from a forward-only reader — with
// size its total byte length; extractSourceArchive is what actually
// supplies both (a temp file it already spooled r to in order to sniff the
// format in the first place).
func extractSourceZip(r io.ReaderAt, size int64, destDir string) (moduleRoot string, err error) {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return "", fmt.Errorf("not a valid zip archive: %w", err)
	}

	var total int64
	var foundGoMod string
	for _, zf := range zr.File {
		fi := zf.FileInfo()
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("refusing to extract %q: a source upload may not contain symlinks", zf.Name)
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			// Device nodes, FIFOs, etc. (or anything else a nonstandard zip
			// writer might encode) have no business in a source tree; skip
			// rather than fail, the same as extractSourceTarGz does for tar
			// entry types it doesn't recognize.
			continue
		}

		// Same boundary check as extractSourceTarGz, applied to a zip
		// entry's name instead of a tar header's — see its comment for why
		// filepath.Clean alone isn't the actual boundary.
		name := filepath.Clean(zf.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("refusing to extract %q: escapes the upload directory", zf.Name)
		}
		target := filepath.Join(destDir, name)
		if target != destDir && !strings.HasPrefix(target, destDir+string(filepath.Separator)) {
			return "", fmt.Errorf("refusing to extract %q: escapes the upload directory", zf.Name)
		}

		if fi.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		mode := fi.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		rc, err := zf.Open()
		if err != nil {
			return "", fmt.Errorf("%q: %w", zf.Name, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return "", err
		}
		declared := int64(zf.UncompressedSize64)
		nCopied, copyErr := io.Copy(out, io.LimitReader(rc, declared+1))
		out.Close()
		rc.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if nCopied != declared {
			return "", fmt.Errorf("%q: zip entry claimed %d bytes, stream had at least %d", zf.Name, declared, nCopied)
		}
		total += nCopied
		if total > maxSourceExtractedSize {
			return "", fmt.Errorf("upload expands past the %d-byte extraction ceiling", int64(maxSourceExtractedSize))
		}

		if foundGoMod == "" && filepath.Base(target) == "go.mod" {
			foundGoMod = filepath.Dir(target)
		}
	}

	if foundGoMod == "" {
		return "", errors.New("no go.mod found anywhere in the upload — this doesn't look like a gravinet source tree")
	}
	if _, err := os.Stat(filepath.Join(foundGoMod, "cmd", "gravinet")); err != nil {
		return "", errors.New("found a go.mod, but no cmd/gravinet under it — this doesn't look like a gravinet source tree")
	}
	return foundGoMod, nil
}

// goInstallDirs are locations to check for a Go toolchain beyond what's on
// this process's own PATH, in the same order (and for the same reason) the
// platform installers' own ensure_go() checks them: PATH first, then these.
//
//   - /usr/local/go/bin — where official_install_go() in every installer
//     (install-linux.sh, install-macos.sh, install-freebsd.sh,
//     install-openbsd.sh) unpacks the go.dev tarball.
//   - /usr/local/bin — where a *package-manager* install lands instead:
//     FreeBSD's ensure_go() tries `pkg install go` before ever falling back
//     to the tarball, and OpenBSD's tries `pkg_add go` the same way. Both
//     pkg/pkg_add install into the ports prefix's bin dir directly, not into
//     a private go/bin subdirectory — that layout is specific to unpacking
//     the tarball by hand. On FreeBSD in particular, pkg is tried first and
//     is the common case, so a box that bootstrapped that way had its Go
//     invisible to this list until this entry was added.
//
// This matters because the web admin's build runs inside the gravinet
// daemon, which a service manager (launchd on macOS, rc.d on FreeBSD/
// OpenBSD, systemd on Linux) starts with its own minimal, inherited
// environment — not the interactive shell PATH an operator had when they
// (or an earlier run of the installer) put Go at one of these well-known
// locations. A toolchain that is unambiguously on the machine, and that the
// installer itself would find and reuse without re-fetching anything, is
// otherwise invisible to a bare exec.LookPath. The resulting "no Go
// toolchain found" error is wrong in exactly the way that's most confusing
// to debug: the operator can run `go version` just fine, from the very
// terminal they're reading the error in — because that terminal's PATH was
// never the daemon's PATH.
var goInstallDirs = []string{"/usr/local/go/bin", "/usr/local/bin"}

// locateGo finds a usable `go` binary: this process's PATH first (so an
// operator-customized service PATH, or a PATH a package-manager install
// already lives on, is always respected), then the well-known install
// locations above.
func locateGo() (string, error) {
	if p, err := exec.LookPath("go"); err == nil {
		return p, nil
	}
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	for _, dir := range goInstallDirs {
		p := filepath.Join(dir, name)
		if fi, statErr := os.Stat(p); statErr == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no Go toolchain found on this node (checked PATH and %s) — install Go first, the same as the platform installer would need to", strings.Join(goInstallDirs, ", "))
}

// buildFromSource runs `go build ./cmd/gravinet` against moduleRoot, mirroring
// install-linux.sh's build_from_source: try a cgo build first (needed for PAM
// web-admin auth), and if that specifically fails to compile, fall back to a
// static build rather than failing outright — a box without libpam headers
// still gets a working (if PAM-less) binary out of this, same as the
// installer. Returns the path to the built binary and the combined
// build output of whichever attempt succeeded (empty on success, for
// symmetry with the error case where it's the only diagnostic available).
//
// This does NOT attempt to install a Go toolchain or system packages if
// they're missing — that's what install-linux.sh's ensure_go/
// install_build_deps do, and doing the same thing from an HTTP handler would
// mean this node's web admin reaching out to a package manager and to the
// network to fetch a toolchain on its own initiative. That's a materially
// bigger and separate capability than "compile the source I already gave
// you", so it's out of scope here: if `go` can't be located at all (see
// locateGo above), this fails with a clear message instead, the same as the
// installer would if it couldn't obtain one either.
func buildFromSource(ctx context.Context, moduleRoot, outPath string) (output string, err error) {
	goBin, err := locateGo()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()

	// A self-contained cache/home under the upload's own temp dir, rather
	// than whatever (or nothing) the daemon's own service environment
	// provides — a systemd/launchd service often runs with a minimal env
	// that has no $HOME, which a bare `go build` depends on for its module
	// and build caches.
	goEnv := filepath.Join(filepath.Dir(outPath), ".goenv")
	env := append(os.Environ(),
		"GOTOOLCHAIN=auto",
		"HOME="+goEnv,
		"GOCACHE="+filepath.Join(goEnv, "cache"),
		"GOPATH="+filepath.Join(goEnv, "path"),
		// goBin was possibly found outside PATH (the whole point of
		// locateGo's fallback); put its directory on PATH too, in case the
		// build itself shells out expecting `go` to be reachable that way.
		"PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
	)

	run := func(cgo string) (string, error) {
		cmd := exec.CommandContext(ctx, goBin, "build", "-buildvcs=false", "-trimpath", "-ldflags", "-s -w", "-o", outPath, "./cmd/gravinet")
		cmd.Dir = moduleRoot
		cmd.Env = append(append([]string{}, env...), "CGO_ENABLED="+cgo)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	cgoOut, cgoErr := run("1")
	if cgoErr == nil {
		return "", nil
	}
	staticOut, staticErr := run("0")
	if staticErr == nil {
		return "", nil
	}
	return "cgo build failed:\n" + cgoOut + "\n\nstatic build also failed:\n" + staticOut, fmt.Errorf("build failed: %w", staticErr)
}

// StageFromSource is the full pipeline handleUpgradeStageSource drives:
// extract, build, probe, ingest. src is the raw archive stream — a
// gzip-compressed tar (.tgz/.tar.gz) or a zip (.zip), either is fine (see
// extractSourceArchive, which is what tells them apart, by content, and
// which self-caps how much it will read regardless of the caller). notes is
// recorded in the generated manifest as the artifact's provenance. Every
// temp directory this creates is cleaned up before returning, win or lose.
//
// Exported (v553) so `gravinet upgrade stage -src` can reuse it verbatim —
// same reasoning as RunVtysh/LocalRouteTableText/TakeHostSnapshot in v550:
// one implementation of the extract-build-probe-ingest sequence, reached
// from two front doors, not a CLI reimplementation that could drift from
// what the web admin's upload does. The caller owns the trust-policy check
// (refusing when trusted_keys is configured) exactly the way
// handleUpgradeStageSource does — see its doc comment for why source
// builds are only ever offered in local-only-unsigned mode.
func StageFromSource(st *upgrade.Store, src io.Reader, notes string) (upgrade.Manifest, error) {
	workDir, err := os.MkdirTemp(st.Dir(), ".source-build-*")
	if err != nil {
		return upgrade.Manifest{}, err
	}
	defer os.RemoveAll(workDir)

	extractDir := filepath.Join(workDir, "src")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return upgrade.Manifest{}, err
	}
	moduleRoot, err := extractSourceArchive(src, extractDir)
	if err != nil {
		return upgrade.Manifest{}, err
	}

	// Windows requires an executable to carry a recognized extension
	// (.exe, .bat, ...) before os/exec will run it — even when given the
	// file's full, unambiguous path. Without this, `go build -o` here
	// happily produces a perfectly good PE binary named "gravinet-built"
	// (no extension), and the very next step, ProbeBinary running it to
	// read back its own version, fails with "executable file not found in
	// %PATH%": a message about PATH for a binary that was never looked up
	// on PATH at all, describing a missing extension instead of a missing
	// file. install-windows.ps1's own build path already knows this — its
	// output is unconditionally named gravinet.exe.
	binName := "gravinet-built"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	outPath := filepath.Join(workDir, binName)
	buildOutput, err := buildFromSource(context.Background(), moduleRoot, outPath)
	if err != nil {
		if buildOutput != "" {
			return upgrade.Manifest{}, fmt.Errorf("%w\n\n%s", err, truncateBuildOutput(buildOutput))
		}
		return upgrade.Manifest{}, err
	}

	probe, err := upgrade.ProbeBinary(context.Background(), outPath)
	if err != nil {
		return upgrade.Manifest{}, fmt.Errorf("built a binary but could not identify it: %w", err)
	}
	m, err := upgrade.NewManifest(outPath, probe.Version, probe.OS, probe.Arch, probe.PAM, notes)
	if err != nil {
		return upgrade.Manifest{}, err
	}
	f, err := os.Open(outPath)
	if err != nil {
		return upgrade.Manifest{}, err
	}
	defer f.Close()
	if err := st.Ingest(m, f); err != nil {
		return upgrade.Manifest{}, err
	}
	return m, nil
}

// truncateBuildOutput keeps a failed build's compiler output from ballooning
// into a multi-MiB error response — the tail is what almost always has the
// actual error, so keep that over the head.
func truncateBuildOutput(s string) string {
	const limit = 8 << 10
	if len(s) <= limit {
		return s
	}
	return "\u2026 (truncated) \u2026\n" + s[len(s)-limit:]
}
