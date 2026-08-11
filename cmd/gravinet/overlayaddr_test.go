package main

import (
	"errors"
	"net/netip"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// nullDev satisfies mesh.Device without a kernel behind it: this test only
// needs the engine to hold a network's self addresses.
type nullDev struct{}

func (nullDev) Read(p []byte) (int, error)       { select {} }
func (nullDev) Write(p []byte) (int, error)      { return len(p), nil }
func (nullDev) Name() string                     { return "nulldev0" }
func (nullDev) MTU() int                         { return 1400 }
func (nullDev) Close() error                     { return nil }
func (nullDev) AddIPv4(netip.Addr, int) error    { return nil }
func (nullDev) AddIPv6(netip.Addr, int) error    { return nil }
func (nullDev) AddRoute(netip.Prefix, int) error { return nil }
func (nullDev) DelRoute(netip.Prefix, int) error { return nil }
func (nullDev) IfIndex() (int32, error)          { return 0, errors.New("no index") }

// A changed overlay address must be detected, because it is the one setting
// ReloadRuntime cannot absorb: without this the address is written to config
// and quietly ignored, and the old one comes back on the next page load.
func TestOverlayAddrChanged(t *testing.T) {
	e := mesh.NewEngine(mesh.Options{NodeID: "self", Nets: []mesh.NetSpec{{
		ID: 1, Name: "lan", Dev: nullDev{},
		Self4: netip.MustParseAddr("10.42.0.5"),
		Self6: netip.MustParseAddr("fd00:42::5"),
	}}})

	for _, tc := range []struct {
		name string
		n    config.Network
		want bool
	}{
		{"unchanged", config.Network{Address4: "10.42.0.5/16"}, false},
		{"v4 changed", config.Network{Address4: "10.42.0.9/16"}, true},
		{"v6 changed", config.Network{Address6: "fd00:42::9/64"}, true},
		{"both, only v6 changed", config.Network{Address4: "10.42.0.5/16", Address6: "fd00:42::9/64"}, true},
		// Empty means "self-assign", and the address DAD already picked is a
		// valid answer to that — so clearing the field must not force a
		// rebuild and cost every session on the network for nothing.
		{"cleared means self-assign, not a change", config.Network{}, false},
		// A prefix length change alone is not an address change: the address
		// the engine holds is the same one.
		{"prefix length only", config.Network{Address4: "10.42.0.5/24"}, false},
		{"unparseable is not a change", config.Network{Address4: "nonsense"}, false},
	} {
		if got := overlayAddrChanged(e, 1, tc.n); got != tc.want {
			t.Errorf("%s: overlayAddrChanged = %v, want %v", tc.name, got, tc.want)
		}
	}

	// A network the engine does not have cannot have changed.
	if overlayAddrChanged(e, 999, config.Network{Address4: "10.42.0.9/16"}) {
		t.Error("an unknown network should not report a change")
	}
}
