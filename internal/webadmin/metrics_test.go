package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gravinet/internal/config"
	"gravinet/internal/logx"
)

func TestMetricsCollectorSample(t *testing.T) {
	be := &stubBackend{}
	mc := newMetricsCollector(be)
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
}

func TestMetricsWindowClamp(t *testing.T) {
	mc := newMetricsCollector(&stubBackend{})
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
	srv.metrics = newMetricsCollector(srv.be)
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
}
