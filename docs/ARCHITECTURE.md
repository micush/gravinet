# [gravinet] — architecture & roadmap

A single-binary, full-mesh, encrypted overlay VPN. Go, stdlib-only core,
`CGO_ENABLED=0` static build, cross-compiles to linux/windows/darwin/freebsd.

## Why Go
Single static binary, trivial cross-compile, AES-256-GCM + X25519 in the
stdlib (`crypto/aes`, `crypto/cipher`, `crypto/ecdh`), goroutine concurrency,
UDP in `net`. The only places stdlib genuinely can't reach are the platform
TUN device (raw ioctl/syscall on Linux, Wintun DLL on Windows, utun socket on
macOS) and PAM/Windows auth for the admin UI — those are isolated behind small
build-tagged files so the core stays pure.

## Component map
```
cmd/gravinet        entrypoint, subcommands (run, genkey, ctl, version)
internal/config    single source of truth; hot-reloadable; what the web UI edits
internal/crypto    key slots, keyID derivation, AEAD session cipher, replay window
internal/protocol  wire framing: packet types, headers, MTU/fragmentation
internal/transport UDP sockets, port fallback, NAT-aware send/recv, worker pool
internal/tun       platform TUN device (linux/windows/darwin/freebsd build tags)
internal/mesh      peers, handshake, sessions, mesh formation, relaying, routing
internal/control   control plane: node list gossip, bans, hostnames, routes, NAT
internal/webadmin  hot-config admin UI + PAM/Windows auth + login throttle
internal/service   systemd / Windows SC / launchd install + run
```

## Wire protocol (outer UDP payload)
Byte 0 is always version, byte 1 is type.

Data (hot path, O(1) decrypt — no per-packet key trial):
```
[ver:1][type=DATA:1][recv_session:4][counter:8][ciphertext..][tag:16]
```
`recv_session` is the index the *receiver* assigned at handshake; it selects the
session, which carries the negotiated key and peer. `counter` is the GCM nonce
source and feeds a sliding-window replay filter.

The data path is built to allocate nothing per packet. AES-256-GCM runs on the
stdlib AEAD, which selects the AES-NI + CLMUL assembly automatically on amd64/arm64
(measured here at ~3–4.8 GB/s of seal throughput per core, so the cipher is not the
bottleneck). On send, the whole outer packet — header, inner type, plaintext, tag —
is laid out in one pooled buffer and the frame is encrypted in place (`Seal` with
`dst = plaintext[:0]`); the send counter is bumped with an atomic, so sealing is
lock-free and safe to parallelize. On receive, the packet is decrypted in place
back into the transport's pooled read buffer. Versus the earlier allocate-and-copy
path this is roughly 4 fewer allocations and ~280× less allocation volume per
packet, which removes the GC pressure that otherwise caps throughput. (Per-flow
throughput is still bounded by the single TUN-reader goroutine and one syscall per
packet; segmentation offload and batched syscalls are the next levers.)

Handshake (no session yet, so keyed by key identity, not slot):
```
[ver:1][type=HS_INIT:1][network:8][keyID:8][nonce:12][ciphertext..][tag:16]
[ver:1][type=HS_RESP:1][recv_session:4][nonce:12][ciphertext..][tag:16]
```
`keyID = first 8 bytes of SHA-256(key)`. This is the mechanism that makes
"slots don't need to match across hosts, only the key must match": keys are
matched by identity, not by slot position. The HS_INIT carries an X25519
ephemeral public key; the PSK from the matched slot authenticates the exchange
and the ECDH result provides forward secrecy. Session keys =
HKDF(ecdh_shared, psk, transcript).

Control messages travel as encrypted DATA packets with an internal control
channel marker, so the same session crypto protects them.

## Key auth & ban logic (interpreting the spec)
- A node holds up to 8 keys per network. On join it tries them in order.
- Each key is tried as a separate HS_INIT. The responder matches by keyID
  against *any* of its slots.
- One *failed authentication event* = an initiator burst from a source that
  exhausts its keys without a match, coalesced within a short window so the
  natural "up to 8 keys" burst counts as **one** attempt — this resolves the
  spec's open question.
- 3 failed events from one source IP within 60s ⇒ ban that source for 15min
  (handshake-layer ban, separate from the distributed node ban list).
- Banning a node tears down its session and floods the ban; unbanning floods a
  removal. The catch is that the unban can't reach the banned node over a session
  that no longer exists, and the formerly-banned node won't re-dial until its own
  stale session times out (`peerTimeout`, 75s) — so a naive unban leaves the peer
  offline for over a minute. To avoid that, each node remembers the banned peer's
  last-known *underlay* endpoint in the ban record (kept locally, not gossiped),
  and on unban (explicit, gossiped, force, or TTL expiry) it re-dials that
  endpoint. Reconnection then happens in about a second. The remembered endpoint
  is the address the peer's packets actually came from, so it's a real underlay
  address; if it's gone stale the re-dial simply fails and the normal seed path
  takes over.

- Route redistribution applies live (no restart), like the firewall and peer
  toggles. The advertised route set and the reject set are held in atomic pointers
  on each network (`advRoutes`/`advReject`), swapped on reload; the reload floods
  the delta — newly-added routes are advertised, removed ones are *withdrawn*. A
  withdrawal is a flooded control message (`ctrlRouteDel`, honored only from the
  route's origin, re-flooded like an unban) so every node drops it from its
  redistributed table. Without that, a redistributed CIDR — once learned — would
  linger on peers until they restarted. (Earlier, route changes only touched
  config and required a restart on the advertising node before they were flooded
  at all, which is why a freshly-redistributed route never reached peers.)

- A learned redistributed route is also **installed into the host routing table**
  on the receiving node (`<prefix> dev <tun>`), not just the engine's internal
  table. Without that the kernel never hands the traffic to the TUN — the route
  would be invisible to `ip route` and unused by the host — so the feature only
  worked for traffic that already happened to enter the overlay. On Linux this is
  done over rtnetlink directly (no `ip`/`route` binary, no cgo — `route_linux.go`,
  matching the ioctl-based interface setup); macOS, FreeBSD, and Windows shell
  out to `route`/`netsh` like their interface setup does. Install happens when the route
  is learned (`onRouteAdd`) and removal when the *last* advertiser withdraws it
  (`onRouteDel` only pulls the OS route once no origin still advertises the
  prefix, so redundant advertisers don't tear each other's route down). The
  origin node never installs a route for a prefix it advertises — it already
  reaches that network directly. Failures are logged, not fatal: the engine's
  internal forwarding still works, so a platform that can't program the table
  (or IPv6 where it's disabled) degrades rather than breaking the mesh. Verified
  end-to-end with two daemons — the advertised CIDR appears in the peer's real
  kernel table.

- Routes are removed from peers both on an explicit withdrawal (the operator
  deletes the route, which floods `ctrlRouteDel`) **and** when the advertising
  node's session goes away — a silent peer that gets pruned past `peerTimeout`,
  or one that is banned or disabled, has its learned routes dropped (and their OS
  routes uninstalled) on every other node, rather than lingering until a restart.
  They are re-learned if the node returns and re-advertises.

- A reject rule removes the matching route live, the same way a withdrawal does:
  on reject the route is dropped from the forwarding table and uninstalled from
  the OS routing table immediately (no restart), and further advertisements of it
  are refused. Lifting a reject lets the route be re-learned the next time its
  origin advertises it (on reconnect, a route change, or restart).

- Redistributed routes are re-advertised on a timer, not only on change: every
  node re-floods its own routes at a configurable cadence (`route_advertise_interval`
  seconds; default 10s, settable live in the CLI config or the web Settings panel).
  Re-advertisement heals advertisements lost to packet drops, lets a node that
  joined late or just lifted a reject pick the route back up within one interval
  without a reconnect, and keeps learned routes fresh. Receivers treat a repeat of
  a route they already hold as a no-op (or a metric update), so the periodic flood
  is cheap and doesn't amplify across hops.

- Each redistributed route carries a **metric** that is settable in the CLI
  (`route redistribute CIDR -metric N`), the web UI (a field on the add row, and
  double-click a metric cell to edit it in place), and the config. The metric is
  advertised on the wire, propagates live when changed (the origin re-advertises;
  peers update in place and re-flood), and is used by the forwarding lookup as a
  tie-break: among equally specific prefixes the lowest metric wins. All of it
  applies live with no restart.

- For a node to actually *route* between the overlay and its other interfaces
  (forward packets for redistributed routes, or NAT a LAN onto the mesh), the
  host kernel must have IP forwarding on. The daemon enables IPv4 and IPv6
  forwarding at startup (`internal/ipfwd`): on Linux by writing the procfs sysctls
  directly (`net.ipv4.ip_forward`, `net.ipv6.conf.all.forwarding`), on macOS and
  FreeBSD via the same `sysctl` knobs (`net.inet.ip.forwarding`,
  `net.inet6.ip6.forwarding` — FreeBSD goes through the `sysctl` binary rather
  than a direct file write since procfs isn't reliably mounted there), on
  Windows via the `IPEnableRouter` registry values. It records the
  prior values and restores them on a clean shutdown, and it only reverts a knob
  it actually changed — forwarding that was already on stays on. A missing knob
  (IPv6 disabled) or a permission failure is logged, not fatal. This defaults on
  and is opt-out per daemon with `"ip_forwarding": false`. Forwarding is the
  on-ramp's counterpart: routes tell the kernel to hand packets to the TUN, and
  forwarding lets the node pass them on toward their real destination.

- Local peer enable/disable is the local-only counterpart to a ban. A ban is a
  mesh-wide verdict (flooded, enforced everywhere); disabling a peer is a private
  decision on one node — it never floods. Each network carries a `disabled_peers`
  list in config; the engine keeps it as a per-network set (`netState.disabledPeers`)
  populated at startup and hot-swapped on reload, so enable/disable applies live
  (no restart). A disabled peer is refused at every place a ban is — both
  handshake directions, both relay paths, and the gossip-dial path — and an
  already-connected peer is torn down immediately via `localDisconnect`, which
  (unlike `applyBan`) keeps the peer's learned endpoint so re-enabling lets the
  maintenance loop redial it. Because it's config-driven, the web UI/CLI just edit
  `disabled_peers` and reload; no gossip is involved and other nodes are
  unaffected.

## Storm control
Token-bucket per (network, traffic-class) for broadcast, multicast, and the
hostname-gossip channel. Excess is dropped, not queued.

## Hosts file sync
Receivers write `hostname → overlay IP` to the platform hosts file
(`/etc/hosts`, `%SystemRoot%\System32\drivers\etc\hosts`, macOS-formatted),
v4 and/or v6 per the peer's assigned addressing, inside a fenced managed block.
Entries expire on a TTL when a peer goes silent.

## MTU / fragmentation
Default tunnel MTU 9216 (jumbo). Outer packets set DF; when an inner packet
would exceed discovered path MTU, gravinet fragments at its own protocol layer
(reassembled by recv_session + frag header) rather than relying on IP frag.

## Roadmap (priority order — each step compiles & is testable)
1. ✅ Skeleton, config, logging, crypto core, protocol framing
2. ✅ TUN device abstraction (linux) + UDP transport w/ port fallback + worker pool
3. ✅ Handshake + sessions + point-to-point encrypted tunnel
4. ✅ Mesh formation: node-list gossip, auto full-mesh, NAT keepalive
5. ✅ Overlay addressing: subnet handout, random pick, DAD
6. ✅ Control plane: distributed bans, hostname→hosts sync, route redist/reject
7. ✅ Relaying when direct mesh fails
8. ✅ Broadcast/multicast + storm control (and ban TTL/refresh/adopt)
9. ✅ Bandwidth throttling: per-network rate caps, up / down / both
10. ✅ Quality of service: classify and prioritise traffic within the shaped link
11. ✅ Firewalling: ordered rulebase, default allow-all, full rule management
12. ✅ NAT (overlay↔underlay, overlay↔overlay)
13. ✅ Web admin (hot config) + auth + login throttle
14. ✅ Service integration (systemd/launchd/Windows SCM) + Windows/macOS TUN
15. ✅ Hardening: fuzzing, replay/anti-spoofing tests, release matrix

## Hardening (step 15)

The wire-facing decoders are continuously fuzzed with Go's native fuzzer: the
outer header parsers (`DecodeData`/`DecodeHSInit`/`DecodeHSResp`/`PacketType`),
every inner-control decoder (bans, routes, relay envelopes, peer lists,
addresses, the L4/IP parsers), and — most importantly — the `OnPacket` network
entry point itself, driven with arbitrary datagrams through a live engine.
Hundreds of thousands of executions per target surface no panics, and garbage
input never forms a peer.

Security properties are covered by targeted tests: AEAD tamper rejection (any
change to ciphertext or the authenticated header fails to open — the
anti-spoofing guarantee, since forgery needs the session key regardless of
source address), the replay window (a re-sent counter is rejected), and the
brute-force join throttle (repeated bad-key joins from one source get it banned).
Every decoder is also checked against truncated input.

`scripts/build-release.sh` produces the full static, stripped, version-stamped
matrix — linux (amd64/arm64/arm), windows (amd64/arm64), darwin (amd64/arm64),
freebsd/amd64 — with SHA-256 checksums, CGO disabled throughout.

## Status — roadmap complete
All 15 steps are done. The tree is ~62k lines of dependency-free Go (~98k
including tests) as of v560, passes `go vet` and the full suite under
`-race`, is fuzzed at the network boundary, and cross-compiles to the whole
release matrix.

### Post-roadmap refinements
- **Per-feature on/off switches.** The firewall, NAT, QoS, and bandwidth
  throttle each have an `enabled` flag and are **off by default**. In config the
  firewall and NAT are objects (`{"enabled":…,"rules":[…]}`); throttle and QoS
  carry an `enabled` field. A disabled feature is wired out entirely — the
  firewall isn't even constructed, so the data path keeps its fast path, and the
  firewall management API/CLI returns "firewall is disabled" rather than acting.
- **Configurable bans.** Both brute-force throttles — the auth/join ban
  (`auth_ban`) and the admin login ban (`login_ban`) — are fully configurable
  from config, including the coalesce window (`coalesce_seconds`) that folds a
  single join's multiple key-tries into one counted failure. The honest, unchanged caveats
are the genuinely platform/cgo-bound seams — PAM/Windows admin auth and the
macOS utun / Windows Wintun TUN backends and Windows SCM dispatcher — which are
written as real implementations and verified by compilation + review but can
only be exercised on their target OSes; and container-level limits (IPv6
disabled here, so v6 paths are review + v4-equivalent tested; relay validated
in-process). Trust remains mesh-wide: the pre-shared key is the trust boundary.

## Service integration & platform backends (step 14)

**Service integration** (`internal/service`): the daemon can run as a managed OS
service. It generates the right definition per platform — a `Type=notify`
systemd unit (Linux, with ambient `CAP_NET_ADMIN` so it can run as a non-root
user), a launchd daemon plist (macOS), or the `sc.exe` commands to register a
Windows service — via `gravinet service <print|install|uninstall>`. On Linux it
sends `READY=1` over the sd_notify socket once the engine is up (hand-rolled,
no libsystemd). The daemon's run loop was refactored around a stop channel so
the same body serves interactive/systemd/launchd operation (SIGINT/SIGTERM) and,
on Windows, the Service Control Manager: `RunService` talks to the SCM through
advapi32 (`StartServiceCtrlDispatcher` + a `syscall.NewCallback` handler), and
falls back to an interactive run when not launched by the SCM.

**Platform TUN backends**: alongside the Linux ioctl backend there are now real
macOS, Windows, and FreeBSD implementations. macOS uses the built-in `utun`
kernel control (socket/connect/ioctl), handling the 4-byte address-family
header utun frames carry, with addressing via `ifconfig`. FreeBSD uses the
built-in `tun(4)` driver in "multi-af" mode (`TUNSIFHEAD`), which carries the
same 4-byte address-family framing as utun for the same reason (one device,
both IP versions), with addressing via `ifconfig`/`route(8)` and explicit
interface teardown (`SIOCIFDESTROY`) on close rather than relying on the file
descriptor closing to imply it, the way it does on macOS. Windows uses the
Wintun userspace driver through `syscall` and its ring-buffer session API,
with addressing via `netsh`.
Windows has no built-in TUN, so a signed kernel driver is unavoidable there;
Wintun's DLL is **embedded in the binary** (`go:embed`, per architecture) and
extracted to a per-user cache directory on first use, so a release build is a
single self-contained `.exe`. If no real DLL was bundled (the source tree ships
a placeholder so it compiles), the backend loads a `wintun.dll` placed beside
the executable instead. The signed kernel driver itself is still required at
runtime — that's a hard Windows platform constraint, not a packaging choice.
`scripts/build-release.sh` stages the signed DLLs into the embed slots when
`WINTUN_DIR` points at them (e.g. the `bin/` folder from the wintun.net zip).
`scripts/build-all.sh` is the one-shot wrapper: it downloads and
checksum-verifies the signed Wintun driver (pinned to a known SHA-256), runs
gofmt/vet/the full test suite, then builds the entire matrix with the driver
embedded — producing single self-contained artifacts plus `SHA256SUMS`. If the
driver can't be fetched (offline, or `REQUIRE_WINTUN=1` not set), it falls back
to the side-by-side mode with a warning rather than embedding anything
unverified. Because gravinet's Go code has zero module dependencies, the Wintun
driver is the only thing the build needs to fetch.

### Installers
`install/` carries turnkey installers — `install-linux.sh` (systemd),
`install-macos.sh` (launchd), `install-freebsd.sh` (rc.d), and
`install-windows.ps1` (Windows service) — each
of which places the binary, scaffolds a config via `gravinet ... -init`,
registers the daemon using the binary's own `service install` (so the unit/plist
path matches the install location), **starts the service, and upgrades in place**
on re-run: a running daemon is stopped before its binary is replaced, then
restarted (on Windows the stop is mandatory, since a running `.exe` is locked).
When no prebuilt binary is present the installers **build from source** — each
bootstraps a Go toolchain if the host lacks one (system package manager or the
official go.dev tarball, checksum-verified), builds the binary, and installs it;
the Windows installer additionally fetches the signed Wintun driver (pinned
SHA-256) to ship beside the executable. `--no-start`/`-NoStart` installs without
starting; `--uninstall`/`-Uninstall` removes cleanly. The raw daemon definitions are generated from the single source
of truth in `internal/service` (via `gravinet service print -os
<linux|darwin|windows>`) and ship as `gravinet.service`,
`com.gravinet.daemon.plist`, and `windows-service.txt`. The release build bundles
all of these into the output directory alongside the binaries and checksums.

### Best-effort transport binding
So that auto-start succeeds on hosts where one IP family is unavailable (e.g. a
server with IPv6 disabled), the transport binds in two passes: first strictly —
a single shared port where every enabled family binds — and, if that fails for
all candidate ports, best-effort, accepting whatever families come up as long as
at least one does. A partial bind logs a warning (`running IPv4-only` /
`running IPv6-only`) rather than aborting the daemon.

### Socket and queue buffer sizing
At multi-Gbps the kernel-default UDP socket buffers (~208 KB) overflow on bursts
and silently drop datagrams — visible as `RcvbufErrors` in `netstat -su` — which
TCP over the overlay sees as loss and answers with retransmits and congestion
backoff, capping throughput well below the link. Each bound socket is therefore
sized to `Options.SocketBuffer` (default 4 MiB). On Linux the daemon runs as root,
so it sets `SO_RCVBUFFORCE`/`SO_SNDBUFFORCE`, which bypass `net.core.{r,w}mem_max`
and spare the operator from raising sysctls (it falls back to the clamped option
otherwise); other platforms use the portable `SetReadBuffer`/`SetWriteBuffer`.
Symmetrically, the Linux TUN interface tx queue is deepened past its 500-packet
default so a brief stall in the single overlay reader doesn't drop outbound
packets at the qdisc. Both are best-effort: a failure forgoes the tuning, never
the socket. (Throughput is still ultimately bounded by the single TUN reader and
one syscall per packet; segmentation offload and batched syscalls are the next
levers.)

## Status
Steps 1–14 complete. The service-definition generators (systemd/launchd/rc.d/
Windows SCM) and the sd_notify readiness handshake are unit-tested (including
a real round-trip over a unixgram socket), the `service` CLI installs a
working unit, and the refactored stop-channel run loop was verified to start
and shut down cleanly on SIGTERM. The whole tree passes `-race` and
cross-compiles cleanly for linux, windows, darwin, and freebsd on amd64 and
arm64 (freebsd/arm64 isn't in the official release matrix, but it builds and
vets clean too) — which is what compiles the macOS utun, the Windows Wintun
backend, the FreeBSD tun(4) backend, and the Windows SCM dispatcher.

Honest scope: the macOS `utun`/Windows Wintun/FreeBSD `tun(4)` TUN backends
and the Windows SCM dispatcher are genuinely platform-bound — they're written
as real implementations and verified by compilation + review, but this Linux
build host cannot execute them (just as PAM/Windows auth can't be
pure-stdlib). The Linux data path, service generation, and sd_notify are the
parts exercised here.

Kernel NAT (`internal/netfilter`) now has a real backend on every supported
platform, each driving whatever that OS actually provides — there's no
universal API, so the shape of what's expressible differs slightly per
backend:

- **Linux**: nft (preferred) or iptables, in a dedicated `gravinet_nat`
  table/chain pair. Full Masquerade/SNAT/DNAT, v4 and v6.
- **macOS**: pf, via a `com.apple/gravinet` anchor. macOS's default
  `/etc/pf.conf` already wildcard-hooks `nat-anchor "com.apple/*"` etc., so
  loading rules there takes effect immediately with no edit to pf.conf and no
  risk of colliding with Application Firewall or Internet Sharing's own
  anchors. Enabled via pfctl's macOS-only ref-counted `-E`/`-X` so we never
  fight another component that also enabled pf. Full Masquerade/SNAT/DNAT.
- **FreeBSD / OpenBSD**: pf, via a plain `gravinet_nat` anchor. Neither ships
  a default wildcard hook the way macOS does, and pf only evaluates an anchor
  that's referenced from the *active* main ruleset — so gravinet appends a
  clearly-marked, idempotent `nat-anchor`/`rdr-anchor` block to `/etc/pf.conf`
  the first time it runs (the same treatment the FreeBSD Handbook's own
  ftp-proxy(8) setup requires, for the same reason) and reloads. It tracks
  whether *it* was the one that enabled pf, so Clear only disables pf if
  nothing else needed it on; the pf.conf hook itself is left in place
  permanently once added, since it's inert when the anchor is empty and
  removing it automatically would risk breaking something that came to
  depend on pf being active in the meantime. Full Masquerade/SNAT/DNAT.
- **Windows**: WinNAT, via the NetNat PowerShell module (`New-NetNat`,
  the same engine behind Docker Desktop's and Hyper-V's NAT switches). WinNAT
  is a narrower model than the others — it's fundamentally single-address
  PAT keyed by an internal prefix, with no oifname-style interface match and
  no notion of "SNAT to a fixed, arbitrary address" — so gravinet's Windows
  backend fully supports Masquerade, but reports SNAT-to-fixed-address, v6,
  and address-only DNAT (WinNAT's only redirect primitive,
  `Add-NetNatStaticMapping`, requires an explicit protocol+port pair per
  mapping, and our rule model is address-only) back as unsupported rather
  than silently dropping or half-applying them.

Everything else (the overlay itself, routing, firewall, QoS, bandwidth
limiting) already worked the same on all five platforms.

## Managed clustering

Two independent, off-by-default flags, not one: `managed` (config `managed:
true`, `gravinet managed on`, or the header toggle) controls whether *this*
node can be managed by others; `manager` (config `manager: true`, `gravinet
manager on`, the other header toggle) controls whether *this* node can manage
other Managed peers. The four combinations are all meaningful — Managed+Manager
is the old single-flag behavior, Manager-only is a bastion/admin-console node
(can drive the fleet, can't itself be reached without a login), Managed-only is
manageable but can't manage anyone, and neither is fully local-admin-only. Being
Managed no longer implies anything about being able to manage others, and vice
versa — that used to be bundled into one flag before the split.

Both advertisements ride the existing handshake identity — `Managed`, `Manager`,
and the web-admin `WebPort` are optional trailing fields next to the
hostname/Name fields (`Managed`/`Manager` share one trailing byte, one bit
each), so older peers simply omit them (all nodes must still rebuild to
exchange them). A direct neighbour learns these from the handshake; for peers
that are *not* directly connected (reached via a relay, or simply not yet
meshed full), the same `managed`/`manager`/`webPort` also ride the node-list
**gossip** — `flagManaged` and `flagManager` bits (the latter carries no extra
data, unlike `flagManaged`'s trailing port) on each gossiped peer entry — so
both propagate mesh-wide rather than only to direct neighbours. (Relying on
full connectivity alone was a bug: a relayed or multi-hop managed peer showed
up with no web port and so couldn't be managed.) For a direct neighbour the
handshake stays authoritative; gossip only fills in these fields for peers we
hold no session to. The node registry records all of it alongside `lastSeen`.

Toggling either flag live (`SetManaged`/`SetManager`, called from the CLI, the
web toggle, or config reload) pushes the new value immediately to every
already-connected peer via a dedicated `ctrlClusterNotify` message — a small
gap the handshake/gossip design above otherwise leaves open: gossip explicitly
does *not* override a directly-connected peer's registry entry (see the
previous paragraph), so without this push, a peer already meshed before the
toggle would keep whatever was true at its *last* handshake until something
forced a reconnect (restart, roam, timeout) — which could be indefinitely on a
long-lived session. That was invisible for a single always-on flag set before
anyone ever connected, but became an immediately-reproducible bug once Manager
requires the *target* peer to have fresh, live knowledge of the caller's flag:
flip Manager on and try to use it right away against an already-connected
peer, and the target's `IsManagerAddr` would still say no. `ctrlClusterNotify`
carries the same `[mflag:1][webPort:2][tcpPort:2]` shape as the handshake's own
trailing field, and the receiving side (`onClusterNotify`) updates both the
live session and the registry entry directly — no "already connected" gate
needed, since (unlike third-party gossip) this arrives from the node it
describes, over its own already-authenticated session.

`Engine.ManagedPeers(maxAge)` returns peers that advertised managed and were
heard within the window, deduped, self excluded — this is the "can be managed"
listing and is unaffected by Manager; it's what the header dropdown is filled
from. The web admin exposes this at `/api/cluster`, on a 6s timer — a peer not
heard from within the 90s TTL simply stops appearing. Selecting a peer sets a
target, and the GUI routes its `/api/*` calls through `/api/proxy?node=…`,
which forwards them to that peer's web admin over the overlay. The proxy target
is constrained with `OverlayContains` — it must be a structural overlay address
(inside a configured overlay subnet, never loopback/link-local/multicast) — so a
malicious peer can't advertise e.g. `127.0.0.1`, a LAN host, or a cloud-metadata
address to turn the proxy into an SSRF; only `/api/` paths are forwarded. The
local view stays put — the cluster, login, and proxy endpoints are never
themselves proxied. `/api/managed` and `/api/manager` join that exclusion list
by explicit design, not just convention: unlike every other per-host setting on
the Settings page (NAT, QoS, firewall, route-advertisement interval, ...),
Managed/Manager mode are never remotely configurable — they always read and
write *this* node's own status regardless of which peer is selected. An
earlier version let them follow the proxy like anything else, which produced a
worse bug than the one it was meant to avoid: toggling what looked like a
selected peer's Managed mode silently changed the operator's own node instead,
with no indication anything had gone to the wrong place. `handleProxy` rejects
both paths outright (`403`, checked against the path with any query string
stripped) — enforced server-side since the frontend simply never routing them
through the proxy is a client convention, not a trust boundary.

The remote authorizes the hop by **source address plus caller identity**: a
managed node's auth middleware accepts a request whose connecting IP is a
structural overlay address (`OverlayContains`: inside an overlay subnet) *and*
resolves, via the node registry, to a peer currently advertising Manager mode
(`IsManagerAddr`) — with no separate login. Crucially `OverlayContains` is
*not* a registry lookup — the registry is filled from untrusted peer
advertisements, so trusting "any address some peer claims as overlay" let a
malicious peer poison it with an attacker's underlay IP and bypass login on a
managed node; the subnet check is structural and cannot be poisoned.
`IsManagerAddr`, by contrast, *is* a registry lookup (same trust model as
`ManagedPeers`/`IsOverlayAddr`) — that's an accepted, bounded trade-off, not an
oversight: `OverlayContains` already gated the address to something
structurally real first, so at worst a malicious gossip entry mislabels an
address inside the subnet as belonging to a manager; it can't manufacture a
connection that genuinely arrives from an address whose real owner isn't the
one making the request, since that requires an actual live mesh session (the
PSK) for that address. A direct neighbour's own handshake is authoritative here
and can't be overridden by a third party's gossip, so the residual gap only
exists at all for a peer known solely through relay/gossip. Reaching a node on
a real overlay address already required the mesh PSK, which is the cluster's
trust boundary; underlay callers still log in normally. This is the deliberate
(and, since the Manager/Managed split, considerably narrowed) trade-off of
managed mode — any *Manager* mesh peer can manage a managed node, not any mesh
peer — which is why both flags are opt-in and off by default. The cross-host
overlay hop is verified here by unit/logic tests and review; a full multi-host
overlay management run isn't exercised on this single-container build (same
constraint as IPv6 and the non-Linux backends).

For the proxy to actually reach a peer, that peer's web admin has to be
**listening on its overlay address**. The primary listener is almost always
bound to loopback (the safe default, `127.0.0.1:8443`), which a remote peer
cannot reach — so a loopback-bound node returned connection-refused to the proxy
and the GUI showed "no networks" for every peer. The daemon therefore binds an
**additional** listener on each network's overlay address (same port, same
handler and auth) once that address is assigned, via `Server.EnsureListener`,
driven by a short ticker because overlay addresses are assigned dynamically by
DAD. This makes the node reachable for cluster management over the overlay
*without* exposing the underlay (the listener is bound to the overlay IP, not
`0.0.0.0`). It is skipped when the primary bind is already a wildcard (which
covers the overlay anyway), and is idempotent. One subtlety that also broke this
path: a dual-stack listener reports an inbound IPv4 connection's source as a
4-in-6 mapped address (`::ffff:a.b.c.d`), which `netip.Prefix.Contains` will not
match against an IPv4 subnet, so `OverlayContains` now unmaps before testing
containment.

## Web admin (step 13)

A small HTTPS server (`internal/webadmin`) exposing a single-page UI and a JSON
API — for the live mesh (peers, bans, firewall) and the full config (networks,
routes, NAT, QoS, bandwidth) — so an operator has the **entire CLI surface from a
browser**. Config edits go through the same shared `*config.Config` ops the CLI
uses (`internal/config/ops.go`): each web handler (`edit.go`) loads the config
file, applies the op, validates, saves, and triggers the live reload.

- **Authentication** is pluggable behind an `Authenticator` interface, and the
  admin UI is enabled by default (bound to `127.0.0.1:8443`). System auth is the
  default mode: **PAM** on Linux/macOS (`auth_pam.go`, cgo + libpam, via
  `pam_start`/`pam_authenticate`/`pam_acct_mgmt` with an all-C conversation
  callback) and **`LogonUser`** on Windows (`auth_windows.go`, pure `syscall` —
  no cgo). Both accept an optional `allow_users` allow-list. A `local` mode also
  exists, verifying usernames against PBKDF2-HMAC-SHA256 hashes in config
  (hand-rolled on stdlib `crypto/hmac`+`sha256`; `gravinet genpass` mints one).
  The build split: the installers build natively, so Linux/macOS get real PAM
  (cgo) and Windows gets real `LogonUser` (no cgo needed). The cross-compiled
  `CGO_ENABLED=0` release matrix keeps real auth on Windows (syscall) but on
  Linux/macOS the PAM file (`auth_nopam.go`) reports unavailable, so those static
  binaries log a warning and fall back to local auth. PAM auth uses the
  `gravinet` service; the installers drop an `/etc/pam.d/gravinet` (distro common
  stack on Linux, `pam_opendirectory` on macOS).
- **Login throttling** reuses the brute-force `ratelimit.Throttle`: three failed
  logins within a minute lock the client IP out for fifteen minutes (configurable
  via `login_ban`), matching the join-throttle policy.
- **Sessions** are server-side; login sets an HttpOnly, Secure, SameSite=Strict
  cookie carrying a 256-bit random token with an 8-hour TTL. The API endpoints
  are gated by a session-check middleware.
- **Sessions persist across restarts.** A login issues a *stateless* signed
  cookie — `base64(user|expiry).base64(HMAC-SHA256)` under a 32-byte key persisted
  next to the cert (`webadmin-session.key`, 0600). Validity is proven by the
  signature, not a server-side map, so the cookie keeps working after the daemon
  restarts (previously the in-memory session table was lost and every restart
  bounced the admin back to the login screen). Logout clears the cookie and adds
  the token to a small in-memory denylist for the rest of its life; that denylist
  is the only piece not restart-durable, which at worst lets a *captured* logged-out
  token be replayed after a restart, bounded by the 8h expiry.
- **TLS** is on by default; a configured cert/key is used if present, otherwise
  a self-signed P-256 certificate is generated in memory (so a fresh install is
  HTTPS immediately, with the usual browser warning for the self-signed cert).
- **Editing.** Live-applied surfaces — bans, force-unban, firewall, and NAT/QoS/
  bandwidth — take effect immediately (firewall/bans via the engine API; NAT/QoS/
  bandwidth by writing the config file and calling the same reload the control
  socket uses, which re-applies them through atomic pointer swaps). All edits are
  written back to the config file so they survive a restart. Bringing networks
  up or down — add, delete, enable, disable, join — now also applies live: the
  reload hook diffs the new config against the running set and starts or stops
  just those networks, no restart. Only a subnet/addressing change or an underlay
  change (primary/extra ports, worker count, MTU) returns
  `restart:true`; the UI then offers a **Restart now** button (`/api/restart`,
  Linux `systemctl restart`) that replies first and restarts a beat later. To
  reload cleanly afterwards the page captures the daemon's per-process boot id
  from the unauthenticated `/api/ping` endpoint, then polls it and reloads when
  the id changes — detecting the new process positively, so it works even when the
  restart is faster than the poll interval (a bounded cap is the only fallback).

## Status
Steps 1–13 complete. The PBKDF2 KDF and local authenticator are unit-tested
(correct/wrong/unknown-user), as is the login throttle (three failures then a
lockout that blocks even correct credentials), the full session lifecycle
(login → cookie → authed API → ban reaches the backend → logout invalidates),
and self-signed cert generation; a live daemon was driven over real HTTPS
(unauth 401, bad login 401, good login + cookie, authed status, served UI). The
tree passes `-race` and cross-compiles to linux/windows/darwin on amd64/arm64
(the Windows auth seam compiles via build tags). (Container IPv6 stays disabled,
validated by review + v4-equivalent tests; PAM/Windows auth and the TUN
backends are the genuinely platform/cgo-bound seams.)

## Config management & persistence

Seeds and ports: the daemon binds its primary port (65432). If that *bind* fails
locally (port already in use, or insufficient privilege) it fails over to the
next of a set of well-known ports — 443, 4500, 3478, 1194, 500, 53 — settling on
the first that comes up. A seed peer entered without a port is expanded at load
time across that same standard set (`resolveSeeds`), so bootstrap is tried on
every port the remote might have landed on; an explicit `host:port` pins one.

To be reachable *through* a restrictive firewall (which blocks inbound 65432 but
permits, say, 443), set `extra_listen_ports` — these bind concurrently with the
primary rather than as failover, so the node actually answers on them. Binding is
best-effort: a privileged or already-taken port is logged and skipped, not fatal.
Crucially, a reply is sent back out the socket the peer arrived on (tracked per
remote in the transport), so a peer that dialed `:443` sees the answer come from
`:443` — without that, a stateful firewall or NAT would drop a reply originating
from `:65432`. Replies still default to the primary port for peers reached the
ordinary way. The dialer already fans portless seeds across the standard set, so
this is the listen-side half that makes that fan-out land.

`network join` stores the bare host and lets the daemon do the expansion, and
(like the other `network` commands) is applied live by the reload hook, which
brings the new overlay up without a restart.

Bootstrap resilience — the peer cache: `network join` records a single seed, which
is fragile across restarts (if that one host is down, there's nothing to bootstrap
from). So each network keeps an auto-managed `peer_cache`: the underlay endpoints
of peers seen in the last session, persisted to config (deduped, capped at 32,
freshest first) whenever the connected-peer set changes. At startup the daemon
bootstraps from the configured seeds *and* the cache combined, and `initLoop`
dials them all — first to answer wins — so as long as any recently-seen peer is
reachable, the node rejoins even if its original seed is gone. The cached entries
are underlay endpoints (the address a peer's packets came from), so they're literal
addresses, immune to the hosts-file trap below, and the overlay-subnet guard
rejects any that aren't.

Seeds must be underlay addresses, never overlay ones — and there's a trap worth
calling out because the hostname→hosts sync can spring it. That sync writes each
connected peer's hostname mapped to its *overlay* (tunnel) IP into the OS hosts
file. If you then use that same hostname as a seed, on the next boot it resolves
(via `/etc/hosts`) to the tunnel IP, which is only reachable through the very
tunnel you're trying to establish — the node deadlocks offline. Two guards prevent
this. First, the managed hosts block is cleared at startup, before seeds are
resolved, so a stale mapping from a previous run can't hijack resolution (it's
repopulated as peers reconnect). Second, `resolveSeeds` drops any seed that
resolves into one of the node's own overlay subnets and logs why — a bootstrap
peer is by definition an underlay address, so an overlay result is always a stale
or misconfigured entry. The upshot: hostnames, FQDNs, and literal v4/v6 addresses
are all still valid seeds; the protection is the overlay-subnet check, not a
restriction on the address form.

Joining by id, and learned identity: a network's id is its on-the-wire identity —
two nodes only peer when their ids match exactly — so you join an existing mesh by
that id, not by a name you pick locally: `network join <id> key <KEY> peer <host>`.
The id is shown by `network list` on any node already in the network. The joiner
starts bare: it has the id, a key, and a seed, but no name or subnet. Both are
*learned from the network*. Every handshake advertises the sender's subnet and
name (the subnet advert already existed so a joiner can self-assign an address;
the name rides alongside it). On first contact the joiner adopts whichever of
these it is still missing (`absorbIdentity`) and the persist hook writes them back
to config, so after the first peering `network list` shows the real name and
subnet and the node self-assigns an address inside the learned subnet. Because the
name now propagates this way, a joined network needs neither set in advance;
`Validate` permits a subnet-less network as long as it has a seed to learn from.
A local `network rename` still overrides the label without touching the id.

Address stability across restarts: an overlay address may be set statically in
config (`address4`/`address6`) or, if left blank, self-assigned at runtime — the
node picks a random host in the subnet and runs duplicate-address detection
(`dadPick` + `dadProbe`). A self-assigned address used to live only in memory, so
a restart re-rolled it to a new random host. Now, when DAD succeeds, the same
persist hook writes the chosen address back to config (`address4`/`address6`, with
the subnet's mask) — but only when no address is already pinned there. On the next
start the address is applied statically and DAD doesn't run, so a node keeps its
mesh IP across restarts. This is the same hook and write path that persists learned
name/subnet and live firewall edits, which is also why it's installed before the
engine starts (so a lone node's first assignment, which happens during start, is
captured). One caveat: if you later change a network's subnet, clear the now-stale
`address4`/`address6` so the node re-assigns inside the new range.

The config file is the single source of truth, and there are two ways changes
reach it:

- **CLI (`network`/`route`/`nat`/`qos`/`bandwidth`/`fw`/`list`/`status`).** Each
  config command loads the file, mutates the in-memory struct, runs
  `Config.Validate`, and writes it back with `SaveTo`. Because the edit *is* a
  file write, these changes are durable by construction. The CLI then opens the
  control socket and sends `{"cmd":"reload"}` (best effort); if no daemon is
  listening it simply reports that the change applies on next start. `list` prints
  the whole config; `status` is the only runtime-only view (live peers/bans/routes
  over the control socket). `ban`/`unban` stay runtime as well — bans are
  distributed, TTL'd state, not config. The control socket's `-net` accepts a
  network **name or hex id** (the daemon installs a name→id resolver that reads
  the live config), so `ban`/`unban`/`fw` select a network the same way the config
  commands do. Every per-network surface — bans, routes, firewall, NAT, QoS,
  bandwidth, and the 8 join-key slots — is scoped to exactly one network on both
  the CLI (`key list|generate|show|set|enable|disable|delete`) and the web GUI
  (a **Keys** section). `-init` writes a base config with **no networks**; one
  exists only once you `network add` it (which mints its first key). Key changes
  (generate, import, enable, disable, delete, relabel, **set an expiry**) apply
  **live** — the engine holds the key set in an atomic pointer that the reload
  hook swaps, so adding or rotating keys needs no restart. When a key leaves the
  set (disabled, deleted, or expired), the engine tears down sessions that
  authenticated with it (each session records its key id), freeing the endpoint
  so the peers re-handshake on a still-valid key automatically. Each slot can
  carry an RFC3339 **expiry**; past it the key stops authenticating, and a 30s
  daemon sweep notices the boundary crossing and applies it live the same way an
  admin edit would. Sessions that never re-key keep running until they do.
- **Live edits (web UI or control socket).** Firewall mutations go straight to
  the engine's atomic rulebase. The engine now carries an optional *persist hook*
  (`SetPersistHook`); after every firewall add/delete/move/cut/paste it calls the
  hook with the network id. The daemon installs a hook that re-loads the config
  file, replaces that network's `firewall.rules` with the engine's current
  rulebase, and `SaveTo`s it (serialized by a mutex, last-write-wins). So a rule
  toggled in the browser is on disk before the next packet.

**Reload semantics.** The control `reload` command re-reads the file and applies
everything that can change without recreating interfaces or sessions:
firewall rules, NAT, QoS, and bandwidth throttles. `Engine.ReloadRuntime` does
this per network by rebuilding each component from a fresh spec and swapping it
in. The three runtime components the data path touches per packet — the egress
shaper (which also carries the QoS classifier), the ingress policer, and the NAT
table — are held in `netState` as `atomic.Pointer`s, so the tx/rx goroutines load
them locklessly while a reload stores a replacement. This means NAT, QoS, and
both throttle directions can be turned **on or off live**, not just adjusted. The
up-throttle shaper owns a goroutine, so reload starts the new shaper, atomically
swaps the pointer, then retires the old one; a reload may drop a handful of
in-flight shaped packets as the old shaper is closed (TCP recovers, and it is
imperceptible). NAT masquerade rules scoped to an interface (`nat add eth0`)
resolve that interface's IPv4 as the SNAT address at (re)build time.

Firewall *rules* and the firewall on/off flag both reload live (the reload hook
calls `setEnabled` and swaps the rule set atomically). Adding, removing, enabling,
and disabling networks — and key changes — also apply live now: the engine holds
its network set in a copy-on-write map (`atomic.Pointer`) read lock-free on the
hot path, and each network owns its goroutines via a `done` channel and wait
group, so one network can be torn down without touching the others. The remaining
underlay changes — a network's subnet/addressing, the primary/extra UDP ports,
worker count, or MTU — are saved immediately and applied on the next restart,
since they re-home an interface or rebind sockets shared across all networks.

This was exercised live: a rule added over the control socket appeared in the
config file immediately and survived a kill+restart; and CLI `bandwidth`, `nat`,
and `qos` changes drove `ReloadRuntime` on the running daemon (confirmed in the
log: `nat=true up=6250000 qos=true`), with the `-race` detector clean across the
atomic swaps under concurrent data-path load.

## NAT (step 12)

Stateful address translation on the overlay data path, with connection tracking
so replies reverse automatically. It sits around the firewall: ingress packets
are translated then filtered then delivered; egress packets are filtered then
translated then sent — so the filter always sees the internal address view.

- **SNAT / masquerade** (overlay→underlay, overlay→overlay): rewrite the source
  of matching egress packets to a single address, with port translation (PAT) so
  many internal hosts can share it. A conntrack entry is recorded; replies
  arriving on ingress have their destination restored to the original host.
- **DNAT / port-forward** (underlay→overlay): rewrite the destination of matching
  ingress packets to an internal host; replies leaving on egress have their
  source restored.
- IPv4 + TCP/UDP checksums are recomputed after every rewrite; conntrack entries
  time out after two minutes of silence. NAT is IPv4-only and, like a router's
  masquerade, the *forwarding* of translated traffic onto a non-mesh physical
  interface (true exit-node operation) is a host/OS concern — the daemon does
  the translation and tracking.

## Status
Steps 1–12 complete. Checksum correctness, SNAT and DNAT round-trips, and PAT
port reallocation on collision are unit-tested, and a full masquerade round-trip
is verified across two live engines (a LAN-sourced packet is source-translated,
reaches the peer, and the peer's reply is reverse-translated back to the
original host with valid checksums). The tree passes `-race` and cross-compiles
to linux/windows/darwin on amd64/arm64. (Container IPv6 stays disabled, so v6
paths are validated by review and v4-equivalent tests; NAT itself is IPv4-only.)

## Firewalling (step 11)

A per-network packet filter with a deliberately permissive default: the policy
is **allow**, so an empty rulebase (or a packet matching no rule) passes. You add
rules to *restrict*. The filter is **off by default** and is enabled per network
(an atomic flag, so enabling, disabling, and editing rules all apply live with no
restart — in the web UI, double-click the firewall card's status tag to toggle).
The enabled flag governs only *enforcement*: the rulebase is loaded into the
engine whether or not the firewall is on, so disabling it leaves the rules
intact and visible (the GUI reads the live engine rules) rather than appearing to
delete them, and a config write while disabled can't wipe them. `allow()`
short-circuits to allow-all when the flag is off, so the retained rules simply
aren't applied until it's switched back on.

- **Rules** are evaluated top-to-bottom, first match wins, each rule allow or
  deny. A rule matches on direction (in/out/both), protocol (tcp/udp/icmp),
  source and destination CIDR/host, and source/destination port ranges — reusing
  the QoS L4 parser. The filter runs at a fixed point in the pipeline: egress
  packets are checked (out) as they leave the TUN, ingress packets (in) after
  decryption before they reach the TUN, both on the overlay address view (NAT,
  step 12, will translate at the edges around this stage).
- **Reads are lock-free.** The active ruleset is an immutable snapshot swapped
  atomically on mutation, so the data path never blocks on rule edits.
- **Stateful by default.** The firewall tracks connections automatically: once a
  flow is permitted, return traffic for it is allowed regardless of the rules.
  So rules effectively describe *new* connections — a single `deny in` rule
  blocks unsolicited inbound while replies to connections this node initiated
  still come back, with no per-rule state to configure. Tracking is keyed by a
  direction-independent flow tuple and entries time out (60 s unconfirmed, 300 s
  established); it engages only when the rulebase contains a deny rule, so
  pure-allow or empty rulebases keep the lock-free fast path.
- **Live management** over the control socket / CLI: add (at any position),
  reorder (move by id), delete, and a clipboard for copy / cut / paste. Pasting
  inserts fresh-id copies at a chosen position; copy preserves rule order. Rules
  have stable ids; an initial rulebase can also be set in config.
- **A configurable "always-allowed" allowlist the rulebase can't override.**
  Before the rulebase runs, `allow()` waves through traffic on an exemption list,
  so an operator can't accidentally cut themselves off at the knees with a broad
  deny. The list is **node-global** — one allowlist applies to every network's
  firewall, not a separate list per network — and is **pre-populated with sensible
  defaults**: remote web-admin management (TCP to/from this node's admin port),
  BGP (TCP 179), OSPF (IP protocol 89), and RIP/RIPng (UDP 520/521). It is fully
  editable from the web UI's **Settings → Always allowed** panel (a `+/-` table
  where any cell edits on double-click), the CLI (`gravinet fw exempt
  <list|add|del|reset>`), or the config's top-level `firewall_exempt` array. The
  remote-management entry carries a `mgmt` flag that tracks the live web-admin
  port automatically and is shown in the UI/CLI as the actual port number; other
  entries match a protocol (`tcp/udp/icmp/ospf/<number>/any`) and an optional port
  (against source *or* destination). Edits apply live (the active set is swapped
  atomically on reload, no restart). A nil list means "use the defaults"; an
  explicit empty list means "no exemptions, filter everything" — full control if
  you want it. Separately, the mesh's *own* control plane (route and hostname/peer
  advertisements) never reaches the firewall at all: it travels as encrypted
  control frames, not as overlay IP packets through the TUN (the only two
  `allow()` call sites), so it is structurally unblockable regardless of the
  allowlist.

## Status
Steps 1–11 complete. Rule evaluation (default-allow, first-match, direction /
protocol / port / CIDR matching), automatic stateful tracking (replies to
outbound-initiated flows pass an inbound-deny policy with no per-rule state,
while unsolicited connections are blocked — proven by the same packet being
denied without a prior flow), the full management lifecycle (add/move/copy/paste/cut/delete with order and fresh-id
semantics), and a data-path drop are all tested; the firewall IPC round-trips
through the control socket; and the whole lifecycle is verified live via the
CLI against a running daemon. Any change to the rulebase — add, remove, enable,
disable, or toggling the whole firewall — flushes the connection-tracking table,
so the new rules are re-evaluated against *existing* connections on their next
packet, not just brand-new flows. Without that flush an already-established flow
keeps being allowed from conntrack regardless of a newly added deny rule, which
made live edits look like they needed a service restart (they don't). The tree
passes `-race` and cross-compiles to linux/windows/darwin on amd64/arm64.
(Container IPv6 stays disabled, so v6 paths are validated by review and
v4-equivalent tests.)

## Quality of service (step 10)

QoS prioritises traffic within the egress shaper, so when the link is saturated
the packets that matter go first. It builds directly on the step-9 shaper: the
single queue becomes a set of priority classes.

- **Classification.** Each outbound overlay packet is matched against ordered
  rules — by protocol (tcp/udp/icmp), source/destination port range, and/or
  DSCP marking — and assigned to a priority class (0 = highest). First match
  wins; unmatched traffic falls to a default class. There are five classes by
  default (0–2 above normal, 3 = normal/default, 4 = bulk); the web UI lets you
  add, double-click-edit, or delete rules, including ICMP. DSCP rules let the
  mesh honour markings already set by applications (e.g. EF for voice).
- **Scheduling.** The drainer serves the highest-priority non-empty class first,
  re-selecting after each pacing wait so a newly-arrived high-priority packet
  jumps ahead. All classes share the one rate budget from the throttle; QoS only
  decides ordering, so it has effect only when an up-throttle creates
  contention. Each class has its own queue capacity (independent tail-drop).
- This is strict priority: under sustained saturation a busy high class can
  starve lower ones (weighted/deficit round-robin is a possible later refinement
  if fairness is wanted).

## Status
Steps 1–10 complete. Classification (protocol/port/DSCP → class, with a nil
classifier collapsing to a single FIFO) and strict-priority scheduling
(high-priority traffic drained ahead of low within one rate budget) are
unit-tested; the step-9 shaper, throttle, broadcast, relay, ban, route, mesh,
addressing, and crypto behaviour all still pass, under `-race`, and the tree
cross-compiles to linux/windows/darwin on amd64/arm64. (Container IPv6 stays
disabled, so v6 paths are validated by review and v4-equivalent tests; IPv6
extension headers aren't walked when reading L4 ports for classification.)

## Bandwidth throttling (step 9)

Per-network rate caps, configurable independently for each direction (set up
only, down only, or both):

- **Egress is shaped.** Outbound overlay packets enter a bounded queue, and a
  single drainer goroutine releases them paced to the up-rate by a byte token
  bucket. Shaping (delay) rather than dropping means we smooth our own outbound
  without provoking retransmits; only a sustained overload that fills the queue
  causes tail-drop. Control traffic (gossip, keepalive, bans) bypasses the
  shaper so management never starves.
- **Ingress is policed.** We can't slow a remote sender over UDP, so inbound
  overlay traffic that exceeds the down-rate is dropped by a byte token bucket
  on the receive path — which, for TCP flows, signals the sender to back off.
- Burst defaults to ~250 ms of the down-rate (floored at one maximum-size
  packet). A one-packet burst would shred TCP — every normal micro-burst would
  overflow it and collapse the sender well below the cap — so the policer admits
  realistic bursts while the sustained average still settles at the configured
  rate. Broadcast/multicast stay on the step-8 packet-rate storm control (not
  double-counted by the byte shaper).

## Status
Steps 1–9 complete. The byte token bucket, shaper pacing, and queue tail-drop
are unit-tested deterministically, and an end-to-end test confirms a throttled
node paces a real two-node tunnel to its configured rate (~58 KB/s measured
against a 50 KB/s cap, the small overage being the burst allowance). The whole
tree passes `-race` and cross-compiles to linux/windows/darwin on amd64/arm64.
(Container IPv6 stays disabled, so v6 paths are validated by review and
v4-equivalent tests. Down-throttle is receiver-side policing; true cooperative
down-shaping by the sending peer is a possible later refinement.)

## Broadcast / multicast + storm control (step 8)

The TUN read path classifies each overlay packet's destination:

- **Unicast** routes to the one peer that owns the address (or a redistributed
  route, or a relay).
- **Broadcast** (255.255.255.255 or the overlay subnet's all-ones address) and
  **multicast** (224.0.0.0/4, or IPv6 ff00::/8) are flooded to every connected
  peer. Receivers write the packet to their own interface but never re-flood, so
  there are no loops (full-mesh / relay reach assumed; multi-hop flooding is out
  of scope).
- **Storm control** is a per-network, per-class token bucket (configurable
  packets-per-second and burst). A node that exceeds its broadcast or multicast
  budget drops the excess at the source rather than amplifying a storm.

## Ban lifetime: TTL, refresh, adopt (step 8 refinement)

The original "only the origin can unban" rule wedged a ban if its origin left
the mesh forever. Bans are now soft state:

- Every ban carries an **expiry**; a live origin **refreshes** it (re-floods with
  a bumped expiry well before it lapses). A departed origin's bans simply expire
  — no fragile "is that node offline?" detection, which would misfire on network
  partitions. Refresh doubles as the flood version: a record is accepted if it's
  new or extends the expiry, and ignored otherwise, so refreshes converge
  without looping.
- Bans are keyed by (origin, target), so multiple admins can independently ban
  the same node (**union** semantics); unban clears only your own record, and a
  node that still wants someone out simply re-issues (**adopt**).
- A local **force-unban** break-glass clears every origin's ban on a target and
  floods a wildcard removal. If an origin is actually still alive it re-asserts
  on its next refresh — so force-unban only sticks for genuinely departed
  origins, which is exactly the intent.

## Status
Steps 1–8 complete. Broadcast/multicast delivery and storm-control limiting are
verified over real UDP sockets (a source capped at burst N delivers exactly N of
a rapid burst); the ban TTL/expiry, refresh-versioning, union/adopt, and
force-unban paths are covered by deterministic tests; and force-unban is
verified live across three daemons (a non-origin clears a departed origin's ban
mesh-wide). The whole tree passes `-race` and cross-compiles to
linux/windows/darwin on amd64/arm64. (Container IPv6 stays disabled, so v6 paths
are validated by review and v4-equivalent tests; relaying and broadcast reach
assume a full/relayed mesh.)
