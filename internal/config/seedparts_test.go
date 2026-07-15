package config

import "testing"

func TestSeedPartsAndValidate(t *testing.T) {
	for _, c := range []struct{ in, tr, hp string }{
		{"tcp://203.0.113.5:443", "tcp", "203.0.113.5:443"},
		{"udp://host:1", "udp", "host:1"},
		{"host:65432", "udp", "host:65432"},
		{"TCP://h:2", "tcp", "h:2"},
		{"  198.51.100.1  ", "udp", "198.51.100.1"},
	} {
		tr, hp := SeedParts(c.in)
		if tr != c.tr || hp != c.hp {
			t.Errorf("SeedParts(%q) = %q,%q want %q,%q", c.in, tr, hp, c.tr, c.hp)
		}
	}
	if err := validateSeedAddr("tcp://203.0.113.5:443"); err != nil {
		t.Errorf("valid tcp seed rejected: %v", err)
	}
	if err := validateSeedAddr("tcp://"); err == nil {
		t.Error("empty tcp seed accepted")
	}
	if err := validateSeedAddr("host:99999"); err == nil {
		t.Error("out-of-range port accepted")
	}
	// Multi-port: "host:port,port,..." is the new syntax under test.
	if err := validateSeedAddr("host:65432,443,53"); err != nil {
		t.Errorf("valid multi-port seed rejected: %v", err)
	}
	if err := validateSeedAddr("host:65432,99999"); err == nil {
		t.Error("multi-port seed with one out-of-range port accepted")
	}
	if err := validateSeedAddr("host:65432,abc"); err == nil {
		t.Error("multi-port seed with one non-numeric port accepted")
	}
	if err := validateSeedAddr("host:65432,"); err == nil {
		t.Error("multi-port seed with a trailing comma (empty port) accepted")
	}
}
