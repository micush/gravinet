//go:build openbsd

package tun

import (
	"net/netip"
	"testing"
)

// TestRouteFamilyHostVsNet locks in the fix: a full-length prefix (/32 for
// IPv4, /128 for IPv6) must use "-host" with the bare address, never "-net"
// with an all-ones-mask CIDR suffix — the shape that was silently dropping
// redistributed /32 mesh routes from the OpenBSD routing table while every
// other prefix length installed fine. Anything shorter than full-length
// stays a "-net" destination with the CIDR notation intact.
func TestRouteFamilyHostVsNet(t *testing.T) {
	d := &Device{
		name:   "tun7",
		v4Addr: netip.MustParseAddr("10.9.2.1"),
		v6Addr: netip.MustParseAddr("fd00::1"),
	}

	cases := []struct {
		name         string
		prefix       netip.Prefix
		wantFam      string
		wantDestFlag string
		wantDest     string
		wantGW       string
	}{
		{
			name:         "v4 host route (/32)",
			prefix:       netip.MustParsePrefix("10.9.2.55/32"),
			wantFam:      "-inet",
			wantDestFlag: "-host",
			wantDest:     "10.9.2.55", // bare address, no "/32" suffix
			wantGW:       "10.9.2.1",
		},
		{
			name:         "v4 subnet route (/24)",
			prefix:       netip.MustParsePrefix("10.50.0.0/24"),
			wantFam:      "-inet",
			wantDestFlag: "-net",
			wantDest:     "10.50.0.0/24",
			wantGW:       "10.9.2.1",
		},
		{
			name:         "v6 host route (/128)",
			prefix:       netip.MustParsePrefix("fd00:9:2::55/128"),
			wantFam:      "-inet6",
			wantDestFlag: "-host",
			wantDest:     "fd00:9:2::55", // bare address, no "/128" suffix
			wantGW:       "fd00::1",
		},
		{
			name:         "v6 subnet route (/64)",
			prefix:       netip.MustParsePrefix("fd00:50::/64"),
			wantFam:      "-inet6",
			wantDestFlag: "-net",
			wantDest:     "fd00:50::/64",
			wantGW:       "fd00::1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fam, destFlag, dest, gw, err := d.routeFamily(tc.prefix)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fam != tc.wantFam || destFlag != tc.wantDestFlag || dest != tc.wantDest || gw != tc.wantGW {
				t.Fatalf("routeFamily(%s) = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
					tc.prefix, fam, destFlag, dest, gw,
					tc.wantFam, tc.wantDestFlag, tc.wantDest, tc.wantGW)
			}
		})
	}
}

// TestRouteFamilyMissingAddr checks the error path when the relevant family's
// address hasn't been assigned yet (AddIPv4/AddIPv6 not called), which should
// produce a clear, actionable error rather than a zero-value gateway silently
// reaching exec.Command.
func TestRouteFamilyMissingAddr(t *testing.T) {
	d := &Device{name: "tun7", v4Addr: netip.MustParseAddr("10.9.2.1")}
	if _, _, _, _, err := d.routeFamily(netip.MustParsePrefix("fd00:9:2::55/128")); err == nil {
		t.Fatal("expected an error for a v6 route with no v6 address assigned")
	}

	d2 := &Device{name: "tun7", v6Addr: netip.MustParseAddr("fd00::1")}
	if _, _, _, _, err := d2.routeFamily(netip.MustParsePrefix("10.9.2.55/32")); err == nil {
		t.Fatal("expected an error for a v4 route with no v4 address assigned")
	}
}
