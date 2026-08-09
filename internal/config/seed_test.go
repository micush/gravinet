package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func seedCfg() *Config {
	return &Config{Networks: []Network{{Name: "corp", ID: "0000000000000001", Subnet4: "10.1.0.0/16"}}}
}

func TestSeedAddRemove(t *testing.T) {
	c := seedCfg()
	if err := c.SeedAdd("corp", "203.0.113.5:51820"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedAdd("corp", "seed.example.com"); err != nil { // bare host ok
		t.Fatal(err)
	}
	if err := c.SeedAdd("corp", "203.0.113.5:51820"); err != nil { // dup is a no-op
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds; !reflect.DeepEqual(got, SeedList{{Address: "203.0.113.5:51820"}, {Address: "seed.example.com"}}) {
		t.Fatalf("seeds = %v", got)
	}
	if err := c.SeedRemove("corp", "203.0.113.5:51820"); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds; !reflect.DeepEqual(got, SeedList{{Address: "seed.example.com"}}) {
		t.Fatalf("after remove seeds = %v", got)
	}
}

func TestSeedAddValidation(t *testing.T) {
	c := seedCfg()
	for _, bad := range []string{"", "host:99999", "host:0", "host:abc", "a b:1", "host:1,99999", "host:1,abc", "host:1,"} {
		if err := c.SeedAdd("corp", bad); err == nil {
			t.Fatalf("expected error for seed %q", bad)
		}
	}
	if len(c.Networks[0].Seeds) != 0 {
		t.Fatalf("invalid seeds must not be stored: %v", c.Networks[0].Seeds)
	}
}

// TestSeedAddMultiPort verifies a comma-separated port list is accepted and
// stored verbatim (resolveSeeds/resolveTCPSeeds, not SeedAdd, is what expands
// it into individual dial candidates — see cmd/gravinet/seeds_test.go).
func TestSeedAddMultiPort(t *testing.T) {
	c := seedCfg()
	if err := c.SeedAdd("corp", "203.0.113.5:65432,443,53"); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds; !reflect.DeepEqual(got, SeedList{{Address: "203.0.113.5:65432,443,53"}}) {
		t.Fatalf("seeds = %v", got)
	}
}

// TestSeedListUnmarshalLegacy verifies a config written before Notes existed
// (a bare JSON string array for "seeds") still loads, with every seed getting
// empty Notes — the exact backward-compatibility case SeedList.UnmarshalJSON
// exists for.
func TestSeedListUnmarshalLegacy(t *testing.T) {
	var sl SeedList
	if err := json.Unmarshal([]byte(`["203.0.113.5:51820","seed.example.com"]`), &sl); err != nil {
		t.Fatalf("legacy string-array seeds should still unmarshal: %v", err)
	}
	want := SeedList{{Address: "203.0.113.5:51820"}, {Address: "seed.example.com"}}
	if !reflect.DeepEqual(sl, want) {
		t.Fatalf("legacy unmarshal = %+v, want %+v", sl, want)
	}
}

// TestSeedListUnmarshalCurrent verifies the object form (the only one ever
// written back out, per Notes' comment) round-trips including Notes.
func TestSeedListUnmarshalCurrent(t *testing.T) {
	var sl SeedList
	raw := `[{"address":"203.0.113.5:51820","notes":"office uplink"},{"address":"seed.example.com"}]`
	if err := json.Unmarshal([]byte(raw), &sl); err != nil {
		t.Fatalf("object-form seeds should unmarshal: %v", err)
	}
	want := SeedList{{Address: "203.0.113.5:51820", Notes: "office uplink"}, {Address: "seed.example.com"}}
	if !reflect.DeepEqual(sl, want) {
		t.Fatalf("current unmarshal = %+v, want %+v", sl, want)
	}
	// And it round-trips back out as the object form (no custom Marshal
	// needed — the default []Seed encoding already is the object form).
	out, err := json.Marshal(sl)
	if err != nil {
		t.Fatal(err)
	}
	var back SeedList
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, want) {
		t.Fatalf("round-trip = %+v, want %+v", back, want)
	}
}

func TestSeedSetNotes(t *testing.T) {
	c := seedCfg()
	if err := c.SeedAdd("corp", "203.0.113.5:51820"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSetNotes("corp", "203.0.113.5:51820", "office uplink"); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds[0].Notes; got != "office uplink" {
		t.Fatalf("notes = %q, want %q", got, "office uplink")
	}
	// Clearing (empty notes) is allowed and distinct from an error.
	if err := c.SeedSetNotes("corp", "203.0.113.5:51820", ""); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds[0].Notes; got != "" {
		t.Fatalf("notes after clear = %q, want empty", got)
	}
	// Setting notes on an address that isn't a configured seed is an error.
	if err := c.SeedSetNotes("corp", "203.0.113.9:51820", "x"); err == nil {
		t.Fatal("expected error for unknown seed address")
	}
}

// TestSeedUpdateAddr covers the rename op the web UI uses for both an
// address/port edit and a udp/tcp transport flip: it must preserve Notes and
// the seed's position in the list (the whole point of it existing instead of
// the add-then-remove sequence it replaced — see SeedUpdateAddr's doc
// comment), plus reject an unknown old address, a collision with an existing
// different seed, and an invalid new address.
func TestSeedUpdateAddr(t *testing.T) {
	c := seedCfg()
	if err := c.SeedAdd("corp", "203.0.113.5:51820"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedAdd("corp", "seed.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSetNotes("corp", "203.0.113.5:51820", "office uplink"); err != nil {
		t.Fatal(err)
	}

	// Renaming the first seed must not disturb the second seed's position or
	// lose the first seed's notes.
	if err := c.SeedUpdateAddr("corp", "203.0.113.5:51820", "203.0.113.5:65432"); err != nil {
		t.Fatal(err)
	}
	want := SeedList{{Address: "203.0.113.5:65432", Notes: "office uplink"}, {Address: "seed.example.com"}}
	if got := c.Networks[0].Seeds; !reflect.DeepEqual(got, want) {
		t.Fatalf("seeds = %+v, want %+v", got, want)
	}

	// A transport-flip rename (adding a tcp:// scheme) behaves the same way.
	if err := c.SeedUpdateAddr("corp", "203.0.113.5:65432", "tcp://203.0.113.5:65432"); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds[0]; got.Address != "tcp://203.0.113.5:65432" || got.Notes != "office uplink" {
		t.Fatalf("after transport flip = %+v", got)
	}

	// Renaming an address that isn't configured is an error.
	if err := c.SeedUpdateAddr("corp", "203.0.113.9:1", "203.0.113.9:2"); err == nil {
		t.Fatal("expected error for unknown old address")
	}
	// Renaming onto an address that's already a different seed is an error
	// (would otherwise silently collide two entries into one).
	if err := c.SeedUpdateAddr("corp", "seed.example.com", "tcp://203.0.113.5:65432"); err == nil {
		t.Fatal("expected error renaming onto an existing different seed")
	}
	// An invalid new address is rejected the same way SeedAdd rejects one.
	if err := c.SeedUpdateAddr("corp", "seed.example.com", "host:99999"); err == nil {
		t.Fatal("expected error for invalid new address")
	}
}

// A disabled seed keeps everything about itself — address, notes, and its
// position in the list — and differs only in being absent from EnabledAddrs.
// The two accessors are the whole point of the field, so they're asserted
// against each other rather than in isolation: Addrs is what the operator
// wrote down, EnabledAddrs is what gets dialed, and a change that quietly
// collapsed one into the other would pass any test that only checked one.
func TestSeedSetEnabled(t *testing.T) {
	c := seedCfg()
	for _, a := range []string{"a.example.com", "b.example.com", "c.example.com"} {
		if err := c.SeedAdd("corp", a); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SeedSetNotes("corp", "b.example.com", "branch office"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSetEnabled("corp", "b.example.com", false); err != nil {
		t.Fatal(err)
	}

	seeds := c.Networks[0].Seeds
	// Position preserved: still the middle entry, not moved or dropped.
	if want := (SeedList{
		{Address: "a.example.com"},
		{Address: "b.example.com", Notes: "branch office", Disabled: true},
		{Address: "c.example.com"},
	}); !reflect.DeepEqual(seeds, want) {
		t.Fatalf("seeds = %#v, want %#v", seeds, want)
	}
	if got, want := seeds.Addrs(), []string{"a.example.com", "b.example.com", "c.example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Addrs must still list every configured seed: %v", got)
	}
	if got, want := seeds.EnabledAddrs(), []string{"a.example.com", "c.example.com"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("EnabledAddrs = %v, want %v", got, want)
	}

	// Re-enabling restores it without disturbing the note.
	if err := c.SeedSetEnabled("corp", "b.example.com", true); err != nil {
		t.Fatal(err)
	}
	if s := c.Networks[0].Seeds[1]; s.Disabled || s.Notes != "branch office" {
		t.Fatalf("re-enable should clear Disabled and leave notes alone: %#v", s)
	}
	if n := len(c.Networks[0].Seeds.EnabledAddrs()); n != 3 {
		t.Fatalf("all three seeds should be enabled again, got %d", n)
	}
}

// Every seed disabled is a legitimate state, not an error — a site parked
// while it's down, with the network still reachable through PeerCache.
// EnabledAddrs must say so with an empty list rather than falling back to
// the full one, which would make disabling the last seed a silent no-op.
func TestSeedEnabledAddrsAllDisabled(t *testing.T) {
	c := seedCfg()
	if err := c.SeedAdd("corp", "only.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSetEnabled("corp", "only.example.com", false); err != nil {
		t.Fatal(err)
	}
	if got := c.Networks[0].Seeds.EnabledAddrs(); len(got) != 0 {
		t.Fatalf("EnabledAddrs with everything disabled = %v, want empty", got)
	}
	if got := c.Networks[0].Seeds.Addrs(); len(got) != 1 {
		t.Fatalf("the seed itself must still be configured: %v", got)
	}
}

// Re-adding a disabled address puts it back in service rather than doing
// nothing, matching HostRejectAdd/DNSRejectAdd. The entry is updated in
// place, so its notes and position survive.
func TestSeedAddReEnables(t *testing.T) {
	c := seedCfg()
	if err := c.SeedAdd("corp", "a.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedAdd("corp", "b.example.com"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSetNotes("corp", "a.example.com", "keep me"); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedSetEnabled("corp", "a.example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := c.SeedAdd("corp", "a.example.com"); err != nil {
		t.Fatal(err)
	}
	if want := (SeedList{
		{Address: "a.example.com", Notes: "keep me"},
		{Address: "b.example.com"},
	}); !reflect.DeepEqual(c.Networks[0].Seeds, want) {
		t.Fatalf("re-add should re-enable in place, keeping notes and position: %#v", c.Networks[0].Seeds)
	}
}

func TestSeedSetEnabledUnknown(t *testing.T) {
	c := seedCfg()
	if err := c.SeedSetEnabled("corp", "nope.example.com", false); err == nil {
		t.Fatal("expected an error for a seed that isn't configured")
	}
}

// The Disabled field is omitempty, so an enabled seed serializes exactly as
// it did before the field existed, and a config written by an older version
// loads with every seed enabled. Both directions matter on a rolling upgrade.
func TestSeedDisabledJSONCompat(t *testing.T) {
	var sl SeedList
	if err := json.Unmarshal([]byte(`[{"address":"a.example.com"},{"address":"b.example.com","disabled":true}]`), &sl); err != nil {
		t.Fatal(err)
	}
	if sl[0].Disabled {
		t.Fatal("a seed with no disabled key must load enabled")
	}
	if !sl[1].Disabled {
		t.Fatal("disabled:true must load disabled")
	}
	// Legacy bare-string form: every seed enabled.
	var legacy SeedList
	if err := json.Unmarshal([]byte(`["a.example.com"]`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy[0].Disabled {
		t.Fatal("a legacy string seed must load enabled")
	}
	// An enabled seed must not emit the key at all.
	b, err := json.Marshal(SeedList{{Address: "a.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `[{"address":"a.example.com"}]` {
		t.Fatalf("enabled seed should serialize unchanged, got %s", b)
	}
}
