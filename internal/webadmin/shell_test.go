package webadmin

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// shellTestServer builds a test server with the shell feature configured
// (allow controls AllowRemoteShell) and a real, saved config file so
// mutateConfig-based edits (the setting toggle) work, matching the pattern
// TestHandleManagedAppliesLiveNoRestart already uses. Returns the server, the
// stub backend, the httptest server, and the config directory (so a test can
// inspect the shell-sessions/ transcript dir under it).
func shellTestServer(t *testing.T, allow bool) (*Server, *stubBackend, *httptest.Server, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg.json")
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{
		AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan:         config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900},
		AllowRemoteShell: allow,
	}
	cfg := &config.Config{PrimaryPort: 65432, EnableIPv4: true, WebAdmin: wcfg}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	be := &stubBackend{}
	srv := New(wcfg, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(ts.Close)
	return srv, be, ts, dir
}

func TestShellFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeShellFrame(&buf, shellFrameData, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := writeShellFrame(&buf, shellFrameResize, encodeResize(40, 120)); err != nil {
		t.Fatal(err)
	}
	if err := writeShellFrame(&buf, shellFrameExit, encodeExit(7)); err != nil {
		t.Fatal(err)
	}

	typ, payload, err := readShellFrame(&buf)
	if err != nil || typ != shellFrameData || string(payload) != "hello" {
		t.Fatalf("data frame: typ=%d payload=%q err=%v", typ, payload, err)
	}
	typ, payload, err = readShellFrame(&buf)
	if err != nil || typ != shellFrameResize {
		t.Fatalf("resize frame: typ=%d err=%v", typ, err)
	}
	if rows, cols, ok := decodeResize(payload); !ok || rows != 40 || cols != 120 {
		t.Fatalf("resize decode = %d,%d,%v", rows, cols, ok)
	}
	typ, payload, err = readShellFrame(&buf)
	if err != nil || typ != shellFrameExit {
		t.Fatalf("exit frame: typ=%d err=%v", typ, err)
	}
	if code, ok := decodeExit(payload); !ok || code != 7 {
		t.Fatalf("exit decode = %d,%v", code, ok)
	}
	if _, _, err := readShellFrame(&buf); err != io.EOF {
		t.Fatalf("expected EOF after all frames consumed, got %v", err)
	}
}

// clientMaskedFrame builds a single client->server WebSocket frame (masked,
// as RFC 6455 requires — see readFrame's doc comment on why an unmasked one
// is rejected). Payload must be <=125 bytes; that's every message this test
// file sends.
func clientMaskedFrame(t *testing.T, opcode int, payload []byte) []byte {
	t.Helper()
	if len(payload) > 125 {
		t.Fatal("clientMaskedFrame: payload too long for this helper")
	}
	var buf bytes.Buffer
	buf.WriteByte(0x80 | byte(opcode))
	buf.WriteByte(0x80 | byte(len(payload)))
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	buf.Write(mask[:])
	for i, b := range payload {
		buf.WriteByte(b ^ mask[i%4])
	}
	return buf.Bytes()
}

// readServerFrame reads one unmasked server->client WebSocket frame (small
// helper mirroring wsConn.readFrame's shape, for the client side of a test).
func readServerFrame(t *testing.T, br *bufio.Reader) (opcode int, payload []byte) {
	t.Helper()
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	opcode = int(head[0] & 0x0f)
	if head[1]&0x80 != 0 {
		t.Fatal("server frame must not be masked")
	}
	length := uint64(head[1] & 0x7f)
	switch length {
	case 126:
		ext := make([]byte, 2)
		io.ReadFull(br, ext)
		length = uint64(binary.BigEndian.Uint16(ext))
	case 127:
		ext := make([]byte, 8)
		io.ReadFull(br, ext)
		length = binary.BigEndian.Uint64(ext)
	}
	payload = make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("read frame payload: %v", err)
	}
	return opcode, payload
}

// TestWebSocketHandshakeAndEcho checks the opening handshake against RFC
// 6455 §1.3's own worked example (an external, known-correct test vector,
// not just self-consistency with our own Sec-WebSocket-Accept computation),
// then proves a masked client frame round-trips through ReadMessage/
// WriteMessage correctly and comes back unmasked.
func TestWebSocketHandshakeAndEcho(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgradeWebSocket(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer ws.Close()
		op, payload, err := ws.ReadMessage()
		if err != nil {
			t.Errorf("server read: %v", err)
			return
		}
		if err := ws.WriteMessage(op, payload); err != nil {
			t.Errorf("server write: %v", err)
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	const key = "dGhlIHNhbXBsZSBub25jZQ==" // RFC 6455 §1.3's own example key
	req := "GET /ws HTTP/1.1\r\nHost: " + host + "\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\nSec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}
	const wantAccept = "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" // RFC 6455 §1.3's own worked example
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != wantAccept {
		t.Fatalf("Sec-WebSocket-Accept = %q, want %q (the RFC's own worked example)", got, wantAccept)
	}

	payload := []byte("hello pty")
	if _, err := conn.Write(clientMaskedFrame(t, wsOpBinary, payload)); err != nil {
		t.Fatal(err)
	}
	op, got := readServerFrame(t, br)
	if op != wsOpBinary {
		t.Fatalf("echoed opcode = %d, want binary", op)
	}
	if string(got) != string(payload) {
		t.Fatalf("echoed payload = %q, want %q", got, payload)
	}
}

// TestShellSettingRejectsOverlayBypass proves /api/shell/setting does NOT
// accept the overlay/Manager bypass that /api/managed and /api/manager do —
// the whole point of using sessionOnly instead of authed() for it (see
// handleShellSetting's doc comment). Mirrors
// TestAuthedOverlayBypassNeedsCallerIsManager's setup exactly, so the only
// variable is which endpoint is called.
func TestShellSettingRejectsOverlayBypass(t *testing.T) {
	be := &stubBackend{
		managed:     true,
		overlayAddr: netip.MustParseAddr("10.42.0.5"),
		managerAddr: netip.MustParseAddr("10.42.0.5"), // caller IS a genuine manager peer
	}
	h := secServer(be).handler()
	req := httptest.NewRequest("GET", "/api/shell/setting", nil)
	req.RemoteAddr = "10.42.0.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a Manager-peer overlay source must NOT reach the shell setting endpoint (it must always need a real session); got %d", rec.Code)
	}
}

// TestProxyRejectsShellSetting proves handleProxy's own explicit block on
// this path (belt-and-suspenders alongside sessionOnly above).
func TestProxyRejectsShellSetting(t *testing.T) {
	be := &stubBackend{
		managed: true, overlayAddr: netip.MustParseAddr("127.0.0.1"),
	}
	ts := httptest.NewServer(secServer(be).handler())
	defer ts.Close()
	c := sessionFor(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/api/proxy?node=x&path="+url.QueryEscape("/api/shell/setting"), nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", resp.StatusCode)
	}
}

func TestShellSettingSessionAndToggle(t *testing.T) {
	srv, _, ts, _ := shellTestServer(t, false)
	c := sessionFor(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/api/shell/setting", nil)
	req.AddCookie(c)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["allow_remote_shell"] != false {
		t.Fatalf("expected allow_remote_shell=false initially, got %v", out)
	}

	body, _ := json.Marshal(map[string]any{"On": true})
	req, _ = http.NewRequest("POST", ts.URL+"/api/shell/setting", bytes.NewReader(body))
	req.AddCookie(c)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	out = map[string]any{}
	json.NewDecoder(resp.Body).Decode(&out)
	if out["restart"] != true {
		t.Fatalf("toggling this flag must report restart:true (like GeoIPLookup — captured once at startup); got %v", out)
	}

	// And it actually persisted.
	saved, err := config.Load(srv.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.WebAdmin.AllowRemoteShell {
		t.Fatal("AllowRemoteShell was not persisted to disk")
	}
}

// TestShellHijackRequiresAllowRemoteShell proves the inner endpoint refuses
// to spawn anything when the flag is off, even for an otherwise-authorized
// caller (a genuine overlay-sourced Manager peer) — the actual point of
// having a separate flag at all.
func TestShellHijackRequiresAllowRemoteShell(t *testing.T) {
	be := &stubBackend{
		managed:     true,
		overlayAddr: netip.MustParseAddr("10.42.0.5"),
		managerAddr: netip.MustParseAddr("10.42.0.5"),
	}
	h := secServer(be).handler() // AllowRemoteShell defaults false
	req := httptest.NewRequest("POST", "/api/shell/hijack", nil)
	req.RemoteAddr = "10.42.0.5:1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when AllowRemoteShell is off, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestShellHijackAndLocalWSSession is the end-to-end test: with
// AllowRemoteShell on, it (a) opens the browser-facing WebSocket directly
// (as runLocalShellSession would serve it) and drives a real shell through
// it, and (b) separately drives the inner hijack endpoint directly with a
// raw client exactly like relayShellSession's would, proving both transports
// reach the same underlying PTY machinery correctly. Also checks a
// transcript file was written and contains both directions.
func TestShellHijackAndLocalWSSession(t *testing.T) {
	if !ptySupported {
		t.Skip("PTY not supported on this platform/architecture")
	}
	_, _, ts, dir := shellTestServer(t, true)
	c := sessionFor(t, ts)

	host := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := "GET /api/shell/ws?rows=10&cols=40 HTTP/1.1\r\nHost: " + host + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\nCookie: " + c.Name + "=" + c.Value + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	marker := "GRAVINET_SHELL_TEST_OK"
	cmd := "echo " + marker + "\r"
	if _, err := conn.Write(clientMaskedFrame(t, wsOpBinary, []byte(cmd))); err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	sentExit := false
	exitReceived := false
	var exitCode int
	deadline := time.Now().Add(15 * time.Second)
	for !exitReceived {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for the exit message; output so far: %q", output.String())
		}
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		op, payload := readServerFrame(t, br)
		switch op {
		case wsOpBinary:
			output.Write(payload)
		case wsOpText:
			var ctl shellControl
			if json.Unmarshal(payload, &ctl) == nil {
				if ctl.Type == "error" {
					t.Fatalf("server reported error: %s", ctl.Message)
				}
				if ctl.Type == "exit" {
					exitCode = ctl.Code
					exitReceived = true
				}
			}
		}
		if strings.Contains(output.String(), marker) && !sentExit {
			// Ask the shell to exit now that we've seen the echo, so the
			// session ends promptly instead of waiting for the read deadline.
			conn.Write(clientMaskedFrame(t, wsOpBinary, []byte("exit\r")))
			sentExit = true
		}
	}
	if !strings.Contains(output.String(), marker) {
		t.Fatalf("shell output never contained marker: %q", output.String())
	}
	if exitCode != 0 {
		t.Fatalf("expected a clean exit (0), got %d", exitCode)
	}

	// A transcript file should exist and contain both the input we sent and
	// the output we saw.
	entries, err := os.ReadDir(filepath.Join(dir, "shell-sessions"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected a transcript file in shell-sessions/: %v", err)
	}
	transcript, err := os.ReadFile(filepath.Join(dir, "shell-sessions", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), marker) {
		t.Fatalf("transcript missing expected content: %s", transcript)
	}
	if !strings.Contains(string(transcript), "IN ") || !strings.Contains(string(transcript), "OUT") {
		t.Fatalf("transcript missing IN/OUT direction tags: %s", transcript)
	}
}

// TestShellRelayBetweenTwoNodes exercises relayShellSession specifically:
// node A (Manager) relays a browser's shell session to node B (Managed,
// AllowRemoteShell on), which is the actual multi-node path the header
// dropdown's "open a shell on this peer" uses — as opposed to
// TestShellHijackDirectRawProtocol above, which drives B's hijack endpoint
// directly without A in between at all.
func TestShellRelayBetweenTwoNodes(t *testing.T) {
	if !ptySupported {
		t.Skip("PTY not supported on this platform/architecture")
	}
	// B is the target: AllowRemoteShell on, and it must be a TLS server —
	// relayShellSession always dials out with tls.Dial (self-signed/
	// InsecureSkipVerify, matching proxyClient's own trust model), so a
	// plain httptest.Server on the far end would fail the dial outright.
	_, beB, tsB := newTLSShellServer(t, true)
	beB.managed = true // B must itself report Managed for authed()'s bypass path, mirroring real nodes

	bHost := strings.TrimPrefix(tsB.URL, "https://")
	_, bPortStr, err := net.SplitHostPort(bHost)
	if err != nil {
		t.Fatal(err)
	}
	var bPort int
	if _, err := fmt.Sscan(bPortStr, &bPort); err != nil {
		t.Fatal(err)
	}

	// A is what the browser talks to: Manager mode, with B listed as a
	// currently-advertised managed peer at 127.0.0.1:bPort, and A's own
	// overlay containing 127.0.0.1 (both stub's OverlayContains and B's own
	// authed() overlay check use plain equality against this one address —
	// see stubBackend's OverlayContains/IsManagerAddr).
	_, beA, tsA, _ := shellTestServer(t, false) // A itself doesn't need AllowRemoteShell — it only relays
	beA.manager = true
	beA.overlayAddr = netip.MustParseAddr("127.0.0.1")
	beA.managedPeers = []mesh.ManagedPeer{{
		NodeID: "nodeB", Hostname: "b", Overlay4: netip.MustParseAddr("127.0.0.1"),
		WebPort: uint16(bPort), LastSeen: time.Now(), Connected: true, Manager: false,
	}}
	// B must recognize A as a genuine Manager peer arriving over "the
	// overlay" (both loopback here, standing in for a real overlay hop).
	beB.overlayAddr = netip.MustParseAddr("127.0.0.1")
	beB.managerAddr = netip.MustParseAddr("127.0.0.1")

	c := sessionFor(t, tsA)
	aHost := strings.TrimPrefix(tsA.URL, "http://")
	conn, err := net.Dial("tcp", aHost)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "GET /api/shell/ws?node=nodeB&rows=10&cols=40 HTTP/1.1\r\nHost: " + aHost + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\nCookie: " + c.Name + "=" + c.Value + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("expected 101, got %d", resp.StatusCode)
	}

	marker := "GRAVINET_RELAY_TEST_OK"
	if _, err := conn.Write(clientMaskedFrame(t, wsOpBinary, []byte("echo "+marker+"\r"))); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	sentExit := false
	exitReceived := false
	var exitCode int
	deadline := time.Now().Add(20 * time.Second)
	for !exitReceived {
		if time.Now().After(deadline) {
			t.Fatalf("timed out relaying to B; output so far: %q", output.String())
		}
		conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		op, payload := readServerFrame(t, br)
		if op == wsOpText {
			var ctl shellControl
			if json.Unmarshal(payload, &ctl) == nil {
				if ctl.Type == "error" {
					t.Fatalf("relay reported error: %s", ctl.Message)
				}
				if ctl.Type == "exit" {
					exitCode = ctl.Code
					exitReceived = true
				}
			}
		} else if op == wsOpBinary {
			output.Write(payload)
		}
		if strings.Contains(output.String(), marker) && !sentExit {
			conn.Write(clientMaskedFrame(t, wsOpBinary, []byte("exit\r")))
			sentExit = true
		}
	}
	if !strings.Contains(output.String(), marker) {
		t.Fatalf("shell output (relayed from B) never contained marker: %q", output.String())
	}
	if exitCode != 0 {
		t.Fatalf("expected a clean exit (0) relayed back from B, got %d", exitCode)
	}
}

// newTLSShellServer is shellTestServer's TLS-listening counterpart, needed
// for anything acting as the *target* of a relay (relayShellSession always
// dials out over TLS — see proxyClient's own doc comment on why).
func newTLSShellServer(t *testing.T, allow bool) (*Server, *stubBackend, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cfg.json")
	cred, _ := GenerateCredential("admin", "pw", 10000)
	wcfg := config.WebAdmin{
		AuthMode: "local", Users: []config.AdminUser{cred},
		LoginBan:         config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900},
		AllowRemoteShell: allow,
	}
	cfg := &config.Config{PrimaryPort: 65432, EnableIPv4: true, WebAdmin: wcfg}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveTo(cfgPath); err != nil {
		t.Fatal(err)
	}
	be := &stubBackend{}
	srv := New(wcfg, be, logx.Default())
	srv.SetConfigPath(cfgPath)
	srv.SetReload(func() error { return nil })
	ts := httptest.NewTLSServer(srv.handler())
	t.Cleanup(ts.Close)
	return srv, be, ts
}

// raw client speaking the shellframe protocol (bypassing the WS hop
// entirely) — exactly what relayShellSession does when proxying to a peer,
// just without a second node in between. Authorizes via the overlay/Manager
// bypass, the real path a proxied call uses.
func TestShellHijackDirectRawProtocol(t *testing.T) {
	if !ptySupported {
		t.Skip("PTY not supported on this platform/architecture")
	}
	_, be, ts, _ := shellTestServer(t, true)
	host := strings.TrimPrefix(ts.URL, "http://")
	// Authorize as a genuine overlay-sourced Manager peer, the same as
	// relayShellSession's real caller would be authorized on the far end.
	be.managed = true
	be.overlayAddr = netip.MustParseAddr("127.0.0.1")
	be.managerAddr = netip.MustParseAddr("127.0.0.1")

	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := "POST /api/shell/hijack?rows=10&cols=40 HTTP/1.1\r\nHost: " + host + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	marker := "GRAVINET_HIJACK_TEST_OK"
	if err := writeShellFrame(conn, shellFrameData, []byte("echo "+marker+"\r")); err != nil {
		t.Fatal(err)
	}

	var output strings.Builder
	deadline := time.Now().Add(15 * time.Second)
	sentExit := false
	for !strings.Contains(output.String(), marker) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out; got so far: %q", output.String())
		}
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))
		typ, payload, err := readShellFrame(br)
		if err != nil {
			t.Fatalf("readShellFrame: %v", err)
		}
		if typ == shellFrameData {
			output.Write(payload)
		}
		if strings.Contains(output.String(), marker) && !sentExit {
			writeShellFrame(conn, shellFrameData, []byte("exit\r"))
			sentExit = true
		}
	}

	// Drain until the exit frame so the session (and its PTY/process) winds
	// down cleanly rather than leaking past the test.
	for {
		typ, payload, err := readShellFrame(br)
		if err != nil {
			t.Fatalf("readShellFrame (draining to exit): %v", err)
		}
		if typ == shellFrameExit {
			if code, ok := decodeExit(payload); !ok || code != 0 {
				t.Fatalf("exit code = %d, ok=%v, want 0", code, ok)
			}
			break
		}
	}
}
