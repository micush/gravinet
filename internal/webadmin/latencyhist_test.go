package webadmin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"gravinet/internal/mesh"
)

// TestLatencyCollectorRetention pins latencyRetention at its new 24-hour
// value (raised from 4h to back the Latency history modal's new 8/12/24hr
// range buttons): a point just inside the window must survive a sample
// pass, one just outside it must be trimmed. Exercised directly against the
// collector's sample()/trim path, since that's the thing this change
// actually touches — no need to wait on the real ticker or go through HTTP.
func TestLatencyCollectorRetention(t *testing.T) {
	be := &stubBackend{
		peersByNet: map[uint64][]mesh.PeerInfo{
			0x1234: {{NodeID: "peerX", Hostname: "hostx", Overlay4: "10.0.0.9", RTTMs: 12.5}},
		},
	}
	lc := newLatencyCollector(be, nil)
	now := time.Now().Unix()
	// appendTrim's prefix-scan assumes ascending time order, same as real
	// history (points are always appended as they're sampled) — oldest
	// point first.
	lc.nets["lan"] = map[string]*latencyPeerHistory{
		"peerX": {
			Hostname: "hostx", Overlay: "10.0.0.9",
			Hist: []metricPoint{
				{T: now - int64(25*time.Hour/time.Second), V: 99}, // outside 24h: must be trimmed
				{T: now - int64(23*time.Hour/time.Second), V: 10}, // inside 24h: must survive
			},
		},
	}
	lc.sample()

	hist := lc.nets["lan"]["peerX"].Hist
	for _, p := range hist {
		if p.V == 99 {
			t.Fatalf("the 25h-old point should have been trimmed by a 24h retention window, but it's still present: %+v", hist)
		}
	}
	found23h := false
	for _, p := range hist {
		if p.V == 10 {
			found23h = true
		}
	}
	if !found23h {
		t.Errorf("the 23h-old point should have survived a 24h retention window, got %+v", hist)
	}
}

// TestLatencyHistoryClampAllows24h locks in the /api/latency/history minutes
// clamp at 1440 (24h) rather than the old 240 (4h) ceiling — matching the
// frontend's new longest range button. Seeds a point old enough to fall
// inside the new ceiling but outside the old one, and confirms a request
// for the new range actually surfaces it (not just that the endpoint
// returns 200).
func TestLatencyHistoryClampAllows24h(t *testing.T) {
	srv, be, ts := newTestServer(t)
	// newTestServer drives the handler directly via httptest rather than
	// through Start() (the only place a real server constructs
	// s.latencyHist), so it's nil here by default — build one explicitly
	// and seed it directly rather than starting its background ticker.
	srv.latencyHist = newLatencyCollector(be, nil)
	c := sessionFor(t, ts)

	now := time.Now().Unix()
	srv.latencyHist.nets = map[string]map[string]*latencyPeerHistory{
		"lan": {"peerX": &latencyPeerHistory{
			Hostname: "hostx", Overlay: "10.0.0.9",
			Hist: []metricPoint{{T: now - int64(20*time.Hour/time.Second), V: 42}},
		}},
	}

	histFor := func(minutes int) []metricPoint {
		req, _ := http.NewRequest("GET", ts.URL+"/api/latency/history?minutes="+strconv.Itoa(minutes), nil)
		req.AddCookie(c)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Networks map[string]map[string]struct {
				Hist []metricPoint `json:"hist"`
			} `json:"networks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body.Networks["lan"]["peerX"].Hist
	}

	if h := histFor(60); len(h) != 0 {
		t.Errorf("a 60-minute request should not see a 20h-old point, got %+v", h)
	}
	// Past the old 240-minute ceiling, within the new 1440-minute one —
	// and a request past 1440 should clamp there, not reject.
	if h := histFor(100000); len(h) != 1 {
		t.Errorf("expected the 20h-old point within the new 24h ceiling, got %d points: %+v", len(h), h)
	}
}
