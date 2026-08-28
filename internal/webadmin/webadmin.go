// Package webadmin serves the hot-config administration interface: a small
// HTTPS server with authenticated session login (local PBKDF2 users, or an
// OS/PAM seam), brute-force login throttling, and a JSON API over the running
// engine for peers, bans, routes, and the firewall rulebase.
package webadmin

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/pprof"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/logx"
	"gravinet/internal/mesh"
	"gravinet/internal/ratelimit"
	"gravinet/internal/service"
	"gravinet/internal/tcshape"
)

// Backend is the slice of the engine the admin UI drives.
type Backend interface {
	NetworkIDs() []uint64
	Interfaces() []mesh.IfaceInfo
	NATStatusStrings() (string, string)
	ListPeers(networkID uint64) []mesh.PeerInfo
	ListBans(networkID uint64) []mesh.BanInfo
	DisabledPeers(networkID uint64) []mesh.DisabledPeerInfo
	// SeedNodeOwners reports which node currently sits behind each seed
	// address, keyed by "ip:port" and bare "ip" (see mesh's own doc
	// comment). Feeds the seed/peer state coupling in seedpeercouple.go.
	SeedNodeOwners(networkID uint64) map[string]string
	Routes(networkID uint64) []mesh.RouteInfo
	// SetBGPRoutes updates networkID's BGP-into-mesh redistribution set (see
	// config.Network.RedistributeBGPRoutes and mesh's SetBGPRoutes) — the reverse
	// direction from Routes above, which reports what this node has *learned*
	// from peers; this pushes what it's currently *originating* from its own
	// BGP RIB. Called by bgpMeshRedistributor, never directly by an HTTP
	// handler.
	SetBGPRoutes(networkID uint64, routes []netip.Prefix, metric int) bool
	// SetBGPASN advertises this node's current effective BGP AS number to
	// every connected peer (see mesh.Engine.SetBGPASN / hsPayload.BGPASN's
	// doc comment) — 0 when BGP isn't enabled here at all. Called by
	// autoBGPReconciler on every reconcile pass, whether or not AutoBGP
	// itself produced the value, so a peer wanting to build a BGP neighbor
	// for this node — via AutoBGP or otherwise — always has this node's
	// real current ASN to use, never a guess reconstructed from an address.
	SetBGPASN(asn uint32)
	BanNode(networkID uint64, target, notes string) error
	UnbanNode(networkID uint64, target string) error
	EditBanNotes(networkID uint64, target, notes string) error
	ForceUnban(networkID uint64, target string) error
	ResetNetwork(networkID uint64) error
	FirewallRules(networkID uint64) ([]mesh.FirewallRule, error)
	FirewallExemptsFor(networkID uint64) []mesh.ExemptInfo
	FirewallAdd(networkID uint64, r mesh.FirewallRule, at int) (mesh.FirewallRule, error)
	FirewallDelete(networkID uint64, ids []uint64) error
	FirewallMove(networkID, id uint64, to int) error
	// Object/service catalog (node-global, shared by every network — see
	// Config.FirewallObjects' doc comment) + counters (v392 firewall parity).
	FirewallObjectsList() ([]mesh.FirewallObject, error)
	SetFirewallObjects(objs []mesh.FirewallObject) error
	FirewallServicesList() ([]mesh.FirewallService, error)
	SetFirewallServices(svcs []mesh.FirewallService) error
	FirewallResetCounters(networkID uint64, ids []uint64) error
	FloodKey(networkID uint64, keyB64, label string, expiresNano int64, slot int) error
	RetractKey(networkID uint64, keyB64 string) error

	// Managed-cluster surface.
	ManagedPeers(maxAge time.Duration) []mesh.ManagedPeer
	Managed() bool
	Manager() bool
	// LogLevel reports the daemon's current log level (see handleLogLevel).
	LogLevel() string
	IsManagerAddr(ip netip.Addr) bool
	// IsManagerNeighborAddr is the strict, direct-session-only form of
	// IsManagerAddr, used to gate the remote-upgrade (code-execution) path
	// where gossip-level manager trust is insufficient. See its doc comment
	// in the mesh engine.
	IsManagerNeighborAddr(ip netip.Addr) bool
	Hostname() string
	SelfID() string
	SelfOverlay() netip.Addr
	// SelfPeer returns this node's own identity on networkID (hostname, node
	// id, its own overlay address there) in the same shape as a ListPeers
	// entry, so the peers table can show this node alongside the peers it
	// connects to. ok is false if networkID isn't configured here.
	SelfPeer(networkID uint64) (mesh.PeerInfo, bool)
	OverlayContains(ip netip.Addr) bool
	OverlayReachable(ip netip.Addr) bool
	// OverlayPathHealthy reports whether this node's overlay data plane can
	// currently carry traffic to dst (interface for that subnet present, up, and
	// addressed), with a human reason when it can't. Used to fail management
	// proxying fast instead of leaking the dial to the underlay.
	OverlayPathHealthy(dst netip.Addr) (bool, string)
}

const (
	sessionCookie = "gravinetadmin"
	sessionTTL    = 8 * time.Hour
)

// Server is the admin HTTP server.
type Server struct {
	cfg      config.WebAdmin
	be       Backend
	log      *logx.Logger
	auth     Authenticator
	throttle *ratelimit.Throttle
	noAuth   bool // true when no usable authenticator exists (login always fails)

	mu      sync.Mutex
	secret  []byte               // HMAC key for stateless session cookies; persisted across restarts
	revoked map[string]time.Time // tokens explicitly logged out (value -> token expiry); in-memory only

	configPath         string       // optional: enables the /api/config view + edits
	logPath            string       // optional: enables the /api/logs view
	logClear           func() error // optional: truncates the active log file (Clear button)
	readmePath         string       // optional: enables the /api/readme view
	licensePath        string       // optional: enables the /api/license view
	gettingStartedPath string       // optional: enables the Info -> Getting Started page (markdown-rendered)
	apiDocPath         string       // optional: enables the Info -> API page (reads API.md from disk, markdown-rendered)
	reload             func() error // optional: re-applies config live after an edit
	cfgMu              sync.Mutex   // serializes config-file read-modify-write

	upg *UpgradeCtl // mesh-distributed binary upgrades; nil when not configured

	// managedFamilyCache remembers which of a dual-stack peer's overlay
	// addresses last answered a connect, so resolveManagedTarget probes at
	// most once per managedFamilyTTL per peer rather than once per
	// management call. Keyed by node id. Its own mutex, not mu: this is on
	// the path of every proxied peer request and has nothing to do with the
	// session state mu guards.
	managedFamilyMu    sync.Mutex
	managedFamilyCache map[string]managedFamilyChoice

	// dialProbe, when set, replaces net.DialTimeout in pickManagedAddr. Tests
	// only; nil in production.
	dialProbe func(hostport string, timeout time.Duration) (net.Conn, error)

	httpSrv *http.Server
	ln      net.Listener
	extraLn map[string]net.Listener // additional listeners (e.g. overlay addresses), by address

	tlsCert *x509.Certificate // the cert Start() actually loaded (custom or self-signed); for display only

	// historyMu guards the fields below: config history debouncing. Several
	// commits in a short window (e.g. every field on the Performance card
	// being changed in one sitting) collapse into one snapshot comparing the
	// state before the *first* commit of the burst against the state after
	// the *last*, rather than one snapshot per field — see
	// scheduleHistorySnapshot's doc comment.
	historyMu            sync.Mutex
	historyPendingBefore *config.Config
	historyPendingUser   string
	historyTimer         *time.Timer

	bootID string // random per-process id; lets the admin UI detect a restart

	version string // gravinet build version (for the About tab); set via SetVersion
	commit  string // gravinet build commit

	metrics     *metricsCollector     // CPU/mem/interface time series for the Metrics tab
	latencyHist *latencyCollector     // passive per-peer RTT history for the Latency tab's expanded chart
	capture     *captureState         // live packet capture for the Capture tab
	meshCapture *meshCaptureManager   // fan-out "capture all mesh peers at once" job, same tab
	bgpRedis    *bgpMeshRedistributor // polls FRR's RIB, pushes BGP routes into the mesh (config.Network.RedistributeBGPRoutes)
	autoBGP     *autoBGPReconciler    // derives ASN/router-id and maintains one Neighbor per online mesh peer (config.BGPConfig.AutoBGP)
}

// SetVersion records the build version/commit for the Info → About tab.
func (s *Server) SetVersion(version, commit string) { s.version, s.commit = version, commit }

// SetConfigPath lets the admin UI read NAT/QoS/bandwidth/network settings from
// the config file for its read-only views.
func (s *Server) SetConfigPath(path string) { s.configPath = path }

// SetLogPath enables the /api/logs view by pointing it at the daemon's log file.
func (s *Server) SetLogPath(path string) { s.logPath = path }

// SetLogClear wires the Clear-log action to a function that empties the active
// log file (typically the rotating writer's Truncate method).
func (s *Server) SetLogClear(fn func() error) { s.logClear = fn }

// SetReadmePath enables the /api/readme view by pointing it at the README on
// disk (installed alongside the binary). Empty disables the view.
func (s *Server) SetReadmePath(path string) { s.readmePath = path }

// SetLicensePath enables the /api/license view by pointing it at the LICENSE on
// disk (installed alongside the binary). Empty disables the view.
func (s *Server) SetLicensePath(path string) { s.licensePath = path }

// SetGettingStartedPath enables the Info → Getting Started page by pointing
// it at getting-started.md, the markdown source rendered natively via
// mdRender — the same renderer README uses — so it matches the rest of the
// app's own styling. (A separate getting-started.html once existed, shown
// in an iframe; removed in favor of this single markdown source once
// native styling was what was actually wanted, so there's no second file
// to keep in sync.) Empty shows a friendly "not installed" message, the
// same graceful-degradation shape as Readme/License — the sidebar item
// itself is always present, matching those two.
func (s *Server) SetGettingStartedPath(path string) { s.gettingStartedPath = path }

// SetAPIDocPath enables the Info -> API page by pointing it at API.md, the
// HTTP API reference, rendered natively via mdRender exactly like
// Readme/Getting-Started. Reads the file from disk on every request rather
// than embedding a copy of it in the UI, so there is exactly one place the
// API surface is documented and it can't silently drift from what the
// running binary actually serves. Empty shows the same graceful "not
// installed" message as Readme/License/Getting-Started.
func (s *Server) SetAPIDocPath(path string) { s.apiDocPath = path }

// SetReload installs the callback that re-applies the config live after the web
// UI edits it (firewall/NAT/QoS/bandwidth take effect immediately; structural
// changes still need a restart).
func (s *Server) SetReload(fn func() error) { s.reload = fn }

// mutateConfig loads the config file, applies fn, validates, saves, and reloads.
// It serializes concurrent edits so two requests can't clobber the file — and,
// via config.WithLock, also serializes against the engine's independent async
// persist hook (mesh-learned state written back on its own schedule), so that
// writer can't silently revert a change made here by saving a copy it loaded
// before this one committed. See WithLock's doc for why that combination once
// mattered in practice, not just in theory.
// mutateConfig loads the config, applies fn, validates, saves, reloads, and
// (on success) snapshots the before/after into config history — see
// internal/config/history.go. r identifies who made the change (via
// validSession; "" if it can't be determined, e.g. a Managed-mode proxy
// request with no session cookie of its own — shown as "system" in the
// config history table, matching how an unattributed snapshot already
// displays there).
func (s *Server) mutateConfig(r *http.Request, fn func(*config.Config) error) error {
	applyLive, err := s.mutateConfigDeferReload(r, fn)
	if err != nil {
		return err
	}
	applyLive()
	return nil
}

// mutateConfigDeferReload is mutateConfig with the live-apply step handed back
// to the caller instead of run inline. Everything else is identical: load,
// apply, validate, save and snapshot all happen before it returns, under the
// same locks, so a caller that ignores the returned function still has a
// committed change — it just isn't running yet.
//
// This exists for one situation, and it should stay rare. Applying a config
// change can take down the overlay interface the request being served arrived
// on: a changed overlay address makes reloadFn rebuild that network, which
// closes the TUN, removes the address the connection is bound to, and re-forms
// every session. When a manager is driving a peer over the mesh (handleProxy
// dials the peer's overlay address — it is the only kind of address
// resolveManagedTarget will return), the peer would remove the address, then
// try to answer down a socket whose source no longer exists. Nothing came back
// and no RST could, since the return path was the overlay itself, so the
// manager sat until proxyClient's 15s deadline and reported "context deadline
// exceeded" for a change that had in fact been saved and applied.
//
// Deferring lets the handler write and flush its response first, while the
// interface is still up. That is ordering, not a guarantee: the reply is
// handed to the kernel and goes out over a live tunnel, but if that segment is
// lost the retransmission has nothing left to travel over. The remaining
// window is one packet wide instead of the whole rebuild.
//
// Only handlers that can sever their own path should use this. Reloading
// inline is the right default everywhere else — it is what makes an edit's
// effect true by the time the response says it succeeded, and every other
// caller depends on that.
func (s *Server) mutateConfigDeferReload(r *http.Request, fn func(*config.Config) error) (applyLive func(), err error) {
	if s.configPath == "" {
		return nil, fmt.Errorf("config path not set")
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	err = config.WithLock(s.configPath, func() error {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			return err
		}
		before, err := cfg.Clone()
		if err != nil {
			return err
		}
		if err := fn(cfg); err != nil {
			return err
		}
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := cfg.SaveTo(s.configPath); err != nil {
			return err
		}
		user := ""
		if r != nil {
			user, _ = s.validSession(r)
		}
		s.scheduleHistorySnapshot(before, user)
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Re-taken rather than held across the caller's response write: a reload
	// reads the config file back, so it needs the same protection against the
	// engine's async persist hook that the commit above did. Nesting is the
	// same shape as before — the inline path ran reload inside both locks too.
	return func() {
		if s.reload == nil {
			return
		}
		s.cfgMu.Lock()
		defer s.cfgMu.Unlock()
		if err := config.WithLock(s.configPath, s.reload); err != nil {
			s.log.Warnf("webadmin: reload after edit failed: %v", err)
		}
	}, nil
}

// restoreConfig replaces the live config outright with candidate, running it
// through the same validate/save/reload/snapshot pipeline as any other
// change (see mutateConfig) rather than a bespoke path of its own — so a
// restore is validated exactly as strictly as a normal edit, and is itself
// snapshotted, which is what makes "restore the restore" (undoing a bad
// restore) possible without anything special.
func (s *Server) restoreConfig(r *http.Request, candidate *config.Config) error {
	return s.mutateConfig(r, func(cfg *config.Config) error {
		*cfg = *candidate
		return nil
	})
}

// historyDebounceWindow is how long a burst of commits can stay quiet
// before it's treated as finished — matches PERF_RESTART_DEBOUNCE_MS /
// LOGIN_BAN_RESTART_DEBOUNCE_MS on the JS side, the client-side debounce
// this mirrors server-side for exactly the same reason (see
// scheduleHistorySnapshot's doc comment).
const historyDebounceWindow = 3 * time.Second

// scheduleHistorySnapshot debounces config history the same way the UI
// already debounces a restart for multi-field cards (Performance, Login):
// several commits close together collapse into one snapshot rather than
// one per commit. Unlike the client-side restart debounce, this one is
// server-side and applies uniformly to every commit through mutateConfig,
// not just the cards that opted into a JS debounce — gravinet's handlers
// are far more granular than parapet's (SeedAdd/SeedRemove/SeedSetNotes are
// three commits for what's conceptually "editing one seed"), so without
// this, editing several fields in one sitting produced one snapshot per
// field instead of one per editing session.
//
// The first commit of a new burst remembers its "before" state; each
// subsequent commit within the window just resets the timer and updates
// who gets credited (the most recent committer — in practice always the
// same person mid-session). When the window elapses with no further
// commits, flushPendingHistorySnapshot compares that remembered "before"
// against whatever the config looks like *now*, so the one snapshot taken
// reflects the whole burst, not just its last commit.
func (s *Server) scheduleHistorySnapshot(before *config.Config, user string) {
	s.historyMu.Lock()
	defer s.historyMu.Unlock()
	if s.historyPendingBefore == nil {
		s.historyPendingBefore = before
	}
	s.historyPendingUser = user
	if s.historyTimer != nil {
		s.historyTimer.Stop()
	}
	s.historyTimer = time.AfterFunc(historyDebounceWindow, s.flushPendingHistorySnapshot)
}

// flushPendingHistorySnapshot takes the debounced snapshot immediately,
// skipping the rest of the wait. Called from two places: the debounce timer
// itself when the window elapses normally, and every path that's about to
// restart this process (handleRestart, and the hostname-change handler's
// own direct restart) — a process restart kills the timer along with
// everything else in memory, so without an explicit flush first, a
// snapshot still mid-debounce would simply never get taken. That matters
// most for GeoIP/UPnP/Remote shell, which restart immediately with no
// debounce of their own at all: without this, toggling one of those would
// silently stop producing a snapshot, since the process would be gone
// before the 3-second window ever elapsed on its own. Harmless no-op if
// nothing is pending.
func (s *Server) flushPendingHistorySnapshot() {
	s.historyMu.Lock()
	before := s.historyPendingBefore
	user := s.historyPendingUser
	if s.historyTimer != nil {
		s.historyTimer.Stop()
		s.historyTimer = nil
	}
	s.historyPendingBefore = nil
	s.historyPendingUser = ""
	s.historyMu.Unlock()
	if before == nil {
		return
	}
	after, err := config.Load(s.configPath)
	if err != nil {
		return
	}
	config.OnCommit(s.configPath, before, after, user, after.EffectiveConfigHistoryLimit())
}

// New builds a Server, choosing the authenticator from the config auth mode.
func New(cfg config.WebAdmin, be Backend, log *logx.Logger) *Server {
	var auth Authenticator
	noAuth := false
	switch strings.ToLower(cfg.AuthMode) {
	case "pam", "windows", "system":
		if a, ok := systemAuthenticator(cfg.PAMService, log); ok {
			auth = a
			warnIfPAMServiceMissing(cfg.PAMService, log)
		} else {
			log.Errorf("webadmin: auth_mode=%q but this binary was built WITHOUT system-auth support "+
				"(CGO disabled), so %s login cannot work. Reinstall with the platform installer (it builds "+
				"with CGO so PAM/LogonUser are present), or set web_admin.auth_mode=\"local\" and add a user "+
				"with 'gravinet genpass'.", cfg.AuthMode, systemAuthName())
			auth = NewLocalAuth(cfg.Users)
			if len(cfg.Users) == 0 {
				noAuth = true
				log.Errorf("webadmin: no local users configured either — every login will fail until this is fixed")
			} else {
				log.Warnf("webadmin: falling back to %d configured local user(s)", len(cfg.Users))
			}
		}
	default:
		auth = NewLocalAuth(cfg.Users)
		if len(cfg.Users) == 0 {
			noAuth = true
			log.Errorf("webadmin: auth_mode=local but no users configured — every login will fail; add one with 'gravinet genpass'")
		}
	}
	lb := cfg.LoginBan
	maxF := lb.EffectiveMaxFailures()
	win := lb.Window()
	if win <= 0 {
		win = time.Minute
	}
	ban := time.Duration(lb.EffectiveBanSeconds()) * time.Second
	return &Server{
		cfg:         cfg,
		be:          be,
		log:         log,
		auth:        auth,
		noAuth:      noAuth,
		throttle:    ratelimit.New(maxF, win, ban, lb.Coalesce()),
		bootID:      randomBootID(),
		capture:     newCaptureState(),
		meshCapture: newMeshCaptureManager(),
	}
}

// randomBootID returns a fresh random id for this process. The admin UI captures
// it before a restart and reloads when it changes, which detects the new process
// reliably regardless of how briefly the old one was unreachable.
func randomBootID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand failure is effectively impossible here; fall back to a time value.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// pamServiceFileApplies reports whether goos builds the PAM backend and so
// has an /etc/pam.d/<service> file that can go missing. Split out from
// warnIfPAMServiceMissing so the platform list can be exercised directly by a
// test without depending on the test binary's own runtime.GOOS. Must track
// auth_pam.go's build tag `(linux || darwin || freebsd) && cgo` — the set of
// OSes that actually compile the PAM authenticator — not just "the first two
// platforms this existed for", which is how freebsd got left off checked here
// after auth_pam.go grew to include it.
func pamServiceFileApplies(goos string) bool {
	return goos == "linux" || goos == "darwin" || goos == "freebsd"
}

// warnIfPAMServiceMissing logs a warning when the PAM service file the
// authenticator will use doesn't exist, which makes every login fail (PAM falls
// through to a deny-by-default "other" stack). Linux, macOS, and FreeBSD all
// build the PAM backend (see auth_pam.go's `(linux || darwin || freebsd) &&
// cgo` build tag) and all three installers write /etc/pam.d/<service> the same
// way (install-linux.sh, install-macos.sh, install-freebsd.sh) — so this check
// applies to all three, not just the first two. OpenBSD never reaches here: it
// authenticates via BSD auth (login_passwd(8), see auth_bsdauth.go), which has
// no PAM service file to be missing.
func warnIfPAMServiceMissing(service string, log *logx.Logger) {
	if service == "" {
		service = "gravinet"
	}
	if !pamServiceFileApplies(runtime.GOOS) {
		return
	}
	path := "/etc/pam.d/" + service
	if _, err := os.Stat(path); err != nil {
		log.Errorf("webadmin: PAM service file %s is missing — logins will fail. "+
			"Reinstall with the platform installer (it writes this file), or create it, e.g.: "+
			"printf '#%%%%PAM-1.0\\nauth required pam_unix.so\\naccount required pam_unix.so\\n' | sudo tee %s", path, path)
	}
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/static/xterm.js", s.handleXtermJS)
	mux.HandleFunc("/static/xterm.css", s.handleXtermCSS)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/ping", s.handlePing) // unauthenticated: liveness + boot id
	mux.HandleFunc("/api/status", s.authed(s.handleStatus))
	mux.HandleFunc("/api/config", s.authed(s.handleConfig))
	mux.HandleFunc("/api/radvd", s.authed(s.handleRouterAdvert))
	mux.HandleFunc("/api/dhcp", s.authed(s.handleDHCP))
	mux.HandleFunc("/api/system/interfaces", s.authed(s.handleSystemInterfaces))
	mux.HandleFunc("/api/system/vlans", s.authed(s.handleSystemVLANs))
	mux.HandleFunc("/api/system/interface-edit", s.authed(s.handleSystemInterfaceEdit))
	mux.HandleFunc("/api/ban", s.authed(s.handleBan))
	mux.HandleFunc("/api/ban/notes", s.authed(s.handleBanNotes))
	mux.HandleFunc("/api/peer", s.authed(s.handlePeer))
	mux.HandleFunc("/api/unban", s.authed(s.handleUnban))
	mux.HandleFunc("/api/firewall", s.authed(s.handleFirewall))
	mux.HandleFunc("/api/network", s.authed(s.handleNetwork))
	mux.HandleFunc("/api/network/token", s.authed(s.handleNetworkToken))
	mux.HandleFunc("/api/network/reset", s.authed(s.handleNetworkReset))
	mux.HandleFunc("/api/key", s.authed(s.handleKey))
	mux.HandleFunc("/api/route", s.authed(s.handleRoute))
	mux.HandleFunc("/api/seed", s.authed(s.handleSeed))
	mux.HandleFunc("/api/seed-info", s.authed(s.handleSeedInfo))
	mux.HandleFunc("/api/peer-info", s.authed(s.handlePeerInfo))
	mux.HandleFunc("/api/host", s.authed(s.handleHost))
	mux.HandleFunc("/api/dns", s.authed(s.handleDNS))
	mux.HandleFunc("/api/interfaces", s.authed(s.handleInterfaces))
	mux.HandleFunc("/api/nat", s.authed(s.handleNAT))
	mux.HandleFunc("/api/qos", s.authed(s.handleQoS))
	mux.HandleFunc("/api/bandwidth", s.authed(s.handleBandwidth))
	mux.HandleFunc("/api/bgp", s.authed(s.handleBGP))                                         // read-only BGP peer status via FRR/vtysh
	mux.HandleFunc("/api/bgp/config", s.authed(s.handleBGPConfig))                            // read/write BGP+BFD config; drives the FRR daemon
	mux.HandleFunc("/api/bgp/import", s.authed(s.handleBGPImport))                            // read live FRR config to reflect a pre-existing setup
	mux.HandleFunc("/api/bgp/table", s.authed(s.handleBGPTable))                              // read-only full BGP table ('show bgp') via FRR/vtysh
	mux.HandleFunc("/api/webadmin/listen-options", s.authed(s.handleListenOptions))           // addresses this host can bind the admin interface to
	mux.HandleFunc("/api/webadmin/listen-addrs", s.authed(s.handleListenAddrsSave))           // save the picked set (restart to apply)
	mux.HandleFunc("/api/bgp/redistribute-options", s.authed(s.handleBGPRedistributeOptions)) // this host's current connected/static routes, for the redistribute pickers
	mux.HandleFunc("/api/bfd", s.authed(s.handleBFD))                                         // read-only BFD session status via FRR/vtysh
	mux.HandleFunc("/api/restart", s.authed(s.handleRestart))
	mux.HandleFunc("/api/system/power", s.authed(s.handleSystemPower))       // reboot/shut down the host (System > Power)
	mux.HandleFunc("/api/system/resolver", s.authed(s.handleSystemResolver)) // hostname / default DNS (System > Resolver)
	mux.HandleFunc("/api/system/time", s.authed(s.handleSystemTime))         // host clock / timezone / NTP (System > Time)
	mux.HandleFunc("/api/system/users", s.authed(s.handleSystemUsers))       // console OS accounts (System > Users)
	mux.HandleFunc("/api/system/snmp", s.authed(s.handleSystemSNMP))         // SNMPv2c agent (System > SNMP)
	mux.HandleFunc("/api/system/lldp", s.authed(s.handleSystemLLDP))         // LLDP/CDP agent (System > LLDP)
	mux.HandleFunc("/api/l2neighbors", s.authed(s.handleL2Neighbors))        // read-only LLDP/CDP neighbor table (Monitor > L2 Peers)
	mux.HandleFunc("/api/system/syslog", s.authed(s.handleSystemSyslog))     // remote syslog forwarding (System > Syslog)
	mux.HandleFunc("/api/cluster", s.authed(s.handleCluster))
	mux.HandleFunc("/api/loglevel", s.authed(s.handleLogLevel))
	mux.HandleFunc("/api/logsize", s.authed(s.handleLogSize))
	mux.HandleFunc("/api/managed", s.authed(s.handleManaged))
	mux.HandleFunc("/api/manager", s.authed(s.handleManager))
	mux.HandleFunc("/api/upgrade/accept-manager", s.authed(s.handleAcceptManagerUpgrades))
	// Upgrade surface: local-only, per-node. Every handler here enforces its
	// own session check (upgradeLocalOnly) rather than relying on authed()'s
	// Managed/Manager bypass — see that function's doc comment.
	mux.HandleFunc("/api/upgrade", s.authed(s.handleUpgradeHome))
	mux.HandleFunc("/api/upgrade/os-updates", s.authed(s.handleUpgradeOSUpdates))
	mux.HandleFunc("/api/upgrade/source", s.authed(s.handleUpgradeSource))
	mux.HandleFunc("/api/upgrade/rollback", s.authed(s.handleUpgradeRollback))
	// remote-apply is the one upgrade endpoint a peer may reach, and only a
	// directly-connected Manager, and only when this node opted in
	// (accept_manager_upgrades). It is registered WITHOUT authed() on purpose:
	// authed()'s bypass would admit a gossip-only "manager", which is too weak
	// to build and run code as root. The handler does its own stricter gating
	// (opt-in + IsManagerNeighborAddr, or a genuine local session), so it must
	// see the raw request rather than an authed()-filtered one.
	mux.HandleFunc("/api/upgrade/remote-apply", s.handleUpgradeRemoteApply)
	// push is the manager side: local-only (driven from the node you're on),
	// streams one uploaded source archive to selected peers' remote-apply
	// endpoints, which each build it natively.
	mux.HandleFunc("/api/upgrade/push", s.authed(s.handleUpgradePush))
	mux.HandleFunc("/api/routeadv", s.authed(s.handleRouteAdv))
	mux.HandleFunc("/api/keepalive", s.authed(s.handleKeepalive))
	mux.HandleFunc("/api/peertimeout", s.authed(s.handlePeerTimeout))
	mux.HandleFunc("/api/port", s.authed(s.handlePort))
	mux.HandleFunc("/api/tcpport", s.authed(s.handleTCPPort))
	mux.HandleFunc("/api/natstate", s.authed(s.handleNATState))
	mux.HandleFunc("/api/loginban", s.authed(s.handleLoginBan))
	mux.HandleFunc("/api/tls-cert", s.authed(s.handleTLSCert))
	mux.HandleFunc("/api/tls-cert/reset", s.authed(s.handleTLSCertReset))
	mux.HandleFunc("/api/history", s.authed(s.handleHistoryList))
	mux.HandleFunc("/api/history/get", s.authed(s.handleHistoryGet))
	mux.HandleFunc("/api/history/diff", s.authed(s.handleHistoryDiff))
	mux.HandleFunc("/api/history/restore", s.authed(s.handleHistoryRestore))
	mux.HandleFunc("/api/history/snapshot", s.authed(s.handleHistorySnapshot))
	mux.HandleFunc("/api/history/import", s.authed(s.handleHistoryImport))
	mux.HandleFunc("/api/history/limit", s.authed(s.handleConfigHistoryLimit))
	mux.HandleFunc("/api/geoip", s.authed(s.handleGeoIPSetting))
	mux.HandleFunc("/api/ipforwarding", s.authed(s.handleIPForwardingSetting))
	mux.HandleFunc("/api/redirects", s.authed(s.handleRedirectsSetting))
	mux.HandleFunc("/api/upnp", s.authed(s.handleUPnPSetting))
	mux.HandleFunc("/api/worker-threads", s.authed(s.handleWorkerThreads))
	mux.HandleFunc("/api/tun-queues", s.authed(s.handleTunQueues))
	mux.HandleFunc("/api/udp-gso", s.authed(s.handleUDPGSOSetting))
	mux.HandleFunc("/api/socket-buffer", s.authed(s.handleSocketBuffer))
	mux.HandleFunc("/api/shell/setting", s.sessionOnly(s.handleShellSetting))
	mux.HandleFunc("/api/shell/ws", s.sessionOnly(s.handleShellWS))
	mux.HandleFunc("/api/shell/hijack", s.authed(s.handleShellHijack))
	mux.HandleFunc("/api/exempt", s.authed(s.handleExempt))
	mux.HandleFunc("/api/logs", s.authed(s.handleLogs))
	mux.HandleFunc("/api/logs/clear", s.authed(s.handleLogsClear))
	mux.HandleFunc("/api/tshoot", s.authed(s.handleTshoot))          // one-file diagnostic bundle (Monitor -> Logs -> tshoot)
	mux.HandleFunc("/api/tshoot/mesh", s.authed(s.handleMeshTshoot)) // fan-out sibling: every reachable managed peer's bundle in one download
	mux.HandleFunc("/api/readme", s.authed(s.handleReadme))
	mux.HandleFunc("/api/license", s.authed(s.handleLicense))
	mux.HandleFunc("/api/getting-started", s.authed(s.handleGettingStarted))
	mux.HandleFunc("/api/api-doc", s.authed(s.handleAPIDoc))
	mux.HandleFunc("/api/about", s.authed(s.handleAbout))
	mux.HandleFunc("/api/metrics", s.authed(s.handleMetrics))
	mux.HandleFunc("/api/capture/interfaces", s.authed(s.handleCaptureInterfaces))
	mux.HandleFunc("/api/capture/start", s.authed(s.handleCaptureStart))
	mux.HandleFunc("/api/capture/stop", s.authed(s.handleCaptureStop))
	mux.HandleFunc("/api/capture/clear", s.authed(s.handleCaptureClear))
	mux.HandleFunc("/api/capture/packets", s.authed(s.handleCapturePackets))
	mux.HandleFunc("/api/capture/pcap", s.authed(s.handleCapturePcap))
	mux.HandleFunc("/api/capture/mesh-iface", s.authed(s.handleCaptureMeshIface))
	mux.HandleFunc("/api/capture/mesh/start", s.authed(s.handleCaptureMeshStart))
	mux.HandleFunc("/api/capture/mesh/status", s.authed(s.handleCaptureMeshStatus))
	mux.HandleFunc("/api/capture/mesh/download", s.authed(s.handleCaptureMeshDownload))
	mux.HandleFunc("/api/speedtest/source", s.authed(s.handleSpeedtestSource))
	mux.HandleFunc("/api/speedtest/sink", s.authed(s.handleSpeedtestSink))
	mux.HandleFunc("/api/speedtest/run", s.authed(s.handleSpeedtestRun))
	mux.HandleFunc("/api/localroutes", s.authed(s.handleLocalRoutes))
	mux.HandleFunc("/api/localhosts", s.authed(s.handleLocalHosts))
	mux.HandleFunc("/api/localdns", s.authed(s.handleLocalDNS))
	mux.HandleFunc("/api/latency", s.authed(s.handleLocalLatency))
	mux.HandleFunc("/api/latency/history", s.authed(s.handleLatencyHistory))
	// /debug/pprof/* — Go's standard runtime profiler, under the same auth as
	// every other route here rather than the usual DefaultServeMux/:6060
	// convention (which would mean an unauthenticated port). Not surfaced in
	// the web UI navigation; reached directly by URL (logged-in browser tab,
	// or the "gravinetadmin" session cookie via curl) when actually
	// diagnosing something.
	mux.HandleFunc("/debug/pprof/", s.authed(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", s.authed(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", s.authed(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", s.authed(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", s.authed(pprof.Trace))
	mux.HandleFunc("/api/proxy", s.authed(s.handleProxy))
	return mux
}

// Start begins serving in the background (TLS, self-signed if no cert given).
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.ln = ln

	var cert tls.Certificate
	if s.cfg.TLSCert != "" && s.cfg.TLSKey != "" {
		cert, err = tls.LoadX509KeyPair(s.cfg.TLSCert, s.cfg.TLSKey)
	} else {
		cert, err = s.selfSignedCert()
	}
	if err != nil {
		ln.Close()
		return err
	}
	// Parsed for display only: the Settings > TLS certificate card shows
	// what's actually loaded — custom or self-signed, its CN, its expiry —
	// rather than just echoing whether tls_cert is set in config, which
	// wouldn't catch a config pointing at files that failed to load and fell
	// through to self-signed. (In practice that distinction doesn't arise:
	// the LoadX509KeyPair branch above returns an error and aborts Start
	// entirely on that failure, so this always reflects whichever branch
	// actually ran.) Best-effort: cert.Certificate[0] is the leaf by TLS
	// convention; a parse failure here isn't fatal to serving TLS itself.
	if len(cert.Certificate) > 0 {
		s.tlsCert, _ = x509.ParseCertificate(cert.Certificate[0])
	}
	s.httpSrv = &http.Server{
		Handler:   s.handler(),
		TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		// Bound how long a client may hold a connection while trickling a request,
		// so a slow-loris can't tie up server resources indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	s.metrics = newMetricsCollector(s.be, s.log, metricsHistoryPath(s.configPath))
	go s.metrics.run()
	s.latencyHist = newLatencyCollector(s.be, s.log, latencyHistoryPath(s.configPath))
	go s.latencyHist.run()
	s.bgpRedis = newBGPMeshRedistributor(s)
	go s.bgpRedis.run()
	s.autoBGP = newAutoBGPReconciler(s)
	go s.autoBGP.run()
	// If FRR is on this host, make sure the daemons BGP/BFD need are enabled
	// and actually running (Linux: bgpd/bfdd in /etc/frr/daemons; FreeBSD:
	// rc.conf's frr_enable/frr_daemons plus the /var/lib/frr bootstrap its
	// rc.d script would otherwise only do on a bare `start` — see
	// frr_freebsd.go's ensureFRRDaemonsEnabled), restarting FRR if it had to
	// change anything. Runs in the background so a slow restart can't hold up
	// serving; it's a one-time no-op on every subsequent boot once they're on.
	go ensureFRRDaemonsEnabled(s.log)
	go func() {
		if err := s.httpSrv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			s.log.Errorf("webadmin: serve: %v", err)
		}
	}()
	s.log.Infof("webadmin: listening on https://%s (auth=%s)", s.cfg.Listen, s.auth.Name())
	return nil
}

// EnsureListener starts an additional TLS listener on addr (host:port) serving
// the same admin interface, unless one is already running for that address. The
// primary listener is often bound to loopback for safety, which makes the node
// unreachable for cluster management over its overlay address; binding the
// overlay address here fixes that without exposing the underlay. Idempotent, and
// tolerant of the address already being covered (e.g. a wildcard primary bind).
func (s *Server) EnsureListener(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpSrv == nil {
		return errors.New("web admin not started")
	}
	if s.extraLn == nil {
		s.extraLn = map[string]net.Listener{}
	}
	if _, ok := s.extraLn[addr]; ok {
		return nil // already listening on this address
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.extraLn[addr] = ln
	go func() {
		if err := s.httpSrv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
			s.log.Errorf("webadmin: serve %s: %v", addr, err)
		}
	}()
	s.log.Infof("webadmin: also listening on https://%s (overlay management)", addr)
	return nil
}

// Close stops the server.
func (s *Server) Close() error {
	if s.capture != nil {
		s.capture.stop()
	}
	if s.bgpRedis != nil {
		s.bgpRedis.close()
	}
	if s.autoBGP != nil {
		s.autoBGP.close()
	}
	if s.latencyHist != nil {
		s.latencyHist.close()
	}
	// Was missing entirely before metrics history was persisted: the collector
	// goroutine outlived Close(), which was merely untidy while it held only
	// in-memory state. Now it is also what writes the final checkpoint, so a
	// clean shutdown has to reach it.
	if s.metrics != nil {
		s.metrics.close()
	}
	if s.httpSrv != nil {
		return s.httpSrv.Close()
	}
	return nil
}

// ---- auth / sessions ----

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newSession issues a stateless signed cookie: base64(user|expiry).base64(HMAC).
// Because validity is proven by the signature, not a server-side map, the cookie
// keeps working across a daemon restart (the signing key is persisted).
func (s *Server) newSession(user string) string {
	exp := time.Now().Add(sessionTTL).Unix()
	payload := user + "|" + strconv.FormatInt(exp, 10)
	mac := s.sign(payload)
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(mac)
}

func (s *Server) validSession(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", false
	}
	dot := strings.IndexByte(c.Value, '.')
	if dot < 0 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(c.Value[:dot])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(c.Value[dot+1:])
	if err != nil {
		return "", false
	}
	if !hmac.Equal(sig, s.sign(string(payload))) {
		return "", false
	}
	bar := strings.LastIndexByte(string(payload), '|')
	if bar < 0 {
		return "", false
	}
	exp, err := strconv.ParseInt(string(payload[bar+1:]), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "", false
	}
	// Honor an explicit logout (in-memory; a restart clears the denylist, but
	// logout also clears the browser cookie, so only a captured token replayed
	// after a restart is affected — bounded by the token's expiry).
	s.mu.Lock()
	_, gone := s.revoked[c.Value]
	s.mu.Unlock()
	if gone {
		return "", false
	}
	return string(payload[:bar]), true
}

// sign returns the HMAC-SHA256 of payload under the persisted session key.
func (s *Server) sign(payload string) []byte {
	m := hmac.New(sha256.New, s.signingSecret())
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// signingSecret loads (or generates once) the HMAC key. It is persisted next to
// the TLS cert so sessions survive restarts; without a config path it is
// per-process (sessions then reset on restart, as before).
func (s *Server) signingSecret() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.secret != nil {
		return s.secret
	}
	var path string
	if s.configPath != "" {
		path = filepath.Join(filepath.Dir(s.configPath), "webadmin-session.key")
		if b, err := os.ReadFile(path); err == nil && len(b) >= 32 {
			s.secret = b
			return s.secret
		}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// A guessable signing key means forgeable session cookies — worse than
		// not serving at all. crypto/rand failing on a functioning OS is close
		// to impossible; if it does, fail closed rather than sign with a
		// time-derived (predictable) value.
		s.log.Errorf("webadmin: could not generate a session signing key from crypto/rand: %v; refusing to start authenticated sessions", err)
		panic("webadmin: crypto/rand unavailable for session signing key")
	}
	if path != "" {
		if err := os.WriteFile(path, secret, 0o600); err != nil {
			s.log.Warnf("webadmin: could not persist session key to %s: %v (logins won't survive restart)", path, err)
		} else {
			s.log.Infof("webadmin: session signing key persisted to %s (logins survive restarts)", path)
		}
	}
	s.secret = secret
	return s.secret
}

func (s *Server) authed(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// None of this is safe to cache: config and status can change out from
		// under a client entirely (a reinstall with a fresh config is the most
		// jarring case — without this, a browser could in principle keep
		// showing networks from before the reinstall even after a fresh
		// login, since login here doesn't do a full page navigation and so
		// doesn't get a cache-busting free ride from that).
		w.Header().Set("Cache-Control", "no-store")
		if _, ok := s.validSession(r); ok {
			next(w, r)
			return
		}
		// Managed mode: accept management that arrives over the overlay from a
		// mesh peer that is itself in Manager mode. The source must be a
		// structural overlay address (inside an overlay subnet) — not merely one
		// the registry has heard advertised, so a peer can't poison the registry
		// with an attacker's underlay IP to slip past login. Reaching us on such
		// a source required the mesh PSK, which is the cluster's trust boundary;
		// underlay callers still log in. On top of that, the caller must resolve
		// to a node currently advertising Manager mode (IsManagerAddr) — being
		// Managed no longer means "any mesh peer may manage me," only "any
		// Manager peer may" (see config.Config's Managed/Manager doc comments).
		//
		// The three ways this bypass can fail are distinguished in the response
		// (with the actual observed source address for the address-mismatch and
		// not-a-manager cases) rather than collapsed into one generic message:
		// "this node isn't in managed mode", "the connection didn't look like a
		// genuine overlay one", and "the caller isn't in manager mode" point the
		// operator at three completely different fixes, and a bare "not
		// authenticated" left them unable to tell which applied — the single
		// biggest complaint this endpoint gets from peer-to-peer callers like
		// speedtest.
		reason := "log in instead"
		if s.be.Managed() {
			if ip := remoteIP(r); ip.IsValid() {
				if s.be.OverlayContains(ip) {
					if s.be.IsManagerAddr(ip) {
						next(w, r)
						return
					}
					reason = fmt.Sprintf("the connection arrived from %s, which is a valid overlay address but isn't in manager mode — log in instead", ip)
				} else {
					reason = fmt.Sprintf("the connection arrived from %s, which isn't inside any of this node's overlay subnets — log in instead", ip)
				}
			} else {
				reason = "could not determine the caller's address — log in instead"
			}
		} else {
			reason = "this node is not in managed mode — log in instead"
		}
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "not authenticated: " + reason})
	}
}

// remoteIP parses the connecting peer's IP from RemoteAddr.
func remoteIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ap, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return ap.Unmap()
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ip := clientIP(r)
	if s.noAuth {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the server has no working authentication configured — check the daemon log; " +
				"reinstall with the platform installer (for PAM/Windows login) or set auth_mode=local and add a user with 'gravinet genpass'",
		})
		return
	}
	if s.throttle.Banned(ip) {
		until := s.throttle.BanUntil(ip)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": "too many failed logins; locked out", "retry_after_seconds": int(time.Until(until).Seconds()),
		})
		return
	}
	var req struct{ User, Pass string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	if !s.auth.Authenticate(req.User, req.Pass) {
		s.throttle.Fail(ip)
		s.log.Warnf("webadmin: failed login for %q from %s", req.User, ip)
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials"})
		return
	}
	s.throttle.Reset(ip)
	tok := s.newSession(req.User)
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/",
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	s.log.Infof("webadmin: %q logged in from %s", req.User, ip)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "user": req.User})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Matching handleLogin, which has always checked. A GET here was
		// reachable from an <img> tag on any page the operator visited: the
		// session cookie is SameSite=Strict so nothing got revoked, but the
		// clearing cookie below is honoured on a cross-site response all the
		// same, so an arbitrary web page could log an admin out.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Requiring POST closed the <img>/<script> shape of that; a cross-site
	// form post still reached it, and SameSite does not help, because it
	// governs when a cookie is *sent*, not whether a Set-Cookie is honoured.
	//
	// Sec-Fetch-Site is the check rather than Origin, because it needs no
	// comparison against r.Host: a reverse proxy that rewrites Host would
	// make a correct Origin look wrong and 403 the logout button, and this
	// header carries the browser's own verdict instead. Absent means a
	// non-browser client (curl, a script, an older browser) and is allowed —
	// those are not the ones being tricked, and the peer proxy forwards
	// neither this header nor a cookie. "none" is a user-initiated
	// navigation, which is the operator themselves.
	//
	// This is the last of the cross-site surface here, and it is smaller
	// than v926 implied: every authed() endpoint already refuses a
	// cross-site request, because it needs validSession, which needs the
	// SameSite=Strict cookie the browser withholds. Logout is the exception
	// only because it is deliberately unauthenticated.
	switch r.Header.Get("Sec-Fetch-Site") {
	case "cross-site", "same-site":
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// Record the token as revoked for the rest of its lifetime so a replay
	// within this process is rejected, then clear the browser cookie.
	//
	// Only a token that actually validates is recorded. This used to take
	// whatever was in the cookie: unauthenticated, unsigned, unparsed, and
	// held for sessionTTL. Since /api/logout is deliberately not behind
	// authed() — you must be able to log out with a stale session — that
	// made the map an unauthenticated write primitive. Measured at 1,000
	// requests with junk cookies: 1,000 entries retained for eight hours,
	// keys attacker-chosen and bounded only by MaxHeaderBytes, and the
	// opportunistic sweep below never reaches them because none has expired
	// yet. Requiring a valid signature bounds the map by real logins, which
	// is what it was always meant to hold; revoking a token that was never
	// valid accomplishes nothing anyway.
	if _, ok := s.validSession(r); ok {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			s.mu.Lock()
			if s.revoked == nil {
				s.revoked = map[string]time.Time{}
			}
			now := time.Now()
			for tok, exp := range s.revoked { // opportunistic cleanup
				if now.After(exp) {
					delete(s.revoked, tok)
				}
			}
			s.revoked[c.Value] = now.Add(sessionTTL)
			s.mu.Unlock()
		}
	}
	// Every attribute here mirrors the cookie handleLogin sets, which is what
	// makes this a replacement of that cookie rather than something that
	// happens to share its name. Browsers key on name/domain/path, so the
	// bare form did delete it — but the security argument for keeping the
	// revocation list in memory (see validSession) is precisely "logout also
	// clears the browser cookie", and a clearing cookie written in a
	// different shape from the one it clears is a poor thing to rest that on.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- API ----

// allNetworkIDs is every mesh network this node currently runs, which is empty
// on a node that runs none — the case the node-global firewall exists for.
func (s *Server) allNetworkIDs() []uint64 { return s.be.NetworkIDs() }

func (s *Server) resolveNet(hexID string) (uint64, bool) {
	ids := s.be.NetworkIDs()
	if hexID == "" {
		if len(ids) == 1 {
			return ids[0], true
		}
		return 0, false
	}
	id, err := strconv.ParseUint(hexID, 16, 64)
	return id, err == nil
}

// handlePing is unauthenticated and cheap. It reports liveness and this process's
// boot id so the admin UI can detect a restart by a changed id (robust to the new
// process coming back faster than the poll interval).
func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "boot": s.bootID})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	type netView struct {
		ID       string                  `json:"id"`
		Peers    []mesh.PeerInfo         `json:"peers"`
		Bans     []mesh.BanInfo          `json:"bans"`
		Disabled []mesh.DisabledPeerInfo `json:"disabled_peers"`
		Routes   []mesh.RouteInfo        `json:"routes"`
		Firewall []mesh.FirewallRule     `json:"firewall"`
		// Self is this node's own identity on the network — same shape as a
		// Peers entry — so the admin UI's peers table can show this node
		// alongside the peers it actually connects to (see SelfPeer).
		Self mesh.PeerInfo `json:"self"`
	}
	var out []netView
	for _, id := range s.be.NetworkIDs() {
		fw, _ := s.be.FirewallRules(id)
		self, _ := s.be.SelfPeer(id)
		out = append(out, netView{
			// Zero-padded to the same 16 hex chars every network ID is stored
			// and displayed as elsewhere (config file, /api/config, the web
			// UI). strconv.FormatUint doesn't pad, so an ID with a leading
			// zero nibble would come out short here otherwise — and that
			// mismatch is exactly what let network deletion fail silently
			// (see the comment on Config.NetworkDelete).
			ID:       fmt.Sprintf("%016x", id),
			Peers:    s.be.ListPeers(id),
			Bans:     s.be.ListBans(id),
			Disabled: s.be.DisabledPeers(id),
			Routes:   s.be.Routes(id),
			Firewall: fw,
			Self:     self,
		})
	}
	natClass, natPublic := s.be.NATStatusStrings()
	writeJSON(w, http.StatusOK, map[string]any{"nets": out, "nat_class": natClass, "public": natPublic})
}

// handleConfig returns a read-only, secret-free view of per-network NAT, QoS,
// bandwidth, and addressing for the admin UI. Requires SetConfigPath.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		writeJSON(w, http.StatusOK, map[string]any{"nets": []any{}})
		return
	}
	cfg, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"nets": []any{}, "error": err.Error()})
		return
	}
	type keyMeta struct {
		Slot        int    `json:"slot"`
		Label       string `json:"label"`
		Enabled     bool   `json:"enabled"`
		Set         bool   `json:"set"`
		Expires     string `json:"expires"`
		Distributed bool   `json:"distributed"`
		Notes       string `json:"notes"`
	}
	type cfgNet struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Enabled  bool   `json:"enabled"`
		Notes    string `json:"notes"`
		Subnet4  string `json:"subnet4"`
		Subnet6  string `json:"subnet6"`
		Address4 string `json:"address4"`
		Address6 string `json:"address6"`
		// MTU is the overlay interface MTU, surfaced so Settings > Networks can
		// show and edit it — see handleNetwork's "mtu" op.
		MTU      int                  `json:"mtu"`
		Seeds    config.SeedList      `json:"seeds"`
		Routes   []config.Route       `json:"routes"`
		RouteRej []config.RejectRoute `json:"route_reject"`
		// RoutePrefer mirrors config.Network.RoutePrefer verbatim — the
		// ordered per-prefix origin preference Traffic > Routes' "Preferred
		// peers" card edits. Surfaced alongside RouteRej rather than folded
		// into Routes because it describes routes this node *learns*, not
		// ones it advertises.
		RoutePrefer []config.PreferRoute `json:"route_prefer"`
		// AllowRelay/SelfSeed mirror config.Network's own fields verbatim —
		// see their doc comments. Neither is currently hot-reloadable (see
		// NetworkSetAllowRelay/NetworkSetSelfSeed); a toggle in Settings >
		// Networks needs a restart of this node before it's advertised.
		AllowRelay bool `json:"allow_relay"`
		SelfSeed   bool `json:"self_seed"`
		// Mesh mirrors config.Network.Mesh verbatim (empty or "full" means
		// unrestricted, "partial" means the seed/peer hub-and-spoke
		// restriction) — see its doc comment. Also not hot-reloadable, same
		// restart caveat as AllowRelay/SelfSeed above.
		Mesh string `json:"mesh"`
		// RedistributeBGPRoutes/RedistributeBGPMetric mirror config.Network's
		// own fields verbatim — see its doc comment. Surfaced here (not
		// folded into Routes above) because they're a single per-network
		// selection + metric, not another entry in that list.
		RedistributeBGPRoutes []string   `json:"redistribute_bgp_routes"`
		RedistributeBGPMetric int        `json:"redistribute_bgp_metric"`
		NAT                   config.NAT `json:"nat"`
		QoS                   config.QoS `json:"qos"`
		// Iface is the kernel interface this network's tunnel runs on —
		// its TUNName, or the mesh<N> assigned from its position. Reported
		// because shaping is keyed by interface (v960) and the Shaping page
		// needs to say which network an entry carries, and offer the
		// interfaces that exist, without a second round trip.
		Iface    string              `json:"iface"`
		Firewall config.Firewall     `json:"firewall"`
		Hosts    []config.HostRecord `json:"hosts_advertise"`
		HostsRej []config.HostReject `json:"hosts_reject"`
		DNS      []config.DNSForward `json:"dns_advertise"`
		DNSRej   []config.DNSReject  `json:"dns_reject"`
		Keys     []keyMeta           `json:"keys"`
	}
	var out []cfgNet
	for ni, n := range cfg.Networks {
		id := n.ID
		if v, err := strconv.ParseUint(n.ID, 16, 64); err == nil {
			id = fmt.Sprintf("%016x", v) // zero-padded, matching /api/status (see its comment)
		}
		var keys []keyMeta
		for i, k := range n.Keys {
			keys = append(keys, keyMeta{
				Slot: i, Label: k.Label, Enabled: k.Enabled,
				Set: k.Key != "", Expires: k.Expires, Distributed: k.Distributed, Notes: k.Notes,
			})
		}
		meshMode := "full"
		if n.MeshPartial() {
			meshMode = "partial"
		}
		out = append(out, cfgNet{
			ID: id, Name: n.Name, Enabled: n.Enabled, Notes: n.Notes,
			Subnet4: n.Subnet4, Subnet6: n.Subnet6, Address4: n.Address4, Address6: n.Address6, MTU: n.MTU, Seeds: n.Seeds,
			Routes: n.Routes, RouteRej: n.RouteRej, RoutePrefer: n.RoutePrefer, AllowRelay: n.AllowRelay, SelfSeed: n.SelfSeed, Mesh: meshMode,
			RedistributeBGPRoutes: n.RedistributeBGPRoutes, RedistributeBGPMetric: n.RedistributeBGPMetric,
			NAT: n.NAT, QoS: n.QoS, Iface: cfg.IfaceForNetworkAt(ni), Firewall: n.Firewall, Hosts: n.HostsAdvertise, HostsRej: n.HostsReject,
			DNS: n.DNSAdvertise, DNSRej: n.DNSReject, Keys: keys,
		})
	}
	snmpSupported, _ := service.SNMPSupported()
	lldpSupported, _ := service.LLDPSupported()
	syslogSupported, _ := service.SyslogSupported()
	// tlsSource/tlsCN/tlsNotAfter describe whichever certificate Start()
	// actually loaded (s.tlsCert), not just whether tls_cert is set in
	// config — see that field's own comment for why those always agree in
	// practice. Cert info is best-effort: nil if Start() hasn't run yet
	// (shouldn't happen once the API is serving requests at all) or if
	// parsing it failed, in which case these just come back blank.
	tlsSource := "self-signed"
	if s.cfg.TLSCert != "" {
		tlsSource = "custom"
	}
	var tlsCN, tlsNotAfter string
	if s.tlsCert != nil {
		tlsCN = s.tlsCert.Subject.CommonName
		tlsNotAfter = s.tlsCert.NotAfter.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nets": out, "udp_ports": cfg.UDPPortList(), "tcp_ports": cfg.TCPPortList(), "nat_state_timeout": cfg.NATStateTimeout, "nat": cfg.NAT, "qos": cfg.QoS, "shaping": cfg.Shaping, "shaping_enabled": cfg.ShapingEnabled(), "shaping_kinds": shapingKinds(cfg), "shaping_kernel": kernelShapingBackend(), "firewall": cfg.Firewall, "geoip_lookup": s.cfg.GeoIPEnabled(), "enable_upnp": cfg.EnableUPnP, "ip_forwarding": cfg.ForwardingEnabled(), "disable_redirects": cfg.RedirectsDisabled(), "allow_remote_shell": s.cfg.AllowRemoteShell, "login_ban_max_failures": s.cfg.LoginBan.EffectiveMaxFailures(), "login_ban_seconds": s.cfg.LoginBan.EffectiveBanSeconds(), "tls_source": tlsSource, "tls_common_name": tlsCN, "tls_not_after": tlsNotAfter, "config_history_limit": cfg.EffectiveConfigHistoryLimit(), "config_history_count": config.Count(s.configPath), "shell_supported": ptySupported, "bgp_supported": bgpSupported(), "ipv6ra_supported": ipv6RASupported(), "dhcp_supported": dhcpSupported(), "snmp_supported": snmpSupported, "lldp_supported": lldpSupported, "syslog_supported": syslogSupported, "log_level": s.be.LogLevel(), "log_max_size": cfg.LogMaxSizeString(),
		"worker_threads": cfg.WorkerThreads, "tun_queues": cfg.TunQueues, "tun_queues_supported": tunMultiQueueSupported, "udp_gso": cfg.UDPGSOEnabled(), "udp_gso_supported": udpGSOSupported, "socket_buffer_mb": cfg.SocketBufferMB(), "socket_buffer_max_mb": config.SocketBufferMaxBytes >> 20,
		// Node-global firewall object/service catalog (see Config.FirewallObjects'
		// doc comment) — shared by every network above, not nested under any one
		// of them. The seeded flags let the admin UI populate the well-known
		// catalog exactly once, ever, for this node (see
		// Config.ObjectsCatalogSeeded's doc comment).
		"firewall_objects": cfg.FirewallObjects, "firewall_services": cfg.FirewallServices,
		"firewall_objects_seeded": cfg.ObjectsCatalogSeeded, "firewall_services_seeded": cfg.ServicesCatalogSeeded,
	})
}

func (s *Server) handleBan(w http.ResponseWriter, r *http.Request) {
	var req struct{ Net, Node, Notes string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	id, ok := s.resolveNet(req.Net)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
		return
	}
	if err := s.be.BanNode(id, req.Node, req.Notes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleBanNotes edits the notes on an existing ban this node originated and
// re-floods it to the mesh. Only the origin node can edit its own bans (the
// engine enforces this); the UI only offers the edit on rows this node owns.
func (s *Server) handleBanNotes(w http.ResponseWriter, r *http.Request) {
	var req struct{ Net, Node, Notes string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	id, ok := s.resolveNet(req.Net)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
		return
	}
	if err := s.be.EditBanNotes(id, req.Node, req.Notes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleNetworkReset drops every current peer session on a network and clears
// seed retry backoff, so the engine immediately redials every known peer and
// seed instead of waiting out any existing timeout. It's a live, in-place
// action — no config change and no restart.
func (s *Server) handleNetworkReset(w http.ResponseWriter, r *http.Request) {
	var req struct{ Net string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	id, ok := s.resolveNet(req.Net)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
		return
	}
	if err := s.be.ResetNetwork(id); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePeer enables or disables a peer locally by node id. Disabling is
// local-only (this node refuses to connect to the peer) — unlike a ban, which
// floods mesh-wide. It writes the network's DisabledPeers list in config and
// reloads, so the change applies live (no restart): a newly-disabled peer is
// disconnected immediately and refused on reconnect; a re-enabled peer is
// redialed by the maintenance loop.
func (s *Server) handlePeer(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net, Node, Op, Notes string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	if req.Op != "enable" && req.Op != "disable" && req.Op != "notes" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "op must be enable, disable, or notes"})
		return
	}
	if req.Node == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify peer node id"})
		return
	}
	var err error
	var woken []string
	if req.Op == "notes" {
		err = s.mutateConfig(r, func(cfg *config.Config) error {
			return cfg.PeerSetNotes(req.Net, req.Node, req.Notes)
		})
	} else {
		on := req.Op == "enable"
		err = s.mutateConfig(r, func(cfg *config.Config) error {
			syncSeedNodes(cfg, s.seedOwnersByNet())
			if e := cfg.PeerSetEnabled(req.Net, req.Node, on); e != nil {
				return e
			}
			// The seeds that reach this node move with it, in both
			// directions, so a peer's state and its addresses' states can
			// never disagree about the same decision.
			seeds, e := couplePeerState(cfg, req.Net, req.Node, on)
			woken = seeds
			return e
		})
	}
	// The reload applies the change to the running engine live, so no restart.
	if err == nil && len(woken) > 0 {
		verb := "enabled"
		if req.Op == "disable" {
			verb = "disabled"
		}
		s.editResultNote(w, nil, false, "also "+verb+" seed "+strings.Join(woken, ", "))
		return
	}
	s.editResult(w, err, false)
}

func (s *Server) handleUnban(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Net, Node string
		Force     bool
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	id, ok := s.resolveNet(req.Net)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
		return
	}
	var err error
	if req.Force {
		err = s.be.ForceUnban(id, req.Node)
	} else {
		err = s.be.UnbanNode(id, req.Node)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleFirewall reads and edits this node's rulebase.
//
// Config-first from v957, inverting how this worked through v956. The rules
// used to live in the live mesh engine: the UI read them from it, every edit
// went through it, and its persist hook copied them down into config
// afterwards. That made a running engine — and therefore a mesh network — a
// precondition for writing a firewall rule down at all, which is backwards for
// something whose rules are a statement about packets.
//
// So config is the source of truth and the engine is reloaded from it. The
// engine still owns one thing the config cannot: per-rule hit counters, which
// are live traffic tallies rather than configuration. Those are merged in on
// read, keyed by the stable id config now assigns (see config.FirewallRule.ID)
// and which the engine adopts rather than minting its own.
func (s *Server) handleFirewall(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		cfg, err := config.Load(s.configPath)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": s.firewallRulesWithCounters(cfg)})
		return
	}
	var req struct {
		Net      string
		Op       string
		At       int
		To       int
		IDs      []uint64
		Idxs     []int
		Rule     mesh.FirewallRule
		Objects  []mesh.FirewallObject
		Services []mesh.FirewallService
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}

	// objects / services replace the node-global address-object / service
	// catalog every rule resolves its src/dst/services references against.
	// Still engine-owned and still persisted by the engine's persist hook —
	// unlike the rules, these were never per-network and so were never the
	// thing making a mesh a precondition.
	if req.Op == "objects" || req.Op == "services" {
		var err error
		if req.Op == "objects" {
			err = s.be.SetFirewallObjects(req.Objects)
		} else {
			err = s.be.SetFirewallServices(req.Services)
		}
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": false})
		return
	}

	// mark-objects-seeded / mark-services-seeded record that the admin UI's
	// well-known catalog auto-populate (see ui.go's fwAutoPopulateCatalog) has
	// already run for this node, so it never runs again. The flag has no
	// effect on packet filtering, only on whether the UI's next visit tries to
	// add anything.
	if req.Op == "mark-objects-seeded" || req.Op == "mark-services-seeded" {
		objects := req.Op == "mark-objects-seeded"
		err := s.mutateConfig(r, func(cfg *config.Config) error {
			if objects {
				return cfg.FirewallMarkObjectsCatalogSeeded()
			}
			return cfg.FirewallMarkServicesCatalogSeeded()
		})
		s.editResult(w, err, false)
		return
	}

	// reset-counters is the one op that still goes to the engine: a hit tally
	// is live traffic, not configuration, and there is nothing in the config
	// file to reset. Applied to every network, since the rulebase is one list
	// and a rule may be enforced on several.
	if req.Op == "reset-counters" {
		for _, id := range s.allNetworkIDs() {
			if err := s.be.FirewallResetCounters(id, req.IDs); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": false})
		return
	}

	// Everything else is a config edit. The reload pushes it into whichever
	// engines are running; a node with none simply has nothing to push to,
	// which is the whole point of the change.
	var err error
	switch req.Op {
	case "enable", "disable":
		on := req.Op == "enable"
		err = s.mutateConfig(r, func(cfg *config.Config) error { return cfg.FirewallSetEnabled(on) })
	case "rule-enable", "rule-disable":
		if len(req.IDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no rule id"})
			return
		}
		on := req.Op == "rule-enable"
		err = s.mutateConfig(r, func(cfg *config.Config) error {
			for _, id := range req.IDs {
				if e := cfg.FirewallRuleSetEnabled(id, on); e != nil {
					return e
				}
			}
			return nil
		})
	case "add":
		err = s.mutateConfig(r, func(cfg *config.Config) error {
			return cfg.FirewallRuleAdd(fwRuleToConfig(req.Rule), req.At)
		})
	case "update":
		if len(req.IDs) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "update requires exactly one rule id"})
			return
		}
		err = s.mutateConfig(r, func(cfg *config.Config) error {
			return cfg.FirewallRuleUpdate(req.IDs[0], fwRuleToConfig(req.Rule))
		})
	case "del":
		err = s.mutateConfig(r, func(cfg *config.Config) error { return cfg.FirewallRuleDelete(req.IDs) })
	case "move":
		if len(req.IDs) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "move requires exactly one rule id"})
			return
		}
		err = s.mutateConfig(r, func(cfg *config.Config) error { return cfg.FirewallRuleMove(req.IDs[0], req.To) })
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown op"})
		return
	}
	s.editResult(w, err, false)
}

// fwRuleToConfig converts the wire form into the config form. The id is
// deliberately not carried: add assigns one, and update keeps the existing.
func fwRuleToConfig(r mesh.FirewallRule) config.FirewallRule {
	return config.FirewallRule{
		Disabled: r.Disabled, Action: r.Action, Direction: r.Direction, Proto: r.Proto,
		Src: r.Src, Dst: r.Dst, SrcNegate: r.SrcNegate, DstNegate: r.DstNegate,
		SrcPortMin: r.SrcPortMin, SrcPortMax: r.SrcPortMax,
		DstPortMin: r.DstPortMin, DstPortMax: r.DstPortMax,
		Services: r.Services, ServicesNegate: r.ServicesNegate, Log: r.Log,
		Notes: r.Notes, Scope: r.Scope,
	}
}

// firewallRulesWithCounters returns the configured rules with live hit tallies
// folded in.
//
// A rule enforced on several networks has a counter per network, so they are
// summed: the operator asked one question — how much traffic has this rule
// matched — and one number answers it. A node with no engine running gets the
// rules with zero counters rather than an error, which is what makes the page
// work before any mesh network exists.
func (s *Server) firewallRulesWithCounters(cfg *config.Config) []mesh.FirewallRule {
	pkts := map[uint64]uint64{}
	bytes := map[uint64]uint64{}
	for _, id := range s.allNetworkIDs() {
		live, err := s.be.FirewallRules(id)
		if err != nil {
			continue
		}
		for _, lr := range live {
			pkts[lr.ID] += lr.Packets
			bytes[lr.ID] += lr.Bytes
		}
	}
	out := make([]mesh.FirewallRule, 0, len(cfg.Firewall.Rules))
	for _, r := range cfg.Firewall.Rules {
		out = append(out, mesh.FirewallRule{
			ID: r.ID, Disabled: r.Disabled, Action: r.Action, Direction: r.Direction, Proto: r.Proto,
			Src: r.Src, Dst: r.Dst, SrcNegate: r.SrcNegate, DstNegate: r.DstNegate,
			SrcPortMin: r.SrcPortMin, SrcPortMax: r.SrcPortMax,
			DstPortMin: r.DstPortMin, DstPortMax: r.DstPortMax,
			Services: r.Services, ServicesNegate: r.ServicesNegate, Log: r.Log,
			Notes: r.Notes, Scope: r.Scope,
			Packets: pkts[r.ID], Bytes: bytes[r.ID],
		})
	}
	return out
}

// handleIndex serves the app shell (HTML + embedded JS). Unauthenticated —
// login itself happens inside the page, not before it loads — so this never
// goes through authed()'s Cache-Control: no-store, and until now was the one
// significant response in the whole server without any cache header at all.
// That's the same staleness risk authed()'s own comment already describes
// (a browser keeps showing something from before a change on the server),
// just missed on the one response that matters most for it: this is the
// page that actually delivers the JavaScript doing the asking. A cached copy
// can keep running old client-side logic indefinitely — silently correct
// against an old server, silently wrong against a new one, since every
// /api/* call it makes still reaches the current backend and "succeeds" by
// its own (stale) rules. Explicit no-store here closes that gap the same
// way authed() already closes it for data.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(indexHTML))
}

// handleXtermJS/handleXtermCSS serve the vendored terminal emulator the
// shell feature's frontend loads (see vendor/xterm/VENDORED.md) — static,
// unauthenticated assets at the same trust level as handleIndex's own
// HTML/JS/CSS, versioned by the content itself rather than a query string,
// so a long max-age is safe: a version bump changes vendor_xterm.go (and
// this binary), which serves new bytes at the same URL immediately on
// restart regardless of what a browser cached.
func (s *Server) handleXtermJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Write([]byte(xtermJS))
}

func (s *Server) handleXtermCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Write([]byte(xtermCSS))
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// selfSignedCert returns a stable self-signed certificate: it reuses one
// persisted next to the config if present, otherwise generates one and saves it.
// Persisting keeps the cert identical across restarts, so a browser that already
// trusted it stays connected (and the admin UI's post-restart reconnect works)
// instead of hitting a fresh cert warning every time.
func (s *Server) selfSignedCert() (tls.Certificate, error) {
	certPath, keyPath := s.selfSignedPaths()
	if certPath != "" {
		if c, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
			return c, nil
		}
	}
	certPEM, keyPEM, err := genSelfSignedPEM(s.cfg.Listen)
	if err != nil {
		return tls.Certificate{}, err
	}
	if certPath != "" {
		if werr := os.WriteFile(keyPath, keyPEM, 0o600); werr != nil {
			s.log.Warnf("webadmin: could not persist TLS key to %s: %v (cert will change on restart)", keyPath, werr)
		} else if werr := os.WriteFile(certPath, certPEM, 0o644); werr != nil {
			s.log.Warnf("webadmin: could not persist TLS cert to %s: %v (cert will change on restart)", certPath, werr)
		} else {
			s.log.Infof("webadmin: generated self-signed TLS cert (persisted to %s)", certPath)
		}
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func (s *Server) selfSignedPaths() (certPath, keyPath string) {
	if s.configPath == "" {
		return "", ""
	}
	dir := filepath.Dir(s.configPath)
	return filepath.Join(dir, "webadmin-cert.pem"), filepath.Join(dir, "webadmin-key.pem")
}

func genSelfSignedPEM(listen string) (certPEM, keyPEM []byte, err error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "gravinet-admin"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	if host, _, err := net.SplitHostPort(listen); err == nil {
		if ip := net.ParseIP(host); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if host != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// shapingKinds maps each shaping entry's interface to the mechanism that
// enforces it ("tunnel" or "kernel"), so the Shaping page can say which
// without re-deriving the mesh interface list in the browser.
func shapingKinds(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Shaping))
	for _, s := range cfg.Shaping {
		out[s.Iface] = cfg.ShapingKind(s.Iface)
	}
	return out
}

// kernelShapingBackend reports whether this host can program kernel shaping,
// and with what. Empty means it cannot — a non-Linux host, or one without
// iproute2 installed — which the page shows on the affected rows rather than
// leaving a configured rate looking as though it were in force.
//
// Probed per request rather than cached: tc can be installed while gravinet
// is running, and a page that kept saying "unavailable" until a restart would
// be wrong for as long as it took someone to notice.
func kernelShapingBackend() string {
	m, err := tcshape.New()
	if err != nil {
		return ""
	}
	return m.Backend()
}
