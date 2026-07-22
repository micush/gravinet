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

	"gravinet/internal/mesh"
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

// stPktSample is stSample's packet-rate counterpart, plotted as its own
// chart rather than a second series on the Mbps one (very different scales —
// Mbps in the tens to low hundreds, pps in the thousands — so sharing one
// y-axis would flatten one of them). T is on the same start-relative time
// base as stSample's, even though the two are graphed separately.
type stPktSample struct {
	T   float64 `json:"t"`
	Pps float64 `json:"pps"`
}

type stResult struct {
	Samples []stSample `json:"samples"`
	AvgMbps float64    `json:"avg_mbps"`
	Bytes   int64      `json:"bytes"`
	// DurationSec is the actual measured window this result covers — the
	// same interval AvgMbps was computed over, in seconds since it's a
	// wall-clock quantity computed from a time.Duration rather than a raw
	// count. Zero when nothing was measured at all (a connection that failed
	// before reading/writing began). handleSpeedtestRun divides a packet-count
	// delta by this, not by the wall time around the whole request, so
	// PacketsPerSec and AvgMbps are scoped to the identical interval.
	DurationSec float64 `json:"duration_sec"`
	// PacketsPerSec is the outer-datagram rate this transfer drove to the
	// target peer, sourced from that peer's own TxPackets/RxPackets session
	// counters (see peerSession's doc comment on why those, not
	// FragsSent/FragsRcvd, are the honest choice here) snapshotted immediately
	// before and after this phase by handleSpeedtestRun — set there, not in
	// measureDownload/measureUpload, since only the caller has the backend
	// handle needed to read peer counters. Omitted (stays 0) when the target
	// couldn't be found in this node's own peer list at snapshot time, which
	// the UI should treat as "not available" rather than "zero packets".
	PacketsPerSec float64 `json:"packets_per_sec,omitempty"`
	// PacketSamples is PacketsPerSec's time series, for its own chart —
	// collected by pktSampleLoop polling the same peer counters every
	// stSampleEvery while this phase runs, concurrently with
	// measureDownload/measureUpload rather than inside them (same reason
	// PacketsPerSec itself is computed in handleSpeedtestRun: only the
	// caller has the backend handle). Empty, not a zero-filled series, when
	// the peer was never found during this phase.
	PacketSamples []stPktSample `json:"packet_samples,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// findPeerByOverlay looks up a peer's current PeerInfo by its overlay
// address, searching every network this node has configured — the caller
// (handleSpeedtestRun) only has the target's overlay IP, not which network
// it's on, the same situation OverlayReachable is already used to resolve.
// ok is false if no network's peer list has a matching entry right now
// (the peer hasn't fully connected yet, or dropped between requests); the
// caller treats that as "no packet counters available for this run" rather
// than an error, since the throughput measurement itself doesn't depend on
// finding it.
func findPeerByOverlay(be Backend, overlay netip.Addr) (mesh.PeerInfo, bool) {
	want := overlay.String()
	for _, netID := range be.NetworkIDs() {
		for _, p := range be.ListPeers(netID) {
			if p.Overlay4 == want || p.Overlay6 == want {
				return p, true
			}
		}
	}
	return mesh.PeerInfo{}, false
}

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

	// Snapshot the target's own packet counters immediately around each
	// phase — as close to the actual measured window as this can get without
	// threading the backend handle into measureDownload/measureUpload
	// themselves. The gap this leaves is connect time: a slow/cold connect
	// (up to stConnectSlack) sits inside the snapshot window but outside
	// DurationSec, so a keepalive that happens to land during a slow connect
	// would count toward the packet delta without contributing to the
	// duration it's divided by. In practice this is negligible — the bulk
	// transfer that dominates the window (64KB chunks flooding for
	// stDuration) vastly outnumbers one incidental keepalive — and not worth
	// the more invasive refactor an exact fix would need. The same gap
	// applies to pktSampleLoop's time series below it, for the same reason:
	// it also has to wrap the whole measure call from outside rather than
	// start only once reading/writing actually begins.
	beforeDown, haveBeforeDown := findPeerByOverlay(s.be, ip)
	downPktDone := make(chan struct{})
	downPktOut := make(chan []stPktSample, 1)
	go pktSampleLoop(s.be, ip, true /* rx: download counts our RxPackets */, time.Now(), downPktDone, downPktOut)
	down := measureDownload(base + "/api/speedtest/source")
	close(downPktDone)
	down.PacketSamples = <-downPktOut
	afterDown, haveAfterDown := findPeerByOverlay(s.be, ip)
	// Download: this node is the receiver, so it's our RxPackets that moved.
	down.PacketsPerSec = packetsPerSec(beforeDown.RxPackets, afterDown.RxPackets, haveBeforeDown, haveAfterDown, down.DurationSec)

	beforeUp, haveBeforeUp := findPeerByOverlay(s.be, ip)
	upPktDone := make(chan struct{})
	upPktOut := make(chan []stPktSample, 1)
	go pktSampleLoop(s.be, ip, false /* tx: upload counts our TxPackets */, time.Now(), upPktDone, upPktOut)
	up := measureUpload(base + "/api/speedtest/sink")
	close(upPktDone)
	up.PacketSamples = <-upPktOut
	afterUp, haveAfterUp := findPeerByOverlay(s.be, ip)
	// Upload: this node is the sender, so it's our TxPackets that moved.
	up.PacketsPerSec = packetsPerSec(beforeUp.TxPackets, afterUp.TxPackets, haveBeforeUp, haveAfterUp, up.DurationSec)

	writeJSON(w, http.StatusOK, map[string]any{
		"target_hostname": req.TargetHostname,
		"download":        down,
		"upload":          up,
	})
}

// packetsPerSec turns a before/after outer-datagram count and a measured
// duration into a rate, honestly: it reports 0 — never a negative or
// fabricated number — whenever the inputs can't support a real answer. That
// covers three cases: the peer wasn't in this node's own peer list at one end
// of the snapshot (haveBefore/haveAfter false — it hadn't fully connected, or
// dropped between requests), nothing was actually measured (durSec <= 0, the
// connection failed before any read/write began), or the counter went
// backwards (after < before) — which happens if the session re-handshook
// between the two snapshots and its packet counters reset with it, the same
// event that resets EstablishedAt. A reset invalidates the delta outright
// rather than just meaning "very few packets," so this refuses to guess.
func packetsPerSec(before, after uint64, haveBefore, haveAfter bool, durSec float64) float64 {
	if !haveBefore || !haveAfter || durSec <= 0 || after < before {
		return 0
	}
	return float64(after-before) / durSec
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

// pktSampleLoop is sampleLoop's packet-rate counterpart, same interval and
// same start-relative timestamps, but a different source entirely: it polls
// the target peer's own TxPackets/RxPackets session counters through be
// (findPeerByOverlay) rather than a local byte meter, since packet counts
// aren't something measureDownload/measureUpload can see themselves — only
// the caller holds the backend handle that can read them. rx selects which
// counter tracks this phase's direction (true for download, false for
// upload — same mapping handleSpeedtestRun's single before/after
// PacketsPerSec snapshot uses).
//
// The first successful read only establishes a baseline; it can't yet
// produce a sample, since a session's packet counters are cumulative over
// its whole lifetime, not zeroed at the start of this call the way the local
// byte meter is — there is no meaningful "delta since zero" for a number
// that didn't start at zero. A tick where the peer isn't currently listed
// (haveBefore-style miss) is skipped rather than treated as zero traffic;
// the next successful tick resumes from its own fresh baseline rather than
// spanning the gap with the previous one, which would understate the true
// rate by including that observation gap as this-tick elapsed time.
func pktSampleLoop(be Backend, overlay netip.Addr, rx bool, start time.Time, done <-chan struct{}, out chan<- []stPktSample) {
	var samples []stPktSample
	tick := time.NewTicker(stSampleEvery)
	defer tick.Stop()
	var last uint64
	haveLast := false
	lastT := start
	for {
		select {
		case <-done:
			out <- samples
			return
		case now := <-tick.C:
			p, ok := findPeerByOverlay(be, overlay)
			if !ok {
				haveLast = false // resume from a fresh baseline on the next successful read
				continue
			}
			cur := p.TxPackets
			if rx {
				cur = p.RxPackets
			}
			if haveLast && cur >= last {
				if dt := now.Sub(lastT).Seconds(); dt > 0 {
					samples = append(samples, stPktSample{
						T:   now.Sub(start).Seconds(),
						Pps: float64(cur-last) / dt,
					})
				}
			}
			last, lastT, haveLast = cur, now, true
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
	return stResult{Samples: <-out, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur), DurationSec: dur.Seconds()}
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
		return stResult{Samples: samples, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur), DurationSec: dur.Seconds(), Error: "upload: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("upload: peer returned %d", resp.StatusCode)
		if detail := peerErrorDetail(resp); detail != "" {
			msg += ": " + detail
		}
		resp.Body.Close()
		return stResult{Samples: samples, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur), DurationSec: dur.Seconds(), Error: msg}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	return stResult{Samples: samples, Bytes: m.load(), AvgMbps: avgMbps(m.load(), dur), DurationSec: dur.Seconds()}
}
