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

// TestSNMPRunnableRequiresCommunity ports parapet's runnable_requires_community,
// extended for a list: at least one *enabled* community is required, a
// disabled-only list doesn't count, and a second disabled entry alongside
// an enabled one doesn't stop it from being runnable.
func TestSNMPRunnableRequiresCommunity(t *testing.T) {
	var s config.SNMPConfig
	if s.IsRunnable() {
		t.Error("zero-value config should not be runnable")
	}
	s.Enabled = true
	if s.IsRunnable() {
		t.Error("enabled with no communities should not be runnable")
	}
	s.Communities = []config.SNMPCommunity{{Community: "public", Disabled: true}}
	if s.IsRunnable() {
		t.Error("enabled with only a disabled community should not be runnable")
	}
	s.Communities = append(s.Communities, config.SNMPCommunity{Community: "private"})
	if !s.IsRunnable() {
		t.Error("enabled with one enabled community (alongside a disabled one) should be runnable")
	}
}

// TestSNMPConfIncludesLocationAndContact ports parapet's
// conf_includes_location_and_contact.
func TestSNMPConfIncludesLocationAndContact(t *testing.T) {
	cfg := config.SNMPConfig{
		Enabled:     true,
		Communities: []config.SNMPCommunity{{Community: "public"}},
		Location:    "Server Room A",
		Contact:     "noc@example.com",
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
	conf := renderSNMPConf(config.SNMPConfig{Enabled: true, Communities: []config.SNMPCommunity{{Community: "public"}}})
	if strings.Contains(conf, "sysLocation") || strings.Contains(conf, "sysContact") {
		t.Errorf("conf should omit sysLocation/sysContact when empty:\n%s", conf)
	}
}

// TestSNMPConfMultipleCommunities checks each enabled community gets its
// own rocommunity line, in order, and a disabled one is skipped entirely
// rather than written out inert — unlike syslog's host-file-is-truth
// design, config.SNMPConfig itself is the source of truth here, so there's
// nothing to recover by re-parsing snmpd.conf; a disabled community simply
// isn't rendered.
func TestSNMPConfMultipleCommunities(t *testing.T) {
	cfg := config.SNMPConfig{
		Enabled: true,
		Communities: []config.SNMPCommunity{
			{Community: "public"},
			{Community: "internal-ro", Disabled: true},
			{Community: "monitoring"},
		},
	}
	conf := renderSNMPConf(cfg)
	var roLines []string
	for _, ln := range strings.Split(conf, "\n") {
		if strings.HasPrefix(ln, "rocommunity ") {
			roLines = append(roLines, ln)
		}
	}
	want := []string{"rocommunity public", "rocommunity monitoring"}
	if len(roLines) != len(want) {
		t.Fatalf("got %d rocommunity lines %v, want %d %v", len(roLines), roLines, len(want), want)
	}
	for i, w := range want {
		if roLines[i] != w {
			t.Errorf("rocommunity line %d = %q, want %q", i, roLines[i], w)
		}
	}
	if strings.Contains(conf, "internal-ro") {
		t.Errorf("disabled community leaked into the rendered conf:\n%s", conf)
	}
}

// TestSNMPConfInjectionResistance checks that a community string or
// location/contact value crafted to look like a second directive can't
// actually inject one — the whole point of cleanSNMPCommunity/snmpConfValue.
func TestSNMPConfInjectionResistance(t *testing.T) {
	cfg := config.SNMPConfig{
		Enabled:     true,
		Communities: []config.SNMPCommunity{{Community: "public\nrwcommunity evil"}},
		Location:    "a\"\nrwcommunity evil2\nsysLocation \"b",
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
