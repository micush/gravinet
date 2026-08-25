package main

import (
	"fmt"
	"sync"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// controlAPI wraps the mesh engine for the control socket so that firewall rule
// mutations land in the config file rather than only in the live engine.
//
// Without this, `gravinet fw add` silently loses the rule. Through v956 the
// engine was where rules lived and its persist hook copied them down into
// config afterwards; v957 inverted that, making config the source of truth and
// removing the write-back — which left the control socket mutating a live
// rulebase that nothing records and that the next reload overwrites from
// config. An add would appear to work and be gone on restart; a delete would
// appear to work and come back.
//
// Everything else the control surface does is unchanged, which is what the
// embedded *mesh.Engine is for: only the methods below are overridden, and they
// are exactly the ones that write rules.
//
// Reads still come from the engine. Its rulebase for a network is what config
// says after scope resolution, so a read is the same either way, and going to
// the engine keeps the live hit counters that config has no idea about.
type controlAPI struct {
	*mesh.Engine

	// edit applies a change to the config file and reloads. Same function the
	// web admin's mutateConfig uses, so the two front doors take the same lock
	// and cannot interleave a half-written file.
	edit func(func(*config.Config) error) error

	// clip is the cut/copy buffer. Held here rather than in the engine because
	// the rules being copied are config's now; the engine's own clipboard would
	// paste into a rulebase that the next reload discards.
	//
	// Node-wide and not per network, matching the rulebase itself: a rule
	// copied while looking at one overlay can be pasted while looking at
	// another, which is most of the reason to copy one.
	mu   sync.Mutex
	clip []config.FirewallRule
}

// FirewallRules with a zero network id reports the configured rulebase rather
// than a live one — which is what a node running no mesh networks has. With a
// real id it goes to the engine, so the reply carries that network's hit
// counters and only the rules in scope for it.
func (c *controlAPI) FirewallRules(networkID uint64) ([]mesh.FirewallRule, error) {
	if networkID != 0 {
		return c.Engine.FirewallRules(networkID)
	}
	cfg, err := config.Load(controlCfgPath)
	if err != nil {
		return nil, err
	}
	out := make([]mesh.FirewallRule, 0, len(cfg.Firewall.Rules))
	for _, r := range cfg.Firewall.Rules {
		out = append(out, fwToMesh(r))
	}
	return out, nil
}

func (c *controlAPI) FirewallAdd(networkID uint64, r mesh.FirewallRule, at int) (mesh.FirewallRule, error) {
	var added config.FirewallRule
	err := c.edit(func(cfg *config.Config) error {
		if e := cfg.FirewallRuleAdd(fwFromMesh(r), at); e != nil {
			return e
		}
		// Report the rule as stored, so the CLI can print the id config
		// assigned rather than one the engine happened to mint.
		// FirewallRuleAdd appends when at is out of range, so the rule just
		// stored is at `at` when that was a real position and last otherwise.
		idx := len(cfg.Firewall.Rules) - 1
		if at >= 0 && at < len(cfg.Firewall.Rules) {
			idx = at
		}
		added = cfg.Firewall.Rules[idx]
		return nil
	})
	if err != nil {
		return mesh.FirewallRule{}, err
	}
	return fwToMesh(added), nil
}

func (c *controlAPI) FirewallDelete(networkID uint64, ids []uint64) error {
	return c.edit(func(cfg *config.Config) error { return cfg.FirewallRuleDelete(ids) })
}

func (c *controlAPI) FirewallMove(networkID, id uint64, to int) error {
	return c.edit(func(cfg *config.Config) error { return cfg.FirewallRuleMove(id, to) })
}

func (c *controlAPI) FirewallCopy(networkID uint64, ids []uint64) error {
	cfg, err := config.Load(controlCfgPath)
	if err != nil {
		return err
	}
	picked := pickFirewallRules(cfg.Firewall.Rules, ids)
	if len(picked) == 0 {
		return fmt.Errorf("no firewall rule with those ids")
	}
	c.mu.Lock()
	c.clip = picked
	c.mu.Unlock()
	return nil
}

func (c *controlAPI) FirewallCut(networkID uint64, ids []uint64) error {
	if err := c.FirewallCopy(networkID, ids); err != nil {
		return err
	}
	return c.FirewallDelete(networkID, ids)
}

func (c *controlAPI) FirewallPaste(networkID uint64, at int) (int, error) {
	c.mu.Lock()
	clip := append([]config.FirewallRule(nil), c.clip...)
	c.mu.Unlock()
	if len(clip) == 0 {
		return 0, fmt.Errorf("nothing copied")
	}
	err := c.edit(func(cfg *config.Config) error {
		for i, r := range clip {
			// Ids are dropped: a pasted rule is a new rule, and reusing the
			// source's id would give two rules one identity.
			r.ID = 0
			pos := at
			if pos >= 0 {
				pos += i // keep the pasted block in the order it was copied
			}
			if e := cfg.FirewallRuleAdd(r, pos); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(clip), nil
}

// pickFirewallRules returns the rules matching ids, in rulebase order rather
// than in the order the ids were given — a paste should come out looking like
// what was copied.
func pickFirewallRules(rules []config.FirewallRule, ids []uint64) []config.FirewallRule {
	want := map[uint64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []config.FirewallRule
	for _, r := range rules {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

func fwFromMesh(r mesh.FirewallRule) config.FirewallRule {
	return config.FirewallRule{
		Disabled: r.Disabled, Action: r.Action, Direction: r.Direction, Proto: r.Proto,
		Src: r.Src, Dst: r.Dst, SrcNegate: r.SrcNegate, DstNegate: r.DstNegate,
		SrcPortMin: r.SrcPortMin, SrcPortMax: r.SrcPortMax,
		DstPortMin: r.DstPortMin, DstPortMax: r.DstPortMax,
		Services: r.Services, ServicesNegate: r.ServicesNegate, Log: r.Log,
		Notes: r.Notes, Scope: r.Scope,
	}
}

func fwToMesh(r config.FirewallRule) mesh.FirewallRule {
	return mesh.FirewallRule{
		ID: r.ID, Disabled: r.Disabled, Action: r.Action, Direction: r.Direction, Proto: r.Proto,
		Src: r.Src, Dst: r.Dst, SrcNegate: r.SrcNegate, DstNegate: r.DstNegate,
		SrcPortMin: r.SrcPortMin, SrcPortMax: r.SrcPortMax,
		DstPortMin: r.DstPortMin, DstPortMax: r.DstPortMax,
		Services: r.Services, ServicesNegate: r.ServicesNegate, Log: r.Log,
		Notes: r.Notes, Scope: r.Scope,
	}
}

// controlCfgPath is the config file the control surface edits. Set once at
// daemon start, alongside the wrapper itself.
var controlCfgPath string
