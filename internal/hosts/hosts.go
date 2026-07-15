// Package hosts maintains a gravinet-managed block inside the OS hosts file,
// mapping peer hostnames to their overlay addresses. It rewrites only its own
// delimited block, leaving the rest of the file untouched.
package hosts

import (
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// syncMu serializes Sync's read-modify-write of the (shared) hosts file. Multiple
// networks each manage their own delimited block in the same file, so without
// this their concurrent reads-then-writes could interleave and a stale write
// would drop another network's freshly-written block — which, combined with each
// network's content-signature debounce, would never get restored.
var syncMu sync.Mutex

// Entry maps a hostname to its overlay addresses. A node with both families
// yields a v4 line and a v6 line; with one family, a single line.
type Entry struct {
	Hostname string
	V4       netip.Addr
	V6       netip.Addr
}

func begin(tag string) string { return "# BEGIN gravinet " + tag }
func end(tag string) string   { return "# END gravinet " + tag }

// Render returns the contents of the hosts file with the managed block for tag
// replaced by entries (or removed entirely if entries is empty). Other content,
// including blocks for other tags, is preserved.
func Render(existing []byte, tag string, entries []Entry) []byte {
	lines := strings.Split(string(existing), "\n")
	var kept []string
	inBlock := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == begin(tag) {
			inBlock = true
			continue
		}
		if t == end(tag) {
			inBlock = false
			continue
		}
		if inBlock {
			continue // drop old managed lines
		}
		kept = append(kept, ln)
	}

	// Trim trailing blank lines from kept content.
	for len(kept) > 0 && strings.TrimSpace(kept[len(kept)-1]) == "" {
		kept = kept[:len(kept)-1]
	}

	var b bytes.Buffer
	if len(kept) > 0 {
		b.WriteString(strings.Join(kept, "\n"))
		b.WriteString("\n")
	}
	if len(entries) > 0 {
		block := renderBlock(tag, entries)
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(block)
	}
	return b.Bytes()
}

func renderBlock(tag string, entries []Entry) string {
	// Stable order so the file doesn't churn.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Hostname < entries[j].Hostname })
	var b strings.Builder
	b.WriteString(begin(tag))
	b.WriteString("\n")
	for _, e := range entries {
		if e.Hostname == "" {
			continue
		}
		if e.V4.IsValid() {
			fmt.Fprintf(&b, "%-15s %s\n", e.V4.String(), e.Hostname)
		}
		if e.V6.IsValid() {
			fmt.Fprintf(&b, "%-39s %s\n", e.V6.String(), e.Hostname)
		}
	}
	b.WriteString(end(tag))
	b.WriteString("\n")
	return b.String()
}

// Sync reads the hosts file at path, rewrites the managed block for tag, and
// writes it back atomically (where the platform allows).
// afterRead is a test seam invoked after Sync reads the file but before it writes
// back; it is nil in production. Tests use it to force the read-modify-write
// interleaving that the syncMu lock must prevent.
var afterRead func()

func Sync(path, tag string, entries []Entry) error {
	syncMu.Lock()
	defer syncMu.Unlock()
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if afterRead != nil {
		afterRead()
	}
	out := Render(existing, tag, entries)

	// Target permissions: keep the existing file's mode but guarantee the file
	// stays group/other-readable (0o044). os.CreateTemp makes 0600 files, and a
	// rename carries that mode onto the destination — which would leave
	// /etc/hosts unreadable to normal users. Default to 0644 for a new file.
	mode := os.FileMode(0o644)
	if fi, statErr := os.Stat(path); statErr == nil {
		mode = fi.Mode().Perm() | 0o044
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gravinet-hosts-*")
	if err != nil {
		// Fall back to in-place write (e.g. Windows ACLs on the temp create).
		return writeInPlace(path, out, mode)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Set the mode on the temp file before the rename so the destination ends up
	// world-readable rather than inheriting the temp file's 0600.
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		// Cross-device or Windows lock: fall back to direct write.
		return writeInPlace(path, out, mode)
	}
	return nil
}

// writeInPlace writes out to path and enforces mode, since os.WriteFile leaves an
// existing file's permissions untouched (so a previously-restrictive hosts file
// would stay unreadable otherwise).
func writeInPlace(path string, out []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, out, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
