package webadmin

import (
	"strings"
	"testing"
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
