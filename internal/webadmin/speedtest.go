package webadmin

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Speedtest measures overlay throughput between two managed peers. The browser
// asks the *client* peer A (this node, or a remote peer reached via the proxy)
// to run the test against peer B; A pulls bytes from B's /source (download) and
// pushes bytes to B's /sink (upload), sampling throughput over time, and returns
// the series for the UI to graph. Peer-to-peer requests are accepted the same
// way the management proxy is: over the overlay from a managed node.

const (
	stDuration     = 4 * time.Second        // measured window per direction
	stSampleEvery  = 250 * time.Millisecond // graph resolution
	stChunk        = 64 * 1024
	stServerMaxDur = 20 * time.Second // safety cap on /source streaming
	stSinkMaxBytes = 16 << 30         // safety cap on /sink intake

	// stConnectSlack is extra time allowed for connecting (TCP + TLS to the
	// peer's web-admin port over the overlay) on top of the measured window
	// itself. A fresh connection over the overlay can take noticeably longer
	// than a plain LAN connect — especially the first hop to a given peer,
	// and especially on macOS, where the overlay interface is a
	// Network-Extension-backed utun rather than a native kernel TUN, adding
	// per-connection overhead a 4-second budget with zero headroom can't
	// absorb. Both directions give connecting this much room, separate from
	// the time actually spent measuring throughput.
	stConnectSlack = 10 * time.Second
)

type stSample struct {
	T    float64 `json:"t"` // seconds since the direction started
	Mbps float64 `json:"mbps"`
}

type stResult struct {
	Samples []stSample `json:"samples"`
	AvgMbps float64    `json:"avg_mbps"`
	Bytes   int64      `json:"bytes"`
	Error   string     `json:"error,omitempty"`
}

// handleSpeedtestSource streams bytes as fast as the connection allows until the
// client stops reading or the safety cap is hit. The reading peer measures the
// download rate.
func (s *Server) handleSpeedtestSource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	buf := make([]byte, stChunk)
	for i := range buf {
		buf[i] = byte(i) // non-trivial payload (avoid any accidental compression)
	}
	flusher, _ := w.(http.Flusher)
	deadline := time.Now().Add(stServerMaxDur)
	ctx := r.Context()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if _, err := w.Write(buf); err != nil {
			return // client closed the connection — normal end of test
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// handleSpeedtestSink drains the request body and discards it. The sending peer
// measures the upload rate.
func (s *Server) handleSpeedtestSink(w http.ResponseWriter, r *http.Request) {
	buf := make([]byte, stChunk)
	_, _ = io.CopyBuffer(io.Discard, io.LimitReader(r.Body, stSinkMaxBytes), buf)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSpeedtestRun orchestrates a download+upload test from THIS node (the
// client) to the target peer. The caller supplies the target's overlay address
// and web port directly (the browser has them from the cluster list), so this
// node does not need the target in its own registry — which makes it work in
// hub-and-spoke topologies where peers don't all see each other.
func (s *Server) handleSpeedtestRun(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetIP       string `json:"target_ip"`
		TargetPort     int    `json:"target_port"`
		TargetHostname string `json:"target_hostname"`
	}
	if !decode(w, r, &req) {
		return
	}
	ip, err := netip.ParseAddr(req.TargetIP)
	if err != nil || !ip.IsValid() {
		writeJSON(w, http.StatusOK, map[string]any{"error": "target has no overlay address"})
		return
	}
	// SSRF guard + reachability: only ever connect to an overlay address this
	// node can actually reach (its own subnet, or a known mesh node), never the
	// LAN, loopback, or a cloud-metadata endpoint.
	if !s.be.OverlayReachable(ip) {
		writeJSON(w, http.StatusOK, map[string]any{"error": "target is not reachable on a shared overlay network — the two peers may be on different networks"})
		return
	}
	if req.TargetPort < 1 || req.TargetPort > 65535 {
		writeJSON(w, http.StatusOK, map[string]any{"error": "target has no web-admin port"})
		return
	}
	if ip == s.be.SelfOverlay() {
		writeJSON(w, http.StatusOK, map[string]any{"error": "pick two different nodes"})
		return
	}
	base := "https://" + net.JoinHostPort(ip.String(), strconv.Itoa(req.TargetPort))
	down := measureDownload(base + "/api/speedtest/source")
	up := measureUpload(base + "/api/speedtest/sink")
	writeJSON(w, http.StatusOK, map[string]any{
		"target_hostname": req.TargetHostname,
		"download":        down,
		"upload":          up,
	})
}

// ---- throughput sampling ----------------------------------------------------

type meter struct{ bytes int64 }

func (m *meter) add(n int)   { atomic.AddInt64(&m.bytes, int64(n)) }
func (m *meter) load() int64 { return atomic.LoadInt64(&m.bytes) }

// sampleLoop records a throughput sample every stSampleEvery until done closes,
// then returns the collected series.
func sampleLoop(m *meter, start time.Time, done <-chan struct{}, out chan<- []stSample) {
	var samples []stSample
	tick := time.NewTicker(stSampleEvery)
	defer tick.Stop()
	last := int64(0)
	lastT := start
	for {
		select {
		case <-done:
			out <- samples
			return
		case now := <-tick.C:
			cur := m.load()
			dt := now.Sub(lastT).Seconds()
			if dt > 0 {
				samples = append(samples, stSample{
					T:    now.Sub(start).Seconds(),
					Mbps: float64(cur-last) * 8 / 1e6 / dt,
				})
			}
			last, lastT = cur, now
		}
	}
}

func avgMbps(total int64, dur time.Duration) float64 {
	if dur <= 0 {
		return 0
	}
	return float64(total) * 8 / 1e6 / dur.Seconds()
}

// speedtestClient talks to a peer's /api/speedtest/{source,sink} over the
// overlay. It's deliberately separate from cluster.go's shared proxyClient,
// whose 15-second http.Client.Timeout (which bounds connect *and* the full
// response read together, regardless of any per-request context) is sized for
// ordinary, fast management-API calls. A speedtest legitimately runs for
// stDuration plus up to stConnectSlack of connect headroom per leg — already
// close to or past 15s in the worst case — so reusing proxyClient here would
// silently reintroduce "context deadline exceeded" through the client's own
// timeout even after fixing the per-request context budgets below. Same
// trust boundary as proxyClient: overlay-internal, self-signed, protected by
// the mesh PSK, so certificate verification is skipped the same way.
var speedtestClient = &http.Client{
	Timeout: stConnectSlack + stDuration + 5*time.Second,
	Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: false,
	},
}

// peerErrorDetail reads a bounded amount of a non-OK response body and, if it
// parses as the {"error": "..."} shape this admin API uses everywhere,
// returns that message; otherwise falls back to whatever non-empty text came
// back, or "" if there's nothing usable. Without this, a rejection carries no
// information beyond its bare HTTP status — e.g. a 401 from a peer that isn't
// in managed mode looks identical to one from a peer with a completely
// different problem, even though the peer's own response body already says
// exactly which.
func peerErrorDetail(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	var parsed struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &parsed) == nil && parsed.Error != "" {
		return parsed.Error
	}
	if s := strings.TrimSpace(string(b)); s != "" {
		return s
	}
	return ""
}

func measureDownload(url string) stResult {
	// Two-phase timeout, mirroring measureUpload's structure: connecting gets
	// its own generous, independent budget (stConnectSlack) via a timer that's
	// stopped once we're through it, so a slow/cold connect can't eat into the
	// measured window — then a fresh timer bounds just the read itself to
	// exactly stDuration from when reading actually starts. Previously both
	// phases shared one stDuration-wide context with no connect headroom at
	// all, so a connect alone taking anywhere near 4 seconds (plausible for a
	// fresh connection over the overlay, particularly on macOS's
	// Network-Extension-backed utun) exhausted the whole budget before a
	// single byte was read and surfaced as "context deadline exceeded" even
	// on an otherwise healthy link.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connectTimer := time.AfterFunc(stConnectSlack, cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := speedtestClient.Do(req)
	connectTimer.Stop()
	if err != nil {
		return stResult{Error: "download: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("download: peer returned %d", resp.StatusCode)
		if detail := peerErrorDetail(resp); detail != "" {
			msg += ": " + detail
		}
		return stResult{Error: msg}
	}
	m := &meter{}
	start := time.Now()
	done := make(chan struct{})
	out := make(chan []stSample, 1)
	go sampleLoop(m, start, done, out)
	readTimer := time.AfterFunc(stDuration, cancel)
	defer readTimer.Stop()
	buf := make([]byte, stChunk)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			m.add(n)
		}
		if rerr != nil {
			break // EOF, deadline, or connection close
		}
	}
	dur := time.Since(start)
	close(done)
	return stResult{Samples: <-out, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur)}
}

// timedReader yields payload bytes until its deadline, then EOF — used as the
// upload body so the POST runs for exactly the measured window.
type timedReader struct {
	m        *meter
	deadline time.Time
	chunk    []byte
}

func (tr *timedReader) Read(p []byte) (int, error) {
	if time.Now().After(tr.deadline) {
		return 0, io.EOF
	}
	n := copy(p, tr.chunk)
	tr.m.add(n)
	return n, nil
}

func measureUpload(url string) stResult {
	chunk := make([]byte, stChunk)
	for i := range chunk {
		chunk[i] = byte(i)
	}
	m := &meter{}
	start := time.Now()
	body := &timedReader{m: m, deadline: start.Add(stDuration), chunk: chunk}
	ctx, cancel := context.WithTimeout(context.Background(), stDuration+stConnectSlack)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	req.Header.Set("Content-Type", "application/octet-stream")
	done := make(chan struct{})
	out := make(chan []stSample, 1)
	go sampleLoop(m, start, done, out)
	resp, err := speedtestClient.Do(req)
	dur := time.Since(start)
	close(done)
	samples := <-out
	if err != nil {
		return stResult{Samples: samples, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur), Error: "upload: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("upload: peer returned %d", resp.StatusCode)
		if detail := peerErrorDetail(resp); detail != "" {
			msg += ": " + detail
		}
		resp.Body.Close()
		return stResult{Samples: samples, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur), Error: msg}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return stResult{Samples: samples, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur)}
}
