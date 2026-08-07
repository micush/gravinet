package config

import (
	"strings"
	"testing"
)

// The operator's case: masquerade the IPv6 half of a dual-stack overlay out a
// physical interface, exactly as the IPv4 half already is. Before this, every
// field that could carry an IPv6 address was rejected with "NAT is IPv4-only",
// so there was no way to express it at all.
func TestIPv6MasqueradeAccepted(t *testing.T) {
	r, err := buildNATRule("fd00:203::/64", "", "", "", "masquerade", "ens18")
	if err != nil {
		t.Fatalf("IPv6 masquerade rejected: %v", err)
	}
	if r.Source != "fd00:203::/64" || r.Translate != "masquerade" || r.Interface != "ens18" {
		t.Fatalf("round-trip wrong: %+v", r)
	}
	if is6, set := natFieldFamily(r.Source); !set || !is6 {
		t.Fatal("source did not classify as IPv6; kernelNATRules would put this in the ip table")
	}
}

// Masquerade has no target address, so source is the only field that can name
// a family. Blank means IPv4 — which is correct and unchanged, but silently
// programming half of what was asked for is the kind of thing that costs an
// afternoon, so it has to be said out loud.
func TestBlankSourceMasqueradeExplainsItselfRatherThanSilentlyDoingV4(t *testing.T) {
	_, err := buildNATRule("", "", "", "", "masquerade", "ens18")
	if err == nil {
		t.Fatal("blank-source masquerade accepted silently; it covers IPv4 only and the operator is not told")
	}
	if !strings.Contains(err.Error(), "IPv4 only") {
		t.Fatalf("error does not explain the family limitation: %v", err)
	}
}

// A Rule carries one V6 flag and kernelNATRules derives it from a single
// field, rendering every other field with that family's keyword. A mixed rule
// would emit "ip6 saddr 192.168.203.0/24" — rejected by nft, and under the
// iptables backend simply handed to ip6tables. Neither is diagnosable by then.
func TestMixedFamilyRuleRejected(t *testing.T) {
	for _, tc := range []struct{ name, src, dst, translate, iface string }{
		{"v4 source, v6 static target", "192.168.203.0/24", "", "fd00:203::1", ""},
		{"v6 source, v4 static target", "fd00:203::/64", "", "192.168.203.1", ""},
		{"v4 source, v6 dest", "192.168.203.0/24", "fd00:203::/64", "masquerade", "ens18"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildNATRule(tc.src, tc.dst, "", "", tc.translate, tc.iface)
			if err == nil {
				t.Fatal("mixed-family rule accepted; it cannot be programmed coherently")
			}
			if !strings.Contains(err.Error(), "mix address families") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// An IPv6 address is full of colons, so the first-colon split both the config
// layer and kernelNATRules used while NAT was IPv4-only turns "fd00:203::5"
// into address "fd00" with "203::5" as its port. Brackets disambiguate when a
// remap port follows; without one the bare form is unambiguous and allowed.
func TestSplitNATTargetHandlesIPv6(t *testing.T) {
	for _, tc := range []struct {
		in       string
		addr     string
		port     string
		hasPort  bool
		wantsErr bool
	}{
		{in: "192.168.5.10", addr: "192.168.5.10"},
		{in: "192.168.5.10:443", addr: "192.168.5.10", port: "443", hasPort: true},
		{in: "fd00:203::5", addr: "fd00:203::5"},
		{in: "[fd00:203::5]", addr: "fd00:203::5"},
		{in: "[fd00:203::5]:443", addr: "fd00:203::5", port: "443", hasPort: true},
		{in: "[fd00:203::5", wantsErr: true},
		{in: "[fd00:203::5]443", wantsErr: true},
		{in: "", wantsErr: true},
	} {
		addr, port, hasPort, err := SplitNATTarget(tc.in)
		if tc.wantsErr {
			if err == nil {
				t.Errorf("SplitNATTarget(%q) = %q/%q, want error", tc.in, addr, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitNATTarget(%q): %v", tc.in, err)
			continue
		}
		if addr != tc.addr || port != tc.port || hasPort != tc.hasPort {
			t.Errorf("SplitNATTarget(%q) = %q,%q,%v; want %q,%q,%v", tc.in, addr, port, hasPort, tc.addr, tc.port, tc.hasPort)
		}
	}
}

// A stored v6 port-forward must be readable back by the same splitter, so the
// brackets have to survive normalization when a remap port is present.
func TestIPv6PortForwardRoundTrips(t *testing.T) {
	r, err := buildNATRule("", "fd00:203::/64", "8443", "tcp", "port-forward:[fd00:203::5]:443", "")
	if err != nil {
		t.Fatalf("IPv6 port-forward rejected: %v", err)
	}
	rest, ok := cutNATPortForwardPrefix(r.Translate)
	if !ok {
		t.Fatalf("stored translate lost its prefix: %q", r.Translate)
	}
	addr, port, hasPort, err := SplitNATTarget(rest)
	if err != nil || addr != "fd00:203::5" || port != "443" || !hasPort {
		t.Fatalf("stored %q did not read back: addr=%q port=%q hasPort=%v err=%v", r.Translate, addr, port, hasPort, err)
	}
}

// IPv4-mapped forms are the trap in the middle: netip.Addr.Is6 answers true
// for ::ffff:a.b.c.d, so one would be sorted into the ip6 table and emitted as
// a match that can never fire — a dead rule rather than an error.
func TestIPv4MappedRejected(t *testing.T) {
	for _, src := range []string{"::ffff:192.168.203.1", "::ffff:192.168.203.0/120"} {
		if _, err := buildNATRule(src, "", "", "", "masquerade", "ens18"); err == nil {
			t.Errorf("IPv4-mapped source %q accepted; it would land in the ip6 table as a rule that never matches", src)
		}
	}
	if _, err := buildNATRule("192.168.203.0/24", "", "", "", "::ffff:10.0.0.1", ""); err == nil {
		t.Error("IPv4-mapped translate target accepted")
	}
}

// Everything that worked before must still work, unchanged.
func TestIPv4RulesUnchanged(t *testing.T) {
	r, err := buildNATRule("192.168.203.0/24", "", "", "", "masquerade", "ens18")
	if err != nil {
		t.Fatalf("existing IPv4 masquerade broke: %v", err)
	}
	if r.Source != "192.168.203.0/24" || r.Translate != "masquerade" || r.Interface != "ens18" {
		t.Fatalf("IPv4 masquerade changed shape: %+v", r)
	}
	pf, err := buildNATRule("", "", "32400", "tcp", "port-forward:192.168.5.10:32400", "")
	if err != nil {
		t.Fatalf("existing IPv4 port-forward broke: %v", err)
	}
	if pf.Translate != "port-forward:192.168.5.10:32400" {
		t.Fatalf("IPv4 port-forward re-rendered as %q", pf.Translate)
	}
	if _, err := buildNATRule("192.168.203.0/24", "", "", "", "10.0.0.1", ""); err != nil {
		t.Fatalf("existing IPv4 static SNAT broke: %v", err)
	}
}
