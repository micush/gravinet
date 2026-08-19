package main

import (
	"net/netip"
	"os"
	"regexp"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/mesh"
)

// A disabled seed has exactly one job: not be dialed. The way that breaks is
// not a wrong answer from EnabledAddrs — it is a call site that reaches for
// SeedList.Addrs() instead, which returns every configured seed, compiles
// cleanly, passes every type check, and quietly dials the address an operator
// just took out of service. Nothing about the resulting behavior looks wrong
// until you notice the node is still talking to a seed the UI renders as
// "disabled".
//
// So this asserts the wiring, not the function: the seed lists that main.go
// hands to resolveSeeds/resolveTCPSeeds must come from EnabledAddrs. Guarding
// it at the source, the way navparity_test.go guards the CLI/GUI nav split,
// is the only place the mistake is visible — by the time it reaches a
// NetSpec, an enabled and a disabled seed are indistinguishable []string
// entries.
func TestSeedResolutionUsesEnabledAddrs(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Skipf("can't read main.go (%v) — nothing to check against", err)
	}
	src := string(b)

	// Sanity check the guard itself: if the expected form has vanished
	// entirely, this test is silently guarding nothing and should say so
	// rather than report success.
	if !strings.Contains(src, "n.Seeds.EnabledAddrs()") {
		t.Fatal("no n.Seeds.EnabledAddrs() call found in main.go — seed resolution moved or was renamed; this guard needs updating, not deleting")
	}

	bad := regexp.MustCompile(`n\.Seeds\.Addrs\(\)`)
	for i, line := range strings.Split(src, "\n") {
		if bad.MatchString(line) {
			t.Errorf("main.go:%d resolves seeds from Addrs(), which includes disabled ones — use EnabledAddrs():\n\t%s", i+1, strings.TrimSpace(line))
		}
	}
}

// The end of the same wiring, checked for real rather than by reading source:
// a disabled seed must not survive into the resolved endpoint list that
// becomes NetSpec.Seeds. resolveSeeds itself knows nothing about enablement —
// it takes a []string — so this pins the composition the daemon actually
// performs.
func TestResolveSeedsSkipsDisabled(t *testing.T) {
	n := config.Network{Seeds: config.SeedList{
		{Address: "198.51.100.7:60000"},
		{Address: "203.0.113.9:60001", Disabled: true},
	}}

	got := resolveSeeds(n.Seeds.EnabledAddrs(), config.DefaultUDPPort, nil)
	if len(got) != 1 {
		t.Fatalf("resolved %d endpoints, want 1: %v", len(got), got)
	}
	if got[0].Port() != 60000 {
		t.Errorf("resolved the wrong seed: %v", got[0])
	}
	for _, ap := range got {
		if ap.Port() == 60001 {
			t.Errorf("disabled seed %v was resolved into the dial set", ap)
		}
	}

	// Disabling everything yields an empty dial set, not the full one — the
	// degenerate case a fallback-to-Addrs bug would quietly paper over.
	n.Seeds[0].Disabled = true
	if got := resolveSeeds(n.Seeds.EnabledAddrs(), config.DefaultUDPPort, nil); len(got) != 0 {
		t.Errorf("all seeds disabled should resolve to nothing, got %v", got)
	}
}

// applySeedState is where a disabled seed is actually kept out of the dial
// set, and the case that matters is the overlap with PeerCache. The dial set
// deliberately folds PeerCache in, and PeerCache is exactly where a seed this
// node has connected to before will be sitting — so if the subtraction were
// missing, disabling would silently do nothing for the seeds most likely to
// be disabled. Merely omitting the disabled address from EnabledAddrs is not
// enough; it has to be removed after the fold.
func TestApplySeedStateSubtractsDisabledFromPeerCache(t *testing.T) {
	disabled := "203.0.113.5:60000"
	n := config.Network{
		Seeds: config.SeedList{
			{Address: "198.51.100.9:60001"},
			{Address: disabled, Disabled: true},
		},
		// The disabled seed is also a known peer endpoint — the common case,
		// since PeerCache is populated from peers actually connected to.
		PeerCache: []string{disabled, "192.0.2.30:60002"},
	}

	var spec mesh.NetSpec
	applySeedState(&spec, n, config.DefaultUDPPort, 65432, nil)

	want := netip.MustParseAddrPort(disabled)
	for _, ap := range spec.Seeds {
		if ap == want {
			t.Fatalf("a disabled seed must not re-enter the dial set through PeerCache: %v", spec.Seeds)
		}
	}
	if len(spec.RetiredSeeds) == 0 {
		t.Fatal("the disabled seed should be reported as retired so the engine can drop its session")
	}
	found := false
	for _, ap := range spec.RetiredSeeds {
		if ap == want {
			found = true
		}
	}
	if !found {
		t.Errorf("RetiredSeeds = %v, want it to contain %v", spec.RetiredSeeds, want)
	}

	// The other addresses are untouched.
	var haveEnabled, haveCache bool
	for _, ap := range spec.Seeds {
		switch ap {
		case netip.MustParseAddrPort("198.51.100.9:60001"):
			haveEnabled = true
		case netip.MustParseAddrPort("192.0.2.30:60002"):
			haveCache = true
		}
	}
	if !haveEnabled {
		t.Error("an enabled seed must stay in the dial set")
	}
	if !haveCache {
		t.Error("an unrelated PeerCache address must stay in the dial set")
	}

	// ConfiguredSeeds is the operator's own list and must exclude the
	// disabled entry too — it is what upgrade sequencing reads as "this
	// node's seeds".
	for _, ap := range spec.ConfiguredSeeds {
		if ap == want {
			t.Errorf("a disabled seed must not count as a configured seed: %v", spec.ConfiguredSeeds)
		}
	}
}

// With nothing disabled there is nothing to retire, and the dial set is
// exactly what it was before this feature existed.
func TestApplySeedStateNoDisabledSeeds(t *testing.T) {
	n := config.Network{
		Seeds:     config.SeedList{{Address: "198.51.100.9:60001"}},
		PeerCache: []string{"192.0.2.30:60002"},
	}
	var spec mesh.NetSpec
	applySeedState(&spec, n, config.DefaultUDPPort, 65432, nil)

	if len(spec.RetiredSeeds) != 0 || len(spec.RetiredTCPSeeds) != 0 {
		t.Fatalf("nothing disabled should retire nothing: %v %v", spec.RetiredSeeds, spec.RetiredTCPSeeds)
	}
	if len(spec.Seeds) != 2 {
		t.Fatalf("dial set = %v, want both the seed and the peer-cache address", spec.Seeds)
	}
}

// A disabled tcp:// seed retires on the TCP side, where the TCP dialer
// would otherwise keep priming it independently of the UDP seed set.
func TestApplySeedStateRetiresDisabledTCPSeed(t *testing.T) {
	n := config.Network{Seeds: config.SeedList{
		{Address: "tcp://203.0.113.5:65432", Disabled: true},
	}}
	var spec mesh.NetSpec
	applySeedState(&spec, n, config.DefaultUDPPort, 65432, nil)

	if len(spec.RetiredTCPSeeds) == 0 {
		t.Fatal("a disabled tcp:// seed should be retired on the TCP side")
	}
	if len(spec.TCPSeeds) != 0 {
		t.Errorf("TCPSeeds = %v, want empty", spec.TCPSeeds)
	}
}
