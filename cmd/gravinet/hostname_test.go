package main

import "testing"

// TestShortHostname covers the OpenBSD /etc/myname-is-an-FQDN case that
// left the Peers table showing "gn-openbsd.cush.local" instead of the short
// "gn-openbsd" every other platform's os.Hostname() already returns.
func TestShortHostname(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already short, no domain", "gn-cush1", "gn-cush1"},
		{"openbsd-style FQDN in /etc/myname", "gn-openbsd.cush.local", "gn-openbsd"},
		{"single-label domain suffix", "media1.local", "media1"},
		{"multiple dots, only first label kept", "gn-win11.corp.example.com", "gn-win11"},
		{"empty string", "", ""},
		{"leading dot (degenerate)", ".cush.local", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shortHostname(c.in); got != c.want {
				t.Errorf("shortHostname(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
