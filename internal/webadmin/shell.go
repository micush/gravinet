package webadmin

// Remote shell access: a real OS shell/PTY through the web admin, gated by
// config.WebAdmin.AllowRemoteShell (off by default — see its doc comment for
// why this is a separate flag from Managed/Manager rather than folded into
// either).
//
// Two hops, two different transports, one reason: the browser's real
// WebSocket object is always the outer hop (browser <-> the node the admin
// is actually logged into), carrying raw PTY bytes as binary frames and a
// tiny JSON control protocol (resize/exit/error) as text frames — see ws.go.
// When the target is a *different* managed peer, that node relays to the
// peer over a second, inner hop: a raw hijacked TCP/TLS stream (both ends
// are gravinet's own code, so there's no reason to pay for a second real
// WebSocket handshake) using the length-prefixed frame codec in
// shellframe.go. handleShellWS is the outer hop's handler either way;
// handleShellHijack is the inner hop's, and is also what actually spawns
// the PTY — including for a "local" session, which still goes through it,
// just via an in-process call instead of a network round trip, so there is
// exactly one code path that ever spawns a shell.
//
// Every session — local or proxied — gets a full input+output transcript on
// the node that actually runs the shell (see shellTranscript below). That
// necessarily captures anything typed into that shell, including a password
// typed at a sudo/su/login prompt that the terminal itself declined to echo
// back — full-transcript logging was a deliberate choice (the alternative,
// output-only logging, would actually miss less by relying on the PTY's own
// echo, but wouldn't be a true independent record of input). Treat the
// transcript directory as at least as sensitive as the config file next to
// which it lives.

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gravinet/internal/config"
)

var errShellUnsupported = fmt.Errorf("remote shell is not supported on this platform/architecture yet")

// shellClampSize keeps a client-supplied terminal size within sane bounds —
// defends against a bogus or hostile size value rather than expressing any
// real terminal limit.
func shellClampSize(v, def int) int {
	if v <= 0 {
		return def
	}
	if v > 1000 {
		return 1000
	}
	return v
}

// sessionOnly is a stricter alternative to authed() for endpoints that must
// never accept the overlay/Manager bypass, only a real local browser
// session: the browser-facing shell WebSocket, and the shell setting toggle.
// authed() is intentionally not reused here — see handleShellSetting's doc
// comment for why this endpoint's trust boundary is deliberately tighter
// than Managed/Manager's.
func (s *Server) sessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if _, ok := s.validSession(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not authenticated: log in instead"})
			return
		}
		next(w, r)
	}
}

// handleShellSetting toggles AllowRemoteShell. Unlike handleManaged/
// handleManager (which use authed() and so accept a call arriving over the
// overlay from an authorized Manager peer, exactly like every other config
// endpoint), this uses sessionOnly: a Manager peer can open a shell *through*
// a Managed peer that already has AllowRemoteShell on, but must never be
// able to remotely turn that flag on in the first place. handleProxy also
// hard-blocks this path explicitly (belt and suspenders, matching how
// /api/managed and /api/manager are already double-guarded there), but the
// real boundary is this handler never accepting the overlay bypass at all.
//
// Like AuthMode/Users/GeoIPLookup, AllowRemoteShell is captured once into
// Server.cfg at startup, so this reports restart:true — see config.WebAdmin's
// doc comment on the field.
func (s *Server) handleShellSetting(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"allow_remote_shell": s.cfg.AllowRemoteShell, "supported": ptySupported})
		return
	}
	var req struct{ On bool }
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.WebAdmin.AllowRemoteShell = req.On
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allow_remote_shell": req.On, "restart": true})
}

// shellControl is the JSON text-frame protocol between the browser and the
// node it's logged into (see the package doc comment above). Only one field
// set is meaningful per message: Type "resize" uses Rows/Cols; "exit" uses
// Code; "error" uses Message.
type shellControl struct {
	Type    string `json:"type"`
	Rows    int    `json:"rows,omitempty"`
	Cols    int    `json:"cols,omitempty"`
	Code    int    `json:"code"` // no omitempty: a clean exit (0) is the common case and must not be indistinguishable from a missing/absent code
	Message string `json:"message,omitempty"`
}

func (c *wsConn) sendControl(v shellControl) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.WriteMessage(wsOpText, b)
}

// handleShellWS is the browser-facing WebSocket endpoint. ?node= selects the
// target: empty (or this node's own id) spawns locally; any other value must
// be a currently-advertised managed peer, and the session is relayed there
// (see handleShellHijack). Query params rows/cols set the initial terminal
// size.
func (s *Server) handleShellWS(w http.ResponseWriter, r *http.Request) {
	rows := shellClampSize(atoiOr(r.URL.Query().Get("rows"), 0), 24)
	cols := shellClampSize(atoiOr(r.URL.Query().Get("cols"), 0), 80)
	node := r.URL.Query().Get("node")

	ws, err := upgradeWebSocket(w, r)
	if err != nil {
		s.log.Warnf("webadmin: shell websocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	who, _ := s.validSession(r) // sessionOnly already required this to succeed

	if node == "" || node == s.be.SelfID() {
		s.runLocalShellSession(ws, who, rows, cols)
		return
	}
	s.relayShellSession(ws, who, node, rows, cols)
}

// atoiOr parses s as an int, returning def on any error or if s is empty.
func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// runLocalShellSession spawns a PTY on this node and pumps it directly
// against the browser's WebSocket — no relay, no shellframe encoding needed
// (a WS binary message already is one unit of data; resize/exit/error ride
// the text-frame control channel instead).
func (s *Server) runLocalShellSession(ws *wsConn, who string, rows, cols int) {
	if !s.cfg.AllowRemoteShell {
		ws.sendControl(shellControl{Type: "error", Message: "remote shell is disabled on this node (Settings \u2192 Allow remote shell)"})
		return
	}
	sess, err := spawnPTY("", rows, cols)
	if err != nil {
		ws.sendControl(shellControl{Type: "error", Message: err.Error()})
		return
	}
	tr := s.newShellTranscript(who, "this node (local)")
	s.pumpShellSession(ws, sess, tr)
}

// relayShellSession resolves node to a managed peer, opens the inner
// hijacked stream to its /api/shell/hijack, and pumps bytes between the
// browser's WebSocket and that stream (re-encoding each direction between
// WS messages and shellframe frames). The actual PTY runs on the peer, not
// here — this node never sees anything but the byte stream.
func (s *Server) relayShellSession(ws *wsConn, who, node string, rows, cols int) {
	target, err := s.resolveManagedTarget(node)
	if err != nil {
		ws.sendControl(shellControl{Type: "error", Message: err.Error()})
		return
	}
	hostport := net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))
	conn, err := tls.Dial("tcp", hostport, &tls.Config{InsecureSkipVerify: true}) // overlay-internal, self-signed — see proxyClient's doc comment
	if err != nil {
		ws.sendControl(shellControl{Type: "error", Message: fmt.Sprintf("reaching %s: %v", node, err)})
		return
	}
	defer conn.Close()

	req := fmt.Sprintf("POST /api/shell/hijack?rows=%d&cols=%d HTTP/1.1\r\nHost: %s\r\nX-Gravinet-Admin: %s\r\n\r\n",
		rows, cols, hostport, who)
	if _, err := conn.Write([]byte(req)); err != nil {
		ws.sendControl(shellControl{Type: "error", Message: fmt.Sprintf("reaching %s: %v", node, err)})
		return
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		ws.sendControl(shellControl{Type: "error", Message: fmt.Sprintf("reaching %s: %v", node, err)})
		return
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		ws.sendControl(shellControl{Type: "error", Message: fmt.Sprintf("%s refused: %s", node, strings.TrimSpace(string(msg)))})
		return
	}
	// resp.Body is not used from here on — the connection is a raw stream
	// from this point (200 OK with no declared length is our own hijack
	// protocol, not a normal HTTP response body). Any bytes ReadResponse
	// already buffered past the header terminator are real frame data, so
	// br (not a fresh reader on conn) must keep being used for every
	// subsequent read.

	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	go func() { // browser -> peer
		defer closeDone()
		for {
			op, payload, err := ws.ReadMessage()
			if err != nil {
				return
			}
			switch op {
			case wsOpBinary:
				if err := writeShellFrame(conn, shellFrameData, payload); err != nil {
					return
				}
			case wsOpText:
				var ctl shellControl
				if json.Unmarshal(payload, &ctl) == nil && ctl.Type == "resize" {
					if err := writeShellFrame(conn, shellFrameResize, encodeResize(ctl.Rows, ctl.Cols)); err != nil {
						return
					}
				}
			}
		}
	}()

	go func() { // peer -> browser
		defer closeDone()
		for {
			typ, payload, err := readShellFrame(br)
			if err != nil {
				return
			}
			switch typ {
			case shellFrameData:
				if err := ws.WriteMessage(wsOpBinary, payload); err != nil {
					return
				}
			case shellFrameExit:
				code, _ := decodeExit(payload)
				ws.sendControl(shellControl{Type: "exit", Code: code})
				return
			}
		}
	}()

	<-done
}

// handleShellHijack is the inner hop's endpoint: it actually spawns the PTY.
// Reached either by another node relaying a session here (over the overlay,
// authorized by authed()'s existing Manager-peer bypass — the same door
// every other proxied call already uses) or, in principle, by a direct local
// session; either way AllowRemoteShell still gates whether a shell actually
// starts. rows/cols query params set the initial size.
func (s *Server) handleShellHijack(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AllowRemoteShell {
		http.Error(w, "remote shell is disabled on this node", http.StatusForbidden)
		return
	}
	if !ptySupported {
		http.Error(w, errShellUnsupported.Error(), http.StatusNotImplemented)
		return
	}
	rows := shellClampSize(atoiOr(r.URL.Query().Get("rows"), 0), 24)
	cols := shellClampSize(atoiOr(r.URL.Query().Get("cols"), 0), 80)
	sess, err := spawnPTY("", rows, cols)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		sess.close()
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		sess.close()
		s.log.Warnf("webadmin: shell hijack failed: %v", err)
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 200 OK\r\n\r\n"); err != nil || rw.Flush() != nil {
		sess.close()
		conn.Close()
		return
	}

	who := r.Header.Get("X-Gravinet-Admin") // informational only, never used for auth
	origin := "proxied from " + remoteIP(r).String()
	if who != "" {
		origin += " (admin: " + who + ")"
	}
	if u, ok := s.validSession(r); ok { // reached directly by a local session, not proxied
		origin = "local session (admin: " + u + ")"
	}
	tr := s.newShellTranscript(origin, "this node (via proxy)")

	s.pumpShellHijack(conn, rw, sess, tr)
}

// pumpShellSession wires a local PTY session directly to the browser's
// WebSocket: binary frames carry raw bytes each direction, and a resize
// control message adjusts the PTY's window size live. writeMu serializes the
// two goroutines that can each write to ws (this one, for the final exit
// control message; the pty-reader goroutine below, for output) — wsConn
// itself only guarantees a single reader concurrent with a single writer is
// safe, not two concurrent writers.
//
// Both read loops (pty output, browser input) run in their own goroutines;
// this function's own body just blocks on exitCh. An earlier version instead
// ran the browser-input loop directly in this function, checking exitCh
// non-blockingly right before each (blocking) ws.ReadMessage() call — which
// meant that once that read call was blocked waiting for the browser's next
// message, the shell exiting couldn't be noticed until the browser happened
// to send something else. Since the browser has no reason to send anything
// more once the *shell* has exited (it's waiting on *us* to say so), this
// deadlocked every clean "type exit, session ends" case rather than being a
// rare race — found by an end-to-end browser test, not by inspection.
func (s *Server) pumpShellSession(ws *wsConn, sess *ptySession, tr *shellTranscript) {
	var writeMu sync.Mutex
	writeBinary := func(p []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.WriteMessage(wsOpBinary, p)
	}
	writeControl := func(v shellControl) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return ws.sendControl(v)
	}

	exitCh := make(chan int, 1)
	go func() { exitCh <- sess.wait() }()

	go func() { // pty -> browser
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.ptmx.Read(buf)
			if n > 0 {
				tr.out(buf[:n])
				if werr := writeBinary(buf[:n]); werr != nil {
					sess.close()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() { // browser -> pty (and control messages), until the ws drops
		for {
			op, payload, err := ws.ReadMessage()
			if err != nil {
				sess.close()
				return
			}
			switch op {
			case wsOpBinary:
				tr.in(payload)
				if _, err := sess.ptmx.Write(payload); err != nil {
					sess.close()
					return
				}
			case wsOpText:
				var ctl shellControl
				if json.Unmarshal(payload, &ctl) == nil && ctl.Type == "resize" {
					resizePTY(sess, ctl.Rows, ctl.Cols)
				}
			}
		}
	}()

	code := <-exitCh
	writeControl(shellControl{Type: "exit", Code: code})
	tr.close(code)
}

// pumpShellHijack is pumpShellSession's counterpart for the inner (node-to-
// node) hop: same PTY-spawning code path, but the peer side speaks the
// length-prefixed shellframe protocol (see shellframe.go) over the raw
// hijacked connection instead of WebSocket frames. writeMu serializes writes
// to rw between the pty-reader goroutine and the final exit-frame write —
// same reasoning as pumpShellSession's writeMu.
func (s *Server) pumpShellHijack(conn net.Conn, rw *bufio.ReadWriter, sess *ptySession, tr *shellTranscript) {
	defer conn.Close()
	var writeMu sync.Mutex
	writeFrame := func(typ byte, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if err := writeShellFrame(rw, typ, payload); err != nil {
			return err
		}
		return rw.Flush()
	}

	exitCh := make(chan int, 1)
	go func() { exitCh <- sess.wait() }()

	go func() { // pty -> raw conn
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.ptmx.Read(buf)
			if n > 0 {
				tr.out(buf[:n])
				if werr := writeFrame(shellFrameData, buf[:n]); werr != nil {
					sess.close()
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	go func() { // raw conn -> pty
		for {
			typ, payload, err := readShellFrame(rw)
			if err != nil {
				sess.close()
				return
			}
			switch typ {
			case shellFrameData:
				tr.in(payload)
				if _, err := sess.ptmx.Write(payload); err != nil {
					sess.close()
					return
				}
			case shellFrameResize:
				if rows, cols, ok := decodeResize(payload); ok {
					resizePTY(sess, rows, cols)
				}
			}
		}
	}()

	code := <-exitCh
	writeFrame(shellFrameExit, encodeExit(code))
	tr.close(code)
}

// --- audit transcript -------------------------------------------------------

// shellTranscript is a full input+output record of one shell session, one
// file per session, written next to the config file (matching where
// webadmin-session.key already lives — see sign()'s doc comment) under a
// shell-sessions/ subdirectory. See the package doc comment above for why
// this necessarily captures unecho'd input too (e.g. a sudo password) and
// should be treated accordingly.
type shellTranscript struct {
	mu sync.Mutex
	f  *os.File // nil if the transcript couldn't be opened — logging then silently no-ops rather than failing the session
}

func shellSessionID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newShellTranscript opens (creating if needed) the transcript directory and
// this session's file, and writes its header. who/target are free-text for
// the header only (who: the initiating admin and/or proxying origin; target:
// which node's shell this is). Any failure to open the file is logged once
// and the session proceeds without a transcript rather than being refused —
// an audit-log problem shouldn't itself be a way to deny legitimate access.
func (s *Server) newShellTranscript(who, target string) *shellTranscript {
	id := shellSessionID()
	dir := filepath.Join(filepath.Dir(s.configPath), "shell-sessions")
	if s.configPath == "" {
		dir = filepath.Join(os.TempDir(), "gravinet-shell-sessions")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		s.log.Warnf("webadmin: shell session %s: could not create transcript dir %s: %v (proceeding without a transcript)", id, dir, err)
		return &shellTranscript{}
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.log", time.Now().UTC().Format("20060102T150405Z"), id))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		s.log.Warnf("webadmin: shell session %s: could not open transcript %s: %v (proceeding without a transcript)", id, path, err)
		return &shellTranscript{}
	}
	fmt.Fprintf(f, "=== gravinet shell session %s ===\nstarted: %s\nwho:     %s\ntarget:  %s\n\n",
		id, time.Now().UTC().Format(time.RFC3339), who, target)
	s.log.Infof("webadmin: shell session %s started (%s -> %s), transcript: %s", id, who, target, path)
	return &shellTranscript{f: f}
}

func (t *shellTranscript) log(dir string, p []byte) {
	if len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return
	}
	fmt.Fprintf(t.f, "[%s] %s %q\n", time.Now().UTC().Format("15:04:05.000"), dir, p)
}

func (t *shellTranscript) in(p []byte)  { t.log("IN ", p) }
func (t *shellTranscript) out(p []byte) { t.log("OUT", p) }

func (t *shellTranscript) close(exitCode int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return
	}
	fmt.Fprintf(t.f, "\n=== session ended, exit=%d, at %s ===\n", exitCode, time.Now().UTC().Format(time.RFC3339))
	t.f.Close()
	t.f = nil
}
