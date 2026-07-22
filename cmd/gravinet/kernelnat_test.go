package main

import (
	"net/netip"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/netfilter"
)

// TestKernelNATRulesModes covers kernelNATRules' translation of each
// config.NATRule Translate form into the right netfilter.Rule kind:
// masquerade, a literal address (static SNAT), and port-forward:<addr>
// (DNAT) — the three forms buildNATRule accepts, now that the mode lives in
// Translate itself rather than a separate Direction field (see
// config.NATRule's doc comment). Also covers the cases that must produce no
// kernel rule at all: a disabled rule, NAT disabled network-wide, the
// network itself disabled, and a port-forward rule with an unparsable
// target (matches the old direction-based code's behavior of silently
// skipping a malformed DNAT rather than erroring the whole pass).
func TestKernelNATRulesModes(t *testing.T) {
	mk := func(rules []config.NATRule) *config.Config {
		return &config.Config{Networks: []config.Network{{
			ID: "1", Name: "n", Enabled: true,
			NAT: config.NAT{Enabled: true, Rules: rules},
		}}}
	}

	t.Run("masquerade", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Translate: "masquerade", Interface: "eth0", Enabled: true}})
		out := kernelNATRules(cfg)
		if len(out) != 1 || out[0].Kind != netfilter.Masquerade || out[0].OutIface != "eth0" {
			t.Fatalf("expected one Masquerade rule on eth0, got %+v", out)
		}
	})

	t.Run("literal SNAT", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Source: "10.0.0.0/24", Translate: "203.0.113.9", Enabled: true}})
		out := kernelNATRules(cfg)
		if len(out) != 1 || out[0].Kind != netfilter.SNAT || out[0].To != netip.MustParseAddr("203.0.113.9") {
			t.Fatalf("expected one SNAT rule to 203.0.113.9, got %+v", out)
		}
	})

	t.Run("port-forward DNAT", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Dest: "203.0.113.5", Translate: "port-forward:10.0.0.9", Enabled: true}})
		out := kernelNATRules(cfg)
		if len(out) != 1 || out[0].Kind != netfilter.DNAT || out[0].To != netip.MustParseAddr("10.0.0.9") {
			t.Fatalf("expected one DNAT rule to 10.0.0.9, got %+v", out)
		}
	})

	t.Run("port-forward case-insensitive", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Translate: "Port-Forward:10.0.0.9", Enabled: true}})
		out := kernelNATRules(cfg)
		if len(out) != 1 || out[0].Kind != netfilter.DNAT {
			t.Fatalf("expected mixed-case port-forward prefix to still produce a DNAT rule, got %+v", out)
		}
	})

	t.Run("port-forward bad target skipped, not fatal", func(t *testing.T) {
		cfg := mk([]config.NATRule{
			{Translate: "port-forward:not-an-ip", Enabled: true},
			{Translate: "masquerade", Interface: "eth0", Enabled: true},
		})
		out := kernelNATRules(cfg)
		if len(out) != 1 || out[0].Kind != netfilter.Masquerade {
			t.Fatalf("expected the malformed port-forward rule skipped and the good rule kept, got %+v", out)
		}
	})

	t.Run("disabled rule excluded", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Translate: "masquerade", Interface: "eth0", Enabled: false}})
		if out := kernelNATRules(cfg); len(out) != 0 {
			t.Fatalf("expected a disabled rule to produce no kernel rule, got %+v", out)
		}
	})

	t.Run("NAT disabled network-wide excluded", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Translate: "masquerade", Interface: "eth0", Enabled: true}})
		cfg.Networks[0].NAT.Enabled = false
		if out := kernelNATRules(cfg); len(out) != 0 {
			t.Fatalf("expected NAT-disabled network to produce no kernel rules, got %+v", out)
		}
	})

	t.Run("network disabled excluded", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Translate: "masquerade", Interface: "eth0", Enabled: true}})
		cfg.Networks[0].Enabled = false
		if out := kernelNATRules(cfg); len(out) != 0 {
			t.Fatalf("expected a disabled network to produce no kernel rules, got %+v", out)
		}
	})

	// The old code special-cased Direction=="overlay2overlay" to skip kernel
	// rule generation entirely, on the theory that overlay<->overlay traffic
	// never crosses a physical interface. That distinction no longer exists
	// (see config.NATRule's doc comment: it never actually did anything
	// besides suppress this rule, since overlay2overlay and overlay2underlay
	// were identical SNAT in the userspace engine either way) — every
	// masquerade/SNAT-style rule now always gets a kernel-side attempt,
	// which is harmless for traffic that never actually egresses the named
	// interface (the rule just never matches). This test exists to make that
	// an explicit, intentional behavior rather than a silent side effect of
	// the refactor.
	t.Run("masquerade always attempted, no overlay2overlay carve-out", func(t *testing.T) {
		cfg := mk([]config.NATRule{{Translate: "masquerade", Interface: "eth0", Enabled: true}})
		out := kernelNATRules(cfg)
		if len(out) != 1 {
			t.Fatalf("expected a kernel rule regardless of whether the traffic is overlay-only, got %+v", out)
		}
	})
}
