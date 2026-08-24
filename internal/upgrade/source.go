package upgrade

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// MaxSourceUploadSize caps a source archive upload — a gzip-compressed tar
// (.tgz/.tar.gz) or a zip (.zip), detected by content, not extension (see
// extractSourceArchive). gravinet's own source tree (no vendor/, no build
// output, no git history in the archive the installers or GitHub's own
// "Download ZIP" button produce) is a few MiB; 128 MiB is generous headroom
// without being large enough to be a meaningful disk-fill vector on a small
// box.
const MaxSourceUploadSize = 128 << 20

// maxSourceExtractedSize caps the *decompressed* total a tgz may expand to —
// a gzip bomb is a handful of KiB on the wire and gigabytes off it, and the
// wire-side cap above does nothing to stop that.
//
// This is a bound on the copy, not a check run after each entry is written.
// The distinction is the whole value of the constant: the header's declared
// size is attacker-controlled and never verified against the actual stream,
// so limiting a copy to it and totting up afterwards let one entry write as
// much as it liked and reported the overrun once the bytes were already on
// disk. Both extractors now clamp each copy to whatever is left of the
// budget.
const maxSourceExtractedSize = 512 << 20

// maxSourceEntries caps how many files and directories an upload may create,
// which the byte ceiling above cannot do: a zero-length file costs no bytes
// and still costs an inode and a directory entry. Measured, an empty tar
// entry compresses to under five bytes on the wire, so a MaxSourceUploadSize
// upload could ask for roughly 27 million of them.
//
// The number is chosen against real trees, not against the attack. gravinet's
// own source archive is 690 files; a Go project that vendors heavily runs to
// a few tens of thousands. 100,000 is two orders of magnitude above what this
// project ships, comfortably above any vendored tree an operator might
// legitimately upload, and 270 times below what an upload can currently
// demand. If a real source tree ever hits it, the fix is to raise the number,
// not to remove the check — so the error says the limit out loud.
const maxSourceEntries = 100_000

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
// at MaxSourceUploadSize+1 bytes independent of whatever the caller's own
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
	n, copyErr := io.Copy(tmp, io.LimitReader(r, MaxSourceUploadSize+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > MaxSourceUploadSize {
		return "", fmt.Errorf("upload exceeds the %d-byte size ceiling", int64(MaxSourceUploadSize))
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

	// Normalized once, so the boundary check below compares against exactly
	// what filepath.Join will produce. An uncleaned destDir (a trailing
	// separator, say) would make the prefix test fail closed on every entry
	// rather than open, but "rejects every legitimate upload" is still a bug.
	destDir = filepath.Clean(destDir)

	tr := tar.NewReader(gz)
	var total int64
	var entries int
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

		// Counted after the type switch, so entries this extractor skips
		// entirely (device nodes and the like) do not spend budget — and
		// before anything is created, so the cap bounds what is made rather
		// than reporting on what was.
		entries++
		if entries > maxSourceEntries {
			return "", fmt.Errorf("upload contains more than %d files and directories", maxSourceEntries)
		}

		// The boundary, in two independent steps.
		//
		// filepath.IsLocal is the first, and does everything the hand-rolled
		// IsAbs/".."-prefix test it replaces did: it rejects absolute paths,
		// empty names, and anything that climbs out of the directory it is
		// relative to. It also covers two Windows cases that test silently
		// let through — volume-relative names like `C:x`, for which
		// filepath.IsAbs reports false, and reserved device names like
		// `NUL` — which matters because Windows is one of the platforms this
		// project builds for.
		//
		// Joining and re-checking the prefix is the second, and is not a
		// restatement of the first: it constrains the path actually about to
		// be opened, after Join has had its say, so nothing added between
		// here and the write can quietly move the boundary.
		//
		// filepath.Clean alone is not, and never was, the boundary — "../x"
		// cleans to itself. It only normalizes the name the two checks are
		// then applied to.
		name := filepath.Clean(hdr.Name)
		if name == "." {
			// The archive's own root ("./" or "."), which destDir already
			// is: nothing to create, and nothing that could be created here
			// without writing over the destination directory itself.
			continue
		}
		if !filepath.IsLocal(name) {
			return "", fmt.Errorf("refusing to extract %q: escapes the upload directory", hdr.Name)
		}
		target := filepath.Join(destDir, name)
		if !strings.HasPrefix(target, destDir+string(filepath.Separator)) {
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
		// The ceiling bounds the copy itself; it is not a check run
		// afterwards. Limiting to hdr.Size — a number the archive supplies
		// and nothing has verified — meant an entry declaring 64 MiB got
		// 64 MiB written to disk and only then had the cumulative total
		// consulted. Measured on a 128 MiB upload of compressible zeros
		// (1028:1), that was roughly 138 GB on disk for an upload inside
		// every wire-side cap. The limit is now the lesser of what the
		// header claims and what is left of the budget, plus the one byte
		// that detects going over.
		remaining := int64(maxSourceExtractedSize) - total
		limit := hdr.Size
		if limit > remaining {
			limit = remaining
		}
		n, err := io.Copy(f, io.LimitReader(tr, limit+1))
		f.Close()
		if err != nil {
			return "", err
		}
		if n > remaining {
			return "", fmt.Errorf("upload expands past the %d-byte extraction ceiling", int64(maxSourceExtractedSize))
		}
		if n != hdr.Size {
			return "", fmt.Errorf("%q: tar header claimed %d bytes, stream had at least %d", hdr.Name, hdr.Size, n)
		}
		total += n

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

	// Normalized once, for the same reason extractSourceTarGz does it.
	destDir = filepath.Clean(destDir)

	var total int64
	var entries int
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

		// Same entry ceiling as extractSourceTarGz, counted at the same
		// point in the loop.
		entries++
		if entries > maxSourceEntries {
			return "", fmt.Errorf("upload contains more than %d files and directories", maxSourceEntries)
		}

		// Same boundary as extractSourceTarGz — filepath.IsLocal on the
		// cleaned entry name, then a prefix check on the joined path —
		// applied to a zip entry's name instead of a tar header's. See its
		// comment for what each of the two steps is for, and for why
		// filepath.Clean is not itself the boundary.
		name := filepath.Clean(zf.Name)
		if name == "." {
			continue
		}
		if !filepath.IsLocal(name) {
			return "", fmt.Errorf("refusing to extract %q: escapes the upload directory", zf.Name)
		}
		target := filepath.Join(destDir, name)
		if !strings.HasPrefix(target, destDir+string(filepath.Separator)) {
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
		// zf.UncompressedSize64 is a uint64 straight out of the archive, so
		// it can be a value int64 cannot hold. The conversion would make it
		// negative, which the mismatch check below would catch, but by
		// accident rather than on purpose — say so here instead.
		if zf.UncompressedSize64 > math.MaxInt64 {
			return "", fmt.Errorf("%q: zip entry declares an impossible size", zf.Name)
		}
		declared := int64(zf.UncompressedSize64)
		// See extractSourceTarGz for why the ceiling bounds the copy rather
		// than being checked after it.
		remaining := int64(maxSourceExtractedSize) - total
		limit := declared
		if limit > remaining {
			limit = remaining
		}
		nCopied, copyErr := io.Copy(out, io.LimitReader(rc, limit+1))
		out.Close()
		rc.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if nCopied > remaining {
			return "", fmt.Errorf("upload expands past the %d-byte extraction ceiling", int64(maxSourceExtractedSize))
		}
		if nCopied != declared {
			return "", fmt.Errorf("%q: zip entry claimed %d bytes, stream had at least %d", zf.Name, declared, nCopied)
		}
		total += nCopied

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

// build runs `go build ./cmd/gravinet` against moduleRoot, mirroring
// install-linux.sh's build_from_source: try a cgo build first (needed for PAM
// web-admin auth), and if that specifically fails to compile, fall back to a
// static build rather than failing outright — a box without libpam headers
// still gets a working (if PAM-less) binary out of this, same as the
// installer. Returns the combined build output of whichever attempt failed
// (empty on success, where there is nothing to report).
//
// This does NOT attempt to install a Go toolchain or system packages if
// they're missing — that's what the platform installers' ensure_go/
// install_build_deps do, and doing the same thing from an HTTP handler would
// mean this node's web admin reaching out to a package manager and to the
// network to fetch a toolchain on its own initiative. That's a materially
// bigger and separate capability than "compile the source I already gave
// you", so it's out of scope here: if `go` can't be located at all (see
// locateGo above), this fails with a clear message instead, the same as the
// installer would if it couldn't obtain one either.
func build(ctx context.Context, moduleRoot, outPath string) (output string, err error) {
	goBin, err := locateGo()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, buildTimeout)
	defer cancel()

	// A self-contained cache/home under the build's own temp dir, rather
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

// truncateBuildOutput keeps a failed build's compiler output from ballooning
// into a multi-MiB error response — the tail is what almost always has the
// actual error, so keep that over the head.
func truncateBuildOutput(s string) string {
	const limit = 8 << 10
	if len(s) <= limit {
		return s
	}
	return "… (truncated) …\n" + s[len(s)-limit:]
}

// extractToTemp extracts src into a fresh temp directory under workRoot and
// returns that temp directory, the module root within it (the directory
// containing go.mod), and a cleanup func that removes the whole temp tree.
// Shared by Build, which extracts and then compiles (into a binary placed
// alongside — not inside — the extracted tree, via workDir), and
// ExtractedVersion, which extracts and stops there — reading the version
// costs a few file-system operations, not a multi-minute `go build`.
func extractToTemp(src io.Reader, workRoot, prefix string) (workDir, moduleRoot string, cleanup func(), err error) {
	workDir, err = os.MkdirTemp(workRoot, prefix)
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() { os.RemoveAll(workDir) }
	extractDir := filepath.Join(workDir, "src")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	modRoot, err := extractSourceArchive(src, extractDir)
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return workDir, modRoot, cleanup, nil
}

// Build extracts a gravinet source archive, compiles it with this node's own
// Go toolchain, and probes the result. It returns the path to the built
// binary, the extracted source tree's root (the directory containing
// go.mod — also where README.md/LICENSE/getting-started.md/docs/API.md
// live, for a caller that wants to refresh installed docs alongside the
// binary via SyncInstalledDocs), and a cleanup func the caller must invoke
// once it is done with both paths (typically after Apply has copied the
// binary next to the target and any doc sync has run — cleanup removes the
// whole extracted tree, moduleRoot included).
//
// This — not a downloaded binary — is the only way a new gravinet gets onto a
// node. The project ships no prebuilt release artifact for any platform, so
// source is what every operator actually has, and compiling on the node that
// will run it is what makes one archive work across a mesh of Linux, the
// BSDs, macOS and Windows boxes simultaneously. The binary produced here is
// native by construction: there is no os/arch negotiation to get wrong,
// because nothing platform-specific ever crossed a wire.
func Build(ctx context.Context, src io.Reader, workRoot string) (binPath, moduleRoot string, p Probe, cleanup func(), err error) {
	workDir, modRoot, cleanup, err := extractToTemp(src, workRoot, ".build-*")
	if err != nil {
		return "", "", Probe{}, func() {}, err
	}
	fail := func(e error) (string, string, Probe, func(), error) {
		cleanup()
		return "", "", Probe{}, func() {}, e
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
	buildOutput, err := build(ctx, modRoot, outPath)
	if err != nil {
		if buildOutput != "" {
			return fail(fmt.Errorf("%w\n\n%s", err, truncateBuildOutput(buildOutput)))
		}
		return fail(err)
	}

	probe, err := ProbeBinary(ctx, outPath)
	if err != nil {
		return fail(fmt.Errorf("built a binary but could not identify it: %w", err))
	}
	return outPath, modRoot, probe, cleanup, nil
}

// SourceVersion reads the version string baked into an extracted source tree
// (cmd/gravinet/main.go's `version = "NNN"` line) without building anything.
//
// This is the same line install-linux.sh's source_version() greps for, and it
// exists here for the same reason: it answers "what am I about to install?"
// before committing to a ten-minute compile.
func SourceVersion(moduleRoot string) string {
	b, err := os.ReadFile(filepath.Join(moduleRoot, "cmd", "gravinet", "main.go"))
	if err != nil {
		return ""
	}
	m := sourceVersionRe.FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

var sourceVersionRe = regexp.MustCompile(`(?m)^\s*version\s*=\s*"([^"]+)"`)

// ExtractedVersion answers SourceVersion's own question — "what am I about
// to install?" — straight from an unextracted archive, extracting just far
// enough to read cmd/gravinet/main.go and then discarding the tree, so a
// caller never pays for a `go build` merely to find out the candidate is the
// version already running. A Manager pushing to a fleet can show the target
// version up front this same way; controlOp's "apply" case (cmd/gravinet)
// is what actually uses this to skip the build entirely when the archive
// matches RunningVersion.
//
// Returns "" (never an error) when the version can't be read — a corrupt or
// unrecognized archive, say — so a caller can treat that the same as "unknown,
// proceed and let the real extraction inside Build report the actual
// problem" rather than needing a second error path here that would just
// duplicate Build's own.
func ExtractedVersion(src io.Reader, workRoot string) string {
	_, modRoot, cleanup, err := extractToTemp(src, workRoot, ".srcver-*")
	if err != nil {
		return ""
	}
	defer cleanup()
	return SourceVersion(modRoot)
}
