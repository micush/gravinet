package config

import (
	"testing"
	"time"
)

func TestKeyOps(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "corp", ID: "0000000000000001"}}}
	c.Networks[0].Keys[0] = KeySlot{Key: "AAAA", Label: "initial", Enabled: true} // placeholder

	slot, key, err := c.KeyGenerate("corp", "rot1")
	if err != nil || slot != 1 || key == "" {
		t.Fatalf("KeyGenerate: slot=%d key=%q err=%v", slot, key, err)
	}
	if c.Networks[0].Keys[1].Label != "rot1" || !c.Networks[0].Keys[1].Enabled {
		t.Errorf("generated slot not set correctly: %+v", c.Networks[0].Keys[1])
	}
	if err := c.KeySetEnabled("corp", 0, false); err != nil {
		t.Errorf("disable slot 0: %v", err)
	}
	if err := c.KeySetEnabled("corp", 1, false); err == nil {
		t.Errorf("disabling last enabled key should fail")
	}
	if err := c.KeyDelete("corp", 0); err != nil {
		t.Errorf("delete slot 0: %v", err)
	}
	if c.Networks[0].Keys[0].Key != "" {
		t.Errorf("slot 0 should be empty after delete")
	}
	if err := c.KeyDelete("corp", 1); err == nil {
		t.Errorf("deleting last enabled key should fail")
	}
	if err := c.KeySetEnabled("corp", 99, true); err == nil {
		t.Errorf("slot 99 should be out of range")
	}
	if got, err := c.KeyReveal("corp", 1); err != nil || got != key {
		t.Errorf("KeyReveal = %q,%v want %q", got, err, key)
	}
}

func TestFindNetworkAndID(t *testing.T) {
	c := &Config{Networks: []Network{
		{Name: "corp", ID: "00000000986bc4a3"},
		{Name: "lab", ID: "f1d458279d4d8866"},
	}}
	cases := []struct {
		ref      string
		wantName string
	}{
		{"corp", "corp"},             // by name
		{"lab", "lab"},               // by name
		{"00000000986bc4a3", "corp"}, // by exact (zero-padded) id
		{"986bc4a3", "corp"},         // by trimmed id (as shown in status / web)
		{"F1D458279D4D8866", "lab"},  // hex is case-insensitive
		{"nope", ""},                 // unknown name
		{"deadbeefdeadbeef", ""},     // unknown id
	}
	for _, tc := range cases {
		n := c.FindNetwork(tc.ref)
		got := ""
		if n != nil {
			got = n.Name
		}
		if got != tc.wantName {
			t.Errorf("FindNetwork(%q) = %q, want %q", tc.ref, got, tc.wantName)
		}
	}

	// NetworkID resolves a name to the numeric engine id.
	if id, ok := c.NetworkID("corp"); !ok || id != 0x986bc4a3 {
		t.Errorf("NetworkID(corp) = %x,%v want 986bc4a3,true", id, ok)
	}
	if _, ok := c.NetworkID("nope"); ok {
		t.Errorf("NetworkID(nope) should be false")
	}
}

func TestNetworkRename(t *testing.T) {
	c := &Config{Networks: []Network{
		{Name: "corp", ID: "0000000000000001", Subnet4: "10.1.0.0/16"},
		{Name: "lab", ID: "0000000000000002", Subnet4: "10.2.0.0/16"},
	}}
	if err := c.NetworkRename("corp", "office"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if c.Networks[0].Name != "office" {
		t.Fatalf("name not changed: %q", c.Networks[0].Name)
	}
	if c.Networks[0].ID != "0000000000000001" {
		t.Fatalf("rename must not change id; got %q", c.Networks[0].ID)
	}
	if err := c.NetworkRename("office", "lab"); err == nil {
		t.Fatalf("expected collision error renaming to an existing name")
	}
	if err := c.NetworkRename("office", "  "); err == nil {
		t.Fatalf("expected error on blank name")
	}
	if err := c.NetworkRename("nope", "x"); err == nil {
		t.Fatalf("expected error renaming unknown network")
	}
}

func TestNetworkSetSubnets(t *testing.T) {
	c := &Config{Networks: []Network{
		{Name: "corp", ID: "0000000000000001", Subnet4: "10.1.0.0/16", Subnet6: "fd00:1::/64"},
	}}
	n := &c.Networks[0]

	// Change v4, leave v6 unchanged (empty = keep).
	if err := c.NetworkSetSubnets("corp", "10.9.0.0/16", ""); err != nil {
		t.Fatalf("set v4: %v", err)
	}
	if n.Subnet4 != "10.9.0.0/16" || n.Subnet6 != "fd00:1::/64" {
		t.Fatalf("unexpected subnets: %s %s", n.Subnet4, n.Subnet6)
	}
	// Clear v6 with "none", keep v4.
	if err := c.NetworkSetSubnets("corp", "", "none"); err != nil {
		t.Fatalf("clear v6: %v", err)
	}
	if n.Subnet4 != "10.9.0.0/16" || n.Subnet6 != "" {
		t.Fatalf("v6 not cleared: %s %s", n.Subnet4, n.Subnet6)
	}
	// Refuse to clear the last family.
	if err := c.NetworkSetSubnets("corp", "none", ""); err == nil {
		t.Fatalf("expected error clearing the only remaining subnet")
	}
	// Reject wrong family in the v4 slot.
	if err := c.NetworkSetSubnets("corp", "fd00::/64", ""); err == nil {
		t.Fatalf("expected family validation error")
	}
	// Unknown network.
	if err := c.NetworkSetSubnets("nope", "10.0.0.0/8", ""); err == nil {
		t.Fatalf("expected error for unknown network")
	}
}

func TestNetworkSetRedistributeBGPRoutes(t *testing.T) {
	c := &Config{Networks: []Network{
		{Name: "corp", ID: "0000000000000001", Subnet4: "10.1.0.0/16"},
	}}
	n := &c.Networks[0]

	if len(n.RedistributeBGPRoutes) != 0 {
		t.Fatal("expected RedistributeBGPRoutes empty by default")
	}
	if err := c.NetworkSetRedistributeBGPRoutes("corp", []string{"172.16.0.0/24", "172.16.1.0/24"}, 42); err != nil {
		t.Fatalf("set: %v", err)
	}
	if len(n.RedistributeBGPRoutes) != 2 || n.RedistributeBGPMetric != 42 {
		t.Fatalf("got routes=%v metric=%d, want 2 routes metric=42", n.RedistributeBGPRoutes, n.RedistributeBGPMetric)
	}
	// Clearing the selection still updates the metric in the same call — the
	// UI posts both together (see ui.go's redistribute-bgp picker/metric input).
	if err := c.NetworkSetRedistributeBGPRoutes("corp", nil, 7); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if len(n.RedistributeBGPRoutes) != 0 || n.RedistributeBGPMetric != 7 {
		t.Fatalf("got routes=%v metric=%d, want empty metric=7", n.RedistributeBGPRoutes, n.RedistributeBGPMetric)
	}
	if err := c.NetworkSetRedistributeBGPRoutes("nope", []string{"10.0.0.0/24"}, 0); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

func TestThrottleSetPreservesEnabled(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "n", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}
	tn := &c.Networks[0]

	// Disabled: setting a rate stores it but must NOT enable the limiter.
	if err := c.ThrottleSet("n", "up", 5_000_000); err != nil {
		t.Fatalf("set up: %v", err)
	}
	if tn.Throttle.Enabled {
		t.Fatal("setting a rate while disabled must not enable the limiter")
	}
	if tn.Throttle.UpBytesPerSec != 5_000_000 {
		t.Fatalf("rate not stored: up=%d", tn.Throttle.UpBytesPerSec)
	}
	// Editing the other direction must also leave the (still disabled) state alone.
	if err := c.ThrottleSet("n", "down", 1_000_000); err != nil {
		t.Fatalf("set down: %v", err)
	}
	if tn.Throttle.Enabled {
		t.Fatal("editing a second rate must not flip the enabled state")
	}

	// Now turn it on explicitly, then edit rates: it must STAY on.
	if err := c.ThrottleSetEnabled("n", true); err != nil {
		t.Fatal(err)
	}
	if err := c.ThrottleSet("n", "up", 0); err != nil { // clear the up cap to unlimited
		t.Fatalf("clear up: %v", err)
	}
	if !tn.Throttle.Enabled {
		t.Fatal("clearing one rate must not disable an explicitly-enabled limiter")
	}
	if err := c.ThrottleSet("n", "down", 0); err != nil { // clear the down cap too
		t.Fatalf("clear down: %v", err)
	}
	if !tn.Throttle.Enabled {
		t.Fatal("clearing all rates must not disable; only the toggle does that")
	}
}

func TestThrottleSetEnabledKeepsRates(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "n", ID: "0000000000000001", Subnet4: "10.1.0.0/16",
		Throttle: Throttle{Enabled: true, UpBytesPerSec: 5_000_000}}}}
	tn := &c.Networks[0]

	// Disabling must not discard the configured rate.
	if err := c.ThrottleSetEnabled("n", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if tn.Throttle.Enabled || tn.Throttle.UpBytesPerSec != 5_000_000 {
		t.Fatalf("disable should keep the rate: enabled=%v up=%d", tn.Throttle.Enabled, tn.Throttle.UpBytesPerSec)
	}
	// Re-enabling restores the limit with the same rate.
	if err := c.ThrottleSetEnabled("n", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if !tn.Throttle.Enabled || tn.Throttle.UpBytesPerSec != 5_000_000 {
		t.Fatalf("enable should restore the rate: enabled=%v up=%d", tn.Throttle.Enabled, tn.Throttle.UpBytesPerSec)
	}
}

func TestNetworkJoinByID(t *testing.T) {
	c := &Config{}
	const key = "Gw+MBYbuVb7OEzjnd6jZqtCXgBpUBjNQub7ves3WQ3M="
	// Join an unknown network by id: creates a bare network (name/subnet learned later).
	if err := c.NetworkJoin("e4ba5a47668a1465", key, "203.0.113.5:51820", "", ""); err != nil {
		t.Fatalf("join: %v", err)
	}
	if len(c.Networks) != 1 {
		t.Fatalf("expected 1 network, got %d", len(c.Networks))
	}
	n := c.Networks[0]
	if n.ID != "e4ba5a47668a1465" {
		t.Fatalf("id not set from join: %q", n.ID)
	}
	if n.Name != "" || n.Subnet4 != "" || n.Subnet6 != "" {
		t.Fatalf("name/subnet should be blank (learned from network): name=%q s4=%q", n.Name, n.Subnet4)
	}
	if len(n.Seeds) != 1 || n.Seeds[0].Address != "203.0.113.5:51820" {
		t.Fatalf("seed not recorded: %v", n.Seeds)
	}
	if !n.Keys[0].Enabled || n.Keys[0].Key != key {
		t.Fatalf("key not set")
	}
	// A bad id is rejected.
	if err := c.NetworkJoin("not-hex", key, "", "", ""); err == nil {
		t.Fatalf("expected error on non-hex id")
	}
}

func TestValidateAllowsSeededNoSubnet(t *testing.T) {
	const key = "Gw+MBYbuVb7OEzjnd6jZqtCXgBpUBjNQub7ves3WQ3M="
	// A joined network with a seed but no subnet yet must validate (it will learn one).
	c := &Config{
		NodeID:      "abcd",
		PrimaryPort: 51820,
		EnableIPv4:  true,
		Networks:    []Network{{ID: "0000000000000001", MTU: 1400, Seeds: SeedList{{Address: "203.0.113.5"}}, Keys: [KeySlots]KeySlot{{Key: key, Enabled: true}}}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("seeded subnet-less network should validate: %v", err)
	}
	// But with no subnet AND no seed, it must fail.
	c.Networks[0].Seeds = nil
	if err := c.Validate(); err == nil {
		t.Fatalf("subnet-less network without a seed should fail validation")
	}
}

func TestValidateAcceptsPeerCacheOnly(t *testing.T) {
	const key = "Gw+MBYbuVb7OEzjnd6jZqtCXgBpUBjNQub7ves3WQ3M="
	c := &Config{
		NodeID: "abcd", PrimaryPort: 51820, EnableIPv4: true,
		Networks: []Network{{
			ID: "0000000000000001", MTU: 1400,
			PeerCache: []string{"198.51.100.7:51820"}, // bootstrap source, no seeds/subnet yet
			Keys:      [KeySlots]KeySlot{{Key: key, Enabled: true}},
		}},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("network with a peer_cache should validate without seeds/subnet: %v", err)
	}
}

// TestNetworkDeleteByIDAmongDuplicates ensures deleting by a unique ID removes
// only that network even when several share a name (the web UI targets by ID).
func TestNetworkDeleteByIDAmongDuplicates(t *testing.T) {
	c := &Config{Networks: []Network{
		{ID: "aaaa", Name: "dup", Subnet4: "10.70.0.0/24"},
		{ID: "bbbb", Name: "dup", Subnet4: "10.71.0.0/24"},
		{ID: "cccc", Name: "other"},
	}}
	if err := c.NetworkDelete("bbbb"); err != nil {
		t.Fatalf("delete by id: %v", err)
	}
	if len(c.Networks) != 2 {
		t.Fatalf("expected 2 networks left, got %d", len(c.Networks))
	}
	for _, n := range c.Networks {
		if n.ID == "bbbb" {
			t.Fatal("bbbb should be gone")
		}
	}
	// the other same-named network must survive
	var keptDup bool
	for _, n := range c.Networks {
		if n.ID == "aaaa" && n.Name == "dup" {
			keptDup = true
		}
	}
	if !keptDup {
		t.Fatal("the other 'dup' network (aaaa) must survive an id-targeted delete")
	}
}

// TestNetworkDeleteByNumericallyEqualID covers the bug where NetworkDelete did
// a plain string comparison instead of matching the way FindNetwork does (see
// its comment): /api/status and /api/config both hand the web UI an id that's
// numerically parsed and reformatted, which silently drops any leading zero
// nibble a stored, zero-padded id has. A network whose id happened to start
// with a zero hex digit could be renamed, enabled/disabled, and re-subnetted
// fine (those all go through FindNetwork already) but never deleted — the web
// UI would report "no network named <id>" for a network that plainly existed.
func TestNetworkDeleteByNumericallyEqualID(t *testing.T) {
	c := &Config{Networks: []Network{
		{ID: "0000000000001234", Name: "cush1", Subnet4: "10.70.0.0/24"},
		{ID: "000000000000abcd", Name: "cush2", Subnet4: "10.71.0.0/24"},
	}}
	// "1234" is numerically equal to the stored "0000000000001234" but not an
	// exact string match — exactly what a zero-trimmed id from the API sends.
	if err := c.NetworkDelete("1234"); err != nil {
		t.Fatalf("delete by numerically-equal id: %v", err)
	}
	if len(c.Networks) != 1 || c.Networks[0].Name != "cush2" {
		t.Fatalf("expected only cush2 left, got %+v", c.Networks)
	}
}

// TestKeyZeroLabelAndRelabel checks the first key is labelled "key0" (not
// "initial"/"joined") and that KeySetLabel changes only the label.
func TestKeyZeroLabelAndRelabel(t *testing.T) {
	dir := t.TempDir()
	c := &Config{PrimaryPort: DefaultUDPPort, EnableIPv4: true, path: dir + "/c.json"}
	if _, err := c.NetworkAdd("lan", "10.96.0.0/24", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	n := c.Networks[0]
	if n.Keys[0].Label != "key0" {
		t.Fatalf("key 0 label = %q, want key0", n.Keys[0].Label)
	}
	origKey := n.Keys[0].Key
	if err := c.KeySetLabel("lan", 0, "primary"); err != nil {
		t.Fatalf("relabel: %v", err)
	}
	if c.Networks[0].Keys[0].Label != "primary" {
		t.Fatalf("relabel did not take: %q", c.Networks[0].Keys[0].Label)
	}
	if c.Networks[0].Keys[0].Key != origKey {
		t.Fatal("relabel must not change the key material")
	}
	if err := c.KeySetLabel("lan", 4, "x"); err == nil {
		t.Fatal("relabel of an empty slot should error")
	}
}

func TestKeySetExpiry(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "n", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}
	if _, err := c.KeyGenerateInto("n", 0, "k0"); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	// Set a valid future expiry.
	future := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if err := c.KeySetExpiry("n", 0, future); err != nil {
		t.Fatalf("set expiry: %v", err)
	}
	if c.Networks[0].Keys[0].Expires != future {
		t.Fatalf("expiry not stored: %q", c.Networks[0].Keys[0].Expires)
	}
	if c.Networks[0].Keys[0].Expired(time.Now()) {
		t.Fatal("future expiry should not be expired now")
	}
	// A past expiry should read as expired.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := c.KeySetExpiry("n", 0, past); err != nil {
		t.Fatalf("set past expiry: %v", err)
	}
	if !c.Networks[0].Keys[0].Expired(time.Now()) {
		t.Fatal("past expiry should be expired")
	}
	// Clearing.
	if err := c.KeySetExpiry("n", 0, ""); err != nil {
		t.Fatalf("clear expiry: %v", err)
	}
	if c.Networks[0].Keys[0].Expires != "" {
		t.Fatal("expiry not cleared")
	}
	// Bad value rejected.
	if err := c.KeySetExpiry("n", 0, "not-a-date"); err == nil {
		t.Fatal("expected error on bad expiry")
	}
	// Empty slot rejected.
	if err := c.KeySetExpiry("n", 3, future); err == nil {
		t.Fatal("expected error setting expiry on empty slot")
	}
	// Validate rejects a bad stored value.
	c.Networks[0].Keys[0].Expires = "garbage"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject bad expires")
	}
}

func TestPeerSetEnabled(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "n", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}

	// disable two peers (local-only blocklist grows; idempotent on repeats)
	if err := c.PeerSetEnabled("n", "nodeB", false); err != nil {
		t.Fatal(err)
	}
	if err := c.PeerSetEnabled("n", "nodeC", false); err != nil {
		t.Fatal(err)
	}
	if err := c.PeerSetEnabled("n", "nodeB", false); err != nil { // repeat must not duplicate
		t.Fatal(err)
	}
	dp := c.Networks[0].DisabledPeers
	if len(dp) != 2 {
		t.Fatalf("DisabledPeers = %v, want exactly [nodeB nodeC]", dp)
	}

	// re-enable one removes only it
	if err := c.PeerSetEnabled("n", "nodeB", true); err != nil {
		t.Fatal(err)
	}
	dp = c.Networks[0].DisabledPeers
	if len(dp) != 1 || dp[0] != "nodeC" {
		t.Fatalf("after enabling nodeB, DisabledPeers = %v, want [nodeC]", dp)
	}

	// enabling a peer that isn't disabled is a no-op, not an error
	if err := c.PeerSetEnabled("n", "nodeZ", true); err != nil {
		t.Fatal(err)
	}
	if len(c.Networks[0].DisabledPeers) != 1 {
		t.Fatalf("enabling an absent peer changed the list: %v", c.Networks[0].DisabledPeers)
	}

	// empty node id is rejected
	if err := c.PeerSetEnabled("n", "", false); err == nil {
		t.Fatal("expected error for empty node id")
	}
}

func TestNetworkSetNotes(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "corp", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}
	if err := c.NetworkSetNotes("corp", "  staging environment  "); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Notes; got != "staging environment" {
		t.Fatalf("notes = %q, want trimmed %q", got, "staging environment")
	}
	// Clearing is allowed.
	if err := c.NetworkSetNotes("corp", ""); err != nil {
		t.Fatal(err)
	}
	if c.Networks[0].Notes != "" {
		t.Fatalf("notes after clear = %q, want empty", c.Networks[0].Notes)
	}
	if err := c.NetworkSetNotes("nope", "x"); err == nil {
		t.Fatal("expected error setting notes on unknown network")
	}
}

func TestKeySetNotes(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "n", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}
	if _, _, err := c.KeyGenerate("n", "k0"); err != nil {
		t.Fatal(err)
	}
	if err := c.KeySetNotes("n", 0, "rotated in after the March incident"); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Keys[0].Notes; got != "rotated in after the March incident" {
		t.Fatalf("notes = %q", got)
	}
	// Label is untouched by a notes edit.
	if c.Networks[0].Keys[0].Label != "k0" {
		t.Fatalf("label changed by notes edit: %q", c.Networks[0].Keys[0].Label)
	}
	// Setting notes on an empty slot is an error (matches KeySetLabel's rule).
	if err := c.KeySetNotes("n", 1, "x"); err == nil {
		t.Fatal("expected error setting notes on an empty slot")
	}
}

func TestPeerSetNotes(t *testing.T) {
	c := &Config{Networks: []Network{{Name: "n", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}
	if err := c.PeerSetNotes("n", "nodeB", "laptop, offsite"); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].PeerNotes["nodeB"]; got != "laptop, offsite" {
		t.Fatalf("notes = %q", got)
	}
	// Clearing removes the map entry entirely rather than leaving an empty string.
	if err := c.PeerSetNotes("n", "nodeB", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Networks[0].PeerNotes["nodeB"]; ok {
		t.Fatalf("expected nodeB removed from PeerNotes after clearing, got %v", c.Networks[0].PeerNotes)
	}
	// Empty node id is rejected, matching PeerSetEnabled.
	if err := c.PeerSetNotes("n", "", "x"); err == nil {
		t.Fatal("expected error for empty node id")
	}
}

func TestNetworkSetAddress(t *testing.T) {
	c := &Config{Networks: []Network{
		{Name: "corp", ID: "0000000000000001", Subnet4: "10.1.0.0/16", Subnet6: "fd00:1::/64"},
	}}
	n := &c.Networks[0]

	// Set v4 within the subnet; leave v6 unchanged (empty = keep).
	if err := c.NetworkSetAddress("corp", "10.1.0.5/16", ""); err != nil {
		t.Fatalf("set v4: %v", err)
	}
	if n.Address4 != "10.1.0.5/16" || n.Address6 != "" {
		t.Fatalf("unexpected addresses: %q %q", n.Address4, n.Address6)
	}
	// Set v6 within the subnet.
	if err := c.NetworkSetAddress("corp", "", "fd00:1::5/64"); err != nil {
		t.Fatalf("set v6: %v", err)
	}
	if n.Address6 != "fd00:1::5/64" {
		t.Fatalf("v6 not set: %q", n.Address6)
	}
	// "none" clears (restores auto-assignment), keeping v4.
	if err := c.NetworkSetAddress("corp", "", "none"); err != nil {
		t.Fatalf("clear v6: %v", err)
	}
	if n.Address4 != "10.1.0.5/16" || n.Address6 != "" {
		t.Fatalf("v6 not cleared: %q %q", n.Address4, n.Address6)
	}
	// An address outside the subnet is rejected.
	if err := c.NetworkSetAddress("corp", "192.168.0.5/24", ""); err == nil {
		t.Fatal("expected error for address outside subnet4")
	}
	// Wrong family in the v4 slot is rejected.
	if err := c.NetworkSetAddress("corp", "fd00:1::9/64", ""); err == nil {
		t.Fatal("expected family error in v4 slot")
	}
	// A bare address without a prefix is rejected.
	if err := c.NetworkSetAddress("corp", "10.1.0.9", ""); err == nil {
		t.Fatal("expected error for missing prefix length")
	}
	// Unknown network.
	if err := c.NetworkSetAddress("nope", "10.1.0.9/16", ""); err == nil {
		t.Fatal("expected error for unknown network")
	}
}

// TestNetworkSetAddressRejectsMismatchedPrefixLength is the regression test
// for a real live incident: an address that falls inside the subnet but
// doesn't carry the subnet's own prefix length — most naturally a /32,
// since that's how anyone would type "my one address" — used to be accepted
// outright. gravinet assigns the overlay address as a point-to-point pair
// with that prefix length standing in for the netmask (see tun_darwin.go's
// AddIPv4), so a /32 there silently produces a working address with no route
// to the rest of the subnet: the node itself pings fine, but every peer
// address outside its own /32 is unreachable, which is indistinguishable
// from a mesh outage until someone thinks to check `ifconfig`/`netstat -rn`.
func TestNetworkSetAddressRejectsMismatchedPrefixLength(t *testing.T) {
	c := &Config{Networks: []Network{
		{Name: "corp", ID: "0000000000000001", Subnet4: "192.168.203.0/24", Subnet6: "fd00:203::/64"},
	}}
	n := &c.Networks[0]

	// The exact shape of the live incident: address inside the subnet, but
	// pinned as a /32 instead of the subnet's /24.
	if err := c.NetworkSetAddress("corp", "192.168.203.140/32", ""); err == nil {
		t.Fatal("expected error for a /32 address on a /24 subnet")
	}
	if n.Address4 != "" {
		t.Fatalf("a rejected address must not be persisted, got %q", n.Address4)
	}
	// Same failure mode the other direction (too broad rather than too narrow)
	// must also be rejected — any mismatch, not just /32 specifically.
	if err := c.NetworkSetAddress("corp", "192.168.203.140/16", ""); err == nil {
		t.Fatal("expected error for a /16 address on a /24 subnet")
	}
	// The matching prefix length succeeds.
	if err := c.NetworkSetAddress("corp", "192.168.203.140/24", ""); err != nil {
		t.Fatalf("matching prefix length should be accepted: %v", err)
	}
	if n.Address4 != "192.168.203.140/24" {
		t.Fatalf("address4 = %q, want %q", n.Address4, "192.168.203.140/24")
	}

	// Same class of bug, same fix, for v6: a /128 on a /64 subnet.
	if err := c.NetworkSetAddress("corp", "", "fd00:203::5/128"); err == nil {
		t.Fatal("expected error for a /128 address on a /64 subnet")
	}
	if n.Address6 != "" {
		t.Fatalf("a rejected address6 must not be persisted, got %q", n.Address6)
	}
	if err := c.NetworkSetAddress("corp", "", "fd00:203::5/64"); err != nil {
		t.Fatalf("matching v6 prefix length should be accepted: %v", err)
	}
}
