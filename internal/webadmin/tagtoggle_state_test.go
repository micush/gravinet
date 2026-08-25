package webadmin

import (
	"strings"
	"testing"
)

// TestTagToggleRowsCarryEnabledState guards against reintroducing "state
// enabled|disabled doesn't work at all — the page flashes, but nothing
// changes".
//
// toggleTagState decides which way a double-click goes by reading the row it
// is in:
//
//	const cur = tr.dataset.enabled !== undefined ? ... : tag.classList...
//
// and posts enable or disable accordingly. A row that renders a tag-toggle but
// never writes data-enabled therefore used to read as disabled on every click,
// so every click computed "turn it on" and posted enable. Disabling was
// unreachable: the optimistic repaint plus the background refresh() made the
// page flash and land back exactly where it started, which is a toggle that
// looks wired up and is not.
//
// Three tables shipped that way — DHCP subnets, IPv6 RA interfaces, and
// tagged interfaces — because nothing tied the markup to the helper that reads
// it. toggleTagState now falls back to the tag's own on/off class, so a
// missing attribute is no longer fatal, but the attribute is still the
// documented contract and the other three data-* readers of it (the edit
// gating in the routes, seeds and QoS tables) have no such fallback. So this
// checks the markup rather than trusting the fallback to cover for it.
//
// Scans the served page as text rather than running the JS: this package has
// no JS runtime in its test suite, the same constraint the other UI guards
// work under.
func TestTagToggleRowsCarryEnabledState(t *testing.T) {
	// Every row-building literal that renders a state tag. Keyed by the class
	// the row is given, which is also what makes the failure message name the
	// table an operator would recognise.
	rows := map[string]string{
		"dhrow":  "DHCP subnets (System > DHCP)",
		"dlrow":  "DHCP relay links (System > DHCP)",
		"rarow":  "IPv6 RA interfaces (Traffic > IPv6 RA)",
		"vlrow":  "tagged interfaces (System > Interfaces)",
		"qrow":   "QoS rules (Traffic > QoS)",
		"fwrow":  "firewall rules (Traffic > Firewall)",
		"natrow": "NAT rules (Traffic > NAT)",
	}
	for cls, what := range rows {
		// The row's opening tag is built by concatenating JS string fragments,
		// so it cannot be matched as one quoted literal. Take everything from
		// the class name to the row's first cell, which is where the
		// attributes stop.
		i := strings.Index(indexHTML, `class="`+cls)
		if i < 0 {
			t.Errorf("%s: no row literal with class %q found — this guard has gone stale, not the page", what, cls)
			continue
		}
		open := indexHTML[i:]
		if j := strings.Index(open, "<td"); j >= 0 {
			open = open[:j]
		}
		if !strings.Contains(open, "data-enabled=") {
			t.Errorf("%s: the row renders a state tag but never writes data-enabled, "+
				"so toggleTagState cannot tell which way to go and disable is unreachable:\n  %s", what, open)
		}
	}
}

// And the helper itself keeps the fallback, so the next table to forget the
// attribute degrades to a working toggle rather than a silent one-way switch.
func TestToggleTagStateFallsBackToTheTagClass(t *testing.T) {
	fn := between(t, indexHTML, "function toggleTagState(tag, path, buildPayload){", "\n}")
	if !strings.Contains(fn, "classList.contains('on')") {
		t.Error("toggleTagState no longer falls back to the tag's own class, so a row " +
			"missing data-enabled is once again a toggle that only ever enables")
	}
	if !strings.Contains(fn, "dataset.enabled !== undefined") {
		t.Error("toggleTagState no longer prefers data-enabled where it is present")
	}
}
