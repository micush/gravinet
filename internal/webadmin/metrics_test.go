package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

func TestMetricsCollectorSample(t *testing.T) {
	be := &stubBackend{}
	mc := newMetricsCollector(be, logx.Default())
	// Two samples produce at least one CPU/iface rate point (rates need a delta).
	mc.sample()
	mc.sample()
	snap := mc.snapshot(60)
	if avail, _ := snap["available"].(bool); !avail {
		t.Fatal("expected metrics available on this (linux) host")
	}
	if _, ok := snap["cpu"].([]metricPoint); !ok {
		t.Fatalf("cpu series missing: %T", snap["cpu"])
	}
	if mp, ok := snap["mem"].([]metricPoint); !ok || len(mp) == 0 {
		t.Fatalf("expected memory points, got %v", snap["mem"])
	}
	if dp, ok := snap["disk"].([]metricPoint); !ok || len(dp) == 0 {
		t.Fatalf("expected disk points, got %v", snap["disk"])
	}
	if lbl, ok := snap["disk_path"].(string); !ok || lbl == "" {
		t.Fatalf("expected non-empty disk_path, got %v", snap["disk_path"])
	}
	ifs, ok := snap["ifaces"].([]ifaceMetrics)
	if !ok || len(ifs) != 1 {
		t.Fatalf("expected one interface series (from stub), got %v", snap["ifaces"])
	}
	if ifs[0].Iface != "mesh0" || ifs[0].Network != "lan" {
		t.Fatalf("interface mapping wrong: %+v", ifs[0])
	}
	// This test runs on Linux, where readUptime() is genuinely wired up (via
	// /proc/uptime), so uptime_seconds should be present — not just omitted
	// as "unavailable" — and hold something sane for a host that's actually
	// running (i.e. not 0, and not implausibly large).
	up, ok := snap["uptime_seconds"].(uint64)
	if !ok {
		t.Fatalf("uptime_seconds missing or wrong type: %v (%T)", snap["uptime_seconds"], snap["uptime_seconds"])
	}
	if up == 0 || up > 10*365*86400 {
		t.Fatalf("uptime_seconds = %d, want a plausible non-zero uptime", up)
	}
	// server_now is what the frontend draws the chart's "now" edge from
	// (ui.go's renderMetricGraphs) instead of the browser's own clock — see
	// v454's changelog entry. Confirm it's actually present and current,
	// not just silently missing (which would fall back to Date.now() and
	// mask exactly the client/server clock mismatch this exists to avoid).
	sn, ok := snap["server_now"].(int64)
	if !ok {
		t.Fatalf("server_now missing or wrong type: %v (%T)", snap["server_now"], snap["server_now"])
	}
	if d := time.Now().Unix() - sn; d < -2 || d > 2 {
		t.Fatalf("server_now = %d is %ds from time.Now() — want it essentially current", sn, d)
	}
}

func TestMetricsWindowClamp(t *testing.T) {
	mc := newMetricsCollector(&stubBackend{}, logx.Default())
	mc.sample()
	// snapshot() should accept any window; the handler clamps 1..60.
	if got := mc.snapshot(1); got["sample_interval"].(int) != 2 {
		t.Fatalf("sample_interval = %v, want 2", got["sample_interval"])
	}
}

func TestHandleMetrics(t *testing.T) {
	cfgPath := t.TempDir() + "/cfg.json"
	cfg := &config.Config{PrimaryPort: 65432, EnableIPv4: true,
		WebAdmin: config.WebAdmin{Listen: "127.0.0.1:8443"},
		Networks: []config.Network{{ID: "1234", Name: "lan", Enabled: true, Subnet4: "10.0.0.0/24"}}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	_ = cfg.SaveTo(cfgPath)
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan: config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900}}
	srv := New(wcfg, &stubBackend{}, logx.Default())
	srv.SetConfigPath(cfgPath)
	// The collector is normally started by Start(); set it up directly for the test.
	srv.metrics = newMetricsCollector(srv.be, srv.log)
	srv.metrics.sample()
	srv.metrics.sample()
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()
	c := sessionFor(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/api/metrics?minutes=5", strings.NewReader(""))
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if avail, _ := out["available"].(bool); !avail {
		t.Fatalf("expected available metrics: %v", out)
	}
	if _, ok := out["ifaces"].([]any); !ok {
		t.Fatalf("expected ifaces array: %v", out["ifaces"])
	}
	if lbl, ok := out["disk_path"].(string); !ok || lbl == "" {
		t.Fatalf("expected non-empty disk_path: %v", out["disk_path"])
	}
	// JSON numbers decode as float64, unlike the uint64 in the in-process
	// snapshot() test above — confirming it survives the actual HTTP/JSON
	// round trip, not just the direct map lookup.
	if up, ok := out["uptime_seconds"].(float64); !ok || up <= 0 {
		t.Fatalf("expected a positive uptime_seconds over the wire, got %v", out["uptime_seconds"])
	}
	if sn, ok := out["server_now"].(float64); !ok || sn <= 0 {
		t.Fatalf("expected a positive server_now over the wire, got %v", out["server_now"])
	}
}

// On macOS, sample() populates every series (CPU, memory, disk, uptime, and
// every interface) from readers that mostly shell out to separate short-lived
// processes (top, vm_stat, sysctl x2, netstat — see metrics_darwin.go), each
// costing real wall time on top of the fork/exec itself; top in particular
// has its own ~1s internal sampling delay by design. Run one at a time, five
// of those per sample() call add up to comfortably more than
// metricSampleInterval (2s) — and because every series is only ever updated
// from this one function, a slow run doesn't delay just one graph, it pushes
// every graph's newest point behind actual wall-clock time by the same
// margin. At the 1-minute zoom (where the frontend draws its right edge from
// the browser's own clock — see ui.go's renderMetricGraphs — not from
// whatever the newest point happens to be) that showed up as every line, not
// just one, visibly stopping short of "now".
//
// This proves the fix: sample()'s readers run concurrently, so wall time is
// bounded by the slowest single one rather than their sum. It substitutes
// slow fakes for two independent readers (memory and network) and confirms
// sample() returns in comfortably less than both delays added together —
// reverting to the old sequential calls makes this fail (verified by hand).
func TestSampleRunsReadersConcurrently(t *testing.T) {
	const delay = 200 * time.Millisecond
	origMem, origNet := readMemUsedPctFn, readNetDevFn
	defer func() { readMemUsedPctFn, readNetDevFn = origMem, origNet }()
	readMemUsedPctFn = func() (float64, bool) {
		time.Sleep(delay)
		return 42, true
	}
	readNetDevFn = func() map[string]devCounters {
		time.Sleep(delay)
		return map[string]devCounters{}
	}

	mc := newMetricsCollector(&stubBackend{}, logx.Default())
	start := time.Now()
	mc.sample()
	if elapsed := time.Since(start); elapsed >= 2*delay {
		t.Fatalf("sample() took %v with two independent %v readers — they must run concurrently, "+
			"not sequentially, or a slow platform (macOS) falls the same amount behind on every "+
			"graph at once, not just one", elapsed, delay)
	}
}

// A reader that fails after previously succeeding (the shape a flaky macOS
// subprocess spawn takes — see metrics_darwin.go) used to just skip that
// tick entirely: no new point, so the graph's newest point fell further and
// further behind actual wall-clock time with every failed tick, visible at
// the 1-minute zoom as the line stopping short of "now". sample() now
// carries the last known value forward instead, so the line keeps reaching
// "now" through a transient failure — flat while it lasts, but never stale.
func TestSampleCarriesLastValueForwardOnFailure(t *testing.T) {
	origMem := readMemUsedPctFn
	defer func() { readMemUsedPctFn = origMem }()

	memUp := true
	readMemUsedPctFn = func() (float64, bool) {
		if memUp {
			return 55, true
		}
		return 0, false
	}

	mc := newMetricsCollector(&stubBackend{}, logx.Default())
	mc.sample()
	if len(mc.mem) != 1 || mc.mem[0].V != 55 {
		t.Fatalf("expected one 55%% point after the first successful sample, got %v", mc.mem)
	}
	firstT := mc.mem[0].T

	// The reader starts failing. Without carry-forward, mem would stay at
	// length 1 forever; with it, a new point still lands every tick, just
	// holding the last known value.
	memUp = false
	time.Sleep(1100 * time.Millisecond) // force a distinct Unix-second timestamp
	mc.sample()
	mc.sample()

	if len(mc.mem) != 3 {
		t.Fatalf("expected sample() to keep appending (carrying the last value forward) through "+
			"a reader failure, got %d point(s): %v", len(mc.mem), mc.mem)
	}
	for i, p := range mc.mem {
		if p.V != 55 {
			t.Fatalf("point %d = %v, want carried-forward value 55", i, p)
		}
	}
	if mc.mem[len(mc.mem)-1].T <= firstT {
		t.Fatalf("carried-forward point's timestamp %d did not advance past the first point's %d — "+
			"the line wouldn't actually reach \"now\"", mc.mem[len(mc.mem)-1].T, firstT)
	}

	// And recovering should resume real readings, not get stuck on the
	// carried-forward value.
	memUp = true
	time.Sleep(1100 * time.Millisecond)
	mc.sample()
	if got := mc.mem[len(mc.mem)-1].V; got != 55 {
		// (55 is also the "real" reading here by construction; this mainly
		// guards against a future change accidentally freezing lastMemPct.)
		t.Fatalf("expected a real reading after recovery, got %v", got)
	}
}

// The macOS gap (all graphs stopping short of the right edge) came down to
// points being timestamped BEFORE the readers ran, not after: readCPUTotals
// there shells out to `top -l 1`, which blocks ~1s before emitting anything,
// so a `now` captured at the top of sample() was ~1s stale by the time the
// point was actually appended. With snapshot()'s server_now read fresh at
// request time, that staleness was a fixed gap between every series' newest
// point and the chart's right edge. This asserts the point carries a
// timestamp from AFTER its readers finished, not before.
func TestSampleTimestampsPointsAfterReadersFinish(t *testing.T) {
	const readDelay = 1200 * time.Millisecond
	origCPU := readCPUTotalsFn
	defer func() { readCPUTotalsFn = origCPU }()

	var tick uint64
	readCPUTotalsFn = func() (uint64, uint64, bool) {
		time.Sleep(readDelay)
		tick += 100
		return tick, tick / 4, true // ever-increasing total so a CPU point is produced
	}

	mc := newMetricsCollector(&stubBackend{}, logx.Default())
	mc.sample() // primes CPU delta state (no point yet)

	before := time.Now().Unix()
	mc.sample() // this one produces a CPU point; its readers block readDelay
	after := time.Now().Unix()

	if len(mc.cpu) == 0 {
		t.Fatal("expected a CPU point after the second sample")
	}
	ts := mc.cpu[len(mc.cpu)-1].T
	// The point must be stamped at/after the moment the (slow) readers
	// returned — i.e. no earlier than `before`+readDelay, and within the
	// sample's actual wall-clock span. A pre-read timestamp (the bug) would
	// be < before+1s.
	if ts < before {
		t.Fatalf("CPU point timestamp %d predates the sample() call start %d", ts, before)
	}
	if ts > after {
		t.Fatalf("CPU point timestamp %d is after sample() returned %d", ts, after)
	}
	// Most telling: the timestamp should reflect that ~1.2s elapsed reading,
	// i.e. it should be at least ~1s past when we started, not stamped up
	// front.
	if ts < before+1 {
		t.Fatalf("CPU point timestamp %d was stamped before the ~%v reader delay elapsed (start %d) — "+
			"points must be timestamped after their readers finish, not before", ts, readDelay, before)
	}
}
