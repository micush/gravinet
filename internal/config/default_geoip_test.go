package config

import "testing"

// TestDefaultGeoIPLookupOn locks in that fresh configs get Geo-IP lookups on
// by default (see WebAdmin.GeoIPLookup's doc comment for the reasoning) —
// a refactor of Default() that started explicitly setting the field to a
// literal &true, instead of leaving it nil, would reintroduce the exact bug
// TestGeoIPLookupExplicitFalsePersists below guards against.
func TestDefaultGeoIPLookupOn(t *testing.T) {
	if Default().WebAdmin.GeoIPLookup != nil {
		t.Fatal("Default().WebAdmin.GeoIPLookup should be left nil (see its doc comment), not set to a literal value")
	}
	if !Default().WebAdmin.GeoIPEnabled() {
		t.Fatal("Default().WebAdmin.GeoIPEnabled() = false, want true")
	}
}

// TestGeoIPLookupExplicitFalsePersists is a regression test for a real bug
// hit during development: Load() seeds a fresh Config from Default() and
// unmarshals the file's JSON on top of it, so a field that (a) defaults to
// true and (b) uses a plain bool with omitempty can never actually persist
// an explicit false — Marshal drops a false value from the file entirely,
// and the next Load() silently resurrects the Default()-seeded true, no
// matter how many times it's explicitly set back to false in between. This
// round-trips an explicit false through Save→Load twice to make sure it
// actually sticks.
func TestGeoIPLookupExplicitFalsePersists(t *testing.T) {
	path := t.TempDir() + "/cfg.json"
	cfg := Default()
	if err := cfg.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	off := false
	c1, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	c1.WebAdmin.GeoIPLookup = &off
	if err := c1.SaveTo(path); err != nil {
		t.Fatal(err)
	}

	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c2.WebAdmin.GeoIPEnabled() {
		t.Fatal("explicit false did not survive a save+reload cycle")
	}

	// And it should still be false after a second, unrelated save — not just
	// immediately after the one that set it (this is what would have caught
	// dropping omitempty as an alternative "fix": a plain bool with no
	// omitempty round-trips fine on its own, but permanently bakes an
	// explicit true into the file on the very next unrelated save, which is
	// indistinguishable from every other config also getting bumped to true).
	c2.PrimaryPort = 12345
	if err := c2.SaveTo(path); err != nil {
		t.Fatal(err)
	}
	c3, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c3.WebAdmin.GeoIPEnabled() {
		t.Fatal("explicit false did not survive an unrelated second save+reload")
	}
}
