//go:build linux

package netfilter

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// Manager applies the gravinet kernel NAT ruleset via nft (preferred) or
// iptables. It detects which tool is present at New time. For the iptables
// backend it also resolves ip6tables so IPv6 NAT rules can be programmed.
type Manager struct {
	mode string // "nft" or "iptables"
	bin  string // resolved path (nft, or iptables for IPv4)
	bin6 string // resolved ip6tables path (iptables mode only; "" if absent)
}

// New picks a backend: nft if available, else iptables. Returns an error if
// neither tool is on PATH, so the caller can warn that kernel NAT is unavailable.
func New() (*Manager, error) {
	if p, err := exec.LookPath("nft"); err == nil {
		return &Manager{mode: "nft", bin: p}, nil
	}
	if p, err := exec.LookPath("iptables"); err == nil {
		m := &Manager{mode: "iptables", bin: p}
		if p6, err := exec.LookPath("ip6tables"); err == nil {
			m.bin6 = p6
		}
		return m, nil
	}
	return nil, fmt.Errorf("neither nft nor iptables found on PATH")
}

// Backend reports which tool is in use ("nft" or "iptables").
func (m *Manager) Backend() string { return m.mode }

// Apply installs exactly the given rules, replacing any we installed before.
// Idempotent: calling it again with the same rules is a no-op in effect.
func (m *Manager) Apply(rules []Rule) error {
	if m.mode == "nft" {
		if err := m.run(m.bin, strings.NewReader(nftScript(rules)), "-f", "-"); err != nil {
			return err
		}
		// Drop a family's table when it has no rules now, clearing anything a
		// previous config left behind. Tolerant: the table may not exist.
		if !anyFamily(rules, "ip") {
			m.runTolerant(m.bin, "delete", "table", "ip", tableName)
		}
		if !anyFamily(rules, "ip6") {
			m.runTolerant(m.bin, "delete", "table", "ip6", tableName)
		}
		return nil
	}
	return m.applyIptables(rules)
}

// Clear removes the gravinet NAT ruleset entirely.
func (m *Manager) Clear() error {
	if m.mode == "nft" {
		// Ignore errors when a table was never created.
		m.runTolerant(m.bin, "delete", "table", "ip", tableName)
		m.runTolerant(m.bin, "delete", "table", "ip6", tableName)
		return nil
	}
	m.clearIptablesOn(m.bin)
	if m.bin6 != "" {
		m.clearIptablesOn(m.bin6)
	}
	return nil
}

func (m *Manager) run(bin string, stdin *strings.Reader, args ...string) error {
	cmd := exec.Command(bin, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return nil
}

// runTolerant runs a command but does not treat a non-zero exit as fatal — used
// for setup/teardown steps that may legitimately fail (chain already exists,
// jump already present, nothing to delete).
func (m *Manager) runTolerant(bin string, args ...string) {
	_ = exec.Command(bin, args...).Run()
}

// applyIptables programs IPv4 rules via iptables and IPv6 rules via ip6tables.
func (m *Manager) applyIptables(rules []Rule) error {
	v4, v6 := splitFamily(rules)
	if err := m.applyIptablesOn(m.bin, v4); err != nil {
		return err
	}
	if len(v6) > 0 {
		if m.bin6 == "" {
			return fmt.Errorf("ip6tables not found on PATH; cannot program IPv6 NAT")
		}
		if err := m.applyIptablesOn(m.bin6, v6); err != nil {
			return err
		}
	} else if m.bin6 != "" {
		// No IPv6 rules now: clear any our chains left from a previous config.
		m.clearIptablesOn(m.bin6)
	}
	return nil
}

// applyIptablesOn sets up our dedicated chains on one binary (iptables or
// ip6tables) and (re)populates them with the given rules.
func (m *Manager) applyIptablesOn(bin string, rules []Rule) error {
	// Own dedicated chains so we never disturb the operator's NAT rules.
	m.runTolerant(bin, "-t", "nat", "-N", iptPostChain)
	m.runTolerant(bin, "-t", "nat", "-N", iptPreChain)
	// Link them once (check first to avoid duplicate jumps).
	if exec.Command(bin, "-t", "nat", "-C", "POSTROUTING", "-j", iptPostChain).Run() != nil {
		if err := m.run(bin, nil, "-t", "nat", "-A", "POSTROUTING", "-j", iptPostChain); err != nil {
			return err
		}
	}
	if exec.Command(bin, "-t", "nat", "-C", "PREROUTING", "-j", iptPreChain).Run() != nil {
		if err := m.run(bin, nil, "-t", "nat", "-A", "PREROUTING", "-j", iptPreChain); err != nil {
			return err
		}
	}
	// Flush our chains, then (re)populate.
	if err := m.run(bin, nil, "-t", "nat", "-F", iptPostChain); err != nil {
		return err
	}
	if err := m.run(bin, nil, "-t", "nat", "-F", iptPreChain); err != nil {
		return err
	}
	for _, r := range rules {
		if args := iptablesRuleArgs(r); args != nil {
			if err := m.run(bin, nil, args...); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) clearIptablesOn(bin string) {
	// Unlink (tolerant — may already be gone), then flush and delete our chains.
	m.runTolerant(bin, "-t", "nat", "-D", "POSTROUTING", "-j", iptPostChain)
	m.runTolerant(bin, "-t", "nat", "-D", "PREROUTING", "-j", iptPreChain)
	m.runTolerant(bin, "-t", "nat", "-F", iptPostChain)
	m.runTolerant(bin, "-t", "nat", "-F", iptPreChain)
	m.runTolerant(bin, "-t", "nat", "-X", iptPostChain)
	m.runTolerant(bin, "-t", "nat", "-X", iptPreChain)
}
