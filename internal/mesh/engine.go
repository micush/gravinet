// Package mesh ties the underlay transport and the overlay interface together:
// it runs the PSK-authenticated, X25519 handshake, manages per-peer sessions,
// and moves encrypted packets between the TUN device and the network. Step 3
// establishes direct point-to-point tunnels to configured seeds; mesh-wide
// gossip and relaying arrive in later roadmap steps.
package mesh

import (
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gravinet/internal/config"
	"gravinet/internal/crypto"
	"gravinet/internal/hosts"
	"gravinet/internal/logx"
	"gravinet/internal/protocol"
	"gravinet/internal/ratelimit"
	"gravinet/internal/resolver"
	"gravinet/internal/transport"
)

// Device is the overlay interface contract the engine depends on. *tun.Device
// satisfies it; tests substitute an in-memory fake.
type Device interface {
	Read(p []byte) (int, error)
	Write(p []byte) (int, error)
	Name() string
	MTU() int
	Close() error
	AddIPv4(addr netip.Addr, prefix int) error
	AddIPv6(addr netip.Addr, prefix int) error
	AddRoute(prefix netip.Prefix, metric int) error
	DelRoute(prefix netip.Prefix, metric int) error
	// IfIndex returns this device's kernel interface index, so a
	// physical-gateway lookup for a full-tunnel peer-bypass route (see
	// fulltunnel.go) never mistakes this tun's own installed default route
	// for the real one. Platforms without a real gateway backend yet return
	// a clear error (see internal/tun/gateway_unsupported.go) rather than a
	// number nothing yet consumes correctly.
	IfIndex() (int32, error)
}

// Sender abstracts the transport for testability.
type Sender interface {
	Send(to netip.AddrPort, payload []byte) error
}

// fallbackDialer is an optional capability of the attached Sender (implemented by
// transport.Dual): it opens a TCP/TLS fallback connection to a peer when UDP
// can't reach it. The engine type-asserts its Sender to this; a transport
// without it (UDP-only) simply never falls back.
type fallbackDialer interface {
	DialFallback(to netip.AddrPort) error
	HasFallback(to netip.AddrPort) bool
}

// NetSpec describes one overlay the engine should serve.
// RejectRule is one advertised-route reject. By default it matches only the exact
// prefix; Inclusive makes it also match every more-specific route contained in it.
type RejectRule struct {
	Prefix    netip.Prefix
	Inclusive bool
}

type NetSpec struct {
	ID   uint64
	Name string
	Keys *crypto.KeySet
	// KeyLabels maps each key in Keys (by derived ID) to its config label,
	// purely for display in the admin UI (e.g. which key label authenticated a
	// given peer's session). Never consulted for authentication itself.
	KeyLabels map[crypto.KeyID]string
	// KeyExpires maps each key in Keys (by derived ID) to its config expiry
	// (RFC3339, "" = never), the expiry counterpart to KeyLabels. Used only by
	// the propagated-key reconciliation (forgetConfiguredKeys) to tell whether
	// config has fully caught up with a mesh-learned key's current expiry, not
	// for authentication itself (key expiry enforcement reads config directly).
	KeyExpires map[crypto.KeyID]string
	Dev        Device
	// NewDevice, if set, creates a fresh overlay Device for this network with
	// the same name/MTU as Dev. The data-plane supervisor calls it to rebuild a
	// tun the OS tore down out from under us (driver reset, `ip link del`, VM or
	// Wi-Fi churn). Injected by the caller (cmd/gravinet) so the engine stays
	// decoupled from internal/tun and tests keep substituting a fake. Nil
	// disables interface recreation entirely — a read error then ends the data
	// plane until process restart, exactly as before this existed.
	NewDevice  func() (Device, error)
	Subnet4    netip.Prefix
	Subnet6    netip.Prefix
	Self4      netip.Addr // this node's overlay v4 (advertised in handshakes)
	Self6      netip.Addr
	Seeds      []netip.AddrPort
	TCPSeeds   []netip.AddrPort // seeds to dial over the TCP/TLS fallback directly (cold bootstrap when UDP is blocked)
	AllowRelay bool
	Ban        config.BanPolicy
	// SeedTCPPort is an optional fallback port to dial Seeds on at cold start when
	// their advertised port isn't known yet (from a join token). 0 = assume own port.
	SeedTCPPort int

	// Route redistribution (step 6).
	Routes      []netip.Prefix       // CIDRs this node advertises to the mesh
	RouteMetric map[netip.Prefix]int // per-route metric (0 if unset)
	RouteReject []RejectRule         // advertised routes matching these are ignored

	// Hosts sync (step 6).
	HostsSync bool
	HostsPath string        // override; defaults to the OS hosts file
	HostsTTL  time.Duration // drop a host entry after this much silence

	// DNS conditional-forwarding sync. Iface is the tun interface entries get
	// registered against (Linux) or the ownership tag (all platforms) — see
	// internal/resolver. Off by default, unlike HostsSync.
	DNSSync bool
	DNSTag  string        // ownership tag passed to internal/resolver; defaults to the network ID hex
	DNSTTL  time.Duration // drop a forward entry after this much silence

	// SearchDomains are plain suffixes this node's OS resolver should try
	// automatically on an unqualified (single-label) query — the search-
	// domain counterpart to AdvDNS's routing domains. Local-only: not
	// gossiped, hot-reloadable like AdvDNS/DNSReject. Linux and Windows only
	// (see internal/resolver's package doc for why macOS/FreeBSD can't do
	// this without reaching outside gravinet's own interface).
	SearchDomains []string

	// SearchLearned additionally promotes every domain this node currently
	// accepts a conditional forward for — its own AdvDNS entries *and*
	// whatever it has learned from peers and not locally rejected — to a
	// search suffix, not just the domains already listed in SearchDomains.
	// On by default (see config.DNSSync.DisableSearchDomains, which this is
	// the negation of); not hot-reloadable (same restart-required tier as
	// DNSSync itself, since it changes what this node is willing to trust
	// gossip for, not just which already-trusted domains are listed).
	SearchLearned bool

	// Distributed ban lifetime (step 8). A live origin refreshes its bans; a
	// dead origin's bans lapse after BanTTL. 0 uses the default.
	BanTTL time.Duration

	// Broadcast/multicast storm control (step 8).
	BroadcastPPS int
	MulticastPPS int
	StormBurst   int

	// Bandwidth throttling (step 9), in bytes/sec; 0 = unlimited.
	ThrottleUp    int // egress (shaped)
	ThrottleDown  int // ingress (policed)
	ThrottleBurst int // token-bucket burst in bytes; 0 = default
	ThrottleQueue int // egress queue capacity in bytes; 0 = default

	// QoS (step 10): prioritises traffic within the egress shaper. nil = none.
	QoS *classifier

	// Firewall (step 11): initial rulebase (empty = default allow-all). The
	// object/service catalog rules resolve against is node-global, not part
	// of this per-network spec — see Options.FirewallObjects' doc comment.
	FirewallEnabled bool
	FirewallRules   []FirewallRule
	FirewallExempts []FirewallExempt

	// NAT (step 12): address translation rules (empty = none).
	NATEnabled      bool
	NAT             []NATRuleSpec
	NATStateTimeout time.Duration // conntrack idle TTL; 0 = default

	// AdvHosts are custom name -> IP records this node advertises mesh-wide for
	// every peer's hosts file (beyond the automatic peer-hostname entries).
	AdvHosts []HostRecordSpec

	// HostReject is a local filter: hostnames whose peer-advertised records this
	// node refuses to write into its hosts file (the host-record analog of
	// RouteReject). Matched case-insensitively.
	HostReject []string

	// AdvDNS are conditional-forwarding domains this node advertises mesh-wide
	// (the DNS analog of AdvHosts).
	AdvDNS []DNSForwardSpec

	// DNSReject is a local filter: forwarding domains whose peer-advertised
	// records this node refuses to apply to its OS resolver (the DNS analog of
	// HostReject). Matched case-insensitively.
	DNSReject []string

	// DisabledPeers is a local-only set of peer node IDs this node refuses to
	// connect to. Local (never flooded), unlike bans. Hot-reloadable.
	DisabledPeers []string

	// PeerNotes are local-only, operator-authored notes about specific peers,
	// keyed by node id. Purely informational (never flooded, never consulted
	// for anything but display), the notes counterpart to DisabledPeers.
	// Hot-reloadable.
	PeerNotes map[string]string
}

// Options construct an Engine.
type Options struct {
	NodeID   string
	Hostname string
	Nets     []NetSpec
	Log      *logx.Logger
	Managed  bool   // advertise this node as remotely manageable
	Manager  bool   // advertise this node as able to manage other Managed peers
	WebPort  uint16 // web-admin port advertised for management over the overlay
	// RouteAdvInterval is how often this node re-floods its own redistributed
	// routes. Zero means the built-in default (10s).
	RouteAdvInterval time.Duration

	// KeepaliveInterval is how often this node sends a NAT keepalive
	// (ctrlPing) to each connected peer — also what RTT tracking (relay
	// scoring) samples ride on. Zero means the built-in default (10s); the
	// minimum honored value is 1s.
	KeepaliveInterval time.Duration

	// PeerTimeout is how long a session may go without any received traffic
	// before it's considered dead and torn down — this is what governs how
	// long a gone-silent peer keeps showing as connected in the peers
	// table. Zero means the built-in default (20s); an explicit value below
	// the current keepalive interval is clamped up to it, since timing a
	// session out before a single keepalive round trip could complete would
	// cause constant unnecessary reconnection thrashing rather than faster
	// failure detection.
	PeerTimeout time.Duration

	// UnderlayMTU caps a single UDP datagram on the wire; overlay packets too big
	// to fit are fragmented and reassembled by the peer. Zero means the default.
	UnderlayMTU int

	// UnderlayMTUMax is the ceiling for per-peer path-MTU discovery. When greater
	// than UnderlayMTU, the engine probes each peer's path for the largest working
	// datagram between the two; when <= UnderlayMTU, discovery is off.
	UnderlayMTUMax int

	// TCPFallbackPort is the port peers are assumed to listen on for the TCP/TLS
	// fallback (default 443). When UDP can't reach a peer, the engine dials this
	// port at the peer's address. Zero disables outbound fallback.
	TCPFallbackPort int
	// PrimaryPort is this node's own primary UDP listen port, used to build the
	// host candidates it advertises (see localcand.go). 0 = UDP disabled.
	PrimaryPort int
	// ExtraTCPPorts/ExtraUDPPorts are this node's own additional listen ports
	// (config extra_tcp_listen_ports/extra_listen_ports), advertised to peers
	// via the handshake/gossip so they can try them too when the primary
	// doesn't get through — see the engine fields of the same name.
	ExtraTCPPorts []uint16
	ExtraUDPPorts []uint16

	// TunWorkers is how many goroutines process outbound packets pulled from
	// the TUN device per network — see tunLoop's doc comment for why this
	// exists (the read itself is unavoidably serial; everything after it
	// isn't). Zero => runtime.NumCPU()-1, min 1 — same convention as, and
	// meant to be given, the same value as config.WorkerThreads/transport's
	// Workers, so the TUN and UDP worker pools scale together under the
	// same knob rather than one respecting it and the other silently
	// picking its own count.
	TunWorkers int

	// FirewallObjects / FirewallServices seed the node-global firewall
	// object/service catalog (see config.Config.FirewallObjects' doc
	// comment) before any of Nets' initial network states are built, so
	// every one of them — not just ones added later via AddNetwork — starts
	// with the real catalog instead of empty. Shared by every network on
	// this node; not part of NetSpec, which is otherwise the per-network
	// counterpart to this node-global Options struct.
	FirewallObjects  []FirewallObject
	FirewallServices []FirewallService
}

// Engine is the running mesh for one node.
type Engine struct {
	nodeID   string
	hostname string
	log      *logx.Logger

	// bypassMu/bypassRefs back the full-tunnel peer-bypass host routes (see
	// fulltunnel.go): a reference count per address, not a boolean, because
	// more than one independent tracker can need a route to the same
	// address at once — a seed and the live peer session it eventually
	// becomes, or (in principle) two different networks with a peer at the
	// same underlay address — and the route must stay up until every
	// tracker that asked for it has released it, not just the first one
	// that's done. Global (per-Engine), not per-netState: the OS routing
	// table is one shared resource regardless of which overlay a reference
	// came from, so per-network bookkeeping could let one network's cleanup
	// delete a route another network's peer still depends on.
	bypassMu   sync.Mutex
	bypassRefs map[netip.Addr]bypassRef

	managed atomic.Bool // advertised "managed" mode; toggled live
	manager atomic.Bool // advertised "manager" mode (can manage other managed peers); toggled live
	// bgpASN is this node's current effective BGP AS number, advertised in
	// every handshake and pushed live to already-connected peers exactly the
	// way managed/manager are — see SetBGPASN and hsPayload.BGPASN's doc
	// comment for what it's for and why it's scoped to direct peers only.
	bgpASN  atomic.Uint32
	webPort uint16 // advertised web-admin port

	routeAdvNs    atomic.Int64 // route re-advertisement interval (ns); tuned live
	keepaliveNs   atomic.Int64 // NAT keepalive interval (ns); tuned live
	peerTimeoutNs atomic.Int64 // dead-session timeout (ns); tuned live

	// maxInnerFrag is the floor: the largest overlay-packet slice that fits in one
	// underlay datagram at the configured floor MTU, used until a peer's path-MTU
	// discovery raises it (see pmtu.go).
	maxInnerFrag int
	fragSeq      atomic.Uint32 // per-packet fragment-group id generator

	// reflexive holds, per reporting peer (by node id), the underlay address that
	// peer observes this node at. Aggregated to learn our own public endpoint and
	// classify our NAT (see reflexive.go). Shared across networks: every peer sees
	// the same underlay socket.
	reflexiveMu sync.Mutex
	reflexive   map[string]reflexiveObs

	// fwCatalogMu / fwObjects / fwServices hold the node-global firewall
	// object/service catalog (see config.Config.FirewallObjects' doc
	// comment) — one shared list, not one copy per network. Every network's
	// *firewall instance gets the same objs/svcs pushed into it whenever
	// SetFirewallObjects/SetFirewallServices runs; this pair is the
	// authoritative copy that (a) FirewallObjectsList/FirewallServicesList
	// read back, and (b) newNetState reads when bringing up a network — at
	// boot (Options.FirewallObjects/FirewallServices seeds this before any
	// initial network is built) or later via AddNetwork — so a network
	// never starts out with an empty catalog just because it wasn't the one
	// SetFirewallObjects/Services happened to be called against most
	// recently.
	fwCatalogMu sync.Mutex
	fwObjects   []FirewallObject
	fwServices  []FirewallService

	// localUnderlay is the source address the kernel currently uses to reach the
	// underlay. When it changes (e.g. switching Wi-Fi/5G), the path MTU to every
	// peer may have shrunk, so we re-run discovery. Guarded by underlayMu.
	// underlayRefNode is the node ID of the peer currently used as the fixed
	// probe destination (see connectedEndpoint) — comparisons only count as a
	// real change when the reference peer is unchanged; see checkUnderlayChange.
	//
	// defaultPathSrc is a SECOND, peer-independent roam signal: the source
	// address the kernel would use to reach a fixed off-subnet anchor (see
	// defaultPathSourceIP). The peer-anchored localUnderlay check above can
	// miss a roam entirely when every peer's stored underlay endpoint has
	// become unroutable on the new network — precisely the case where a roam
	// left the mesh fully partitioned (every peer "no reply") and recovery
	// was needed most. The anchor lookup doesn't depend on any peer's state,
	// so it still fires then. Guarded by underlayMu.
	underlayMu      sync.Mutex
	localUnderlay   netip.Addr
	underlayRefNode string
	defaultPathSrc  netip.Addr
	haveDefaultPath bool
	// defaultGW tracks the physical default gateway (address + egress
	// interface index) as a THIRD roam signal, independent of both the peer-
	// anchored source and the anchor source-IP above. Roaming between two
	// networks that hand out the same local IP — e.g. two APs on the same
	// 192.168.x.y subnet, or the same SSID re-joined with the same DHCP
	// lease — leaves defaultPathSrc identical, so neither the anchor nor the
	// peer signal fires, yet the underlay genuinely changed (different
	// gateway/L2, old peer endpoints now unreachable). The gateway address or
	// its interface index almost always differs across such a move, so this
	// catches the same-source-IP roam the other two miss. Guarded by
	// underlayMu.
	defaultGW         netip.Addr
	defaultGWIf       int32
	haveDefaultGW     bool
	lastUnderlayCheck time.Time

	// pmtuFloor/pmtuCeil bound per-peer path-MTU discovery (outer datagram bytes).
	// When ceil<=floor, discovery is disabled and every peer stays at the floor.
	pmtuFloor int
	pmtuCeil  int
	pmtuSeq   atomic.Uint32 // probe id generator

	// tunWorkers is the size of each network's outbound worker pool — see
	// tunLoop's doc comment. Set once in NewEngine from Options.TunWorkers
	// (defaulted there if zero), read-only afterward.
	tunWorkers int

	// fallbackPort is this node's own TCP/TLS fallback listen port, advertised to
	// peers and used as the default port to dial a peer whose advertised port we
	// don't yet know. Zero disables outbound fallback. Atomic: updated live on a
	// fallback-port config change and read from the handshake/gossip builders.
	fallbackPort atomic.Int64
	// primaryPort is this node's own primary UDP listen port. Needed to build
	// host candidates (localEndpoints): an interface address is only dialable
	// paired with the port we're actually listening on, and unlike the TCP
	// fallback port nothing else in the engine had any reason to know it —
	// a peer's UDP port is normally learned by observing where its packets
	// come from, but that's exactly the mechanism host candidates exist to
	// work around. 0 means UDP is disabled (the '-' port setting).
	primaryPort atomic.Int64
	// loopDrops counts underlay datagrams of gravinet's own that were read
	// back off a TUN device and dropped by processOutbound's loop guard —
	// see isUnderlayLoop. loopWarnUnix is the unix time of the last WARN
	// logged about it, so a storm arriving at line rate produces one line
	// per ~10s instead of one per packet.
	loopDrops    atomic.Uint64
	loopWarnUnix atomic.Int64
	// localCands is the published host-candidate set (see localcand.go).
	// Refreshed off the handshake path by refreshLocalCandidates and read
	// atomically by localEndpoints, which buildHSInit calls under ns.mu — the
	// enumeration itself must never happen there.
	localCands atomic.Pointer[[]netip.AddrPort]
	// ownAddrs is every address this host holds, on any interface — including
	// the virtual bridges and down interfaces we refuse to advertise. Used to
	// reject a peer's host candidate that names an address we occupy ourselves,
	// which could only ever dial this daemon. See localcand.go's isOwnAddr.
	ownAddrs atomic.Pointer[map[netip.Addr]bool]
	// haveV4/haveV6 record whether this host holds any routable address of each
	// family, refreshed alongside ownAddrs. A send or dial into a family we
	// cannot source is a guaranteed ENETUNREACH — see canSourceFamily.
	haveV4           atomic.Bool
	haveV6           atomic.Bool
	lastLocalCandSig atomic.Pointer[string]
	// extraTCPPorts/extraUDPPorts are this node's own additional advertised
	// listen ports (config extra_tcp_listen_ports/extra_listen_ports),
	// advertised alongside fallbackPort/the primary UDP port so peers can
	// try them too — see ensureFallback (TCP) and the UDP seed-injection in
	// handshake_engine.go. atomic.Pointer rather than fallbackPort's
	// atomic.Int64 since these are lists, not scalars; same live-update
	// contract. A nil pointer (the zero value) and an empty-but-non-nil one
	// both mean "no extra ports" — callers use loadPortList to normalize.
	extraTCPPorts atomic.Pointer[[]uint16]
	extraUDPPorts atomic.Pointer[[]uint16]

	tr Sender

	mu       sync.RWMutex
	sessions map[uint32]*peerSession // by our inbound index (DATA/keepalive demux)
	nextIdx  uint32

	// nets is copy-on-write: the data path reads a snapshot via Load() with no
	// lock, while AddNetwork/RemoveNetwork swap in a new map under netMu. This
	// keeps per-packet routing lock-free while letting networks come and go live.
	nets  atomic.Pointer[map[uint64]*netState]
	netMu sync.Mutex // serializes nets writers (copy-on-write)

	persist func(networkID uint64) // optional: persist a network's live changes to config

	// onSuspendResume, if set, is called (at most effectively once, since it's
	// expected to tear the process down for a clean restart) when the
	// maintenance loop detects the host slept and woke — see maintLoop's
	// suspend/resume detection and SetSuspendResumeHook's doc for why
	// in-process patch-up (onResume) isn't a complete fix on its own.
	onSuspendResumeOnce sync.Once
	onSuspendResume     func()

	// onUnderlayChange, if set, is called (also at most once, for the same
	// tear-down-for-a-restart reason) when checkUnderlayChange detects this
	// host's own underlay source address moved — a Wi-Fi/cellular roam. It
	// feeds the same clean-restart path as onSuspendResume; see
	// SetUnderlayChangeHook. startedAt anchors the startup-grace guard in
	// notifyUnderlayChange that keeps a link flapping right after boot from
	// spinning the service.
	onUnderlayChangeOnce sync.Once
	onUnderlayChange     func()
	startedAt            time.Time

	reloadMu sync.Mutex // serializes ReloadRuntime swaps

	stop chan struct{}
	wg   sync.WaitGroup
}

// SetPersistHook installs a callback invoked after live mutations (e.g. firewall
// edits from the web UI or control socket) so they can be written back to the
// config file.
func (e *Engine) SetPersistHook(fn func(networkID uint64)) { e.persist = fn }

// SetSuspendResumeHook installs a callback invoked the first time the
// maintenance loop detects the host has slept and woken (see maintLoop's
// wall-clock-vs-monotonic check). onResume already does what it can from
// inside the engine — aging sessions so they're re-dialed, resetting path-MTU
// discovery, and re-asserting the overlay interface's address and routes —
// but a real sleep/wake cycle can also invalidate state this package doesn't
// own at all: the underlay UDP socket, the TUN/utun device itself, or
// firewall/NAT rules the OS silently dropped. None of that can be rebuilt
// in-place from here. The intended callback is therefore a full, clean
// process restart (see cmd/gravinet's selfRestart): everything gets torn down
// and recreated from scratch exactly as it would on a manual restart, without
// requiring an operator (or an external wake-hook script) to notice and do it
// themselves. Called at most once per detected event via sync.Once, since a
// mesh with multiple networks would otherwise fire it once per network on the
// same underlying wake.
func (e *Engine) SetSuspendResumeHook(fn func()) { e.onSuspendResume = fn }

// underlayRestartGrace is how long after startup the underlay-change restart
// hook stays muted. A genuine roam normally lands well into a session, so
// muting the first window costs almost nothing — while it stops a link that
// flaps right after boot (or right after a previous underlay-change restart)
// from becoming a restart loop, since the fresh process would otherwise
// re-detect a change seconds in and fire again. checkUnderlayChange's own
// first-observation rebase already swallows the very first reading; this
// covers a real change that lands a few seconds into the new process's life.
const underlayRestartGrace = 45 * time.Second

// SetUnderlayChangeHook installs a callback invoked the first time
// checkUnderlayChange sees this host's own underlay source address change (a
// Wi-Fi/cellular roam), once the process has been up past underlayRestartGrace.
// Like the suspend/resume hook, the intended callback is a full, clean process
// restart (cmd/gravinet's selfRestart): a roam can leave peers pinned to
// endpoints on the network we just left, sockets bound to a source that's gone,
// and OS routes pointing at the old gateway — state a from-scratch restart
// rebuilds reliably where in-process patch-up (resetAllPMTU + reassertOSState)
// does not. Called at most once per engine lifetime via sync.Once.
func (e *Engine) SetUnderlayChangeHook(fn func()) { e.onUnderlayChange = fn }

// notifyUnderlayChange invokes the underlay-change hook, if set, at most once
// for this engine's lifetime — but only after the process has been up past
// underlayRestartGrace, so a flapping link can't spin the service. A skip
// (still within the grace window) deliberately does NOT consume the one-shot:
// a later, post-grace change can still fire it.
func (e *Engine) notifyUnderlayChange() {
	if e.onUnderlayChange == nil {
		return
	}
	if up := time.Since(e.startedAt); up < underlayRestartGrace {
		e.log.Debugf("mesh: underlay changed but process only up %s; deferring restart-on-underlay-change to avoid a restart loop", up.Round(time.Second))
		return
	}
	e.onUnderlayChangeOnce.Do(e.onUnderlayChange)
}

func (e *Engine) notifyChange(networkID uint64) {
	if e.persist != nil {
		e.persist(networkID)
	}
}

// notifySuspendResume invokes the suspend/resume hook, if set, exactly once
// for the lifetime of this engine — see SetSuspendResumeHook's doc.
func (e *Engine) notifySuspendResume() {
	if e.onSuspendResume == nil {
		return
	}
	e.onSuspendResumeOnce.Do(e.onSuspendResume)
}

// NetworkIdentity returns a network's current (possibly peer-learned) name and
// subnets, so the persist hook can write learned values back to config.
// NetworkSelfAddrs returns this node's current overlay addresses on a network,
// so the persist hook can pin them in config for restart stability.
// Hostname returns this node's configured hostname (may be empty).
func (e *Engine) Hostname() string { return e.hostname }

func (e *Engine) NetworkSelfAddrs(networkID uint64) (self4, self6 netip.Addr, ok bool) {
	ns := e.network(networkID)
	if ns == nil {
		return netip.Addr{}, netip.Addr{}, false
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.self4, ns.self6, true
}

func (e *Engine) NetworkIdentity(networkID uint64) (name string, sub4, sub6 netip.Prefix, ok bool) {
	ns := e.network(networkID)
	if ns == nil {
		return "", netip.Prefix{}, netip.Prefix{}, false
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return ns.name, ns.subnet4, ns.subnet6, true
}

type netState struct {
	spec     NetSpec
	throttle *ratelimit.Throttle

	// hsSeen defends HS_INIT against replay within the clock-skew window that
	// freshTimestamp alone permits (±clockSkew). A legitimate initiator picks a
	// fresh ephemeral X25519 key per attempt, so its 32-byte public key is a
	// natural single-use nonce; we record each accepted one with an expiry and
	// reject a duplicate seen before it lapses. Bounded in size by
	// maxHSSeen with oldest-eviction so a flood can't grow it without limit.
	// Guarded by hsSeenMu (separate from mu: it's touched on the handshake hot
	// path and never needs the main lock).
	hsSeenMu sync.Mutex
	hsSeen   map[string]time.Time // ephemeral-pubkey -> expiry

	mu          sync.RWMutex
	byNode      map[string]*peerSession
	routes4     map[netip.Addr]*peerSession
	routes6     map[netip.Addr]*peerSession
	fwd         atomic.Pointer[fwdSnap]
	pending     map[uint32]*pendingHS        // by initiator index (idxI)
	seeds       []netip.AddrPort             // mutable; grows as peers are learned
	tcpSeeds    []netip.AddrPort             // explicit TCP/TLS-fallback seeds to dial directly (ns.mu)
	seedBackoff map[netip.AddrPort]time.Time // don't retry a seed before this time
	// seedFallback records, per seed, the specific fallback address
	// ensureFallback last resolved and dialed for it (same IP, a different
	// port). Used by connectedTo/install to recognize "this seed's peer is
	// now connected via its fallback path" precisely — matched against this
	// one resolved address, not by IP alone, which would wrongly treat any
	// other peer sharing that IP (e.g. several nodes behind one NAT gateway,
	// or in tests, several local peers all on 127.0.0.1) as satisfying this
	// seed too.
	seedFallback map[netip.AddrPort]netip.AddrPort
	// seedOwner records, for a seed entry, the node ID it's known to belong to
	// — populated only where that's cheaply known at add time (currently:
	// gossip-learned endpoints, which carry a node ID alongside the address).
	// A seed with no entry here has an unknown owner and is left alone by
	// install()'s pruning; this is the safe default; whatever is here is only
	// ever additive precision, never assumed.
	seedOwner map[netip.AddrPort]string
	// explicitSeed marks which addresses came from the operator's own config
	// (NetSpec.Seeds, at network construction or ReloadRuntime's config-seed
	// merge — see AddExplicitSeed) rather than being discovered dynamically
	// via gossip (AddSeedFor) or a fallback dial's own bookkeeping (AddSeed
	// from dialFallbackCandidate/ban.go's unban re-dial). explicitSeedNode is
	// the node-keyed counterpart addSeed promotes an entry into the moment a
	// node ID becomes known for an address already in this map (via
	// seedOwner, from either gossip or a direct connection) — node-keyed
	// rather than address-keyed so the priority follows the node even after
	// it roams onto a different address, the same reason seedOwner itself is
	// node-precise rather than address-precise. seedOwnerNeedsUpgrade uses
	// explicitSeedNode to skip directUpgradeInterval's throttle for a node
	// the operator explicitly configured: unlike a peer only ever learned
	// about through gossip, an explicit seed is a specific, deliberate
	// statement "this address should be directly reachable" — worth
	// retrying at initSeedTick's normal pace, not backed off to once every
	// 5 minutes just because a relay happens to be covering for it.
	explicitSeed     map[netip.AddrPort]bool
	explicitSeedNode map[string]bool
	// hostCand records which seeds are host candidates (a peer's self-declared
	// LAN address — see localcand.go), as opposed to an endpoint someone
	// observed. install()'s pruning must leave these alone: its premise is
	// "this address is a stale guess at where the peer is, superseded by the
	// address it's actually at," which is simply false here. A host candidate
	// is *by definition* an address other than the observed endpoint — that's
	// the entire point of it — so the prune matched every single one and
	// deleted it on every re-handshake, moments after it was learned. Gossip
	// then re-delivered it, it was added again, and the next handshake deleted
	// it again. Nothing could dial an address that never survived a full tick.
	// Also the reason a candidate would log as "learned" over and over: it
	// genuinely was new each time, because it kept being destroyed. Entries
	// here are never removed, so a candidate that gets swept as dead and later
	// re-gossiped doesn't re-announce itself as though it were news.
	hostCand map[netip.AddrPort]bool
	// hostCandDead records host candidates that were given their fair chance
	// (hostCandGrace) and never once connected. Without it, sweepDeadSeeds
	// evicts a dud and the very next gossip re-adds it, forever: learnPeers
	// re-delivers every peer's full candidate list on every interval, so a
	// candidate that cannot possibly work would be re-dialed for the life of
	// the process. Cleared whenever this node's *own* candidate set changes
	// (refreshLocalCandidates), since that means our own attachment to the
	// network moved and what was unreachable a moment ago may not be now.
	hostCandDead map[netip.AddrPort]bool
	// upgradeNodeAt is the last time an upgrade handshake was launched toward a
	// node, keyed by node id rather than by seed. A peer owns many seeds and any
	// of them can trigger an upgrade; without a per-node gate they all fire at
	// once, every tick. See seedOwnerNeedsUpgrade.
	upgradeNodeAt map[string]time.Time
	// fallbackAttempt records when ensureFallback last dialed each seed, so a
	// retry is paced instead of re-issued on every init tick. initSeedTick calls
	// ensureFallback for every cooling-down seed each tick; with host candidates
	// persisting there can be dozens, and a dial is the most expensive thing in
	// the loop (socket + TLS handshake + goroutine). The first attempt for a
	// seed is always immediate — only retries wait.
	fallbackAttempt map[netip.AddrPort]time.Time
	// seedFirstSeen records when each seed entry first entered ns.seeds, so
	// sweepDeadSeeds (control.go) can tell "still deserves a fair chance"
	// apart from "has been retried for a long time and never once worked."
	seedFirstSeen map[netip.AddrPort]time.Time
	// directUpgradeAttempt records, per seed, the last time initLoop tried a
	// fresh handshake toward it *despite* its owner already having a live
	// (but relay-only) session — see initLoop's own comment. Deliberately
	// separate from seedBackoff: that map's short cooldown (15s) exists to
	// notice a genuinely offline seed coming back quickly, whereas this one
	// gates a purely opportunistic upgrade attempt against a peer that's
	// already working fine over a relay, so it uses a much longer interval
	// (directUpgradeInterval) — retrying every 15s would add real traffic
	// and log noise for no urgency at all.
	directUpgradeAttempt map[netip.AddrPort]time.Time
	// everConnected records every underlay address (a seed's own, or one of
	// its resolved TCP/TLS fallbacks — see seedFallback) that has ever backed
	// a successfully installed session, for the life of this process. This is
	// what actually lets sweepDeadSeeds distinguish "never once worked" (safe
	// to give up on) from "worked before, just not connected at this exact
	// instant" (a live peer mid-reconnect after a ban, a restart on their
	// end, a network blip, or anything else merely temporary) — install()
	// sets an entry the moment a session completes and nothing ever clears
	// it, so a seed that has proven reachable at least once is never
	// auto-pruned by staleness again, no matter how long the current outage
	// lasts.
	everConnected map[netip.AddrPort]bool
	// dialing tracks fallback addresses with a DialFallback currently in
	// flight, so concurrent ensureFallback calls for the same resolved
	// address (e.g. from several stale duplicate seed entries for one peer,
	// all processed in the same initLoop tick) don't all race past the
	// already-connected checks and each independently dial+log.
	dialing map[netip.AddrPort]bool
	nodes   map[string]*nodeInfo // learned node registry (by node id)
	// relayRefusedLog/relayDeclinedLog throttle the two relay-refusal warnings
	// (logRelayRefused, keyed by target; logRelayDeclined, keyed by src\x00dst)
	// to one per relayRefusedLogEvery. Both report persistent misconfiguration
	// that tryRelays/onRelay would otherwise re-hit on every tick or every
	// forwarded packet.
	relayRefusedLog  map[string]time.Time
	relayDeclinedLog map[string]time.Time

	lastGossip     time.Time
	lastGossipSig  string    // content signature of the last full peer-list broadcast (see peerListSig)
	lastGossipFull time.Time // when that broadcast happened, regardless of whether content changed
	lastKeepalive  time.Time

	// pendingPeerAdds tracks in-flight redundant retransmission of single-peer
	// ctrlPeerAdd announcements: nodeID -> remaining resend attempts. See
	// announcePeerChange's doc comment for why a changed peer is re-flooded a
	// few times over the next few maintenance ticks instead of sent once.
	pendingPeerAdds map[string]int

	// Overlay addressing (mutable; may be assigned dynamically at runtime).
	self4        netip.Addr
	self6        netip.Addr
	subnet4      netip.Prefix
	subnet6      netip.Prefix
	name         string     // local label; learned from peers if we joined by id only
	assigning    bool       // a DAD/assignment pass is in flight
	dadCandidate netip.Addr // address currently being probed
	dadDefended  bool       // set if a peer defended dadCandidate

	// Data-plane supervision (see dataplane.go). liveDev is the current overlay
	// device, swapped atomically when the interface is rebuilt so the hot
	// Read/Write paths never see a torn interface value; it's seeded from
	// spec.Dev in newNetState and read via ns.dev(). dpMu guards the small
	// rebuild state machine.
	liveDev             atomic.Pointer[Device]
	dpMu                sync.Mutex
	dpState             int       // dpHealthy | dpRebuilding | dpClosing
	dpRebuilds          int       // count of successful rebuilds (diagnostics)
	dpLastLog           time.Time // rate-limit for repeated rebuild-failure logs
	dpLastRouteReassert time.Time // last defensive base-route re-add (dataplane reconcile)

	// Distributed control plane (step 6).
	bans          map[string]*banRecord   // key = origin|target
	disabledPeers map[string]bool         // local-only blocklist of node IDs (not flooded)
	peerNotes     map[string]string       // local-only, operator-authored notes by node ID (not flooded)
	redist        []routeEntry            // redistributed routes (next-hop = origin)
	knownRoute    map[string]bool         // dedup for route floods (origin|prefix)
	learnedHosts  map[string]*learnedHost // custom hosts records learned from peers (origin|name)
	learnedDNS    map[string]*learnedDNS  // conditional-forward records learned from peers (origin|domain)

	osMu     sync.Mutex           // serializes OS route-table ops + osMetric + demotedGatewayMetric
	osMetric map[netip.Prefix]int // metric each prefix is currently installed with
	// demotedGatewayMetric records, per full-tunnel prefix (0.0.0.0/0 or
	// ::/0), the metric this network's pre-existing physical default route
	// had before demotePhysicalDefaultRoute (fulltunnel.go) deprioritized
	// it to make room for gravinet's own — populated on every platform
	// where routeDemotionNeeded is true, which as of v317 is every
	// platform gravinet has a real gateway backend for, Linux included.
	// restorePhysicalDefaultRoute consults and clears this once the mesh's
	// own default route is withdrawn again, so the physical route doesn't
	// stay pinned at its demoted metric permanently.
	demotedGatewayMetric map[netip.Prefix]int
	// physicalGW caches, per address family (syscall.AF_INET or
	// syscall.AF_INET6), the physical gateway/interface physicalGateway
	// (fulltunnel.go) resolved for this network's current full-tunnel
	// activation. Guarded by osMu, same as the OS-state fields above it —
	// see physicalGateway's doc comment for why this exists: a routine
	// re-resolve-from-the-live-table on every bypass-route acquisition,
	// which is what this replaced, stops working the moment
	// demotePhysicalDefaultRoute has actually removed the physical route
	// from that table (every routeDemotionNeeded platform, as of v320).
	// Cleared on full-tunnel withdrawal so the next activation resolves
	// fresh rather than reusing a gateway from however long ago
	// full-tunnel was last on.
	physicalGW   map[int]physicalGWCache
	lastRouteAdv time.Time
	lastHostAdv  time.Time
	lastDNSAdv   time.Time

	// fullTunnel gates the peer-bypass host-route machinery in
	// fulltunnel.go: when true, every direct peer session gets a live /32
	// (or /128) host route through the real physical gateway, keeping the
	// mesh's own underlay traffic out of a full-tunnel default route
	// accepted on this network. False (the default) makes
	// syncPeerBypassRoute an inert no-op everywhere it's called from the
	// session lifecycle — so adding those call sites doesn't change any
	// existing behavior on its own; something has to opt a network in.
	fullTunnel atomic.Bool

	// seedBypassMu/seedBypassHeld track which addresses this network's own
	// seed lists (ns.seeds, ns.tcpSeeds) currently hold a full-tunnel
	// bypass-route reference for (see syncSeedBypassRoutes) — separate from
	// Engine.bypassRefs itself, which is the shared, ref-counted source of
	// truth across every tracker (seeds here, peer sessions in
	// syncPeerBypassRoute). This is just this one tracker's own memory of
	// what *it* is responsible for releasing.
	seedBypassMu   sync.Mutex
	seedBypassHeld map[netip.Addr]bool
	lastHostsSig   string // debounce hosts-file rewrites
	lastDNSSig     string // debounce OS resolver (conditional-forwarding) rewrites
	lastDNSErr     string // last DNS-sync failure, so an unchanged one isn't re-shouted every tick
	lastPeerSig    string // debounce peer-cache persistence

	// Hot-reloadable route redistribution. advRoutes is the set this node floods
	// to the mesh; advReject is the set of advertised routes to ignore. Both swap
	// atomically on reload (like egress/ingress/nat/keys), so adding or removing a
	// redistributed route applies live without a restart.
	advRoutes atomic.Pointer[[]netip.Prefix]
	advReject atomic.Pointer[[]RejectRule]
	advHosts  atomic.Pointer[[]hostRecord]         // custom hosts records this node advertises
	advDNS    atomic.Pointer[[]dnsForward]         // conditional-forward domains this node advertises
	advMetric atomic.Pointer[map[netip.Prefix]int] // metric for each advertised route

	// bgpRoutes is this node's current BGP-into-mesh redistribution set (see
	// config.Network.RedistributeBGPRoutes) — the CIDRs pulled from FRR's RIB and
	// gossiped to this network's peers, each carrying the single metric the
	// whole batch shares. Deliberately separate state from advRoutes/
	// advMetric above rather than merged into them: advRoutes is swapped
	// wholesale on every config reload (reloadRoutes, driven by
	// ReloadRuntime), which runs for any config edit at all, not just a
	// routes-related one — folding a live BGP RIB into that same pointer
	// would mean an unrelated config change (renaming this network, say)
	// could race a BGP poll and clobber whichever updated last. Kept apart,
	// each side only ever touches its own state; reloadBGPRoutes does its
	// own diff-and-flood exactly the way reloadRoutes does for the
	// config-driven set, and advertiseRoutes (flooding the full current set
	// to a newly-connected peer) is what brings the two together, at read
	// time, into what actually gets sent.
	bgpRoutes atomic.Pointer[bgpRedistSet]

	advHostReject atomic.Pointer[[]string] // hostnames whose learned records this node refuses (lowercased)
	advDNSReject  atomic.Pointer[[]string] // forward domains whose learned records this node refuses (lowercased)

	// searchDomains are this node's own OS-resolver search domains for this
	// network — plain suffixes tried automatically for an unqualified query,
	// as opposed to advDNS's routing domains (which only match fully-
	// qualified queries). Local-only: unlike every other advX/adv-reject pair
	// here, this is never gossiped to peers, so there's no learned/reject
	// counterpart — it's a property of how *this* node's resolver behaves,
	// not something peers need to agree on.
	searchDomains atomic.Pointer[[]string]

	banTTL         time.Duration
	lastBanRefresh time.Time
	lastFWFQDN     time.Time // last firewall FQDN-object re-resolution
	bcast          *tokenBucket
	mcast          *tokenBucket

	// Hot-reloadable runtime components. Read on the data path via Load(); a
	// config reload swaps them via ReloadRuntime. nil = feature disabled.
	egress  atomic.Pointer[shaper]      // up-throttle + QoS (nil = unlimited)
	ingress atomic.Pointer[tokenBucket] // down-throttle (nil = unlimited)

	fw  *firewall                // packet filter (always non-nil; empty = allow-all)
	nat atomic.Pointer[natTable] // address translation (nil = none)

	// Live authentication key set. Read by the handshake via Load(); a config
	// reload swaps it via ReloadRuntime, so add/import/enable/disable/delete of
	// keys take effect without a restart. Existing sessions keep their derived
	// keys; only new handshakes use the updated set.
	keys atomic.Pointer[crypto.KeySet]

	// keyLabels maps a configured key's derived ID to its config label, purely
	// for display (the engine itself never trusts or compares on labels). Swapped
	// alongside keys on every reload that rebuilds them, so the peers view can
	// show which key label authenticated each session (peerSession.keyID).
	keyLabels atomic.Pointer[map[crypto.KeyID]string]

	// propagatedKeys holds keys this node learned via mesh key-rotation
	// propagation (ctrlKeyAdd), keyed by derived ID. They are also folded into
	// the live `keys` set on receipt; this map retains their label/expiry
	// metadata so the persist hook can write them into config (so they survive a
	// restart and appear in the admin UI). Guarded by mu.
	propagatedKeys map[crypto.KeyID]propagatedKey

	// retractedKeys holds the IDs of keys this node has been told (via a
	// flooded ctrlKeyDel) to stop trusting, that it still holds locally. The
	// persist hook removes the matching config slot (config.KeyDelete, which
	// refuses to strip a network's last enabled key) and the entry is then
	// forgotten once the key is actually gone from the live set — see
	// forgetAppliedRetractions. Guarded by mu.
	retractedKeys map[crypto.KeyID]bool

	// Per-network goroutine lifecycle. done is closed to stop just this
	// network's loops (tunLoop/initLoop/maintLoop/runDAD); wg tracks them so
	// RemoveNetwork can wait for a clean teardown without stopping the engine.
	done chan struct{}
	wg   sync.WaitGroup
}

type banRecord struct {
	target      string // banned node id
	origin      string // node id that issued the ban
	notes       string
	atNano      int64
	expiresNano int64          // ban lapses after this; refreshed by a live origin
	endpoint    netip.AddrPort // last-known underlay endpoint, for fast re-dial on unban
	hostname    string         // banned node's hostname, captured at ban time
}

type routeEntry struct {
	origin   string
	prefix   netip.Prefix
	metric   int
	lastSeen time.Time // last time the origin (re-)advertised this route
}

// nodeInfo is what we know about a node in the mesh, whether or not we currently
// hold a session to it. Used for gossip propagation and (step 6) hosts sync.
type nodeInfo struct {
	nodeID   string
	hostname string
	overlay4 netip.Addr
	overlay6 netip.Addr
	endpoint netip.AddrPort
	managed  bool
	manager  bool // node advertised Manager mode (see config.Config.Manager)
	// bgpASN mirrors peerSession's field of the same name — the
	// gossip/handshake-learned copy for a node that isn't (or isn't
	// currently) a direct peer. Not itself propagated through the wider
	// peer-list gossip (see hsPayload.BGPASN's doc comment); kept here
	// purely for consistency with managed/manager rather than as something
	// anything currently reads back out.
	bgpASN  uint32
	webPort uint16
	tcpPort uint16
	// extraTCPPorts/extraUDPPorts mirror peerSession's fields of the same
	// name — this is the gossip/handshake-learned copy for a node that
	// isn't (or isn't currently) a direct peer.
	extraTCPPorts []uint16
	extraUDPPorts []uint16
	lastSeen      time.Time
}

type peerSession struct {
	sess      *crypto.Session
	recvMu    sync.Mutex
	localIdx  uint32 // index the peer puts in packets to us
	remoteIdx uint32 // index we put in packets to the peer

	mu       sync.Mutex
	endpoint netip.AddrPort // peer underlay (updates on NAT roaming)
	// allowRelay/relayKnown are the peer's advertised willingness to act as an
	// intermediary for other peers' traffic (hsPayload.AllowRelay, from its
	// config allow_relay). relayKnown is false for a peer too old to advertise
	// it at all, which bestRelay treats optimistically (assume willing) so a
	// mixed-version mesh behaves exactly as it did before the field existed.
	allowRelay bool
	relayKnown bool
	// localEndpoints are the peer's advertised host candidates — see
	// hsPayload.LocalEndpoints. Kept on the session so buildPeerList can
	// re-gossip them onward, which is how a node learns the LAN address of a
	// peer it has never directly spoken to.
	localEndpoints []netip.AddrPort
	relay          *peerSession // if non-nil, reach this peer through this relay session
	// bypassAddr is the address (if any) this session currently has a
	// full-tunnel peer-bypass host route installed for — see fulltunnel.go.
	// Tracked so a roam or teardown knows exactly what to withdraw, without
	// re-deriving it (endpoint may have already moved on by teardown time).
	// Guarded by mu, same as endpoint.
	bypassAddr netip.Addr

	nodeID   string
	hostname string
	overlay4 netip.Addr
	overlay6 netip.Addr
	net      *netState
	keyID    crypto.KeyID // which PSK authenticated this session (for live key retirement)
	managed  bool         // peer advertised itself as remotely manageable
	manager  bool         // peer advertised Manager mode (can manage other Managed peers)
	// bgpASN is the peer's current effective BGP AS number, as advertised in
	// its handshake and kept fresh by ctrlClusterNotify — see
	// hsPayload.BGPASN's doc comment. 0 means "no BGP configured there" (or
	// a peer too old to advertise it at all — the two are indistinguishable,
	// same as any other value this struct learns only from a handshake
	// field that didn't always exist).
	bgpASN  uint32
	webPort uint16 // peer's web-admin port (for management over the overlay)
	tcpPort uint16 // peer's advertised TCP/TLS fallback port (0 = none/unknown)
	// extraTCPPorts/extraUDPPorts are the peer's additional advertised listen
	// ports (config extra_tcp_listen_ports/extra_listen_ports on their end),
	// tried alongside tcpPort/endpoint when the primary doesn't get through —
	// see ensureFallback and the UDP seed-injection in handshake_engine.go.
	extraTCPPorts []uint16
	extraUDPPorts []uint16
	lastRx        time.Time
	established   time.Time // when this session was installed (see install()); resets on every reconnect

	reportedMu sync.Mutex
	reported   map[string]bool // node ids this peer advertised (relay candidate info)

	reasmMu sync.Mutex // guards reasm (fragment reassembly buffers)
	reasm   map[uint32]*fragReasm

	effMTU  atomic.Int32 // discovered underlay MTU for this peer (outer datagram bytes)
	maxFrag atomic.Int32 // largest overlay slice per datagram at effMTU (send hot path)

	// Fragmentation/reassembly diagnostics (atomic; surfaced per-peer in the UI so
	// an "it hangs" report can be localized: clean counters here mean the loss is
	// upstream of the mesh, climbing reasmDrop/fragSendDrop point at the path).
	fragsSent    atomic.Uint64 // fragment datagrams handed to the transport
	fragSendDrop atomic.Uint64 // overlay packets we failed to send (EMSGSIZE / too many pieces)
	fragsRcvd    atomic.Uint64 // fragment datagrams accepted for reassembly
	reasmOK      atomic.Uint64 // overlay packets fully reassembled and delivered
	reasmDrop    atomic.Uint64 // reassembly groups dropped incomplete (evicted, expired, inconsistent)
	spoofDrop    atomic.Uint64 // inbound packets dropped: source address not owned by this peer (anti-spoofing)
	pmtuMu       sync.Mutex    // guards pmtu
	pmtu         *pmtuState    // path-MTU discovery state (see pmtu.go)

	// Round-trip time to this peer, derived from the existing ctrlPing/
	// ctrlPong NAT keepalive (sendKeepalive, every keepaliveInterval) rather
	// than a dedicated probe — it's a real end-to-end measurement over
	// whatever this session's actual path currently is (direct, or already
	// relayed: ctrlPing/ctrlPong travel inside the per-peer encrypted
	// session like any other control message, so a relayed peer's RTT here
	// is the true you-through-the-relay-to-them figure once connected).
	// pingSentNanos is armed just before a ping goes out and read (not
	// cleared) when the matching pong lands; rttNanos holds the most recent
	// sample, 0 meaning none yet. Used by relay.go to score candidates —
	// see bestRelay — and surfaced per-peer via PeerInfo.RTTMs.
	pingSentNanos atomic.Int64
	rttNanos      atomic.Int64
}

type pendingHS struct {
	idxI      uint32
	eph       *crypto.Ephemeral
	keyCursor int // which slot we're currently trying
	endpoint  netip.AddrPort
	started   time.Time

	relay      *peerSession // send the handshake through this relay (nil = direct)
	targetNode string       // relayed handshake: destination node id
}

const (
	handshakeRetry   = 2 * time.Second
	handshakeBackoff = 5 * time.Second
	clockSkew        = 120 * time.Second

	gossipInterval = 10 * time.Second // how often we check whether to share our peer list
	// gossipFullRefresh is the baseline resend floor for broadcastGossip: even
	// with nothing changed, re-flood at this cadence so a peer that missed an
	// earlier change (a dropped packet, a brief reconnect) self-heals without
	// needing its own trigger. It's the backstop for total loss of a
	// peerAddResends burst, so its length is the worst-case staleness window
	// that backstop leaves on the table — kept to a few minutes (a couple
	// times peerTimeout/managedPeerTTL, not an order of magnitude beyond
	// them) rather than stretched further for bandwidth savings, since that
	// window is exactly what you'd be staring at while debugging a peer that
	// "looks wrong" for no obvious reason. See peerListSig's doc comment for
	// why most ticks skip the resend entirely instead of using this as the
	// cadence.
	gossipFullRefresh = 180 * time.Second
	// peerAddResends is how many extra times announcePeerChange re-floods a
	// single changed peer's ctrlPeerAdd after the initial send, once per
	// maintenance tick. These control messages ride over UDP with no ACK or
	// retry of their own, so a lone send has no protection against an
	// isolated dropped datagram; a few redundant copies spread a few seconds
	// apart make that loss scenario require several independent drops in a
	// row instead of one, without needing to build a real ack/retry protocol
	// for what's normally a rare, small message.
	peerAddResends   = 2
	seedRetryBackoff = 15 * time.Second // unreachable seed cooldown
	maintInterval    = 5 * time.Second  // maintenance loop tick
	// suspendSkew is the excess of wall-clock over monotonic elapsed between two
	// maintenance ticks that we treat as a host suspend (the monotonic clock
	// freezes while asleep). Well above normal scheduling jitter and NTP steps,
	// and below the default peer timeout so we reconnect before a stale session
	// would even be noticed — an operator who's lowered PeerTimeout below this
	// (see mesh.Options.PeerTimeout) trades away that particular margin
	// knowingly, the same way lowering it below a few missed keepalives
	// already trades away tolerance for ordinary packet loss.
	suspendSkew = 30 * time.Second

	defaultKeepaliveInterval = 10 * time.Second // NAT keepalive cadence
	defaultPeerTimeout       = 20 * time.Second // drop a session after this much silence

	defaultRouteAdvInterval = 10 * time.Second // route re-advertisement cadence

	defaultBanTTL = 24 * time.Hour // distributed ban lifetime without refresh
	banRefresh    = 8 * time.Hour  // a live origin re-floods its bans this often
)

// directUpgradeInterval throttles how often initLoop retries a seed whose
// owner is already connected, but only via relay — see initLoop's doc
// comment for why this exists and why it's much longer than
// seedRetryBackoff: this isn't chasing a broken connection, it's an
// opportunistic check on one that's already working. A relay fallback is
// usually caused by something that doesn't resolve itself quickly (real NAT
// type incompatibility) or that resolves almost immediately (a single
// dropped handshake packet) — there's little value in the minutes-scale
// range between "fast" and "slow", so this sits on the order of a few
// minutes rather than being tuned finer. A var, not a const, so tests can
// shorten it — see fallbackHandshakeGrace for the same pattern.
var directUpgradeInterval = 5 * time.Minute

// upgradeNodeInterval is the minimum gap between upgrade handshakes toward the
// same peer, regardless of which of its seeds triggers them. It exists because
// the unthrottled upgrade paths (explicit seeds, host candidates) are keyed by
// seed while a peer routinely owns a dozen of them — see seedOwnerNeedsUpgrade.
// Sized to a single handshake attempt: long enough that one try can land before
// the next seed takes a turn, short enough that escaping a relay still happens
// in seconds, not minutes.
var upgradeNodeInterval = handshakeRetry

// NewEngine builds an Engine. Attach a transport with Attach before Start.
func NewEngine(o Options) *Engine {
	log := o.Log
	if log == nil {
		log = logx.Default()
	}
	e := &Engine{
		nodeID:     o.NodeID,
		hostname:   o.Hostname,
		log:        log,
		webPort:    o.WebPort,
		sessions:   make(map[uint32]*peerSession),
		stop:       make(chan struct{}),
		reflexive:  make(map[string]reflexiveObs),
		bypassRefs: make(map[netip.Addr]bypassRef),
		startedAt:  time.Now(),
	}
	e.managed.Store(o.Managed)
	e.manager.Store(o.Manager)
	if o.RouteAdvInterval > 0 {
		e.routeAdvNs.Store(int64(o.RouteAdvInterval))
	} else {
		e.routeAdvNs.Store(int64(defaultRouteAdvInterval))
	}
	if o.KeepaliveInterval > 0 {
		e.keepaliveNs.Store(int64(o.KeepaliveInterval))
	} else {
		e.keepaliveNs.Store(int64(defaultKeepaliveInterval))
	}
	if o.PeerTimeout > 0 {
		pt := o.PeerTimeout
		if min := time.Duration(e.keepaliveNs.Load()); pt < min {
			pt = min
		}
		e.peerTimeoutNs.Store(int64(pt))
	} else {
		e.peerTimeoutNs.Store(int64(defaultPeerTimeout))
	}
	e.maxInnerFrag = computeMaxInnerFrag(o.UnderlayMTU)
	floor := o.UnderlayMTU
	if floor <= 0 {
		floor = 1280
	}
	ceil := o.UnderlayMTUMax
	if ceil < floor {
		ceil = floor // discovery disabled
	}
	// See pmtu_cap_darwin.go / pmtu_cap_openbsd.go: on platforms where the
	// don't-fragment guarantee doesn't actually hold for every family, don't
	// let discovery grow past a size that's safe without relying on it. A
	// no-op on every other platform.
	if ceil > platformPMTUCeil {
		ceil = platformPMTUCeil
		if ceil < floor {
			ceil = floor
		}
	}
	e.pmtuFloor, e.pmtuCeil = floor, ceil
	e.tunWorkers = o.TunWorkers
	if e.tunWorkers <= 0 {
		e.tunWorkers = runtime.NumCPU() - 1
		if e.tunWorkers < 1 {
			e.tunWorkers = 1
		}
	}
	e.fallbackPort.Store(int64(o.TCPFallbackPort))
	e.primaryPort.Store(int64(o.PrimaryPort))
	e.extraTCPPorts.Store(&o.ExtraTCPPorts)
	e.extraUDPPorts.Store(&o.ExtraUDPPorts)
	// Seed the node-global firewall catalog before any network state is
	// built below, so every initial network (not just ones added later via
	// AddNetwork) starts with the real objects/services instead of empty.
	e.fwObjects = append([]FirewallObject(nil), o.FirewallObjects...)
	e.fwServices = append([]FirewallService(nil), o.FirewallServices...)
	nm := make(map[uint64]*netState, len(o.Nets))
	for _, spec := range o.Nets {
		ns := e.newNetState(spec)
		nm[spec.ID] = ns
	}
	e.nets.Store(&nm)
	// Prime the host-candidate set before any handshake can be built. Done here
	// (after nets are stored, so isOverlayAddr can see the overlay subnets)
	// rather than lazily on first use, because the only caller of
	// localEndpoints runs under ns.mu and must never do the enumeration itself.
	e.refreshLocalCandidates()
	return e
}

// newNetState builds a network's runtime state from its spec (no goroutines
// started yet). Shared by NewEngine and AddNetwork.
func (e *Engine) newNetState(spec NetSpec) *netState {
	b := spec.Ban
	if b.MaxFailures == 0 {
		b = config.BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900, CoalesceSeconds: 3}
	}
	coalesce := b.Coalesce()
	if coalesce <= 0 {
		coalesce = 3 * time.Second // fold a single join's key-tries into one failure
	}
	ns := &netState{
		spec:                 spec,
		throttle:             ratelimit.New(b.MaxFailures, b.Window(), b.Ban(), coalesce),
		byNode:               make(map[string]*peerSession),
		routes4:              make(map[netip.Addr]*peerSession),
		routes6:              make(map[netip.Addr]*peerSession),
		pending:              make(map[uint32]*pendingHS),
		seeds:                append([]netip.AddrPort(nil), spec.Seeds...),
		tcpSeeds:             append([]netip.AddrPort(nil), spec.TCPSeeds...),
		seedBackoff:          make(map[netip.AddrPort]time.Time),
		seedFallback:         make(map[netip.AddrPort]netip.AddrPort),
		seedOwner:            make(map[netip.AddrPort]string),
		explicitSeed:         explicitSeedSet(spec.Seeds),
		explicitSeedNode:     make(map[string]bool),
		hostCand:             make(map[netip.AddrPort]bool),
		hostCandDead:         make(map[netip.AddrPort]bool),
		upgradeNodeAt:        make(map[string]time.Time),
		fallbackAttempt:      make(map[netip.AddrPort]time.Time),
		seedFirstSeen:        make(map[netip.AddrPort]time.Time),
		directUpgradeAttempt: make(map[netip.AddrPort]time.Time),
		everConnected:        make(map[netip.AddrPort]bool),
		dialing:              make(map[netip.AddrPort]bool),
		nodes:                make(map[string]*nodeInfo),
		relayRefusedLog:      make(map[string]time.Time),
		relayDeclinedLog:     make(map[string]time.Time),
		self4:                spec.Self4,
		self6:                spec.Self6,
		subnet4:              spec.Subnet4,
		subnet6:              spec.Subnet6,
		name:                 spec.Name,
		bans:                 make(map[string]*banRecord),
		disabledPeers:        make(map[string]bool),
		peerNotes:            make(map[string]string),
		knownRoute:           make(map[string]bool),
		learnedHosts:         make(map[string]*learnedHost),
		learnedDNS:           make(map[string]*learnedDNS),
		osMetric:             make(map[netip.Prefix]int),
		demotedGatewayMetric: make(map[netip.Prefix]int),
		physicalGW:           make(map[int]physicalGWCache),
		hsSeen:               make(map[string]time.Time),
		pendingPeerAdds:      make(map[string]int),
		done:                 make(chan struct{}),
	}
	if spec.Dev != nil {
		ns.setDev(spec.Dev) // seed the live-device holder; swapped later by a rebuild
	}
	ns.banTTL = spec.BanTTL
	if ns.banTTL <= 0 {
		ns.banTTL = defaultBanTTL
	}
	ns.keys.Store(spec.Keys) // live key set; swapped on reload
	kl := spec.KeyLabels
	ns.keyLabels.Store(&kl) // display-only label map alongside the keys, kept in sync on reload
	for _, id := range spec.DisabledPeers {
		ns.disabledPeers[id] = true
	}
	for id, note := range spec.PeerNotes {
		ns.peerNotes[id] = note
	}
	{
		rs := append([]netip.Prefix(nil), spec.Routes...)
		ns.advRoutes.Store(&rs)
		rj := append([]RejectRule(nil), spec.RouteReject...)
		ns.advReject.Store(&rj)
		mm := cloneMetricMap(spec.RouteMetric)
		ns.advMetric.Store(&mm)
		hr := toHostRecords(spec.AdvHosts)
		ns.advHosts.Store(&hr)
		hrj := lowerAll(spec.HostReject)
		ns.advHostReject.Store(&hrj)
		dr := toDNSForwards(spec.AdvDNS)
		ns.advDNS.Store(&dr)
		drj := lowerAll(spec.DNSReject)
		ns.advDNSReject.Store(&drj)
		sd := append([]string(nil), spec.SearchDomains...)
		ns.searchDomains.Store(&sd)
	}
	ns.bcast = newTokenBucket(spec.BroadcastPPS, spec.StormBurst)
	ns.mcast = newTokenBucket(spec.MulticastPPS, spec.StormBurst)
	if s := e.buildEgress(spec); s != nil {
		ns.egress.Store(s)
	}
	if t := buildIngress(spec); t != nil {
		ns.ingress.Store(t)
	}
	// Build the firewall unconditionally so the pointer is stable on the hot
	// path; the enabled flag (toggled live) decides whether it filters.
	ns.fw = newFirewall(nil)
	ns.fw.setLogger(e.log)
	ns.fw.setMgmtPort(e.webPort)
	objs, svcs := e.firewallCatalogSnapshot()
	ns.fw.setCatalog(objs, svcs)
	ns.fw.loadRules(spec.FirewallRules)
	ns.fw.setExempts(spec.FirewallExempts)
	ns.fw.setEnabled(spec.FirewallEnabled)
	if nt := e.buildNAT(spec); nt != nil {
		ns.nat.Store(nt)
	}
	return ns
}

// firewallCatalogSnapshot returns copies of the node-global firewall object/
// service catalog — see the fwCatalogMu field group's doc comment.
func (e *Engine) firewallCatalogSnapshot() ([]FirewallObject, []FirewallService) {
	e.fwCatalogMu.Lock()
	defer e.fwCatalogMu.Unlock()
	return append([]FirewallObject(nil), e.fwObjects...), append([]FirewallService(nil), e.fwServices...)
}

// network returns the netState for an id, or nil. Lock-free (reads a snapshot).
func (e *Engine) network(id uint64) *netState {
	if m := e.nets.Load(); m != nil {
		return (*m)[id]
	}
	return nil
}

// netSnapshot returns the current network map for iteration. Do not mutate it.
func (e *Engine) netSnapshot() map[uint64]*netState {
	if m := e.nets.Load(); m != nil {
		return *m
	}
	return nil
}

// mutateNets applies fn to a fresh copy of the network map and swaps it in.
func (e *Engine) mutateNets(fn func(m map[uint64]*netState)) {
	e.netMu.Lock()
	defer e.netMu.Unlock()
	cur := e.netSnapshot()
	nm := make(map[uint64]*netState, len(cur)+1)
	for k, v := range cur {
		nm[k] = v
	}
	fn(nm)
	e.nets.Store(&nm)
}

// AddNetwork brings a network up at runtime: it builds the state and starts the
// per-network loops, with no engine restart. The spec must carry a live TUN
// device and key set. Errors if the network is already running.
func (e *Engine) AddNetwork(spec NetSpec) error {
	if e.network(spec.ID) != nil {
		return fmt.Errorf("network %016x already running", spec.ID)
	}
	ns := e.newNetState(spec)
	e.mutateNets(func(m map[uint64]*netState) { m[spec.ID] = ns })
	e.startNetwork(ns)
	e.log.Infof("mesh: network %016x added live (%s)", spec.ID, spec.Name)
	return nil
}

// RemoveNetwork tears a network down at runtime: it stops the per-network loops,
// drops its sessions, closes its TUN, and removes it — with no engine restart.
func (e *Engine) RemoveNetwork(id uint64) error {
	ns := e.network(id)
	if ns == nil {
		return fmt.Errorf("no such network %016x", id)
	}
	// Pull it from the routing map first so the data path stops handing it work.
	e.mutateNets(func(m map[uint64]*netState) { delete(m, id) })
	// Signal the loops, then close the TUN so tunLoop's blocking Read unblocks;
	// only then can we wait for a clean exit. The dpClosing flag + dpMu ensure a
	// concurrent data-plane rebuild can't swap in a fresh device after we've
	// closed the current one (which tunLoop would then block reading forever,
	// hanging wg.Wait): either the rebuild aborts, or it completes and we close
	// the device it installed.
	close(ns.done)
	ns.dpMu.Lock()
	ns.dpState = dpClosing
	if d := ns.dev(); d != nil {
		d.Close()
	}
	ns.dpMu.Unlock()
	ns.wg.Wait()
	if eg := ns.egress.Swap(nil); eg != nil {
		eg.close()
	}
	// Drop any sessions that belonged to this network.
	e.mu.Lock()
	for idx, ps := range e.sessions {
		if ps.net == ns {
			delete(e.sessions, idx)
		}
	}
	e.mu.Unlock()
	// Clear this network's managed hosts block and DNS conditional-forwards
	// immediately, rather than leaving them until the whole daemon stops.
	// clearStaleHostsBlocks/clearStaleDNSForwards only run at process
	// startup/shutdown, so without this a network that's disabled or removed
	// via a live config reload (the daemon keeps running) leaves its overlay
	// hostnames and forwarding domains stuck in the OS hosts file/resolver
	// indefinitely.
	e.clearNetworkHosts(ns)
	e.clearNetworkDNS(ns)
	e.log.Infof("mesh: network %016x removed live (%s)", id, ns.spec.Name)
	return nil
}

// clearNetworkHosts wipes ns's managed block from its hosts file, mirroring
// clearStaleHostsBlocks but for a single network being torn down live.
func (e *Engine) clearNetworkHosts(ns *netState) {
	if !ns.spec.HostsSync {
		return
	}
	path := ns.spec.HostsPath
	if path == "" {
		path = hosts.DefaultPath()
	}
	tag := fmt.Sprintf("%016x", ns.spec.ID)
	if err := hosts.Sync(path, tag, nil); err != nil {
		e.log.Debugf("mesh: clearing hosts block on remove (net %016x, %s): %v", ns.spec.ID, path, err)
	}
}

// clearNetworkDNS withdraws ns's conditional-forward domains from the OS
// resolver, mirroring clearStaleDNSForwards but for a single network being
// torn down live.
func (e *Engine) clearNetworkDNS(ns *netState) {
	if !ns.spec.DNSSync {
		return
	}
	iface := ns.spec.Name
	if ns.spec.Dev != nil {
		iface = ns.spec.Dev.Name()
	}
	tag := ns.spec.DNSTag
	if tag == "" {
		tag = fmt.Sprintf("%016x", ns.spec.ID)
	}
	if err := resolver.Clear(tag, iface); err != nil {
		e.log.Debugf("mesh: clearing dns forwards on remove (net %016x, iface %s): %v", ns.spec.ID, iface, err)
	}
}

// startNetwork starts a single network's loops. Caller must have added ns to the
// map first. Used by Start (all networks) and AddNetwork (one).
func (e *Engine) startNetwork(ns *netState) {
	if eg := ns.egress.Load(); eg != nil {
		go eg.run()
	}
	ns.wg.Add(4)
	go e.tunLoop(ns)
	go e.initLoop(ns)
	go e.maintLoop(ns)
	go e.pmtuLoop(ns)
	// Self-assign an overlay address right away so a lone/first node in a
	// network doesn't sit address-less until the first maintenance tick.
	e.maybeAssignAddress(ns)
}

// isOverlayAddr reports whether addr falls inside ANY of this node's overlay
// subnets — i.e. it is a mesh/tunnel address, not a reachable underlay endpoint.
// Such an address must never be dialed as a bootstrap target or cached as a peer
// endpoint: doing so makes a peer in one network "reachable" only through another
// network's tunnel (a fragile cross-network dependency), and is how a network1
// overlay (mesh0) address can leak into network2's peer_cache. Subnets are set at
// construction and never mutated, so reading them lock-free is safe.
func (e *Engine) isOverlayAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	for _, ns := range e.netSnapshot() {
		if ns.subnet4.IsValid() && ns.subnet4.Contains(addr) {
			return true
		}
		if ns.subnet6.IsValid() && ns.subnet6.Contains(addr) {
			return true
		}
	}
	return false
}

// explicitSeedSet builds the initial explicitSeed map from a network's
// configured seed list, for newNetState — see explicitSeed's own doc comment.
func explicitSeedSet(seeds []netip.AddrPort) map[netip.AddrPort]bool {
	m := make(map[netip.AddrPort]bool, len(seeds))
	for _, s := range seeds {
		m[s] = true
	}
	return m
}

// AddSeed registers an underlay endpoint to (re)connect to on a network. Used
// to bootstrap and, in later steps, to dial peers learned via gossip. The
// seed's owning node is left unrecorded, so install()'s stale-seed pruning
// will never touch it — the safe default for callers that don't cheaply know
// which node an address belongs to.
func (e *Engine) AddSeed(networkID uint64, seed netip.AddrPort) {
	e.addSeed(networkID, seed, "", false)
}

// AddSeedFor is AddSeed plus recording which node this address is known to
// belong to, so that once that node is connected, install() can safely prune
// this specific entry if it turns out to be stale (superseded by the node's
// current endpoint) — without guessing based on IP alone, which would risk
// evicting a different node's still-needed seed if they happen to share an
// address (see the same precision requirement in connectedTo).
func (e *Engine) AddSeedFor(networkID uint64, seed netip.AddrPort, nodeID string) {
	e.addSeed(networkID, seed, nodeID, false)
}

// AddExplicitSeed is AddSeed plus marking the address as one the operator
// configured directly (NetSpec.Seeds), rather than one discovered
// dynamically via gossip or a fallback dial's own bookkeeping. Used only at
// network construction (newNetState) and by ReloadRuntime's config-seed
// merge — see netState.explicitSeed's doc comment for what this unlocks.
func (e *Engine) AddExplicitSeed(networkID uint64, seed netip.AddrPort) {
	e.addSeed(networkID, seed, "", true)
}

// addSeed returns true if seed was newly added to ns.seeds (false if it was
// already present, or was rejected as an overlay address). Callers use this to
// log only genuinely new information rather than re-announcing on every tick.
func (e *Engine) addSeed(networkID uint64, seed netip.AddrPort, nodeID string, explicit bool) bool {
	if e.isOverlayAddr(seed.Addr()) {
		// A gossip-learned (or otherwise discovered) endpoint that lands inside an
		// overlay subnet is a mesh address, not a real underlay endpoint. Dialing
		// it would tunnel one network's traffic through another's overlay. Drop it.
		e.log.Debugf("mesh: ignoring endpoint %s on net %016x — overlay (mesh) address, not a reachable underlay endpoint", seed, networkID)
		return false
	}
	ns := e.network(networkID)
	if ns == nil {
		return false
	}
	ns.mu.Lock()
	defer ns.mu.Unlock()
	if explicit {
		ns.explicitSeed[seed] = true
		if owner, known := ns.seedOwner[seed]; known && owner != "" {
			// The owner was already known (e.g. learned via gossip on an
			// earlier connection) before this address was reaffirmed as an
			// explicit config seed on a later reload — promote now rather
			// than waiting for a future AddSeedFor to do it.
			ns.explicitSeedNode[owner] = true
		}
	}
	if nodeID != "" {
		ns.seedOwner[seed] = nodeID
		// Promote to the node-keyed map the moment an explicitly-configured
		// address's owner becomes known, so the priority follows that node
		// even once it's reached at a different (e.g. roamed) address — see
		// explicitSeed's doc comment for why this is node-keyed, not just
		// address-keyed.
		if ns.explicitSeed[seed] {
			ns.explicitSeedNode[nodeID] = true
		}
	}
	for _, s := range ns.seeds {
		if s == seed {
			return false
		}
	}
	ns.seeds = append(ns.seeds, seed)
	if _, ok := ns.seedFirstSeen[seed]; !ok {
		ns.seedFirstSeen[seed] = time.Now()
	}
	return true
}

// SessionCount reports the number of active inbound sessions across networks.
func (e *Engine) SessionCount() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.sessions)
}

// PeerCount reports the number of distinct connected peers on a network.
func (e *Engine) PeerCount(networkID uint64) int {
	ns := e.network(networkID)
	if ns == nil {
		return 0
	}
	ns.mu.RLock()
	defer ns.mu.RUnlock()
	return len(ns.byNode)
}

// Attach wires the transport used for sending. Safe to call concurrently with
// the transport's receive workers.
func (e *Engine) Attach(s Sender) {
	e.mu.Lock()
	e.tr = s
	e.mu.Unlock()
}

// send transmits via the attached transport, tolerating the brief window before
// Attach is called.
func (e *Engine) send(to netip.AddrPort, b []byte) error {
	e.mu.RLock()
	s := e.tr
	e.mu.RUnlock()
	if s == nil || b == nil {
		return nil
	}
	err := s.Send(to, b)
	if err != nil {
		// EMSGSIZE is not a transient failure — it is the kernel telling us,
		// synchronously and for free, the exact thing PMTU discovery exists to
		// find out: this packet is larger than the outgoing path allows. Feed it
		// straight back into the search. Ignoring it (as this did) meant that a
		// host moving from a jumbo-frame LAN to a ~1400-byte link had every
		// oversized packet rejected locally, logged at Debug, and dropped — then
		// re-sent at the same size a second later, forever. The peer's eff MTU
		// was still sized for a link that no longer existed, so nothing could get
		// out at all, and the node went completely dark until the original link
		// came back. See pmtuState.tooBig.
		if isMsgTooLong(err) {
			e.noteTooLong(to, len(b))
		}
		e.log.Debugf("mesh: send: %v", err)
	}
	return err
}

// isMsgTooLong reports whether err is the OS's "datagram exceeds the path MTU"
// error (EMSGSIZE on Unix, WSAEMSGSIZE on Windows — x/sys maps both to
// syscall.EMSGSIZE, and errors.Is unwraps the *net.OpError/*os.SyscallError
// chain the transport returns).
func isMsgTooLong(err error) bool {
	return errors.Is(err, syscall.EMSGSIZE)
}

// noteTooLong clamps the PMTU of whichever peer owns endpoint `to` after the
// kernel refused a datagram of size bytes as too large. Matching on the
// endpoint (not the node id) is deliberate: send() is also used for handshakes
// to seeds we have no session with yet, and those have no peer to clamp — they
// are small enough that they will never hit this in practice, and if one
// somehow does, silently ignoring it is correct.
func (e *Engine) noteTooLong(to netip.AddrPort, size int) {
	now := time.Now()
	for _, ns := range e.netSnapshot() {
		ns.mu.RLock()
		peers := make([]*peerSession, 0, len(ns.byNode))
		for _, ps := range ns.byNode {
			peers = append(peers, ps)
		}
		ns.mu.RUnlock()
		for _, ps := range peers {
			if ps.ep() != to {
				continue
			}
			ps.pmtuMu.Lock()
			var eff int
			changed := false
			if ps.pmtu != nil {
				changed = ps.pmtu.tooBig(size, now)
				eff = ps.pmtu.eff
			}
			ps.pmtuMu.Unlock()
			if changed {
				ps.setEff(eff)
				e.log.Infof("mesh: path to %q rejected a %d-byte packet (link MTU shrank, e.g. a roam onto a smaller-MTU network); underlay MTU dropped to %d and re-discovering", ps.nodeID, size, eff)
			}
			return
		}
	}
}

// NoteSendTooLong reports a datagram the kernel refused as too large for the
// path (EMSGSIZE) on a send that had already returned. The batched UDP send
// path on Linux (transport.Options.OnSendMsgSize) writes datagrams after Send
// has returned nil, so it cannot deliver EMSGSIZE as a return value the way the
// direct path does; it calls this instead. The effect is identical either way —
// the peer owning that endpoint has its path MTU clamped and re-discovered —
// so this is deliberately the same noteTooLong the synchronous path uses rather
// than a second, parallel notion of the same event.
//
// Safe to call from any goroutine, including a transport flusher.
func (e *Engine) NoteSendTooLong(to netip.AddrPort, size int) { e.noteTooLong(to, size) }

// Start launches the TUN read loops, handshake initiation, and maintenance.
func (e *Engine) Start() {
	for _, ns := range e.netSnapshot() {
		e.startNetwork(ns)
	}
}

// Stop halts the engine loops.
func (e *Engine) Stop() {
	close(e.stop)
	// Close every TUN so each tunLoop's blocking Read returns (covers networks
	// added live, whose devices the caller doesn't track). Mark dpClosing first
	// so an in-flight rebuild can't install a fresh device past the close (see
	// RemoveNetwork). Close the live device, which may differ from spec.Dev if a
	// rebuild swapped it.
	for _, ns := range e.netSnapshot() {
		ns.dpMu.Lock()
		ns.dpState = dpClosing
		if d := ns.dev(); d != nil {
			d.Close()
		}
		ns.dpMu.Unlock()
	}
	for _, ns := range e.netSnapshot() {
		ns.wg.Wait()
	}
	// Loops are done (no more enqueues); now drain-stop the shapers.
	for _, ns := range e.netSnapshot() {
		if eg := ns.egress.Load(); eg != nil {
			eg.close()
		}
	}
}

// allocIndex returns a fresh, non-zero inbound session index.
// LogLevel reports the process-global log level, for the web admin's settings
// surface (see webadmin.handleLogLevel).
func (e *Engine) LogLevel() string { return logx.LevelName() }

func (e *Engine) allocIndex() uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		e.nextIdx++
		if e.nextIdx == 0 {
			continue
		}
		if _, exists := e.sessions[e.nextIdx]; !exists {
			return e.nextIdx
		}
	}
}

// OnPacket is the transport receive handler. The payload is borrowed; we copy
// anything we retain past this call.
func (e *Engine) OnPacket(payload []byte, from netip.AddrPort, fam transport.Family) {
	e.dispatch(payload, from, nil)
}

// dispatch routes a packet. via is non-nil when the packet arrived wrapped
// inside a relay session (so replies and the resulting session use that relay).
func (e *Engine) dispatch(payload []byte, from netip.AddrPort, via *peerSession) {
	ty, err := protocol.PacketType(payload)
	if err != nil {
		if err == protocol.ErrVersion {
			// Unauthenticated at this point — `from` isn't verified, so this
			// fires on any raw traffic hitting the socket with a mismatched
			// first byte, port scanners and other internet background noise
			// included, not just a real peer on an incompatible build. Debug
			// only, unconditionally: Debug is opt-in, so this can't become a
			// flood vector against a default-configured node the way a
			// Warn/Info here could, but it's there to find when a specific
			// known peer genuinely won't connect and this is why — matches
			// the existing precedent of logging other pre-auth malformed
			// input at Debug (e.g. transport's own read-error Debugf).
			e.log.Debugf("mesh: dropped packet from %s: wire version mismatch (got %d, this build speaks %d) — if this is a known peer rather than background noise, it's very likely running an incompatible gravinet build", from, payload[0], protocol.Version)
		}
		return
	}
	switch ty {
	case protocol.TypeData:
		e.onData(payload, from, via)
	case protocol.TypeHSInit:
		e.onHSInit(payload, from, via)
	case protocol.TypeHSResp:
		e.onHSResp(payload, from, via)
	}
}

// Inner frame types multiplex the encrypted session between tunnelled IP, the
// control plane, and relayed traffic for other peers.
const (
	innerIP       = 0x00 // payload is a raw IP packet for the overlay interface
	innerCtrl     = 0x01 // payload is a control message (see control.go)
	innerRelay    = 0x02 // payload is a relay envelope (see relay.go)
	innerFrag     = 0x03 // payload is one fragment of a larger overlay IP packet (see frag.go)
	innerMTUProbe = 0x04 // path-MTU discovery probe (see pmtu.go)
	innerMTUAck   = 0x05 // path-MTU discovery probe acknowledgement
)

// ---- data path ----

func (e *Engine) onData(payload []byte, from netip.AddrPort, via *peerSession) {
	h, aad, ct, err := protocol.DecodeData(payload)
	if err != nil {
		return
	}
	e.mu.RLock()
	ps := e.sessions[h.RecvSession]
	e.mu.RUnlock()
	if ps == nil {
		return
	}
	ps.recvMu.Lock()
	pt, err := ps.sess.Open(ct[:0], ct, aad, h.Counter) // decrypt in place into the RX buffer
	ps.recvMu.Unlock()
	if err != nil {
		return // replay or authentication failure
	}
	if roamed := ps.touch(from, via); roamed {
		e.syncPeerBypassRoute(ps.net, ps)
	}
	if len(pt) == 0 {
		return
	}
	switch pt[0] {
	case innerIP:
		e.deliverInner(ps, pt[1:], len(payload))
	case innerCtrl:
		e.onControl(ps, pt[1:])
	case innerRelay:
		e.onRelay(ps, pt[1:])
	case innerFrag:
		e.onFragment(ps, pt[1:])
	case innerMTUProbe:
		e.onMTUProbe(ps, pt[1:])
	case innerMTUAck:
		e.onMTUAck(ps, pt[1:])
	}
}

// deliverInner applies ingress NAT, firewall, and down-rate policing to a
// reassembled overlay IP packet and writes it to the TUN. outerLen is the size
// of the underlay datagram(s) this packet arrived in, used for ingress policing.
func (e *Engine) deliverInner(ps *peerSession, ip []byte, outerLen int) {
	// Anti-spoofing: a peer may only source packets from an overlay address it
	// legitimately owns — its own assigned overlay address(es), or any prefix it
	// advertises as a redistributed route (where it is the gateway for that
	// subnet). Without this, any authenticated mesh member could inject packets
	// claiming another node's overlay source, impersonating it or evading
	// source-matched firewall rules. This is gravinet's equivalent of
	// WireGuard's cryptokey-routing/AllowedIPs source check. Checked on the
	// as-received source, before translateIn rewrites it.
	if !e.sourceAllowedFrom(ps, ip) {
		ps.spoofDrop.Add(1)
		return
	}
	if nat := ps.net.nat.Load(); nat != nil {
		nat.translateIn(ip) // reverse-SNAT / DNAT
	}
	if !ps.net.fw.allow(fwIn, ip) {
		return // dropped by firewall (ingress)
	}
	if ig := ps.net.ingress.Load(); ig != nil && !ig.allowN(float64(outerLen)) {
		return // ingress policing: over the down-rate, drop
	}
	if _, err := ps.net.dev().Write(ip); err != nil {
		e.log.Debugf("mesh: tun write: %v", err)
	}
}

// sourceAllowedFrom reports whether peer ps may source a packet with the given
// inner IP's source address. The guard is deliberately narrow: it blocks the
// one thing an authenticated mesh member must never be able to do — impersonate
// *another* peer's overlay identity — while staying out of the way of every
// legitimate reason a peer emits a non-own source (NAT masquerade to an
// arbitrary translate address, gatewaying a redistributed subnet, etc.).
//
// Concretely, a source is refused only when it is an overlay address currently
// owned by a *different* peer. The sending peer's own overlay address is always
// fine; so is any address no other peer claims (a NAT translate address, a
// gatewayed host behind it). This keeps identity-spoofing impossible without
// forcing every masquerade/translate address to be pre-advertised, which strict
// per-address allow-listing would have required (and which broke legitimate NAT).
//
// An unparseable header is refused — a well-formed peer never sends one.
func (e *Engine) sourceAllowedFrom(ps *peerSession, ip []byte) bool {
	src, ok := parseSrc(ip)
	if !ok {
		return false
	}
	src = src.Unmap()
	snap := ps.net.fwd.Load()
	if snap == nil {
		return true
	}
	var owner *peerSession
	if src.Is4() {
		owner = snap.routes4[src]
	} else {
		owner = snap.routes6[src]
	}
	if owner != nil && owner.nodeID == ps.nodeID {
		return true
	} // own addr, by node
	for _, re := range snap.redist { // gateway prefixes
		if re.origin == ps.nodeID && re.prefix.Contains(src) {
			return true
		}
	}
	if owner != nil {
		return false
	} // another node's overlay identity
	return true
}

func (ps *peerSession) touch(from netip.AddrPort, via *peerSession) (roamed bool) {
	ps.mu.Lock()
	ps.lastRx = time.Now()
	if via != nil {
		ps.relay = via // keep the relay path fresh
	} else if from.IsValid() && ps.endpoint != from {
		ps.endpoint = from // NAT roaming: follow the observed source (direct only)
		roamed = true
	}
	ps.mu.Unlock()
	if roamed {
		// The underlay path to this peer changed; its old path MTU is no longer
		// trustworthy. Re-discover (dropping to the floor now so traffic keeps
		// flowing if the new path is smaller).
		ps.resetPMTU()
	}
	return roamed
}

func (ps *peerSession) ep() netip.AddrPort {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.endpoint
}

func (ps *peerSession) getRelay() *peerSession {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.relay
}

// dataBufPool supplies scratch buffers for the outbound data path so each packet
// is encrypted in place into a reused buffer instead of allocating header, frame,
// and ciphertext slices per packet.
var dataBufPool = sync.Pool{New: func() any { b := make([]byte, protocol.MaxUDPPayload); return &b }}

// sealAndSend wraps body in an inner frame, encrypts it under the session, and
// delivers it to the peer (directly, or wrapped through a relay). The whole outer
// packet — header, inner type, ciphertext, and tag — is built in one pooled
// buffer and the frame is encrypted in place, so the hot path allocates nothing.
//
// The pooled buffer goes back to dataBufPool the moment this returns, which is
// safe even though the transport may batch the write asynchronously: the
// batched path copies the payload into its own ring slot at enqueue precisely
// so this buffer can be recycled here (see transport's sendRing).
//
// Note that on the batched path a nil return does not mean the kernel accepted
// the datagram — only that it was queued. sendData's isMsgSize check below
// therefore cannot see EMSGSIZE from a batched send; that signal arrives
// out-of-band via transport.Options.OnSendMsgSize, wired to NoteSendTooLong,
// which clamps the same peer's PMTU by endpoint.
func (e *Engine) sealAndSend(ps *peerSession, innerType byte, body []byte) error {
	const h = protocol.DataHeaderLen
	need := h + 1 + len(body) + 16 // header + innerType + body + GCM tag
	bufp := dataBufPool.Get().(*[]byte)
	buf := *bufp
	if cap(buf) < need {
		buf = make([]byte, need)
	}
	buf = buf[:h+1+len(body)]
	buf[h] = innerType
	copy(buf[h+1:], body)

	var aad [6]byte
	aad[0] = protocol.Version
	aad[1] = byte(protocol.TypeData)
	aad[2] = byte(ps.remoteIdx >> 24)
	aad[3] = byte(ps.remoteIdx >> 16)
	aad[4] = byte(ps.remoteIdx >> 8)
	aad[5] = byte(ps.remoteIdx)

	pt := buf[h:]                                   // innerType + body
	counter, ct := ps.sess.Seal(pt[:0], pt, aad[:]) // in place: ct aliases buf[h:]
	protocol.EncodeData(buf[:h], protocol.DataHeader{RecvSession: ps.remoteIdx, Counter: counter})
	err := e.deliver(ps, buf[:h+len(ct)])

	*bufp = buf
	dataBufPool.Put(bufp)
	return err
}

// deliver sends a fully-formed outer packet to a peer: directly to its underlay
// endpoint, or — if the peer is reached via a relay — wrapped in a relay
// envelope and sent through the relay session (which never sees the plaintext).
func (e *Engine) deliver(ps *peerSession, outer []byte) error {
	if r := ps.getRelay(); r != nil {
		return e.sealAndSend(r, innerRelay, encodeRelay(e.nodeID, ps.nodeID, outer))
	}
	return e.send(ps.ep(), outer)
}

// sendData encrypts and transmits an overlay IP packet to the peer. Packets that
// would exceed the underlay datagram cap once sealed are split into authenticated
// fragments (see frag.go) and reassembled by the peer.
func (e *Engine) sendData(ps *peerSession, packet []byte) {
	per := int(ps.maxFrag.Load())
	if per <= 0 {
		per = e.maxInnerFrag // session not yet PMTU-initialised: use the floor
	}
	if per > 0 && len(packet) > per {
		e.sendFragmented(ps, packet, per)
		return
	}
	if err := e.sealAndSend(ps, innerIP, packet); err != nil && isMsgSize(err) {
		ps.resetPMTU() // path MTU shrank below our estimate; shrink and re-discover
	}
}

func (e *Engine) tunLoop(ns *netState) {
	// The worker-pool machinery below (channel, sync.Pool, extra goroutine)
	// is pure overhead when there's only going to be one worker anyway — a
	// real measured benchmark (BenchmarkOutboundThroughput) on single-core
	// hardware showed the pooled path running ~70% *slower* than the
	// original fully-inline loop (channel handoff + pool Get/Put costing
	// more than the parallelism was ever going to recover with no second
	// core to run on). tunWorkerCount()==1 covers both that case and an
	// operator explicitly setting TunWorkers=1, so route both to
	// tunLoopSerial: the same shape as the pre-worker-pool code, not the
	// pooled path with N forced to 1.
	if e.tunWorkerCount() <= 1 {
		e.tunLoopSerial(ns)
		return
	}
	e.tunLoopPooled(ns)
}

// tunLoopSerial is tunLoop's single-worker fast path: read, process, send,
// inline, one buffer, no channel, no pool, no extra goroutine — exactly the
// shape gravinet used before the outbound worker pool existed. See
// tunLoopPooled for the concurrent version and its doc comment for what
// motivated splitting these two apart instead of always paying the pooled
// path's coordination cost. The read-error/device-rebuild handling here and
// in tunLoopPooled's reader are intentionally kept in lockstep — if one
// changes, check the other.
func (e *Engine) tunLoopSerial(ns *netState) {
	defer ns.wg.Done()
	dev := ns.dev()
	buf := make([]byte, dev.MTU()+128)
	for {
		select {
		case <-e.stop:
			return
		case <-ns.done:
			return
		default:
		}
		n, err := dev.Read(buf)
		if err != nil {
			if e.shuttingDown(ns) {
				return // intentional: dev.Close() during teardown unblocked us
			}
			if ns.spec.NewDevice == nil {
				e.log.Warnf("mesh: tun read (net %x, iface %s): %v — outbound packet delivery for this network has stopped until restart", ns.spec.ID, dev.Name(), err)
				return
			}
			e.log.Warnf("mesh: tun read (net %x, iface %s): %v — overlay interface lost; rebuilding", ns.spec.ID, dev.Name(), err)
			if !e.recoverDataplane(ns) {
				return // gave up because the network is shutting down
			}
			dev = ns.dev()
			buf = make([]byte, dev.MTU()+128)
			continue
		}
		e.processOutbound(ns, buf[:n])
	}
}

// tunWorkerCount returns how many goroutines should process outbound
// packets for one network — see tunLoop.
func (e *Engine) tunWorkerCount() int {
	if e.tunWorkers > 0 {
		return e.tunWorkers
	}
	return 1 // defensive: NewEngine always sets tunWorkers>=1, but never trust a zero divisor
}

// tunJob is one packet handed from tunLoopPooled's single reader goroutine
// to a tunWorker. bufp is the pooled buffer buf[:n] was read into; the
// worker returns it to pool once done, whether or not it also allocates a
// longer-lived copy along the way (see processOutbound).
type tunJob struct {
	bufp *[]byte
	n    int
}

// tunLoopPooled reads overlay packets and routes them to the right peer
// session, using tunWorkerCount() worker goroutines for everything after
// the read — see tunLoop, which only calls this when that count is > 1.
//
// The read itself stays on this one goroutine: dev is a single-queue fd
// (no IFF_MULTI_QUEUE), and nothing here assumes concurrent Read calls on
// it are safe. But everything after the read — firewall, NAT, classify,
// route lookup, encrypt, send: processOutbound below — is handed off over a
// channel to the worker pool, so packet N+1 can be read while N is still
// being processed elsewhere. Before this, the whole pipeline ran inline on
// the reader goroutine: one packet fully routed, NAT'd, encrypted, and sent
// before the next Read even happened, capping every byte this node
// *originates* (as opposed to relays or receives) to whatever one core
// could push through that sequence, no matter how many cores were actually
// available. Inbound already had this — readLoop in internal/transport
// runs one goroutine per REUSEPORT socket — outbound just hadn't caught up.
//
// Everything processOutbound touches (ns.fw, ns.nat, ns.routes4/6 via
// routeTo, peerSession.sess) is already built for concurrent access: the
// exact same firewall/NAT/routing state is already hit concurrently today
// from deliverInner, called from every UDP read-worker goroutine on the
// inbound side, and peerSession.sess's send counter is allocated with a
// plain atomic add (crypto.Cipher.Seal), so two workers sealing packets to
// the same peer at once can't race or collide — they just get two distinct,
// still-monotonic counters. The one behavioral change worth naming
// explicitly: packets to the same destination are no longer guaranteed to
// be sent in the order they were read, since two different workers can
// finish out of order. That's not a new failure mode for anything running
// over this tunnel — UDP itself never promised ordering, the replay window
// (64 packets) comfortably absorbs reordering from goroutine scheduling
// alone, and any inner protocol that cares (TCP) already carries its own
// sequence numbers for exactly this reason.
func (e *Engine) tunLoopPooled(ns *netState) {
	defer ns.wg.Done()
	dev := ns.dev()

	n := e.tunWorkerCount()
	jobs := make(chan tunJob, n*4)
	pool := &sync.Pool{New: func() any { b := make([]byte, dev.MTU()+128); return &b }}

	var workers sync.WaitGroup
	workers.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer workers.Done()
			for job := range jobs {
				e.processOutbound(ns, (*job.bufp)[:job.n])
				pool.Put(job.bufp)
			}
		}()
	}
	// Closing jobs is what lets the workers above exit their range loop; wait
	// for them here so nothing spawned by this tunLoop invocation is still
	// running by the time it reports done via ns.wg — teardown callers that
	// ns.wg.Wait() rely on that being a complete stop, not "the reader
	// stopped, but N workers might still be mid-send."
	defer func() {
		close(jobs)
		workers.Wait()
	}()

	for {
		select {
		case <-e.stop:
			return
		case <-ns.done:
			return
		default:
		}
		need := dev.MTU() + 128
		bufp := pool.Get().(*[]byte)
		if cap(*bufp) < need {
			b := make([]byte, need)
			bufp = &b
		}
		buf := (*bufp)[:need]
		nRead, err := dev.Read(buf)
		if err != nil {
			pool.Put(bufp)
			if e.shuttingDown(ns) {
				return // intentional: dev.Close() during teardown unblocked us
			}
			// The device itself failed on a live network — the interface was
			// reset or destroyed under us (driver bounce, `ip link del`, VM/host
			// network churn). Rebuild it in place rather than silently ending
			// outbound delivery forever. Keeping the rebuild here, in the one
			// goroutine that owns the read, means no extra goroutine for ns.wg
			// to track and no race with teardown's Wait.
			if ns.spec.NewDevice == nil {
				// Recreation not wired: preserve the old behaviour (and its
				// diagnostic WARN) — delivery stops until process restart.
				e.log.Warnf("mesh: tun read (net %x, iface %s): %v — outbound packet delivery for this network has stopped until restart", ns.spec.ID, dev.Name(), err)
				return
			}
			e.log.Warnf("mesh: tun read (net %x, iface %s): %v — overlay interface lost; rebuilding", ns.spec.ID, dev.Name(), err)
			if !e.recoverDataplane(ns) {
				return // gave up because the network is shutting down
			}
			dev = ns.dev()
			continue
		}
		select {
		case jobs <- tunJob{bufp: bufp, n: nRead}:
		case <-e.stop:
			pool.Put(bufp)
			return
		case <-ns.done:
			pool.Put(bufp)
			return
		}
	}
}

// processOutbound is the firewall/NAT/classify/route/encrypt/send pipeline
// for one packet read from the TUN device — see tunLoop, which runs this on
// a pool of worker goroutines rather than inline on the reader. buf is only
// valid for the duration of this call (the caller reclaims its backing
// array immediately after); nothing here may retain it without copying —
// which is exactly why the very first thing that survives past a firewall
// check is a fresh copy (pkt), same as before this was split out.
func (e *Engine) processOutbound(ns *netState, buf []byte) {
	dst, ok := parseDst(buf)
	if !ok {
		return
	}
	if e.isUnderlayLoop(ns, buf, dst) {
		// One of gravinet's own underlay datagrams was read back off the
		// overlay interface: the kernel routed our encrypted output into the
		// very tunnel it came out of. Re-encapsulating it would loop it
		// forever — each pass growing it by one header+tag until it starts
		// fragmenting, at which point every cycle multiplies the packet count
		// and a single trigger packet becomes a self-sustaining storm that
		// saturates every core (see changelog v552 for the field report and
		// CPU profile this was diagnosed from). Drop it here, before the
		// firewall/NAT/route pipeline spends anything more on it.
		e.noteUnderlayLoop(ns, dst)
		return
	}
	if !ns.fw.allow(fwOut, buf) {
		return // dropped by firewall (egress)
	}
	pkt := buf
	if nat := ns.nat.Load(); nat != nil {
		// Never masquerade traffic sourced from this node's own overlay
		// address: that address is our identity on the mesh, and rewriting it
		// breaks return paths for overlay-internal traffic (e.g. managed-mode
		// web admin reached over the tunnel). Masquerade still applies to other
		// sources we gateway (LAN hosts, forwarded traffic).
		self4, self6 := ns.selfAddrs()
		if src, ok := parseSrc(pkt); !ok || (src != self4 && src != self6) {
			nat.translateOut(pkt) // SNAT/masquerade
		}
	}
	dst, ok = parseDst(pkt) // dst may be unchanged by SNAT, re-read to be safe
	if !ok {
		return
	}
	switch ns.classify(dst) {
	case kindBroadcast:
		if ns.bcast.allow() {
			e.flood(ns, pkt)
		}
		return
	case kindMulticast:
		if ns.mcast.allow() {
			e.flood(ns, pkt)
		}
		return
	}
	ps := ns.routeTo(dst)
	if ps == nil {
		ps = e.redistRoute(ns, dst) // redistributed CIDR routes
	}
	if ps == nil {
		return // no peer for this destination yet (relay later)
	}
	if eg := ns.egress.Load(); eg != nil {
		if !eg.enqueue(ps, append([]byte(nil), pkt...)) {
			e.log.Debugf("mesh: egress queue full on net %x, dropping packet", ns.spec.ID)
		}
	} else {
		e.sendData(ps, pkt)
	}
}

type fwdSnap struct {
	routes4 map[netip.Addr]*peerSession
	routes6 map[netip.Addr]*peerSession
	byNode  map[string]*peerSession
	redist  []routeEntry
}

// Caller must hold ns.mu.
func (ns *netState) publishFwd() {
	ns.fwd.Store(&fwdSnap{
		routes4: maps.Clone(ns.routes4),
		routes6: maps.Clone(ns.routes6),
		byNode:  maps.Clone(ns.byNode),
		redist:  append([]routeEntry(nil), ns.redist...),
	})
}

func (ns *netState) routeTo(dst netip.Addr) *peerSession {
	s := ns.fwd.Load()
	if s == nil {
		return nil
	}
	if dst.Is4() {
		return s.routes4[dst]
	}
	return s.routes6[dst]
}

// parseDst extracts the destination IP from a raw IPv4/IPv6 packet.
func parseDst(p []byte) (netip.Addr, bool) {
	if len(p) < 1 {
		return netip.Addr{}, false
	}
	switch p[0] >> 4 {
	case 4:
		if len(p) < 20 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{p[16], p[17], p[18], p[19]}), true
	case 6:
		if len(p) < 40 {
			return netip.Addr{}, false
		}
		var a [16]byte
		copy(a[:], p[24:40])
		return netip.AddrFrom16(a), true
	}
	return netip.Addr{}, false
}

// udpPorts extracts the UDP source and destination ports from a raw
// IPv4/IPv6 packet. ok is false for anything that isn't a plainly-parseable
// UDP datagram: another protocol, a non-first IPv4 fragment (no transport
// header present to read), an IPv6 packet with extension headers ahead of
// UDP, or a packet too short to hold the headers it claims. Callers treat
// "not parseable" as "not a loop suspect" — this feeds a best-effort guard
// (isUnderlayLoop), not a security boundary.
func udpPorts(p []byte) (src, dst uint16, ok bool) {
	if len(p) < 1 {
		return 0, 0, false
	}
	switch p[0] >> 4 {
	case 4:
		if len(p) < 20 || p[9] != 17 { // protocol 17 = UDP
			return 0, 0, false
		}
		if p[6]&0x1f != 0 || p[7] != 0 {
			return 0, 0, false // non-first IP fragment: no UDP header to read
		}
		ihl := int(p[0]&0x0f) * 4
		if ihl < 20 || len(p) < ihl+8 {
			return 0, 0, false
		}
		return uint16(p[ihl])<<8 | uint16(p[ihl+1]), uint16(p[ihl+2])<<8 | uint16(p[ihl+3]), true
	case 6:
		if len(p) < 48 || p[6] != 17 { // next header 17 = UDP, immediately after the fixed header
			return 0, 0, false
		}
		return uint16(p[40])<<8 | uint16(p[41]), uint16(p[42])<<8 | uint16(p[43]), true
	}
	return 0, 0, false
}

// ownUDPPort reports whether port is one this node's own transport is bound
// to (the primary UDP listen port, or any extra listen port — replies go
// back out the arrival socket, so an extra port is a possible source port
// for our own datagrams too, not just an inbound-only detail).
func (e *Engine) ownUDPPort(port uint16) bool {
	if pp := e.primaryPort.Load(); pp > 0 && uint16(pp) == port {
		return true
	}
	for _, p := range loadPortList(&e.extraUDPPorts) {
		if p == port {
			return true
		}
	}
	return false
}

// sameUnderlayAddrPort compares two underlay endpoints with 4-in-6 mapped
// addresses canonicalized: a dual-stack socket reports an IPv4 peer as
// ::ffff:a.b.c.d, while the same address parsed out of an IPv4 header is
// plain a.b.c.d — those must compare equal here.
func sameUnderlayAddrPort(a, b netip.AddrPort) bool {
	return a.Port() == b.Port() && a.Addr().Unmap() == b.Addr().Unmap()
}

// isUnderlayLoop reports whether a packet just read from the TUN device is
// one of gravinet's own underlay datagrams — encrypted mesh output that the
// kernel routed back into the overlay interface instead of out the physical
// one. That happens when a route steering traffic into the tunnel (most
// plausibly a redistributed prefix installed by syncRoute) covers a peer's
// real underlay endpoint; see processOutbound's call site for why the only
// safe thing to do with such a packet is drop it.
//
// The test is deliberately narrow, so ordinary gatewayed/forwarded traffic
// can't false-positive: the packet must be plain UDP, its source port must
// be one of this node's own bound listen ports (no other local process can
// send from those while gravinet holds them), and its destination
// address:port must match a live peer session endpoint or a currently-dialed
// seed on this network — i.e. an address gravinet itself sends underlay
// datagrams to. Only the (rare, in healthy operation: zero) packets passing
// the cheap port checks ever pay for the endpoint scan.
func (e *Engine) isUnderlayLoop(ns *netState, pkt []byte, dst netip.Addr) bool {
	srcPort, dstPort, ok := udpPorts(pkt)
	if !ok || !e.ownUDPPort(srcPort) {
		return false
	}
	target := netip.AddrPortFrom(dst, dstPort)
	// Snapshot sessions under ns.mu, but read each endpoint (ps.mu) only
	// after releasing it — same discipline as resyncAllBypassRoutes, so no
	// ns.mu→ps.mu ordering is introduced here.
	ns.mu.RLock()
	for _, s := range ns.seeds {
		if sameUnderlayAddrPort(s, target) {
			ns.mu.RUnlock()
			return true
		}
	}
	ns.mu.RUnlock()
	if snap := ns.fwd.Load(); snap != nil {
		for _, ps := range snap.byNode {
			if sameUnderlayAddrPort(ps.ep(), target) {
				return true
			}
		}
	}
	return false
}

// noteUnderlayLoop counts a dropped looped underlay datagram and warns —
// throttled to once per ~10s, since the whole point of the drop is that
// these can arrive at line rate — with enough context for an operator to go
// find the route that's capturing underlay traffic. The bypass-route
// machinery (see meshRouteCovers and fulltunnel.go) should normally have
// kept the loop from forming at all; this fires when it couldn't (platform
// without a gateway backend, a route gravinet didn't install itself, a
// bypass install that failed and was logged at the time).
func (e *Engine) noteUnderlayLoop(ns *netState, dst netip.Addr) {
	n := e.loopDrops.Add(1)
	now := time.Now().Unix()
	last := e.loopWarnUnix.Load()
	if now-last < 10 || !e.loopWarnUnix.CompareAndSwap(last, now) {
		return
	}
	e.log.Warnf("mesh: dropped gravinet's own underlay datagram to %s read back from the overlay interface on net %x (%d dropped since start) — a route on this host is steering mesh underlay traffic into the tunnel itself; check `ip route get %s` for a mesh-installed route covering that peer endpoint", dst, ns.spec.ID, n, dst)
}

// parseSrc returns the source address of an IPv4/IPv6 packet.
func parseSrc(p []byte) (netip.Addr, bool) {
	if len(p) < 1 {
		return netip.Addr{}, false
	}
	switch p[0] >> 4 {
	case 4:
		if len(p) < 20 {
			return netip.Addr{}, false
		}
		return netip.AddrFrom4([4]byte{p[12], p[13], p[14], p[15]}), true
	case 6:
		if len(p) < 40 {
			return netip.Addr{}, false
		}
		var a [16]byte
		copy(a[:], p[8:24])
		return netip.AddrFrom16(a), true
	}
	return netip.Addr{}, false
}
