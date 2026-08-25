package main

import (
	"os"
	"path/filepath"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// newControlFWTest builds a controlAPI backed by a real config file, with the
// same load/mutate/save/reload shape main.go wires up.
func newControlFWTest(t *testing.T) (*controlAPI, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := config.Default()
	cfg.Networks = nil // the case that matters: a node with no mesh network
	if err := cfg.SaveTo(path); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	prev := controlCfgPath
	controlCfgPath = path
	t.Cleanup(func() { controlCfgPath = prev })

	api := &controlAPI{edit: func(mut func(*config.Config) error) error {
		cur, err := config.Load(path)
		if err != nil {
			return err
		}
		if err := mut(cur); err != nil {
			return err
		}
		if err := cur.Validate(); err != nil {
			return err
		}
		return cur.SaveTo(path)
	}}
	return api, path
}

func fwOnDisk(t *testing.T, path string) []config.FirewallRule {
	t.Helper()
	cur, err := config.Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cur.Firewall.Rules
}

// `gravinet fw add` must reach the config file.
//
// v957 made config the source of truth and removed the engine's rulebase
// write-back, but the control socket still mutated the engine directly — so an
// add landed in a live rulebase that nothing recorded and that the next reload
// rebuilt from config. It looked like it worked and was gone on restart. This
// is the regression that fix exists for, so it is the test that has to hold.
func TestControlFWAddPersistsToConfig(t *testing.T) {
	api, path := newControlFWTest(t)
	added, err := api.FirewallAdd(0, mesh.FirewallRule{Action: "deny", Proto: "tcp", DstPortMin: 22, DstPortMax: 22}, -1)
	if err != nil {
		t.Fatalf("add on a node with no mesh networks: %v", err)
	}
	if added.ID == 0 {
		t.Error("the reply carries no rule id, so the CLI cannot print one to act on")
	}
	rules := fwOnDisk(t, path)
	if len(rules) != 1 || rules[0].Action != "deny" || rules[0].ID != added.ID {
		t.Fatalf("rule did not reach the config file: %+v", rules)
	}
	// Blank scope: enforced on every network, including ones added later.
	if rules[0].Scope != "" {
		t.Errorf("scope %q, want blank (every network)", rules[0].Scope)
	}
}

// Delete has the mirror-image failure: it would appear to work and the rule
// would come back on the next reload.
func TestControlFWDeleteAndMovePersist(t *testing.T) {
	api, path := newControlFWTest(t)
	for _, a := range []string{"allow", "deny", "allow"} {
		if _, err := api.FirewallAdd(0, mesh.FirewallRule{Action: a}, -1); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	rules := fwOnDisk(t, path)
	if len(rules) != 3 {
		t.Fatalf("setup: want 3 rules, got %d", len(rules))
	}
	third := rules[2].ID

	if err := api.FirewallMove(0, third, 0); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := fwOnDisk(t, path); got[0].ID != third {
		t.Errorf("move did not reach the config file: %+v", got)
	}

	if err := api.FirewallDelete(0, []uint64{third}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, r := range fwOnDisk(t, path) {
		if r.ID == third {
			t.Error("delete did not reach the config file; the rule would return on the next reload")
		}
	}
}

// Copy/paste goes through config too, and a pasted rule is a new rule: reusing
// the source's id would give two rules one identity.
func TestControlFWCopyPasteMakesNewRules(t *testing.T) {
	api, path := newControlFWTest(t)
	if _, err := api.FirewallAdd(0, mesh.FirewallRule{Action: "deny", Notes: "original"}, -1); err != nil {
		t.Fatalf("add: %v", err)
	}
	src := fwOnDisk(t, path)[0]
	if err := api.FirewallCopy(0, []uint64{src.ID}); err != nil {
		t.Fatalf("copy: %v", err)
	}
	n, err := api.FirewallPaste(0, -1)
	if err != nil {
		t.Fatalf("paste: %v", err)
	}
	if n != 1 {
		t.Fatalf("pasted %d, want 1", n)
	}
	rules := fwOnDisk(t, path)
	if len(rules) != 2 {
		t.Fatalf("paste did not reach the config file: %+v", rules)
	}
	if rules[1].ID == rules[0].ID {
		t.Error("the pasted rule reuses the source's id; two rules now share one identity")
	}
	if rules[1].Notes != "original" {
		t.Errorf("the pasted rule lost its content: %+v", rules[1])
	}
	// Pasting nothing says so rather than silently succeeding.
	empty := &controlAPI{edit: api.edit}
	if _, err := empty.FirewallPaste(0, -1); err == nil {
		t.Error("pasting an empty clipboard should report it")
	}
}

// A read with no network id reports the configured rulebase, which is what a
// node running no mesh networks has — the engine has nothing to ask.
func TestControlFWListWithNoNetworks(t *testing.T) {
	api, path := newControlFWTest(t)
	if _, err := api.FirewallAdd(0, mesh.FirewallRule{Action: "deny", Notes: "n"}, -1); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := api.FirewallRules(0)
	if err != nil {
		t.Fatalf("list on a node with no mesh networks: %v", err)
	}
	if len(got) != 1 || got[0].Notes != "n" {
		t.Fatalf("list returned %+v", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config vanished: %v", err)
	}
}
