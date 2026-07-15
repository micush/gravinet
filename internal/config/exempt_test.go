package config

import (
	"encoding/json"
	"testing"
)

func TestFirewallExemptDefaults(t *testing.T) {
	c := &Config{}

	// Unset global list reports the built-in defaults, flagged as default.
	list, isDefault := c.FirewallExemptList()
	if !isDefault {
		t.Error("fresh config should report default exemptions")
	}
	want := DefaultFirewallExempts()
	if len(list) != len(want) {
		t.Fatalf("default exempt count = %d, want %d", len(list), len(want))
	}
	names := map[string]bool{}
	for _, e := range list {
		names[e.Name] = true
	}
	for _, n := range []string{"remote management", "BGP", "OSPF", "RIP", "RIPng"} {
		if !names[n] {
			t.Errorf("default exemptions missing %q", n)
		}
	}
}

func TestFirewallExemptAddMaterializesDefaults(t *testing.T) {
	c := &Config{}
	if err := c.FirewallExemptAdd(FirewallExempt{Name: "vxlan", Proto: "udp", Port: 4789}); err != nil {
		t.Fatal(err)
	}
	list, isDefault := c.FirewallExemptList()
	if isDefault {
		t.Error("after add, list should be custom, not default")
	}
	if len(list) != len(DefaultFirewallExempts())+1 {
		t.Fatalf("expected defaults+1, got %d", len(list))
	}
	if list[len(list)-1].Name != "vxlan" {
		t.Errorf("new entry not appended; got %q", list[len(list)-1].Name)
	}
}

func TestFirewallExemptDeleteAndReset(t *testing.T) {
	c := &Config{}
	all := len(DefaultFirewallExempts())
	idxs := make([]int, all)
	for i := range idxs {
		idxs[i] = i
	}
	if err := c.FirewallExemptDelete(idxs); err != nil {
		t.Fatal(err)
	}
	list, isDefault := c.FirewallExemptList()
	if isDefault {
		t.Error("emptied list must not revert to defaults")
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}
	c.FirewallExemptReset()
	if _, isDefault = c.FirewallExemptList(); !isDefault {
		t.Error("reset should restore the default exemptions")
	}
}

func TestFirewallExemptSet(t *testing.T) {
	c := &Config{}
	err := c.FirewallExemptSet([]FirewallExempt{
		{Name: "mgmt", Proto: "tcp", Mgmt: true},
		{Name: "vxlan", Proto: "udp", Port: 4789},
	})
	if err != nil {
		t.Fatal(err)
	}
	list, isDefault := c.FirewallExemptList()
	if isDefault || len(list) != 2 {
		t.Fatalf("set should replace list; got %d isDefault=%v", len(list), isDefault)
	}
	// Setting an invalid entry must fail and not mutate the list.
	if err := c.FirewallExemptSet([]FirewallExempt{{Name: "bad", Proto: "frob"}}); err == nil {
		t.Error("invalid proto in set should be rejected")
	}
	if l, _ := c.FirewallExemptList(); len(l) != 2 {
		t.Error("failed set must not mutate the list")
	}
}

func TestFirewallExemptValidation(t *testing.T) {
	c := &Config{}
	if err := c.FirewallExemptAdd(FirewallExempt{Name: "bad", Proto: "frobnicate"}); err == nil {
		t.Error("unknown proto should be rejected")
	}
	if err := c.FirewallExemptAdd(FirewallExempt{Name: "bad", Proto: "tcp", Port: 70000}); err == nil {
		t.Error("out-of-range port should be rejected")
	}
	if err := c.FirewallExemptAdd(FirewallExempt{Name: "ok", Proto: "ospf"}); err != nil {
		t.Errorf("ospf should be valid: %v", err)
	}
	if err := c.FirewallExemptAdd(FirewallExempt{Name: "ok2", Proto: "47"}); err != nil {
		t.Errorf("numeric proto should be valid: %v", err)
	}
}

func TestParseExemptProto(t *testing.T) {
	cases := map[string]struct {
		num uint8
		ok  bool
	}{
		"": {0, true}, "any": {0, true}, "tcp": {6, true}, "udp": {17, true},
		"icmp": {1, true}, "ospf": {89, true}, "89": {89, true}, "frob": {0, false},
		"999": {0, false},
	}
	for in, want := range cases {
		n, ok := ParseExemptProto(in)
		if ok != want.ok || (ok && n != want.num) {
			t.Errorf("ParseExemptProto(%q) = (%d,%v), want (%d,%v)", in, n, ok, want.num, want.ok)
		}
	}
}

func TestFirewallExemptSetEnabled(t *testing.T) {
	c := &Config{}
	// Add a custom entry (this materializes the defaults first).
	if err := c.FirewallExemptAdd(FirewallExempt{Name: "BFD", Proto: "udp", Port: 3784}); err != nil {
		t.Fatal(err)
	}
	last := len(c.FirewallExempts) - 1
	// New entries are enabled by default.
	if c.FirewallExempts[last].Disabled {
		t.Fatal("new exemption should be enabled")
	}
	// Disable it — stays in the list, only the flag flips.
	if err := c.FirewallExemptSetEnabled(last, false); err != nil {
		t.Fatal(err)
	}
	if !c.FirewallExempts[last].Disabled {
		t.Fatal("exemption should be disabled")
	}
	if c.FirewallExempts[last].Name != "BFD" || c.FirewallExempts[last].Port != 3784 {
		t.Fatalf("fields should be preserved across toggle: %+v", c.FirewallExempts[last])
	}
	// Re-enable.
	if err := c.FirewallExemptSetEnabled(last, true); err != nil {
		t.Fatal(err)
	}
	if c.FirewallExempts[last].Disabled {
		t.Fatal("exemption should be enabled again")
	}
	// Out-of-range index errors.
	if err := c.FirewallExemptSetEnabled(99, false); err == nil {
		t.Error("out-of-range index should error")
	}
}

// TestFirewallExemptSetEnabledMaterializesDefaults verifies that toggling on a
// pristine config (nil list) first materializes the built-in defaults, so the
// toggle applies to a default entry and the protective defaults aren't dropped.
func TestFirewallExemptSetEnabledMaterializesDefaults(t *testing.T) {
	c := &Config{}
	if c.FirewallExempts != nil {
		t.Fatal("precondition: list should start nil")
	}
	if err := c.FirewallExemptSetEnabled(0, false); err != nil {
		t.Fatal(err)
	}
	if c.FirewallExempts == nil {
		t.Fatal("toggling should materialize the defaults")
	}
	if len(c.FirewallExempts) != len(DefaultFirewallExempts()) {
		t.Fatalf("defaults not fully materialized: have %d", len(c.FirewallExempts))
	}
	if !c.FirewallExempts[0].Disabled {
		t.Fatal("entry 0 should now be disabled")
	}
}

// TestFirewallExemptDisabledJSON pins the polarity: an entry written before the
// Disabled field existed loads as enabled, and an enabled entry omits the key.
func TestFirewallExemptDisabledJSON(t *testing.T) {
	var e FirewallExempt
	if err := json.Unmarshal([]byte(`{"name":"BGP","proto":"tcp","port":179}`), &e); err != nil {
		t.Fatal(err)
	}
	if e.Disabled {
		t.Fatal("an entry with no disabled field must load as enabled")
	}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"name":"BGP","proto":"tcp","port":179}` {
		t.Fatalf("enabled entry should omit disabled key, got %s", b)
	}
	// A disabled entry carries the key.
	e.Disabled = true
	b, _ = json.Marshal(e)
	if string(b) != `{"name":"BGP","proto":"tcp","port":179,"disabled":true}` {
		t.Fatalf("disabled entry should carry the key, got %s", b)
	}
}
