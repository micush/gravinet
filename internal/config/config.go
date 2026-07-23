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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultUDPPort is tried first; FallbackUDPPorts are tried in order if the
// primary cannot bind or a peer is unreachable on it.
//
// 65432 sits in the IANA dynamic/private range (49152-65535), so it carries no
// registered-service assignment to collide with — and it deliberately isn't
// 51820 (WireGuard), 41641 (Tailscale), 9993 (ZeroTier), or any other overlay's
// well-known port, so gravinet doesn't masquerade as something it isn't. The
// descending 6-5-4-3-2 is just easy to remember. Any value works; this is only
// the out-of-the-box default and is changeable live under Settings.
const DefaultUDPPort = 65432

// DefaultTCPFallbackPort is the default TCP/TLS fallback listen port. It mirrors
// the UDP port so a node listens on the same number on both transports by
// default; set tcp_fallback_port to anything (e.g. 443) to change it.
const DefaultTCPFallbackPort = 65432

// DefaultControlSocket is the local IPC endpoint used by the CLI. It's
// platform-specific — see socket_linux.go / socket_bsd.go / socket_windows.go /
// socket_other.go — since "/run" is a Linux (systemd/FHS) convention that
// doesn't exist by default on macOS, FreeBSD, or Windows.

// FallbackUDPPorts are well-known UDP ports likely to traverse restrictive
// middleboxes when the primary is blocked.
var FallbackUDPPorts = []int{443, 4500, 3478, 1194, 500, 53}

// Config is the whole daemon configuration.
type Config struct {
	// Node identity & global behavior.
	NodeID   string `json:"node_id"`  // stable random id; auto-generated if empty
	Hostname string `json:"hostname"` // advertised to peers; OS hostname if empty
	LogLevel string `json:"log_level"`

	// Underlay listening. PrimaryPort is bound first; if it fails, the daemon
	// walks FallbackUDPPorts. 0 turns UDP off entirely (the "-" sentinel in the
	// web admin's UDP port field) — Validate refuses this unless the TCP/TLS
	// fallback is enabled, since the node needs at least one live transport.
	PrimaryPort int `json:"primary_port"`
	// ExtraListenPorts are bound *in addition* to the primary so peers behind a
	// restrictive firewall can reach this node on a well-known port (e.g. 443).
	// Best-effort: a port that can't bind (privileged or in use) is skipped with
	// a warning. Replies go back out the port a peer arrived on. Empty by default.
	ExtraListenPorts []int `json:"extra_listen_ports,omitempty"`
	// TCP/TLS fallback: every node also listens on this TCP port (default 65432,
	// same as the UDP port), wrapped in TLS so it looks like HTTPS, so peers on
	// UDP-hostile networks can still reach the mesh. Set it to any port (e.g. 443)
	// to change it. On by default; set disable_tcp_fallback to opt out. A bind
	// failure (privileged/in use) is non-fatal (UDP-only).
	TCPFallbackPort    int  `json:"tcp_fallback_port,omitempty"` // 0 => 65432
	DisableTCPFallback bool `json:"disable_tcp_fallback,omitempty"`
	// ExtraTCPListenPorts are additional TCP/TLS fallback listeners, bound *in
	// addition* to TCPFallbackPort — the TCP-side equivalent of
	// ExtraListenPorts above, same motivation and same best-effort semantics
	// (a port that can't bind is skipped with a warning, not fatal). Empty by
	// default.
	ExtraTCPListenPorts []int `json:"extra_tcp_listen_ports,omitempty"`

	// EnableUPnP turns on gravinet's own best-effort UPnP IGD port-forwarding
	// helper: on startup, gravinet asks the LAN gateway (via UPnP, if it
	// supports it and has UPnP enabled) to forward every port this node
	// actually listens on — the primary UDP port, the TCP/TLS fallback port,
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

	EnableIPv4    bool `json:"enable_ipv4"`    // underlay v4
	EnableIPv6    bool `json:"enable_ipv6"`    // underlay v6
	WorkerThreads int  `json:"worker_threads"` // 0 => runtime.NumCPU()-1, min 1

	// IPForwarding controls whether the daemon turns on host IPv4/IPv6 forwarding
	// at startup (the on-ramp for redistributed routes and NAT). nil means the
	// default, which is enabled; set to false to leave host forwarding untouched.
	// The prior value is restored on a clean shutdown.
	IPForwarding *bool `json:"ip_forwarding,omitempty"`

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
	// connected in the peers table. 0 or unset means the default (20s). An
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
	// layer and reassembled by the peer, so the jumbo tunnel MTU (9216) works
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

	// path is where this config was loaded from / will be saved to.
	path string
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
	MTU     int    `json:"mtu"`      // tunnel MTU, default 9216

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
	// SeedTCPPort is an optional TCP/TLS fallback port to dial the Seeds on when
	// UDP can't reach them at cold start (before any peer's port is learned via
	// handshake/gossip). Populated from a join token so a node can bootstrap over
	// TCP onto a mesh using a non-default port. 0 means "assume our own port".
	SeedTCPPort int `json:"seed_tcp_port,omitempty"`

	StormControl   StormControl `json:"storm_control"`
	Throttle       Throttle     `json:"throttle"`
	QoS            QoS          `json:"qos"`
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
	NAT                   NAT `json:"nat"`

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
}

// UnmarshalJSON backfills DNSSync to its documented on-by-default value
// (NewNetworkDefaults's {Enabled:true, GossipPPS:5, TTLSeconds:300}) when a
// network's JSON has no "dns_sync" key at all, instead of leaving it at
// encoding/json's zero value of {Enabled:false, GossipPPS:0, TTLSeconds:0}.
//
// This matters because DNSSync was added after HostsSync: every config ever
// written by gravinet has always had "hosts_sync" (it predates the public
// project), so HostsSync's identically-shaped Enabled bool never hits this.
// Any config saved before conditional DNS forwarding existed has no
// "dns_sync" key at all, so without this backfill it silently loads as fully
// disabled — indistinguishable from an operator's deliberate choice, and,
// worse, the very next SaveTo (triggered by any unrelated edit: adding a
// seed, a host record, anything) marshals that zero value back out as an
// explicit "enabled": false. At that point the key is no longer absent, it's
// explicit, and DNS forwarding stays silently off across every future
// restart until someone notices — restarting the daemon re-reads the same
// file and gets the same answer every time.
//
// Only the true "key entirely absent" case is backfilled; a config that
// already has an explicit "dns_sync" object (even one that happens to be
// all zeros, which is also a valid deliberate choice: disabled, unlimited
// gossip, default TTL) is left exactly as written — this can only add the
// default for networks that never had an opinion recorded, never override
// one that did.
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
	}
	return nil
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

// Addrs returns just the addresses, in order — the shape most callers that
// only care about where to dial actually want (resolving, de-duplicating,
// joining into a boot list alongside PeerCache, etc.).
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

// Throttle caps tunnel bandwidth. Up is the egress (shaped) rate; Down is the
// ingress (policed) rate. Set one for a single direction, both for "both",
// neither for unlimited. All values are bytes per second; 0 = unlimited.
type Throttle struct {
	Enabled         bool `json:"enabled"` // off by default
	UpBytesPerSec   int  `json:"up_bytes_per_sec"`
	DownBytesPerSec int  `json:"down_bytes_per_sec"`
	BurstBytes      int  `json:"burst_bytes"` // token-bucket depth; 0 = default
	QueueBytes      int  `json:"queue_bytes"` // egress queue capacity; 0 = default
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
}

// Firewall is a network's packet filter. It is off by default; when enabled with
// an empty rulebase the default policy is allow (stateful), so add rules to
// restrict. When disabled, no filtering happens at all.
//
// Rules reference reusable address-object and service catalogs by name (see
// FirewallRule.Src/Dst and FirewallRule.Services) — those catalogs are node-
// global (Config.FirewallObjects/FirewallServices, shared by every network on
// this node), not part of this per-network struct.
type Firewall struct {
	Enabled bool           `json:"enabled"`
	Rules   []FirewallRule `json:"rules"`
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
// TCPFallbackEnabled reports whether the node should also listen on the TCP/TLS
// fallback port. On by default for every node; set disable_tcp_fallback to opt out.
func (c *Config) TCPFallbackEnabled() bool { return !c.DisableTCPFallback }

// TCPFallbackPortValue is the TCP/TLS fallback listen port (default 443).
func (c *Config) TCPFallbackPortValue() int {
	if c.TCPFallbackPort == 0 {
		return DefaultTCPFallbackPort
	}
	return c.TCPFallbackPort
}

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
//     restored automatically).
type NATRule struct {
	Source    string `json:"source"`
	Dest      string `json:"dest"`
	Translate string `json:"translate"`
	Interface string `json:"interface,omitempty"` // egress interface for masquerade
	Enabled   bool   `json:"enabled"`

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

// NAT is a network's address translation. It is off by default; when disabled no
// translation happens. Individual rules also have their own Enabled flag.
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
	Enabled    bool        `json:"enabled"`
	Listen     string      `json:"listen"`   // e.g. 127.0.0.1:8443
	TLSCert    string      `json:"tls_cert"` // path; self-signed generated if empty
	TLSKey     string      `json:"tls_key"`
	AuthMode   string      `json:"auth_mode"`   // "local", "pam" (linux/macos/freebsd), "system" (openbsd bsd_auth), or "windows"
	PAMService string      `json:"pam_service"` // e.g. "gravinet" or "login"
	AllowUsers []string    `json:"allow_users"` // for pam/windows: limit to these system users (empty = any)
	Users      []AdminUser `json:"users"`       // for auth_mode "local"
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

func (h HostsSync) TTL() time.Duration { return time.Duration(h.TTLSeconds) * time.Second }

// Default returns a config with sane defaults and one empty disabled network.
func Default() *Config {
	return &Config{
		LogLevel:      "info",
		PrimaryPort:   DefaultUDPPort,
		EnableIPv4:    true,
		EnableIPv6:    true,
		WorkerThreads: 0,
		AuthBan:       BanPolicy{MaxFailures: 3, WindowSeconds: 60, BanSeconds: 900, CoalesceSeconds: 3},
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
		MTU:          9216,
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

// ForwardingEnabled reports whether the daemon should enable host IP forwarding
// at startup. Defaults to true when unset (nil); an explicit false opts out.
func (c *Config) ForwardingEnabled() bool {
	return c.IPForwarding == nil || *c.IPForwarding
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
// value in seconds, defaulting to 20s when unset, floored at 1s, and — like
// mesh.Engine.SetPeerTimeout — clamped up to KeepaliveDuration if an
// explicit value would otherwise be shorter than a single keepalive cadence.
func (c *Config) PeerTimeoutDuration() time.Duration {
	d := 20 * time.Second
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
	// 0 means the UDP underlay is turned off entirely (see the "-" sentinel
	// in the web admin's UDP port field, and Dual.Send's corresponding nil
	// check) — anything else must be a real port.
	if c.PrimaryPort < 0 || c.PrimaryPort > 65535 {
		return fmt.Errorf("primary_port %d out of range", c.PrimaryPort)
	}
	if c.PrimaryPort == 0 && !c.TCPFallbackEnabled() {
		return fmt.Errorf("primary_port is off (0) and the TCP/TLS fallback is also off — at least one underlay transport must stay on, or this node could never be reached")
	}
	for _, p := range c.ExtraListenPorts {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("extra_listen_ports: %d out of range", p)
		}
	}
	if c.TCPFallbackPort != 0 && (c.TCPFallbackPort < 0 || c.TCPFallbackPort > 65535) {
		return fmt.Errorf("tcp_fallback_port %d out of range", c.TCPFallbackPort)
	}
	for _, p := range c.ExtraTCPListenPorts {
		if p <= 0 || p > 65535 {
			return fmt.Errorf("extra_tcp_listen_ports: %d out of range", p)
		}
	}
	// A configured log cap must parse; reject bad input at save time so the
	// running daemon never has to fall back silently (see LogMaxBytes).
	if strings.TrimSpace(c.LogMaxSize) != "" {
		if _, err := ParseSize(c.LogMaxSize); err != nil {
			return fmt.Errorf("log_max_size: %v", err)
		}
	}
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
	for i := range c.Networks {
		for j := range c.Networks[i].NAT.Rules {
			r := &c.Networks[i].NAT.Rules[j]
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
			n.MTU = 9216
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
		// QoS: 5 priority classes by default with class 3 (normal) for unmatched
		// traffic. Classes 0-2 are above normal, 4 is bulk. Migrate older 3-class
		// configs up so existing rules (classes 0-2) keep working.
		if n.QoS.Enabled {
			if n.QoS.Classes < 5 {
				n.QoS.Classes = 5
			}
			if n.QoS.DefaultClass <= 0 || n.QoS.DefaultClass >= n.QoS.Classes {
				n.QoS.DefaultClass = 3
			}
			// QoS is inert without an egress rate cap to create contention for
			// the priority queue to reorder, so enabling QoS enables the
			// up-throttle. If no rate is configured yet, seed a placeholder the
			// operator should lower to their real uplink.
			n.Throttle.Enabled = true
			if n.Throttle.UpBytesPerSec <= 0 {
				n.Throttle.UpBytesPerSec = defaultQoSUpBytesPerSec
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
	if c.BGP.Enabled && c.BGP.ASN == 0 {
		return fmt.Errorf("bgp: a local AS number is required to enable BGP")
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
