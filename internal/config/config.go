// Package config is the single source of truth for gravinet. Every tunable in
// the spec lives here. The running daemon holds the config behind an atomic
// pointer so the web admin can swap in a new version without a restart; live
// subsystems subscribe to changes and re-apply only what actually moved.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gravinet/internal/hostnet"
	"gravinet/internal/protocol"
)

// DefaultUDPPort is tried first; AltUDPPorts are tried in order if the
// primary cannot bind or a peer is unreachable on it.
//
// 65432 sits in the IANA dynamic/private range (49152-65535), so it carries no
// registered-service assignment to collide with — and it deliberately isn't
// 51820 (WireGuard), 41641 (Tailscale), 9993 (ZeroTier), or any other overlay's
// well-known port, so gravinet doesn't masquerade as something it isn't. The
// descending 6-5-4-3-2 is just easy to remember. Any value works; this is only
// the out-of-the-box default and is changeable live under Settings.
const DefaultUDPPort = 65432

// DefaultTCPPort is the default TCP/TLS listen port. It mirrors
// the UDP port so a node listens on the same number on both transports by
// default; set tcp_fallback_port to anything (e.g. 443) to change it.
const DefaultTCPPort = 65432

// DefaultControlSocket is the local IPC endpoint used by the CLI. It's
// platform-specific — see socket_linux.go / socket_bsd.go / socket_windows.go /
// socket_other.go — since "/run" is a Linux (systemd/FHS) convention that
// doesn't exist by default on macOS, FreeBSD, or Windows.

// AltUDPPorts are well-known UDP ports likely to traverse restrictive
// middleboxes when the primary is blocked.
var AltUDPPorts = []int{443, 4500, 3478, 1194, 500, 53}

// Config is the whole daemon configuration.
type Config struct {
	// Node identity & global behavior.
	// NodeID identifies this node in every handshake. Generated and written
	// back to the file at startup when empty (cmd/gravinet's
	// ensureNodeIdentity): it has to be stable across restarts and distinct
	// between nodes, and two nodes sharing the empty id cannot peer at all.
	NodeID string `json:"node_id"`
	// Hostname is advertised to peers. Taken from the OS hostname and written
	// back at startup when empty, so after the first run it is a fixed value
	// in the file rather than one that follows the OS: rename the host and
	// this keeps the old name until it is edited, or cleared to re-detect.
	Hostname string `json:"hostname"`
	LogLevel string `json:"log_level"`

	// Underlay listening. A node listens on a set of UDP ports and a set of
	// TCP/TLS ports. That is the whole model — there is no primary and no
	// fallback, because neither was ever a real distinction: a peer is
	// reachable at some set of {address, protocol, port}, and any of them
	// might work.
	//
	// The old shape (PrimaryPort + ExtraListenPorts, TCPPort +
	// ExtraTCPListenPorts) encoded a hierarchy the network does not have, and
	// the hierarchy leaked. Because TCP was "derived from" UDP, the engine
	// kept re-deriving a peer's TCP port from things that were not that
	// peer — our own config, an unrelated session at the same IP — and two
	// nodes behind one NAT ended up on one socket. See v788.
	//
	// UDPPorts empty turns UDP off entirely (the "-" sentinel in the web
	// admin's UDP port field); TCPPorts empty turns TCP off. Validate refuses
	// both being empty, since the node needs at least one live transport.
	// Binding is best-effort per port: one that can't bind (privileged, in
	// use) is skipped with a warning, never fatal, so a list is safe to widen.
	// Replies go back out the port a peer arrived on.
	//
	// The first entry in each list is what this node advertises to peers as
	// its canonical port. That is a presentation choice, not a tier: every
	// bound port answers identically, and a peer may reach this node on any
	// of them.
	// No omitempty, deliberately. "This transport is off" is an empty list,
	// and with omitempty an empty list is written as nothing at all — which on
	// the next load is indistinguishable from a config that predates these
	// keys, so the migration would re-derive defaults and silently reopen a
	// port the operator had closed. Written as [] it round-trips.
	UDPPorts []int `json:"udp_ports"`
	TCPPorts []int `json:"tcp_ports"`

	// The pre-v789 spelling (primary_port, extra_listen_ports,
	// tcp_fallback_port, disable_tcp_fallback, extra_tcp_listen_ports) is
	// still read from disk — see legacyPorts and Load — but deliberately has
	// no field here. Keeping the fields would have let every existing reader
	// go on compiling while silently seeing zero once the migration cleared
	// them, which is the worst possible outcome for a schema change: no
	// error, no warning, just a node that quietly binds nothing. Removing
	// them turns each one into a compile failure that has to be looked at.

	// EnableUPnP turns on gravinet's own best-effort UPnP IGD port-forwarding
	// helper: on startup, gravinet asks the LAN gateway (via UPnP, if it
	// supports it and has UPnP enabled) to forward every port this node
	// actually listens on — the primary UDP port, the TCP/TLS port,
	// and any configured extra listen ports — from its WAN side to this
	// host. This is the same "auto-configure my router" convenience many
	// P2P/VPN tools offer, so a node behind a home/office router with no
	// manual port forward configured can still be reached directly by
	// peers. See internal/upnp for the client/lifecycle implementation.
	//
	// Off by default: unlike the firewall/NAT settings elsewhere in this
	// struct (this host's own kernel), turning this on reaches out and asks
	// a *different* device — the LAN gateway — to reconfigure itself, which
	// not every operator wants happening automatically. Plenty of routers
	// have UPnP disabled or entirely absent anyway; that's a silent no-op
	// here, not an error, and each port is mapped independently (one being
	// rejected doesn't stop the rest — every discovery/mapping failure is
	// logged and retried in the background, never fatal to startup). Every
	// mapping is best-effort removed again on a clean shutdown. Takes
	// effect on the next restart, not live — see webadmin's
	// handleUPnPSetting.
	EnableUPnP bool `json:"enable_upnp,omitempty"`

	EnableIPv4 bool `json:"enable_ipv4"` // underlay v4
	EnableIPv6 bool `json:"enable_ipv6"` // underlay v6
	// WorkerThreads is the size of the outbound worker pool and the number of
	// SO_REUSEPORT receive sockets. 0 selects DefaultWorkerThreads, capped at
	// NumCPU so a small machine is not oversubscribed (and a single-core host
	// still resolves to 1, which routes to tunLoopSerial — the pooled path
	// measured ~70% slower there; see mesh.tunLoop).
	WorkerThreads int `json:"worker_threads"`
	// TunQueues opts each overlay interface into Linux's IFF_MULTI_QUEUE (see
	// internal/tun.NewMultiQueue): that many independent read queues on one
	// tun device, each with its own goroutine, instead of the single
	// goroutine/single read()-syscall-per-packet path every network gets
	// otherwise. Unlike worker_threads (parallelizes processing *after* one
	// read), this parallelizes the read itself — the actual origin-side
	// throughput ceiling for a single network's outbound traffic — but it
	// only helps aggregate throughput across many concurrent flows, since a
	// single flow still tends to land on one queue (kernel flow-hash queue
	// selection, done specifically to avoid reordering a flow's own
	// packets). 0 or 1 = off (today's exact single-queue behavior, and the
	// default): unlike worker_threads, this does not default to NumCPU()-1,
	// because it changes how the interface itself is opened rather than
	// adding concurrency behind an unchanged read, and gravinet's own history
	// with that category of data-plane change (see docs/changelog.md's Phase
	// B/C entries) is why it ships opt-in here too. No effect on
	// platforms/configs where multi-queue TUN isn't implemented (everything
	// but Linux, today) — the operator gets one queue, silently, same as
	// leaving this unset.
	TunQueues int `json:"tun_queues,omitempty"`
	// EnableUDPGSO turns on UDP-side segmentation offload (UDP_SEGMENT send /
	// UDP_GRO receive; see internal/transport/gso_linux.go) — the config-driven
	// alternative to the GRAVINET_UDP_GSO=1 environment variable, which
	// predates this field and is still honored (either enables it; see
	// transport.Options.EnableUDPGSO). Off by default, same posture as
	// tun_queues and for the same reason: this project's own field history
	// with unproven data-plane changes (docs/changelog.md's Phase B/C
	// entries) is why the env var existed in the first place, gating it
	// behind deliberate operator action rather than a default. A config
	// field (and the Settings-panel toggle built on it) trades away some of
	// that deliberateness for discoverability — a judgment call, not a
	// verification result; nothing about exposing the switch changes how
	// unproven the mechanism itself still is on real hardware under real
	// load. Linux amd64/arm64 only; a harmless no-op elsewhere.
	// Nil means enabled (the default since v665); false explicitly disables.
	// Pointer rather than plain bool so "absent" and "set to false" stay
	// distinguishable, same shape as PMTUDiscovery above.
	EnableUDPGSO *bool `json:"udp_gso,omitempty"`

	// IPForwarding controls whether the daemon turns on host IPv4/IPv6 forwarding
	// at startup (the on-ramp for redistributed routes and NAT). nil means the
	// default, which is enabled; set to false to leave host forwarding untouched.
	// The prior value is restored on a clean shutdown.
	IPForwarding *bool `json:"ip_forwarding,omitempty"`

	// DisableRedirects controls whether the daemon turns off host acceptance
	// and (where the platform exposes it) sending of ICMP IPv4/IPv6 redirects
	// at startup. An unauthenticated ICMP redirect can rewrite a host's route
	// table — see internal/ipfwd's DisableRedirects doc comment for the actual
	// per-platform knobs — which matters more for gravinet than for an
	// ordinary host precisely because IPForwarding above may have this node
	// routing real traffic for other peers. nil means the default, which is
	// disabled (i.e. redirects are turned off); set to false to leave the
	// host's redirect settings untouched. The prior values are restored on a
	// clean shutdown, same shape as IPForwarding.
	DisableRedirects *bool `json:"disable_redirects,omitempty"`

	// RouteAdvInterval is how often (seconds) this node re-advertises its own
	// redistributed routes to the mesh. Re-advertising heals advertisements lost
	// to packet drops, lets a peer that joined or lifted a reject pick the route
	// back up without a reconnect, and refreshes routes after a transient. 0 or
	// unset means the default (10s); the minimum honored value is 1s.
	RouteAdvInterval int `json:"route_advertise_interval,omitempty"`

	// KeepaliveInterval is how often (seconds) this node sends a NAT
	// keepalive to each connected peer — also what per-peer RTT tracking
	// (used for relay scoring) samples ride on. 0 or unset means the
	// default (10s); the minimum honored value is 1s. Lowering this
	// detects a dead link faster (via PeerTimeout, see below) at the cost
	// of more keepalive traffic; raising it saves traffic at the cost of
	// slower dead-link detection.
	KeepaliveInterval int `json:"keepalive_interval,omitempty"`

	// PeerTimeout is how long (seconds) a session may go without any
	// received traffic before it's considered dead and torn down — this is
	// what governs how long a peer that's gone silent keeps showing as
	// connected in the peers table. 0 or unset means the default (30s). An
	// explicit value below the (possibly also-configured) keepalive
	// interval is clamped up to it: a session timing out before a single
	// keepalive round trip could even complete would just cause constant
	// unnecessary reconnection thrashing, not faster failure detection.
	PeerTimeout int `json:"peer_timeout,omitempty"`

	// FirewallExempts is the node-global always-allowed list: traffic classes the
	// firewall rulebase can never block, applied to every network so a broad deny
	// can't lock the operator out of remote management or the routing protocols
	// that keep the overlay glued together. A nil list means the built-in defaults
	// (see DefaultFirewallExempts); an explicit empty list disables all exemptions.
	FirewallExempts []FirewallExempt `json:"firewall_exempt,omitempty"`

	// FirewallObjects / FirewallServices are the node-global reusable address-
	// object and service catalogs every network's firewall rules resolve their
	// src/dst/services references against (see FirewallRule.Src/Dst/Services) —
	// one catalog, shared by every network on this node, not duplicated per
	// network. A rule always lives on a specific network (it only makes sense
	// applied to that network's traffic), but the named objects/services it
	// references are node-wide: the same "google.com" or "HTTPS" definition a
	// rule on one network names is the same definition a rule on any other
	// network here would name too, edited in one place.
	FirewallObjects  []FirewallObject  `json:"firewall_objects,omitempty"`
	FirewallServices []FirewallService `json:"firewall_services,omitempty"`
	// ObjectsCatalogSeeded / ServicesCatalogSeeded record that the admin UI's
	// well-known object/service catalog (FW_COMMON_WILDCARD_OBJECTS /
	// FW_COMMON_SERVICES in internal/webadmin's embedded JS) has already been
	// populated into FirewallObjects/FirewallServices once, node-wide. Purely
	// local bookkeeping the packet-filter engine never reads and the persist
	// hook never re-derives (unlike FirewallObjects/FirewallServices
	// themselves, which the engine is the source of truth for) — its only
	// reader is the admin UI's auto-populate pass, which uses it to populate
	// exactly once, ever, for this node, and then leave the operator's own
	// additions/removals alone from then on: without it, a deleted well-known
	// entry would silently reappear on every visit to a firewall tab, since
	// there'd be nothing on disk distinguishing "never populated" from
	// "populated, then deliberately edited."
	ObjectsCatalogSeeded  bool `json:"objects_catalog_seeded,omitempty"`
	ServicesCatalogSeeded bool `json:"services_catalog_seeded,omitempty"`

	// UnderlayMTU caps the size of a single UDP datagram we put on the wire.
	// Overlay packets larger than what fits are fragmented at the application
	// layer and reassembled by the peer, so the jumbo tunnel MTU works
	// across underlays that can't carry it — notably mobile/5G paths that drop
	// IP-fragmented or oversized datagrams. Default 1280 (the IPv6 minimum, safe
	// almost everywhere); raise it on clean networks for less per-packet overhead.
	UnderlayMTU int `json:"underlay_mtu,omitempty"`

	// UnderlayMTUMax is the ceiling for path-MTU discovery: the daemon probes
	// each peer's path for the largest datagram it carries intact, between
	// UnderlayMTU (the floor/fallback) and this value, and fragments to whatever
	// it finds. Default 9000 (so jumbo underlays are discovered automatically);
	// the effective ceiling is also bounded by the local interface. Set equal to
	// UnderlayMTU to pin a fixed size.
	UnderlayMTUMax int `json:"underlay_mtu_max,omitempty"`

	// SocketBuffer is the per-UDP-socket SO_RCVBUF/SO_SNDBUF target in bytes.
	// Accepts megabytes or bytes: a value of 1024 or less is read as MB, so
	// "socket_buffer": 32 is 32 MiB (see SocketBufferMBThreshold). Default
	// 16 MiB; clamped to [256 KiB, 256 MiB]. The daemon runs as root and sets
	// it with SO_RCVBUFFORCE, so net.core.rmem_max does not cap it.
	//
	// This is a real throughput knob at multi-Gbps, not a micro-optimisation.
	// The receive goroutine drains the socket while also decrypting, filtering
	// and writing to the TUN, so the buffer has to cover any stall in that
	// pipeline; when it doesn't, the kernel drops datagrams and the counter is
	// UdpRcvbufErrors, which TCP sees as ordinary loss. At jumbo sizes a
	// buffer holds buffer/~8900 datagrams — 4 MiB was only ~470, about 8 ms at
	// 4 Gbps, and measurably overflowed on a live link.
	SocketBuffer int `json:"socket_buffer,omitempty"`

	// PMTUDiscovery enables the probe-based path-MTU discovery described above.
	// Nil/true means enabled; false pins the underlay size at UnderlayMTU.
	PMTUDiscovery *bool `json:"pmtu_discovery,omitempty"`

	// RestartOnUnderlayChange makes the daemon restart itself when it detects
	// this host's own underlay source address changed (a Wi-Fi/cellular roam),
	// forcing a from-scratch re-establishment of every peer, socket, and route.
	// It's a deliberately blunt recovery for roams that in-process patch-up
	// doesn't fully heal. Nil/true means enabled; false disables it. The restart
	// is muted for the first ~45s of each process's life so a link flapping right
	// after boot can't spin the service, and it goes through the service manager
	// where one is managing gravinet (falling back to an in-place re-exec on
	// Unix when run interactively). Not yet supported for interactive runs on
	// Windows — see cmd/gravinet's selfRestart.
	RestartOnUnderlayChange *bool `json:"restart_on_underlay_change,omitempty"`

	// NATStateTimeout is the global idle lifetime (seconds) of a tracked NAT
	// connection before its mapping is reclaimed. 0 uses the default (120s). It
	// applies to every network's NAT and replaces the former per-network setting.
	NATStateTimeout int `json:"nat_state_timeout,omitempty"`

	// NAT is this node's address translation, node-global since v953. See the
	// NAT type's doc comment for why it is not per mesh network, and
	// natRuleAppliesToOverlay for how a rule reaches one when it needs to.
	NAT NAT `json:"nat,omitempty"`

	// QoS is this node's traffic classifier, node-global since v954. See the
	// QoS type's doc comment, and QoSRule.Scope for how a rule reaches one
	// overlay rather than all of them.
	QoS QoS `json:"qos,omitempty"`

	// Firewall is this node's rulebase, node-global since v957. See the
	// Firewall type's doc comment, and FirewallRule.Scope for how a rule
	// reaches one overlay rather than all of them.
	Firewall Firewall `json:"firewall,omitempty"`

	// Shaping is this node's bandwidth limits, one entry per interface.
	// Node-global and keyed by interface name since v960 — see IfaceShaping.
	Shaping []IfaceShaping `json:"shaping,omitempty"`

	// ShapingDisabled is the node-global off switch for shaping, the
	// counterpart of NAT.Enabled / QoS.Enabled / Firewall.Enabled, added in
	// v968 so Traffic > Shaping carries the same enabled/disabled pill beside
	// its title that every other page in that group does.
	//
	// Inverted — disabled rather than enabled — so the zero value keeps
	// shaping on. Shaping predates this switch, so every config already out
	// there has entries and no flag; an Enabled bool would read false on all
	// of them and silently unshape every upgraded node. Same reason
	// DiscoveryConfig.Disabled is spelled that way.
	//
	// Flipping it leaves the entries alone: it is the "flip the flag, leave
	// the rules alone" split NAT and QoS already use, so an operator can lift
	// every cap for an afternoon and put them all back without retyping a
	// rate. Enforcement is at the two points that consume shaping —
	// ShapingThrottle for the tunnel path and KernelShaping for the qdisc
	// path — rather than in ShapingFor, which the editor also goes through
	// and which must keep working while the feature is off.
	ShapingDisabled bool `json:"shaping_disabled,omitempty"`

	// Throttle is the pre-v960 node default bandwidth limit. Retained as a
	// field so an existing config still parses; Config.Validate hoists it
	// into Shaping and clears it so it is never written back out.
	//
	// It was introduced in v955 as a node-global rate applied to every mesh
	// network without an override of its own (Network.Throttle, v956). v960
	// replaced the two-level default/override model with one flat list keyed
	// by the thing a rate is actually applied to.
	Throttle Throttle `json:"throttle,omitempty"`

	// LogFile is where the daemon mirrors its log output (in addition to the
	// console). Empty means the default: "gravinet.log" alongside the config
	// file. Set an explicit path to override, or "-"/"none" to disable the file.
	LogFile string `json:"log_file,omitempty"`

	// LogMaxSize caps the log file: once a write would push it past this size,
	// the oldest lines are dropped from the front to make room (FIFO), so the
	// file is a rolling window of the most recent output rather than growing
	// without bound. Accepts a human size with an optional unit suffix — "200M",
	// "99K", "1G", or a bare byte count — and is what the web admin's Logging >
	// Log Size box writes. Empty means the default (200M). This is the modern
	// replacement for the LogMaxMB/LogKeep numbered-rotation pair below; when
	// LogMaxSize is set it takes precedence and the file runs in FIFO mode with
	// no numbered backups.
	LogMaxSize string `json:"log_max_size,omitempty"`

	// LogMaxMB is the size (in MB) the log file may reach before it rotates; 0
	// means the default. LogKeep is how many rotated files to retain
	// (<path>.1 … <path>.N); 0 means the default (5). Set LogKeep to a negative
	// value via the helper to keep none (rotate by truncation). Superseded by
	// LogMaxSize above for setting the cap; retained for back-compat with
	// existing configs and the numbered-backup rotation mode.
	LogMaxMB int `json:"log_max_mb,omitempty"`
	LogKeep  int `json:"log_keep,omitempty"`

	// ReadmeFile overrides where the web admin reads the project README from. When
	// empty the daemon looks in install-standard locations (see ReadmePath).
	ReadmeFile string `json:"readme_path,omitempty"`

	// LicenseFile overrides where the web admin reads the LICENSE from; empty
	// means search the install-standard locations (see LicensePath).
	LicenseFile string `json:"license_path,omitempty"`

	// GettingStartedFile overrides where the web admin reads
	// getting-started.md from; empty means search the install-standard
	// locations (see GettingStartedPath). Same shape as ReadmeFile/LicenseFile
	// — the field name/JSON key predate getting-started.md itself (this used
	// to point at a separate getting-started.html, removed once the web
	// admin's Getting Started page rendered its own markdown copy natively
	// instead of iframing that file; keeping one file, not two, made the
	// html version redundant, so it's gone rather than kept in sync forever).
	GettingStartedFile string `json:"getting_started_path,omitempty"`

	// APIDocFile overrides where the web admin reads API.md (the HTTP API
	// reference shown under Info -> API) from; empty means search the
	// install-standard locations (see APIDocPath). Same shape as
	// ReadmeFile/LicenseFile/GettingStartedFile: the page renders this file's
	// own markdown natively rather than keeping a second, in-app copy that
	// could drift from it.
	APIDocFile string `json:"api_doc_path,omitempty"`

	// Networks are independent overlays multiplexed on this node.
	Networks []Network `json:"networks"`

	// ConfigHistoryLimit caps how many automatic + manual config snapshots
	// (see internal/config/history.go) are kept, FIFO — oldest pruned first
	// once the count exceeds this. 0 uses the default (250).
	ConfigHistoryLimit int `json:"config_history_limit,omitempty"`
	// DDNS is dynamic DNS registration: this node publishing its own name and
	// addresses into the zone it searches, on a timer. Off unless an interval
	// is set. See DDNSConfig.
	DDNS DDNSConfig `json:"ddns,omitempty"`

	// WebAdmin is the hot-config administration interface.
	WebAdmin WebAdmin `json:"web_admin"`

	// ControlSocket is the local IPC endpoint for the CLI (ban/unban/list).
	// A filesystem path => Unix socket; a host:port => TCP.
	//
	// Empty means "use the platform default" (see NormalizeControlSocket), which
	// is the scaffolded state — omitempty keeps it out of the file entirely so a
	// SaveTo round-trip can't silently freeze today's default into the config and
	// strand it there across a future correction. Both the daemon and the CLI
	// resolve this same way, so they cannot disagree about where the socket is.
	ControlSocket string `json:"control_socket,omitempty"`

	// Handshake-layer brute-force protection (separate from distributed bans).
	AuthBan BanPolicy `json:"auth_ban"`

	// Managed turns on remote management ("managed" cluster mode). Off by default.
	// When on, the node advertises itself to mesh peers as remotely manageable and
	// accepts web-admin management arriving over the overlay from a mesh peer that
	// is itself in Manager mode (see Manager below) — the mesh PSK is the trust
	// boundary for reaching the overlay at all, and Manager mode is the boundary
	// for who's allowed to drive that management once reached. It also lets this
	// node's web GUI configure other managed peers selected from the header
	// dropdown, provided this node is also in Manager mode.
	Managed bool `json:"managed,omitempty"`

	// Manager turns on this node's ability to manage other Managed peers: browse
	// them in the header dropdown and proxy admin calls to them. Off by default,
	// like Managed. Manager governs the *outbound* direction only — whether this
	// node can be selected from someone else's dropdown is entirely Managed's
	// concern. A node can be Manager without being Managed (a bastion/admin-console
	// node: it can reach out and drive the rest of the fleet, but nothing can
	// manage it without a normal login), Managed without being Manager (manageable,
	// but can't itself manage anyone), both, or neither. See docs/ARCHITECTURE.md's
	// "Managed clustering" section for the full authorization model.
	Manager bool `json:"manager,omitempty"`

	// Upgrade governs binary distribution across the mesh (internal/upgrade).
	Upgrade Upgrade `json:"upgrade,omitempty"`

	// BGP is this node's dynamic-routing configuration, rendered into FRR's
	// frr.conf and applied by driving the FRR daemon (see internal/webadmin's
	// frr.go). It's node-global — one BGP speaker per host — not per network,
	// the same way the firewall object/service catalog is. gravinet doesn't
	// itself speak BGP; it owns the config and the daemon lifecycle and lets
	// FRR run the sessions. Empty/disabled by default; when disabled the
	// rendered config carries no BGP block and bgpd is switched off in
	// /etc/frr/daemons. Ported from parapet's Bgp model.
	BGP BGPConfig `json:"bgp,omitempty"`
	// RouterAdvert configures IPv6 router advertisements (radvd on Linux,
	// rtadvd on FreeBSD). Off unless Enabled and at least one interface is
	// listed; gravinet writes no daemon config and touches no service until
	// then, so installing gravinet on a host already running radvd by hand
	// changes nothing until an operator opts in.
	RouterAdvert RAConfig `json:"router_advert,omitempty"`
	// DHCP is this node's DHCP relay: which client-facing links it forwards
	// from, and to which servers. Node-global like BGP above. gravinet also
	// served leases of its own through Kea until v988; see DHCPMode for what
	// is left of that field.
	DHCP DHCPConfig `json:"dhcp,omitempty"`
	// HostInterfaces is host addressing gravinet has been told to own, so it
	// travels with the configuration: back it up, restore it, and the node's
	// own IP addresses come back with everything else. Without this, a
	// restored config brought back every mesh setting and left the host
	// unreachable at the address the operator expected — which is what this
	// field exists to fix.
	//
	// Opt-in per interface, and only ever populated by an operator editing
	// that interface through gravinet. An interface not listed here is not
	// managed, not reconciled, and not touched: gravinet is a guest on a host
	// whose networking is usually configured by something else, and a config
	// section that quietly claimed every NIC would be a much larger promise
	// than the one being made.
	HostInterfaces []HostIface `json:"host_interfaces,omitempty"`

	// HostVLANs are 802.1Q tagged interfaces gravinet creates on this host.
	//
	// Separate from HostInterfaces because the two answer different
	// questions. A HostIface record says what addressing an interface that
	// already exists should have; a HostVLAN says the interface should exist
	// at all. Once created, a tagged interface is addressed through
	// HostInterfaces like any other — there is no second addressing model
	// here, and the interfaces page shows it in the same table as its parent.
	//
	// This is the only kind of interface gravinet creates on the host, and it
	// creates them the way it creates its own mesh devices: at every startup
	// and reload, before addressing is reconciled onto them. That is what
	// makes them survive a reboot. It also means they exist only while
	// gravinet does — deliberately, because the alternative is writing VLAN
	// stanzas into netplan or NetworkManager, which is co-owning the file
	// that decides whether the host comes back with any networking at all.
	HostVLANs []HostVLAN `json:"host_vlans,omitempty"`
	// HostSettings is the rest of this host's own configuration that gravinet
	// edits on an operator's behalf — syslog forwarding, timezone and NTP,
	// hostname and DNS. It is here for the same reason HostInterfaces is:
	// without it, a restored backup brought back every mesh setting and left
	// the node with the wrong clock, the wrong resolvers and no log
	// forwarding, which is not what "restore my configuration" means.
	//
	// Every field is opt-in. Each is populated only when an operator changes
	// that setting through gravinet, and an unset field is not reconciled and
	// not touched — a host whose DNS is managed by DHCP keeps it.
	// A pointer, because encoding/json's omitempty does not apply to structs:
	// a value field would write "host_settings":{} into every config file on
	// this host, changing files nobody asked to change.
	HostSettings *HostSettings `json:"host_settings,omitempty"`

	// SNMP runs a read-only SNMPv2c agent (net-snmp's snmpd) on this host —
	// System > SNMP. It's node-global, like BGP: one agent per host, not
	// per network. Unlike BGP (which FRR runs and gravinet only configures)
	// and unlike parapet's own SnmpManager (which spawns and supervises
	// snmpd as a direct child of its own process), gravinet manages snmpd
	// as an ordinary OS service — write snmpd.conf, then enable/restart or
	// disable/stop it through systemctl/service/rcctl/launchctl/sc, the
	// same way it already treats FRR. A child of gravinet's own process
	// would die every time gravinet itself restarts (a config change, an
	// upgrade, ...), which happens far more often than an operator wants
	// SNMP monitoring to blink; a real OS service persists across that.
	// Ported from parapet's Snmp model — see internal/service/snmp.go.
	SNMP SNMPConfig `json:"snmp,omitempty"`

	// Discovery runs a link-layer discovery agent (lldpd, speaking LLDP and
	// optionally CDP) on this host — System > LLDP. Node-global,
	// like SNMP/BGP: one agent per host. Same architecture choice as SNMP,
	// for the same reason: gravinet manages lldpd as an ordinary OS service
	// rather than a child of its own process the way parapet's LldpManager
	// does, so it survives a gravinet restart instead of blinking on every
	// one. Ported from parapet's Discovery/DiscoveryIface models — see
	// internal/service/lldp.go.
	Discovery DiscoveryConfig `json:"discovery,omitempty"`

	// OSUpdates schedules automatic host OS package updates — System >
	// Upgrade's "OS updates" section. Deliberately unrelated to gravinet's
	// own binary-upgrade mechanism above it on that same page: this patches
	// whatever the host's package manager (apt/dnf/zypper/pacman/pkg/
	// pkg_add/softwareupdate) already manages, not gravinet itself. See
	// internal/service/osupdate.go.
	OSUpdates OSUpdateConfig `json:"os_updates,omitempty"`

	// path is where this config was loaded from / will be saved to.
	path string
}

// SNMPConfig is this node's read-only SNMPv2c agent configuration, rendered
// into net-snmp's snmpd.conf. Ported from parapet's Snmp model; grew from a
// single community string to a list in v736 so an agent can answer more
// than one community (e.g. a stricter one scoped by the monitoring
// system's own source-IP allowlist, alongside a looser one for ad-hoc
// polling) without giving every consumer the same string.
type SNMPConfig struct {
	Enabled bool `json:"enabled"`
	// Communities are the SNMPv2c read-only community strings this agent
	// accepts — each becomes its own rocommunity line in snmpd.conf. At
	// least one enabled entry with a non-empty string is required for the
	// agent to actually run — see IsRunnable. A disabled entry is kept in
	// the list but not written to snmpd.conf, the same zero-value-is-
	// enabled convention as FirewallRule.Disabled/FirewallExempt.Disabled
	// elsewhere in this package (and service.SyslogTarget.Disabled).
	Communities []SNMPCommunity `json:"communities,omitempty"`
	// Community is deprecated: the pre-v736 single-community field. Load
	// migrates any non-empty value into Communities (see Validate) and
	// clears this, so it is never written back out by a save that went
	// through this package — retained only so a config file from before
	// v736 still parses and its community string isn't silently dropped.
	Community string `json:"community,omitempty"`
	// ListenAddr is snmpd's listen spec, e.g. "udp:161" or "0.0.0.0:161".
	// Empty means snmpd's own default (udp:161 on every address).
	ListenAddr string `json:"listen_addr,omitempty"`
	// Location/Contact become snmpd.conf's sysLocation/sysContact.
	Location string `json:"location,omitempty"`
	Contact  string `json:"contact,omitempty"`
}

// SNMPCommunity is one SNMPv2c read-only community string this node's
// agent accepts.
type SNMPCommunity struct {
	Community string `json:"community"`
	Disabled  bool   `json:"disabled,omitempty"`
}

// IsRunnable reports whether this config is enough to actually start
// snmpd: enabled, with at least one enabled, non-empty community string
// (an agent with no active community can't answer anything, so there's no
// point starting it — matches parapet's own Snmp::is_runnable, extended
// from "the one community is non-empty" to "at least one is").
func (s SNMPConfig) IsRunnable() bool {
	if !s.Enabled {
		return false
	}
	for _, c := range s.Communities {
		if !c.Disabled && c.Community != "" {
			return true
		}
	}
	return false
}

// DiscoveryIface is one interface row in the LLDP/CDP configuration table.
// Ported from parapet's DiscoveryIface.
type DiscoveryIface struct {
	Name string `json:"name"`
	// LLDP advertises and receives IEEE 802.1AB LLDP frames on this interface.
	LLDP bool `json:"lldp"`
	// CDP additionally advertises and receives CDP on this interface.
	// Meaningful without LLDP also being on (lldpd's -c flag is a global
	// agent-wide switch, not conditioned on a specific interface's LLDP
	// state), but the UI nudges toward turning LLDP on too, the same as
	// parapet's own page comment does, since CDP alone with no LLDP is an
	// unusual choice.
	CDP bool `json:"cdp"`
}

// DiscoveryConfig is this node's link-layer discovery (LLDP/CDP)
// configuration. gravinet runs and manages an lldpd agent that advertises
// and receives these protocols per-interface. Ported from parapet's
// Discovery model.
type DiscoveryConfig struct {
	// Disabled is the master off switch, independent of which interfaces
	// are picked below — the same "flag separate from what it gates" split
	// SNMPConfig.Enabled and NAT/QoS/Bandwidth's per-network Enabled
	// already use relative to their own fields/rules. Flipping it just
	// starts or stops lldpd (see service.ApplyLLDP) without touching
	// Interfaces at all.
	//
	// The zero value (false) means enabled — not a stylistic choice, but
	// what makes this field backward compatible: a config saved before it
	// existed, with interfaces already picked, was running with no
	// separate flag at all, so it must keep running after an upgrade
	// rather than reading as freshly disabled the moment this field
	// appears in the struct. Same "zero value already means what the old
	// behavior meant" polarity RejectRoute.Disabled uses elsewhere in this
	// file, for the identical reason.
	Disabled bool `json:"disabled,omitempty"`
	// Interfaces holds only the rows an operator has actually touched —
	// sparse, not one entry per host interface. An interface absent from
	// this list is simply off (lldp:false, cdp:false) — an "absence means
	// off" shape, so the web UI merges this against the host's live
	// interface list (the same one NAT's masquerade-interface picker already fetches via
	// /api/interfaces) to show a complete table with nothing configured
	// read as "off" rather than "unknown".
	Interfaces []DiscoveryIface `json:"interfaces,omitempty"`
}

// IsRunnable reports whether lldpd should actually run: not Disabled, and
// at least one non-loopback interface has LLDP or CDP enabled. Mirrors
// parapet's Discovery::is_runnable, plus the Disabled gate parapet's own
// model has no equivalent of. lo is excluded here (not just at the UI
// layer) since LLDP/CDP are link-layer discovery protocols and loopback has
// no link partner to discover — the same reasoning parapet's own model
// applies, and a defense against a hand-edited config file naming "lo"
// directly.
func (d DiscoveryConfig) IsRunnable() bool {
	if d.Disabled {
		return false
	}
	for _, i := range d.Interfaces {
		if i.Name != "lo" && (i.LLDP || i.CDP) {
			return true
		}
	}
	return false
}

// AnyCDP reports whether at least one non-loopback interface has CDP
// enabled — lldpd's -c flag is global, not per-interface, so this decides
// whether to pass it at all. Mirrors parapet's Discovery::any_cdp.
func (d DiscoveryConfig) AnyCDP() bool {
	for _, i := range d.Interfaces {
		if i.Name != "lo" && i.CDP {
			return true
		}
	}
	return false
}

// OSUpdateConfig schedules automatic host OS package updates. Applies
// whatever's available via the host's package manager (a plain "update
// everything installed" pass — not gravinet itself, and not an OS version
// upgrade); never reboots on its own, even if the update implies one wants
// to happen — that stays a deliberate, separate decision made from System >
// Power. Windows isn't supported: there's no simple, dependency-free way to
// drive Windows Update from a script the way there is for every package
// manager this covers.
type OSUpdateConfig struct {
	Enabled bool `json:"enabled"`
	// Cadence: "daily", "weekly", or "monthly". Meaningless unless Enabled.
	Cadence string `json:"cadence,omitempty"`
	// Weekday: 0=Sunday..6=Saturday. Only consulted when Cadence is "weekly".
	Weekday int `json:"weekday,omitempty"`
	// DayOfMonth: 1-28 — capped there, not 31, so every month actually has
	// that day rather than "the 31st" silently skipping February and every
	// 30-day month. Only consulted when Cadence is "monthly".
	DayOfMonth int `json:"day_of_month,omitempty"`
	// Hour/Minute: time of day to run, host-local, 24-hour. The zero value
	// (0:00, midnight) is a legitimate choice an operator might actually
	// pick, not treated as "unset" — service/osupdate.go's own default
	// (applied only when Enabled is first turned on with no time chosen
	// yet) is 03:00, not encoded here.
	Hour   int `json:"hour"`
	Minute int `json:"minute"`
}

// BGPConfig is this node's BGP configuration and the BFD settings attached to
// it. It maps onto an FRR `router bgp <asn>` block: the local AS and router
// id, the peers to bring up (each optionally with an MD5 password and BFD),
// the prefixes to originate, and whether to redistribute connected/static
// routes into BGP. BFD (Bidirectional Forwarding Detection) gives sub-second
// neighbor-failure detection and is set per neighbor — there is no global
// toggle; a fresh neighbor defaults to BFD on (see the web UI's isNewCfg
// handling), but each one is its own setting from then on.
type BGPConfig struct {
	Enabled   bool          `json:"enabled"`
	ASN       uint32        `json:"asn"`
	RouterID  string        `json:"router_id,omitempty"`
	Neighbors []BGPNeighbor `json:"neighbors,omitempty"`
	Networks  []string      `json:"networks,omitempty"`
	// RedistributeConnectedRoutes/RedistributeStaticRoutes select exactly
	// which of this host's currently-connected/static routes (as FRR/zebra
	// see them right now — see showIPRouteConnected/showIPRouteStatic) get
	// redistributed into BGP; empty means none. This replaced a blanket
	// on/off toggle (FRR's own `redistribute connected`/`redistribute
	// static` with no filter) because that swept in every such route on the
	// box indiscriminately — there was no way to advertise just one LAN
	// subnet without also advertising every other connected/static route
	// gravinet had no opinion on. Rendered as an FRR route-map matched
	// against a prefix-list built from this list (see renderFRR), the same
	// selective-redistribution shape RedistributeMeshRoutes already used
	// for mesh routes, just needing FRR's route-map machinery here since —
	// unlike a mesh route — a connected/static route has no `network`
	// statement equivalent to render directly. A CIDR here that's since
	// stopped being an actual connected/static route (interface went away,
	// route was removed) simply matches nothing; it isn't pruned from this
	// list automatically, so re-adding the same route later doesn't lose
	// the earlier selection.
	RedistributeConnectedRoutes []string `json:"redistribute_connected_routes,omitempty"`
	RedistributeStaticRoutes    []string `json:"redistribute_static_routes,omitempty"`
	// RedistributeMeshRoutes selects exactly which of the CIDRs currently
	// listed on the Mesh Routes page (Traffic > Mesh Routes' "Advertise"
	// table, i.e. each enabled Route on an enabled Network) get advertised
	// into BGP; empty means none. This is deliberately not FRR's
	// `redistribute kernel`: a mesh-learned route is installed into the OS
	// routing table like any other kernel route (see internal/mesh's
	// syncRoute/AddRoute), so a blanket `redistribute kernel` would sweep in
	// every other kernel-table entry on the box too (a manual static route,
	// another VPN's routes, whatever else is there) — not just the mesh's
	// own, and not just the subset actually wanted here. gravinet instead
	// renders one explicit `network` statement per selected mesh route (see
	// renderFRR/meshRouteCIDRs), keeping it live as routes are added,
	// removed, or enabled/disabled on the Mesh Routes page — not only when
	// this BGP config itself is next saved. A CIDR selected here that's
	// since stopped being advertised on the Mesh Routes page is simply
	// never rendered (effectiveBGPNetworks intersects this list against
	// meshRoutes' current contents); same non-pruning reasoning as the two
	// fields above.
	RedistributeMeshRoutes []string `json:"redistribute_mesh_routes,omitempty"`
	// ASPrepend, when on, prepends this node's own ASN 2 times to the
	// AS-PATH of every route it advertises outbound to every BGP neighbor —
	// a route it originates (a Networks entry or a selected
	// RedistributeConnectedRoutes/RedistributeStaticRoutes/
	// RedistributeMeshRoutes prefix) or one it's re-advertising after
	// learning it from elsewhere, all the same, since it's applied as an
	// outbound route-map rather than at any one specific origination point
	// (see renderFRR). The classic inbound-traffic-engineering trick: a
	// longer AS-PATH is less preferred by a peer's best-path selection (all
	// else equal), so this makes every route this node advertises less
	// attractive without touching what it actually accepts or how it
	// selects its own best path — it only ever changes what gets sent out.
	// The prepend count is fixed at 2, not configurable — enough to
	// meaningfully lengthen the path in a typical multi-homed comparison
	// without a per-prepend-count UI for what's fundamentally a blunt,
	// binary policy ("make me less preferred" vs not).
	ASPrepend bool `json:"as_prepend,omitempty"`
	// KeepaliveTime and HoldTime are the BGP session timers, in seconds,
	// rendered as FRR's `timers bgp <keepalive> <hold>`. Keepalive is how often
	// a peer sends keepalive messages; hold is how long without any message
	// before the session is declared down. The conventional ratio is 1:3, and
	// gravinet defaults a new config to a fast 4s/12s (versus FRR's sluggish
	// 60s/180s) so a dropped peer is detected in seconds. 0 on either means
	// "unset" — the timers line is omitted and FRR uses its own defaults.
	KeepaliveTime uint32 `json:"keepalive_time,omitempty"`
	HoldTime      uint32 `json:"hold_time,omitempty"`

	// AutoBGP turns this node's BGP speaker into a self-numbering,
	// self-peering one for every other node on its mesh networks, instead of
	// a hand-maintained one. When on (see internal/webadmin/autobgp.go):
	//   - ASN is derived from this node's own first tunnel IPv4 address (across
	//     its networks, in NetworkIDs order) if not already set — a
	//     predictable mapping into the 4-byte private ASN range
	//     (4200000000-4294967294, RFC 6996), not a real public AS.
	//   - RouterID is set to that same tunnel IPv4 address if not already set.
	//   - Enabled is forced on — AutoBGP numbering a speaker that never runs
	//     would be pointless.
	//   - one Neighbor per currently-connected mesh peer is kept in sync: its
	//     first tunnel IPv4 and/or IPv6 address (whichever it has — v4-only,
	//     v6-only, and dual-stack peers are all managed), under the same
	//     predictable remote AS — derived from the tunnel IPv4 address the
	//     same way as this node's own ASN when the peer has one, or from its
	//     tunnel IPv6 address (a different, address-family-appropriate
	//     derivation — see deriveASNFromIPv6) when it doesn't — Description
	//     set to the peer's name, Password "autobgp", BFD on, not shut down.
	//     Appears and disappears within one poll of the peer actually
	//     connecting/disconnecting — see autoBGPPollInterval.
	// AutoBGP only ever touches a Neighbor entry whose Password is exactly
	// "autobgp" (its own marker for "I created this"); anything else in
	// Neighbors — a real external peer, or one added by hand that happens to
	// share an address with a mesh peer — is never added, edited, or removed
	// by it. Turning AutoBGP back off freezes whatever it last left in place;
	// it does not retroactively remove those neighbors or turn BGP back off.
	AutoBGP bool `json:"auto_bgp,omitempty"`
}

// BGPNeighbor is one BGP peer: its address, the AS it belongs to, an optional
// human description, an optional MD5 session password, whether BFD runs on
// this specific session, and whether the session is administratively shut
// down. Ported from parapet's BgpNeighbor.
type BGPNeighbor struct {
	Peer        string `json:"peer"`
	RemoteAS    uint32 `json:"remote_as"`
	Description string `json:"description,omitempty"`
	Password    string `json:"password,omitempty"`
	BFD         bool   `json:"bfd,omitempty"`
	// Shutdown administratively disables this one session (FRR's
	// `neighbor <peer> shutdown`) without removing the neighbor's
	// configuration — the peer stays defined, just held down. Independent of
	// the other neighbors on this router; disabling one doesn't touch the
	// rest.
	Shutdown bool `json:"shutdown,omitempty"`
	// FilterIn/FilterOut are this neighbor's own inbound/outbound route
	// filters — the BGP-itself counterpart to BGPConfig's
	// RedistributeConnectedRoutes/RedistributeStaticRoutes/
	// RedistributeMeshRoutes. Those three control what gets fed *into* BGP
	// from elsewhere on this host; nothing before this field controlled what
	// BGP itself accepts from, or advertises to, a given peer — every prefix
	// a neighbor sent was accepted, and every route this speaker carried was
	// sent to every neighbor, unfiltered, which is `no bgp
	// ebgp-requires-policy`'s whole point (see renderFRR's doc comment on
	// that line). FilterIn/FilterOut are each a CIDR allow-list scoped to
	// this one neighbor: a non-empty list means only those exact prefixes
	// are permitted in that direction on this session — anything else is
	// implicitly denied by the route-map's own trailing deny, the standard
	// standard FRR route-map convention. An empty list (the default, and every
	// neighbor's state before this field existed) means no filtering at all
	// in that direction, so upgrading to a build with this field is a no-op
	// for every session already configured — filtering is opt-in per
	// neighbor, per direction, not a blanket default that could silently cut
	// off a working session.
	//
	// Rendered as a per-neighbor `ip prefix-list`/`ipv6 prefix-list`/
	// `route-map`, the same selective-filter shape as the redistribute
	// fields, just attached to `neighbor <peer> route-map <name> in`/`out`
	// instead of a `redistribute` line (see renderFRR/renderNeighborFilter).
	// Unlike those three fields, this can't share one route-map across every
	// neighbor — each neighbor's allow-list is its own, so each gets its own
	// named route-map. Outbound is the one direction that can collide with
	// ASPrepend, which also wants to own this neighbor's single outbound
	// route-map slot; when both are set for the same neighbor, renderFRR
	// folds the prepend into that neighbor's own filter route-map rather
	// than losing one or the other (see renderNeighborFilter's doc comment).
	// A CIDR here that no longer matches anything a peer actually sends (or
	// that this speaker no longer carries) simply matches nothing — not
	// pruned automatically, same non-pruning reasoning as the redistribute
	// fields above.
	FilterIn  []string `json:"filter_in,omitempty"`
	FilterOut []string `json:"filter_out,omitempty"`
}

// Upgrade configures this node's own upgrades. Upgrades are always from
// source: an operator hands this node a gravinet source archive, it compiles
// it with the local Go toolchain, and swaps the result in behind a
// confirm-or-rollback guard. gravinet ships no prebuilt binary for any
// platform, and a mesh routinely spans Linux, the BSDs, macOS and Windows at
// once, so source is both the only artifact that exists and the only one that
// can be distributed to every node from a single upload.
//
// By default this is strictly local-only: nothing here, in the default
// configuration, gives a peer — Manager or otherwise — a way to trigger,
// drive, or observe an upgrade on this node.
//
// The one exception is entirely opt-in and off by default: setting
// AcceptManagerUpgrades below lets a directly-authenticated Manager peer
// *offer* a source archive this node then independently verifies, builds and
// applies. See that field's own comment for the full trust model, and
// docs/UPGRADES.md.
type Upgrade struct {
	// StateDir is where the guard's state.json lives — the record of an
	// in-flight upgrade that lets a node back out a bad binary on its own
	// after a restart. Empty means "upgrades" next to the config file. It is
	// created 0700.
	//
	// LegacyStoreDir is the former name of this field, from when this
	// directory also held staged binaries. It is still read, and must stay
	// read: a node that set store_dir explicitly and is upgraded *while an
	// upgrade is pending* would otherwise look for state.json in the default
	// location, find nothing, conclude nothing is in flight, and quietly lose
	// its own crash-loop revert — the exact failure the guard exists to
	// prevent, introduced by a rename.
	StateDir       string `json:"state_dir,omitempty"`
	LegacyStoreDir string `json:"store_dir,omitempty"`

	// ConfirmSeconds is how long a freshly-swapped binary has to prove it is
	// healthy — up, on the mesh, with peers again — before this node backs it
	// out on its own (see internal/upgrade's guard). 0 means the default (90s).
	// This is the timer that makes a bad upgrade survivable on a node whose only
	// management path is the very mesh the bad binary is failing to join.
	ConfirmSeconds int `json:"confirm_seconds,omitempty"`

	// AcceptManagerUpgrades opts this node in to remote-initiated upgrades
	// pushed by a Manager peer. Default false, which preserves the strictly
	// local-only behaviour described above: with this off, no peer — Manager
	// or otherwise — can stage or apply anything here, exactly as before.
	//
	// Turning it on lets a Manager *offer* a source archive over the mesh;
	// this node then makes its own decision, and only proceeds if ALL of the
	// following hold, none of which the Manager controls:
	//   - the offer arrived from a Manager this node holds a live, directly
	//     handshake-authenticated session with (not one known only through
	//     gossip/relay — that flag is untrusted; see IsManagerAddr's caveat),
	//   - the pushed archive's content hash matches the digest the Manager
	//     declared alongside it,
	//   - it compiles here, with this node's own toolchain, into a binary that
	//     runs and reports itself,
	//   - that binary passes the same `selftest` config gate a local upgrade
	//     must pass,
	//   - and the same confirm-or-rollback guard arms afterwards, so a bad
	//     push is backed out on this node's own authority within
	//     ConfirmSeconds — the Manager cannot hold this node on a broken binary.
	//
	// Note what a Manager does *not* get, even with this on: it never supplies
	// executable bytes. It supplies source, which this node compiles itself.
	//
	// This is opt-in per node precisely because it converts "a Manager can
	// manage my config" into "a Manager can cause code to be built and run as
	// root here." That is a strictly larger grant and nobody should get it
	// implicitly.
	AcceptManagerUpgrades bool `json:"accept_manager_upgrades,omitempty"`
}

// UpgradeStateDir resolves where the guard keeps its state file, honouring the
// legacy store_dir spelling so a node that set it keeps using the same
// directory across this rename (see Upgrade.LegacyStoreDir).
func (c *Config) UpgradeStateDir() string {
	if c.Upgrade.StateDir != "" {
		return c.Upgrade.StateDir
	}
	if c.Upgrade.LegacyStoreDir != "" {
		return c.Upgrade.LegacyStoreDir
	}
	return filepath.Join(c.dir(), "upgrades")
}

// UpgradeEnabled reports whether this node's upgrade machinery is available
// at all. Always true — there is no key or other configuration required just
// to use the feature. What a node needs in practice is a Go toolchain, which
// is a property of the host rather than of this config, and is reported as a
// preflight failure at upgrade time rather than a config error at load time.
func (c *Config) UpgradeEnabled() bool { return true }

// UpgradeConfirmSeconds is the health-confirmation window, with its default.
func (c *Config) UpgradeConfirmSeconds() int {
	if c.Upgrade.ConfirmSeconds <= 0 {
		return 90
	}
	return c.Upgrade.ConfirmSeconds
}

// Network is a single overlay. Multiple networks coexist on one node, fully
// isolated by their key sets and network id.
type Network struct {
	ID      string `json:"id"`   // 64-bit network id, hex; unique per overlay
	Name    string `json:"name"` // human label
	Enabled bool   `json:"enabled"`

	// Notes is a free-form operator-authored note about this network (e.g. its
	// purpose, who owns it, an environment label). Purely local/informational —
	// never gossiped, never consulted by the engine.
	Notes string `json:"notes,omitempty"`

	// Keys: up to 8 active shared secrets for rotation. Slots are independent
	// per host — only the key material must overlap, matched by keyID.
	Keys [8]KeySlot `json:"keys"`

	// Overlay addressing. The first node defines the subnets; joining nodes
	// receive them and self-assign a random address after DAD.
	Subnet4 string `json:"subnet4"` // e.g. 10.42.0.0/16, empty disables v4 overlay
	Subnet6 string `json:"subnet6"` // e.g. fd00:42::/64, empty disables v6 overlay

	// Optional static overlay addresses for this node (CIDR, e.g. 10.42.0.5/16).
	// If empty, the node self-assigns a random address via DAD (roadmap step 5).
	Address4 string `json:"address4"`
	Address6 string `json:"address6"`

	TUNName string `json:"tun_name"` // interface name; auto if empty
	// MTU is the overlay interface MTU. Defaults to protocol.DefaultTunnelMTU
	// (8915), which is the largest packet that still fits one underlay datagram
	// at the default underlay_mtu_max of 9000 — see that constant for why.
	MTU int `json:"mtu"`

	// Seeds are underlay addresses (host:port) used to bootstrap into the mesh,
	// each with an optional operator-facing note (see Seed). SeedList accepts
	// both this object form and a bare JSON string array on read, so configs
	// written before Notes existed keep loading unchanged; every save writes
	// the object form.
	Seeds SeedList `json:"seeds"`
	// PeerCache is auto-managed: the underlay endpoints of peers seen in the last
	// session, persisted so a restart has many bootstrap candidates (not just the
	// one configured seed). Tried alongside Seeds; first to answer wins.
	PeerCache []string `json:"peer_cache,omitempty"`
	// SeedTCPPort is an optional TCP/TLS port to dial the Seeds on when
	// UDP can't reach them at cold start (before any peer's port is learned via
	// handshake/gossip). Populated from a join token so a node can bootstrap over
	// TCP onto a mesh using a non-default port. 0 means "assume our own port".
	SeedTCPPort int `json:"seed_tcp_port,omitempty"`

	// Mesh selects this network's connectivity topology: "full" (the default,
	// including when left empty) grows every node toward a session with every
	// other node it learns about, same as always. "partial" restricts the mesh
	// to a hub-and-spoke shape built entirely out of each node's own SelfSeed
	// declaration, with no separate roster to maintain: a node that has
	// SelfSeed set is a seed for this purpose, every other node is a peer, and
	// exactly two kinds of link are permitted — seed-to-seed and seed-to-peer.
	// A peer-to-peer link is refused outright, on both the accepting and the
	// completing side of the handshake, regardless of how the two nodes came
	// to dial each other (gossip, a misconfigured Seeds entry, stale
	// PeerCache) — see mesh.Engine.onHSInit/onHSResp. This is enforced by the
	// engine itself, not merely by suppressing gossip-driven auto-dials
	// between peers (learnPeers also stops proposing peer-to-peer dials in
	// this mode, which is what actually keeps a large partial mesh's
	// connection count down, but the handshake-level refusal is what makes it
	// a hard guarantee rather than an optimization).
	//
	// Every node in a partial-mesh network decides this independently from
	// its own config; there is no central list of "which network is partial"
	// distributed over the wire. A node still on "full" (or an old build that
	// predates this field) simply keeps dialing everyone as before, so mixing
	// mesh modes on one network doesn't hard-fail — but it does forfeit the
	// point of turning partial mesh on for anyone who hasn't, so all nodes on
	// a given network should agree.
	//
	// Broadcast/multicast flooding (see mesh.Engine.flood) was built assuming
	// full-mesh reach: it replicates a packet to every *directly* connected
	// peer and relies on every node being directly connected to every other
	// node for that single hop to cover the whole network, since a receiver
	// never re-floods (that's what keeps it loop-free). On a partial mesh a
	// peer is only ever directly connected to seeds, so its broadcast/
	// multicast traffic reaches only those seeds, not the network's other
	// peers — there is no multi-hop flooding to carry it further. Unicast,
	// routed, and relayed traffic are unaffected; this is specific to
	// broadcast/multicast delivery, and worth knowing before relying on a
	// partial-mesh network for a broadcast-dependent protocol.
	Mesh string `json:"mesh,omitempty"`

	StormControl StormControl `json:"storm_control"`
	// Throttle is the pre-v960 per-network bandwidth override. Retained as a
	// field so an existing config still parses; Config.Validate hoists it
	// into the node-global Config.Shaping, keyed by this network's interface,
	// and clears it so it is never written back out.
	//
	// A pointer, because under the old model "no override, use the node
	// default" and "this network is explicitly uncapped" had to be
	// distinguishable — as a value they would both be the zero Throttle and
	// one would silently become the other. There is no default to fall back
	// to now, so the distinction does not survive the hoist and does not need
	// to: an entry is a rate, and no entry is no rate.
	Throttle *Throttle `json:"throttle,omitempty"`
	// QoS is the pre-v954 per-network classifier. Retained as a field so an
	// existing config still parses; Config.Validate hoists it into the
	// node-global Config.QoS with each rule scoped to this network, and clears
	// it so it is never written back out.
	QoS QoS `json:"qos"`
	// Firewall is the pre-v957 per-network rulebase. Retained as a field so an
	// existing config still parses; Config.Validate hoists it into the
	// node-global Config.Firewall with each rule scoped to this network, and
	// clears it so it is never written back out.
	Firewall       Firewall     `json:"firewall"`
	HostsSync      HostsSync    `json:"hosts_sync"`
	HostsAdvertise []HostRecord `json:"hosts_advertise,omitempty"`
	HostsReject    []HostReject `json:"hosts_reject,omitempty"` // peer-advertised host records to refuse (see HostReject)

	// DNSSync controls writing peer-advertised conditional-forwarding domains
	// into the OS's native split-DNS mechanism (systemd-resolved routing
	// domains on Linux, /etc/resolver on macOS, NRPT on Windows). Unlike
	// HostsSync, this never writes a plain name -> address mapping; it only
	// ever tells the OS resolver "queries under this domain go to these
	// servers". The OS's own hosts-before-DNS lookup order means any name
	// HostsSync already resolved never reaches this path, so the two are
	// complementary rather than overlapping.
	DNSSync      DNSSync      `json:"dns_sync"`
	DNSAdvertise []DNSForward `json:"dns_advertise,omitempty"`
	DNSReject    []DNSReject  `json:"dns_reject,omitempty"` // peer-advertised forward domains to refuse (see DNSReject)

	Routes   []Route       `json:"routes"`       // local routes to redistribute
	RouteRej []RejectRoute `json:"route_reject"` // advertised routes to reject (see RejectRoute)
	// RoutePrefer picks which peer's advertisement of a prefix this node
	// follows, when several advertise the same one. See PreferRoute.
	RoutePrefer []PreferRoute `json:"route_prefer,omitempty"`
	// RedistributeBGPRoutes selects exactly which of this node's current
	// BGP-learned routes (FRR's RIB) get gossiped to this network's mesh
	// peers alongside its own Route entries above — the reverse direction
	// from BGPConfig's own RedistributeMeshRoutes (mesh routes into BGP):
	// this is BGP routes into the mesh. Empty means none. A live BGP RIB can
	// hold thousands of entries — the same "which of possibly thousands"
	// problem RedistributeConnectedRoutes/RedistributeStaticRoutes/
	// RedistributeMeshRoutes solve for BGPConfig, same non-auto-pruning
	// behavior (a selected CIDR that's dropped out of the live RIB simply
	// contributes nothing until/unless it reappears, rather than being
	// silently forgotten). Each redistributed route is tagged with
	// RedistributeBGPMetric so peers can rank it against any other path to
	// the same prefix the normal way a route's metric already works (lower
	// wins) — see webadmin's bgpMeshRedistributor, which polls FRR and calls
	// mesh's SetBGPRoutes; gravinet itself never originates or terminates a
	// BGP session, so this only ever has anything to redistribute while
	// FRR/bgpd is actually up and BGP.Enabled is true.
	RedistributeBGPRoutes []string `json:"redistribute_bgp_routes,omitempty"`
	// RedistributeBGPMetric is the metric every route RedistributeBGPRoutes
	// gossips carries. One value for the whole selection, not per-prefix
	// like Route.Metric.
	RedistributeBGPMetric int `json:"redistribute_bgp_metric,omitempty"`
	// NAT is the pre-v953 per-network address translation. Retained as a
	// field so an existing config still parses; Config.Validate hoists it into
	// the node-global Config.NAT with each rule scoped to this network, and
	// clears it so it is never written back out. Nothing reads it after load.
	NAT NAT `json:"nat"`

	// DisabledPeers is a local-only blocklist of peer node IDs this node refuses
	// to connect to. Unlike bans (which flood mesh-wide), disabling a peer only
	// affects this node; other nodes are unaffected. Survives restart.
	DisabledPeers []string `json:"disabled_peers,omitempty"`

	// PeerNotes are operator-authored notes about specific mesh peers, keyed by
	// node id. Local-only informational metadata: never gossiped, and — unlike
	// DisabledPeers — never consulted by the engine for anything but display.
	// The peer itself is never persisted here (it's re-learned from the mesh
	// each session); this just remembers what an operator wrote about a given
	// node id across restarts/reconnects.
	PeerNotes map[string]string `json:"peer_notes,omitempty"`

	AllowRelay bool `json:"allow_relay"` // permit relaying others' traffic through us

	// SelfSeed is an explicit operator declaration: "treat this node as a
	// seed for this network." Advertised to every peer in the handshake
	// (hsPayload.SelfSeed) so they don't have to infer seed status by
	// matching addresses against their own configured Seeds list — an
	// inference that's necessarily approximate (see mesh.ManagedPeer.IsSeed's
	// doc comment for the specific ways it can fall short: two seeds sharing
	// one host:port disambiguated only by transport, or a seed habitually
	// reached over a faster gossiped LAN shortcut instead of its configured
	// public address). Consulted by System > Upgrade's seed-aware push
	// sequencing as an additional, authoritative signal alongside — never in
	// place of — those address-based checks. On a "full" mesh network (see
	// Mesh) this remains purely advisory: it has no effect on connectivity or
	// routing, and a node with it set behaves identically to one without it
	// except in how other nodes sequence upgrades that include it. On a
	// "partial" mesh network it stops being advisory: it's the one signal
	// that decides which nodes form the seed backbone, and every other node
	// treats this node as a peer it may connect to only via a seed.
	SelfSeed bool `json:"self_seed,omitempty"`
}

// UnmarshalJSON backfills DNSSync and AllowRelay to their documented
// on-by-default values (NewNetworkDefaults's DNSSync.Enabled=true and
// AllowRelay=true) when a network's JSON has no "dns_sync"/"allow_relay" key
// at all, instead of leaving them at encoding/json's zero value of false.
//
// This matters because both fields were added after HostsSync: every config
// ever written by gravinet has always had "hosts_sync" (it predates the
// public project), so HostsSync's identically-shaped Enabled bool never hits
// this. Any config saved before conditional DNS forwarding (or before
// AllowRelay) existed has no corresponding key at all, so without this
// backfill it silently loads as fully disabled — indistinguishable from an
// operator's deliberate choice, and, worse, the very next SaveTo (triggered
// by any unrelated edit: adding a seed, a host record, anything) marshals
// that zero value back out as an explicit "enabled": false / "allow_relay":
// false. At that point the key is no longer absent, it's explicit, and the
// feature stays silently off across every future restart until someone
// notices — restarting the daemon re-reads the same file and gets the same
// answer every time.
//
// Only the true "key entirely absent" case is backfilled; a config that
// already has an explicit "dns_sync" object or "allow_relay" key (even one
// that's all zeros / literal false, which is also a valid deliberate
// choice) is left exactly as written — this can only add the default for a
// network that never had an opinion recorded, never override one that did.
func (n *Network) UnmarshalJSON(b []byte) error {
	type alias Network
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*n = Network(a)

	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err == nil {
		if _, present := probe["dns_sync"]; !present {
			n.DNSSync = NewNetworkDefaults().DNSSync
		}
		if _, present := probe["allow_relay"]; !present {
			n.AllowRelay = NewNetworkDefaults().AllowRelay
		}
	}
	return nil
}

// MeshPartial reports whether this network is configured for the restricted
// hub-and-spoke topology (Mesh == "partial", case-insensitively). Any other
// value, including the empty string left by every config written before this
// field existed, means the unrestricted full mesh — the long-standing
// default behavior. Callers should use this rather than comparing n.Mesh
// directly, so a stray case difference (e.g. a hand-edited "Partial") isn't
// silently read back as full mesh.
func (n Network) MeshPartial() bool {
	return strings.EqualFold(n.Mesh, "partial")
}

// KeySlot is one rotation slot. Empty Key means unused.
type KeySlot struct {
	Key     string `json:"key"`   // base64 of 32 random bytes (AES-256)
	Label   string `json:"label"` // optional note, e.g. "2026-Q1"
	Enabled bool   `json:"enabled"`
	Expires string `json:"expires,omitempty"` // RFC3339; "" = never. Past it, the key stops authenticating.
	// Distributed marks a key as pushed out to the mesh (see mesh.FloodKey):
	// ticking it back off retracts the key from every peer that has it, and a
	// label or expiry change while it's set re-propagates the new value to
	// them too. Purely a web-UI/engine concern — this flag itself is never
	// sent over the wire, only what it triggers.
	Distributed bool `json:"distributed,omitempty"`
	// Notes is a free-form operator note about this key slot (e.g. why it was
	// rotated in, who holds a copy). Unlike Label, Notes is never part of the
	// distributed-key flood payload (see mesh.PropagatedKeyInfo) — it stays
	// local to this node's own config even for a Distributed slot.
	Notes string `json:"notes,omitempty"`
}

// Seed is an underlay bootstrap address (host or host:port, optionally
// prefixed with a "tcp://" or "udp://" scheme — see SeedParts) used to dial
// into a mesh, with an optional operator-facing note (e.g. which site or
// host it corresponds to). Address is the only field ever dialed, matched
// for de-duplication, or carried in a join token; Notes is purely
// local/informational and never leaves this node's own config.
type Seed struct {
	Address string `json:"address"`
	Notes   string `json:"notes,omitempty"`
	// Disabled follows the firewall-rule convention: the zero value is
	// enabled, so every config written before this field existed keeps every
	// seed it had. A disabled seed stays in the list with its address and
	// notes intact — it is simply not dialed, not counted as one of this
	// node's configured seeds, and not embedded in a join token. See
	// SeedList.EnabledAddrs for the list every one of those consumers reads.
	Disabled bool `json:"disabled,omitempty"`
	// Node is the node id last confirmed reachable at this address by a
	// completed handshake. It exists so a seed's state can be coupled to
	// that node's own enabled state (see SeedSetEnabled): an operator
	// disabling a seed means "stop talking to the machine behind this", and
	// which machine that is happens to be a runtime fact the config layer
	// has no other way to know.
	//
	// Learned, never authored. Filled in from the engine's own seed
	// attribution while a session is live, and deliberately kept when the
	// seed is disabled — that is precisely when it is needed, and the
	// handshake that would re-establish it cannot happen while the seed is
	// parked. An address that has never completed a handshake has no Node
	// and therefore no coupling: a real gap, but a visible one rather than
	// a silent mis-association.
	//
	// One address can front several nodes behind a NAT. This records the
	// most recent, which is the one an operator reading the row means.
	Node string `json:"node,omitempty"`
}

// SeedList is a network's configured bootstrap seeds. Its custom
// UnmarshalJSON accepts either this object form (the current format, the
// only one ever written back out) or a bare JSON string array — the format
// every config used before Notes existed — so an old config keeps loading
// unchanged; the very next save upgrades it to the object form in place, the
// same "accept both, always write the new shape" approach used for e.g. a
// join token's plain string seeds. MarshalJSON needs no override: a
// []Seed's default encoding is already the object form.
type SeedList []Seed

func (sl *SeedList) UnmarshalJSON(b []byte) error {
	// Try the current object-array form first — this is what every config
	// written by a version with Notes support produces, so it should be the
	// hot path once older configs have been resaved at least once.
	type seedAlias Seed // avoid recursing back into this UnmarshalJSON
	var objs []seedAlias
	if err := json.Unmarshal(b, &objs); err == nil {
		out := make(SeedList, len(objs))
		for i, o := range objs {
			out[i] = Seed(o)
		}
		*sl = out
		return nil
	}
	// Fall back to the legacy bare-string-array form.
	var strs []string
	if err := json.Unmarshal(b, &strs); err != nil {
		return fmt.Errorf("seeds: expected an array of strings or {address,notes} objects: %w", err)
	}
	out := make(SeedList, len(strs))
	for i, s := range strs {
		out[i] = Seed{Address: s}
	}
	*sl = out
	return nil
}

// Addrs returns every configured address, in order, disabled ones included.
// This is the list to read when the question is "what has the operator
// written down" — displaying the table, de-duplicating an add, matching a
// live peer's endpoint back to its seed row.
//
// It is NOT the list to dial. Anything that resolves seeds to actually
// connect, counts this node's seeds, or hands them to another node wants
// EnabledAddrs below; using Addrs there is exactly the bug the Disabled
// field exists to prevent, and it fails silently — a disabled seed dials
// perfectly well, which is the whole problem.
func (sl SeedList) Addrs() []string {
	if len(sl) == 0 {
		return nil
	}
	out := make([]string, len(sl))
	for i, s := range sl {
		out[i] = s.Address
	}
	return out
}

// EnabledAddrs returns the addresses of the seeds that are actually in
// service, in order — Addrs minus anything an operator has disabled. Every
// consumer that dials a seed, resolves one into a NetSpec, or embeds one in
// a join token reads this. Returns nil when nothing is enabled, which is a
// legitimate state (a network can be reached entirely through PeerCache, or
// have its whole seed list parked while a site is down) and not an error at
// this layer.
func (sl SeedList) EnabledAddrs() []string {
	var out []string
	for _, s := range sl {
		if !s.Disabled {
			out = append(out, s.Address)
		}
	}
	return out
}

// DisabledAddrs is EnabledAddrs' complement: the addresses of seeds the
// operator has taken out of service. The daemon needs these named explicitly
// rather than inferred, because "not in the enabled list" cannot distinguish
// a seed that was disabled from one that was deleted, or from a gossip-learned
// address that was never a configured seed at all — and those three want
// different handling. See mesh.NetSpec.RetiredSeeds.
func (sl SeedList) DisabledAddrs() []string {
	var out []string
	for _, s := range sl {
		if s.Disabled {
			out = append(out, s.Address)
		}
	}
	return out
}

// NodeFor returns the node id attributed to a seed address, or "" when that
// address has never completed a handshake and so has no known owner. Callers
// must treat "" as "unknown", never as "no peer to couple to" — the
// difference matters, because silently doing nothing is what makes an
// operator think the toggle is broken.
func (sl SeedList) NodeFor(addr string) string {
	addr = strings.TrimSpace(addr)
	for _, s := range sl {
		if s.Address == addr {
			return s.Node
		}
	}
	return ""
}

// AddrsForNode is NodeFor's reverse: every configured seed address attributed
// to a node, in order. Used to re-enable a node's seeds when the node itself
// is re-enabled.
func (sl SeedList) AddrsForNode(nodeID string) []string {
	if nodeID == "" {
		return nil
	}
	var out []string
	for _, s := range sl {
		if s.Node == nodeID {
			out = append(out, s.Address)
		}
	}
	return out
}

// Expired reports whether the slot has an expiry that has passed. An unparseable
// expiry is treated as never (Validate rejects bad values on save).
func (k KeySlot) Expired(now time.Time) bool {
	if k.Expires == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, k.Expires)
	if err != nil {
		return false
	}
	return now.After(t)
}

// StormControl bounds broadcast/multicast and gossip rates with token buckets.
type StormControl struct {
	BroadcastPPS int `json:"broadcast_pps"` // sustained packets/sec, 0 disables limit
	MulticastPPS int `json:"multicast_pps"`
	Burst        int `json:"burst"` // bucket depth
}

// Throttle caps bandwidth. Up is the egress (shaped) rate; Down is the
// ingress (policed) rate. Set one for a single direction, both for "both",
// neither for unlimited. All values are bytes per second; 0 = unlimited.
// Throttle is a bandwidth limit: an egress rate, an ingress rate, and the
// bucket/queue sizing around them.
//
// It is the rate half of IfaceShaping, and is also still the shape of the
// legacy Config.Throttle / Network.Throttle fields that v960 hoists.
type Throttle struct {
	Enabled         bool `json:"enabled"` // off by default
	UpBytesPerSec   int  `json:"up_bytes_per_sec"`
	DownBytesPerSec int  `json:"down_bytes_per_sec"`
	BurstBytes      int  `json:"burst_bytes"` // token-bucket depth; 0 = default
	QueueBytes      int  `json:"queue_bytes"` // egress queue capacity; 0 = default
}

// IfaceShaping is one bandwidth limit, bound to the interface it shapes.
//
// The shaper has always been per interface: one bounded queue and one drainer
// on one tunnel device, with no point at which two devices meet. Until v960
// the configuration said otherwise — a node default plus per-network
// overrides, resolved to an interface only at the moment a spec was built —
// and the model had to keep explaining that a default was "that much to each
// network, never a total shared between them". Keying the rate to the
// interface makes that the shape of the data rather than a caveat about it:
// there is no total to mistake it for, because there is nothing above an
// interface to state one on.
//
// Iface is a kernel interface name, matched literally. For a mesh network
// that is its TUN device — Network.TUNName, or the mesh<N> the node assigns
// when that is empty; see Config.IfaceForNetwork.
//
// An entry may name an interface that does not exist yet, which is the point
// of being able to write one: a rate can be set before the network that
// carries it, and survives that network being rebuilt. Nothing enforces it
// until an interface by that name is up and gravinet is the thing moving
// packets on it — see Config.ShapingUnenforced for the interfaces where that
// second half does not hold, which the admin UI and CLI both report rather
// than leaving to be discovered.
type IfaceShaping struct {
	Iface string `json:"iface"`
	// Throttle is embedded, so an entry encodes flat — {"iface":"mesh0",
	// "enabled":true,"up_bytes_per_sec":…} — and the rate fields keep the
	// names they had under the old model.
	Throttle
}

// FirewallRule is one entry in a network's ordered rulebase. Default policy is
// allow, so an empty rulebase permits all traffic; add rules to restrict.
// Empty Src/Dst (or "any") match any address; zero ports match any port.
//
// SrcNegate/DstNegate/ServicesNegate flip what their dimension's match
// means — "anything except this" instead of "this" — applied uniformly
// whether the field is a literal, an object reference, or (for services) a
// named service; see mesh.FirewallRule's doc comment for the full
// semantics, including the deliberate non-special-casing of negating an
// empty/"any" field.
type FirewallRule struct {
	// ID is stable for the life of the rule and unique across this node's
	// rulebase. Assigned here from Firewall.NextID rather than minted by the
	// engine, because config is the durable record and the engine is a live
	// working copy rebuilt on every reload: an engine-minted id could not
	// survive a restart, and the admin UI keys selections, counter resets and
	// reordering off it. The engine adopts this id rather than allocating its
	// own.
	ID             uint64   `json:"id,omitempty"`
	Disabled       bool     `json:"disabled,omitempty"`  // true = rule is skipped; active by default
	Action         string   `json:"action"`              // allow|deny
	Direction      string   `json:"direction,omitempty"` // in|out|both
	Proto          string   `json:"proto,omitempty"`     // tcp|udp|icmp|any
	Src            string   `json:"src,omitempty"`       // CIDR, host, "any", or object name
	Dst            string   `json:"dst,omitempty"`
	SrcNegate      bool     `json:"src_negate,omitempty"` // match anything EXCEPT Src
	DstNegate      bool     `json:"dst_negate,omitempty"` // match anything EXCEPT Dst
	SrcPortMin     int      `json:"sport_min,omitempty"`
	SrcPortMax     int      `json:"sport_max,omitempty"`
	DstPortMin     int      `json:"dport_min,omitempty"`
	DstPortMax     int      `json:"dport_max,omitempty"`
	Services       []string `json:"services,omitempty"`        // named service-catalog entries
	ServicesNegate bool     `json:"services_negate,omitempty"` // match any service EXCEPT Proto/ports+Services
	Log            bool     `json:"log,omitempty"`             // log a line whenever this rule matches
	Notes          string   `json:"notes,omitempty"`           // free-form operator note, e.g. why the rule exists

	// Scope names the mesh network this rule is enforced on, or is empty to
	// enforce on every network this node runs.
	//
	// Empty means every network, as with QoSRule.Scope and for the same
	// reason: the firewall has no kernel path, so a rule that named no network
	// would do nothing at all. It also means a rule written before any network
	// exists starts enforcing the moment one does.
	Scope string `json:"scope,omitempty"`
}

// Firewall is a network's packet filter. It is off by default; when enabled with
// an empty rulebase the default policy is allow (stateful), so add rules to
// restrict. When disabled, no filtering happens at all.
//
// Rules reference reusable address-object and service catalogs by name (see
// FirewallRule.Src/Dst and FirewallRule.Services) — those catalogs are node-
// global (Config.FirewallObjects/FirewallServices, shared by every network on
// this node), not part of this per-network struct.
// Firewall is this node's rulebase. Node-global from v957, not per mesh
// network: a firewall rule is a statement about packets, and a node should not
// need an overlay before it can write one down.
//
// Enforcement is unchanged and still per-network — internal/mesh evaluates on
// each tunnel's in/out path, which is the only place a firewall rule is
// enforced at all — so a rule reaches an overlay via FirewallRule.Scope.
//
// Order matters: first match wins. One ordered list per node now, and each
// network evaluates the subset in scope for it, in this list's order.
type Firewall struct {
	Enabled bool           `json:"enabled"`
	Rules   []FirewallRule `json:"rules"`

	// NextID is the counter for FirewallRule.ID. Kept here rather than derived
	// from max(ID)+1 so that deleting the highest-numbered rule cannot hand
	// its id to the next one created — an id that comes back means stale hit
	// counters and stale UI selections bind to the wrong rule.
	NextID uint64 `json:"next_id,omitempty"`
}

// FirewallObject is a named, reusable address object referenced by rules. kind
// is host|subnet|range|fqdn|group; a group bundles other objects by name.
type FirewallObject struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	Addresses []string `json:"addresses,omitempty"` // literals/CIDRs/ranges/fqdns (non-group)
	Members   []string `json:"members,omitempty"`   // member object names (group)
	Notes     string   `json:"notes,omitempty"`
}

// FirewallServicePort is one protocol/port leg of a named service.
type FirewallServicePort struct {
	Proto   string `json:"proto"`
	PortMin int    `json:"port_min,omitempty"`
	PortMax int    `json:"port_max,omitempty"`
}

// FirewallService is a named, reusable protocol/port bundle (e.g. DNS = udp/53 +
// tcp/53) referenced by rules via FirewallRule.Services.
type FirewallService struct {
	Name  string                `json:"name"`
	Ports []FirewallServicePort `json:"ports"`
	Notes string                `json:"notes,omitempty"`
}

// FirewallExempt is one always-allowed traffic class. It matches a packet when
// the protocol matches and the port (matched against either the source or the
// destination port) matches. A zero Port matches any port, which is what
// port-less protocols like OSPF want. If Mgmt is set, the port is this node's
// live web-admin port instead of Port — so "remote management" follows the
// configured admin port automatically.
type FirewallExempt struct {
	Name  string `json:"name"`            // human label, e.g. "BGP"
	Proto string `json:"proto,omitempty"` // tcp|udp|icmp|ospf|<number>|any (empty = any)
	Port  int    `json:"port,omitempty"`  // matches src OR dst; 0 = any/port-less
	Mgmt  bool   `json:"mgmt,omitempty"`  // match this node's web-admin port (overrides Port)
	// Disabled follows the firewall-rule convention: the zero value is enabled,
	// so entries written before this field existed — and the built-in defaults —
	// stay in force. A disabled entry is kept in the allowlist but not applied,
	// so its traffic class is once again subject to the rulebase.
	Disabled bool `json:"disabled,omitempty"`
}

// DefaultFirewallExempts is the built-in allowlist used when a network's Exempt
// list is unset: remote web-admin management, plus the BGP/OSPF/RIP routing
// protocols. It is the starting point an operator can edit or clear.
func DefaultFirewallExempts() []FirewallExempt {
	return []FirewallExempt{
		{Name: "remote management", Proto: "tcp", Mgmt: true},
		{Name: "BGP", Proto: "tcp", Port: 179},
		{Name: "OSPF", Proto: "ospf"},
		{Name: "RIP", Proto: "udp", Port: 520},
		{Name: "RIPng", Proto: "udp", Port: 521},
	}
}

// EffectiveFirewallExempt returns the node-global always-allowed list,
// substituting the built-in defaults when the operator hasn't set one. The list
// is global (not per-network): the same exemptions apply to every network's
// firewall. Use this anywhere the *active* allowlist matters (the engine, status
// views, the CLI, the web admin).
func (c *Config) EffectiveFirewallExempt() []FirewallExempt {
	if c.FirewallExempts == nil {
		return DefaultFirewallExempts()
	}
	return c.FirewallExempts
}

// ReadmePath resolves where the README lives on disk for the web admin to read.
// An explicit readme_path wins; otherwise it searches the locations the
// installer uses — next to the binary's install prefix (…/share/doc/gravinet),
// beside the binary (Windows), next to the config, then the current directory
// (dev tree) — and returns the first that exists, or "" if none do. exeDir is
// the directory of the running binary (from os.Executable); pass "" if unknown.
func (c *Config) ReadmePath(configPath, exeDir string) string {
	return resolveDocPath("README.md", c.ReadmeFile, configPath, exeDir)
}

// LicensePath resolves where the LICENSE lives on disk, the same way as
// ReadmePath. An explicit license_path overrides the search.
func (c *Config) LicensePath(configPath, exeDir string) string {
	return resolveDocPath("LICENSE", c.LicenseFile, configPath, exeDir)
}

// GettingStartedPath resolves where getting-started.md lives on disk, the
// same way as ReadmePath/LicensePath. An explicit getting_started_path
// overrides the search. (A separate getting-started.html existed briefly —
// see GettingStartedFile's doc comment for why there's only one file now.)
func (c *Config) GettingStartedPath(configPath, exeDir string) string {
	return resolveDocPath("getting-started.md", c.GettingStartedFile, configPath, exeDir)
}

// APIDocPath resolves where API.md lives on disk, the same way as
// ReadmePath/LicensePath/GettingStartedPath. An explicit api_doc_path
// overrides the search. The Info -> API page reads this file fresh on every
// request rather than embedding a copy in the UI, so it can never drift out
// of date with the actual endpoint set the running binary exposes.
func (c *Config) APIDocPath(configPath, exeDir string) string {
	return resolveDocPath("API.md", c.APIDocFile, configPath, exeDir)
}

// resolveDocPath finds an installed doc file (README/LICENSE) on disk, trying the
// install-standard locations in priority order and returning the first that
// exists ("" if none). An explicit override always wins.
func resolveDocPath(filename, override, configPath, exeDir string) string {
	if override != "" {
		return override
	}
	var cands []string
	if exeDir != "" {
		// Unix install prefix: /usr/local/bin/gravinet -> /usr/local/share/doc/...
		cands = append(cands, filepath.Join(exeDir, "..", "share", "doc", "gravinet", filename))
		// Beside the binary: the Windows installer drops the file next to the .exe
		// (e.g. %ProgramFiles%\gravinet\<file>), same dir as wintun.dll.
		cands = append(cands, filepath.Join(exeDir, filename))
	}
	if configPath != "" {
		cands = append(cands, filepath.Join(filepath.Dir(configPath), filename))
	}
	cands = append(cands, filename)
	for _, p := range cands {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// UnderlayMTUValue is the resolved underlay datagram cap in bytes. Default 1280;
// clamped to [590, 9216] so a fragment always carries useful payload and the cap
// never exceeds the jumbo tunnel ceiling.
// UDPPortList and TCPPortList are the ports this node listens on, in bind
// order. They are the only supported way to read the listen configuration —
// the legacy fields are folded in by Load and cleared, so reading them
// directly would see zero on any config this build has saved.
//
// An empty list means that protocol is off. Both empty is refused by Validate:
// a node with no transport can never be reached.
func (c *Config) UDPPortList() []int { return dedupePorts(c.UDPPorts) }
func (c *Config) TCPPortList() []int { return dedupePorts(c.TCPPorts) }

// UDPEnabled/TCPEnabled report whether each transport has any port at all.
func (c *Config) UDPEnabled() bool { return len(c.UDPPortList()) > 0 }
func (c *Config) TCPEnabled() bool { return len(c.TCPPortList()) > 0 }

// AdvertisedUDPPort and AdvertisedTCPPort are the ports this node tells peers
// about as its canonical ones — the first in each list. This is presentation,
// not precedence: every bound port answers identically, and a peer may reach
// this node on any of them. It exists because a peer's candidate list has to
// start somewhere, and "the one the operator wrote first" is the least
// surprising choice. 0 means the protocol is off.
func (c *Config) AdvertisedUDPPort() int { return firstPort(c.UDPPortList()) }
func (c *Config) AdvertisedTCPPort() int { return firstPort(c.TCPPortList()) }

func firstPort(ps []int) int {
	if len(ps) == 0 {
		return 0
	}
	return ps[0]
}

// dedupePorts drops duplicates and out-of-range entries while preserving
// order. Binding the same port twice fails the second time and looks like a
// configuration error in the log; a list is meant to be safe to widen.
func dedupePorts(in []int) []int {
	// Non-nil even when empty: a nil slice marshals as null, which decodes
	// back into a nil pointer and reads as absent rather than as "off".
	out := []int{}
	seen := map[int]bool{}
	for _, p := range in {
		if p < 1 || p > 65535 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// migratePortConfig folds the pre-v789 primary/fallback fields into the flat
// lists and clears them, so a config round-tripped through this build carries
// only the flat form.
//
// Called from Load, before anything reads a port. The old shape carried the
// same information in a way that implied a hierarchy — PrimaryPort *then*
// ExtraListenPorts, TCPPort *then* ExtraTCPListenPorts — so the fold
// is order-preserving: what was primary leads the list and becomes what this
// node advertises, which keeps an upgraded node advertising exactly what it
// advertised before.
//
// A config that already has flat lists is left alone; the legacy fields are
// still cleared, so a hand-edited file carrying both doesn't silently keep a
// stale value that nothing reads.
// portsOnDisk is what the config file actually said about ports, flat keys and
// legacy keys alike, with pointers so absent and empty are distinguishable.
//
// That distinction is the whole difficulty. Default() seeds UDPPorts/TCPPorts
// before the file is unmarshalled, so "c.UDPPorts is non-empty" cannot mean
// "the file specified it" — the first version of this migration tested exactly
// that and silently never fired, leaving every upgraded node on the defaults
// instead of the ports it had been running. Only presence in the file can
// answer it, and only a separate decode can see presence.
type portsOnDisk struct {
	UDPPorts *[]int `json:"udp_ports"`
	TCPPorts *[]int `json:"tcp_ports"`

	PrimaryPort         *int  `json:"primary_port"`
	ExtraListenPorts    []int `json:"extra_listen_ports"`
	TCPPort             *int  `json:"tcp_fallback_port"`
	DisableTCP          *bool `json:"disable_tcp_fallback"`
	ExtraTCPListenPorts []int `json:"extra_tcp_listen_ports"`
}

// migratePortConfig resolves the port lists from what the file said. Called
// from Load before anything reads a port.
//
// Precedence: the flat keys if present, then the pre-v789 primary/fallback
// keys, then whatever Default() seeded. The fold is order-preserving — what
// was primary leads the list and so stays the port this node advertises, which
// is what keeps an upgraded node reachable at the address its peers already
// know.
func (c *Config) migratePortConfig(d portsOnDisk) {
	switch {
	case d.UDPPorts != nil:
		c.UDPPorts = *d.UDPPorts
	case d.PrimaryPort != nil:
		// primary_port 0 meant "UDP off" and must stay off — an empty list,
		// not a defaulted one.
		if *d.PrimaryPort != 0 {
			c.UDPPorts = append([]int{*d.PrimaryPort}, d.ExtraListenPorts...)
		} else {
			c.UDPPorts = nil
		}
	}

	switch {
	case d.TCPPorts != nil:
		c.TCPPorts = *d.TCPPorts
	case d.PrimaryPort != nil || d.TCPPort != nil || d.DisableTCP != nil:
		// This config predates the flat keys. disable_tcp_fallback was the off
		// switch and is a separate field from the port, so an off config
		// usually still carries a port value — reading the port and ignoring
		// the bool would turn TCP back on. tcp_fallback_port 0 or absent meant
		// "the default", not "off", which is most nodes.
		if d.DisableTCP != nil && *d.DisableTCP {
			c.TCPPorts = nil
		} else {
			p := DefaultTCPPort
			if d.TCPPort != nil && *d.TCPPort != 0 {
				p = *d.TCPPort
			}
			c.TCPPorts = append([]int{p}, d.ExtraTCPListenPorts...)
		}
	}

	c.UDPPorts = dedupePorts(c.UDPPorts)
	c.TCPPorts = dedupePorts(c.TCPPorts)
}

// SocketBufferMinBytes/SocketBufferMaxBytes/SocketBufferDefaultBytes bound and
// default the per-socket buffer. Exported so the web admin's Performance card
// and its handler validate against the same numbers this resolves with.
const (
	SocketBufferDefaultBytes = 16 << 20
	SocketBufferMinBytes     = 256 << 10
	SocketBufferMaxBytes     = 256 << 20

	// SocketBufferMBThreshold is the value at or below which socket_buffer is
	// read as megabytes rather than bytes. Both units are accepted because the
	// two ranges cannot overlap in practice: the smallest meaningful byte
	// value is SocketBufferMinBytes (262144), and the largest meaningful
	// megabyte value is 256. Anything at or under 1024 is therefore
	// unambiguously megabytes — "socket_buffer": 32 means 32 MiB, and so does
	// typing 32 into the Settings card, so the file and the UI agree instead
	// of one wanting 33554432 and the other 32.
	SocketBufferMBThreshold = 1024
)

// SocketBufferValue is the resolved per-socket buffer target in bytes.
// Accepts megabytes or bytes (see SocketBufferMBThreshold); 0 selects
// SocketBufferDefaultBytes; the result is clamped to
// [SocketBufferMinBytes, SocketBufferMaxBytes] so a typo can neither recreate
// the overflow this exists to avoid nor ask the kernel for something absurd.
// Note this is a limit, not a reservation: the kernel allocates against it on
// demand.
func (c *Config) SocketBufferValue() int {
	n := c.SocketBuffer
	if n == 0 {
		return SocketBufferDefaultBytes
	}
	if n > 0 && n <= SocketBufferMBThreshold {
		n <<= 20 // megabytes
	}
	if n < SocketBufferMinBytes {
		return SocketBufferMinBytes
	}
	if n > SocketBufferMaxBytes {
		return SocketBufferMaxBytes
	}
	return n
}

// SocketBufferMB is the resolved buffer in whole megabytes, for display in the
// Settings card (which is denominated in MB).
func (c *Config) SocketBufferMB() int { return c.SocketBufferValue() >> 20 }

// DefaultWorkerThreads is the worker-pool / receive-socket count used when
// worker_threads is unset. Fixed at 4 rather than NumCPU()-1: past that, each
// additional worker mostly adds flow-hash buckets and contention rather than
// throughput, and the outbound pool now pins a flow to a worker (see
// mesh.tunLoopPooled) so what matters is having enough buckets to spread
// concurrent flows, not one per core.
const DefaultWorkerThreads = 4

// DefaultTunQueues is the IFF_MULTI_QUEUE read-queue count used when
// tun_queues is unset. Set tun_queues=1 to force the old single-queue path.
const DefaultTunQueues = 4

// WorkerThreadsValue resolves the worker count: the configured value, or
// DefaultWorkerThreads capped at NumCPU, floored at 1.
func (c *Config) WorkerThreadsValue() int {
	n := c.WorkerThreads
	if n <= 0 {
		n = DefaultWorkerThreads
		if cpus := runtime.NumCPU(); n > cpus {
			n = cpus
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// TunQueuesValue resolves the overlay interface's read-queue count. A no-op
// on platforms without multi-queue TUN, which silently get one queue.
func (c *Config) TunQueuesValue() int {
	n := c.TunQueues
	if n <= 0 {
		n = DefaultTunQueues
		if cpus := runtime.NumCPU(); n > cpus {
			n = cpus
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

// UDPGSOEnabled reports whether UDP segmentation offload (UDP_SEGMENT send /
// UDP_GRO receive) should be requested. Nil means enabled.
func (c *Config) UDPGSOEnabled() bool { return c.EnableUDPGSO == nil || *c.EnableUDPGSO }

func (c *Config) UnderlayMTUValue() int {
	m := c.UnderlayMTU
	if m == 0 {
		return 1280
	}
	if m < 590 {
		return 590
	}
	if m > 9216 {
		return 9216
	}
	return m
}

// UnderlayMTUMaxValue is the resolved ceiling for path-MTU discovery. Default
// 9000; clamped to [floor, 9216] so it never sits below the floor or above the
// datagram ceiling. When discovery is disabled it collapses to the floor.
func (c *Config) UnderlayMTUMaxValue() int {
	floor := c.UnderlayMTUValue()
	if !c.PMTUDiscoveryEnabled() {
		return floor
	}
	m := c.UnderlayMTUMax
	if m == 0 {
		m = 9000
	}
	if m > 9216 {
		m = 9216
	}
	if m < floor {
		m = floor
	}
	return m
}

// PMTUDiscoveryEnabled reports whether probe-based path-MTU discovery runs.
// Defaults to true when unset.
func (c *Config) PMTUDiscoveryEnabled() bool {
	return c.PMTUDiscovery == nil || *c.PMTUDiscovery
}

// RestartOnUnderlayChangeEnabled reports whether the daemon restarts itself on a
// detected underlay (Wi-Fi/cellular) roam. Defaults to true when unset.
func (c *Config) RestartOnUnderlayChangeEnabled() bool {
	return c.RestartOnUnderlayChange == nil || *c.RestartOnUnderlayChange
}

// DefaultLogMaxBytes is the log-file cap used when nothing is configured: a
// 200 MiB rolling window. Exported so the web admin can show the effective
// default in the Log Size box before anything is set.
const DefaultLogMaxBytes int64 = 200 << 20

// minLogMaxBytes floors the configured cap so a tiny misconfiguration ("1K")
// can't make the file thrash on every line.
const minLogMaxBytes int64 = 64 << 10

// ParseSize parses a human byte size with an optional unit suffix into bytes.
// Accepts a bare integer ("1048576"), or a number followed by one of B, K/KB,
// M/MB, G/GB, T/TB (case-insensitive, binary multiples of 1024). A trailing
// "iB" ("MiB") is accepted as a synonym. Whitespace and a single trailing "b"
// after the unit letter are tolerated, so "200M", "200 MB", and "200MiB" all
// mean the same thing. Returns an error on anything it can't make sense of,
// including zero or negative sizes, so callers can reject bad input rather than
// silently falling back to a default.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}
	// Split the trailing unit letters from the leading number.
	i := 0
	for i < len(t) && (t[i] == '.' || t[i] == '-' || t[i] == '+' || (t[i] >= '0' && t[i] <= '9')) {
		i++
	}
	numPart := strings.TrimSpace(t[:i])
	unit := strings.TrimSpace(strings.ToLower(t[i:]))
	if numPart == "" {
		return 0, fmt.Errorf("size %q has no number", s)
	}
	// Normalize unit: strip a trailing "b"/"ib" so "kb", "kib", and "k" all
	// collapse to "k".
	unit = strings.TrimSuffix(unit, "b")
	unit = strings.TrimSuffix(unit, "i")
	var mult int64 = 1
	switch unit {
	case "":
		mult = 1
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	case "t":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size unit %q in %q", unit, s)
	}
	// Allow a fractional number ("1.5M") by parsing as float when a dot is
	// present, integer otherwise, then multiplying.
	var bytes int64
	if strings.Contains(numPart, ".") {
		f, err := strconv.ParseFloat(numPart, 64)
		if err != nil {
			return 0, fmt.Errorf("bad size %q: %v", s, err)
		}
		bytes = int64(f * float64(mult))
	} else {
		n, err := strconv.ParseInt(numPart, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("bad size %q: %v", s, err)
		}
		bytes = n * mult
	}
	if bytes <= 0 {
		return 0, fmt.Errorf("size %q must be positive", s)
	}
	return bytes, nil
}

// FormatSize renders a byte count as a compact human size using the largest
// unit that divides it evenly (so 200<<20 -> "200M", not "204800K"), falling
// back to the next unit down when it doesn't divide cleanly. Used to show the
// effective cap in the web admin.
func FormatSize(b int64) string {
	if b <= 0 {
		return "0"
	}
	type u struct {
		suf string
		val int64
	}
	for _, x := range []u{{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10}} {
		if b%x.val == 0 {
			return strconv.FormatInt(b/x.val, 10) + x.suf
		}
	}
	return strconv.FormatInt(b, 10)
}

// LogMaxBytes is the resolved log-file cap in bytes. Precedence: an explicit
// LogMaxSize ("200M", "1G", …) wins; otherwise the legacy LogMaxMB; otherwise
// the 200 MiB default. Floored at 64 KiB so a tiny value can't thrash. A
// LogMaxSize that fails to parse is ignored here (Validate rejects it up front,
// so a saved config never reaches this with a bad value).
func (c *Config) LogMaxBytes() int64 {
	if strings.TrimSpace(c.LogMaxSize) != "" {
		if b, err := ParseSize(c.LogMaxSize); err == nil {
			if b < minLogMaxBytes {
				b = minLogMaxBytes
			}
			return b
		}
	}
	if c.LogMaxMB > 0 {
		b := int64(c.LogMaxMB) << 20
		if b < minLogMaxBytes {
			b = minLogMaxBytes
		}
		return b
	}
	return DefaultLogMaxBytes
}

// LogMaxSizeString reports the effective cap as a human string for display,
// resolving the same precedence LogMaxBytes uses.
func (c *Config) LogMaxSizeString() string { return FormatSize(c.LogMaxBytes()) }

// LogFIFO reports whether the log file should run in single-file FIFO mode
// (oldest lines dropped from the front when full) rather than the legacy
// numbered-backup rotation. FIFO is the mode whenever a LogMaxSize is set —
// which the web admin always does — so numbered rotation only survives for a
// config that predates LogMaxSize and set LogMaxMB/LogKeep directly.
func (c *Config) LogFIFO() bool { return strings.TrimSpace(c.LogMaxSize) != "" }

// LogBackups is the resolved number of rotated files to keep (default 5). A
// negative LogKeep means keep none.
func (c *Config) LogBackups() int {
	if c.LogKeep == 0 {
		return 5
	}
	if c.LogKeep < 0 {
		return 0
	}
	return c.LogKeep
}

// LogFilePath resolves the effective log-file path given where the config lives.
// Returns "" when file logging is disabled ("-" or "none"); otherwise the
// configured path, or a default of "gravinet.log" next to the config file.
func (c *Config) LogFilePath(configPath string) string {
	switch c.LogFile {
	case "-", "none", "off":
		return ""
	case "":
		dir := filepath.Dir(configPath)
		if dir == "" {
			dir = "."
		}
		return filepath.Join(dir, "gravinet.log")
	default:
		return c.LogFile
	}
}

// WebAdminListenSet returns the admin bind addresses as "host:port", in the
// order they should be bound, with the primary first.
//
// meshAddrs are this node's current overlay addresses, which the caller
// supplies because config can't know them — they're assigned at runtime. They
// are used only to expand the default; once ListenAddrs is set, it is the
// whole answer and nothing is added behind the operator's back.
//
// The primary is chosen rather than taken positionally: loopback first if it
// was picked, because that's the one address that cannot stop working
// underneath you, and Start binds the primary directly while the rest are
// best-effort. If loopback wasn't picked, the first entry leads and the
// operator has accepted that trade by deselecting it.
func (c *Config) WebAdminListenSet(meshAddrs []netip.Addr) []string {
	port := c.WebAdminPort()
	if port == 0 {
		return nil
	}
	var ips []netip.Addr
	if len(c.ListenAddrsRaw()) > 0 {
		for _, raw := range c.ListenAddrsRaw() {
			if ip, err := netip.ParseAddr(strings.TrimSpace(raw)); err == nil {
				ips = append(ips, ip)
			}
		}
	} else {
		// Default: whatever Listen already names, plus the overlay addresses
		// the cluster-management binder would have added anyway.
		if h, _, err := net.SplitHostPort(c.WebAdmin.Listen); err == nil {
			if ip, err := netip.ParseAddr(h); err == nil {
				ips = append(ips, ip)
			}
		}
		ips = append(ips, meshAddrs...)
	}
	// Loopback leads if present.
	sort.SliceStable(ips, func(i, j int) bool {
		return ips[i].IsLoopback() && !ips[j].IsLoopback()
	})
	seen := map[string]bool{}
	var out []string
	for _, ip := range ips {
		if !ip.IsValid() {
			continue
		}
		a := net.JoinHostPort(ip.String(), strconv.Itoa(port))
		if seen[a] {
			continue
		}
		seen[a] = true
		out = append(out, a)
	}
	return out
}

// ListenAddrsRaw is the configured pick list, trimmed of blanks.
func (c *Config) ListenAddrsRaw() []string {
	var out []string
	for _, s := range c.WebAdmin.ListenAddrs {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// WebAdminPort returns the configured web-admin TCP port, or 0 if web admin is
// disabled or the listen address can't be parsed.
func (c *Config) WebAdminPort() int {
	if c.WebAdmin.Listen == "" {
		return 0
	}
	_, ps, err := net.SplitHostPort(c.WebAdmin.Listen)
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(ps)
	if err != nil || p < 1 || p > 65535 {
		return 0
	}
	return p
}

// up-throttle is set (a rate cap is what creates the contention to prioritise).
// QoS is this node's traffic classifier. Node-global from v954, not per mesh
// network: a classification rule is a statement about packets, and there is no
// reason a node should need an overlay before it can be told that SSH outranks
// backups. Enforcement stays where it always was — the classifier runs on each
// network's tunnel egress, feeding that network's shaper — so a rule reaches an
// overlay via QoSRule.Scope. See its doc comment.
type QoS struct {
	Enabled      bool      `json:"enabled"`
	Classes      int       `json:"classes"`       // number of priority levels (default 3)
	DefaultClass int       `json:"default_class"` // class for unmatched traffic
	Rules        []QoSRule `json:"rules"`

	// ClassDSCP is an optional per-class outbound DSCP mark, indexed by
	// class (0 = highest). Every enabled QoS class marks its matching
	// traffic with a DSCP codepoint by default (see mesh.DefaultClassDSCP);
	// an entry here overrides that default for the corresponding class. A
	// missing entry, or one holding -1, keeps the default for that class.
	ClassDSCP []int `json:"class_dscp,omitempty"`
}

// defaultQoSUpBytesPerSec is the placeholder egress cap seeded when QoS is
// enabled without an up-throttle already configured. QoS only reorders traffic
// behind a rate cap, so enabling QoS turns the cap on; 1 Gbit/s is high enough
// not to throttle most links (so it's a safe default) but operators should
// lower it to ~90-95% of the node's real uplink for prioritisation to engage.
const defaultQoSUpBytesPerSec = 125_000_000 // 1 Gbit/s

// QoSRule assigns matching traffic to a priority class (0 = highest). A zero
// Protocol/port means "any"; DSCP nil means "any".
//
// Services names entries from the node-global named service catalog
// (Config.FirewallServices — the same catalog firewall rules resolve their
// own Services field against; see FirewallRule.Services), unioned with the
// literal Protocol/PortMin/PortMax leg exactly the way FirewallRule unions
// its inline proto/port with its named services: a rule can carry a literal
// leg, any number of named services, or both, and traffic matching any of
// them lands in Class. A rule with none of Protocol/PortMin/PortMax/Services
// set matches everything (a catch-all), same as before Services existed.
//
// Disabled follows the firewall-rule convention: the zero value is enabled, so
// rules loaded from configs written before this field existed keep classifying.
// A disabled rule is retained in config (so it can be re-enabled with its match
// intact) but is skipped by the classifier.
type QoSRule struct {
	Protocol string   `json:"protocol"` // "tcp","udp","icmp","any"/"" — combined with Services
	PortMin  int      `json:"port_min"`
	PortMax  int      `json:"port_max"`
	Services []string `json:"services,omitempty"` // named service-catalog entries (Config.FirewallServices), unioned with Protocol/PortMin/PortMax
	DSCP     *int     `json:"dscp,omitempty"`     // nil = any
	Class    int      `json:"class"`
	Disabled bool     `json:"disabled,omitempty"` // true = rule is skipped; active by default

	// Scope names the mesh network this rule classifies traffic on, or is
	// empty to classify on every network this node runs.
	//
	// Empty is the default and the useful one: unlike NAT, QoS has no kernel
	// path, so a rule that named no network would otherwise do nothing at all
	// — "any overlay" is the only reading that leaves a scopeless rule
	// meaningful. It also means a rule written before any network exists
	// starts working the moment one does, which is the point of the move.
	//
	// A named scope is what every pre-v954 rule migrates to, since each was
	// already filed under exactly one network, and is how a node keeps
	// different priorities on different overlays.
	Scope string `json:"scope,omitempty"`
}

// HostsSync controls writing peer hostnames into the OS hosts file.
type HostsSync struct {
	Enabled    bool          `json:"enabled"`
	GossipPPS  int           `json:"gossip_pps"`  // storm control on hostname announcements
	TTLSeconds int           `json:"ttl_seconds"` // remove entry if peer silent this long
	Path       string        `json:"path"`        // override OS hosts file (mainly for testing)
	ttl        time.Duration // cached
}

// HostRecord is a custom name -> IP entry a node advertises mesh-wide, so every
// peer adds it to its hosts file (beyond the automatic peer-hostname entries).
// The IP is arbitrary (an overlay address, or a LAN service reachable via an
// advertised route).
//
// Disabled mirrors the firewall-rule convention: the zero value is enabled, so
// records loaded from configs written before this field existed stay advertised.
// A disabled record is retained in config (name/IP intact for re-enabling) but
// is not advertised to the mesh.
type HostRecord struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Disabled bool   `json:"disabled,omitempty"` // true = record is not advertised; advertised by default
}

// HostReject is a local filter: a hostname this node refuses to accept from the
// mesh, so a custom host record peers advertise for that name is never written
// into this node's hosts file. It is the host-record analog of RejectRoute (a
// local refusal of advertised routes) and, like it, never floods to other nodes.
// Matching is by exact hostname, case-insensitive.
//
// Disabled mirrors the firewall-rule convention: the zero value is enabled, so a
// disabled entry stays in config but stops filtering, and the affected records
// are accepted again.
type HostReject struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
}

// DNSSync controls applying peer-advertised conditional-forwarding domains to
// this node's OS resolver. On by default, same as HostsSync: an unwanted
// domain is refused via DNSReject rather than by a master switch an operator
// has to remember to flip. Set Enabled=false directly in config to opt a node
// out of applying anything (advertising is unaffected either way — see
// DNSForward).
type DNSSync struct {
	Enabled bool `json:"enabled"`
	// GossipPPS storm-controls DNSForward/DNSReject advertisements, mirroring
	// HostsSync.GossipPPS. 0 disables the limit.
	GossipPPS int `json:"gossip_pps"`
	// TTLSeconds removes an advertised forward if the advertising peer goes
	// silent this long, mirroring HostsSync.TTLSeconds. 0 uses the default.
	TTLSeconds int `json:"ttl_seconds"`
	// DisableSearchDomains turns off search-suffix promotion for domains this
	// node only knows about via gossip. By default (the zero value, so every
	// existing config gets this without an edit) every conditional-forward
	// domain this node currently applies — its own DNSAdvertise entries *and*
	// whatever it has accepted from peers via DNSSync — also becomes a plain
	// search suffix, so an unqualified (single-label) query completes against
	// it too, not just a fully-qualified one.
	//
	// A node's own DNSAdvertise domains becoming search suffixes needs no
	// separate trust decision — that's this node's own configuration. A
	// peer-advertised forward learned via gossip is already trusted enough to
	// route fully-qualified queries under it (that trust is inherent in
	// DNSSync.Enabled itself); completing bare queries against it is a
	// modest further step in the same direction, not a separate trust
	// boundary, so it's on by default rather than something every node has to
	// remember to opt into. Set this true to opt a node back out, e.g. a
	// mesh where a peer's forwarded domain might collide with a name this
	// node expects to resolve locally.
	DisableSearchDomains bool `json:"disable_search_domains,omitempty"`
}

func (d DNSSync) TTL() time.Duration { return time.Duration(d.TTLSeconds) * time.Second }

// DNSForward is a conditional-forwarding rule a node advertises mesh-wide: any
// peer with DNSSync.Enabled registers Domain with its OS resolver as a routing
// (not search) domain pointed at Servers, so only fully-qualified queries under
// Domain are affected — bare hostnames are untouched, and names already served
// by the hosts file (HostsSync/HostsAdvertise) never reach this path at all,
// since every supported OS checks hosts before DNS.
//
// Disabled mirrors the firewall-rule/HostRecord convention: the zero value is
// enabled, so records from configs written before this field existed stay
// advertised.
type DNSForward struct {
	Domain   string   `json:"domain"`  // suffix to route, e.g. "corp.internal" (no leading dot)
	Servers  []string `json:"servers"` // upstream resolver IPs for Domain, tried in order
	Disabled bool     `json:"disabled,omitempty"`
}

// DNSReject is a local filter: a forwarding domain this node refuses to accept
// from the mesh, so a DNSForward peers advertise for it is never applied to
// this node's OS resolver. The domain analog of HostReject/RejectRoute.
// Matching is by exact domain, case-insensitive.
type DNSReject struct {
	Domain   string `json:"domain"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Route is a CIDR this node advertises into the mesh for redistribution.
type Route struct {
	CIDR    string `json:"cidr"`
	Metric  int    `json:"metric"`
	Enabled bool   `json:"enabled"`
}

// NATRule describes one translation. Source/Dest are CIDRs (blank = any).
//
// Translate says both what to rewrite a matching packet to and which
// direction the rewrite runs — there's no separate mode/direction field:
//   - "masquerade" (or blank, with Interface set): rewrite the source of
//     egress packets to Interface's address (SNAT — many local addresses
//     share one, e.g. giving a whole overlay subnet internet access).
//   - a literal IPv4: rewrite the source to that fixed address instead of
//     masquerading through an interface (static SNAT).
//   - "port-forward:<ipv4>": rewrite the destination of ingress packets to
//     that internal address instead (DNAT — replies get their source
//     restored automatically). DestPort/Proto (below) scope which packets
//     that covers; leaving both blank keeps the original all-ports
//     behavior.
//   - "port-forward:<ipv4>:<port>": same, but also rewrites the
//     destination *port* to <port> — port address translation (PAT), e.g.
//     forwarding a seed's public port 8443 through to an internal host's
//     443. Only valid together with a single-value DestPort (see
//     DestPort's own doc comment for why a range can't remap this way).
type NATRule struct {
	Source string `json:"source"`
	Dest   string `json:"dest"`
	// SourceNegate/DestNegate flip what their field matches: on, the rule
	// matches every address EXCEPT the prefix named, exactly as
	// FirewallRule.SrcNegate/DstNegate do (same semantics, same "!" marker
	// in the UI, same refusal to pair with an empty field). The two negate
	// independently and combine as AND, so "source not A and dest not B" is
	// one rule with both on — a case that cannot be written as any number
	// of positive rules, since rules OR together and there is no no-NAT
	// action to carve an exclusion with.
	//
	// Negation is a match-side concept only. It never touches the
	// translation target: a negated masquerade still takes its address
	// family from Source (see Validate), because the prefix still names a
	// family whether the rule matches inside it or outside it.
	//
	// Not every backend can express this. WinNAT's
	// -InternalIPInterfaceAddressPrefix takes one concrete prefix with no
	// inverse, so a negated rule is reported through netfilter's existing
	// unsupported-rule path rather than being silently rendered as its
	// positive twin.
	SourceNegate bool `json:"source_negate,omitempty"` // match anything EXCEPT Source
	DestNegate   bool `json:"dest_negate,omitempty"`   // match anything EXCEPT Dest
	// DestPort scopes a port-forward (DNAT) rule to a specific port or
	// range on the *original* (pre-translation) destination — "32400" or
	// "8000-8010" — instead of matching every port on Dest. Blank means
	// every port, the original address-only behavior. Ignored for
	// masquerade/static-SNAT rules, since a rewritten *source* port only
	// ever comes from connection tracking (many local ports sharing one
	// external address) — there's no meaningful "which source port does
	// this rule apply to" the way there is for a DNAT rule's original
	// destination port.
	DestPort string `json:"dest_port,omitempty"`
	// Proto restricts a DestPort match to "tcp" or "udp" — required
	// whenever DestPort is set (a port only means something for those two
	// protocols), and otherwise blank (matches any protocol, the same as
	// before this field existed).
	Proto     string `json:"proto,omitempty"`
	Translate string `json:"translate"`
	Interface string `json:"interface,omitempty"` // egress interface for masquerade
	Enabled   bool   `json:"enabled"`

	// Scope is deprecated and ignored as of v966. It named the mesh network
	// whose overlay traffic a rule also applied to, chosen separately from
	// the rule itself in the admin UI and CLI.
	//
	// It asked the operator to answer a second time, in coarser terms, a
	// question the rule's own fields already answer: Interface is a hard
	// match constraint in the kernel path (OutIface for masquerade/SNAT,
	// InIface for DNAT), so a rule naming a physical interface is by
	// construction about traffic crossing it and cannot describe overlay
	// traffic. Because the two were maintained independently they could
	// disagree, and a rule whose Scope named a network its own prefixes
	// could never match was silently inert — with nothing on screen saying
	// so. natRuleAppliesToOverlay now derives the answer.
	//
	// Retained only so configs written before v966 still parse, the same way
	// Direction and DestNetwork below are. It is cleared on load (see
	// Config.Validate) rather than migrated, because there is nothing to
	// migrate to: the rule already carries the information.
	Scope string `json:"scope,omitempty"`

	// Direction and DestNetwork are deprecated. An earlier version had a
	// separate 3-value direction selector (overlay2underlay/
	// underlay2overlay/overlay2overlay) alongside Translate, with
	// DestNetwork meant to further distinguish overlay2overlay — except
	// DestNetwork was never actually read anywhere, so overlay2overlay
	// rules ran identically to overlay2underlay ones in the userspace NAT
	// engine, and only differed (silently) in whether a redundant
	// kernel-level rule also got installed. Direction is retained only so
	// old configs still parse: on load, an "underlay2overlay" rule's
	// Translate gets "port-forward:" prefixed onto it (see
	// Config.Validate) so it keeps meaning DNAT under the new
	// Translate-carries-the-mode scheme; "overlay2underlay" and
	// "overlay2overlay" both just drop the field, since both already
	// meant plain SNAT. DestNetwork is dropped outright — there was never
	// any real data in it to migrate.
	Direction string `json:"direction,omitempty"`
}

// NAT is this node's address translation. It is off by default; when disabled
// no translation happens. Individual rules also have their own Enabled flag.
//
// Node-global from v953, not per mesh network. A NAT rule is a statement about
// packets, not about an overlay: the kernel rules this produces (see
// kernelNATRules and internal/netfilter) carry no overlay identity whatsoever
// — a Kind, two prefixes, an interface and a target — so "masquerade
// 192.168.1.0/24 out eth0" is an ordinary router rule that a node with no mesh
// network at all can and should be able to write. The per-network nesting was
// history, and its only real effect was to make NAT unreachable on a node
// running gravinet as a plain LAN router.
//
// Which rules the overlay half also enforces is derived from the rules
// themselves — see natRuleAppliesToOverlay in cmd/gravinet and NATRule.Scope's
// doc comment for the selector it replaced.
type NAT struct {
	Enabled bool      `json:"enabled"`
	Rules   []NATRule `json:"rules"`
	// StateTimeout is deprecated. It is retained only so old configs still parse:
	// on load any non-zero value is hoisted into the global Config.NATStateTimeout
	// and this field is cleared. Use the global setting instead.
	StateTimeout int `json:"state_timeout,omitempty"`
}

// WebAdmin configures the admin interface.
type WebAdmin struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"` // e.g. 127.0.0.1:8443

	// ListenAddrs is the set of IP addresses the admin interface binds, as
	// chosen in Settings. The port always comes from Listen — this picks
	// addresses, not sockets, which is the whole question an operator has
	// here ("answer on my LAN address too", "stop answering on loopback").
	//
	// Empty means the default, and the default is deliberately not written
	// out: loopback plus this node's mesh overlay addresses, which is exactly
	// what an unconfigured node already does today (loopback from Listen,
	// overlay addresses from the automatic cluster-management binder). So an
	// existing config keeps its current behaviour with no migration, and the
	// picker starts pre-selected on those same addresses rather than on
	// nothing.
	//
	// Entries are bare IP literals, never names: this binds a socket, and a
	// name that resolved differently later would silently change what the
	// admin interface is exposed on. An address that isn't present yet (a
	// DHCP lease, an interface still coming up) is retried rather than
	// failing startup, and one unbindable entry never costs the others.
	ListenAddrs []string `json:"listen_addrs,omitempty"`

	TLSCert    string `json:"tls_cert"` // path; self-signed generated if empty
	TLSKey     string `json:"tls_key"`
	AuthMode   string `json:"auth_mode"`   // "local", "pam" (linux/macos/freebsd), "system" (openbsd bsd_auth), or "windows"
	PAMService string `json:"pam_service"` // e.g. "gravinet" or "login"
	// AllowUsers is retained only for backward-compatible JSON decoding of
	// old config files; it is no longer consulted for anything. Sign-in
	// under a system-auth mode (pam/system/windows) is gated by membership
	// in the local "gravinet" OS group instead (root is always exempt) —
	// see service.IsGroupMember and the System > Users admin page, which
	// manages that group's membership directly rather than this field.
	AllowUsers []string    `json:"allow_users,omitempty"`
	Users      []AdminUser `json:"users"` // for auth_mode "local"
	LoginBan   BanPolicy   `json:"login_ban"`

	// GeoIPLookup adds an approximate location (city/region/country + a map)
	// to the peer/seed info (🛈) panel, derived from the target's public IP.
	// nil means the default, which is enabled: the info panel's
	// forward/reverse DNS and WHOIS lookups already run unconditionally on
	// the same admin-triggered click, so this joins them rather than needing
	// separate opt-in — but it's still a call to one specific commercial
	// third party (ipapi.co) rather than the internet's own decentralized
	// lookup protocols, so set to false to keep this node from ever
	// contacting one. Use GeoIPEnabled rather than reading this directly.
	//
	// *bool (like IPForwarding above), not a plain bool: Load() seeds a
	// fresh Config from Default() and unmarshals the file's JSON on top of
	// it. A plain bool with omitempty can't express false at all — Marshal
	// drops a false value from the file entirely, so the very next Load()
	// would silently resurrect the Default()-seeded true. Dropping omitempty
	// instead "fixes" that (false now round-trips) but trades away something
	// else: SaveTo marshals the whole config on every edit, not just this
	// field, so the first unrelated save after upgrading would permanently
	// bake an explicit true into the file — indistinguishable from an
	// operator's deliberate choice, and immune to Default() ever changing
	// again later. nil genuinely means "never touched": omitempty on a nil
	// pointer keeps the key out of the file across any number of unrelated
	// saves, for as long as nothing actually sets it.
	GeoIPLookup *bool `json:"geoip_lookup,omitempty"`

	// AllowRemoteShell enables a real OS shell/PTY session through the web
	// admin — for this node directly, and (via the existing Manager/Managed
	// proxy) for a Manager peer opening a shell here too. Off by default,
	// and deliberately separate from Managed: Managed only ever exposed this
	// app's own API surface (firewall rules, peers, keys, ...), which is a
	// meaningfully different risk than a full OS shell running as this
	// daemon's own user (see cmd/gravinet's -h: normally root). Turning
	// Managed on for the web-console proxy must never silently also hand out
	// a root shell.
	//
	// Unlike Managed/Manager, this is never remotely toggleable — not even by
	// an authorized Manager peer over the overlay (see handleShellSetting's
	// doc comment for why that's tighter than Managed/Manager's own "local
	// only" intent). And like AuthMode/Users/GeoIPLookup, it's captured once
	// at startup into Server.cfg and needs a restart to change — deliberately
	// so for a flag this sensitive, not just an artifact of how the other
	// WebAdmin-scoped settings happen to work.
	AllowRemoteShell bool `json:"allow_remote_shell,omitempty"`
}

// AdminUser is a local admin credential (auth_mode "local"). The password is
// stored as a PBKDF2-HMAC-SHA256 hash; generate one with `gravinet genpass`.
type AdminUser struct {
	Name       string `json:"name"`
	Salt       string `json:"salt"`       // hex-encoded
	Hash       string `json:"hash"`       // hex-encoded derived key
	Iterations int    `json:"iterations"` // PBKDF2 iteration count
}

// BanPolicy is the shared brute-force throttle used by both the auth handshake
// and the admin login: N failures within Window ⇒ ban for Duration. Failures
// arriving within Coalesce of each other count as one (so a single join that
// tries several keys isn't over-counted).
type BanPolicy struct {
	MaxFailures     int `json:"max_failures"`     // default 3
	WindowSeconds   int `json:"window_seconds"`   // default 60
	BanSeconds      int `json:"ban_seconds"`      // default 900 (15 min)
	CoalesceSeconds int `json:"coalesce_seconds"` // failures within this window count once
}

func (b BanPolicy) Window() time.Duration   { return time.Duration(b.WindowSeconds) * time.Second }
func (b BanPolicy) Ban() time.Duration      { return time.Duration(b.BanSeconds) * time.Second }
func (b BanPolicy) Coalesce() time.Duration { return time.Duration(b.CoalesceSeconds) * time.Second }

// EffectiveMaxFailures returns MaxFailures, or the default (3) if it is 0 or
// unset. Shared by Server.New (which builds the throttle from it) and the web
// UI's /api/config response (which shows the operator the value actually in
// effect), so the two can never silently disagree about what "default" means.
func (b BanPolicy) EffectiveMaxFailures() int {
	if b.MaxFailures <= 0 {
		return 3
	}
	return b.MaxFailures
}

// EffectiveBanSeconds returns BanSeconds, or the default (900s/15min) if it
// is 0 or unset. See EffectiveMaxFailures's doc comment for why this exists
// rather than each caller re-deriving the same default independently.
func (b BanPolicy) EffectiveBanSeconds() int {
	if b.BanSeconds <= 0 {
		return 900
	}
	return b.BanSeconds
}

func (h HostsSync) TTL() time.Duration { return time.Duration(h.TTLSeconds) * time.Second }

// Default returns a config with sane defaults and one empty disabled network.
// ShortHostname strips any domain suffix from a hostname.
//
// os.Hostname() conventionally returns a short name on Linux, macOS, Windows
// and FreeBSD, but OpenBSD's /etc/myname is very commonly a full FQDN (e.g.
// "gn-openbsd.cush.local") and os.Hostname() there echoes it back verbatim.
// gravinet's Hostname is gossiped mesh-wide and used for peer display and
// bare-hostname resolution, so a lone FQDN breaks both: the peers table shows
// it inconsistently beside every other node's short name, and "ping
// gn-openbsd" would not resolve the way "ping gn-cush1" does.
//
// Applied at the two points where a name enters Config.Hostname from outside
// — cmd/gravinet's startup fill-in, and the web admin's host rename under
// System > Resolver — rather than on every read, so the value in the file is
// already the value advertised. A Hostname edited directly in the config is
// taken verbatim and never passed through here.
func ShortHostname(s string) string {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i]
	}
	return s
}

func Default() *Config {
	return &Config{
		LogLevel:      "info",
		UDPPorts:      []int{DefaultUDPPort},
		TCPPorts:      []int{DefaultTCPPort},
		EnableIPv4:    true,
		EnableIPv6:    true,
		WorkerThreads: 0,
		AuthBan:       BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900, CoalesceSeconds: 3},
		// On by default from v993. A node that publishes its own name is the
		// behaviour somebody setting up a gateway wants without having to know
		// this page exists; set the interval to 0 to switch it off, which the
		// unmarshaller preserves because the field is written out explicitly.
		DDNS: DDNSConfig{IntervalMinutes: DefaultDDNSInterval, TTL: DefaultDDNSTTL},
		// Deliberately left empty rather than set to DefaultControlSocket: writing
		// the current platform default into the scaffolded file freezes it there,
		// so a later correction to the default (as in v393) can never reach an
		// existing install — that's exactly how the stale "/run/gravinet.sock"
		// outlived its fix. Empty means "follow the platform default", resolved at
		// runtime by NormalizeControlSocket, and stays correct if the config is
		// ever copied to another platform. Set it explicitly to pin a path.
		ControlSocket: "",
		WebAdmin: WebAdmin{
			Enabled:    true,
			Listen:     "127.0.0.1:8443",
			AuthMode:   defaultAuthMode(),
			PAMService: "gravinet",
			LoginBan:   BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900},
			// GeoIPLookup deliberately left nil (not GeoIPLookup: true) — see
			// its doc comment for why nil, not a literal true here, is what
			// actually makes "on by default, explicit false persists as
			// false" both true at once. Use GeoIPEnabled(), not this field
			// directly.
		},
		Networks: []Network{},
	}
}

// RejectRoute is one entry in a network's route-reject list. By default a reject
// matches only the exact advertised prefix (CIDR); set Inclusive to also reject
// every more-specific network contained within it.
//
// For backward compatibility it serialises as a bare JSON string when not
// inclusive (so the historical ["0.0.0.0/0"] form is preserved) and as an object
// {"cidr":...,"inclusive":true} when inclusive. On read it accepts either form.
type RejectRoute struct {
	CIDR      string `json:"cidr"`
	Inclusive bool   `json:"inclusive,omitempty"`
	// Disabled follows the firewall-rule convention: the zero value is enabled,
	// so reject entries written before this field existed — including the legacy
	// bare-string "0.0.0.0/0" default — stay in force. A disabled entry is kept
	// in config but not applied, so advertised routes it would have refused are
	// accepted again.
	Disabled bool `json:"disabled,omitempty"`
}

// PreferRoute expresses a receiver-side preference between peers advertising
// the same prefix.
//
// Ordinarily the lowest advertised metric wins, which puts the choice entirely
// in the advertisers' hands. That is the right default — an exit node knows
// its own cost better than its peers do — but it leaves a node with no way to
// say "several of you offer 0.0.0.0/0 and I want that one", short of asking
// the other operators to renumber their metrics.
//
// Origins is ordered, most preferred first, and holds node IDs. Preference is
// applied as a comparison key ahead of metric, not as a filter, so it is a
// preference and not a pin: bestRedistOrigins has already discarded any origin
// with no live session or a dark address family by the time ranking happens,
// so a preferred peer going away simply removes it from the candidates and the
// next-ranked one — or, if none are named, the lowest metric — takes over on
// its own. Nothing has to time out and no route is withdrawn in between.
//
// Origins not listed here rank below every listed one, and among themselves by
// metric as before. Listing an origin that never advertises the prefix is
// harmless: it can never win a comparison it is not part of.
type PreferRoute struct {
	CIDR    string   `json:"cidr"`
	Origins []string `json:"origins"`
	// Disabled follows the same convention as RejectRoute: zero value is
	// enabled, and a disabled entry stays in config but stops applying, so
	// selection reverts to plain lowest-metric.
	Disabled bool `json:"disabled,omitempty"`
}

func (r *RejectRoute) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil { // legacy bare-string form
		r.CIDR = s
		r.Inclusive = false
		r.Disabled = false
		return nil
	}
	type alias RejectRoute
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*r = RejectRoute(a)
	return nil
}

func (r RejectRoute) MarshalJSON() ([]byte, error) {
	// The bare-string form can only carry the CIDR, so it is used only when the
	// entry is in its default state (enabled and non-inclusive). Any extra state
	// forces the object form.
	if !r.Inclusive && !r.Disabled {
		return json.Marshal(r.CIDR)
	}
	type alias RejectRoute
	return json.Marshal(alias(r))
}

// NewNetworkDefaults fills a Network with defaults for a fresh overlay.
func NewNetworkDefaults() Network {
	return Network{
		Enabled:      true,
		MTU:          protocol.DefaultTunnelMTU,
		StormControl: StormControl{BroadcastPPS: 100, MulticastPPS: 200, Burst: 200},
		HostsSync:    HostsSync{Enabled: true, GossipPPS: 5, TTLSeconds: 300},
		// DNSSync defaults on, same as HostsSync: control happens through the
		// advertise/reject lists, not a master switch an operator has to
		// remember to flip. GossipPPS/TTLSeconds mirror HostsSync's defaults.
		// DisableSearchDomains is left at its zero value (false), so search-
		// suffix promotion for learned forwards is on by default too — see
		// its doc on DNSSync.
		DNSSync:    DNSSync{Enabled: true, GossipPPS: 5, TTLSeconds: 300},
		AllowRelay: true,
		// Reject a learned default route by default: advertising 0.0.0.0/0 (or
		// ::/0) over the mesh would install "default dev <tun>" on every peer
		// and loop their underlay into the tunnel. Both families are listed —
		// an earlier version of this default only covered 0.0.0.0/0, leaving
		// a peer-advertised ::/0 accepted (and hitting the same loop) on any
		// network with IPv6 enabled. Remove these entries to opt a node into
		// accepting a full-tunnel default (see fulltunnel.go for how that's
		// then kept from looping the mesh's own traffic into itself).
		RouteRej: []RejectRoute{{CIDR: "0.0.0.0/0"}, {CIDR: "::/0"}},
	}
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := Default()
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.path = path
	// Before anything reads a port: fold the pre-v789 primary/fallback keys
	// into the flat lists, so no consumer ever sees the old shape. Decoded
	// separately because Config no longer has fields for them — see
	// legacyPorts.
	var pod portsOnDisk
	_ = json.Unmarshal(data, &pod)
	c.migratePortConfig(pod)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// fileLocks holds one mutex per config file path, process-wide.
var (
	fileLocksMu sync.Mutex
	fileLocks   = map[string]*sync.Mutex{}
)

func lockFor(path string) *sync.Mutex {
	fileLocksMu.Lock()
	defer fileLocksMu.Unlock()
	l, ok := fileLocks[path]
	if !ok {
		l = &sync.Mutex{}
		fileLocks[path] = l
	}
	return l
}

// Lock returns the process-wide mutex for a config file path, for a caller
// whose existing control flow (many early returns, no natural func() error
// boundary) makes WithLock's shape awkward to retrofit — e.g. the engine's
// persist hook. Prefer WithLock for new code; this is the same underlying
// per-path lock either way, so the two compose correctly together.
func Lock(path string) *sync.Mutex { return lockFor(path) }

// WithLock runs fn (typically a Load, mutate, SaveTo sequence) while holding
// a process-wide lock scoped to path, so two independent read-modify-write
// cycles against the same config file can't race.
//
// This matters because gravinet has (at least) two independent writers: the
// web admin's own editor, and the engine's async persist hook (mesh-learned
// state — addresses, propagated keys, retractions, route/DNS/host
// advertisements — written back so it survives a restart), fired via
// notifyChange on its own schedule, unrelated to any web admin request. Each
// writer used to serialize only against itself (its own local mutex); with no
// shared lock between the two, a persist-hook cycle that started loading the
// file just before a web admin edit saved would still be holding an
// old in-memory copy when it saved afterward — silently reverting the web
// admin's change with no error anywhere. This was most visible on a field the
// persist hook has no independent way to re-derive if lost (a key's
// Distributed flag: nothing else in the engine ever recomputes it), which is
// what made it look tied to that feature specifically — but the race applies
// to any web admin edit landing at the wrong moment, not just that one field.
func WithLock(path string, fn func() error) error {
	l := lockFor(path)
	l.Lock()
	defer l.Unlock()
	return fn()
}

// Save atomically writes the config back to its path (used by the web admin).
func (c *Config) Save() error {
	if c.path == "" {
		return fmt.Errorf("config has no path")
	}
	return c.SaveTo(c.path)
}

// SaveTo atomically writes the config to an explicit path and records it as the
// config's path for subsequent Save calls. The write goes to a uniquely-named
// temp file in the same directory (so the final rename stays on one filesystem)
// created with 0600 up front — a fixed ".tmp" name would let two concurrent
// saves clobber each other's temp file and, as a predictable name, invites a
// symlink pre-creation attack. The temp is cleaned up on any failure.
func (c *Config) SaveTo(path string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".config-*.json.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// On any error past this point, don't leave the temp file behind.
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := f.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	c.path = path
	return nil
}

// Path returns the on-disk location.
func (c *Config) Path() string { return c.path }

// EffectiveConfigHistoryLimit returns ConfigHistoryLimit, or the default
// (250, matching parapet's own config_backups default) if it's 0 or unset.
func (c *Config) EffectiveConfigHistoryLimit() int {
	if c.ConfigHistoryLimit <= 0 {
		return 250
	}
	return c.ConfigHistoryLimit
}

// Clone returns a deep copy via a JSON round-trip — simpler and less
// error-prone than a hand-written field-by-field copy that has to be kept in
// sync as fields are added, at the cost of needing every field to already be
// (un)marshalable, which is already true of the whole Config (it's saved and
// loaded this same way). Used by history.go to capture the "before" state
// right after Load, since the caller's mutator function changes the same
// struct in place — after it runs, the pre-change state only survives if
// something copied it first.
func (c *Config) Clone() (*Config, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	clone := &Config{}
	if err := json.Unmarshal(data, clone); err != nil {
		return nil, err
	}
	clone.path = c.path
	return clone, nil
}

// ForwardingEnabled reports whether the daemon should enable host IP forwarding
// at startup. Defaults to true when unset (nil); an explicit false opts out.
func (c *Config) ForwardingEnabled() bool {
	return c.IPForwarding == nil || *c.IPForwarding
}

// RedirectsDisabled reports whether the daemon should turn off host
// acceptance/sending of ICMP redirects at startup. Defaults to true when
// unset (nil) — unlike ForwardingEnabled, the safer default here is "on"
// (redirects off), since accepting them is rarely needed and is a known
// route-table-spoofing vector; an explicit false leaves the host's redirect
// settings untouched.
func (c *Config) RedirectsDisabled() bool {
	return c.DisableRedirects == nil || *c.DisableRedirects
}

// GeoIPEnabled reports whether the peer/seed info panel should attempt a
// Geo-IP lookup. Defaults to true when unset (nil); an explicit false opts
// out. See WebAdmin.GeoIPLookup's doc comment for why this indirection
// (rather than reading the field directly) is what makes that combination
// actually work.
func (w WebAdmin) GeoIPEnabled() bool {
	return w.GeoIPLookup == nil || *w.GeoIPLookup
}

// RouteAdvDuration is the resolved route re-advertisement interval: the
// configured value in seconds, defaulting to 10s when unset and floored at 1s.
func (c *Config) RouteAdvDuration() time.Duration {
	if c.RouteAdvInterval <= 0 {
		return 10 * time.Second
	}
	if c.RouteAdvInterval < 1 {
		return time.Second
	}
	return time.Duration(c.RouteAdvInterval) * time.Second
}

// KeepaliveDuration is the resolved NAT-keepalive cadence: the configured
// value in seconds, defaulting to 10s when unset and floored at 1s.
func (c *Config) KeepaliveDuration() time.Duration {
	if c.KeepaliveInterval <= 0 {
		return 10 * time.Second
	}
	if c.KeepaliveInterval < 1 {
		return time.Second
	}
	return time.Duration(c.KeepaliveInterval) * time.Second
}

// PeerTimeoutDuration is the resolved dead-session timeout: the configured
// value in seconds, defaulting to 30s when unset, floored at 1s, and — like
// mesh.Engine.SetPeerTimeout — clamped up to KeepaliveDuration if an
// explicit value would otherwise be shorter than a single keepalive cadence.
func (c *Config) PeerTimeoutDuration() time.Duration {
	d := 30 * time.Second
	if c.PeerTimeout > 0 {
		d = time.Duration(c.PeerTimeout) * time.Second
		if d < time.Second {
			d = time.Second
		}
	}
	if ka := c.KeepaliveDuration(); d < ka {
		d = ka
	}
	return d
}

// Validate checks structural invariants and normalizes a few fields.
func (c *Config) Validate() error {
	// A node listens on a set of UDP ports and a set of TCP ports. Either may
	// be empty (that protocol is off); both empty means the node has no
	// transport at all and could never be reached, which is the one
	// combination worth refusing outright.
	for _, spec := range []struct {
		name  string
		ports []int
	}{{"udp_ports", c.UDPPorts}, {"tcp_ports", c.TCPPorts}} {
		for _, p := range spec.ports {
			if p < 1 || p > 65535 {
				return fmt.Errorf("%s: %d out of range (1-65535)", spec.name, p)
			}
		}
	}
	if !c.UDPEnabled() && !c.TCPEnabled() {
		return fmt.Errorf("udp_ports and tcp_ports are both empty — at least one underlay transport must stay on, or this node could never be reached")
	}
	// A configured log cap must parse; reject bad input at save time so the
	// running daemon never has to fall back silently (see LogMaxBytes).
	if strings.TrimSpace(c.LogMaxSize) != "" {
		if _, err := ParseSize(c.LogMaxSize); err != nil {
			return fmt.Errorf("log_max_size: %v", err)
		}
	}
	if err := c.ValidateHostVLANs(); err != nil {
		return err
	}
	// gravinet stopped serving DHCP itself in v988. Retire the mode a node
	// that served was carrying before anything validates it: ValidDHCPMode
	// refuses it, and this runs on every Load, so an unhandled "server" would
	// stop the daemon rather than the server. See migrateServerMode.
	if err := c.DDNS.Validate(); err != nil {
		return fmt.Errorf("ddns: %v", err)
	}
	c.DHCP.migrateServerMode()
	// The relay grew from one global interface list to a list of links in
	// v949. Fold any legacy shape in before validating, so nothing downstream
	// ever sees the old fields — see migrateRelay.
	c.DHCP.Relay.migrateRelay()
	if err := c.DHCP.Validate(); err != nil {
		return fmt.Errorf("dhcp: %v", err)
	}
	// SNMP grew from a single community string to a list in v736. Migrate
	// any legacy value into the new field (only if the new field is still
	// unset, so a config already using Communities is never second-guessed)
	// and clear the old one so it's never written back out.
	if len(c.SNMP.Communities) == 0 && c.SNMP.Community != "" {
		c.SNMP.Communities = []SNMPCommunity{{Community: c.SNMP.Community}}
	}
	c.SNMP.Community = ""
	// NAT state timeout is a single global setting. Migrate any legacy per-network
	// value (largest wins) into the global field, then clear the old fields so
	// they are no longer written.
	if c.NATStateTimeout == 0 {
		for i := range c.Networks {
			if c.Networks[i].NAT.StateTimeout > c.NATStateTimeout {
				c.NATStateTimeout = c.Networks[i].NAT.StateTimeout
			}
		}
	}
	for i := range c.Networks {
		c.Networks[i].NAT.StateTimeout = 0
	}
	if c.NATStateTimeout < 0 || c.NATStateTimeout > 86400 {
		return fmt.Errorf("nat_state_timeout must be 0..86400 seconds")
	}
	// A NAT rule's Direction field is deprecated (see NATRule's doc comment):
	// migrate an "underlay2overlay" rule's meaning into its Translate value
	// via the port-forward: prefix, then clear Direction unconditionally so
	// it's never written back out. "overlay2underlay" and "overlay2overlay"
	// both already meant plain SNAT and need no Translate change, just the
	// field cleared.
	// NAT moved from per-network to node-global in v953. Hoist first, so the
	// Direction migration below has a single list to walk — running it over
	// the per-network rules instead would silently skip a config already in
	// the node-global shape, leaving a legacy Direction to be written back out
	// forever.
	c.migrateNAT()
	// QoS made the same move in v954, and the bandwidth limit in v955 —
	// which v960 then re-keyed from networks to the interfaces the shaper
	// actually runs on.
	c.migrateQoS()
	c.migrateShaping()
	// The firewall made the same move in v957.
	c.migrateFirewall()
	for j := range c.NAT.Rules {
		{
			r := &c.NAT.Rules[j]
			if strings.EqualFold(r.Direction, "underlay2overlay") {
				t := strings.TrimSpace(r.Translate)
				if t != "" && !strings.EqualFold(t, "masquerade") && !strings.HasPrefix(strings.ToLower(t), "port-forward:") {
					r.Translate = "port-forward:" + t
				}
				// else: an underlay2overlay rule with translate left as
				// masquerade/blank was always a rare DNAT-to-self combination
				// (only meaningful if the interface's own address was the
				// intended forward target) with no clean equivalent under the
				// new scheme; it falls back to plain SNAT/masquerade here
				// rather than guessing at an address.
			}
			r.Direction = ""
			// Scope is dropped, not migrated: the rule already carries
			// what it encoded (see NATRule.Scope). Clearing it on load
			// keeps a pre-v966 config from writing a field back out that
			// nothing reads any more, which would leave an operator
			// editing a selector with no effect.
			r.Scope = ""
		}
	}
	if !c.EnableIPv4 && !c.EnableIPv6 {
		return fmt.Errorf("at least one of enable_ipv4/enable_ipv6 must be true")
	}
	seenNet := map[string]bool{}
	for i := range c.Networks {
		n := &c.Networks[i]
		if n.ID == "" {
			return fmt.Errorf("network[%d] missing id", i)
		}
		if seenNet[n.ID] {
			return fmt.Errorf("duplicate network id %q", n.ID)
		}
		seenNet[n.ID] = true
		if n.MTU == 0 {
			n.MTU = protocol.DefaultTunnelMTU
		}
		if n.MTU < 576 || n.MTU > 65535 {
			return fmt.Errorf("network %s: mtu %d out of range", n.ID, n.MTU)
		}
		if n.Subnet4 != "" {
			if _, err := netip.ParsePrefix(n.Subnet4); err != nil {
				return fmt.Errorf("network %s: bad subnet4: %w", n.ID, err)
			}
		}
		if n.Subnet6 != "" {
			if _, err := netip.ParsePrefix(n.Subnet6); err != nil {
				return fmt.Errorf("network %s: bad subnet6: %w", n.ID, err)
			}
		}
		if n.Mesh != "" && !strings.EqualFold(n.Mesh, "full") && !strings.EqualFold(n.Mesh, "partial") {
			return fmt.Errorf("network %s: mesh %q must be \"full\" or \"partial\" (or omitted, which means full)", n.ID, n.Mesh)
		}
		if n.Subnet4 == "" && n.Subnet6 == "" && len(n.Seeds) == 0 && len(n.PeerCache) == 0 {
			return fmt.Errorf("network %s: needs subnet4 and/or subnet6 (or a seed to learn it from)", n.ID)
		}
		for j := range n.Keys {
			if e := n.Keys[j].Expires; e != "" {
				if _, err := time.Parse(time.RFC3339, e); err != nil {
					return fmt.Errorf("network %s: key[%d] bad expires %q (want RFC3339, e.g. 2026-12-31T23:59:59Z): %w", n.ID, j, e, err)
				}
			}
		}
		for j := range n.Routes {
			if _, err := netip.ParsePrefix(n.Routes[j].CIDR); err != nil {
				return fmt.Errorf("network %s: route[%d] bad cidr: %w", n.ID, j, err)
			}
		}
		for _, r := range n.RouteRej {
			if _, err := netip.ParsePrefix(r.CIDR); err != nil {
				return fmt.Errorf("network %s: route_reject %q: %w", n.ID, r.CIDR, err)
			}
		}
		for _, r := range n.RoutePrefer {
			if _, err := netip.ParsePrefix(r.CIDR); err != nil {
				return fmt.Errorf("network %s: route_prefer %q: %w", n.ID, r.CIDR, err)
			}
			if len(r.Origins) == 0 {
				return fmt.Errorf("network %s: route_prefer %q: needs at least one origin node id", n.ID, r.CIDR)
			}
			seen := make(map[string]bool, len(r.Origins))
			for _, o := range r.Origins {
				o = strings.TrimSpace(o)
				if o == "" {
					return fmt.Errorf("network %s: route_prefer %q: empty origin node id", n.ID, r.CIDR)
				}
				if seen[o] {
					// A duplicate is always a mistake and never a harmless
					// one: the ranking is positional, so the second copy is
					// unreachable and silently changes what every origin
					// after it ranks against.
					return fmt.Errorf("network %s: route_prefer %q: origin %q listed twice", n.ID, r.CIDR, o)
				}
				seen[o] = true
			}
		}
		for _, h := range n.HostsAdvertise {
			if strings.TrimSpace(h.Name) == "" {
				return fmt.Errorf("network %s: hosts_advertise: empty name", n.ID)
			}
			if _, err := netip.ParseAddr(h.IP); err != nil {
				return fmt.Errorf("network %s: hosts_advertise %q: invalid ip %q", n.ID, h.Name, h.IP)
			}
		}
		for _, h := range n.HostsReject {
			if strings.TrimSpace(h.Name) == "" {
				return fmt.Errorf("network %s: hosts_reject: empty name", n.ID)
			}
		}
		for _, d := range n.DNSAdvertise {
			dom := strings.TrimSpace(d.Domain)
			if dom == "" {
				return fmt.Errorf("network %s: dns_advertise: empty domain", n.ID)
			}
			if strings.HasPrefix(dom, ".") || strings.HasPrefix(dom, "~") {
				return fmt.Errorf("network %s: dns_advertise %q: domain must not include a leading '.' or '~' (added automatically where the OS requires it)", n.ID, dom)
			}
			if len(d.Servers) == 0 {
				return fmt.Errorf("network %s: dns_advertise %q: no servers", n.ID, dom)
			}
			for _, s := range d.Servers {
				if _, err := netip.ParseAddr(s); err != nil {
					return fmt.Errorf("network %s: dns_advertise %q: invalid server %q", n.ID, dom, s)
				}
			}
		}
		for _, d := range n.DNSReject {
			if strings.TrimSpace(d.Domain) == "" {
				return fmt.Errorf("network %s: dns_reject: empty domain", n.ID)
			}
		}
		// Default reject list is [0.0.0.0/0, ::/0] so a node never silently
		// installs a full-tunnel default learned from the mesh, in either
		// address family. nil means "unset" → apply the default; an explicit
		// list (including an empty one) is the operator's choice and is left
		// alone, so removing the entries sticks.
		if n.RouteRej == nil {
			n.RouteRej = []RejectRoute{{CIDR: "0.0.0.0/0"}, {CIDR: "::/0"}}
		}
	}
	// QoS is inert without an egress rate cap to create contention for the
	// priority queue to reorder, so a node with QoS on needs an up-throttle
	// on every interface the classifier runs over. QoS is node-global (v954)
	// but shaping is per interface (v960), so this is one statement per mesh
	// interface rather than the single one it was between v955 and v959.
	// The placeholder is one the operator should lower to their real uplink.
	//
	// Only mesh interfaces. Seeding a rate onto an entry the operator wrote
	// for something else would be turning on a cap they did not ask for, on
	// an interface QoS does not classify anyway.
	if c.QoS.Enabled {
		for _, iface := range c.MeshIfaces() {
			s := c.ShapingFor(iface)
			if s == nil {
				c.Shaping = append(c.Shaping, IfaceShaping{Iface: iface})
				s = &c.Shaping[len(c.Shaping)-1]
			}
			s.Enabled = true
			if s.UpBytesPerSec <= 0 {
				s.UpBytesPerSec = defaultQoSUpBytesPerSec
			}
		}
	}
	// QoS class geometry: 5 priority classes by default with class 3 (normal)
	// for unmatched traffic. Classes 0-2 are above normal, 4 is bulk. Migrates
	// older 3-class configs up so existing rules (classes 0-2) keep working.
	// One geometry per node from v954, so this runs once rather than per
	// network.
	if c.QoS.Enabled {
		if c.QoS.Classes < 5 {
			c.QoS.Classes = 5
		}
		if c.QoS.DefaultClass <= 0 || c.QoS.DefaultClass >= c.QoS.Classes {
			c.QoS.DefaultClass = 3
		}
		for _, r := range c.QoS.Rules {
			if err := c.checkQoSScope(r.Scope); err != nil {
				return fmt.Errorf("qos: %v", err)
			}
		}
	}
	if err := validateFirewallCatalog(c.FirewallObjects, c.FirewallServices); err != nil {
		return fmt.Errorf("firewall catalog: %w", err)
	}
	for j, ex := range c.FirewallExempts {
		if err := validateExempt(ex); err != nil {
			return fmt.Errorf("firewall_exempt[%d]: %w", j, err)
		}
	}
	if c.Upgrade.ConfirmSeconds < 0 {
		return fmt.Errorf("upgrade.confirm_seconds %d is negative", c.Upgrade.ConfirmSeconds)
	}
	// BGP: an enabled speaker needs a local AS. Everything else the FRR
	// renderer filters defensively (a neighbor with an empty peer or a zero
	// remote-as, an unsafe network token, etc. is simply skipped, never
	// emitted into frr.conf), so validation here is deliberately light — it
	// rejects only the one combination that can't produce a runnable config,
	// mirroring the renderer's own `enabled && asn > 0` gate, and gives a
	// clear error instead of silently writing a BGP block bgpd would refuse.
	// AutoBGP is the exception: it derives the AS number from this node's own
	// tunnel address on its next pass, so "enabled with no ASN yet" is a real
	// and temporary state rather than an unrunnable config.
	//
	// Without this exception the two settings cannot be independent. Enabling
	// BGP would require an ASN, so a fresh node could only get one by having
	// AutoBGP turn BGP on for it — which is exactly the coupling that made
	// the disable switch inoperable.
	if c.BGP.Enabled && c.BGP.ASN == 0 && !c.BGP.AutoBGP {
		return fmt.Errorf("bgp: a local AS number is required to enable BGP (or turn on AutoBGP to derive one)")
	}
	// BGP timers: hold must exceed keepalive (FRR needs hold >= keepalive, and
	// the conventional ratio is 3:1); a non-zero hold below FRR's floor of 3s is
	// rejected. 0/0 means "use FRR defaults" and is fine.
	if c.BGP.Enabled {
		if c.BGP.HoldTime > 0 && c.BGP.HoldTime < 3 {
			return fmt.Errorf("bgp: hold time %ds is below the minimum of 3s", c.BGP.HoldTime)
		}
		if c.BGP.HoldTime > 0 && c.BGP.HoldTime <= c.BGP.KeepaliveTime {
			return fmt.Errorf("bgp: hold time (%ds) must be greater than keepalive (%ds)", c.BGP.HoldTime, c.BGP.KeepaliveTime)
		}
	}
	return nil
}

// validateFirewallCatalog checks the structural sanity of the node-global
// address objects and services: recognised kinds, non-empty names, and (for
// groups) that every referenced member exists. It deliberately does not
// reject rules that reference an unknown object — the engine logs and skips
// those — but it does catch the common typos in the catalog itself at load
// time.
func validateFirewallCatalog(objects []FirewallObject, services []FirewallService) error {
	names := make(map[string]bool, len(objects))
	for _, o := range objects {
		if strings.TrimSpace(o.Name) == "" {
			return fmt.Errorf("object with empty name")
		}
		names[strings.ToLower(strings.TrimSpace(o.Name))] = true
	}
	for _, o := range objects {
		switch strings.ToLower(strings.TrimSpace(o.Kind)) {
		case "host", "subnet", "range", "fqdn":
			if len(o.Addresses) == 0 {
				return fmt.Errorf("object %q (%s) has no addresses", o.Name, o.Kind)
			}
		case "group":
			if len(o.Members) == 0 {
				return fmt.Errorf("group object %q has no members", o.Name)
			}
			for _, m := range o.Members {
				if !names[strings.ToLower(strings.TrimSpace(m))] {
					return fmt.Errorf("group object %q references unknown member %q", o.Name, m)
				}
			}
		default:
			return fmt.Errorf("object %q has unknown kind %q (want host|subnet|range|fqdn|group)", o.Name, o.Kind)
		}
	}
	for _, s := range services {
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("service with empty name")
		}
		if len(s.Ports) == 0 {
			return fmt.Errorf("service %q has no ports", s.Name)
		}
		for _, p := range s.Ports {
			if p.PortMin < 0 || p.PortMin > 65535 || p.PortMax < 0 || p.PortMax > 65535 {
				return fmt.Errorf("service %q has a port out of range", s.Name)
			}
		}
	}
	return nil
}

// Store wraps a Config behind an atomic pointer for lock-free hot reload.
type Store struct{ p atomic.Pointer[Config] }

// NewStore seeds the store with an initial config.
func NewStore(c *Config) *Store {
	s := &Store{}
	s.p.Store(c)
	return s
}

// Get returns the current config snapshot. Callers must treat it as immutable.
func (s *Store) Get() *Config { return s.p.Load() }

// Swap installs a new, already-validated config and returns the previous one.
func (s *Store) Swap(c *Config) *Config { return s.p.Swap(c) }

// dir returns the directory of the config path; useful for sibling state files.
func (c *Config) dir() string { return filepath.Dir(c.path) }

// RAConfig is IPv6 router advertisement: this node telling hosts on a LAN
// that it is a router they can use, and optionally which DNS servers and
// search domains to use with it.
//
// Scoped deliberately to advertising a directly-attached prefix. It does not
// republish prefixes learned over the mesh or via BGP: radvd's model is that
// a prefix it advertises is one this router owns on that link, and a prefix
// that comes and goes with routing state does not fit it — hosts cache what
// they are told for the prefix lifetime and a withdrawal cannot reach them
// faster than that.
type RAConfig struct {
	Enabled    bool          `json:"enabled,omitempty"`
	Interfaces []RAInterface `json:"interfaces,omitempty"`
}

// RAInterface is one link to advertise on.
type RAInterface struct {
	// Iface is the host interface name — the LAN side, never the mesh
	// device. Advertising into the overlay would tell peers this node is
	// their router, which is never what an operator means here.
	Iface string `json:"iface"`
	// Prefixes are the on-link prefixes to advertise. Empty means "every
	// global IPv6 prefix currently configured on Iface", resolved at render
	// time, which is what an operator setting this up on a LAN almost always
	// wants and saves restating an address the interface already carries.
	Prefixes []string `json:"prefixes,omitempty"`
	// Managed and OtherConfig set the M and O flags. Managed says addresses
	// come from DHCPv6 rather than SLAAC; OtherConfig says other information
	// (DNS, NTP) does, while addresses still come from SLAAC. Both default
	// off, which is plain SLAAC with everything carried in the RA itself.
	Managed     bool `json:"managed,omitempty"`
	OtherConfig bool `json:"other_config,omitempty"`
	// DefaultLifetime is how long (seconds) a host should treat this node as
	// a default router. 0 means the daemon's own default; a deliberate 0 to
	// advertise "not a default router" is spelled NotDefault instead, so the
	// unset and the meaningful zero stay distinguishable.
	DefaultLifetime int  `json:"default_lifetime,omitempty"`
	NotDefault      bool `json:"not_default,omitempty"`
	// DNS and Search are RDNSS and DNSSL: recursive DNS servers and search
	// domains handed to hosts in the RA itself (RFC 8106), which is how a
	// SLAAC-only network gets DNS without DHCPv6. DNS entries must be IPv6
	// addresses — RDNSS has no form for an IPv4 server.
	DNS    []string `json:"dns,omitempty"`
	Search []string `json:"search,omitempty"`
	// Preference is the RFC 4191 Default Router Preference — "low", "medium"
	// or "high", empty meaning unset (radvd's own default, which is medium).
	// It lets a host choose between several routers advertising on the same
	// link: a backup router set low is used only while the high one is
	// silent, without the two fighting over who is default.
	//
	// Only meaningful on a router that is advertising itself as one at all.
	// A preference alongside NotDefault is inert, because a host that has
	// been told this node is not a default router has nothing to rank.
	Preference string `json:"preference,omitempty"`
	// Disabled parks an entry without deleting it, following the same
	// convention as every other table.
	Disabled bool `json:"disabled,omitempty"`
}

// RAPreferences are the accepted Preference values, in the order a picker
// should show them.
var RAPreferences = []string{"low", "medium", "high"}

// EnabledInterfaces returns the RA interfaces actually in service.
func (c RAConfig) EnabledInterfaces() []RAInterface {
	if !c.Enabled {
		return nil
	}
	var out []RAInterface
	for _, i := range c.Interfaces {
		if !i.Disabled && strings.TrimSpace(i.Iface) != "" {
			out = append(out, i)
		}
	}
	return out
}

// Validate checks an RA interface entry, returning the first problem.
func (i RAInterface) Validate() error {
	if strings.TrimSpace(i.Iface) == "" {
		return fmt.Errorf("interface name is required")
	}
	for _, p := range i.Prefixes {
		pfx, err := netip.ParsePrefix(strings.TrimSpace(p))
		if err != nil {
			return fmt.Errorf("prefix %q: %v", p, err)
		}
		if !pfx.Addr().Is6() {
			return fmt.Errorf("prefix %q: router advertisements are IPv6 only", p)
		}
		// SLAAC derives an address by appending a 64-bit interface
		// identifier, so a prefix that isn't a /64 cannot be autoconfigured
		// from. Hosts silently ignore it, which looks exactly like the RA
		// not working at all.
		if pfx.Bits() != 64 {
			return fmt.Errorf("prefix %q: must be a /64 — SLAAC cannot autoconfigure from any other length", p)
		}
	}
	for _, d := range i.DNS {
		a, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil {
			return fmt.Errorf("dns %q: %v", d, err)
		}
		if !a.Is6() {
			return fmt.Errorf("dns %q: RDNSS carries IPv6 addresses only", d)
		}
	}
	if p := strings.ToLower(strings.TrimSpace(i.Preference)); p != "" {
		ok := false
		for _, v := range RAPreferences {
			if p == v {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("preference %q: must be low, medium or high", i.Preference)
		}
	}
	if i.DefaultLifetime < 0 || i.DefaultLifetime > 9000 {
		return fmt.Errorf("default lifetime %d: must be between 0 and 9000 seconds", i.DefaultLifetime)
	}
	return nil
}

// HostIface is the addressing gravinet maintains for one host interface.
type HostIface struct {
	Iface string `json:"iface"`
	// Mode4 and Mode6 are where each family's addresses come from: static or
	// dhcp for IPv4, static, dhcp6 or slaac for IPv6. See hostnet.Mode.
	//
	// Independent per family, because a static IPv4 address alongside SLAAC
	// IPv6 on one interface is an ordinary deployment rather than a corner
	// case, and every backend expresses the two separately.
	//
	// Empty reads as static and is omitted from the encoding. Both matter for
	// compatibility: a record written before this release exists only because
	// an operator set a static address, so absent has to mean static, and
	// omitting it is what keeps an untouched config serializing byte for byte
	// as it did.
	Mode4 hostnet.Mode `json:"mode4,omitempty"`
	Mode6 hostnet.Mode `json:"mode6,omitempty"`
	// Addrs are the interface's global addresses in CIDR form. Empty means
	// "no static addresses", which is a thing an operator can mean, and is
	// applied as such rather than read as "leave alone".
	//
	// Only ever addresses of a family in static mode. A leased or
	// autoconfigured address recorded here would be reapplied as a static one
	// at the next reload, pinning an interface to whatever it happened to hold
	// when someone edited its MTU.
	Addrs []string `json:"addrs,omitempty"`
	// GW4/GW6 are optional default gateways. Empty means "do not set one",
	// never "remove the existing one" — a default route belongs to the host
	// rather than to this interface.
	GW4 string `json:"gw4,omitempty"`
	GW6 string `json:"gw6,omitempty"`
	// MTU is the interface MTU. 0 means gravinet does not manage it, which
	// is the default and is different from "set it to zero": an MTU is
	// normally decided by the driver or the link, and an operator who has
	// not touched the field has not asked gravinet to take it over.
	MTU int `json:"mtu,omitempty"`
}

// HostVLAN is one 802.1Q tagged interface gravinet creates on this host.
type HostVLAN struct {
	// Parent is the interface the tagged traffic rides on. A physical NIC or
	// a bond, never a mesh device: the overlay carries gravinet's own
	// encapsulation and a VLAN header inside it addresses nothing.
	Parent string `json:"parent"`
	// ID is the 802.1Q VLAN identifier, 1-4094. 0 and 4095 are reserved by
	// the standard, and 1 is the default VLAN on most switches — allowed,
	// because a trunk that tags VLAN 1 is a real configuration, but it is
	// the one an operator most often means to have typed differently.
	ID int `json:"id"`
	// Name is the device name, and is not set by the interfaces page: a
	// tagged interface is named for the parent it rides on and the tag it
	// carries, parent.id, which is what VLANName returns for an empty field
	// and what an operator reading `ip link` expects to find. There is
	// nothing for a name box to decide that the two fields above have not
	// already decided.
	//
	// The field stays, and stays honoured, for two cases that have no other
	// answer. A configuration written before names were derived carries one,
	// and its device is referenced by name from the addressing records, the
	// DHCP relay links and the firewall — re-deriving it would rename a live
	// interface out from under all of them. And parent.id does not always
	// fit: IFNAMSIZ leaves 15 characters, which a predictable name like
	// enp0s20f0u3u1 exhausts before the tag is appended. Such a host can set
	// this by hand and is the reason Validate's length message points at the
	// configuration file rather than at a field on the page.
	Name string `json:"name,omitempty"`
	// Disabled parks the definition without deleting it. A disabled VLAN is
	// not created, and is torn down if it currently exists, which is the
	// same convention every other table in this configuration uses.
	Disabled bool `json:"disabled,omitempty"`
}

// VLANName is the device name this definition creates.
func (v HostVLAN) VLANName() string {
	if n := strings.TrimSpace(v.Name); n != "" {
		return n
	}
	return fmt.Sprintf("%s.%d", strings.TrimSpace(v.Parent), v.ID)
}

// Validate checks one tagged interface definition.
func (v HostVLAN) Validate() error {
	parent := strings.TrimSpace(v.Parent)
	if parent == "" {
		return fmt.Errorf("parent interface is required")
	}
	if v.ID < 1 || v.ID > 4094 {
		return fmt.Errorf("vlan id %d: must be between 1 and 4094 (0 and 4095 are reserved)", v.ID)
	}
	name := v.VLANName()
	if name == parent {
		return fmt.Errorf("vlan %s: cannot have the same name as its parent", name)
	}
	// IFNAMSIZ is 16 including the terminator, so 15 usable. A longer name is
	// refused by the kernel at creation time; refusing it on save means the
	// operator finds out while the field is still in front of them.
	//
	// The name is derived, so there is no box on the page to shorten — the
	// parent's own name is what does not fit. The message says where the
	// override lives rather than leaving a host with a long predictable
	// interface name unable to carry a tag at all.
	if len(name) > 15 {
		if strings.TrimSpace(v.Name) == "" {
			return fmt.Errorf("%s.%d is %d characters and the kernel allows 15: %s is too long a parent for a derived name, so set \"name\" for this vlan in the configuration file", parent, v.ID, len(name), parent)
		}
		return fmt.Errorf("vlan name %q is %d characters: the kernel allows 15", name, len(name))
	}
	// The characters a device name may not contain. A name with a slash or a
	// space in it is refused by the kernel, and one with a colon collides
	// with the alias syntax older tools use for secondary addresses.
	if strings.ContainsAny(name, " /:\t\n") {
		return fmt.Errorf("vlan name %q: must not contain spaces, slashes or colons", name)
	}
	return nil
}

// ValidateHostVLANs checks the tagged interfaces as a set. The collisions are
// the interesting part: two definitions producing one device name, or two
// producing the same tag on the same parent, are both configurations the
// kernel would accept one of and silently drop the other.
//
// Disabled entries are checked too, for the same reason the DHCP relay links
// are: a definition broken during an edit and then parked saves cleanly and
// fails at the moment somebody re-enables it, which is the moment they least
// want to be reading a validation error.
func (c *Config) ValidateHostVLANs() error {
	names := map[string]bool{}
	tags := map[string]bool{}
	for _, v := range c.HostVLANs {
		if err := v.Validate(); err != nil {
			return err
		}
		n := strings.ToLower(v.VLANName())
		if names[n] {
			return fmt.Errorf("two tagged interfaces are both named %s", v.VLANName())
		}
		names[n] = true
		k := fmt.Sprintf("%s|%d", strings.ToLower(strings.TrimSpace(v.Parent)), v.ID)
		if tags[k] {
			return fmt.Errorf("%s already has a tagged interface for vlan %d", v.Parent, v.ID)
		}
		tags[k] = true
	}
	// A VLAN whose parent is itself a VLAN this node defines is refused. The
	// kernel permits stacked (QinQ) tagging, but nothing else here models the
	// outer tag, so the result would come back from a restart in an order
	// that may or may not have the parent yet.
	for _, v := range c.HostVLANs {
		for _, p := range c.HostVLANs {
			if strings.EqualFold(strings.TrimSpace(v.Parent), p.VLANName()) {
				return fmt.Errorf("vlan %s is stacked on %s, which is itself a tagged interface — that is not supported here", v.VLANName(), v.Parent)
			}
		}
	}
	return nil
}

// Validate checks a host interface entry.
func (h HostIface) Validate() error {
	if strings.TrimSpace(h.Iface) == "" {
		return fmt.Errorf("interface name is required")
	}
	if err := hostnet.ValidMode4(h.Mode4); err != nil {
		return err
	}
	if err := hostnet.ValidMode6(h.Mode6); err != nil {
		return err
	}
	for _, a := range h.Addrs {
		p, err := netip.ParsePrefix(strings.TrimSpace(a))
		if err != nil {
			return fmt.Errorf("address %q: needs a prefix length (e.g. 10.1.1.1/24): %v", a, err)
		}
		if p.Addr().IsLinkLocalUnicast() || p.Addr().IsLoopback() {
			return fmt.Errorf("address %q: link-local and loopback addresses are managed by the kernel", a)
		}
		// A static address under a non-static mode would never be applied by
		// anything. Refused rather than dropped on the way through: silently
		// discarding an address someone typed is how a page comes to disagree
		// with the interface it describes.
		mode, fam := h.Mode4, "IPv4"
		if p.Addr().Is6() {
			mode, fam = h.Mode6, "IPv6"
		}
		if !mode.IsStatic() {
			return fmt.Errorf("address %q cannot be set while %s is in %s mode: the address will come from the network", a, fam, string(mode))
		}
	}
	// 576 is the IPv4 minimum a host must accept; 9216 covers jumbo frames
	// with room for the tag overhead some drivers count separately. Outside
	// that the kernel would refuse it anyway, but with an errno rather than
	// an explanation.
	if h.MTU != 0 && (h.MTU < 576 || h.MTU > 9216) {
		return fmt.Errorf("mtu %d: must be between 576 and 9216, or 0 to leave it unmanaged", h.MTU)
	}
	for _, g := range []struct {
		v, name, fam string
		mode         hostnet.Mode
	}{
		{h.GW4, "gw4", "IPv4", h.Mode4},
		{h.GW6, "gw6", "IPv6", h.Mode6},
	} {
		if strings.TrimSpace(g.v) == "" {
			continue
		}
		a, err := netip.ParseAddr(strings.TrimSpace(g.v))
		if err != nil {
			return fmt.Errorf("%s %q: %v", g.name, g.v, err)
		}
		if (g.name == "gw4") != a.Is4() {
			return fmt.Errorf("%s %q is the wrong address family", g.name, g.v)
		}
		// A default route arrives with the lease under DHCP and in the router
		// advertisement under DHCPv6 or SLAAC — a DHCPv6 server does not supply
		// one, which is why both v6 modes accept RAs. A gateway configured
		// alongside would be a second default route competing with the one the
		// network just handed over, and which wins is not something an operator
		// can reason about from this page.
		if !g.mode.IsStatic() {
			return fmt.Errorf("%s cannot be set while %s is in %s mode: the default route comes with the address", g.name, g.fam, string(g.mode))
		}
	}
	return nil
}

// HostIfaceFor returns the managed entry for an interface, or nil.
func (c *Config) HostIfaceFor(name string) *HostIface {
	for i := range c.HostInterfaces {
		if c.HostInterfaces[i].Iface == name {
			return &c.HostInterfaces[i]
		}
	}
	return nil
}

// SetHostIface records (or replaces) the addressing gravinet maintains for an
// interface, so it travels with the configuration.
func (c *Config) SetHostIface(h HostIface) error {
	if err := h.Validate(); err != nil {
		return err
	}
	h.Iface = strings.TrimSpace(h.Iface)
	if cur := c.HostIfaceFor(h.Iface); cur != nil {
		// A gateway is only replaced when one was given: editing addresses
		// must not silently drop the default route.
		if h.GW4 == "" {
			h.GW4 = cur.GW4
		}
		if h.GW6 == "" {
			h.GW6 = cur.GW6
		}
		if h.MTU == 0 {
			h.MTU = cur.MTU
		}
		// An unset mode here means "this update did not mention it", not
		// "static" — the same as the fields above, and the opposite of what an
		// unset mode means when the record is *read*. The distinction is the
		// difference between a stored record whose mode predates modes and an
		// incoming update that only touched the MTU, and collapsing the two
		// would turn every MTU edit into a switch back to static.
		if h.Mode4 == "" {
			h.Mode4 = cur.Mode4
		}
		if h.Mode6 == "" {
			h.Mode6 = cur.Mode6
		}
		*cur = h
		return nil
	}
	c.HostInterfaces = append(c.HostInterfaces, h)
	return nil
}

// HostSettings is host configuration gravinet maintains outside the mesh.
//
// Each group is separately opt-in, and the zero value of each means "gravinet
// does not manage this". That distinction is the whole design: a reconciler
// that treated an empty field as "set it to empty" would wipe a host's DNS on
// the first reload after an upgrade.
type HostSettings struct {
	Syslog *HostSyslog `json:"syslog,omitempty"`
	// Users are the console accounts gravinet has been asked to maintain.
	//
	// Names and expiry only — never passwords or hashes. A configuration
	// backup is downloaded through a browser and mailed around, and password
	// material has no business travelling in it. A restored node therefore
	// comes back with its accounts present but locked, and someone sets a
	// password once from the console.
	//
	// nil means gravinet manages no accounts here; an empty slice means it
	// manages the set and the set is empty. Neither ever deletes anything —
	// see service.EnsureSystemUser.
	Users    []HostUser    `json:"users,omitempty"`
	Time     *HostTime     `json:"time,omitempty"`
	Resolver *HostResolver `json:"resolver,omitempty"`
}

// HostSyslogTarget is one remote syslog collector.
type HostSyslogTarget struct {
	Host     string `json:"host"`
	Port     int    `json:"port,omitempty"`
	Proto    string `json:"proto,omitempty"` // "udp" or "tcp"
	Disabled bool   `json:"disabled,omitempty"`
}

// HostSyslog is the forwarding configuration as a whole. An empty Targets
// list is meaningful — it means "forward nowhere" — which is why the
// containing pointer, not the slice, is what says whether gravinet manages
// this at all.
type HostSyslog struct {
	Targets []HostSyslogTarget `json:"targets"`
}

// HostTime is the host clock configuration. The clock itself is deliberately
// not stored: restoring a backup must not set the system time to whenever the
// backup was taken, which would be worse than any problem it solved.
type HostTime struct {
	Timezone   string   `json:"timezone,omitempty"`
	NTPEnabled bool     `json:"ntp_enabled"`
	NTPServers []string `json:"ntp_servers,omitempty"`
}

// HostResolver is the host's name configuration.
//
// Hostname is separate from Config.Hostname, which is the name advertised to
// peers. This one is the operating system's, and restoring it onto different
// hardware is usually what an operator wants — it is the node's identity in
// logs, certificates and everything else that is not the mesh.
type HostResolver struct {
	Hostname     string   `json:"hostname,omitempty"`
	DNSServers   []string `json:"dns_servers,omitempty"`
	SearchDomain string   `json:"search_domain,omitempty"`
}

// HostUser is one console account gravinet recreates on restore. Deliberately
// carries no credential of any kind.
type HostUser struct {
	Name string `json:"name"`
	// ExpiresUnix is the account expiry, 0 for none. Worth carrying because
	// an account restored without its expiry is a temporary account that has
	// quietly become permanent.
	ExpiresUnix int64 `json:"expires_unix,omitempty"`
}

// Validate checks the host settings, so a bad value fails on save rather than
// on the next reload of some unrelated change.
func (h HostSettings) Validate() error {
	if h.Syslog != nil {
		for _, t := range h.Syslog.Targets {
			if strings.TrimSpace(t.Host) == "" {
				return fmt.Errorf("syslog target: host is required")
			}
			if t.Port < 0 || t.Port > 65535 {
				return fmt.Errorf("syslog target %s: port %d out of range", t.Host, t.Port)
			}
			if p := strings.ToLower(t.Proto); p != "" && p != "udp" && p != "tcp" {
				return fmt.Errorf("syslog target %s: proto %q must be udp or tcp", t.Host, t.Proto)
			}
		}
	}
	for _, u := range h.Users {
		if strings.TrimSpace(u.Name) == "" {
			return fmt.Errorf("console account: name is required")
		}
	}
	if h.Resolver != nil {
		for _, d := range h.Resolver.DNSServers {
			if _, err := netip.ParseAddr(strings.TrimSpace(d)); err != nil {
				return fmt.Errorf("dns server %q: %v", d, err)
			}
		}
	}
	return nil
}

// --- Dynamic DNS registration ----------------------------------------------

// DDNSConfig is this node registering its own name in DNS.
//
// A host that takes its address from DHCP is registered by whatever hands out
// the lease. A gateway is not: its addresses are static, so nothing on the
// network announces them, and its name resolves only if somebody typed it into
// a zone by hand and remembered to change it afterwards. This is the node doing
// it for itself, which is also the only vantage point that knows what addresses
// it currently has.
//
// There is no enable flag, deliberately. What this needs — a hostname, a search
// domain, and somewhere to send the update — is exactly what System > Resolver
// already holds, and a fourth switch that can disagree with those three is a
// way to have the feature configured and silently off. The interval is the
// switch: zero means never, which is the default.
type DDNSConfig struct {
	// IntervalMinutes is how often to re-register. 0 is off; Default() sets
	// DefaultDDNSInterval.
	//
	// A period rather than an event because the failure modes are all silent:
	// a server that was down when this node booted, a zone that was created
	// afterwards, a record somebody deleted by hand. Re-asserting on a timer
	// converges from every one of those without anything having to notice.
	// Re-asserting costs nothing when nothing changed — the run reads both
	// the forward and the reverse record first and writes only on a difference.
	//
	// No omitempty, and that is load-bearing rather than a style choice. The
	// default is non-zero, and Load starts from Default() before unmarshalling
	// over it — so a 0 omitted from the file would come back as 15 on the
	// next read, and switching registration off would silently undo itself at
	// the next restart. Written out, 0 stays 0.
	IntervalMinutes int `json:"interval_minutes"`

	// TTL is the record lifetime in seconds, and 0 means zero: resolvers are
	// told not to cache the record at all.
	//
	// That is a real answer rather than a missing one, which is why it is not
	// spent as a stand-in for "unset". Default() sets DefaultDDNSTTL, and the
	// field is written out unconditionally for the same reason
	// IntervalMinutes is \u2014 Load starts from Default(), so a 0 dropped by
	// omitempty would come back as 900 and an operator who asked for an
	// uncached record would silently get a fifteen-minute one.
	TTL int `json:"ttl"`

	// TSIGKey signs the updates. Empty sends them unsigned, which is a real
	// configuration rather than a broken one: a zone can equally be set to
	// accept updates from a list of addresses, and on a private network that
	// is a choice an operator has already made in their DNS server.
	//
	// Either a path to a BIND-style key file, or the inline "name:base64secret"
	// or "name:base64secret:algorithm" form. A path is preferred where there is
	// a choice, because it keeps the secret out of this file — which is
	// snapshotted into the config history and exported in support bundles. Both
	// are redacted on the way out (the field name carries "key", which is what
	// the redactor matches), but a path is not a secret at all.
	TSIGKey string `json:"tsig_key,omitempty"`

	// Reverse also publishes a PTR for the primary name. On by default —
	// this is a pointer rather than a bool so an operator can turn it off and
	// have that survive, which a plain false could not be told from unset.
	Reverse *bool `json:"reverse,omitempty"`

	// Mesh publishes this node's overlay addresses too, under the same
	// per-interface alias scheme as any other interface.
	//
	// On by default. The argument for excluding them was that an overlay
	// address is reachable only by mesh peers, who already resolve each other
	// through the hosts-file sync, so publishing one into LAN DNS answers
	// queries from hosts that cannot use the answer.
	//
	// That is true and it is not a reason to withhold the record. A DNS zone is
	// not a promise of reachability — half the addresses in any private zone
	// are unreachable from somewhere — and "this name resolves but you cannot
	// route to it" is an ordinary thing for an operator to work with, where "no
	// record exists and nothing says why" is not. The hosts-file sync serves
	// gravinet's own peers; it does nothing for a monitoring box, a jump host,
	// a script, or anyone holding a terminal, and those are the callers that
	// need a name to resolve. Publishing every interface this node has is also
	// simply the least surprising thing for a feature whose entire job is
	// publishing this node's addresses.
	//
	// Set it false to get the old exclusion back. A pointer for the same reason
	// Reverse is: so an explicit choice survives a later change to the default.
	Mesh *bool `json:"mesh,omitempty"`
}

// DefaultDDNSInterval is how often a node re-registers when nothing says
// otherwise. Fifteen minutes: short enough that a gateway which came up with a
// new address is findable by name within one coffee, long enough that the
// steady-state cost — two queries per name, no writes — stays invisible on a
// server also answering a LAN's worth of ordinary traffic.
const DefaultDDNSInterval = 15

// DefaultDDNSTTL is the lifetime a published record gets when nothing says
// otherwise, in seconds. Fifteen minutes, matching the default registration
// interval: a record is re-asserted about as often as it expires, so a resolver
// that cached the old address during a renumber holds it for roughly one cycle
// rather than indefinitely.
const DefaultDDNSTTL = 900

// Active reports whether this node should be registering itself.
func (d DDNSConfig) Active() bool { return d.IntervalMinutes > 0 }

// Interval is the configured period as a duration.
func (d DDNSConfig) Interval() time.Duration {
	return time.Duration(d.IntervalMinutes) * time.Minute
}

// ReverseEnabled reports whether PTRs are published, defaulting to yes.
func (d DDNSConfig) ReverseEnabled() bool { return d.Reverse == nil || *d.Reverse }

// MeshEnabled reports whether overlay addresses are published, defaulting to
// yes. See the Mesh field.
func (d DDNSConfig) MeshEnabled() bool { return d.Mesh == nil || *d.Mesh }

// Validate checks the block. The TSIG key is parsed rather than pattern-matched
// so a secret that is not valid base64, or an algorithm nothing implements, is
// refused at the moment it is typed instead of once an hour in the log.
func (d DDNSConfig) Validate() error {
	if d.IntervalMinutes < 0 || d.IntervalMinutes > 10080 {
		return fmt.Errorf("interval %d: must be between 0 (off) and 10080 minutes (a week)", d.IntervalMinutes)
	}
	if d.TTL < 0 || d.TTL > 604800 {
		return fmt.Errorf("ttl %d: must be between 0 (do not cache) and 604800 seconds (a week)", d.TTL)
	}
	return nil
}

// --- DHCP ------------------------------------------------------------------

// DHCPMode is what this node does about DHCP on its LANs.
//
// Two values now: off, and relay. gravinet served leases of its own through
// Kea until v988, and this field is what made that role exclusive with this
// one — a node could not both hand out addresses and forward somebody else's
// requests, because the two would shadow each other on any link they shared
// and clients would take whichever reply raced first.
//
// With one role left the exclusion has nothing to exclude, and this could have
// become a bool. It stays a mode for two reasons. The value on disk is
// unchanged, so a node that was relaying before the upgrade is still relaying
// after it, with no migration touching the half that still works. And it is
// still a real control: parking every link one at a time is not the same
// gesture as switching the relay off, and the card's pill needs something to
// write.
type DHCPMode string

const (
	// DHCPOff is the default: gravinet runs no relay and touches nothing. A
	// host already dealing with DHCP its own way is left alone until an
	// operator opts in.
	DHCPOff DHCPMode = ""
	// DHCPRelay forwards client traffic to upstream servers. Implemented in
	// gravinet itself (internal/dhcrelay) rather than by a daemon: a relay is
	// a few hundred lines of well-specified forwarding, and every packaged
	// one is either end-of-life or a second copy of a daemon this node is
	// already running for something else.
	DHCPRelay DHCPMode = "relay"
)

// dhcpModeRetiredServer is what v987 and earlier wrote for a node serving its
// own leases through Kea. Not a mode anyone can select any more; named here
// only so migrateServerMode can recognise one on disk.
//
// Recognising it is not optional. ValidDHCPMode refuses an unknown mode and
// Config.Validate runs on every Load, so leaving "server" unhandled would not
// retire the feature — it would stop the daemon starting at all, on precisely
// the nodes this release affects most.
const dhcpModeRetiredServer DHCPMode = "server"

// DHCPConfig is this node's DHCP configuration. Node-global, like BGP and the
// router advertisements.
type DHCPConfig struct {
	// Mode selects relay, or neither. See DHCPMode.
	Mode DHCPMode `json:"mode,omitempty"`
	// Relay is the forwarding configuration. Kept while the mode is off
	// rather than cleared, so switching the relay off for an afternoon does
	// not cost an operator the addresses they typed in.
	Relay DHCPRelayConfig `json:"relay,omitempty"`

	// retiredServer records that this configuration arrived with v987's
	// server mode set, so the daemon can say so once at startup.
	//
	// Unexported, and therefore never marshalled: it is a fact about the file
	// that was read, not a setting, and it stops being true the moment the
	// configuration is written back out.
	retiredServer bool
}

// RetiredServerMode reports whether this configuration was loaded from a file
// that had this node serving DHCP through Kea, a role removed in v988.
//
// Worth saying out loud at startup rather than passing over in silence,
// because removing the code does not stop the server. gravinet enabled the Kea
// unit when an operator saved a subnet, and nothing here disables it now: the
// code that could is gone, and a release that reached out to stop a daemon
// during an upgrade would take a working LAN down for people who never asked
// for that. So Kea keeps serving from the file gravinet last wrote it, while
// the page it was configured from no longer exists — which is a state an
// operator has to be told about before they can act on it.
func (c DHCPConfig) RetiredServerMode() bool { return c.retiredServer }

// DHCPRelayConfig is the relay configuration.
//
// A list of links rather than one global setting, from v949. The relay was
// always one socket per interface — the address bound on each is the giaddr
// stamped on what it forwards — so the only thing shared between two links was
// that they happened to be typed into the same form. Splitting them lets a
// node relay one LAN to one server and another LAN somewhere else, and gives
// each link the enable/disable that every other table in the UI has.
type DHCPRelayConfig struct {
	// Links are the client-facing links to relay from, one entry each. A
	// relay has to be told which links are downstream: listening everywhere
	// would relay the upstream server's own replies back at it.
	Links []DHCPRelayLink `json:"links,omitempty"`

	// The pre-v949 shape: one interface list sharing one server list and one
	// hop limit. Retained as fields so an existing config still parses, folded
	// into Links by Config.Validate, and cleared there so they are never
	// written back out. Same treatment SNMP's Community got in v736.
	LegacyInterfaces []string `json:"interfaces,omitempty"`
	LegacyServers    []string `json:"servers,omitempty"`
	LegacyMaxHops    int      `json:"max_hops,omitempty"`
}

// DHCPRelayLink is one client-facing link and where it forwards to.
type DHCPRelayLink struct {
	// Iface is the client-facing link to listen on.
	Iface string `json:"iface"`
	// Servers are the upstream DHCP servers to forward to. More than one is
	// allowed and each gets a copy, which is how a relay does redundancy —
	// there is no failover to sequence, the client takes whichever answer
	// arrives and the rest are ignored.
	Servers []string `json:"servers,omitempty"`
	// MaxHops bounds the relay hop count before a packet is dropped, per RFC
	// 1542 §4.1.1. 0 means the default of 4.
	MaxHops int `json:"max_hops,omitempty"`
	// Disabled parks a link without deleting it, the same convention every
	// other table here uses.
	Disabled bool `json:"disabled,omitempty"`
}

// EnabledLinks returns the relay links actually in service: not parked, naming
// an interface, and having somewhere to forward to. Empty unless the mode is
// relay, so every caller gets the off switch for free rather than each having
// to remember to check the mode first.
//
// A link with no server is dropped here rather than rejected on save. It is
// half-written, not wrong: the row exists so the operator can fill the rest in,
// and refusing to store it would mean losing the interface they just chose.
func (c DHCPConfig) EnabledLinks() []DHCPRelayLink {
	if c.Mode != DHCPRelay {
		return nil
	}
	var out []DHCPRelayLink
	for _, l := range c.Relay.Links {
		if !l.Disabled && strings.TrimSpace(l.Iface) != "" && len(trimStrings(l.Servers)) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// RelayActive reports whether the relay should be running. Same shape as
// EnabledLinks and for the same reason.
func (c DHCPConfig) RelayActive() bool {
	return len(c.EnabledLinks()) > 0
}

// ValidDHCPMode checks a mode string.
//
// The retired server value gets its own sentence rather than falling into the
// "unknown mode" case. By the time this is reached, a configuration read from
// disk has already been through migrateServerMode, so a "server" still
// arriving here is one typed at the CLI or posted to the API just now — and
// telling that operator their value is unrecognised, when it was the
// documented answer one release ago, explains nothing about what happened to
// it.
func ValidDHCPMode(m DHCPMode) error {
	switch m {
	case DHCPOff, DHCPRelay:
		return nil
	case dhcpModeRetiredServer:
		return fmt.Errorf("gravinet no longer serves DHCP itself (removed in v988): this node can relay to a server elsewhere, or leave DHCP off")
	}
	return fmt.Errorf("unknown DHCP mode %q: want relay, or empty for off", string(m))
}

// Validate checks the whole DHCP configuration, including the links not
// currently in service.
//
// Every link is checked whether the relay is on or off, and parked ones are
// checked too. The alternative — validating only what is running — means a
// link left broken by an edit made while the relay was off saves cleanly and
// fails at the moment somebody switches it on, which is the moment they least
// want to be debugging an address.
func (c DHCPConfig) Validate() error {
	if err := ValidDHCPMode(c.Mode); err != nil {
		return err
	}
	return c.Relay.Validate()
}

// Validate checks the relay links as a set.
func (r DHCPRelayConfig) Validate() error {
	seen := map[string]bool{}
	for _, l := range r.Links {
		if err := l.Validate(); err != nil {
			return err
		}
		// Two rows for one interface is not two relays, it is two answers to
		// the same question: one socket can only be bound once, and the second
		// row would be silently ignored at bind time.
		k := strings.ToLower(strings.TrimSpace(l.Iface))
		if k == "" {
			continue
		}
		if seen[k] {
			return fmt.Errorf("interface %s has more than one relay entry", l.Iface)
		}
		seen[k] = true
	}
	return nil
}

// Validate checks one relay link.
func (l DHCPRelayLink) Validate() error {
	for _, s := range l.Servers {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil || !a.Is4() {
			return fmt.Errorf("relay server %q: must be an IPv4 address", s)
		}
		// A relay forwards to a unicast address. Broadcast would be relayed
		// straight back onto a link, and the loop is only bounded by the hop
		// count.
		if a.IsMulticast() || a.IsUnspecified() || a == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
			return fmt.Errorf("relay server %q: must be a unicast address", s)
		}
	}
	if l.MaxHops < 0 || l.MaxHops > 16 {
		return fmt.Errorf("max hops %d: must be between 0 and 16, or 0 for the default of 4", l.MaxHops)
	}
	return nil
}

// migrateServerMode retires v987's server role.
//
// The mode becomes off rather than relay, and that is the whole of the care
// needed here. A node that served its own leases has relay links only if
// somebody configured them and then switched away, so turning those on during
// an upgrade would start forwarding this LAN's requests to whatever address
// was typed in months ago. Off is the one answer that cannot surprise
// anybody, and the relay is one pill away for an operator who wants it.
//
// The served subnets go with it, quietly, because there is no longer a field
// for them to land in: encoding/json drops a key with no destination, so they
// are gone on read and gone from the file at the next save. They are not lost
// — the config history and any backup predating the upgrade still hold them,
// which is where to look for a pool worth recreating on whatever serves that
// LAN next. Keeping a dead field on the struct to carry them would mean
// shipping a shape nothing reads, forever, so the file can go on describing a
// feature the binary does not have.
func (c *DHCPConfig) migrateServerMode() {
	if c.Mode == dhcpModeRetiredServer {
		c.Mode, c.retiredServer = DHCPOff, true
	}
}

// migrateRelay folds the pre-v949 relay shape — one interface list sharing one
// server list and one hop limit — into the per-link form, and clears the old
// fields so they are never written back out.
//
// One link per interface, each carrying a copy of what used to be shared, so a
// node that upgrades relays exactly what it relayed before. A config that
// already has Links is left alone rather than second-guessed, the rule every
// other migration here follows.
//
// Legacy servers with no legacy interfaces are dropped, which loses nothing
// that was running: the old RelayActive required both, so that combination was
// a relay that had never started.
func (r *DHCPRelayConfig) migrateRelay() {
	if len(r.Links) == 0 {
		for _, n := range trimStrings(r.LegacyInterfaces) {
			r.Links = append(r.Links, DHCPRelayLink{
				Iface:   n,
				Servers: trimStrings(r.LegacyServers),
				MaxHops: r.LegacyMaxHops,
			})
		}
	}
	r.LegacyInterfaces, r.LegacyServers, r.LegacyMaxHops = nil, nil, 0
}

// trimStrings drops surrounding whitespace and empty entries from a list.
func trimStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// migrateNAT hoists the pre-v953 per-network NAT into the node-global one.
//
// Each network's rules move up with Scope set to that network, which is
// exactly what they already meant: a rule filed under a network was enforced
// in the kernel (where the network was never referenced at all) and in that
// network's overlay table. Scope preserves the second half; the first was
// always node-global in effect.
//
// Two details keep behaviour identical rather than merely similar:
//
//   - A network whose NAT was switched off contributes its rules disabled.
//     The old per-network Enabled flag gated a whole network's rules, and
//     there is no per-network gate to hold that any more, so it is folded into
//     the rules themselves. A node comes back translating exactly what it
//     translated before; what changes is that re-enabling is now per rule.
//   - The node-global switch comes on if any network had NAT on. Off for
//     everyone means off, and the rules are disabled anyway by the line above.
//
// A config already carrying node-global rules is left alone, the rule every
// other migration here follows.
func (c *Config) migrateNAT() {
	if len(c.NAT.Rules) == 0 {
		for i := range c.Networks {
			n := &c.Networks[i]
			for _, r := range n.NAT.Rules {
				// No Scope is written: as of v966 the overlay half is
				// derived from the rule (natRuleAppliesToOverlay), and a
				// per-network rule being hoisted already names the
				// interface it was about.
				if !n.NAT.Enabled {
					r.Enabled = false
				}
				c.NAT.Rules = append(c.NAT.Rules, r)
			}
			if n.NAT.Enabled {
				c.NAT.Enabled = true
			}
		}
	}
	for i := range c.Networks {
		c.Networks[i].NAT = NAT{}
	}
}

// migrateQoS hoists the pre-v954 per-network classifier into the node-global
// one.
//
// Each network's rules move up with Scope set to that network, which is what
// they already meant: classified on that overlay's egress and nowhere else.
// The class geometry — how many classes, which is the default, any DSCP
// overrides — comes from the first network that has QoS switched on, falling
// back to the first that has any rules. It is one setting per node now, and in
// practice it was the same on every network anyway: nothing but the default 5
// classes with 3 as normal unless an operator went looking for it.
//
// A network whose QoS switch was off contributes its rules disabled, the same
// fold migrateNAT does and for the same reason: the per-network gate holding
// them off has no equivalent any more, so it moves into the rules. The node
// classifies exactly what it classified before.
//
// A config already carrying node-global rules is left alone.
func (c *Config) migrateQoS() {
	if len(c.QoS.Rules) == 0 {
		geom := -1
		for i := range c.Networks {
			n := &c.Networks[i]
			for _, r := range n.QoS.Rules {
				r.Scope = qosScopeName(n)
				if !n.QoS.Enabled {
					r.Disabled = true
				}
				c.QoS.Rules = append(c.QoS.Rules, r)
			}
			if n.QoS.Enabled {
				c.QoS.Enabled = true
				if geom < 0 {
					geom = i
				}
			}
			if geom < 0 && len(n.QoS.Rules) > 0 {
				geom = i
			}
		}
		if geom >= 0 {
			src := c.Networks[geom].QoS
			c.QoS.Classes, c.QoS.DefaultClass = src.Classes, src.DefaultClass
			c.QoS.ClassDSCP = append([]int(nil), src.ClassDSCP...)
		}
	}
	for i := range c.Networks {
		c.Networks[i].QoS = QoS{}
	}
}

// qosScopeName is the name a rule uses to reach one network: its name, or its
// id for a network that has none. Same pair natRuleInScope and the scope
// picker use.
func qosScopeName(n *Network) string {
	if strings.TrimSpace(n.Name) != "" {
		return n.Name
	}
	return n.ID
}

// autoIfaceName is the TUN device name a network gets when it has not been
// given one. It must match buildNetSpecs' auto-naming in cmd/gravinet, which
// is the code that actually creates the device; every other caller that needs
// to name a network's interface goes through IfaceForNetwork rather than
// spelling this out again.
func autoIfaceName(idx int) string { return fmt.Sprintf("mesh%d", idx) }

// IfaceForNetwork is the kernel interface name a network's tunnel runs on:
// its configured TUNName, or the auto-assigned mesh<N> for its position in
// the network list.
//
// A network not in this config (or one matched by neither id nor name) falls
// back to its own TUNName, which may be empty — the caller is asking about a
// network this node does not have, and inventing an index for it would name
// some *other* network's device.
func (c *Config) IfaceForNetwork(n Network) string {
	for i := range c.Networks {
		if c.Networks[i].ID == n.ID && c.Networks[i].ID != "" {
			return c.IfaceForNetworkAt(i)
		}
	}
	for i := range c.Networks {
		if c.Networks[i].Name == n.Name && c.Networks[i].Name != "" {
			return c.IfaceForNetworkAt(i)
		}
	}
	return strings.TrimSpace(n.TUNName)
}

// IfaceForNetworkAt names the interface of the network at index i.
func (c *Config) IfaceForNetworkAt(i int) string {
	if i < 0 || i >= len(c.Networks) {
		return ""
	}
	if name := strings.TrimSpace(c.Networks[i].TUNName); name != "" {
		return name
	}
	return autoIfaceName(i)
}

// ShapingFor returns the entry for an interface, or nil. Interface names are
// matched exactly: they are kernel identifiers, and mesh0 and Mesh0 are not
// the same device on the platforms that would let you create both.
func (c *Config) ShapingFor(iface string) *IfaceShaping {
	iface = strings.TrimSpace(iface)
	if iface == "" {
		return nil
	}
	for i := range c.Shaping {
		if c.Shaping[i].Iface == iface {
			return &c.Shaping[i]
		}
	}
	return nil
}

// ShapingThrottle is the limit applied to an interface's shaper. An interface
// with no entry is unshaped, which is the zero Throttle: disabled, both
// directions unlimited.
func (c *Config) ShapingThrottle(iface string) Throttle {
	if !c.ShapingEnabled() {
		return Throttle{}
	}
	if s := c.ShapingFor(iface); s != nil {
		return s.Throttle
	}
	return Throttle{}
}

// ShapingEnabled reports whether shaping is switched on for this node. See
// ShapingDisabled for why the stored field is inverted.
func (c *Config) ShapingEnabled() bool { return !c.ShapingDisabled }

// ShapingForNetwork is the limit applied to a network's tunnel — the entry
// for the interface that tunnel runs on, if there is one.
func (c *Config) ShapingForNetwork(n Network) Throttle {
	return c.ShapingThrottle(c.IfaceForNetwork(n))
}

// MeshIfaces lists the interface name of every network in this config, in
// config order. These are the interfaces gravinet itself moves packets on,
// and therefore the ones a shaping entry can actually be enforced on.
//
// Every network, not just the enabled ones: a disabled network's device is
// absent right now and comes back when it is switched on, and a rate written
// for it is waiting rather than misdirected.
func (c *Config) MeshIfaces() []string {
	out := make([]string, 0, len(c.Networks))
	for i := range c.Networks {
		if name := c.IfaceForNetworkAt(i); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// How a shaping entry is enforced. Both are real enforcement; they differ in
// which machinery does it, and that is decided by the interface rather than
// by any choice the operator makes.
const (
	// ShapeTunnel: gravinet owns the data path on this interface, so the
	// userspace shaper does it (internal/mesh) — a bounded queue and a
	// drainer, which also understands QoS classes and exempts control
	// traffic. A qdisc can see neither, so mesh interfaces stay here.
	ShapeTunnel = "tunnel"
	// ShapeKernel: no gravinet code sits between the application and the
	// wire on this interface, so nothing local can delay a packet. The
	// kernel's own queueing discipline is the only thing that can, and
	// internal/tcshape programs it (tc; Linux only).
	ShapeKernel = "kernel"
)

// ShapingKind reports which mechanism enforces an interface's entry.
//
// Not a preference and not a fallback: an interface gravinet carries a
// network on cannot be shaped correctly by a qdisc (the qdisc cannot see the
// class of an encrypted overlay packet, and would pace gossip and keepalives
// alongside payload), and an interface it carries nothing on cannot be shaped
// in userspace at all, because there is no userspace to shape in.
func (c *Config) ShapingKind(iface string) string {
	for _, name := range c.MeshIfaces() {
		if name == iface {
			return ShapeTunnel
		}
	}
	return ShapeKernel
}

// KernelShaping lists the entries that need a qdisc programmed: the enabled
// ones, on interfaces gravinet does not carry a network on.
//
// Disabled entries are excluded rather than programmed at their configured
// rate, which is what the switch means — lift the cap, keep the number.
func (c *Config) KernelShaping() []IfaceShaping {
	if !c.ShapingEnabled() {
		return nil
	}
	var out []IfaceShaping
	for _, s := range c.Shaping {
		if s.Enabled && c.ShapingKind(s.Iface) == ShapeKernel {
			out = append(out, s)
		}
	}
	return out
}

// migrateShaping hoists the pre-v960 node default and per-network overrides
// into one list keyed by interface.
//
// Each network contributes the rate it was actually getting — its own
// override if it had one, otherwise the node default — under the name of the
// device its tunnel runs on. That is the rate the shaper was already applying
// to that interface, so a migrated node shapes exactly what it shaped before.
// The two-level model is not preserved: an entry that came from the default
// and one that came from an override are the same fact once they name an
// interface, which is the whole of the change.
//
// A node default on a node with **no** networks has no interface to name. It
// is hoisted to mesh0 — the device the first network would be given — rather
// than dropped. That is a guess about which interface was meant, and it is
// made because the alternative is worse: the rate is real configuration
// somebody typed, discarding it silently is the v955 mistake, and a guess
// that lands as a visible, editable row is one an operator can correct in a
// double-click. It is only ever taken when there is nothing else to go on.
//
// A config already carrying a Shaping list is left alone; the legacy fields
// are cleared either way so they are never written back out.
func (c *Config) migrateShaping() {
	defer func() {
		c.Throttle = Throttle{}
		for i := range c.Networks {
			c.Networks[i].Throttle = nil
		}
	}()
	if len(c.Shaping) > 0 {
		return
	}
	set := func(iface string, t Throttle) {
		if iface == "" || t == (Throttle{}) {
			// Nothing configured is not a limit. An entry here would be a row
			// saying "disabled, unlimited", which is what having no entry
			// already means.
			return
		}
		if s := c.ShapingFor(iface); s != nil {
			s.Throttle = t
			return
		}
		c.Shaping = append(c.Shaping, IfaceShaping{Iface: iface, Throttle: t})
	}
	for i := range c.Networks {
		t := c.Throttle
		if o := c.Networks[i].Throttle; o != nil {
			t = *o
		}
		set(c.IfaceForNetworkAt(i), t)
	}
	if len(c.Networks) == 0 {
		set(autoIfaceName(0), c.Throttle)
	}
}

// migrateFirewall hoists the pre-v957 per-network rulebase into the
// node-global one and assigns stable ids.
//
// Each network's rules move up in order, scoped to the network they came from,
// which is what they already meant: enforced on that tunnel and nowhere else.
// Order is preserved within each network, which is what matters — first match
// wins, and a rule only ever competes with the rules in scope alongside it.
//
// A network whose firewall switch was off contributes its rules disabled, the
// same fold NAT and QoS got: the per-network gate has no equivalent now, so it
// moves into the rules and the node enforces exactly what it enforced before.
//
// Ids are assigned here rather than left to the engine because config is the
// durable record. Every rule gets one, including rules that arrive already
// carrying an id from a previous load.
func (c *Config) migrateFirewall() {
	if len(c.Firewall.Rules) == 0 {
		for i := range c.Networks {
			n := &c.Networks[i]
			for _, r := range n.Firewall.Rules {
				r.Scope = fwScopeName(n)
				if !n.Firewall.Enabled {
					r.Disabled = true
				}
				c.Firewall.Rules = append(c.Firewall.Rules, r)
			}
			if n.Firewall.Enabled {
				c.Firewall.Enabled = true
			}
		}
	}
	for i := range c.Networks {
		c.Networks[i].Firewall = Firewall{}
	}
	c.assignFirewallIDs()
}

// assignFirewallIDs gives every rule that lacks one a fresh id, and keeps
// NextID ahead of every id in use.
//
// Never reuses an id, even one freed by a delete: a returning id would bind
// stale hit counters and stale UI selections to a different rule.
func (c *Config) assignFirewallIDs() {
	for _, r := range c.Firewall.Rules {
		if r.ID >= c.Firewall.NextID {
			c.Firewall.NextID = r.ID + 1
		}
	}
	if c.Firewall.NextID == 0 {
		c.Firewall.NextID = 1
	}
	for i := range c.Firewall.Rules {
		if c.Firewall.Rules[i].ID == 0 {
			c.Firewall.Rules[i].ID = c.Firewall.NextID
			c.Firewall.NextID++
		}
	}
}

// fwScopeName is the name a rule uses to reach one network: its name, or its
// id for a network that has none.
func fwScopeName(n *Network) string {
	if strings.TrimSpace(n.Name) != "" {
		return n.Name
	}
	return n.ID
}

// FirewallRulesFor returns the rules enforced on a network, in order: those
// scoped to it, plus every rule that named no network.
func (c *Config) FirewallRulesFor(n Network) []FirewallRule {
	var out []FirewallRule
	for _, r := range c.Firewall.Rules {
		scope := strings.TrimSpace(r.Scope)
		if scope == "" || strings.EqualFold(scope, n.Name) || strings.EqualFold(scope, n.ID) {
			out = append(out, r)
		}
	}
	return out
}

// checkFirewallScope refuses a scope naming no mesh network. Empty is always
// valid and means every network.
func (c *Config) checkFirewallScope(scope string) error {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return nil
	}
	for i := range c.Networks {
		if strings.EqualFold(c.Networks[i].Name, scope) || strings.EqualFold(c.Networks[i].ID, scope) {
			return nil
		}
	}
	return fmt.Errorf("no mesh network named %q — leave the scope blank to enforce on every network", scope)
}
