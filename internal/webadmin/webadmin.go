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
)

// Backend is the slice of the engine the admin UI drives.
type Backend interface {
	NetworkIDs() []uint64
	Interfaces() []mesh.IfaceInfo
	NATStatusStrings() (string, string)
	ListPeers(networkID uint64) []mesh.PeerInfo
	ListBans(networkID uint64) []mesh.BanInfo
	DisabledPeers(networkID uint64) []mesh.DisabledPeerInfo
	Routes(networkID uint64) []mesh.RouteInfo
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
	reload             func() error // optional: re-applies config live after an edit
	cfgMu              sync.Mutex   // serializes config-file read-modify-write

	upg *UpgradeCtl // mesh-distributed binary upgrades; nil when not configured

	httpSrv *http.Server
	ln      net.Listener
	extraLn map[string]net.Listener // additional listeners (e.g. overlay addresses), by address

	bootID string // random per-process id; lets the admin UI detect a restart

	version string // gravinet build version (for the About tab); set via SetVersion
	commit  string // gravinet build commit

	metrics *metricsCollector // CPU/mem/interface time series for the Metrics tab
	capture *captureState     // live packet capture for the Capture tab
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
func (s *Server) mutateConfig(fn func(*config.Config) error) error {
	if s.configPath == "" {
		return fmt.Errorf("config path not set")
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return config.WithLock(s.configPath, func() error {
		cfg, err := config.Load(s.configPath)
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
		if s.reload != nil {
			if err := s.reload(); err != nil {
				s.log.Warnf("webadmin: reload after edit failed: %v", err)
			}
		}
		return nil
	})
}

// New builds a Server, choosing the authenticator from the config auth mode.
func New(cfg config.WebAdmin, be Backend, log *logx.Logger) *Server {
	var auth Authenticator
	noAuth := false
	switch strings.ToLower(cfg.AuthMode) {
	case "pam", "windows", "system":
		if a, ok := systemAuthenticator(cfg.PAMService, cfg.AllowUsers, log); ok {
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
	maxF := lb.MaxFailures
	if maxF <= 0 {
		maxF = 3
	}
	win := lb.Window()
	if win <= 0 {
		win = time.Minute
	}
	ban := lb.Ban()
	if ban <= 0 {
		ban = 15 * time.Minute
	}
	return &Server{
		cfg:      cfg,
		be:       be,
		log:      log,
		auth:     auth,
		noAuth:   noAuth,
		throttle: ratelimit.New(maxF, win, ban, lb.Coalesce()),
		bootID:   randomBootID(),
		capture:  newCaptureState(),
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
	mux.HandleFunc("/api/restart", s.authed(s.handleRestart))
	mux.HandleFunc("/api/cluster", s.authed(s.handleCluster))
	mux.HandleFunc("/api/loglevel", s.authed(s.handleLogLevel))
	mux.HandleFunc("/api/logsize", s.authed(s.handleLogSize))
	mux.HandleFunc("/api/managed", s.authed(s.handleManaged))
	mux.HandleFunc("/api/manager", s.authed(s.handleManager))
	// Upgrade surface: local-only, per-node. Every handler here enforces its
	// own session check (upgradeLocalOnly) rather than relying on authed()'s
	// Managed/Manager bypass — see that function's doc comment.
	mux.HandleFunc("/api/upgrade", s.authed(s.handleUpgradeHome))
	mux.HandleFunc("/api/upgrade/local-apply", s.authed(s.handleUpgradeLocalApply))
	mux.HandleFunc("/api/upgrade/stage", s.authed(s.handleUpgradeStage))
	mux.HandleFunc("/api/upgrade/stage-source", s.authed(s.handleUpgradeStageSource))
	mux.HandleFunc("/api/upgrade/rollback", s.authed(s.handleUpgradeRollback))
	mux.HandleFunc("/api/routeadv", s.authed(s.handleRouteAdv))
	mux.HandleFunc("/api/keepalive", s.authed(s.handleKeepalive))
	mux.HandleFunc("/api/peertimeout", s.authed(s.handlePeerTimeout))
	mux.HandleFunc("/api/port", s.authed(s.handlePort))
	mux.HandleFunc("/api/tcpport", s.authed(s.handleTCPPort))
	mux.HandleFunc("/api/natstate", s.authed(s.handleNATState))
	mux.HandleFunc("/api/geoip", s.authed(s.handleGeoIPSetting))
	mux.HandleFunc("/api/shell/setting", s.sessionOnly(s.handleShellSetting))
	mux.HandleFunc("/api/shell/ws", s.sessionOnly(s.handleShellWS))
	mux.HandleFunc("/api/shell/hijack", s.authed(s.handleShellHijack))
	mux.HandleFunc("/api/exempt", s.authed(s.handleExempt))
	mux.HandleFunc("/api/logs", s.authed(s.handleLogs))
	mux.HandleFunc("/api/logs/clear", s.authed(s.handleLogsClear))
	mux.HandleFunc("/api/readme", s.authed(s.handleReadme))
	mux.HandleFunc("/api/license", s.authed(s.handleLicense))
	mux.HandleFunc("/api/getting-started", s.authed(s.handleGettingStarted))
	mux.HandleFunc("/api/about", s.authed(s.handleAbout))
	mux.HandleFunc("/api/metrics", s.authed(s.handleMetrics))
	mux.HandleFunc("/api/capture/interfaces", s.authed(s.handleCaptureInterfaces))
	mux.HandleFunc("/api/capture/start", s.authed(s.handleCaptureStart))
	mux.HandleFunc("/api/capture/stop", s.authed(s.handleCaptureStop))
	mux.HandleFunc("/api/capture/clear", s.authed(s.handleCaptureClear))
	mux.HandleFunc("/api/capture/packets", s.authed(s.handleCapturePackets))
	mux.HandleFunc("/api/capture/pcap", s.authed(s.handleCapturePcap))
	mux.HandleFunc("/api/speedtest/source", s.authed(s.handleSpeedtestSource))
	mux.HandleFunc("/api/speedtest/sink", s.authed(s.handleSpeedtestSink))
	mux.HandleFunc("/api/speedtest/run", s.authed(s.handleSpeedtestRun))
	mux.HandleFunc("/api/localroutes", s.authed(s.handleLocalRoutes))
	mux.HandleFunc("/api/localhosts", s.authed(s.handleLocalHosts))
	mux.HandleFunc("/api/localdns", s.authed(s.handleLocalDNS))
	mux.HandleFunc("/api/latency", s.authed(s.handleLocalLatency))
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
	s.metrics = newMetricsCollector(s.be, s.log)
	go s.metrics.run()
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
	// Clear the browser cookie and record the token as revoked for the rest of
	// its lifetime so a replay within this process is rejected.
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
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- API ----

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
		ID       string               `json:"id"`
		Name     string               `json:"name"`
		Enabled  bool                 `json:"enabled"`
		Notes    string               `json:"notes"`
		Subnet4  string               `json:"subnet4"`
		Subnet6  string               `json:"subnet6"`
		Address4 string               `json:"address4"`
		Address6 string               `json:"address6"`
		Seeds    config.SeedList      `json:"seeds"`
		Routes   []config.Route       `json:"routes"`
		RouteRej []config.RejectRoute `json:"route_reject"`
		NAT      config.NAT           `json:"nat"`
		QoS      config.QoS           `json:"qos"`
		Throttle config.Throttle      `json:"throttle"`
		Firewall config.Firewall      `json:"firewall"`
		Hosts    []config.HostRecord  `json:"hosts_advertise"`
		HostsRej []config.HostReject  `json:"hosts_reject"`
		DNS      []config.DNSForward  `json:"dns_advertise"`
		DNSRej   []config.DNSReject   `json:"dns_reject"`
		Keys     []keyMeta            `json:"keys"`
	}
	var out []cfgNet
	for _, n := range cfg.Networks {
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
		out = append(out, cfgNet{
			ID: id, Name: n.Name, Enabled: n.Enabled, Notes: n.Notes,
			Subnet4: n.Subnet4, Subnet6: n.Subnet6, Address4: n.Address4, Address6: n.Address6, Seeds: n.Seeds,
			Routes: n.Routes, RouteRej: n.RouteRej,
			NAT: n.NAT, QoS: n.QoS, Throttle: n.Throttle, Firewall: n.Firewall, Hosts: n.HostsAdvertise, HostsRej: n.HostsReject,
			DNS: n.DNSAdvertise, DNSRej: n.DNSReject, Keys: keys,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nets": out, "primary_port": cfg.PrimaryPort, "tcp_fallback_port": cfg.TCPFallbackPortValue(), "tcp_fallback_disabled": !cfg.TCPFallbackEnabled(), "extra_listen_ports": cfg.ExtraListenPorts, "extra_tcp_listen_ports": cfg.ExtraTCPListenPorts, "nat_state_timeout": cfg.NATStateTimeout, "geoip_lookup": s.cfg.GeoIPEnabled(), "allow_remote_shell": s.cfg.AllowRemoteShell, "shell_supported": ptySupported, "log_level": s.be.LogLevel(), "log_max_size": cfg.LogMaxSizeString(),
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
	if req.Op == "notes" {
		err = s.mutateConfig(func(cfg *config.Config) error {
			return cfg.PeerSetNotes(req.Net, req.Node, req.Notes)
		})
	} else {
		on := req.Op == "enable"
		err = s.mutateConfig(func(cfg *config.Config) error {
			return cfg.PeerSetEnabled(req.Net, req.Node, on)
		})
	}
	// The reload applies the change to the running engine live, so no restart.
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

func (s *Server) handleFirewall(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		id, ok := s.resolveNet(r.URL.Query().Get("net"))
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
			return
		}
		rules, err := s.be.FirewallRules(id)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
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
	// enable/disable operate on the config by name (no engine rule id needed).
	if req.Op == "enable" || req.Op == "disable" {
		on := req.Op == "enable"
		err := s.mutateConfig(func(cfg *config.Config) error { return cfg.FirewallSetEnabled(req.Net, on) })
		// The reload applies the toggle to the running engine live (the firewall
		// object always exists), so no restart is needed.
		s.editResult(w, err, false)
		return
	}
	// objects / services replace the node-global address-object / service
	// catalog every network's rules resolve their src/dst/services references
	// against — not scoped to req.Net (there's no per-network catalog left to
	// scope to), applied live and persisted by the engine's persist hook, like
	// rule edits.
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
	// already run for this node, so it never runs again — a plain config
	// mutation like enable/disable above, not an engine op: the flag itself
	// has no effect on packet filtering, only on whether the UI's next visit
	// tries to add anything. Node-global, like the catalog itself — not
	// scoped to req.Net. The client calls this right after an "objects"/
	// "services" save that filled any gaps (or immediately, if there were
	// none to fill) — sequenced after that save completes, so the fresh
	// catalog it just wrote is what's on disk by the time this reads and
	// re-saves the file, not a stale copy from before the objects/services
	// write (both this and the engine's persist hook take the same
	// per-config-path lock; see mutateConfig's comment).
	if req.Op == "mark-objects-seeded" || req.Op == "mark-services-seeded" {
		objects := req.Op == "mark-objects-seeded"
		err := s.mutateConfig(func(cfg *config.Config) error {
			if objects {
				return cfg.FirewallMarkObjectsCatalogSeeded()
			}
			return cfg.FirewallMarkServicesCatalogSeeded()
		})
		s.editResult(w, err, false)
		return
	}
	// rule-enable / rule-disable toggle a single rule's enabled flag by its
	// engine ID; we find it by matching ID in the live rulebase, then apply to config by index.
	if req.Op == "rule-enable" || req.Op == "rule-disable" {
		on := req.Op == "rule-enable"
		if len(req.IDs) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no rule id"})
			return
		}
		ruleID := req.IDs[0]
		id, ok := s.resolveNet(req.Net)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
			return
		}
		rules, err := s.be.FirewallRules(id)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		idx := -1
		for i, fr := range rules {
			if fr.ID == ruleID {
				idx = i
				break
			}
		}
		if idx < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "rule not found"})
			return
		}
		err = s.mutateConfig(func(cfg *config.Config) error {
			return cfg.FirewallRuleSetEnabled(req.Net, idx, on)
		})
		s.editResult(w, err, false)
		return
	}
	id, ok := s.resolveNet(req.Net)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "specify net"})
		return
	}
	var err error
	switch req.Op {
	case "add":
		// Apply to the engine; this is live immediately and the engine's persist
		// hook writes it back to config (the same path the control socket uses).
		// Persisting again here would duplicate the rule.
		if _, err = s.be.FirewallAdd(id, req.Rule, req.At); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	case "del":
		if err = s.be.FirewallDelete(id, req.IDs); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	case "move":
		if len(req.IDs) != 1 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "move requires exactly one rule id"})
			return
		}
		if err = s.be.FirewallMove(id, req.IDs[0], req.To); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	case "reset-counters":
		// Empty IDs resets every rule's hit tally.
		if err = s.be.FirewallResetCounters(id, req.IDs); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown op"})
		return
	}
	// Changes are live in the engine immediately and persisted to config by the
	// engine's persist hook. No restart needed.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart": false})
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
