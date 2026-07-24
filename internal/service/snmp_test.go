package service

import (
	"strings"
	"testing"

	"gravinet/internal/config"
)

// TestSNMPCommunityIsSingleToken ports parapet's community_is_single_token.
func TestSNMPCommunityIsSingleToken(t *testing.T) {
	cases := map[string]string{
		"public":         "public",
		"pub lic":        "public",
		"pu\"b\nlic":     "public",
		"pu\\b\tli\rc":   "public",
		"a; rm -rf /":    "a;rm-rf/",
		"\x01\x02public": "public",
	}
	for in, want := range cases {
		if got := cleanSNMPCommunity(in); got != want {
			t.Errorf("cleanSNMPCommunity(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSNMPValuesAreQuotedAndSafe ports parapet's values_are_quoted_and_safe.
func TestSNMPValuesAreQuotedAndSafe(t *testing.T) {
	if got, want := snmpConfValue("Rack 3"), `"Rack 3"`; got != want {
		t.Errorf("snmpConfValue(%q) = %q, want %q", "Rack 3", got, want)
	}
	// Embedded quote/newline are stripped, not allowed to break out.
	if got, want := snmpConfValue("a\"b\nc"), `"abc"`; got != want {
		t.Errorf("snmpConfValue with embedded quote/newline = %q, want %q", got, want)
	}
	// A backslash can't be used to escape into the surrounding quotes either.
	if got, want := snmpConfValue(`a\"b`), `"ab"`; got != want {
		t.Errorf("snmpConfValue with embedded backslash-quote = %q, want %q", got, want)
	}
}

// TestSNMPListenValidation ports parapet's listen_validation.
func TestSNMPListenValidation(t *testing.T) {
	valid := []string{"udp:161", "0.0.0.0:161", "udp:10.0.0.1:161"}
	for _, s := range valid {
		if !validSNMPListen(s) {
			t.Errorf("validSNMPListen(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "udp:161; rm -rf /", "udp 161", strings.Repeat("a", 65)}
	for _, s := range invalid {
		if validSNMPListen(s) {
			t.Errorf("validSNMPListen(%q) = true, want false", s)
		}
	}
}

// TestSNMPRunnableRequiresCommunity ports parapet's runnable_requires_community.
func TestSNMPRunnableRequiresCommunity(t *testing.T) {
	var s config.SNMPConfig
	if s.IsRunnable() {
		t.Error("zero-value config should not be runnable")
	}
	s.Enabled = true
	if s.IsRunnable() {
		t.Error("enabled with no community should not be runnable")
	}
	s.Community = "public"
	if !s.IsRunnable() {
		t.Error("enabled with a community string should be runnable")
	}
}

// TestSNMPConfIncludesLocationAndContact ports parapet's
// conf_includes_location_and_contact.
func TestSNMPConfIncludesLocationAndContact(t *testing.T) {
	cfg := config.SNMPConfig{
		Enabled:   true,
		Community: "public",
		Location:  "Server Room A",
		Contact:   "noc@example.com",
	}
	conf := renderSNMPConf(cfg)
	if !strings.Contains(conf, "rocommunity public\n") {
		t.Errorf("conf missing rocommunity line:\n%s", conf)
	}
	if !strings.Contains(conf, `sysLocation "Server Room A"`) {
		t.Errorf("conf missing sysLocation line:\n%s", conf)
	}
	if !strings.Contains(conf, `sysContact "noc@example.com"`) {
		t.Errorf("conf missing sysContact line:\n%s", conf)
	}
}

// TestSNMPConfOmitsEmptyLocationAndContact checks the directives are left
// out entirely when unset, rather than rendered as sysLocation "".
func TestSNMPConfOmitsEmptyLocationAndContact(t *testing.T) {
	conf := renderSNMPConf(config.SNMPConfig{Enabled: true, Community: "public"})
	if strings.Contains(conf, "sysLocation") || strings.Contains(conf, "sysContact") {
		t.Errorf("conf should omit sysLocation/sysContact when empty:\n%s", conf)
	}
}

// TestSNMPConfInjectionResistance checks that a community string or
// location/contact value crafted to look like a second directive can't
// actually inject one — the whole point of cleanSNMPCommunity/snmpConfValue.
func TestSNMPConfInjectionResistance(t *testing.T) {
	cfg := config.SNMPConfig{
		Enabled:   true,
		Community: "public\nrwcommunity evil",
		Location:  "a\"\nrwcommunity evil2\nsysLocation \"b",
	}
	conf := renderSNMPConf(cfg)
	// The real question isn't whether stray characters happen to spell out
	// "rwcommunity" somewhere inside an otherwise-harmless mashed-together
	// token (they're free to; a single opaque token has no directive
	// meaning) — it's whether a *new line* was created that snmpd would
	// parse as its own directive. Newlines are exactly what
	// cleanSNMPCommunity/snmpConfValue strip, so check line count instead
	// of substring absence.
	roLines := 0
	sysLocLines := 0
	for _, ln := range strings.Split(conf, "\n") {
		if strings.HasPrefix(ln, "rocommunity ") {
			roLines++
		}
		if strings.HasPrefix(ln, "sysLocation ") {
			sysLocLines++
		}
		if strings.HasPrefix(ln, "rwcommunity") {
			t.Errorf("an injected rwcommunity directive became its own line:\n%s", conf)
		}
	}
	if roLines != 1 {
		t.Errorf("expected exactly 1 rocommunity line, got %d:\n%s", roLines, conf)
	}
	if sysLocLines != 1 {
		t.Errorf("expected exactly 1 sysLocation line, got %d:\n%s", sysLocLines, conf)
	}
}
