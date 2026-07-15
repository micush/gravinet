//go:build openbsd

package webadmin

import "testing"

func TestBsdAuthName(t *testing.T) {
	if got := systemAuthName(); got != "bsd_auth" {
		t.Errorf("systemAuthName() = %q, want %q", got, "bsd_auth")
	}
	a, ok := systemAuthenticator("", nil, nil)
	if !ok {
		t.Fatal("systemAuthenticator returned ok=false on openbsd")
	}
	if a.Name() != "bsd_auth" {
		t.Errorf("Authenticator.Name() = %q, want %q", a.Name(), "bsd_auth")
	}
}

func TestBsdAuthRejectsWithoutHelper(t *testing.T) {
	// These paths must short-circuit to false before ever spawning
	// login_passwd, so they're safe to assert regardless of the host.
	a := &bsdAuth{}
	if a.Authenticate("", "whatever") {
		t.Error("empty username should be rejected")
	}

	restricted := &bsdAuth{allow: allowSet([]string{"alice"})}
	if restricted.Authenticate("bob", "whatever") {
		t.Error("user not in allow_users should be rejected without invoking the helper")
	}
}
