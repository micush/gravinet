package webadmin

import (
	"reflect"
	"testing"

	"gravinet/internal/config"
)

func coupleCfg(t *testing.T) *config.Config {
	t.Helper()
	c := &config.Config{
		UDPPorts: []int{51820}, EnableIPv4: true,
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}},
	}
	c.Networks[0].Seeds = config.SeedList{
		{Address: "74.208.225.216:65432,443", Node: "ionos1"},
		{Address: "66.179.240.44:65432,443", Node: "ionos2"},
		{Address: "198.51.100.9:51820"}, // never handshaken: no attribution
	}
	return c
}

// Seed state and peer state mirror each other in all four directions. This
// walks every one of them against the same config, because the failure this
// replaced was precisely a coupling that worked in some directions and left
// the others to contradict it.
func TestSeedPeerCouplingIsSymmetric(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
	}{{"disable", false}, {"enable", true}} {
		t.Run("seed "+tc.name+" moves the peer", func(t *testing.T) {
			c := coupleCfg(t)
			if !tc.on {
				// start from a state the change actually moves
			} else {
				_ = c.SeedSetEnabled("lan", "74.208.225.216:65432,443", false)
				_ = c.PeerSetEnabled("lan", "ionos1", false)
			}
			if err := c.SeedSetEnabled("lan", "74.208.225.216:65432,443", tc.on); err != nil {
				t.Fatal(err)
			}
			node, err := coupleSeedState(c, "lan", "74.208.225.216:65432,443", tc.on)
			if err != nil {
				t.Fatal(err)
			}
			if node != "ionos1" {
				t.Fatalf("seed %s should move node ionos1, got %q", tc.name, node)
			}
			if got := peerEnabled(&c.Networks[0], "ionos1"); got != tc.on {
				t.Fatalf("peer enabled = %v, want %v", got, tc.on)
			}
			// Blast radius: the unrelated node is untouched either way.
			if !peerEnabled(&c.Networks[0], "ionos2") {
				t.Fatal("an unrelated node must not move with another seed")
			}
		})

		t.Run("peer "+tc.name+" moves the seed", func(t *testing.T) {
			c := coupleCfg(t)
			if tc.on {
				_ = c.SeedSetEnabled("lan", "74.208.225.216:65432,443", false)
				_ = c.PeerSetEnabled("lan", "ionos1", false)
			}
			if err := c.PeerSetEnabled("lan", "ionos1", tc.on); err != nil {
				t.Fatal(err)
			}
			changed, err := couplePeerState(c, "lan", "ionos1", tc.on)
			if err != nil {
				t.Fatal(err)
			}
			if want := []string{"74.208.225.216:65432,443"}; !reflect.DeepEqual(changed, want) {
				t.Fatalf("peer %s should move its seed: got %v want %v", tc.name, changed, want)
			}
			if got := seedEnabled(&c.Networks[0], "74.208.225.216:65432,443"); got != tc.on {
				t.Fatalf("seed enabled = %v, want %v", got, tc.on)
			}
			// The other node's seed, and the unattributed one, stay put.
			if !seedEnabled(&c.Networks[0], "66.179.240.44:65432,443") {
				t.Fatal("another node's seed must not move")
			}
			if !seedEnabled(&c.Networks[0], "198.51.100.9:51820") {
				t.Fatal("an unattributed seed must not move")
			}
		})
	}
}

// A full round trip has to land exactly where it started, in both entry
// points, or the four directions disagree about what the coupled state is.
func TestSeedPeerCouplingRoundTrip(t *testing.T) {
	for _, entry := range []string{"seed", "peer"} {
		c := coupleCfg(t)
		set := func(on bool) {
			t.Helper()
			if entry == "seed" {
				if err := c.SeedSetEnabled("lan", "74.208.225.216:65432,443", on); err != nil {
					t.Fatal(err)
				}
				if _, err := coupleSeedState(c, "lan", "74.208.225.216:65432,443", on); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err := c.PeerSetEnabled("lan", "ionos1", on); err != nil {
				t.Fatal(err)
			}
			if _, err := couplePeerState(c, "lan", "ionos1", on); err != nil {
				t.Fatal(err)
			}
		}
		set(false)
		if peerEnabled(&c.Networks[0], "ionos1") || seedEnabled(&c.Networks[0], "74.208.225.216:65432,443") {
			t.Fatalf("%s entry: both sides should be off", entry)
		}
		set(true)
		if !peerEnabled(&c.Networks[0], "ionos1") || !seedEnabled(&c.Networks[0], "74.208.225.216:65432,443") {
			t.Fatalf("%s entry: both sides should be back on", entry)
		}
		if len(c.Networks[0].DisabledPeers) != 0 {
			t.Fatalf("%s entry: round trip left residue: %v", entry, c.Networks[0].DisabledPeers)
		}
	}
}

// A seed that has never completed a handshake has no node to couple to. The
// contract is that this reports "" so the caller can say so — silently
// changing nothing is what makes an operator conclude the toggle is broken,
// which is the entire complaint this feature answers.
func TestSeedStateWithoutAttributionReportsUnknown(t *testing.T) {
	c := coupleCfg(t)
	node, err := coupleSeedState(c, "lan", "198.51.100.9:51820", false)
	if err != nil {
		t.Fatal(err)
	}
	if node != "" {
		t.Fatalf("an unattributed seed has no node to move, got %q", node)
	}
	if len(c.Networks[0].DisabledPeers) != 0 {
		t.Fatalf("nothing should have been disabled: %v", c.Networks[0].DisabledPeers)
	}
	if !n0SeedOwner(c, "lan", "198.51.100.9:51820") {
		t.Fatal("the unattributed case must be distinguishable from a no-op")
	}
	if n0SeedOwner(c, "lan", "74.208.225.216:65432,443") {
		t.Fatal("an attributed seed must not report as unattributed")
	}
}

// Coupling a state that already matches reports nothing, rather than claiming
// it changed something. This is what lets the handler stay quiet on an
// ordinary toggle and speak up only when it did something wider.
func TestCouplingReportsOnlyRealChanges(t *testing.T) {
	c := coupleCfg(t)
	if node, err := coupleSeedState(c, "lan", "74.208.225.216:65432,443", true); err != nil || node != "" {
		t.Fatalf("peer already enabled: node=%q err=%v", node, err)
	}
	if changed, err := couplePeerState(c, "lan", "ionos1", true); err != nil || len(changed) != 0 {
		t.Fatalf("seed already enabled: changed=%v err=%v", changed, err)
	}
	if changed, err := couplePeerState(c, "lan", "some-gossiped-node", false); err != nil || len(changed) != 0 {
		t.Fatalf("unknown node: changed=%v err=%v", changed, err)
	}
}

// syncSeedNodes is what makes the coupling work at all, because the handshake
// that proves who is behind an address cannot run once the seed is parked —
// so the association has to already be recorded before the disable.
func TestSyncSeedNodesLearnsAttribution(t *testing.T) {
	c := coupleCfg(t)
	c.Networks[0].Seeds[2].Node = ""
	owners := map[uint64]map[string]string{
		0x1234: {
			// Bare-address key: the handshake completed on a port that is
			// not the one written in the seed string, which is the norm for
			// a multi-port seed.
			"198.51.100.9": "debian1",
		},
	}
	if !syncSeedNodes(c, owners) {
		t.Fatal("expected the new attribution to be recorded")
	}
	if got := c.Networks[0].Seeds[2].Node; got != "debian1" {
		t.Fatalf("seed node = %q, want debian1", got)
	}
	// Idempotent: a second pass changes nothing.
	if syncSeedNodes(c, owners) {
		t.Error("re-running with the same attribution should report no change")
	}
	// An address the engine currently knows nothing about keeps whatever it
	// already had — forgetting it here would destroy the association exactly
	// when the re-enable path needs it.
	if got := c.Networks[0].Seeds[0].Node; got != "ionos1" {
		t.Errorf("existing attribution must survive a sync that doesn't mention it, got %q", got)
	}
}

// The address a seed is written as is frequently not the address its
// handshake completed on. seedHostKey is what bridges that, so it has to
// handle every shape an operator can legitimately write.
func TestSeedHostKey(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"74.208.225.216:65432,443", "74.208.225.216"}, // multi-port list
		{"74.208.225.216:443", "74.208.225.216"},       // single port
		{"74.208.225.216", "74.208.225.216"},           // bare
		{"tcp://66.179.240.44:443", "66.179.240.44"},   // scheme stripped
		{"udp://198.51.100.9", "198.51.100.9"},
		{"[2607:f1c0:f00c:db00::1]:443", "2607:f1c0:f00c:db00::1"}, // v6 with port
		{"2607:f1c0:f00c:db00::1", "2607:f1c0:f00c:db00::1"},       // v6 bare
		{"::ffff:198.51.100.9", "198.51.100.9"},                    // v4-mapped canonicalized
		{"seed.example.com:443", ""},                               // hostname: no attribution
		{"seed.example.com", ""},
		{"", ""},
	} {
		if got := seedHostKey(tc.in); got != tc.want {
			t.Errorf("seedHostKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
