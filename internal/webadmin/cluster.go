package webadmin

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	neturl "net/url"
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
	err := s.mutateConfig(r, func(cfg *config.Config) error {
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
	err := s.mutateConfig(r, func(cfg *config.Config) error {
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
	err := s.mutateConfig(r, func(cfg *config.Config) error {
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
	// Manageable reports whether the header picker should offer this node as a
	// proxy target: we have an overlay IP + port to dial (from gossip), AND
	// there's an actual path there — either a live session (Connected) or the
	// peer is a Seed, which partial mesh always permits a link to regardless
	// of whether one happens to be up this instant. Gossip about a node's
	// address travels across the whole mesh independent of whether a session
	// to it can ever form (see partial mesh's peerListSeedBlock), so without
	// the Connected||IsSeed half of this check, a non-seed node would list
	// other non-seed peers it can structurally never reach — direct AND
	// relayed links between two non-seeds are both refused under partial
	// mesh (handshake_engine.go) — and picking one just silently fails back
	// to managing this node locally (see v669's removal of the peer-proxy
	// failure alert) with no indication why.
	Manageable bool `json:"manageable"`
	Manager    bool `json:"manager"` // peer currently advertises Manager mode — only a
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
	// IsSeed mirrors mesh.ManagedPeer.IsSeed — see its doc comment. Consulted
	// by the System > Upgrade push logic (ui.go) to hold seeds back until
	// last, and push them one at a time rather than batched with the rest.
	IsSeed bool `json:"is_seed,omitempty"`
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
			// See clusterPeer.Manageable's doc comment: a known overlay
			// address alone isn't enough on a partial-mesh network, where a
			// gossip-only, non-seed peer can be permanently unreachable.
			Manageable: ip.IsValid() && m.WebPort != 0 && (m.Connected || m.IsSeed),
			Manager:    m.Manager,
			Version:    m.Version,
			IsSeed:     m.IsSeed,
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

// managedDialProbeTimeout bounds one candidate probe in resolveManagedTarget.
// It is deliberately much shorter than proxyClient's 15s budget: this is a
// bare TCP connect to a peer's web port across an overlay whose RTTs run in
// the single-digit-to-low-hundreds of milliseconds, so a candidate that
// hasn't answered in two seconds is not the one to spend a management call
// on. The cost of being wrong is bounded — if no candidate answers, the
// preferred one is returned anyway and the caller fails exactly as it did
// before this probing existed.
const managedDialProbeTimeout = 2 * time.Second

// managedFamilyTTL is how long a probe result is trusted. handleProxy runs on
// essentially every peer-UI interaction, so probing per call would put a TCP
// connect in front of each one; caching the winning address per node bounds
// that to one probe a minute per peer. It is shorter than managedPeerTTL (90s)
// so a cached choice can never outlive the advertisement it came from.
const managedFamilyTTL = 60 * time.Second

type managedFamilyChoice struct {
	ip netip.Addr
	at time.Time
}

// resolveManagedTarget resolves node to a dialable (overlay ip, web port)
// pair, strictly from the live managed-peer set, and confirms the address is
// a genuine overlay address (SSRF guard: a malicious peer's advertisement
// can't aim a proxy at loopback, the LAN, or a cloud-metadata endpoint).
// Shared by handleProxy, the shell relay (shell.go), the fan-out capture
// (meshcapture.go), the fan-out tshoot (meshtshoot.go) and the upgrade push
// (upgrade_push.go) — everything that hops to a managed peer's web admin goes
// through here and needs the same guard.
//
// A dual-stack peer advertises both Overlay4 and Overlay6, and this used to
// take Overlay4 and consult Overlay6 only if Overlay4 was *absent* — never if
// it was merely unreachable. On a node whose v4 overlay path is broken while
// v6 is fine, every management call therefore burned its full timeout dialing
// an address that was never going to answer, with a working alternative
// sitting in the same advertisement. Since this function is the single
// chokepoint, that one line disabled peer management, remote shell, fan-out
// capture, fan-out tshoot and upgrade push on that peer simultaneously.
//
// So both families are candidates now. Each is validated independently against
// the SSRF and local-path checks — a peer can't smuggle a bad address in via
// the family that happens to be tried second — and the survivors are probed in
// preference order, v4 first to keep the previous choice where it still works.
func (s *Server) resolveManagedTarget(node string) (*clusterPeerTarget, error) {
	var cands []netip.Addr
	var port int
	var found bool
	for _, m := range s.be.ManagedPeers(managedPeerTTL) {
		if m.NodeID != node {
			continue
		}
		found = true
		port = int(m.WebPort)
		// v4 first: it is what this resolved to before, so a working v4
		// peer keeps behaving identically and only broken ones change path.
		if m.Overlay4.IsValid() {
			cands = append(cands, m.Overlay4)
		}
		if m.Overlay6.IsValid() {
			cands = append(cands, m.Overlay6)
		}
		break
	}
	if !found || port == 0 || len(cands) == 0 {
		return nil, &managedTargetError{http.StatusBadGateway, "peer not reachable for management (no overlay address/port, or not heard recently)"}
	}

	// Filter to candidates this node could legitimately dial. Both checks run
	// per candidate rather than once on a pre-picked address, because the
	// answer genuinely differs per family: a peer may advertise a valid v6
	// overlay address while its v4 is a spoofed non-overlay one, and this
	// node's own tun may carry one family and not the other.
	//
	// The rejection reasons are kept so that when nothing survives, the error
	// still says which of the three distinct things went wrong — the whole
	// reason managedTargetError exists — instead of collapsing to a generic
	// "not reachable" that hides a spoofing attempt behind a routing problem.
	var dialable []netip.Addr
	var spoofed bool
	var pathReason string
	for _, ip := range cands {
		if !s.be.OverlayContains(ip) {
			spoofed = true
			continue
		}
		// Fail fast if THIS node's overlay data plane can't actually carry the
		// dial. Without this, a node whose tun interface is missing/down would
		// route the connection to the peer's overlay address out its underlay
		// instead (the OS falling back to the default route), and the far end
		// would reject it with a baffling "connection arrived from <underlay
		// ip>, which isn't inside any of this node's overlay subnets".
		// Surfacing the real, local cause here turns that multi-layer mystery
		// into one clear message on the manager's own UI.
		if ok, reason := s.be.OverlayPathHealthy(ip); !ok {
			if pathReason == "" {
				pathReason = reason
			}
			continue
		}
		dialable = append(dialable, ip)
	}
	switch {
	case len(dialable) == 0 && pathReason != "":
		return nil, &managedTargetError{http.StatusServiceUnavailable, "cannot manage peers over the mesh: " + pathReason + " on this node"}
	case len(dialable) == 0 && spoofed:
		return nil, &managedTargetError{http.StatusForbidden, "target is not an overlay address"}
	case len(dialable) == 0:
		return nil, &managedTargetError{http.StatusBadGateway, "peer not reachable for management (no overlay address/port, or not heard recently)"}
	}

	return &clusterPeerTarget{ip: s.pickManagedAddr(node, dialable, port), port: port}, nil
}

// pickManagedAddr chooses which of a peer's dialable overlay addresses to use,
// probing them in order and remembering the winner for managedFamilyTTL.
//
// With a single candidate there is nothing to choose and nothing to learn, so
// it is returned untouched — no probe, no cache entry, behaviour identical to
// before. Probing only ever happens where there is a real alternative to fall
// back to.
//
// If no candidate answers, the first is returned rather than an error. The
// peer may be reachable for the actual request and not for a bare connect
// (mid-restart, a slow first hop on macOS's Network-Extension utun — see
// proxySpeedtestClient's note on exactly that), and this function's job is to
// pick between addresses, not to decide the peer is down. The caller then
// fails the way it always did.
func (s *Server) pickManagedAddr(node string, dialable []netip.Addr, port int) netip.Addr {
	if len(dialable) == 1 {
		return dialable[0]
	}
	if ip, ok := s.managedFamily(node); ok {
		for _, c := range dialable {
			if c == ip {
				return c
			}
		}
	}
	for _, ip := range dialable {
		c, err := s.probeDial(net.JoinHostPort(ip.String(), strconv.Itoa(port)), managedDialProbeTimeout)
		if err != nil {
			continue
		}
		c.Close()
		s.rememberManagedFamily(node, ip)
		return ip
	}
	return dialable[0]
}

// probeDial is the connect pickManagedAddr uses to decide whether a candidate
// address answers. It is a field rather than a direct net.DialTimeout call so
// the dual-stack tests can exercise the v4-dead/v6-live case on machines with
// no usable IPv6 — which includes most CI containers, exactly where this
// fallback would otherwise go untested.
func (s *Server) probeDial(hostport string, timeout time.Duration) (net.Conn, error) {
	if s.dialProbe != nil {
		return s.dialProbe(hostport, timeout)
	}
	return net.DialTimeout("tcp", hostport, timeout)
}

func (s *Server) managedFamily(node string) (netip.Addr, bool) {
	s.managedFamilyMu.Lock()
	defer s.managedFamilyMu.Unlock()
	c, ok := s.managedFamilyCache[node]
	if !ok || time.Since(c.at) > managedFamilyTTL {
		return netip.Addr{}, false
	}
	return c.ip, true
}

func (s *Server) rememberManagedFamily(node string, ip netip.Addr) {
	s.managedFamilyMu.Lock()
	defer s.managedFamilyMu.Unlock()
	if s.managedFamilyCache == nil {
		s.managedFamilyCache = map[string]managedFamilyChoice{}
	}
	s.managedFamilyCache[node] = managedFamilyChoice{ip: ip, at: time.Now()}
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
	// Same trap as the rollout guard above, for the mesh-wide capture job:
	// proxying this would fan out across the *selected peer's* managed-peer
	// list, not the operator's own, and hand back that peer's job/download
	// instead of the one the UI thinks it started.
	if pathBase == "/api/capture/mesh/start" || pathBase == "/api/capture/mesh/status" || pathBase == "/api/capture/mesh/download" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "mesh-wide capture is driven from the node you are logged in to, not through a peer"})
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

	// Build the URL from parts rather than by concatenation, so the authority
	// is assigned by this function and cannot be reached from the query
	// parameter at all.
	//
	// The string form ("https://" + hostport + path) was safe — path has to
	// begin with a slash, so the authority is already closed before it starts
	// — but safe only because of a check several lines away. Parsing the
	// caller's value as a URL in its own right and then setting Scheme and
	// Host makes it structurally impossible instead of conditionally true, and
	// it rejects an absolute URL outright rather than mangling it into the
	// path. It is also what a reader, or a static analyzer, can see locally:
	// CodeQL's go/request-forgery flagged the concatenated form for exactly
	// this reason, unable to tell from the assignment that the host came from
	// the managed-peer set rather than from the request.
	ref, err := neturl.Parse(path)
	if err != nil || ref.Scheme != "" || ref.Host != "" || ref.Opaque != "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only /api/ paths may be proxied"})
		return
	}
	hostport := net.JoinHostPort(target.ip.String(), strconv.Itoa(target.port))
	// Re-check on the parsed path: the string checks above ran on the raw
	// value, and this is the one that will actually be sent.
	if !strings.HasPrefix(ref.Path, "/api/") {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only /api/ paths may be proxied"})
		return
	}
	u := &neturl.URL{
		Scheme:   "https",
		Host:     hostport, // from resolveManagedTarget, never from the caller
		Path:     ref.Path,
		RawPath:  ref.RawPath,
		RawQuery: ref.RawQuery,
	}

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Belt-and-suspenders: after net/url has normalized the request, the path
	// it will actually send must still be under /api/, and the host must still
	// be the one resolved above.
	if !strings.HasPrefix(req.URL.Path, "/api/") || req.URL.Host != hostport {
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

// selfSignedPeerTLS is the client-side TLS configuration for every call this
// package makes to another node's web admin over the overlay: the peer proxy,
// the speedtest legs, and the relayed shell stream. Certificate verification
// is off, and the name says so rather than leaving it to a trailing comment
// on each of the four call sites that used to build this inline.
//
// What actually authenticates these calls is the mesh, not this handshake,
// and the argument is worth stating once in full because it is the whole
// justification for the field below:
//
//   - The address dialled is always an overlay address. resolveManagedTarget
//     drops any candidate failing OverlayContains, so a peer that advertises
//     a non-overlay address is refused rather than dialled.
//   - Reaching that address means holding the peer's mesh session keys. A
//     relayed path does not help an interposer: onRelay forwards opaque
//     ciphertext and the relay never has the keys (see internal/mesh/relay.go).
//   - A mesh member cannot pose as a different one. v185's anti-spoof rule
//     refuses a packet sourced from an overlay address another peer owns.
//
// So an attacker who could benefit from a forged certificate here would
// already have to be the peer. Peer web admins also carry per-node
// self-signed certs with no shared CA and no fingerprint distribution
// (webadmin.go's selfSignedCert), so there is nothing available to verify
// against in the first place.
//
// The one thing this does not cover, stated so it is not mistaken for
// covered: OverlayContains checks that the *address* is inside the overlay
// range, not that the packet left through the tunnel. If the overlay route
// is missing, or something local installs a competing route for that range,
// the dial goes out in the clear and nothing here would notice. Closing that
// needs pinning, which needs fingerprints on the control plane — a design
// change, not a config field.
//
// Shared as one value rather than four copies: net/http clones
// TLSClientConfig per connection and crypto/tls does not modify a Config, so
// reuse across the three clients and the direct tls.Dial is safe. It does
// mean a future per-site setting (a MinVersion, say) has to be split back
// out rather than added here.
var selfSignedPeerTLS = &tls.Config{InsecureSkipVerify: true}

// proxyClient talks to peer web admins over the overlay. Their certs are
// self-signed and the channel is already the encrypted mesh, so we skip cert
// verification here; the overlay + mesh PSK is the trust boundary.
var proxyClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:   selfSignedPeerTLS,
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
		TLSClientConfig:   selfSignedPeerTLS,
		ForceAttemptHTTP2: false,
	},
}
