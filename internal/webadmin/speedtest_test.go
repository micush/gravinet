package webadmin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
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

	up := measureUpload(ts.URL + "/api/speedtest/sink")
	if up.Error != "" {
		t.Fatalf("upload error: %s", up.Error)
	}
	if up.Bytes == 0 || up.AvgMbps <= 0 {
		t.Fatalf("upload produced no throughput: %+v", up)
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
