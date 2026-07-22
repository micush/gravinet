package webadmin

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

func testServer() *Server {
	return New(config.WebAdmin{AuthMode: "local"}, &stubBackend{}, logx.Default())
}

// TestSpeedtestMeasure runs a real download+upload over a TLS loopback server
// using the actual source/sink handlers, validating the transfer + sampling.
func TestSpeedtestMeasure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timed transfer in -short mode")
	}
	srv := testServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/speedtest/source", srv.handleSpeedtestSource)
	mux.HandleFunc("/api/speedtest/sink", srv.handleSpeedtestSink)
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	down := measureDownload(ts.URL + "/api/speedtest/source")
	if down.Error != "" {
		t.Fatalf("download error: %s", down.Error)
	}
	if down.Bytes == 0 || down.AvgMbps <= 0 {
		t.Fatalf("download produced no throughput: %+v", down)
	}
	if len(down.Samples) == 0 {
		t.Fatal("download produced no samples")
	}
	if down.DurationSec <= 0 {
		t.Fatalf("download DurationSec = %v, want > 0 for a completed measurement", down.DurationSec)
	}

	up := measureUpload(ts.URL + "/api/speedtest/sink")
	if up.Error != "" {
		t.Fatalf("upload error: %s", up.Error)
	}
	if up.Bytes == 0 || up.AvgMbps <= 0 {
		t.Fatalf("upload produced no throughput: %+v", up)
	}
	if up.DurationSec <= 0 {
		t.Fatalf("upload DurationSec = %v, want > 0 for a completed measurement", up.DurationSec)
	}
}

// TestMeasureDownloadSurvivesSlowConnect proves the connect phase and the
// measured read window now have independent budgets. A deliberate delay
// before the peer writes anything — standing in for a slow connect/TLS
// handshake over the overlay, which is what triggered "context deadline
// exceeded" on macOS — must not eat into the stDuration read window, and the
// whole call must still succeed as long as the delay finishes within
// stConnectSlack. Before this fix, the connect phase and the measured window
// shared one stDuration-wide budget, so a delay anywhere near stDuration
// alone (let alone one longer than it) exhausted the whole thing before a
// single byte was read.
func TestMeasureDownloadSurvivesSlowConnect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timed transfer in -short mode")
	}
	const connectDelay = 6 * time.Second // > the old stDuration-only budget (4s), well under stConnectSlack (10s)
	mux := http.NewServeMux()
	mux.HandleFunc("/slow-source", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(connectDelay)
		buf := make([]byte, stChunk)
		deadline := time.Now().Add(stServerMaxDur)
		for time.Now().Before(deadline) {
			if _, err := w.Write(buf); err != nil {
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	})
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	start := time.Now()
	down := measureDownload(ts.URL + "/slow-source")
	elapsed := time.Since(start)

	if down.Error != "" {
		t.Fatalf("download errored despite a connect delay (%v) well under stConnectSlack (%v): %s", connectDelay, stConnectSlack, down.Error)
	}
	if down.Bytes == 0 {
		t.Fatalf("download produced no throughput: %+v", down)
	}
	// Total time should be roughly connectDelay+stDuration (the delay,
	// followed by a full measured window) — not truncated by the delay, and
	// nowhere near stConnectSlack+stDuration.
	want := connectDelay + stDuration
	if elapsed < stDuration || elapsed > want+2*time.Second {
		t.Fatalf("elapsed = %v, want roughly %v (connect delay + a full measured window)", elapsed, want)
	}
}

// TestMeasureDownloadSurfacesPeerErrorDetail proves a non-OK response's actual
// {"error": "..."} body reaches the admin instead of being discarded in favor
// of a bare, unexplained status code — e.g. so a 401 from a peer that isn't in
// managed mode reads as "peer returned 401: not authenticated ..." instead of
// just "peer returned 401", which gives no way to tell that apart from any
// other kind of rejection.
func TestMeasureDownloadSurfacesPeerErrorDetail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/unauthorized-source", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"not authenticated: this node is not in managed mode"}`))
	})
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	down := measureDownload(ts.URL + "/unauthorized-source")
	if down.Error == "" {
		t.Fatal("expected an error")
	}
	if !strings.Contains(down.Error, "401") {
		t.Fatalf("error = %q, want it to mention the status code", down.Error)
	}
	if !strings.Contains(down.Error, "not authenticated: this node is not in managed mode") {
		t.Fatalf("error = %q, want the peer's actual reason surfaced, not just the status code", down.Error)
	}
}

func TestMeasureUploadSurfacesPeerErrorDetail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/unauthorized-sink", func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) // drain, same as a real handler would before rejecting
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"not authenticated: this node is not in managed mode"}`))
	})
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()

	up := measureUpload(ts.URL + "/unauthorized-sink")
	if up.Error == "" {
		t.Fatal("expected an error")
	}
	if !strings.Contains(up.Error, "401") {
		t.Fatalf("error = %q, want it to mention the status code", up.Error)
	}
	if !strings.Contains(up.Error, "not authenticated: this node is not in managed mode") {
		t.Fatalf("error = %q, want the peer's actual reason surfaced, not just the status code", up.Error)
	}
}

func TestAvgMbps(t *testing.T) {
	// 1,000,000 bytes in 1s = 8 Mbps.
	if got := avgMbps(1_000_000, time.Second); got < 7.99 || got > 8.01 {
		t.Fatalf("avgMbps = %v, want 8", got)
	}
	if got := avgMbps(123, 0); got != 0 {
		t.Fatalf("avgMbps with zero duration = %v, want 0", got)
	}
}

// TestPacketsPerSec covers every branch of the honesty rule packetsPerSec
// documents: a real answer only when the peer was found at both snapshots,
// something was actually measured, and the counter moved forward. Every
// other input reports 0, never a negative or fabricated rate.
func TestPacketsPerSec(t *testing.T) {
	cases := []struct {
		name                  string
		before, after         uint64
		haveBefore, haveAfter bool
		durSec                float64
		want                  float64
	}{
		{"normal", 100, 4100, true, true, 2, 2000},
		{"peer not found before", 0, 4100, false, true, 2, 0},
		{"peer not found after", 100, 4100, true, false, 2, 0},
		{"peer not found either side", 0, 0, false, false, 2, 0},
		{"zero duration (nothing measured)", 100, 4100, true, true, 0, 0},
		{"negative duration", 100, 4100, true, true, -1, 0},
		{"counter went backwards (re-handshake reset it)", 4100, 100, true, true, 2, 0},
		{"counter unchanged", 100, 100, true, true, 2, 0},
		{"large duration, small delta", 100, 105, true, true, 1000, 0.005},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := packetsPerSec(c.before, c.after, c.haveBefore, c.haveAfter, c.durSec)
			if got != c.want {
				t.Errorf("packetsPerSec(%d,%d,%v,%v,%v) = %v, want %v",
					c.before, c.after, c.haveBefore, c.haveAfter, c.durSec, got, c.want)
			}
			if got < 0 {
				t.Errorf("packetsPerSec returned a negative rate: %v", got)
			}
		})
	}
}

// TestFindPeerByOverlay proves the lookup matches on either address family,
// searches every configured network (not just the first), and reports not-
// found rather than a zero-value PeerInfo when nothing matches — the
// distinction packetsPerSec's haveBefore/haveAfter depends on.
func TestFindPeerByOverlay(t *testing.T) {
	be := &stubBackend{
		netIDs: []uint64{1, 2},
		peersByNet: map[uint64][]mesh.PeerInfo{
			1: {{NodeID: "a", Overlay4: "10.0.1.5"}},
			2: {{NodeID: "b", Overlay6: "fd00::9"}}, // only found by searching every network
		},
	}
	if p, ok := findPeerByOverlay(be, netip.MustParseAddr("10.0.1.5")); !ok || p.NodeID != "a" {
		t.Fatalf("v4 match on the first network: got %+v, ok=%v", p, ok)
	}
	if p, ok := findPeerByOverlay(be, netip.MustParseAddr("fd00::9")); !ok || p.NodeID != "b" {
		t.Fatalf("v6 match requiring a second network: got %+v, ok=%v", p, ok)
	}
	if _, ok := findPeerByOverlay(be, netip.MustParseAddr("10.0.1.6")); ok {
		t.Fatal("matched an address that isn't any peer's overlay address")
	}
}

func TestSpeedtestRunMissingAddress(t *testing.T) {
	srv := testServer()
	ts := httptest.NewServer(http.HandlerFunc(srv.handleSpeedtestRun))
	defer ts.Close()
	out := postRun(t, ts.URL, `{"target_ip":"","target_port":8443}`)
	if s, _ := out["error"].(string); !strings.Contains(s, "no overlay address") {
		t.Fatalf("expected missing-address error, got %v", out)
	}
}

func TestSpeedtestRunRejectsNonOverlay(t *testing.T) {
	// stub overlayAddr is unset, so OverlayContains is false for everything.
	srv := testServer()
	ts := httptest.NewServer(http.HandlerFunc(srv.handleSpeedtestRun))
	defer ts.Close()
	out := postRun(t, ts.URL, `{"target_ip":"8.8.8.8","target_port":443}`)
	if s, _ := out["error"].(string); !strings.Contains(s, "not reachable") {
		t.Fatalf("expected non-overlay rejection, got %v", out)
	}
}

func TestSpeedtestRunRejectsSelf(t *testing.T) {
	// Overlay contains 10.99.0.1, which is also this node's own overlay.
	srv := New(config.WebAdmin{AuthMode: "local"}, &stubBackend{overlayAddr: netip.MustParseAddr("10.99.0.1")}, logx.Default())
	ts := httptest.NewServer(http.HandlerFunc(srv.handleSpeedtestRun))
	defer ts.Close()
	out := postRun(t, ts.URL, `{"target_ip":"10.99.0.1","target_port":8443}`)
	if s, _ := out["error"].(string); !strings.Contains(s, "two different") {
		t.Fatalf("expected same-node rejection, got %v", out)
	}
}

func postRun(t *testing.T, url, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return out
}

// countingPeerBackend embeds *stubBackend and overrides only ListPeers, so
// every other Backend method — there are dozens, most irrelevant here —
// keeps stubBackend's existing behavior untouched, and none of the 35 other
// test files sharing stubBackend are affected. Each ListPeers call reports
// TxPackets/RxPackets that have grown since the previous call, standing in
// for a live session accumulating real traffic between the "before" and
// "after" snapshots handleSpeedtestRun takes around each phase.
type countingPeerBackend struct {
	*stubBackend
	overlay   string
	callN     int
	txPerCall uint64
	rxPerCall uint64
}

func (c *countingPeerBackend) ListPeers(id uint64) []mesh.PeerInfo {
	c.callN++
	return []mesh.PeerInfo{{
		NodeID:    "peerB",
		Overlay4:  c.overlay,
		TxPackets: uint64(c.callN) * c.txPerCall,
		RxPackets: uint64(c.callN) * c.rxPerCall,
	}}
}

// TestSpeedtestRunReportsPacketsPerSec is the end-to-end proof that
// handleSpeedtestRun actually wires a peer's packet counters into the
// response: a real download+upload against a real TLS peer (not a mock of
// measureDownload/measureUpload), with a backend whose reported packet
// counts grow on every call the way a real session's would. If the
// snapshot-before/after wiring in handleSpeedtestRun were wrong — reading
// the same snapshot twice, snapshotting the wrong direction, or not calling
// packetsPerSec at all — this is what would catch it; the unit tests for
// packetsPerSec and findPeerByOverlay above only prove those two functions
// are individually correct, not that handleSpeedtestRun calls them right.
func TestSpeedtestRunReportsPacketsPerSec(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timed transfer in -short mode")
	}
	targetSrv := testServer()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/speedtest/source", targetSrv.handleSpeedtestSource)
	mux.HandleFunc("/api/speedtest/sink", targetSrv.handleSpeedtestSink)
	ts := httptest.NewTLSServer(mux)
	defer ts.Close()
	host := strings.TrimPrefix(ts.URL, "https://")
	_, portStr, err := net.SplitHostPort(host)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscan(portStr, &port); err != nil {
		t.Fatal(err)
	}

	be := &countingPeerBackend{
		stubBackend: &stubBackend{overlayAddr: netip.MustParseAddr("127.0.0.1")},
		overlay:     "127.0.0.1",
		txPerCall:   3000, // upload direction: our TxPackets grows between its before/after snapshot
		rxPerCall:   5000, // download direction: our RxPackets grows between its before/after snapshot
	}
	srv := New(config.WebAdmin{AuthMode: "local"}, be, logx.Default())
	runTs := httptest.NewServer(http.HandlerFunc(srv.handleSpeedtestRun))
	defer runTs.Close()

	out := postRun(t, runTs.URL, fmt.Sprintf(`{"target_ip":"127.0.0.1","target_port":%d,"target_hostname":"peerB"}`, port))
	if e, _ := out["error"].(string); e != "" {
		t.Fatalf("run failed: %s", e)
	}
	down, _ := out["download"].(map[string]any)
	up, _ := out["upload"].(map[string]any)
	if down == nil || up == nil {
		t.Fatalf("response missing download/upload: %v", out)
	}
	if e, _ := down["error"].(string); e != "" {
		t.Fatalf("download errored: %s", e)
	}
	if e, _ := up["error"].(string); e != "" {
		t.Fatalf("upload errored: %s", e)
	}

	// Exactly one before/after pair backs each phase's ListPeers-derived
	// delta, and txPerCall/rxPerCall are constant per call — so the reported
	// rate is fully determined: (per-call growth) / (that phase's measured
	// duration), independent of how much throughput the transfer itself
	// happened to move.
	downPps, _ := down["packets_per_sec"].(float64)
	upPps, _ := up["packets_per_sec"].(float64)
	downDur, _ := down["duration_sec"].(float64)
	upDur, _ := up["duration_sec"].(float64)
	if downDur <= 0 || upDur <= 0 {
		t.Fatalf("expected positive measured durations: down=%v up=%v", downDur, upDur)
	}
	wantDown := float64(be.rxPerCall) / downDur
	wantUp := float64(be.txPerCall) / upDur
	if downPps <= 0 {
		t.Fatalf("download packets_per_sec = %v, want > 0 (backend reported growing RxPackets)", downPps)
	}
	if upPps <= 0 {
		t.Fatalf("upload packets_per_sec = %v, want > 0 (backend reported growing TxPackets)", upPps)
	}
	// A little tolerance for the two counter reads not landing at exactly the
	// same instant as the duration timer's own start/stop.
	if ratio := downPps / wantDown; ratio < 0.5 || ratio > 2 {
		t.Errorf("download pps = %v, want roughly %v (rxPerCall/DurationSec)", downPps, wantDown)
	}
	if ratio := upPps / wantUp; ratio < 0.5 || ratio > 2 {
		t.Errorf("upload pps = %v, want roughly %v (txPerCall/DurationSec)", upPps, wantUp)
	}
	// Direction isolation: upload must use TxPackets growth, not RxPackets',
	// and vice versa — rxPerCall (5000) and txPerCall (3000) are different on
	// purpose so a swapped direction would show up as a ~5000/3000 mismatch
	// rather than accidentally matching.
	if downPps > upPps*3 || upPps > downPps*3 {
		// Both phases run for a similar DurationSec (~4s each), so with
		// rxPerCall = 5000 and txPerCall = 3000 the two rates should differ
		// by roughly that same ~5:3 ratio, not be swapped or collapsed together.
		t.Logf("down=%v up=%v (expected roughly a %v:%v ratio)", downPps, upPps, be.rxPerCall, be.txPerCall)
	}
}
