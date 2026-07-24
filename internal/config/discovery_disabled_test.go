package config

import (
	"encoding/json"
	"testing"
)

// TestDiscoveryDisabledBackCompat pins the same "zero value already means
// what the old behavior meant" polarity RejectRoute.Disabled uses: a
// config.json saved before this field existed, with interfaces already
// picked (which was the only signal that mattered back then — no separate
// flag at all), must decode with Disabled left false and therefore still
// read as runnable. A plain `bool` named Enabled would have broken this —
// an old file with no "enabled" key would decode to Enabled:false and read
// as freshly disabled the moment the field appeared in the struct — which
// is exactly why this field is named (and polarized) the other way around.
func TestDiscoveryDisabledBackCompat(t *testing.T) {
	// A config.json written before this field existed: interfaces picked,
	// no "disabled" key anywhere.
	legacy := `{"interfaces":[{"name":"eth0","lldp":true}]}`
	var d DiscoveryConfig
	if err := json.Unmarshal([]byte(legacy), &d); err != nil {
		t.Fatal(err)
	}
	if d.Disabled {
		t.Fatal("a legacy config with no \"disabled\" key decoded as Disabled:true \u2014 this would silently switch off LLDP on every existing node's next reconcile")
	}
	if !d.IsRunnable() {
		t.Fatal("legacy config with an interface already picked should still be runnable after this field's addition")
	}

	// A config.json that explicitly turned it off keeps that.
	off := `{"disabled":true,"interfaces":[{"name":"eth0","lldp":true}]}`
	var d2 DiscoveryConfig
	if err := json.Unmarshal([]byte(off), &d2); err != nil {
		t.Fatal(err)
	}
	if !d2.Disabled || d2.IsRunnable() {
		t.Fatalf("explicit disabled:true should stay disabled and not runnable, got %+v", d2)
	}
}

// TestDiscoveryDisabledIndependentOfInterfaces confirms the pill's own
// double-click toggle can flip Disabled without ever touching Interfaces,
// and that Disabled overrides an otherwise-runnable interface list rather
// than being merely advisory.
func TestDiscoveryDisabledIndependentOfInterfaces(t *testing.T) {
	d := DiscoveryConfig{Interfaces: []DiscoveryIface{{Name: "eth0", LLDP: true}}}
	if !d.IsRunnable() {
		t.Fatal("interfaces picked, Disabled at its zero value, should be runnable")
	}

	d.Disabled = true
	if d.IsRunnable() {
		t.Error("Disabled should override an otherwise-runnable interface list")
	}
	if len(d.Interfaces) != 1 {
		t.Error("flipping Disabled must not touch Interfaces")
	}

	d.Disabled = false
	if !d.IsRunnable() {
		t.Error("flipping Disabled back off should restore runnability without re-picking anything")
	}

	// The reverse: enabled with nothing ever picked is inert, not
	// runnable and not an error \u2014 mirrors SNMPConfig.IsRunnable's own
	// Enabled-but-no-community case.
	empty := DiscoveryConfig{}
	if empty.IsRunnable() {
		t.Error("Disabled:false with no interfaces picked should still not be runnable")
	}
}
