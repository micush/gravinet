package control

import (
	"path/filepath"
	"testing"

	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

type stubEngine struct {
	peers     []mesh.PeerInfo
	bans      []mesh.BanInfo
	fwRules   []mesh.FirewallRule
	ifaces    []mesh.IfaceInfo
	lastBan   string
	lastUnban string
	lastKey   string
}

func (s *stubEngine) NetworkIDs() []uint64               { return []uint64{0xABCD} }
func (s *stubEngine) ListPeers(uint64) []mesh.PeerInfo   { return s.peers }
func (s *stubEngine) NATStatusStrings() (string, string) { return "open", "203.0.113.5:51820" }
func (s *stubEngine) ListBans(uint64) []mesh.BanInfo     { return s.bans }
func (s *stubEngine) Routes(uint64) []mesh.RouteInfo     { return nil }
func (s *stubEngine) Interfaces() []mesh.IfaceInfo       { return s.ifaces }
func (s *stubEngine) LoopDrops() uint64                  { return 0 }
func (s *stubEngine) BanNode(_ uint64, t, _ string) error {
	s.lastBan = t
	return nil
}
func (s *stubEngine) UnbanNode(_ uint64, t string) error {
	s.lastUnban = t
	return nil
}
func (s *stubEngine) ForceUnban(_ uint64, t string) error {
	s.lastUnban = t
	return nil
}
func (s *stubEngine) DistributeKey(_ uint64, keyB64, _ string, _ int64) error {
	s.lastKey = keyB64
	return nil
}

func (s *stubEngine) FirewallRules(uint64) ([]mesh.FirewallRule, error) { return s.fwRules, nil }
func (s *stubEngine) FirewallAdd(_ uint64, r mesh.FirewallRule, _ int) (mesh.FirewallRule, error) {
	r.ID = uint64(len(s.fwRules) + 1)
	s.fwRules = append(s.fwRules, r)
	return r, nil
}
func (s *stubEngine) FirewallDelete(uint64, []uint64) error  { return nil }
func (s *stubEngine) FirewallMove(_, _ uint64, _ int) error  { return nil }
func (s *stubEngine) FirewallCopy(uint64, []uint64) error    { return nil }
func (s *stubEngine) FirewallCut(uint64, []uint64) error     { return nil }
func (s *stubEngine) FirewallPaste(uint64, int) (int, error) { return 0, nil }

func TestControlSocketRoundTrip(t *testing.T) {
	stub := &stubEngine{
		peers: []mesh.PeerInfo{{NodeID: "peer1", Hostname: "h1", Endpoint: "1.2.3.4:51820"}},
		bans:  []mesh.BanInfo{{Target: "bad", Origin: "me", Notes: "x", Mine: true}},
	}
	sock := filepath.Join(t.TempDir(), "ctl.sock")
	srv, err := Serve(sock, stub, logx.Default())
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	defer srv.Close()

	// list returns peers and bans
	resp, err := Do(sock, Request{Cmd: "list"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !resp.OK || len(resp.Peers) != 1 || resp.Peers[0].NodeID != "peer1" || len(resp.Bans) != 1 {
		t.Fatalf("unexpected list response: %+v", resp)
	}

	// ban routes through to the engine
	resp, err = Do(sock, Request{Cmd: "ban", Node: "villain", Notes: "spam"})
	if err != nil || !resp.OK {
		t.Fatalf("ban: err=%v resp=%+v", err, resp)
	}
	if stub.lastBan != "villain" {
		t.Fatalf("ban not delivered to engine: %q", stub.lastBan)
	}

	// unban routes through
	resp, err = Do(sock, Request{Cmd: "unban", Node: "villain"})
	if err != nil || !resp.OK {
		t.Fatalf("unban: err=%v resp=%+v", err, resp)
	}
	if stub.lastUnban != "villain" {
		t.Fatalf("unban not delivered: %q", stub.lastUnban)
	}

	// unknown command errors cleanly
	resp, _ = Do(sock, Request{Cmd: "bogus"})
	if resp.OK {
		t.Fatal("bogus command should not succeed")
	}

	// firewall: add a rule then list it back through the socket
	resp, err = Do(sock, Request{Cmd: "fw", FWOp: "add", FWAt: -1,
		FWRule: mesh.FirewallRule{Action: "deny", Proto: "tcp", DstPortMin: 80, DstPortMax: 80}})
	if err != nil || !resp.OK {
		t.Fatalf("fw add: err=%v resp=%+v", err, resp)
	}
	resp, err = Do(sock, Request{Cmd: "fw", FWOp: "list"})
	if err != nil || !resp.OK {
		t.Fatalf("fw list: err=%v resp=%+v", err, resp)
	}
	if len(resp.FW) != 1 || resp.FW[0].Action != "deny" {
		t.Fatalf("fw list returned %+v", resp.FW)
	}
}

// TestServeCreatesMissingSocketDir is the regression test for the bug behind
// "dial unix /run/gravinet.sock: connect: no such file or directory" on
// macOS/FreeBSD: the platform default (/run/...) lives in a directory that
// doesn't exist on those OSes, so Serve failed at daemon startup — logged as
// a mere warning ("control socket unavailable"), not a fatal error, so the
// daemon kept running with every CLI command silently unable to reach it.
// Serve must create the socket's parent directory itself rather than assume
// it exists, so a platform/deployment whose default runtime directory isn't
// pre-created still gets a working control socket.
func TestServeCreatesMissingSocketDir(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "not-yet-created", "gravinet.sock")
	srv, err := Serve(sock, &stubEngine{}, logx.Default())
	if err != nil {
		t.Fatalf("serve should create the missing parent directory, got: %v", err)
	}
	defer srv.Close()

	if _, err := Do(sock, Request{Cmd: "list"}); err != nil {
		t.Fatalf("dial after serve: %v", err)
	}
}
