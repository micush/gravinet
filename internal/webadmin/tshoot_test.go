package webadmin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strings"
	"testing"
	"time"
)

// The bundle exists to be handed to someone else, so the redaction is the part
// that has to be right. A missed secret is unrecoverable once the file has been
// sent; a false positive costs one unreadable line in a diagnostic.
func TestRedactConfigRemovesSecrets(t *testing.T) {
	src := `# gravinet config
node_id: gn-debian
listen_port: 65432
private_key: aGVsbG8gd29ybGQgdGhpcyBpcyBzZWNyZXQ=
webadmin:
  password: hunter2
  api_token = deadbeefcafe
keys:
  - AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKK=
  - key0
peer_timeout: 30
`
	got := redactConfig(src)

	for _, leaked := range []string{
		"aGVsbG8gd29ybGQgdGhpcyBpcyBzZWNyZXQ=",
		"hunter2",
		"deadbeefcafe",
		"AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKK=",
	} {
		if strings.Contains(got, leaked) {
			t.Errorf("secret survived redaction: %q\n\n%s", leaked, got)
		}
	}

	// Structure and non-secrets must survive, or the config section is useless
	// for the misconfigurations it exists to catch.
	for _, keep := range []string{"node_id: gn-debian", "listen_port: 65432", "peer_timeout: 30", "# gravinet config"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redaction removed non-secret content: %q\n\n%s", keep, got)
		}
	}
}

// The pattern is deliberately broad so that a secret field added later is
// redacted by default rather than leaked by default.
func TestRedactionIsBroadByDefault(t *testing.T) {
	for _, line := range []string{
		"cluster_shared_secret: abc",
		"AuthToken: abc",
		"tls_private_key_file: /etc/x",
		"psk: abc",
		"password_hash: abc",
	} {
		if got := redactConfig(line); strings.Contains(got, "abc") || strings.Contains(got, "/etc/x") {
			t.Errorf("not redacted: %q -> %q", line, got)
		}
	}
}

// The bundle only makes it back to whoever needs it if it survives being
// gzipped and untarred exactly as sent — a truncated write or a wrong header
// field would silently produce a corrupt or empty file, discovered only after
// someone's already downloaded it and needs it during an actual incident.
func TestPackTshootTgzRoundTrips(t *testing.T) {
	const memberName = "gravinet-tshoot-20260801-000000.txt"
	const content = "gravinet troubleshooting bundle\n\n========== NODE ==========\nhello\n"
	modTime := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	archived, err := packTshootTgz(memberName, content, modTime)
	if err != nil {
		t.Fatalf("packTshootTgz: %v", err)
	}
	if len(archived) == 0 {
		t.Fatal("packTshootTgz returned an empty archive")
	}

	gzr, err := gzip.NewReader(bytes.NewReader(archived))
	if err != nil {
		t.Fatalf("not valid gzip: %v", err)
	}
	tr := tar.NewReader(gzr)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("not valid tar, or no entries: %v", err)
	}
	if hdr.Name != memberName {
		t.Errorf("member name = %q, want %q", hdr.Name, memberName)
	}
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("reading member content: %v", err)
	}
	if string(got) != content {
		t.Errorf("content did not round-trip:\ngot:  %q\nwant: %q", got, content)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("expected exactly one entry, found another (or a read error): %v", err)
	}
}

// A large, highly repetitive bundle (the common case — a busy mesh's log
// tail is mostly the same handful of message shapes over and over) should
// compress substantially. Guards against accidentally writing the tar
// stream uncompressed into the response (e.g. skipping gzw.Close(), which
// would still often "work" for small inputs but silently drop the
// compression benefit that's the actual point of packaging this as .tgz).
func TestPackTshootTgzActuallyCompresses(t *testing.T) {
	content := strings.Repeat("2026/08/01 12:00:00 [DEBUG] mesh: path mtu to peer now 8985 bytes\n", 5000)
	archived, err := packTshootTgz("bundle.txt", content, time.Now())
	if err != nil {
		t.Fatalf("packTshootTgz: %v", err)
	}
	if len(archived) >= len(content)/2 {
		t.Errorf("archive (%d bytes) doesn't look compressed relative to source (%d bytes)", len(archived), len(content))
	}
}

// TestExtraGlobalICMPSysctlsPaths pins the exact procfs paths a real
// diagnosis found missing from the tshoot bundle: with the host firewall and
// gravinet's own per-network firewall both cleanly ruled out elsewhere in the
// bundle for a "peer receives the ping, never replies" report, these four
// knobs were the one remaining explanation the tool couldn't confirm or rule
// out, because it wasn't reading them. The IPv4/IPv6 echo_ignore_all pair is
// asymmetric in shape (IPv6's is nested under icmp/, IPv4's isn't) —
// confirmed against the kernel patch that introduced the IPv6 knob — which
// is exactly the kind of detail a bare string literal could silently drift
// on in a future edit without a test actually pinning it.
func TestExtraGlobalICMPSysctlsPaths(t *testing.T) {
	want := []string{
		"/proc/sys/net/ipv6/icmp/echo_ignore_all",
		"/proc/sys/net/ipv4/icmp_echo_ignore_all",
		"/proc/sys/net/ipv6/icmp/ratelimit",
		"/proc/sys/net/ipv4/icmp_ratelimit",
	}
	if len(extraGlobalICMPSysctls) != len(want) {
		t.Fatalf("extraGlobalICMPSysctls has %d entries, want %d: %v", len(extraGlobalICMPSysctls), len(want), extraGlobalICMPSysctls)
	}
	for i, w := range want {
		if extraGlobalICMPSysctls[i] != w {
			t.Errorf("extraGlobalICMPSysctls[%d] = %q, want %q", i, extraGlobalICMPSysctls[i], w)
		}
	}
	// The asymmetry is the whole point: assert it directly rather than only
	// via the literal strings above, so an "obviously matching" future edit
	// (e.g. someone "fixing" IPv4's path to also nest under icmp/) fails
	// loudly instead of just quietly reading "(absent)" from a bundle forever.
	if strings.Contains(extraGlobalICMPSysctls[1], "/icmp/") {
		t.Error("IPv4's echo_ignore_all path must NOT be nested under an icmp/ subdirectory like IPv6's is — that's real kernel path asymmetry, not something to normalize away")
	}
}
