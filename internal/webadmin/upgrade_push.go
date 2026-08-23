package webadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// pushConcurrency bounds how many peers are built at once. Every target
// compiles the archive locally, so a wide fan-out means N simultaneous Go
// builds across the fleet — on the small boxes gravinet often runs on, that is
// the difference between a rollout and a self-inflicted outage. Sequential
// would be safest and far too slow for a real fleet; this is the middle.
const pushConcurrency = 4

// handleUpgradePush distributes an uploaded source archive to one or more
// managed peers, from the node the operator is logged into. It is the
// counterpart to each peer's handleUpgradeRemoteApply: this side does the
// pushing, that side does the (opt-in, verified) accepting.
//
// One upload, one archive, every platform. Because what is distributed is
// source rather than a built binary, the operator does not select peers by
// architecture, cross-compile anything, or hold a matrix of artifacts — a
// mesh of Linux, FreeBSD, OpenBSD, macOS and Windows nodes all take the same
// bytes and each compiles its own native binary from them.
//
// Like the other fleet-driving actions (see handleProxy's blocklist), this is
// driven from the node you are actually looking at and is never itself proxied
// to a peer — "node A tells node B to push B's archive across B's mesh" is
// exactly the two-managers confusion that blocklist prevents, so this handler
// is local-session-only.
//
// Request: a multipart POST carrying a "nodes" field (a JSON array of peer
// names) and a "source" file (the archive). The archive is spooled once and
// hashed once; every peer then receives the same bytes and the same digest.
//
// Response: newline-delimited JSON, one object per line. Each per-peer result
// is written and flushed the moment that peer's own build+apply finishes —
// not buffered until the slowest peer is done — so a client reading the
// response as it arrives (see drawUpgrade's fetch in ui.go) can show each
// peer's outcome as soon as it's known. Lines arrive in whatever order peers
// actually finish in, not the order "nodes" listed them. A final line
// {"done":true,...} carries the summary once every peer has reported in.
func (s *Server) handleUpgradePush(w http.ResponseWriter, r *http.Request) {
	// Local-only: this drives a fleet action, so it must originate at the node
	// the operator is logged into, never arrive over the proxy from a peer.
	if !s.upgradeLocalOnly(w, r) {
		return
	}
	if s.upgradeOff(w) {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "POST required"})
		return
	}

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected a multipart upload: " + err.Error()})
		return
	}
	var nodes []string
	spooled, sum := "", ""
	defer func() {
		if spooled != "" {
			os.Remove(spooled)
		}
	}()
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		switch part.FormName() {
		case "nodes":
			b, err := io.ReadAll(io.LimitReader(part, 1<<20))
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			if err := json.Unmarshal(b, &nodes); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nodes must be a JSON array of peer names: " + err.Error()})
				return
			}
		case "source":
			if len(nodes) == 0 {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the nodes list must arrive before the source archive"})
				return
			}
			path, got, err := spoolUpload(s.upg.StateDir, part)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			spooled, sum = path, got
		}
	}
	if len(nodes) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "a non-empty nodes list is required"})
		return
	}
	if spooled == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the push carried no source archive"})
		return
	}

	type result struct {
		Node    string `json:"node"`
		OK      bool   `json:"ok"`
		Status  int    `json:"status,omitempty"`
		Skipped bool   `json:"skipped,omitempty"`
		Error   string `json:"error,omitempty"`
	}

	// resultsCh carries each peer's outcome the instant it's known. It is
	// buffered to len(nodes) so that every worker can hand off its result and
	// exit even if the loop below stops reading early (client gone) — nothing
	// blocks waiting for a reader that may never come back.
	resultsCh := make(chan result, len(nodes))

	var wg sync.WaitGroup
	sem := make(chan struct{}, pushConcurrency)
	for _, node := range nodes {
		wg.Add(1)
		go func(node string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			target, err := s.resolveManagedTarget(node)
			if err != nil {
				msg := err.Error()
				if mte, ok := err.(*managedTargetError); ok {
					msg = mte.msg
				}
				resultsCh <- result{Node: node, OK: false, Error: msg}
				return
			}
			status, skipped, perr := s.pushSourceToWithRetry(node, target, spooled, sum)
			if perr != nil {
				resultsCh <- result{Node: node, OK: false, Status: status, Error: perr.Error()}
				return
			}
			resultsCh <- result{Node: node, OK: true, Status: status, Skipped: skipped}
		}(node)
	}
	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	// Newline-delimited JSON, not one JSON array: an array's closing "]" only
	// exists once every peer is done, which is exactly the wait this exists to
	// remove. X-Accel-Buffering plus an explicit Flush after each line is the
	// same pairing handleSpeedtestSource uses to get bytes to the browser as
	// they're produced instead of batched by an intermediary.
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// Flush the header immediately, before waiting on a single peer.
	//
	// WriteHeader does not put bytes on the wire — net/http buffers until the
	// handler writes a body or returns — so without this the browser receives
	// nothing at all until the first peer finishes building, and fetch() does
	// not resolve until response headers arrive. Pushing to one peer that is
	// already up to date returns in moments and hid this completely; pushing
	// to fourteen that each have to build left the connection silent for
	// minutes and the fetch was torn down before it ever resolved, surfacing
	// in the browser as "NetworkError when attempting to fetch resource" with
	// nothing sent.
	if flusher != nil {
		flusher.Flush()
	}

	// Lift this connection's read deadline for the rest of the rollout.
	//
	// The server sets ReadTimeout: 30s to keep a slow-loris from holding a
	// connection open by trickling a request. That is right for every other
	// endpoint and wrong for this one: the request body is fully read by the
	// time we get here (spoolUpload above consumed it), and what remains is a
	// response that legitimately takes minutes. Leaving the deadline in place
	// tears the connection down mid-rollout, which is the other half of the
	// failure described above. There is nothing left to read, so removing it
	// gives a slow-loris nothing to exploit.
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		// Not fatal, and not worth failing the rollout over: on a transport
		// that does not support it the rollout still runs, and short ones
		// still finish inside the deadline.
		s.log.Debugf("upgrade: could not clear read deadline for the push stream: %v", err)
	}

	// keepalive keeps bytes moving while peers build. A rollout can be silent
	// for minutes between results, which is long enough for an intermediary —
	// or the browser itself — to decide an idle connection is dead. The client
	// ignores these lines; they exist to be traffic, not information.
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	var writeMu sync.Mutex
	go func() {
		t := time.NewTicker(pushKeepaliveInterval)
		defer t.Stop()
		for {
			select {
			case <-keepaliveDone:
				return
			case <-t.C:
				writeMu.Lock()
				if _, err := io.WriteString(w, "{\"keepalive\":true}\n"); err == nil && flusher != nil {
					flusher.Flush()
				}
				writeMu.Unlock()
			}
		}
	}()

	enc := json.NewEncoder(w)

	pushed := 0
	// clientGone stops writing to w once a write fails (the operator's tab
	// closed or the connection dropped) but keeps draining resultsCh rather
	// than returning early: the spooled archive is only removed once this
	// handler returns (see the defer above), and workers are still mid-build
	// against that file until wg.Wait() finishes, closing the channel below.
	// Returning while they're still reading it would remove the file out from
	// under them on platforms that don't tolerate deleting an open file.
	clientGone := false
	for res := range resultsCh {
		if res.OK {
			pushed++
		}
		if clientGone {
			continue
		}
		// Serialised against the keepalive goroutine: two writers on one
		// ResponseWriter can interleave a keepalive into the middle of a
		// result line, and the client parses per line.
		writeMu.Lock()
		err := enc.Encode(res)
		if err == nil && flusher != nil {
			flusher.Flush()
		}
		writeMu.Unlock()
		if err != nil {
			clientGone = true
		}
	}

	s.log.Infof("upgrade: pushed source (sha256 %s) to %d of %d requested peer(s)", sum[:12], pushed, len(nodes))
	if !clientGone {
		writeMu.Lock()
		_ = enc.Encode(map[string]any{"done": true, "sha256": sum, "pushed": pushed, "total": len(nodes)})
		if flusher != nil {
			flusher.Flush()
		}
		writeMu.Unlock()
	}
}

// pushKeepaliveInterval is how often the push stream emits a keepalive line
// while peers are building. Well inside the 30s ReadTimeout this handler lifts
// for itself, and inside the 60s idle timeouts common to reverse proxies, so a
// silent rollout still looks alive to anything in the path.
const pushKeepaliveInterval = 15 * time.Second

// pushTransientRetries is how many extra attempts pushSourceToWithRetry makes
// after a transport-level failure — one that never got as far as a response
// at all (connection refused, reset, EOF mid-request, timeout) — before
// giving up on that peer. 2 retries (3 attempts total): enough to ride out a
// momentary blip (a brief network hiccup, the peer's daemon mid-restart from
// something unrelated) without turning a single flaky moment into "this peer
// failed" and, upstream, potentially stopping an otherwise-healthy rollout
// (see handleUpgradePush's canary/rest split) over nothing wrong with the
// build at all.
const pushTransientRetries = 2

// pushRetryBackoff is how long to wait before retry attempt n (1-indexed):
// short and fixed rather than exponential — this is riding out a moment, not
// backing off from an overloaded server, and every extra second here is a
// second added to how long the whole rollout takes to report back. A var,
// not a plain func, so tests can substitute a near-zero backoff rather than
// actually sleeping through it.
var pushRetryBackoff = func(attempt int) time.Duration {
	return time.Duration(attempt) * 3 * time.Second
}

// pushSourceToWithRetry wraps pushSourceTo with a few retries, but only for a
// genuine transport-level failure — status == 0, meaning the request never
// got as far as a response at all. A peer that *did* respond, just
// unsuccessfully (wrong version, hasn't opted in, a real compile error), is
// never retried: retrying wouldn't help, and re-running a build that
// genuinely failed serves no purpose beyond delaying the result.
func (s *Server) pushSourceToWithRetry(node string, target *clusterPeerTarget, srcPath, sum string) (status int, skipped bool, err error) {
	for attempt := 1; ; attempt++ {
		status, skipped, err = s.pushSourceTo(target, srcPath, sum)
		if err == nil || status != 0 || attempt > pushTransientRetries {
			return status, skipped, err
		}
		s.log.Debugf("upgrade: push to %s: transport-level error (attempt %d/%d), retrying: %v",
			node, attempt, pushTransientRetries+1, err)
		time.Sleep(pushRetryBackoff(attempt))
	}
}

// pushSourceTo streams one spooled source archive (digest first, then bytes) to
// a single peer's remote-apply endpoint over the overlay. Returns the peer's
// HTTP status, whether the peer's own apply op reported skipped (it was
// already running this exact version, so nothing was built or restarted —
// see controlOp's "apply" case), and, on anything other than 200, an error
// carrying the peer's own message so the operator sees why that specific
// node refused (most often: it hasn't opted in, or it has no Go toolchain).
//
// The file is reopened per peer rather than buffered in memory: a source tree
// is only a few MiB, but N concurrent pushes each holding their own copy is a
// cost with no upside when the bytes are already on disk.
func (s *Server) pushSourceTo(target *clusterPeerTarget, srcPath, sum string) (status int, skipped bool, err error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return 0, false, fmt.Errorf("reopening the spooled archive: %w", err)
	}
	defer f.Close()

	// Stream the multipart body through a pipe so the archive is never held in
	// memory in full — the reader side feeds the request as it's written.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() { pw.CloseWithError(werr) }()
		// digest part first: the peer refuses to accept a byte of archive
		// before it holds the digest to check it against (see
		// handleUpgradeRemoteApply).
		part, err := mw.CreateFormField("sha256")
		if err != nil {
			werr = err
			return
		}
		if _, err := part.Write([]byte(sum)); err != nil {
			werr = err
			return
		}
		aw, err := mw.CreateFormFile("source", "gravinet-src.tgz")
		if err != nil {
			werr = err
			return
		}
		if _, err := io.Copy(aw, f); err != nil {
			werr = err
			return
		}
		werr = mw.Close()
	}()

	hostport := net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))
	url := "https://" + hostport + "/api/upgrade/remote-apply"
	req, err := http.NewRequest(http.MethodPost, url, pr)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// A source push plus the peer's full build, selftest and swap can take a
	// while — the build alone is bounded at ten minutes on the peer's side, and
	// on a small box it will use most of that. Give it room well beyond the
	// ordinary proxyClient timeout, but still bounded so one wedged peer can't
	// hold a rollout open forever.
	client := &http.Client{Timeout: 15 * time.Minute, Transport: proxyClient.Transport}
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode != http.StatusOK {
		// Surface the peer's own error text (e.g. "does not accept
		// Manager-pushed upgrades", or a compiler error) rather than a bare
		// status.
		msg := string(bytes.TrimSpace(body))
		var je struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &je) == nil && je.Error != "" {
			msg = je.Error
		}
		return resp.StatusCode, false, fmt.Errorf("%s", msg)
	}
	// A 200 carries the daemon apply op's own reply verbatim (see s.op) —
	// {"ok":true,"skipped":true,"already_on":"..."} when the peer was
	// already running this exact version, {"ok":true,"applied":"...",
	// "restarting":true} otherwise. Unmarshal errors are deliberately
	// ignored: a malformed body on an already-200 response just means
	// skipped stays false, the same as an ordinary successful apply.
	var jr struct {
		Skipped bool `json:"skipped"`
	}
	_ = json.Unmarshal(body, &jr)
	return resp.StatusCode, jr.Skipped, nil
}
