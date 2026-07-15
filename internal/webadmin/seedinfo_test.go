package webadmin

import "testing"

func TestSeedHost(t *testing.T) {
	cases := map[string]string{
		"example.com":            "example.com",
		"example.com:8443":       "example.com",
		"tcp://example.com:8443": "example.com",
		"udp://example.com":      "example.com",
		"203.0.113.5":            "203.0.113.5",
		"203.0.113.5:8443":       "203.0.113.5",
		"tcp://203.0.113.5:8443": "203.0.113.5",
		"[2001:db8::1]:8443":     "2001:db8::1",
		"2001:db8::1":            "2001:db8::1",
		"  example.com  ":        "example.com",
		"":                       "",
	}
	for in, want := range cases {
		if got := seedHost(in); got != want {
			t.Errorf("seedHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWhoisReferral(t *testing.T) {
	cases := []struct {
		text string
		want string
	}{
		{"", ""},
		{"% no referral here\ninetnum: 1.1.1.0 - 1.1.1.255\n", ""},
		{"refer:        whois.arin.net\n\ninetnum: ...\n", "whois.arin.net"},
		{"whois:        whois.ripe.net\n", "whois.ripe.net"},
		{"  refer:   whois.apnic.net  \n", "whois.apnic.net"},
	}
	for _, c := range cases {
		if got := whoisReferral(c.text); got != c.want {
			t.Errorf("whoisReferral(%q) = %q, want %q", c.text, got, c.want)
		}
	}
}
