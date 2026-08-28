package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// Registration is on out of the box from v993.
func TestDDNSOnByDefault(t *testing.T) {
	c := Default()
	if !c.DDNS.Active() {
		t.Fatal("a fresh config does not register this node's name")
	}
	if c.DDNS.IntervalMinutes != DefaultDDNSInterval {
		t.Errorf("default interval = %d, want %d", c.DDNS.IntervalMinutes, DefaultDDNSInterval)
	}
	if !c.DDNS.ReverseEnabled() {
		t.Error("reverse records are off by default")
	}
}

// Switching it off has to survive a save and a reload.
//
// The trap is that Load starts from Default(), which now sets 15, and then
// unmarshals the file over it. A 0 that was omitted from the file — which is
// what `omitempty` would do — leaves the default standing, so an operator who
// turned registration off would find it back on after the next restart, with
// nothing in the config to explain why.
func TestDDNSOffSurvivesARoundTrip(t *testing.T) {
	c := Default()
	c.DDNS.IntervalMinutes = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"interval_minutes":0`) {
		t.Fatalf("an explicit 0 was not written to the config, so it will read back as the default: %s", b)
	}
	// And the read back, the way Load does it.
	back := Default()
	if err := json.Unmarshal(b, back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.DDNS.Active() {
		t.Error("registration switched itself back on across a save and reload")
	}
}

// A config written before v992 has no ddns key at all, so it picks the default
// up — which is the point of turning it on, but is also the one upgrade in this
// series that changes what a node does to somebody else's zone without being
// asked. Pinned so the behaviour is a decision rather than an accident.
func TestPreDDNSConfigPicksUpTheDefault(t *testing.T) {
	c := Default()
	if err := json.Unmarshal([]byte(`{"udp_ports":[51820]}`), c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !c.DDNS.Active() {
		t.Error("an upgraded config does not register; the default did not carry")
	}
}

// The TTL default is a value in the config, not a zero the code reinterprets.
// That is what makes 0 available to mean what it means in DNS.
func TestDDNSTTLDefaultIsExplicit(t *testing.T) {
	c := Default()
	if c.DDNS.TTL != DefaultDDNSTTL {
		t.Fatalf("default TTL = %d, want %d", c.DDNS.TTL, DefaultDDNSTTL)
	}
	// And zero survives being asked for, the same trap the interval had: Load
	// starts from Default(), so a 0 dropped by omitempty would read back as
	// 900 and an operator who asked for an uncached record would quietly get a
	// fifteen-minute one.
	c.DDNS.TTL = 0
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"ttl":0`) {
		t.Fatalf("an explicit TTL of 0 was not written, so it will read back as the default: %s", b)
	}
	back := Default()
	if err := json.Unmarshal(b, back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.DDNS.TTL != 0 {
		t.Errorf("TTL read back as %d, want the 0 that was asked for", back.DDNS.TTL)
	}
}

// Nobody's records change lifetime on this upgrade. A config written by v992
// to v994 could not contain an explicit ttl:0 — omitempty dropped it — so an
// absent key is the only pre-v995 shape, and it picks up 900, which is exactly
// what the old 0-means-default resolved to.
func TestUpgradeDoesNotChangeAnyPublishedTTL(t *testing.T) {
	c := Default()
	if err := json.Unmarshal([]byte(`{"udp_ports":[51820],"ddns":{"interval_minutes":15}}`), c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.DDNS.TTL != DefaultDDNSTTL {
		t.Errorf("an upgraded config publishes TTL %d, want the %d its records already had", c.DDNS.TTL, DefaultDDNSTTL)
	}
}

func TestDDNSValidation(t *testing.T) {
	for name, d := range map[string]DDNSConfig{
		"negative interval": {IntervalMinutes: -1},
		"absurd interval":   {IntervalMinutes: 100000},
		"negative ttl":      {IntervalMinutes: 15, TTL: -5},
		"absurd ttl":        {IntervalMinutes: 15, TTL: 99999999},
	} {
		if err := d.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (DDNSConfig{IntervalMinutes: 15, TTL: 900}).Validate(); err != nil {
		t.Errorf("an ordinary config was rejected: %v", err)
	}
}
