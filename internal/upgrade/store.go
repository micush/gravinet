package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Store is a node's staging area for upgrade artifacts: a flat directory of
// verified (or, in local-only-unsigned mode, structurally validated) binaries
// and their manifests, plus the guard state file (guard.go).
//
// Trust is decided in exactly one place — Verify(), below — so every later
// step (serving to a peer, preflighting, swapping) can treat the store as
// trusted input and does not have to re-litigate provenance itself. There is
// exactly one door into this directory (Ingest) and it always goes through
// verify() first.
type Store struct {
	dir     string
	trusted []string // hex Ed25519 public keys; empty means unsigned artifacts are accepted (local-only mode — see Verify)

	mu sync.Mutex // serializes ingest/remove against each other
}

// NewStore opens (creating if needed) the artifact directory. 0700: the store
// holds binaries this node will *execute as root*, so a directory any local
// user can write to is a local privilege escalation with extra steps.
func NewStore(dir string, trusted []string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("upgrade: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("upgrade: creating store %s: %w", dir, err)
	}
	clean := make([]string, 0, len(trusted))
	for _, t := range trusted {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			clean = append(clean, t)
		}
	}
	return &Store{dir: dir, trusted: clean}, nil
}

// Dir is the store's root, for callers that need to place a sibling file.
func (s *Store) Dir() string { return s.dir }

// Trusted reports the configured release keys (a copy; the caller must not be
// able to widen this node's trust set by mutating the slice it was handed).
func (s *Store) Trusted() []string { return append([]string(nil), s.trusted...) }

// paths for an artifact id. id comes from Manifest.ID(), whose components are
// regexp-constrained in Validate — but this is where a bad id would become a
// filesystem operation, so it re-checks rather than assuming every caller
// validated first. Defense in depth costs one regexp match.
func (s *Store) paths(m Manifest) (bin, man string, err error) {
	if err := m.Validate(); err != nil {
		return "", "", err
	}
	id := m.ID()
	return filepath.Join(s.dir, id+".bin"), filepath.Join(s.dir, id+".json"), nil
}

// Verify applies this store's trust policy to a manifest. With trusted_keys
// configured, it's exactly the old behavior: a valid signature from one of
// those keys, no exceptions. With none configured — the local-only-unsigned
// setup — there is no key to check a signature against, so a manifest only
// needs to be structurally sound (Validate): correct fields, a digest that
// looks like a digest, nothing that could forge a path or a signed line later.
// This is safe specifically because Ingest is the *only* door into the store
// and upgrades are local-only, full stop, under any configuration — so the
// only thing that can ever reach this path is a session already
// authenticated as this exact node's own local admin, not a peer.
// Centralized here, once, so Ingest, List, and the web admin's early-reject
// check (before it spends bandwidth on an artifact upload) can never drift
// out of agreement about what "trusted" means.
func (s *Store) Verify(m Manifest) error {
	if len(s.trusted) == 0 {
		return m.Validate()
	}
	return m.Verify(s.trusted)
}

// Ingest verifies and stores an artifact. src streams the binary; m describes
// it. The manifest signature is checked against the trust set, then the bytes
// are hashed *as they are written* and compared to the manifest — so a source
// that serves a manifest for the binary you wanted and bytes for a different
// one is caught here, not at exec time.
//
// The write is staged to a temp file and renamed into place only after both
// checks pass, so a torn download or a killed process can never leave a
// half-written binary sitting where a later apply would find it and run it.
func (s *Store) Ingest(m Manifest, src io.Reader) error {
	if err := s.Verify(m); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	bin, manPath, err := s.paths(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".ingest-*")
	if err != nil {
		return fmt.Errorf("upgrade: staging temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Any early return from here on must not leave the temp file behind.
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op once renamed away
	}()

	h := sha256.New()
	// LimitReader at Size+1: a source that keeps sending past the declared
	// length is either buggy or hostile, and either way we want to notice
	// rather than fill the disk. The +1 makes "sent more than promised"
	// detectable instead of silently truncating to exactly the right length.
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, m.Size+1))
	if err != nil {
		return fmt.Errorf("upgrade: writing artifact: %w", err)
	}
	if n != m.Size {
		return fmt.Errorf("upgrade: artifact is %d bytes, manifest says %d", n, m.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != m.SHA256 {
		return fmt.Errorf("%w: got %s, want %s", ErrDigest, got[:16], m.SHA256[:16])
	}
	// fsync before rename: a crash between rename and writeback would otherwise
	// leave a correctly-named file full of zeroes, which is exactly the artifact
	// we spent this whole function proving we did not have.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("upgrade: syncing artifact: %w", err)
	}
	if err := tmp.Chmod(0o700); err != nil {
		return fmt.Errorf("upgrade: chmod artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("upgrade: closing artifact: %w", err)
	}
	mb, err := m.Bytes()
	if err != nil {
		return err
	}
	if err := os.WriteFile(manPath, mb, 0o600); err != nil {
		return fmt.Errorf("upgrade: writing manifest: %w", err)
	}
	if err := os.Rename(tmpName, bin); err != nil {
		os.Remove(manPath) // don't leave a manifest advertising an artifact we don't have
		return fmt.Errorf("upgrade: installing artifact: %w", err)
	}
	return nil
}

// Have reports whether a verified artifact for m is already on disk. It checks
// the recorded size, not just existence: a truncated file left by a full disk
// should look absent (and be re-fetched), not present-but-broken.
func (s *Store) Have(m Manifest) bool {
	bin, _, err := s.paths(m)
	if err != nil {
		return false
	}
	fi, err := os.Stat(bin)
	return err == nil && fi.Mode().IsRegular() && fi.Size() == m.Size
}

// Open returns a reader for the stored artifact.
func (s *Store) Open(m Manifest) (*os.File, error) {
	bin, _, err := s.paths(m)
	if err != nil {
		return nil, err
	}
	return os.Open(bin)
}

// BinPath is the on-disk path of the stored artifact (what apply.go swaps in).
func (s *Store) BinPath(m Manifest) (string, error) {
	bin, _, err := s.paths(m)
	return bin, err
}

// Lookup finds a stored manifest by artifact id. Returns ok=false if absent or
// if what's on disk no longer verifies — a manifest whose trust key has since
// been removed from the config must stop being usable immediately, not linger
// because it verified when it was written.
func (s *Store) Lookup(id string) (Manifest, bool) {
	for _, m := range s.List() {
		if m.ID() == id {
			return m, true
		}
	}
	return Manifest{}, false
}

// List returns every artifact currently staged, verified, and complete, newest
// version first. Anything that fails to parse, fails to verify, or has no
// matching binary is skipped silently: List is called on the request path (the
// UI's "what can I roll out" list) and one corrupt file should degrade to one
// missing row, not an error page.
func (s *Store) List() []Manifest {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []Manifest
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == stateFile {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		m, err := ParseManifest(b)
		if err != nil || s.Verify(m) != nil || !s.Have(m) {
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Version != out[j].Version {
			return VersionLess(out[j].Version, out[i].Version) // newest first
		}
		return out[i].ID() < out[j].ID()
	})
	return out
}

// Remove deletes an artifact and its manifest.
func (s *Store) Remove(m Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bin, man, err := s.paths(m)
	if err != nil {
		return err
	}
	err1 := os.Remove(bin)
	err2 := os.Remove(man)
	if err1 != nil && !os.IsNotExist(err1) {
		return err1
	}
	if err2 != nil && !os.IsNotExist(err2) {
		return err2
	}
	return nil
}

// GC keeps the newest keep artifacts and deletes the rest. A node that has been
// upgraded twenty times should not be storing twenty binaries; the *previous*
// one that matters for rollback is not in here anyway — it's the .prev file
// next to the running binary (see apply.go), which GC never touches.
func (s *Store) GC(keep int) int {
	if keep < 0 {
		keep = 0
	}
	all := s.List()
	if len(all) <= keep {
		return 0
	}
	n := 0
	for _, m := range all[keep:] {
		if s.Remove(m) == nil {
			n++
		}
	}
	return n
}

// VersionLess orders gravinet version strings. They are a monotonically
// increasing integer counter ("398", "399"), so the fast path is numeric — but
// this must not *assume* that, because a hand-built binary carrying "1.2.0-rc1"
// or a git describe string would otherwise sort nonsensically and, worse, defeat
// the downgrade guard in rollout.go (which asks exactly this question before it
// agrees to replace a binary). Numeric when both sides are numeric; lexical
// otherwise, which is at least stable and total.
func VersionLess(a, b string) bool {
	an, aok := atoiStrict(a)
	bn, bok := atoiStrict(b)
	if aok && bok {
		return an < bn
	}
	return a < b
}

func atoiStrict(s string) (int64, bool) {
	if s == "" || len(s) > 18 {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n, true
}
