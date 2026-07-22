package webadmin

import "testing"

// TestPamServiceFileApplies locks in which platforms build the PAM backend
// and so should be checked for a missing /etc/pam.d/<service> file.
// Regression test: freebsd was originally left off this list even after
// auth_pam.go's build tag grew to include it (`(linux || darwin || freebsd)
// && cgo`), so a FreeBSD host with a missing or corrupted PAM service file
// got no diagnostic at all — the exact failure Linux and macOS already
// warned about. OpenBSD must stay false: it authenticates via BSD auth
// (login_passwd(8)), which has no PAM service file to go missing.
func TestPamServiceFileApplies(t *testing.T) {
	cases := []struct {
		goos string
		want bool
	}{
		{"linux", true},
		{"darwin", true},
		{"freebsd", true},
		{"openbsd", false},
		{"windows", false},
		{"plan9", false},
	}
	for _, tc := range cases {
		if got := pamServiceFileApplies(tc.goos); got != tc.want {
			t.Errorf("pamServiceFileApplies(%q) = %v, want %v", tc.goos, got, tc.want)
		}
	}
}
