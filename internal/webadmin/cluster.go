package webadmin

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"gravinet/internal/config"
)

// managedPeerTTL is how long a managed peer stays listed after we last heard it.
// The header dropdown drops anything older.
const managedPeerTTL = 90 * time.Second

// handleManaged reports and toggles this node's managed mode. GET returns the
// current state; POST {"on":bool} flips it via the same config path as any edit
// (so it persists and reloads live) — engine.SetManaged applies immediately to
// the running daemon, the same as firewall/NAT/QoS/key changes, so there's
// nothing for the caller to restart.
func (s *Server) handleManaged(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"managed": s.be.Managed()})
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.Managed = req.On
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "managed": req.On, "restart": false})
}

// handleManager reports and toggles this node's manager mode (the other half
// of the Managed/Manager split — see config.Config's doc comments). Same shape
// as handleManaged: GET reports, POST {"on":bool} flips it through the same
// live-reload config path, nothing to restart.
func (s *Server) handleManager(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"manager": s.be.Manager()})
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.Manager = req.On
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "manager": req.On, "restart": false})
}

// handleAcceptManagerUpgrades reports and toggles this node's opt-in to
// remote upgrades pushed by a directly-authenticated Manager peer (config
// Upgrade.AcceptManagerUpgrades). Same shape and same live-reload path as
// handleManaged/handleManager.
//
// Like those two, this is a LOCAL-ONLY setting: it must never be flippable on
// a remote peer through the management proxy, because "turn on the switch that
// lets you run binaries on me" is precisely the switch a compromised or
// mislabeled manager would want to flip remotely. handleProxy's blocklist
// enforces that (it lists /api/upgrade/accept-manager alongside /api/managed
// and /api/manager); this handler being reachable only with a genuine local
// session — via authed() with no bypass path a peer can satisfy for a
// non-managed node, and blocked at the proxy for a managed one — is the other
// half of that guarantee.
func (s *Server) handleAcceptManagerUpgrades(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		on := false
		if s.upg != nil && s.upg.AcceptManagerUpgrades != nil {
			on = s.upg.AcceptManagerUpgrades()
		}
		writeJSON(w, http.StatusOK, map[string]any{"accept_manager_upgrades": on})
		return
	}
	var req struct {
		On bool `json:"on"`
	}
	if !decode(w, r, &req) {
		return
	}
	err := s.mutateConfig(func(cfg *config.Config) error {
		cfg.Upgrade.AcceptManagerUpgrades = req.On
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// The remote-apply gate reads accept_manager_upgrades fresh from the
	// config file on each push (see webadminCtl), so this change takes effect
	// immediately — no restart required.
	s.log.Infof("upgrade: accept_manager_upgrades set to %v (local operator)", req.On)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "accept_manager_upgrades": req.On, "restart": false})
}

// clusterPeer is one row in the header dropdown.
type clusterPeer struct {
	NodeID     string `json:"node_id"`
	Hostname   string `json:"hostname"`
	Overlay    string `json:"overlay"`  // overlay IP we reach it on
	WebPort    int    `json:"web_port"` // its web-admin port
	AgeSeconds int    `json:"age_seconds"`
	Connected  bool   `json:"connected"`
	Manageable bool   `json:"manageable"` // we have an overlay IP + port to proxy to
	Manager    bool   `json:"manager"`    // peer currently advertises Manager mode — only a
	// Manager-mode peer is accepted by another node's overlay-sourced auth
	// bypass (see webadmin.authed / mesh.IsManagerAddr), so this is what the
	// Speedtest "from" picker filters on: a merely-Managed peer looks
	// reachable here but gets a 401 the moment it tries to act as the client
	// against a third peer.

	// Version is the peer's build version (see mesh's hsPayload.Version),
	// shown in the upgrade peer picker so an operator can tell at a glance
	// which nodes are behind before pushing to them. Empty for a peer too
	// old to advertise it; the UI shows that as unknown.
	Version string `json:"version,omitempty"`
}

// handleCluster lists managed peers heard within the TTL, plus whether this node
// itself is managed (so the UI can show a "local" entry and the toggle).
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	var peers []clusterPeer
	for _, m := range s.be.ManagedPeers(managedPeerTTL) {
		ip := m.Overlay4
		if !ip.IsValid() {
			ip = m.Overlay6
		}
		peers = append(peers, clusterPeer{
			NodeID:     m.NodeID,
			Hostname:   m.Hostname,
			Overlay:    addrStr(ip),
			WebPort:    int(m.WebPort),
			AgeSeconds: int(now.Sub(m.LastSeen).Seconds()),
			Connected:  m.Connected,
			Manageable: ip.IsValid() && m.WebPort != 0,
			Manager:    m.Manager,
			Version:    m.Version,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"managed":       s.be.Managed(),
		"manager":       s.be.Manager(),
		"self_hostname": s.be.Hostname(),
		"self_id":       s.be.SelfID(),
		"self_overlay":  addrStr(s.be.SelfOverlay()),
		"self_web_port": s.selfWebPort(),
		"peers":         peers,
	})
}

// selfWebPort returns the port this node's web admin listens on, for peers that
// run a speedtest against this node.
func (s *Server) selfWebPort() int {
	_, p, err := net.SplitHostPort(s.cfg.Listen)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(p)
	return n
}

func addrStr(a netip.Addr) string {
	if !a.IsValid() {
		return ""
	}
	return a.String()
}

// managedTargetError pairs an error with the HTTP status handleProxy (and the
// shell relay) should report for it — resolveManagedTarget fails for three
// distinct reasons that each deserve a different status/message on the
// caller's UI (unreachable vs. a spoofed/non-overlay address vs. this node's
// own overlay data plane being down), so collapsing them into one generic
// status would lose that.
type managedTargetError struct {
	status int
	msg    string
}

func (e *managedTargetError) Error() string { return e.msg }

// resolveManagedTarget resolves node to a dialable (overlay ip, web port)
// pair, strictly from the live managed-peer set, and confirms the address is
// a genuine overlay address (SSRF guard: a malicious peer's advertisement
// can't aim a proxy at loopback, the LAN, or a cloud-metadata endpoint).
// Shared by handleProxy and the shell relay (shell.go) — both hop to a
// managed peer's web admin the same way and need the same guard.
func (s *Server) resolveManagedTarget(node string) (*clusterPeerTarget, error) {
	var target *clusterPeerTarget
	for _, m := range s.be.ManagedPeers(managedPeerTTL) {
		if m.NodeID != node {
			continue
		}
		ip := m.Overlay4
		if !ip.IsValid() {
			ip = m.Overlay6
		}
		if ip.IsValid() && m.WebPort != 0 {
			target = &clusterPeerTarget{ip: ip, port: int(m.WebPort)}
		}
		break
	}
	if target == nil {
		return nil, &managedTargetError{http.StatusBadGateway, "peer not reachable for management (no overlay address/port, or not heard recently)"}
	}
	// The target address comes from a peer's advertisement; constrain it to a
	// real overlay address so a malicious peer can't aim the proxy at loopback,
	// the LAN, or a cloud-metadata endpoint (SSRF).
	if !s.be.OverlayContains(target.ip) {
		return nil, &managedTargetError{http.StatusForbidden, "target is not an overlay address"}
	}
	// Fail fast if THIS node's overlay data plane can't actually carry the dial.
	// Without this, a node whose tun interface is missing/down would route the
	// connection to the peer's overlay address out its underlay instead (the OS
	// falling back to the default route), and the far end would reject it with a
	// baffling "connection arrived from <underlay ip>, which isn't inside any of
	// this node's overlay subnets". Surfacing the real, local cause here turns
	// that multi-layer mystery into one clear message on the manager's own UI.
	if ok, reason := s.be.OverlayPathHealthy(target.ip); !ok {
		return nil, &managedTargetError{http.StatusServiceUnavailable, "cannot manage peers over the mesh: " + reason + " on this node"}
	}
	return target, nil
}

// writeManagedTargetError writes err's status/message if it's a
// *managedTargetError (from resolveManagedTarget), or 502 for anything else.
func writeManagedTargetError(w http.ResponseWriter, err error) {
	if mte, ok := err.(*managedTargetError); ok {
		writeJSON(w, mte.status, map[string]any{"error": mte.msg})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
}

// handleProxy forwards an API call to a managed peer's web admin over the
// overlay. The browser stays pointed at this node; selecting a peer in the
// header just adds ?node=<id>&path=<api path> here. The target must be a
// currently-advertised managed peer (SSRF guard), and the hop rides the
// encrypted overlay — the remote authorizes us by our overlay source address.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	path := r.URL.Query().Get("path")
	if node == "" || path == "" || len(path) == 0 || path[0] != '/' {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "node and a /api/... path are required"})
		return
	}
	// Managed/Manager mode are never remotely configurable — always local-only,
	// regardless of which peer is selected (see the web UI's LOCAL_API comment
	// for the full reasoning: an earlier version let these follow the proxy
	// like any other setting, which silently applied a toggle to the wrong
	// node). The frontend already never routes these through the proxy, but
	// that's a client-side convention only — this is the actual trust
	// boundary, so it's enforced here too rather than trusted to the caller.
	// The same applies to /api/shell/setting, tightened even further — see
	// its own doc comment in shell.go.
	pathBase := path
	if i := strings.IndexByte(pathBase, '?'); i >= 0 {
		pathBase = pathBase[:i]
	}
	// Driving a *fleet* is likewise never something to do through a proxy hop.
	// /api/upgrade/rollout on a remote peer would mean asking node B to
	// orchestrate a rollout of node B's staged artifact across node B's view of
	// the mesh, while the browser sat on node A believing it was driving — two
	// managers, two source lists, and a canary neither operator chose. The
	// per-node upgrade endpoints (state, local-apply, rollback) proxy fine and
	// are genuinely useful that way; the orchestration and the artifact upload
	// that seeds it stay on the node the operator is actually looking at.
	if pathBase == "/api/upgrade/rollout" || pathBase == "/api/upgrade/stage" || pathBase == "/api/upgrade/fleet" || pathBase == "/api/upgrade/push" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "rollouts are driven from the node you are logged in to, not through a peer"})
		return
	}
	if pathBase == "/api/managed" || pathBase == "/api/manager" || pathBase == "/api/shell/setting" || pathBase == "/api/upgrade/accept-manager" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "this setting is local-only and cannot be changed on a remote peer"})
		return
	}
	// Resolve the target strictly from the live managed-peer set.
	target, err := s.resolveManagedTarget(node)
	if err != nil {
		writeManagedTargetError(w, err)
		return
	}

	// Only proxy our own API surface, never arbitrary URLs. The raw-prefix check
	// is not enough on its own: "/api/../admin" and its percent-encoded forms
	// pass a naive prefix test but resolve elsewhere. Reject any traversal
	// outright (raw or encoded), then re-verify the *parsed* path still lives
	// under /api/ after normalization — so what actually gets sent can't escape.
	if len(path) < 5 || path[:5] != "/api/" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only /api/ paths may be proxied"})
		return
	}
	lower := strings.ToLower(path)
	if strings.Contains(path, "..") || strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "path traversal is not allowed"})
		return
	}

	hostport := net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))
	url := "https://" + hostport + path

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, url, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Belt-and-suspenders: after net/url has normalized the request, the path it
	// will actually send must still be under /api/. Catches any traversal the
	// string checks above didn't anticipate.
	if !strings.HasPrefix(req.URL.Path, "/api/") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only /api/ paths may be proxied"})
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	client := proxyClient
	if path == "/api/speedtest/run" {
		client = proxySpeedtestClient
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": fmt.Sprintf("reaching %s: %v", node, err)})
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, proxyBodyLimit(pathBase)))
}

// proxyBodyLimit returns the maximum number of bytes handleProxy will copy
// from a peer's response for pathBase. 8 MiB is a generous cap for the
// ordinary JSON API surface this proxies, but /api/capture/pcap is a real
// exception: capMaxBytes (capture.go) lets the rolling packet buffer it
// exports grow up to 32 MiB of raw packet data before the pcap framing
// overhead is even added, so the default cap would silently truncate a
// well-populated capture's download — exactly the way this endpoint used to
// avoid entirely by never being proxied at all (see LOCAL_API's comment in
// ui.go). Sized to the true worst case plus slack for the pcap global
// header and one 16-byte record header per packet, rather than either
// re-imposing the blanket never-proxied rule or leaving capture's one
// binary-download endpoint genuinely uncapped. Split out from handleProxy so
// this can be tested directly without a live overlay round trip.
func proxyBodyLimit(pathBase string) int64 {
	if pathBase == "/api/capture/pcap" {
		return capMaxBytes + (1 << 20)
	}
	return 8 << 20
}

type clusterPeerTarget struct {
	ip   netip.Addr
	port int
}

// proxyClient talks to peer web admins over the overlay. Their certs are
// self-signed and the channel is already the encrypted mesh, so we skip cert
// verification here; the overlay + mesh PSK is the trust boundary.
var proxyClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // overlay-internal, self-signed
		ForceAttemptHTTP2: false,
	},
}

// proxySpeedtestClient is used instead of proxyClient specifically for
// proxying /api/speedtest/run. That call has the target peer run its own
// two-leg (download, then upload — sequential, not concurrent) speedtest
// against a third peer before it responds, which per speedtest.go's own
// speedtestClient budget can legitimately take up to roughly
// 2*(stConnectSlack+stDuration+5s) ≈ 38s in the worst case — speedtest.go
// already had to give the peer-to-peer leg that much room specifically
// because a fresh overlay connection (especially the first hop to a given
// peer, and especially on macOS's Network-Extension-backed utun) can take
// noticeably longer than proxyClient's 15s budget assumes. Proxying the
// outer /api/speedtest/run call through proxyClient reintroduces exactly
// that "context deadline exceeded" failure one hop further out, on the very
// call that exists to trigger the slow operation in the first place.
var proxySpeedtestClient = &http.Client{
	Timeout: 2*(stConnectSlack+stDuration+5*time.Second) + 5*time.Second,
	Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // overlay-internal, self-signed
		ForceAttemptHTTP2: false,
	},
}
