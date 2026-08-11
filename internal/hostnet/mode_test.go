package hostnet

import (
	"net/netip"
	"strings"
	"testing"
)

// Empty reads as static, and this is the compatibility rule the whole feature
// rests on: every `host_interfaces` record written before modes existed has no
// mode field, and exists only because an operator set a static address. If
// absent ever came to mean anything else, those records would change meaning on
// upgrade — silently, and on the one page whose mistakes take a host off the
// network.
func TestUnsetModeIsStatic(t *testing.T) {
	var unset Mode
	if !unset.IsStatic() {
		t.Error("an unset mode must read as static")
	}
	if unset.Or(ModeStatic) != ModeStatic {
		t.Errorf("Or(static) on unset = %q", string(unset.Or(ModeStatic)))
	}
	if unset.AcceptsRA() {
		t.Error("an unset mode must not accept RAs: it reads as static")
	}
	if unset.Autoconf() {
		t.Error("an unset mode must not autoconfigure")
	}
	// Or does not override a mode that was actually set.
	if got := ModeSLAAC.Or(ModeStatic); got != ModeSLAAC {
		t.Errorf("Or must not replace a set mode: got %q", string(got))
	}
}

// The RA/autoconf split is the load-bearing distinction between dhcp6 and
// slaac, and it is what lets the three IPv6 modes land on two kernel knobs. Get
// this table wrong and DHCPv6 hosts lose their default route, which a v6 host
// takes from the advertisement because a DHCPv6 server does not supply one.
func TestIPv6ModeKnobs(t *testing.T) {
	for _, c := range []struct {
		mode              Mode
		static, ra, aconf bool
	}{
		{ModeStatic, true, false, false},
		{ModeSLAAC, false, true, true},
		{ModeDHCP6, false, true, false},
	} {
		if got := c.mode.IsStatic(); got != c.static {
			t.Errorf("%s: IsStatic = %v, want %v", c.mode, got, c.static)
		}
		if got := c.mode.AcceptsRA(); got != c.ra {
			t.Errorf("%s: AcceptsRA = %v, want %v", c.mode, got, c.ra)
		}
		if got := c.mode.Autoconf(); got != c.aconf {
			t.Errorf("%s: Autoconf = %v, want %v", c.mode, got, c.aconf)
		}
	}
	// The one combination that must never occur: deriving an address from an
	// advertisement this interface is not reading.
	for _, m := range []Mode{ModeStatic, ModeSLAAC, ModeDHCP6, ""} {
		if m.Autoconf() && !m.AcceptsRA() {
			t.Errorf("%q autoconfigures without accepting RAs", string(m))
		}
	}
}

// The families have disjoint vocabularies deliberately. "slaac" submitted as an
// IPv4 mode is not a typo to tolerate — it is a misunderstanding, and the error
// has to say which family the word belongs to or the operator retries the same
// thing.
func TestModeValidationNamesTheRightFamily(t *testing.T) {
	for _, m := range []Mode{"", ModeStatic, ModeDHCP} {
		if err := ValidMode4(m); err != nil {
			t.Errorf("ValidMode4(%q) = %v, want nil", string(m), err)
		}
	}
	for _, m := range []Mode{"", ModeStatic, ModeDHCP6, ModeSLAAC} {
		if err := ValidMode6(m); err != nil {
			t.Errorf("ValidMode6(%q) = %v, want nil", string(m), err)
		}
	}

	// A v6 mode in the v4 field is rejected, and says so.
	for _, m := range []Mode{ModeDHCP6, ModeSLAAC} {
		err := ValidMode4(m)
		if err == nil {
			t.Fatalf("ValidMode4(%q) should be refused", string(m))
		}
		if !strings.Contains(err.Error(), "IPv6") {
			t.Errorf("ValidMode4(%q) should say it is an IPv6 mode: %v", string(m), err)
		}
	}
	// And the reverse, which is the likelier mistake: "dhcp" for IPv6 is a
	// reasonable guess at the spelling, so the error gives the right one.
	err := ValidMode6(ModeDHCP)
	if err == nil {
		t.Fatal("ValidMode6(dhcp) should be refused")
	}
	if !strings.Contains(err.Error(), "dhcp6") {
		t.Errorf("ValidMode6(dhcp) should name the correct spelling: %v", err)
	}

	for _, m := range []Mode{"auto", "STATIC", "none", "manual"} {
		if ValidMode4(m) == nil {
			t.Errorf("ValidMode4(%q) should be refused", string(m))
		}
		if ValidMode6(m) == nil {
			t.Errorf("ValidMode6(%q) should be refused", string(m))
		}
	}
}

// The configuration this whole feature was asked for: a static IPv4 address and
// SLAAC IPv6 on one interface. The two families are read through separate modes
// everywhere, so the v6 half must not be treated as static just because the v4
// half is.
func TestMixedFamilyModesAreIndependent(t *testing.T) {
	s := Spec{Iface: "eth0", Mode4: ModeStatic, Mode6: ModeSLAAC}
	v4 := netip.MustParsePrefix("10.1.1.5/24")
	v6 := netip.MustParsePrefix("2001:db8::5/64")

	if !s.ModeFor(v4.Addr()).IsStatic() {
		t.Error("IPv4 is static here")
	}
	if s.ModeFor(v6.Addr()).IsStatic() {
		t.Error("IPv6 is on SLAAC and must not read as static")
	}

	// StaticAddrs is what keeps an autoconfigured address out of the
	// configuration. Recorded as static, it would be reapplied at the next
	// reload — pinning the interface to whatever prefix the router happened to
	// be advertising when someone edited the MTU.
	got := s.StaticAddrs([]netip.Prefix{v4, v6})
	if len(got) != 1 || got[0] != v4 {
		t.Errorf("StaticAddrs = %v, want only the IPv4 address", got)
	}

	// And the mirror, so a filter that hardcoded a family would fail.
	s2 := Spec{Iface: "eth0", Mode4: ModeDHCP, Mode6: ModeStatic}
	got = s2.StaticAddrs([]netip.Prefix{v4, v6})
	if len(got) != 1 || got[0] != v6 {
		t.Errorf("StaticAddrs = %v, want only the IPv6 address", got)
	}

	// An unset pair keeps the pre-modes behaviour: everything is static and
	// nothing is filtered out.
	var s3 Spec
	if got := s3.StaticAddrs([]netip.Prefix{v4, v6}); len(got) != 2 {
		t.Errorf("an unset spec filtered out %d of 2 addresses", 2-len(got))
	}
}
