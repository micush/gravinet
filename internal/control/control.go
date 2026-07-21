// Package control exposes a small local IPC surface so the CLI (and later the
// web admin) can drive a running daemon: list peers/bans/routes, ban, and
// unban. It speaks one line of JSON per request over a Unix socket (or a
// host:port TCP address).
package control

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/logx"
	"gravinet/internal/mesh"
)

// Engine is the subset of *mesh.Engine the control surface needs.
type Engine interface {
	NetworkIDs() []uint64
	NATStatusStrings() (string, string)
	ListPeers(networkID uint64) []mesh.PeerInfo
	ListBans(networkID uint64) []mesh.BanInfo
	Routes(networkID uint64) []mesh.RouteInfo
	BanNode(networkID uint64, target, notes string) error
	UnbanNode(networkID uint64, target string) error
	ForceUnban(networkID uint64, target string) error
	DistributeKey(networkID uint64, keyB64, label string, expiresNano int64) error
	// Interfaces reports the live network -> kernel-interface mapping (e.g.
	// "mesh0"), needed for "gravinet monitor dns-state": which OS interface a
	// network is actually using isn't derivable from config alone (assignment
	// happens at runtime), so this is the one piece of live daemon state that
	// command needs, fetched over the control socket rather than guessed at.
	Interfaces() []mesh.IfaceInfo
	LoopDrops() uint64

	FirewallRules(networkID uint64) ([]mesh.FirewallRule, error)
	FirewallAdd(networkID uint64, r mesh.FirewallRule, at int) (mesh.FirewallRule, error)
	FirewallDelete(networkID uint64, ids []uint64) error
	FirewallMove(networkID, id uint64, to int) error
	FirewallCopy(networkID uint64, ids []uint64) error
	FirewallCut(networkID uint64, ids []uint64) error
	FirewallPaste(networkID uint64, at int) (int, error)
}

// Request is a single command.
type Request struct {
	Cmd   string `json:"cmd"`
	Net   string `json:"net,omitempty"` // hex network id; optional if only one
	Node  string `json:"node,omitempty"`
	Notes string `json:"notes,omitempty"`
	Force bool   `json:"force,omitempty"`

	// Key distribution ("keydist" command).
	Key     string `json:"key,omitempty"`     // base64 AES-256 key to propagate
	Label   string `json:"label,omitempty"`   // optional slot label
	Expires string `json:"expires,omitempty"` // RFC3339 expiry; "" = never

	// Firewall ("fw" command).
	FWOp   string            `json:"fw_op,omitempty"` // list|add|del|move|copy|cut|paste
	FWAt   int               `json:"fw_at,omitempty"` // insert/paste position (-1 = end)
	FWTo   int               `json:"fw_to,omitempty"` // move target index
	FWIDs  []uint64          `json:"fw_ids,omitempty"`
	FWRule mesh.FirewallRule `json:"fw_rule,omitempty"`

	// Upgrade ("upgrade" command). Op selects the operation and Body carries its
	// operation-specific payload, opaque to this package: the control surface is
	// a transport here, not a participant. Keeping the upgrade schema out of the
	// control protocol means internal/control does not have to import
	// internal/upgrade (and grow a dependency on the artifact format) just to
	// pass a rollout plan from the CLI to the daemon that already understands it.
	UpOp   string          `json:"up_op,omitempty"`
	UpBody json.RawMessage `json:"up_body,omitempty"`
}

// Response is the reply.
type Response struct {
	OK       bool                `json:"ok"`
	Error    string              `json:"error,omitempty"`
	Peers    []mesh.PeerInfo     `json:"peers,omitempty"`
	Bans     []mesh.BanInfo      `json:"bans,omitempty"`
	Routes   []mesh.RouteInfo    `json:"routes,omitempty"`
	Nets     []string            `json:"nets,omitempty"`
	Ifaces   []mesh.IfaceInfo    `json:"ifaces,omitempty"`
	FW       []mesh.FirewallRule `json:"fw,omitempty"`
	Count    int                 `json:"count,omitempty"`
	NATClass string              `json:"nat_class,omitempty"`
	Public   string              `json:"public,omitempty"`
	LoopDrops uint64             `json:"loop_drops,omitempty"`
	UpBody   json.RawMessage     `json:"up_body,omitempty"`
}

// network/addr split: a path containing ':' and no '/' is treated as TCP.
func netAndAddr(path string) (string, string) {
	if strings.Contains(path, ":") && !strings.Contains(path, "/") {
		return "tcp", path
	}
	return "unix", path
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// Server accepts and serves control connections.
type Server struct {
	ln       net.Listener
	api      Engine
	log      *logx.Logger
	reload   func() error                    // optional; set by the daemon to re-read its config file
	resolver func(ref string) (uint64, bool) // optional; resolves a network name -> id
	upgrade  func(op string, body []byte) ([]byte, error)
}

// SetUpgrade installs the handler for the "upgrade" command. Optional: a daemon
// with no trusted release keys never sets it, and the command reports that
// rather than silently doing nothing.
func (s *Server) SetUpgrade(fn func(op string, body []byte) ([]byte, error)) { s.upgrade = fn }

// SetReload installs the handler for the "reload" command.
func (s *Server) SetReload(fn func() error) { s.reload = fn }

// SetNameResolver installs a network-name resolver so a -net value can be a
// network name (not just a hex id), matching the config CLI commands.
func (s *Server) SetNameResolver(fn func(ref string) (uint64, bool)) { s.resolver = fn }

// Serve starts a control server listening at path.
func Serve(path string, api Engine, log *logx.Logger) (*Server, error) {
	network, addr := netAndAddr(path)
	if network == "unix" {
		if dir := filepath.Dir(addr); dir != "." && dir != "/" {
			// Create the socket's own directory (e.g. /var/run/gravinet/) if it's
			// missing — but only underneath a parent that already exists. The old
			// unconditional MkdirAll would happily manufacture a *top-level*
			// directory the OS doesn't have: given a stale "/run/gravinet.sock" it
			// created "/run" on FreeBSD (writable /), inventing a non-standard
			// directory and entrenching the bad path, while the same call failed on
			// macOS (read-only APFS system volume) and merely produced a confusing
			// "no such file or directory" later, from the CLI. Refusing to invent a
			// root-level directory makes both platforms fail the same, honest way
			// here — where the error names the daemon and the path — instead of one
			// of them quietly "working".
			if parent := filepath.Dir(dir); parent == dir || dirExists(parent) {
				_ = os.MkdirAll(dir, 0755) // best-effort; Listen reports the real error
			}
		}
		_ = os.Remove(addr) // clear a stale socket
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		_ = os.Chmod(addr, 0600) // local admin only
	}
	s := &Server{ln: ln, api: api, log: log}
	go s.acceptLoop()
	return s, nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handle(conn)
	}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() {
		return
	}
	var req Request
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		writeResp(conn, Response{OK: false, Error: "bad request: " + err.Error()})
		return
	}
	writeResp(conn, s.dispatch(req))
}

func (s *Server) dispatch(req Request) Response {
	switch req.Cmd {
	case "upgrade":
		if s.upgrade == nil {
			return Response{Error: "upgrades are not enabled on this node: no trusted release keys are configured (upgrade.trusted_keys)"}
		}
		out, err := s.upgrade(req.UpOp, req.UpBody)
		if err != nil {
			return Response{Error: err.Error()}
		}
		return Response{OK: true, UpBody: out}
	case "reload":
		if s.reload == nil {
			return Response{OK: false, Error: "reload not supported"}
		}
		if err := s.reload(); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "nets":
		var ids []string
		for _, id := range s.api.NetworkIDs() {
			ids = append(ids, fmt.Sprintf("%016x", id))
		}
		return Response{OK: true, Nets: ids}
	case "ifaces":
		return Response{OK: true, Ifaces: s.api.Interfaces()}
	case "peers", "bans", "routes", "list":
		id, err := s.resolveNet(req.Net)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		resp := Response{OK: true}
		if req.Cmd == "peers" || req.Cmd == "list" {
			resp.Peers = s.api.ListPeers(id)
		}
		if req.Cmd == "bans" || req.Cmd == "list" {
			resp.Bans = s.api.ListBans(id)
		}
		if req.Cmd == "routes" || req.Cmd == "list" {
			resp.Routes = s.api.Routes(id)
		}
		if req.Cmd == "list" {
			resp.NATClass, resp.Public = s.api.NATStatusStrings()
			resp.LoopDrops = s.api.LoopDrops()
		}
		return resp
	case "ban":
		id, err := s.resolveNet(req.Net)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if err := s.api.BanNode(id, req.Node, req.Notes); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "unban":
		id, err := s.resolveNet(req.Net)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if req.Force {
			if err := s.api.ForceUnban(id, req.Node); err != nil {
				return Response{OK: false, Error: err.Error()}
			}
			return Response{OK: true}
		}
		if err := s.api.UnbanNode(id, req.Node); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "keydist":
		id, err := s.resolveNet(req.Net)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		var expNano int64
		if req.Expires != "" {
			t, perr := time.Parse(time.RFC3339, req.Expires)
			if perr != nil {
				return Response{OK: false, Error: "bad expires (want RFC3339): " + perr.Error()}
			}
			expNano = t.UnixNano()
		}
		if err := s.api.DistributeKey(id, req.Key, req.Label, expNano); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "fw":
		id, err := s.resolveNet(req.Net)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return s.dispatchFW(id, req)
	default:
		return Response{OK: false, Error: "unknown command: " + req.Cmd}
	}
}

func (s *Server) dispatchFW(id uint64, req Request) Response {
	switch req.FWOp {
	case "", "list":
		rules, err := s.api.FirewallRules(id)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, FW: rules}
	case "add":
		added, err := s.api.FirewallAdd(id, req.FWRule, req.FWAt)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, FW: []mesh.FirewallRule{added}}
	case "del":
		if err := s.api.FirewallDelete(id, req.FWIDs); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "move":
		if len(req.FWIDs) != 1 {
			return Response{OK: false, Error: "move requires exactly one rule id"}
		}
		if err := s.api.FirewallMove(id, req.FWIDs[0], req.FWTo); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "copy":
		if err := s.api.FirewallCopy(id, req.FWIDs); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "cut":
		if err := s.api.FirewallCut(id, req.FWIDs); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true}
	case "paste":
		n, err := s.api.FirewallPaste(id, req.FWAt)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		return Response{OK: true, Count: n}
	default:
		return Response{OK: false, Error: "unknown fw op: " + req.FWOp}
	}
}

// resolveNet maps a hex network id (or "" when only one network exists) to an id.
func (s *Server) resolveNet(ref string) (uint64, error) {
	ids := s.api.NetworkIDs()
	if ref == "" {
		switch len(ids) {
		case 0:
			return 0, fmt.Errorf("no networks configured; create one with 'gravinet network add NAME'")
		case 1:
			return ids[0], nil
		default:
			return 0, fmt.Errorf("multiple networks; specify -net <name or id>")
		}
	}
	// Prefer a name match (so -net corp works), then fall back to a hex id.
	if s.resolver != nil {
		if id, ok := s.resolver(ref); ok {
			return id, nil
		}
	}
	id, err := strconv.ParseUint(ref, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("unknown network %q (use a network name or hex id)", ref)
	}
	return id, nil
}

func writeResp(conn net.Conn, r Response) {
	b, _ := json.Marshal(r)
	conn.Write(append(b, '\n'))
}

// Close stops the server.
func (s *Server) Close() error { return s.ln.Close() }

// Do connects to a control endpoint, sends one request, and returns the reply.
func Do(path string, req Request) (Response, error) {
	network, addr := netAndAddr(path)
	conn, err := net.Dial(network, addr)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return Response{}, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	if !sc.Scan() {
		return Response{}, fmt.Errorf("no response from daemon")
	}
	var resp Response
	if err := json.Unmarshal(sc.Bytes(), &resp); err != nil {
		return Response{}, err
	}
	return resp, nil
}
