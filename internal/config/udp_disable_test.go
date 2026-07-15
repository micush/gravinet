package config

import "testing"

// TestValidatePrimaryPortZeroRequiresTCPFallback covers the sentinel added
// for the web admin's UDP port "-" field: primary_port == 0 means UDP is
// turned off, which Validate must accept when the TCP/TLS fallback is on,
// and refuse when it isn't — a node with both off could never be reached.
func TestValidatePrimaryPortZeroRequiresTCPFallback(t *testing.T) {
	// UDP off, TCP fallback on (the default — DisableTCPFallback is false
	// unless set) — must validate.
	c := &Config{PrimaryPort: 0, EnableIPv4: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("primary_port=0 with tcp fallback enabled should validate: %v", err)
	}

	// UDP off, TCP fallback also explicitly off — must be refused, since the
	// node would have no way to be reached at all.
	c = &Config{PrimaryPort: 0, EnableIPv4: true, DisableTCPFallback: true}
	if err := c.Validate(); err == nil {
		t.Fatal("primary_port=0 with tcp fallback also disabled should fail validation")
	}

	// A negative primary_port is still out of range regardless of the TCP
	// fallback (0 is the only valid "off" value, not "anything <= 0").
	c = &Config{PrimaryPort: -1, EnableIPv4: true}
	if err := c.Validate(); err == nil {
		t.Fatal("negative primary_port should fail validation")
	}

	// Both on (the ordinary, pre-existing case) still validates.
	c = &Config{PrimaryPort: 51820, EnableIPv4: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("primary_port=51820 with tcp fallback enabled should validate: %v", err)
	}

	// UDP on, TCP fallback off — also still fine (this combination already
	// worked before; only both-off is new territory).
	c = &Config{PrimaryPort: 51820, EnableIPv4: true, DisableTCPFallback: true}
	if err := c.Validate(); err != nil {
		t.Fatalf("primary_port=51820 with tcp fallback disabled should validate: %v", err)
	}
}
