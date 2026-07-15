// Package upgrade distributes and applies new gravinet binaries across a mesh.
//
// The problem this package exists to solve is not "how do I copy a file to ten
// machines" — scp does that. It is that gravinet *is* the network you would
// otherwise use to repair a machine you just broke. A bad binary pushed to ten
// peers takes down the very overlay you'd need to push the fix, and a node that
// is only reachable over the mesh it can no longer join is a node you drive to
// in a car. Everything here is shaped by that:
//
//   - Artifacts are signed (Ed25519) and verified by every node that receives
//     one, so compromising a Manager node is not sufficient to run code on the
//     fleet — you also need the release key, which lives offline. The mesh PSK
//     gets you onto the overlay; Manager mode lets you drive the admin API; but
//     neither lets you *replace the binary*. That is a third, separate boundary.
//   - Nothing is applied because it arrived. Advertisement, staging, and
//     applying are three distinct steps (see store.go, fetch.go, apply.go).
//   - The binary is exercised before it replaces anything (apply.go's preflight)
//     and the old one is kept, so a node can undo the swap on its own without
//     being told to (guard.go). A node that cannot reach the mesh cannot be
//     rescued by the mesh, so it must rescue itself.
//
// See docs/UPGRADES.md for the operator-facing version of all this.
package upgrade

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ManifestVersion is the schema tag that opens every signed byte string. It is
// inside the signature, so a future schema change cannot be downgraded onto an
// old verifier by an attacker stripping fields: v1 bytes only ever verify as v1.
const ManifestVersion = "gravinet-manifest-v1"

// Manifest describes one build artifact: a single gravinet binary for one
// os/arch, its digest, and a signature over all of the above.
//
// The signature covers the digest rather than the bytes, so a node can verify
// the manifest instantly (before deciding whether to spend bandwidth on the
// artifact at all) and verify the artifact itself by streaming it through
// SHA-256 as it arrives — no need to buffer 12 MiB in memory to check a sig.
type Manifest struct {
	Version string `json:"version"` // gravinet version string, e.g. "399"
	OS      string `json:"os"`      // GOOS, e.g. "linux"
	Arch    string `json:"arch"`    // GOARCH, e.g. "amd64"
	Size    int64  `json:"size"`    // artifact length in bytes
	SHA256  string `json:"sha256"`  // lowercase hex digest of the artifact
	Created string `json:"created"` // RFC3339, informational
	Notes   string `json:"notes,omitempty"`

	// PAM records whether this build has PAM compiled in (the "pam=yes|no"
	// field `gravinet version` prints). A node whose web admin authenticates
	// against PAM and which swaps in a pam=no binary still *starts* — it just
	// stops being able to log anyone in, which is precisely the class of
	// failure that looks fine to every automated health check and is discovered
	// by a human at 3am. Preflight compares this against the running binary and
	// refuses a silent downgrade unless explicitly allowed. See apply.go.
	PAM bool `json:"pam"`

	Signer    string `json:"signer"`    // hex-encoded Ed25519 public key (32 bytes)
	Signature string `json:"signature"` // base64 Ed25519 signature over signedBytes()
}

var (
	ErrUnsigned     = errors.New("upgrade: manifest is not signed")
	ErrBadSignature = errors.New("upgrade: manifest signature does not verify")
	ErrUntrusted    = errors.New("upgrade: manifest is signed by a key this node does not trust")
	ErrDigest       = errors.New("upgrade: artifact digest does not match the manifest")
	ErrNoTrustKeys  = errors.New("upgrade: no trusted release keys are configured")
)

// versionRe bounds what may appear in a version string. This is not idle
// paranoia about aesthetics: the version, os, and arch fields are concatenated
// into an artifact ID which becomes a *filename* in the store and a *query
// parameter* on the blob endpoint. An unconstrained version string is a path
// traversal waiting to happen ("../../etc/gravinet/config.json"). Constrain it
// at the type boundary, once, rather than trusting every later use to escape.
var (
	versionRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	osArchRe  = regexp.MustCompile(`^[a-z0-9]{1,16}$`)
	hexRe     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ID is the artifact's stable identity: what the store files it under, what a
// peer asks for over the wire, and what a rollout names. Deliberately filename-
// safe and free of any separator that means something to a path or a URL.
func (m Manifest) ID() string {
	return m.Version + "-" + m.OS + "-" + m.Arch
}

// Validate checks the structural invariants — everything except the signature,
// which needs a key set (see Verify). Called on every manifest that crosses a
// trust boundary: parsed from disk, decoded off the wire, or handed to Sign.
func (m Manifest) Validate() error {
	switch {
	case !versionRe.MatchString(m.Version):
		return fmt.Errorf("upgrade: invalid version %q", m.Version)
	case !osArchRe.MatchString(m.OS):
		return fmt.Errorf("upgrade: invalid os %q", m.OS)
	case !osArchRe.MatchString(m.Arch):
		return fmt.Errorf("upgrade: invalid arch %q", m.Arch)
	case m.Size <= 0:
		return fmt.Errorf("upgrade: invalid size %d", m.Size)
	case m.Size > MaxArtifactSize:
		return fmt.Errorf("upgrade: artifact of %d bytes exceeds the %d-byte ceiling", m.Size, int64(MaxArtifactSize))
	case !hexRe.MatchString(m.SHA256):
		return fmt.Errorf("upgrade: invalid sha256 %q", m.SHA256)
	case strings.ContainsAny(m.Notes, "\n\r"):
		// Newlines in Notes would forge extra lines in signedBytes and let two
		// different manifests produce identical signed bytes. Reject rather
		// than escape: nothing legitimate needs a multi-line note here.
		return errors.New("upgrade: notes may not contain newlines")
	}
	return nil
}

// MaxArtifactSize caps what this node will store, serve, or fetch. The gravinet
// binary is a few tens of MiB with the embedded wintun DLLs and xterm bundle;
// 256 MiB is generous headroom and still small enough that a peer cannot be
// talked into filling its disk by an advertisement claiming a 40 GiB "binary".
const MaxArtifactSize = 256 << 20

// signedBytes is the exact byte string an Ed25519 signature covers. It is a
// hand-rolled canonical encoding rather than JSON on purpose: encoding/json
// gives no cross-version guarantee of field order, escaping, or whitespace, and
// a signature over a non-canonical encoding is a signature over nothing. Fields
// are fixed-order, newline-terminated, and every value is constrained by
// Validate to characters that cannot forge a line break.
func (m Manifest) signedBytes() []byte {
	var b strings.Builder
	b.WriteString(ManifestVersion + "\n")
	b.WriteString("version=" + m.Version + "\n")
	b.WriteString("os=" + m.OS + "\n")
	b.WriteString("arch=" + m.Arch + "\n")
	b.WriteString("size=" + strconv.FormatInt(m.Size, 10) + "\n")
	b.WriteString("sha256=" + m.SHA256 + "\n")
	b.WriteString("pam=" + strconv.FormatBool(m.PAM) + "\n")
	b.WriteString("created=" + m.Created + "\n")
	b.WriteString("notes=" + m.Notes + "\n")
	b.WriteString("signer=" + m.Signer + "\n")
	return []byte(b.String())
}

// Sign fills in Signer/Signature. The caller owns the private key; this package
// never reads one from disk on a daemon's behalf, because a release key on a
// mesh node is a release key on ten mesh nodes. Signing is a build-host and
// laptop operation (`gravinet upgrade sign`), not a daemon one.
func (m *Manifest) Sign(priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return errors.New("upgrade: bad private key length")
	}
	m.Signer = hex.EncodeToString(priv.Public().(ed25519.PublicKey))
	m.Signature = "" // never sign over a previous signature
	if err := m.Validate(); err != nil {
		return err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.signedBytes()))
	return nil
}

// Verify checks the signature and that the signer is in trusted. An empty
// trust set is a hard error, never a pass: "no keys configured" must fail
// closed, or a node that simply hasn't been configured yet would accept
// anything the first peer offers it.
func (m Manifest) Verify(trusted []string) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if m.Signature == "" || m.Signer == "" {
		return ErrUnsigned
	}
	if len(trusted) == 0 {
		return ErrNoTrustKeys
	}
	pub, err := hex.DecodeString(m.Signer)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("upgrade: malformed signer key: %w", err)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil {
		return fmt.Errorf("upgrade: malformed signature: %w", err)
	}
	// Signature first, trust second. Checking membership before validity would
	// mean a node with no matching key returns a different error (and takes a
	// different amount of time) than one with a matching key and a bad sig,
	// which tells a prober which keys a node trusts. Neither check is skipped
	// either way, so the cost is one wasted verify on an untrusted key.
	sigOK := ed25519.Verify(pub, m.signedBytes(), sig)
	trustOK := false
	for _, t := range trusted {
		if strings.EqualFold(strings.TrimSpace(t), m.Signer) {
			trustOK = true
			break
		}
	}
	if !sigOK {
		return ErrBadSignature
	}
	if !trustOK {
		return fmt.Errorf("%w (signer %s)", ErrUntrusted, m.Signer[:16])
	}
	return nil
}

// MarshalJSON/parse helpers ---------------------------------------------------

// ParseManifest decodes and structurally validates a manifest. It deliberately
// does not verify the signature: callers differ in which trust set applies (a
// signing host has none), so verification is always an explicit second call.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("upgrade: parsing manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Bytes serializes the manifest for storage or the wire.
func (m Manifest) Bytes() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }

// DigestFile streams path through SHA-256 and returns (hex digest, size). Used
// both to build a manifest at sign time and to verify an artifact after a fetch
// — the same function on both ends, so a mismatch can never be an artifact of
// the two sides hashing differently.
func DigestFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// NewManifest builds an unsigned manifest for the artifact at path.
func NewManifest(path, version, goos, goarch string, pam bool, notes string) (Manifest, error) {
	sum, size, err := DigestFile(path)
	if err != nil {
		return Manifest{}, err
	}
	m := Manifest{
		Version: version,
		OS:      goos,
		Arch:    goarch,
		Size:    size,
		SHA256:  sum,
		PAM:     pam,
		Created: time.Now().UTC().Format(time.RFC3339),
		Notes:   notes,
	}
	return m, m.Validate()
}

// GenerateKey returns a fresh release keypair. The public half is what goes in
// every node's config (upgrade.trusted_keys); the private half is what does not.
func GenerateKey() (pubHex string, privB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	return hex.EncodeToString(pub), base64.StdEncoding.EncodeToString(priv), nil
}

// ParsePrivateKey decodes the base64 private key GenerateKey emitted.
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("upgrade: malformed private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("upgrade: private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}
