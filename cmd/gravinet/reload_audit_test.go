package main

import (
	"strings"
	"testing"

	"gravinet/internal/crypto"
)

// TestRedactSensitiveCLIArgs is the direct regression test for the thing
// that actually matters about the CLI audit-log feature: a real network key
// or join token must never end up in redactSensitiveCLIArgs' output, since
// that output is what lands in the daemon's persistent log file (see
// reloadDaemon's doc comment). Getting this wrong would be materially worse
// than today — a key typed on the CLI is already in shell history, but
// writing it again into a log file that might get forwarded to syslog or
// reviewed by more people is a real escalation, not a wash.
func TestRedactSensitiveCLIArgs(t *testing.T) {
	// A real generated key, not a hand-typed fake — the whole point is
	// exercising the actual shape crypto.GenerateKey produces.
	realKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "network add: nothing secret, passes through untouched",
			args: []string{"network", "add", "corp", "subnet", "10.50.0.0/16"},
			want: []string{"network", "add", "corp", "subnet", "10.50.0.0/16"},
		},
		{
			name: "key set: the key value is redacted, everything else isn't",
			args: []string{"key", "set", realKey, "-net", "corp", "-slot", "2"},
			want: []string{"key", "set", "<redacted>", "-net", "corp", "-slot", "2"},
		},
		{
			name: "network join with an explicit key: redacted regardless of position",
			args: []string{"network", "join", "57ec4308c912cabd", "key", realKey, "peer", "198.51.100.7"},
			want: []string{"network", "join", "57ec4308c912cabd", "key", "<redacted>", "peer", "198.51.100.7"},
		},
		{
			name: "join token: redacted by its grav1. prefix",
			args: []string{"network", "join", "grav1.eyJ2IjoxLCJpZCI6IjU3In0"},
			want: []string{"network", "join", "<redacted>"},
		},
		{
			name: "quickstart join: same token redaction applies",
			args: []string{"quickstart", "join", "grav1.eyJ2IjoxLCJpZCI6IjU3In0", "-no-service"},
			want: []string{"quickstart", "join", "<redacted>", "-no-service"},
		},
		{
			name: "an ordinary base64-flavored argument that is NOT key-shaped is left alone",
			// Deliberately not 44 chars / doesn't end in "=" — must not be
			// swept up just for containing '/' or '+'.
			args: []string{"host", "add", "nas", "10.0.0.5/24"},
			want: []string{"host", "add", "nas", "10.0.0.5/24"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactSensitiveCLIArgs(c.args)
			if len(got) != len(c.want) {
				t.Fatalf("redactSensitiveCLIArgs(%v) = %v, want %v", c.args, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("redactSensitiveCLIArgs(%v)[%d] = %q, want %q", c.args, i, got[i], c.want[i])
				}
			}
			// Belt-and-suspenders: the real key/token must not appear
			// ANYWHERE in the joined output, not just at the expected index.
			joined := strings.Join(got, " ")
			if strings.Contains(joined, realKey) {
				t.Errorf("real key leaked into redacted output: %q", joined)
			}
			for _, a := range c.args {
				if strings.HasPrefix(a, "grav1.") && strings.Contains(joined, a) {
					t.Errorf("join token leaked into redacted output: %q", joined)
				}
			}
		})
	}
}
