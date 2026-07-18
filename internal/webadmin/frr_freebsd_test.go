//go:build freebsd

package webadmin

import (
	"reflect"
	"testing"

	"gravinet/internal/config"
)

// freebsdNeededDaemons must always lead with mgmtd then zebra — FreeBSD's frr
// rc.d script requires that exact order for anything else in frr_daemons to
// start — followed by whatever the OS-agnostic neededDaemons(b) says this
// config needs (staticd always, bgpd/bfdd as configured).
func TestFreebsdNeededDaemons(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.BGPConfig
		want []string
	}{
		{
			name: "BGP disabled",
			cfg:  config.BGPConfig{},
			want: []string{"mgmtd", "zebra", "staticd"},
		},
		{
			name: "BGP enabled, no BFD",
			cfg:  config.BGPConfig{Enabled: true, ASN: 65001},
			want: []string{"mgmtd", "zebra", "staticd", "bgpd"},
		},
		{
			name: "BGP enabled with per-neighbor BFD",
			cfg: config.BGPConfig{
				Enabled: true, ASN: 65001,
				Neighbors: []config.BGPNeighbor{{Peer: "10.0.0.2", RemoteAS: 65002, BFD: true}},
			},
			want: []string{"mgmtd", "zebra", "staticd", "bgpd", "bfdd"},
		},
		{
			name: "BGP enabled, BFD on one of several neighbors",
			cfg: config.BGPConfig{
				Enabled: true, ASN: 65001,
				Neighbors: []config.BGPNeighbor{
					{Peer: "10.0.0.2", RemoteAS: 65002, BFD: false},
					{Peer: "10.0.0.3", RemoteAS: 65003, BFD: true},
				},
			},
			want: []string{"mgmtd", "zebra", "staticd", "bgpd", "bfdd"},
		},
	}
	for _, c := range cases {
		got := freebsdNeededDaemons(c.cfg)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: freebsdNeededDaemons = %v, want %v", c.name, got, c.want)
		}
	}
}

// freebsdNeededDaemons must not alias or mutate freebsdBaseDaemons — each
// call appends onto a fresh slice, so two calls in a row (as applyBGP makes,
// once for managedDaemonSet and once inside syncDaemons) can't corrupt one
// another via shared backing-array writes.
func TestFreebsdNeededDaemonsNoAliasing(t *testing.T) {
	first := freebsdNeededDaemons(config.BGPConfig{Enabled: true, ASN: 65001})
	second := freebsdNeededDaemons(config.BGPConfig{})
	if len(freebsdBaseDaemons) != 2 || freebsdBaseDaemons[0] != "mgmtd" || freebsdBaseDaemons[1] != "zebra" {
		t.Fatalf("freebsdBaseDaemons mutated: %v", freebsdBaseDaemons)
	}
	if reflect.DeepEqual(first, second) {
		t.Fatalf("expected different daemon sets for different configs, got %v both times", first)
	}
}

// managedDaemonSet is what applyBGP checks daemon liveness against and
// reports the count of; on FreeBSD it must be the full frr_daemons list
// (mgmtd/zebra included), not just the OS-agnostic neededDaemons(b), since
// those two are just as much a part of what frr_daemons gets set to as bgpd
// or staticd are.
func TestManagedDaemonSetFreeBSD(t *testing.T) {
	cfg := config.BGPConfig{Enabled: true, ASN: 65001}
	got := managedDaemonSet(cfg)
	want := freebsdNeededDaemons(cfg)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("managedDaemonSet = %v, want %v (== freebsdNeededDaemons)", got, want)
	}
	for _, d := range []string{"mgmtd", "zebra"} {
		found := false
		for _, g := range got {
			if g == d {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("managedDaemonSet missing required baseline daemon %q: %v", d, got)
		}
	}
}
