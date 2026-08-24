package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// historyFixture makes a history directory with one real snapshot in it, and
// a file outside it that a traversal would be trying to reach.
func historyFixture(t *testing.T) (root, d, outside string) {
	t.Helper()
	root = t.TempDir()
	d = filepath.Join(root, "cfg", "history")
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := writeOne(d, Default(), "alice", "fixture"); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(root, "secret")
	body := []byte(`{"id":"x","ts":1,"stamp":"s","user":"root","summary":"leaked"}`)
	for _, ext := range []string{".json", ".meta"} {
		if err := os.WriteFile(outside+ext, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, d, outside
}

// TestSnapshotFileForRejectsTraversal is the regression test for the reported
// finding. The id reaches this package from a query parameter, a request body
// and a CLI argument, and it must not be able to name a file outside the
// history directory.
func TestSnapshotFileForRejectsTraversal(t *testing.T) {
	for _, bad := range []string{
		"",
		"..",
		"../../etc/passwd",
		`..\..\windows`,
		"/etc/passwd",
		"1/../../../etc/passwd",
		".../....//",
		"current", // the live-config sentinel is handled by Get, never as a path
		"1e3",
		"0x10",
		" 1",
		"1 ",
		"1\x00",
	} {
		if got, err := snapshotFileFor("/var/lib/gravinet/history", bad, ".json"); err == nil {
			t.Errorf("snapshotFileFor(%q) = %q, nil; want a refusal", bad, got)
		}
	}
}

// TestSnapshotFileForRejectsNonCanonicalIDs pins the round-trip check. These
// were accepted by the old validID and then failed to name a file, so what an
// operator sees is unchanged; the point is that only one spelling of an id
// ever reaches the filesystem.
func TestSnapshotFileForRejectsNonCanonicalIDs(t *testing.T) {
	for _, bad := range []string{
		"007",                    // leading zeros
		"+1",                     // signed
		"-1",                     // negative
		"00000000000000000001",   // padded
		"99999999999999999999",   // does not fit an int64
		"1719245412123999999999", // nor this
	} {
		if got, err := snapshotFileFor("/h", bad, ".json"); err == nil {
			t.Errorf("snapshotFileFor(%q) = %q, nil; want only canonical ids", bad, got)
		}
	}
}

// TestSnapshotFileForAcceptsRealIDs guards the other direction: ids are unix
// milliseconds, and every id this package generates must round-trip.
func TestSnapshotFileForAcceptsRealIDs(t *testing.T) {
	for _, ok := range []string{"0", "1", "1719245412123", "9223372036854775807"} {
		got, err := snapshotFileFor("/h", ok, ".json")
		if err != nil {
			t.Errorf("snapshotFileFor(%q) returned %v; want a path", ok, err)
			continue
		}
		if want := "/h/" + ok + ".json"; got != want {
			t.Errorf("snapshotFileFor(%q) = %q; want %q", ok, got, want)
		}
	}
}

// TestSnapshotPathsStayInHistoryDir states the property directly rather than
// enumerating attacks: whatever comes out is a file directly inside the
// history directory, for every input that is accepted at all.
func TestSnapshotPathsStayInHistoryDir(t *testing.T) {
	const d = "/var/lib/gravinet/history"
	for _, id := range []string{"0", "1", "1719245412123", "..", "../x", "007", ""} {
		got, err := snapshotFileFor(d, id, ".json")
		if err != nil {
			continue // refused, which is the other half of the property
		}
		if filepath.Dir(got) != d {
			t.Errorf("snapshotFileFor(%q) = %q, which is not directly inside %s", id, got, d)
		}
		if got != filepath.Clean(got) {
			t.Errorf("snapshotFileFor(%q) = %q, which is not already clean", id, got)
		}
	}
}

// TestReadEnvelopeRefusesTraversal covers the flagged line end to end. It was
// already safe — validID admitted only digits — and must stay that way now
// that the check has moved into the path builder.
func TestReadEnvelopeRefusesTraversal(t *testing.T) {
	root, d, _ := historyFixture(t)
	rel, err := filepath.Rel(d, filepath.Join(root, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := readEnvelope(d, rel); ok {
		t.Errorf("readEnvelope read %q, which is outside the history directory", rel)
	}
}

// TestReadMetaRefusesTraversal is the test for the gap the alert did not
// flag. readMeta built its sidecar path by concatenation with no check at
// all, twenty lines above the line CodeQL pointed at — so it read a .meta
// from anywhere on the filesystem and handed back what it parsed.
//
// Only List called it, with ids it generated itself, so nothing reached it in
// practice. That is exactly the kind of safety this release is trying to stop
// depending on.
func TestReadMetaRefusesTraversal(t *testing.T) {
	root, d, _ := historyFixture(t)
	rel, err := filepath.Rel(d, filepath.Join(root, "secret"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := readMeta(d, rel)
	if ok {
		t.Errorf("readMeta read %q from outside the history directory and returned %+v", rel, m)
	}
	if m.Summary == "leaked" {
		t.Errorf("readMeta disclosed the contents of a file outside the history directory")
	}
}

// TestReadMetaWritesNoSidecarOutsideHistoryDir covers readMeta's other half.
// On a sidecar miss it writes one, so an unchecked id there is a write
// primitive, not just a read: the envelope check happened to stand in the way
// before, and now the path check does so directly.
func TestReadMetaWritesNoSidecarOutsideHistoryDir(t *testing.T) {
	root, d, _ := historyFixture(t)
	victim := filepath.Join(root, "victim")
	rel, err := filepath.Rel(d, victim)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture puts files in root itself, so compare against what was
	// there rather than assuming the directory is empty.
	before := map[string]bool{}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		before[e.Name()] = true
	}
	readMeta(d, rel)
	if _, err := os.Stat(victim + ".meta"); err == nil {
		t.Errorf("readMeta created %s, outside the history directory", victim+".meta")
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range after {
		if !before[e.Name()] {
			t.Errorf("readMeta created %s in the parent of the history directory", e.Name())
		}
	}
}

// TestHistoryRoundTripStillWorks is the guard against fixing the path check
// by breaking the feature. A snapshot written by this package must still be
// listable, readable and metadata-able by the id it was given.
func TestHistoryRoundTripStillWorks(t *testing.T) {
	root, d, _ := historyFixture(t)
	configPath := filepath.Join(root, "cfg", "config.json")

	items, err := List(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("List returned %d snapshots, want 1", len(items))
	}
	id := items[0].ID
	if items[0].Summary != "fixture" || items[0].User != "alice" {
		t.Errorf("List returned %+v; want the fixture's user and summary", items[0])
	}

	env, ok := readEnvelope(d, id)
	if !ok {
		t.Fatalf("readEnvelope could not read back snapshot %q", id)
	}
	if env.ID != id || env.Config == nil {
		t.Errorf("readEnvelope returned %+v; want the snapshot written under %q", env, id)
	}

	// The sidecar fallback path: delete the .meta and confirm readMeta
	// rebuilds it, in the history directory, from the envelope.
	metaPath := filepath.Join(d, id+".meta")
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	m, ok := readMeta(d, id)
	if !ok || m.Summary != "fixture" {
		t.Fatalf("readMeta fallback returned %+v ok=%v; want the fixture's metadata", m, ok)
	}
	data, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("readMeta did not rewrite the sidecar at %s: %v", metaPath, err)
	}
	var back SnapshotMeta
	if json.Unmarshal(data, &back) != nil || back.ID != id {
		t.Errorf("rewritten sidecar is %s; want metadata for %q", data, id)
	}
}

// TestGetRejectsTraversalIDs is the end-to-end version, at the boundary the
// three request handlers and the CLI actually call.
func TestGetRejectsTraversalIDs(t *testing.T) {
	root, _, _ := historyFixture(t)
	configPath := filepath.Join(root, "cfg", "config.json")
	for _, bad := range []string{"../secret", "../../secret", "/etc/passwd", ".."} {
		if _, cfg, err := Get(configPath, bad, Default()); err == nil {
			t.Errorf("Get(%q) returned a config (%v); want \"no such snapshot\"", bad, cfg != nil)
		}
	}
}
