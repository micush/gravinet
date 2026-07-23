package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shadowPathForTest points linuxShadowExpiry's shadowPath var at a fixture for
// the duration of the calling test, restoring it afterward.
func shadowPathForTest(path string) func() {
	prev := shadowPath
	shadowPath = path
	return func() { shadowPath = prev }
}

func TestValidUsernameRejectsInjectionAndSpecialAccounts(t *testing.T) {
	bad := []string{
		"", strings.Repeat("a", 33),
		"bob; rm -rf /", "bob && curl evil", "bob|sh", "$(id)", "`id`",
		"../etc/passwd", "Bob", "bob smith", "1bob", "-bob", "root", "administrator",
	}
	for _, n := range bad {
		if err := validUsername(n); err == nil {
			t.Errorf("validUsername(%q) accepted a value it must reject", n)
		}
	}
}

func TestValidUsernameAcceptsRealNames(t *testing.T) {
	good := []string{"bob", "_svc", "bob-2", "bob_the_builder", "a", strings.Repeat("a", 32)}
	for _, n := range good {
		if err := validUsername(n); err != nil {
			t.Errorf("validUsername(%q) rejected a legitimate name: %v", n, err)
		}
	}
}

func TestValidPasswordRejectsFieldBreakout(t *testing.T) {
	bad := []string{"", "pw\nname:root::::::", "pw\rname:root", "pw:extra", strings.Repeat("a", 600)}
	for _, p := range bad {
		if err := validPassword(p); err == nil {
			t.Errorf("validPassword(%q) accepted a value it must reject", p)
		}
	}
	if err := validPassword("a perfectly normal Pa$$w0rd!"); err != nil {
		t.Errorf("validPassword rejected a legitimate password: %v", err)
	}
}

// TestLinuxShadowExpiry pins the /etc/shadow field-8 (days-since-epoch)
// parsing against a handwritten file, including an empty-expiry line (never
// expires) and a name that doesn't appear at all.
func TestLinuxShadowExpiry(t *testing.T) {
	dir := t.TempDir()
	shadow := filepath.Join(dir, "shadow")
	// name:pw:lastchg:min:max:warn:inactive:expire:reserved (9 fields, expire
	// at index 7)
	body := "root:!:19000:0:99999:7:::\n" +
		"bob:$6$x:19000:0:99999:7::19723:\n" + // 19723 days since epoch
		"alice:$6$y:19000:0:99999:7:::\n" // empty expire field (index 7) == never
	if err := os.WriteFile(shadow, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	restore := shadowPathForTest(shadow)
	defer restore()

	exp, known := linuxShadowExpiry("bob")
	if !known {
		t.Fatal("expected bob's expiry to be known")
	}
	want := time.Unix(19723*86400, 0).UTC()
	if !exp.Equal(want) {
		t.Errorf("bob expiry = %v, want %v", exp, want)
	}

	exp, known = linuxShadowExpiry("alice")
	if !known || !exp.IsZero() {
		t.Errorf("alice (empty expire field) should be known+never, got known=%v exp=%v", known, exp)
	}

	if _, known := linuxShadowExpiry("nobody"); known {
		t.Error("expiry for a name absent from /etc/shadow should be unknown, not a guess")
	}
}

func TestOpenBSDExpiryParsesUserinfoOutput(t *testing.T) {
	cases := []struct {
		out       string
		wantKnown bool
		wantZero  bool
	}{
		{"login\tbob\nExpiration date\tThu Jan 15 00:00:00 2026\n", true, false},
		{"login\tbob\nExpiration date\tNever\n", true, true},
		{"login\tbob\npasswd change\tNever\n", true, true}, // no expiration line at all
	}
	for _, c := range cases {
		exp, known := parseOpenBSDUserinfo(c.out)
		if known != c.wantKnown {
			t.Errorf("parseOpenBSDUserinfo(%q) known=%v, want %v", c.out, known, c.wantKnown)
			continue
		}
		if c.wantZero && !exp.IsZero() {
			t.Errorf("parseOpenBSDUserinfo(%q) expiry=%v, want zero", c.out, exp)
		}
	}
}

func TestWindowsExpiryParsesNetUserOutput(t *testing.T) {
	cases := []struct {
		out       string
		wantKnown bool
		wantZero  bool
	}{
		{"Account expires             Never\n", true, true},
		{"Account expires             1/15/2026 12:00:00 AM\n", true, false},
		{"Account expires             1/15/2026\n", true, false},
		{"Full Name                   Bob\n", false, false}, // no such line
	}
	for _, c := range cases {
		exp, known := parseWindowsNetUser(c.out)
		if known != c.wantKnown {
			t.Errorf("parseWindowsNetUser(%q) known=%v, want %v", c.out, known, c.wantKnown)
			continue
		}
		if c.wantZero && known && !exp.IsZero() {
			t.Errorf("parseWindowsNetUser(%q) expiry=%v, want zero", c.out, exp)
		}
	}
}

func TestFreeBSDExpiryParsesUsershowOutput(t *testing.T) {
	cases := []struct {
		out       string
		wantKnown bool
		wantZero  bool
	}{
		{"bob:*:1001:1001:User &:0:0::Bob:/nonexistent:/usr/sbin/nologin", true, true},
		{"bob:*:1001:1001:User &:0:1768435200::Bob:/nonexistent:/usr/sbin/nologin", true, false},
		{"garbage", false, false},
	}
	for _, c := range cases {
		exp, known := parseFreeBSDUsershow(c.out)
		if known != c.wantKnown {
			t.Errorf("parseFreeBSDUsershow(%q) known=%v, want %v", c.out, known, c.wantKnown)
			continue
		}
		if c.wantZero && known && !exp.IsZero() {
			t.Errorf("parseFreeBSDUsershow(%q) expiry=%v, want zero", c.out, exp)
		}
	}
}
