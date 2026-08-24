package upgrade

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// tgzWith builds a gzip-compressed tar containing the given entries, plus a
// go.mod and a cmd/gravinet directory so extraction would otherwise succeed.
func tgzWith(t *testing.T, entries ...tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		hdr := h
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			if _, err := tw.Write(make([]byte, hdr.Size)); err != nil {
				t.Fatal(err)
			}
		}
	}
	mod := []byte("module gravinet\n")
	if err := tw.WriteHeader(&tar.Header{Name: "src/go.mod", Mode: 0o644, Size: int64(len(mod)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(mod)
	tw.WriteHeader(&tar.Header{Name: "src/cmd/gravinet", Mode: 0o755, Typeflag: tar.TypeDir})
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// zipWith is tgzWith's zip counterpart.
func zipWith(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("payload"))
	}
	w, _ := zw.Create("src/go.mod")
	w.Write([]byte("module gravinet\n"))
	zw.Create("src/cmd/gravinet/")
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// escapeNames are the shapes a hostile or merely badly-built archive uses to
// try to write outside the extraction directory.
var escapeNames = []string{
	"../escaped",
	"../../escaped",
	"../../../../../../../../tmp/escaped",
	"/etc/passwd",
	"/tmp/absolute",
	"a/../../escaped",
	"a/b/../../../escaped",
	"..",
	"./../escaped",
}

// TestTarGzRefusesEscapingEntries is the tar half of the reported finding.
func TestTarGzRefusesEscapingEntries(t *testing.T) {
	for _, name := range escapeNames {
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		data := tgzWith(t, tar.Header{Name: name, Mode: 0o644, Size: 5, Typeflag: tar.TypeReg})
		if _, err := extractSourceTarGz(bytes.NewReader(data), dest); err == nil {
			t.Errorf("entry %q was extracted; want a refusal", name)
		}
		assertNothingOutside(t, root, dest, name)
	}
}

// TestZipRefusesEscapingEntries is the zip half — the flagged function.
func TestZipRefusesEscapingEntries(t *testing.T) {
	for _, name := range escapeNames {
		root := t.TempDir()
		dest := filepath.Join(root, "dest")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		data := zipWith(t, name)
		if _, err := extractSourceZip(bytes.NewReader(data), int64(len(data)), dest); err == nil {
			t.Errorf("entry %q was extracted; want a refusal", name)
		}
		assertNothingOutside(t, root, dest, name)
	}
}

// assertNothingOutside is the property that actually matters, and it is
// checked separately from the error: an entry could in principle be reported
// as refused after something had already been written.
func assertNothingOutside(t *testing.T, root, dest, name string) {
	t.Helper()
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, dest+string(filepath.Separator)) {
			t.Errorf("entry %q caused a write to %s, outside %s", name, p, dest)
		}
		return nil
	})
}

// TestExtractorsRefuseSymlinks pins the check that makes the name-based
// boundary above sufficient. A symlink is how an archive escapes a path check
// that only inspects entry names: extract "link -> /etc", then "link/passwd".
func TestExtractorsRefuseSymlinks(t *testing.T) {
	dest := t.TempDir()
	data := tgzWith(t, tar.Header{Name: "src/link", Linkname: "/etc", Typeflag: tar.TypeSymlink})
	if _, err := extractSourceTarGz(bytes.NewReader(data), dest); err == nil {
		t.Error("a tar symlink entry was accepted")
	}
	dest2 := t.TempDir()
	data2 := tgzWith(t, tar.Header{Name: "src/hard", Linkname: "src/go.mod", Typeflag: tar.TypeLink})
	if _, err := extractSourceTarGz(bytes.NewReader(data2), dest2); err == nil {
		t.Error("a tar hard link entry was accepted")
	}
}

// TestTarGzCeilingBoundsTheCopy is the regression test for the bug found
// alongside the alert.
//
// The ceiling used to be a check run after each entry was fully written, with
// the copy limited to the size the header declared — a number the archive
// supplies and nothing verifies. So one entry wrote as much as it claimed and
// the overrun was reported once the bytes were already on disk. Measured at
// the shipped constants, a 128 MiB upload of compressible zeros (1028:1)
// could put roughly 138 GB on the disk.
//
// The assertion is about bytes on disk, not about the error: the old code
// returned the same error this one does.
func TestTarGzCeilingBoundsTheCopy(t *testing.T) {
	over := int64(maxSourceExtractedSize) + (8 << 20)
	dest := t.TempDir()
	data := tgzWith(t, tar.Header{Name: "src/big.bin", Mode: 0o644, Size: over, Typeflag: tar.TypeReg})
	t.Logf("archive is %d bytes on the wire, declaring %d", len(data), over)

	if _, err := extractSourceTarGz(bytes.NewReader(data), dest); err == nil {
		t.Fatal("an entry past the extraction ceiling was accepted")
	}
	if n := bytesOnDisk(t, dest); n > int64(maxSourceExtractedSize)+1 {
		t.Errorf("wrote %d bytes for a %d-byte ceiling; the copy must be bounded by the budget, not by the declared size", n, int64(maxSourceExtractedSize))
	}
}

// TestZipCeilingBoundsTheCopy is the same for the zip path. Built by hand
// rather than through zipWith so the entry can be large.
func TestZipCeilingBoundsTheCopy(t *testing.T) {
	over := int64(maxSourceExtractedSize) + (8 << 20)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("src/big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(make([]byte, over)); err != nil {
		t.Fatal(err)
	}
	m, _ := zw.Create("src/go.mod")
	m.Write([]byte("module gravinet\n"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	t.Logf("archive is %d bytes on the wire, entry is %d", len(data), over)

	dest := t.TempDir()
	if _, err := extractSourceZip(bytes.NewReader(data), int64(len(data)), dest); err == nil {
		t.Fatal("an entry past the extraction ceiling was accepted")
	}
	if n := bytesOnDisk(t, dest); n > int64(maxSourceExtractedSize)+1 {
		t.Errorf("wrote %d bytes for a %d-byte ceiling; the copy must be bounded by the budget, not by the declared size", n, int64(maxSourceExtractedSize))
	}
}

// TestCeilingIsCumulativeAcrossEntries guards the obvious wrong fix for the
// above: clamping each copy to the whole ceiling rather than to what is left
// of it, which bounds any one entry and nothing else.
func TestCeilingIsCumulativeAcrossEntries(t *testing.T) {
	const each = 96 << 20
	n := int(int64(maxSourceExtractedSize)/each) + 2
	hdrs := make([]tar.Header, 0, n)
	for i := 0; i < n; i++ {
		hdrs = append(hdrs, tar.Header{
			Name: fmt.Sprintf("src/chunk%02d.bin", i), Mode: 0o644,
			Size: each, Typeflag: tar.TypeReg,
		})
	}
	dest := t.TempDir()
	data := tgzWith(t, hdrs...)
	if _, err := extractSourceTarGz(bytes.NewReader(data), dest); err == nil {
		t.Fatalf("%d entries of %d bytes each were accepted past a %d-byte ceiling", n, int64(each), int64(maxSourceExtractedSize))
	}
	if got := bytesOnDisk(t, dest); got > int64(maxSourceExtractedSize)+1 {
		t.Errorf("wrote %d bytes across entries for a %d-byte ceiling", got, int64(maxSourceExtractedSize))
	}
}

// TestHonestArchiveStillExtracts is the guard against fixing the ceiling by
// breaking extraction. A normal source tree must come out intact, with every
// byte present.
func TestHonestArchiveStillExtracts(t *testing.T) {
	dest := t.TempDir()
	data := tgzWith(t,
		tar.Header{Name: "src", Mode: 0o755, Typeflag: tar.TypeDir},
		tar.Header{Name: "src/cmd", Mode: 0o755, Typeflag: tar.TypeDir},
		tar.Header{Name: "src/main.go", Mode: 0o644, Size: 1024, Typeflag: tar.TypeReg},
		tar.Header{Name: "src/internal/thing.go", Mode: 0o644, Size: 4096, Typeflag: tar.TypeReg},
	)
	root, err := extractSourceTarGz(bytes.NewReader(data), dest)
	if err != nil {
		t.Fatalf("an ordinary source archive was refused: %v", err)
	}
	if filepath.Base(root) != "src" {
		t.Errorf("module root is %q; want the directory holding go.mod", root)
	}
	for name, want := range map[string]int64{
		"src/main.go":           1024,
		"src/internal/thing.go": 4096,
		"src/go.mod":            16,
	} {
		fi, err := os.Stat(filepath.Join(dest, name))
		if err != nil {
			t.Errorf("%s missing after extraction: %v", name, err)
			continue
		}
		if fi.Size() != want {
			t.Errorf("%s is %d bytes; want %d — the copy limit must not truncate an honest entry", name, fi.Size(), want)
		}
	}
}

// TestSizeMismatchStillReported keeps the other half of the copy limit
// working: an entry whose stream is shorter than its header claims is a
// corrupt or crafted archive, and must not be extracted silently.
func TestSizeMismatchStillReported(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	// Declare 4096, supply nothing: tar pads, so the reader sees a short entry.
	tw.WriteHeader(&tar.Header{Name: "src/short.go", Mode: 0o644, Size: 4096, Typeflag: tar.TypeReg})
	tw.Write(make([]byte, 10))
	tw.Flush()
	tw.Close()
	gz.Close()

	dest := t.TempDir()
	_, err := extractSourceTarGz(bytes.NewReader(buf.Bytes()), dest)
	if err == nil {
		t.Fatal("an entry shorter than its declared size was accepted")
	}
	if !strings.Contains(err.Error(), "claimed") && !strings.Contains(err.Error(), "tar") {
		t.Logf("refused with: %v", err)
	}
}

func bytesOnDisk(t *testing.T, dir string) int64 {
	t.Helper()
	var total int64
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// TestEntryCeilingBoundsFileCount closes the gap v924 flagged and deferred.
//
// The byte ceiling cannot bound this: a zero-length entry costs no bytes and
// still costs an inode and a directory entry. Measured before the cap, an
// empty tar entry compressed to under five bytes on the wire, so a
// MaxSourceUploadSize upload could ask for roughly 27 million files.
func TestEntryCeilingBoundsFileCount(t *testing.T) {
	hdrs := make([]tar.Header, 0, maxSourceEntries+10)
	for i := 0; i < maxSourceEntries+10; i++ {
		hdrs = append(hdrs, tar.Header{
			Name: fmt.Sprintf("src/d/f%08d", i), Mode: 0o644, Typeflag: tar.TypeReg,
		})
	}
	dest := t.TempDir()
	data := tgzWith(t, hdrs...)
	t.Logf("%d entries in %d wire bytes (%.2f B/entry)", len(hdrs), len(data), float64(len(data))/float64(len(hdrs)))

	_, err := extractSourceTarGz(bytes.NewReader(data), dest)
	if err == nil {
		t.Fatalf("%d entries were accepted past a %d-entry ceiling", len(hdrs), maxSourceEntries)
	}
	if !strings.Contains(err.Error(), "files and directories") {
		t.Errorf("refused with %v; want the entry-ceiling message, which names the limit", err)
	}
	if n := filesOnDisk(t, dest); n > maxSourceEntries {
		t.Errorf("created %d files for a %d-entry ceiling", n, maxSourceEntries)
	}
}

// TestZipEntryCeilingBoundsFileCount is the zip half. Kept small — the point
// is that the check exists on this path too, not to re-measure the ratio.
func TestZipEntryCeilingBoundsFileCount(t *testing.T) {
	names := make([]string, 0, maxSourceEntries+10)
	for i := 0; i < maxSourceEntries+10; i++ {
		names = append(names, fmt.Sprintf("src/d/f%08d", i))
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		if _, err := zw.Create(n); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()

	dest := t.TempDir()
	if _, err := extractSourceZip(bytes.NewReader(data), int64(len(data)), dest); err == nil {
		t.Fatalf("%d zip entries were accepted past a %d-entry ceiling", len(names), maxSourceEntries)
	}
	if n := filesOnDisk(t, dest); n > maxSourceEntries {
		t.Errorf("created %d files for a %d-entry ceiling", n, maxSourceEntries)
	}
}

// TestEntryCeilingLeavesRealTreesAlone is the other half. gravinet's own
// source archive is under a thousand files; the cap must be nowhere near
// anything a real upload contains, including a heavily vendored tree.
func TestEntryCeilingLeavesRealTreesAlone(t *testing.T) {
	const realistic = 60000 // a Go project vendoring heavily
	if realistic >= maxSourceEntries {
		t.Fatalf("the entry ceiling (%d) is not clear of a realistic vendored tree (%d)", maxSourceEntries, realistic)
	}
	hdrs := make([]tar.Header, 0, realistic)
	for i := 0; i < realistic; i++ {
		hdrs = append(hdrs, tar.Header{
			Name: fmt.Sprintf("src/vendor/p%04d/f%04d.go", i/100, i%100), Mode: 0o644, Typeflag: tar.TypeReg,
		})
	}
	dest := t.TempDir()
	data := tgzWith(t, hdrs...)
	if _, err := extractSourceTarGz(bytes.NewReader(data), dest); err != nil {
		t.Fatalf("a %d-file source tree was refused: %v", realistic, err)
	}
}

func filesOnDisk(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			n++
		}
		return nil
	})
	return n
}
