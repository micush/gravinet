package webadmin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gravinet/internal/logx"
)

// Same sibling-file convention as latencyHistoryPath/selfSignedPaths: next to
// the config file, and simply unavailable — not an error — with no config file
// to anchor it to.
func TestMetricsHistoryPath(t *testing.T) {
	if got := metricsHistoryPath(""); got != "" {
		t.Errorf(`metricsHistoryPath("") = %q, want ""`, got)
	}
	want := filepath.Join("/etc/gravinet", "metrics-history.json")
	if got := metricsHistoryPath("/etc/gravinet/config.json"); got != want {
		t.Errorf("metricsHistoryPath(.../config.json) = %q, want %q", got, want)
	}
}

// The core ask: history written by save() comes back through the load() path a
// freshly constructed collector takes on every real startup.
func TestMetricsCollectorSaveLoadRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-history.json")
	mc := newMetricsCollector(&stubBackend{}, nil, path) // nothing on disk yet
	now := time.Now().Unix()
	mc.cpu = []metricPoint{{T: now - 30, V: 11.5}, {T: now, V: 12.5}}
	mc.mem = []metricPoint{{T: now, V: 44.0}}
	mc.disk = []metricPoint{{T: now, V: 71.0}}
	mc.ifaces["mesh0"] = &ifaceMetrics{
		Network: "lan", Iface: "mesh0",
		Rx: []metricPoint{{T: now, V: 1000}},
		Tx: []metricPoint{{T: now, V: 2000}},
	}
	if err := mc.save(); err != nil {
		t.Fatal(err)
	}

	got := newMetricsCollector(&stubBackend{}, nil, path)
	if len(got.cpu) != 2 || got.cpu[1].V != 12.5 {
		t.Fatalf("cpu = %+v, want two points ending 12.5", got.cpu)
	}
	if len(got.mem) != 1 || got.mem[0].V != 44.0 {
		t.Fatalf("mem = %+v", got.mem)
	}
	if len(got.disk) != 1 || got.disk[0].V != 71.0 {
		t.Fatalf("disk = %+v", got.disk)
	}
	ifm := got.ifaces["mesh0"]
	if ifm == nil || len(ifm.Rx) != 1 || ifm.Rx[0].V != 1000 || ifm.Tx[0].V != 2000 {
		t.Fatalf("mesh0 = %+v", ifm)
	}
	if ifm.Network != "lan" || ifm.Iface != "mesh0" {
		t.Fatalf("interface identity lost: %+v", ifm)
	}
}

// The sampler deltas must NOT survive a restart. lastRx/lastTx/lastT are
// counters read moments ago by the previous process; restoring them would make
// the first tick compute a rate spanning the whole downtime — one enormous
// fabricated spike at the exact moment the graph resumes.
func TestMetricsLoadDoesNotRestoreSamplerDeltas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-history.json")
	mc := newMetricsCollector(&stubBackend{}, nil, path)
	now := time.Now().Unix()
	mc.ifaces["mesh0"] = &ifaceMetrics{
		Network: "lan", Iface: "mesh0",
		Rx:     []metricPoint{{T: now, V: 1}},
		Tx:     []metricPoint{{T: now, V: 1}},
		lastRx: 999999, lastTx: 888888, lastT: now - 5, have: true,
	}
	if err := mc.save(); err != nil {
		t.Fatal(err)
	}
	got := newMetricsCollector(&stubBackend{}, nil, path).ifaces["mesh0"]
	if got == nil {
		t.Fatal("interface history lost entirely")
	}
	if got.have || got.lastRx != 0 || got.lastTx != 0 || got.lastT != 0 {
		t.Fatalf("sampler deltas survived the restart (%+v); the next tick would emit a spike covering the downtime", got)
	}
}

// Points older than metricRetention are dropped at load, the same trim the
// append path applies — a daemon down longer than the window must not
// resurrect data normal operation would already have aged out. An interface
// left with nothing is dropped rather than kept as an empty entry.
func TestMetricsLoadTrimsExpiredPoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-history.json")
	now := time.Now().Unix()
	old := now - int64(metricRetention/time.Second) - 3600
	snap := metricsSnapshot{
		CPU: []metricPoint{{T: old, V: 1}, {T: now, V: 2}},
		Ifaces: map[string]*ifaceMetrics{
			"stale": {Network: "lan", Iface: "stale", Rx: []metricPoint{{T: old, V: 5}}},
			"live":  {Network: "lan", Iface: "live", Rx: []metricPoint{{T: now, V: 5}}},
		},
	}
	body, _ := json.Marshal(snap)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	mc := newMetricsCollector(&stubBackend{}, nil, path)
	if len(mc.cpu) != 1 || mc.cpu[0].V != 2 {
		t.Fatalf("cpu = %+v, want only the in-window point", mc.cpu)
	}
	if _, ok := mc.ifaces["stale"]; ok {
		t.Error("an interface with no in-window points was kept")
	}
	if _, ok := mc.ifaces["live"]; !ok {
		t.Error("an interface with in-window points was dropped")
	}
}

// A missing or corrupt file is never fatal: this is a convenience cache of a
// signal the host regenerates on its own, so any failure starts empty exactly
// as the collector did before persistence existed.
func TestMetricsLoadToleratesMissingAndCorrupt(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.json")
	if mc := newMetricsCollector(&stubBackend{}, logx.Default(), missing); len(mc.cpu) != 0 || len(mc.ifaces) != 0 {
		t.Error("a missing file did not start empty")
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if mc := newMetricsCollector(&stubBackend{}, logx.Default(), bad); len(mc.cpu) != 0 || len(mc.ifaces) != 0 {
		t.Error("a corrupt file did not start empty")
	}
}

// Persistence off (no config path to anchor to) must behave exactly as before
// it existed: no file, no error, and close() must not try to write one.
func TestMetricsPersistenceOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	mc := newMetricsCollector(&stubBackend{}, nil, "")
	mc.cpu = []metricPoint{{T: time.Now().Unix(), V: 5}}
	mc.close()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("files written with persistence disabled: %v", ents)
	}
}

// close() is the clean-shutdown half of checkpointing, so it must actually
// write. This also covers the Close() wiring that was missing entirely before:
// the collector's goroutine used to outlive shutdown, which was untidy while it
// held only in-memory state and is a lost checkpoint now.
func TestMetricsCloseSavesSynchronously(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics-history.json")
	mc := newMetricsCollector(&stubBackend{}, nil, path)
	mc.cpu = []metricPoint{{T: time.Now().Unix(), V: 33.0}}
	mc.close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("close() did not write the checkpoint: %v", err)
	}
	if got := newMetricsCollector(&stubBackend{}, nil, path); len(got.cpu) != 1 || got.cpu[0].V != 33.0 {
		t.Fatalf("cpu after reload = %+v", got.cpu)
	}
}
