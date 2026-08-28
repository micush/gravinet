package ddns

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TSIG (RFC 8945): a shared-secret HMAC over the message, carried as an extra
// record in the additional section.
//
// Optional here, because a zone can equally be configured to accept updates
// from a list of addresses with no signature at all, and on a private network
// that is a legitimate choice an operator has already made in their DNS server.
// What is not optional is telling them apart in the log: an unsigned update to
// a zone that wants a key comes back REFUSED or NOTAUTH, which rcodeText spells
// out rather than leaving as a number.
//
// MD5 is deliberately absent. RFC 8945 lists hmac-md5 as an algorithm servers
// MAY support and this project has no reason to be the one that keeps it alive;
// every server that does dynamic updates does SHA-256, which is the default
// here and what a key generated today will be.

// TSIG algorithm names, as they appear on the wire — the algorithm is itself a
// domain name.
const (
	tsigSHA1   = "hmac-sha1"
	tsigSHA224 = "hmac-sha224"
	tsigSHA256 = "hmac-sha256"
	tsigSHA384 = "hmac-sha384"
	tsigSHA512 = "hmac-sha512"
)

// tsigFudge is how far apart the two clocks may be before a server rejects the
// signature. Five minutes is the near-universal default and matches what BIND,
// Knot and Technitium expect; a node further out than that is reported as
// BADTIME, which names the real problem.
const tsigFudge = 300

// Key is a TSIG key: a name the server knows, a secret, and the HMAC to use.
type Key struct {
	Name      string
	Secret    []byte
	Algorithm string
}

// hasher returns the HMAC constructor for this key's algorithm.
func (k Key) hasher() (func() hash.Hash, error) {
	switch k.Algorithm {
	case tsigSHA1:
		return sha1.New, nil
	case tsigSHA224:
		return sha256.New224, nil
	case tsigSHA256:
		return sha256.New, nil
	case tsigSHA384:
		return sha512.New384, nil
	case tsigSHA512:
		return sha512.New, nil
	}
	return nil, fmt.Errorf("unsupported TSIG algorithm %q: want one of hmac-sha1, hmac-sha224, hmac-sha256, hmac-sha384, hmac-sha512", k.Algorithm)
}

var (
	bindKeyName   = regexp.MustCompile(`(?s)key\s+"?([^"\s{]+)"?\s*\{`)
	bindAlgorithm = regexp.MustCompile(`(?s)algorithm\s+([^;]+);`)
	bindSecret    = regexp.MustCompile(`(?s)secret\s+"([^"]+)"`)
)

// ParseKey reads a TSIG key.
//
// The documented form, and the only one offered anywhere a user can see, is
// inline:
//
//	name:base64secret
//	name:base64secret:hmac-sha256
//
// A path to a BIND-style key file is still read if one is given, because
// configs in the field contain them and breaking those on upgrade would be a
// worse outcome than an undocumented form. It is no longer offered, and as of
// v1005 nothing in the settings card, the CLI usage or any error message
// mentions it.
//
// It was offered, and recommended, on the reasoning that a secret in a
// root-owned file is better protected than one in gravinet's config. True, and
// useless as advice, because gravinet cannot put a file on its own host: there
// is no upload and no editor, and the one route that could write one — the
// remote shell — is off by default and transcribes everything typed into it,
// so following the recommendation through gravinet moved the secret out of a
// redacted config field and into a plaintext log. A form most operators had no
// way to use does not belong in the help for a field with one box.
//
// Empty input is not an error — it means no key, which is a supported
// configuration. The caller distinguishes them by the returned pointer.
//
// Use ParseInlineKey, not this, for anything an operator just typed. Only a
// value already in the config on disk reaches the file branch here.
func ParseKey(spec string) (*Key, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if looksLikePath(spec) {
		return parseKeyFile(spec)
	}
	return parseInline(spec)
}

// ParseInlineKey is ParseKey without the file branch: it validates a key an
// operator supplied, and never touches the filesystem.
//
// Every setter uses this — the API handler, the CLI. The distinction is the
// whole point. A path in the config was put there by somebody who could write
// the config, which on this node means root; a path in a request was typed by
// a web session, which is a lower bar, and letting one name a file to open
// hands the session a filesystem probe it should not have. That the session is
// an authenticated administrator is not the answer: it can already read the
// config and upload a certificate, but "can do administrative things" is not
// "can make the daemon open any path on the host as root", and the gap between
// those is worth keeping. It also costs nothing, because as of v1006 nothing
// offers the file form to anybody in the first place.
//
// So a path is refused here with the form that is documented, which is also
// what somebody who typed one deserves to be told.
func ParseInlineKey(spec string) (*Key, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	if looksLikePath(spec) {
		return nil, fmt.Errorf("%s is not a TSIG key: give the key as name:base64secret[:algorithm]", spec)
	}
	return parseInline(spec)
}

// parseInline splits on the first two colons only: base64 has no colon, but a
// name conceivably could, and the algorithm never does.
func parseInline(spec string) (*Key, error) {
	parts := strings.SplitN(spec, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("TSIG key must be name:base64secret[:algorithm]")
	}
	k := Key{Name: strings.TrimSpace(parts[0]), Algorithm: tsigSHA256}
	if len(parts) == 3 {
		k.Algorithm = normalizeAlgorithm(parts[2])
	}
	secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		return nil, fmt.Errorf("TSIG secret is not valid base64: %v", err)
	}
	k.Secret = secret
	return finishKey(k)
}

// looksLikePath reports whether a spec was evidently meant as a filename, so a
// stat that failed can be reported as a missing file rather than as bad inline
// syntax.
//
// Deliberately decided on shape alone, with no second stat: the file has
// already been looked for and was not there, and the only question left is
// which error to print.
//
// A separator anywhere, or a leading ~ or . — none of which can occur in the
// inline form, where the name is a DNS label, the secret is base64 and the
// algorithm comes from a fixed list. A Windows path needs no rule of its own:
// "C:\keys\tsig.key" and "C:/keys/tsig.key" both carry a separator. Testing
// the drive letter directly was the obvious alternative and is wrong — it
// cannot be told from an inline key whose name is one character, which is
// unusual but perfectly legal, and misreading one as a filename would report
// a missing file to someone holding a valid key.
func looksLikePath(spec string) bool {
	return strings.ContainsAny(spec, `/\`) ||
		strings.HasPrefix(spec, "~") || strings.HasPrefix(spec, ".")
}

// maxKeyFile caps the read. A BIND key file is a few hundred bytes; this is
// four orders of magnitude of headroom and still bounds what a config pointing
// at /dev/zero can do to a daemon that reads it on a timer.
const maxKeyFile = 1 << 20

func parseKeyFile(path string) (*Key, error) {
	// Absolute only. A relative path resolves against the daemon's working
	// directory, which nothing here controls and which differs between a unit
	// start and a shell, so the same config would read different files.
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("TSIG key file %s must be an absolute path", path)
	}
	// Regular files only, checked before opening rather than after. A FIFO
	// blocks in open() until something writes, which would hang registration
	// holding whatever the caller holds; a character device may never end.
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s is not a TSIG key: give the key as name:base64secret[:algorithm]", path)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("TSIG key file %s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading TSIG key file: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFile))
	if err != nil {
		return nil, fmt.Errorf("reading TSIG key file: %w", err)
	}
	body := string(b)
	nameM := bindKeyName.FindStringSubmatch(body)
	secretM := bindSecret.FindStringSubmatch(body)
	if nameM == nil || secretM == nil {
		return nil, fmt.Errorf("%s does not look like a BIND key file (no key name or secret found)", path)
	}
	k := Key{Name: nameM[1], Algorithm: tsigSHA256}
	if algoM := bindAlgorithm.FindStringSubmatch(body); algoM != nil {
		k.Algorithm = normalizeAlgorithm(algoM[1])
	}
	secret, err := base64.StdEncoding.DecodeString(strings.TrimSpace(secretM[1]))
	if err != nil {
		return nil, fmt.Errorf("TSIG secret in %s is not valid base64: %v", path, err)
	}
	k.Secret = secret
	return finishKey(k)
}

func finishKey(k Key) (*Key, error) {
	if k.Name == "" {
		return nil, fmt.Errorf("TSIG key has no name")
	}
	if len(k.Secret) == 0 {
		return nil, fmt.Errorf("TSIG key %q has an empty secret", k.Name)
	}
	if _, err := k.hasher(); err != nil {
		return nil, err
	}
	return &k, nil
}

// normalizeAlgorithm accepts the spellings that turn up in key files and in
// documentation — "hmac-sha256", "HMAC-SHA256", "sha256", "hmac_sha256" — and
// returns the wire name. An unrecognised value is passed through so hasher can
// name it in the error rather than this silently substituting a default the
// operator did not ask for.
func normalizeAlgorithm(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.NewReplacer("_", "", "-", "", " ", "").Replace(t)
	t = strings.TrimPrefix(t, "hmac")
	switch t {
	case "sha1":
		return tsigSHA1
	case "sha224":
		return tsigSHA224
	case "sha256":
		return tsigSHA256
	case "sha384":
		return tsigSHA384
	case "sha512":
		return tsigSHA512
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// sign appends a TSIG record to msg and bumps ARCOUNT.
//
// The MAC covers the message as it stands followed by the TSIG *variables* —
// the signed fields, in a defined order, and not the record header around them
// (RFC 8945 §4.3.3). Getting that boundary wrong produces a message every
// server rejects with BADSIG and no other symptom, which is why the variables
// are assembled once here and the rdata is built from the same values rather
// than re-derived.
func sign(msg []byte, id uint16, k Key, now time.Time) ([]byte, error) {
	newHash, err := k.hasher()
	if err != nil {
		return nil, err
	}
	keyName, err := encodeName(k.Name)
	if err != nil {
		return nil, fmt.Errorf("TSIG key name: %w", err)
	}
	algName, err := encodeName(k.Algorithm)
	if err != nil {
		return nil, fmt.Errorf("TSIG algorithm: %w", err)
	}
	signed := now.Unix()

	// The signed variables: key name, class ANY, TTL 0, algorithm, time, fudge,
	// error, other-len. Time is 48 bits — a 16-bit high half and a 32-bit low
	// half — which is the one field here that does not look like anything else
	// in a DNS message.
	var vars []byte
	vars = append(vars, keyName...)
	vars = binary.BigEndian.AppendUint16(vars, classAny)
	vars = binary.BigEndian.AppendUint32(vars, 0) // TTL
	vars = append(vars, algName...)
	vars = binary.BigEndian.AppendUint16(vars, uint16(signed>>32))
	vars = binary.BigEndian.AppendUint32(vars, uint32(signed))
	vars = binary.BigEndian.AppendUint16(vars, tsigFudge)
	vars = binary.BigEndian.AppendUint16(vars, 0) // error
	vars = binary.BigEndian.AppendUint16(vars, 0) // other len

	mac := hmac.New(newHash, k.Secret)
	mac.Write(msg)
	mac.Write(vars)
	sum := mac.Sum(nil)

	var rdata []byte
	rdata = append(rdata, algName...)
	rdata = binary.BigEndian.AppendUint16(rdata, uint16(signed>>32))
	rdata = binary.BigEndian.AppendUint32(rdata, uint32(signed))
	rdata = binary.BigEndian.AppendUint16(rdata, tsigFudge)
	rdata = binary.BigEndian.AppendUint16(rdata, uint16(len(sum)))
	rdata = append(rdata, sum...)
	rdata = binary.BigEndian.AppendUint16(rdata, id) // original id
	rdata = binary.BigEndian.AppendUint16(rdata, 0)  // error
	rdata = binary.BigEndian.AppendUint16(rdata, 0)  // other len

	out := append([]byte{}, msg...)
	out = append(out, keyName...)
	out = binary.BigEndian.AppendUint16(out, typeTSIG)
	out = binary.BigEndian.AppendUint16(out, classAny)
	out = binary.BigEndian.AppendUint32(out, 0) // TTL 0: never cached
	out = binary.BigEndian.AppendUint16(out, uint16(len(rdata)))
	out = append(out, rdata...)

	// ARCOUNT is the last field of the header and has to count the record just
	// appended. Done after signing, because the MAC covers the message with the
	// count it had — the server increments its own copy the same way before
	// verifying (RFC 8945 §5.3.2).
	binary.BigEndian.PutUint16(out[10:12], binary.BigEndian.Uint16(out[10:12])+1)
	return out, nil
}
