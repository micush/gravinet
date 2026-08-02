package webadmin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLatencyHistoryPath pins the sibling-file convention this shares with
// selfSignedPaths/sessionKeyPath (webadmin.go): next to the config file,
// and simply unavailable — not an error — when there's no config file to
// anchor it to.
func TestLatencyHistoryPath(t *testing.T) {
	if got := latencyHistoryPath(""); got != "" {
		t.Errorf(`latencyHistoryPath("") = %q, want ""`, got)
	}
	want := filepath.Join("/etc/gravinet", "latency-history.json")
	if got := latencyHistoryPath("/etc/gravinet/config.json"); got != want {
		t.Errorf("latencyHistoryPath(.../config.json) = %q, want %q", got, want)
	}
}

// TestLatencyCollectorSaveLoadRoundTrips is the core ask this feature
// exists for: history written by save() and read back by a freshly
// constructed collector (the same load() path newLatencyCollector takes on
// every real startup) comes back with the same hostname, overlay address,
// and points a live collector would have produced.
func TestLatencyCollectorSaveLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latency-history.json")
	be := &stubBackend{}
	lc := newLatencyCollector(be, nil, path) // nothing on disk yet -> starts empty
	now := time.Now().Unix()
	lc.nets["lan"] = map[string]*latencyPeerHistory{
		"peerX": {
			Hostname: "hostx", Overlay: "10.0.0.9",
			Hist: []metricPoint{{T: now - 30, V: 12.5}, {T: now, V: 13.1}},
		},
	}
	if err := lc.save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded := newLatencyCollector(be, nil, path)
	ph := reloaded.nets["lan"]["peerX"]
	if ph == nil {
		t.Fatal("reloaded collector has no history for lan/peerX")
	}
	if ph.Hostname != "hostx" || ph.Overlay != "10.0.0.9" {
		t.Errorf("reloaded metadata = %+v, want hostname=hostx overlay=10.0.0.9", ph)
	}
	if len(ph.Hist) != 2 || ph.Hist[0].V != 12.5 || ph.Hist[1].V != 13.1 {
		t.Errorf("reloaded history = %+v, want the two original points intact", ph.Hist)
	}
}

// TestLatencyCollectorLoadTrimsExpiredAndDropsEmpty proves a checkpoint
// written long before a restart doesn't resurrect data past
// latencyRetention — the same trim sample() applies to every live write,
// applied once at load time too — and that a peer or network left with
// zero points after that trim is dropped outright rather than kept as a
// dangling empty entry that would otherwise accumulate forever across
// repeated restarts of a long-lived mesh.
func TestLatencyCollectorLoadTrimsExpiredAndDropsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latency-history.json")
	now := time.Now().Unix()
	onDisk := map[string]map[string]*latencyPeerHistory{
		"lan": {
			// Entirely expired -> the whole peer entry should vanish.
			"gone": {Hostname: "gone-host", Overlay: "10.0.0.1",
				Hist: []metricPoint{{T: now - int64(2*latencyRetention/time.Second), V: 5}}},
			// One expired point, one still fresh -> only the fresh one survives.
			"mixed": {Hostname: "mixed-host", Overlay: "10.0.0.2",
				Hist: []metricPoint{
					{T: now - int64(2*latencyRetention/time.Second), V: 5},
					{T: now - 10, V: 6},
				}},
		},
		// Every peer in this network expires -> the network entry itself
		// should vanish too, not linger as an empty map.
		"wan": {
			"alsogone": {Hostname: "x", Overlay: "10.1.0.1",
				Hist: []metricPoint{{T: now - int64(2*latencyRetention/time.Second), V: 1}}},
		},
	}
	body, err := json.Marshal(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}

	lc := newLatencyCollector(&stubBackend{}, nil, path)

	if _, ok := lc.nets["lan"]["gone"]; ok {
		t.Error("a peer entirely past retention should have been dropped on load, not kept empty")
	}
	if _, ok := lc.nets["wan"]; ok {
		t.Error("a network with every peer past retention should have been dropped on load")
	}
	mixed := lc.nets["lan"]["mixed"]
	if mixed == nil {
		t.Fatal("the partially-expired peer should still be present")
	}
	if len(mixed.Hist) != 1 || mixed.Hist[0].V != 6 {
		t.Errorf("mixed peer's history after load = %+v, want only the still-fresh point", mixed.Hist)
	}
}

// TestLatencyCollectorLoadMissingFileIsSilent proves a first-ever run (or
// an upgrade from a version before this feature existed) starts empty
// without erroring — the file simply not existing yet is the expected,
// common case, not a failure.
func TestLatencyCollectorLoadMissingFileIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	lc := newLatencyCollector(&stubBackend{}, nil, path)
	if len(lc.nets) != 0 {
		t.Errorf("collector with no checkpoint on disk should start empty, got %+v", lc.nets)
	}
}

// TestLatencyCollectorLoadCorruptFileIsSilent proves the same graceful
// fallback for a present-but-unparseable file (truncated write, a version
// skew, manual tampering) — never blocks startup over a checkpoint that
// isn't a source of truth for anything.
func TestLatencyCollectorLoadCorruptFileIsSilent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latency-history.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	lc := newLatencyCollector(&stubBackend{}, nil, path)
	if len(lc.nets) != 0 {
		t.Errorf("collector with a corrupt checkpoint should start empty, got %+v", lc.nets)
	}
}

// TestLatencyCollectorCloseSavesWhenPersistent proves close() performs the
// clean-shutdown save this feature exists for, synchronously — the file
// must already be on disk with the right content by the time close()
// returns, not merely scheduled.
func TestLatencyCollectorCloseSavesWhenPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latency-history.json")
	lc := newLatencyCollector(&stubBackend{}, nil, path)
	now := time.Now().Unix()
	lc.nets["lan"] = map[string]*latencyPeerHistory{
		"peerX": {Hostname: "hostx", Overlay: "10.0.0.9", Hist: []metricPoint{{T: now, V: 7.5}}},
	}

	lc.close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("close() should have written %s synchronously: %v", path, err)
	}
	var onDisk map[string]map[string]*latencyPeerHistory
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	ph := onDisk["lan"]["peerX"]
	if ph == nil || len(ph.Hist) != 1 || ph.Hist[0].V != 7.5 {
		t.Errorf("saved content = %+v, want the one seeded point", onDisk)
	}
}

// TestLatencyCollectorCloseNoopWhenPathEmpty proves persistence being
// disabled (path == "", e.g. an embedding context with no config file) is
// genuinely inert: close() must not attempt to write anything or panic.
func TestLatencyCollectorCloseNoopWhenPathEmpty(t *testing.T) {
	lc := newLatencyCollector(&stubBackend{}, nil, "")
	lc.nets["lan"] = map[string]*latencyPeerHistory{"peerX": {Hostname: "hostx"}}
	lc.close() // must not panic (writeAtomicFile("", ...) would be the failure mode this guards)
}
