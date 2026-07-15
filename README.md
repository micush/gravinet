# [gravinet]

*Created by micush.*

A single-binary, full-mesh, encrypted overlay VPN. Pure Go, stdlib-only core,
`CGO_ENABLED=0` static build. Targets Linux, Windows, macOS, FreeBSD, OpenBSD
(kernel NAT is Linux-only; everything else — the overlay, routing, firewall,
QoS, bandwidth limiting, and per-network DNS forwarding — works across all of
them; on OpenBSD, DNS forwarding needs unbound as the system resolver, which
the installer sets up by default — pass --no-unbound to skip it).

See `docs/ARCHITECTURE.md` for the full design and the prioritized roadmap.

## Features

A self-forming, self-addressing, full-mesh encrypted overlay with a distributed
control plane, relay fallback, and broadcast/multicast:

- **Config / Crypto / Protocol / TUN / Transport** — hot-reloadable config,
  AES-256-GCM + X25519 + HKDF, keyID-by-identity matching, replay protection,
  raw-ioctl Linux TUN (jumbo MTU, v4/v6, poller-managed I/O), UDP underlay with
  port fallback and a cores-1 REUSEPORT worker pool. Both the UDP underlay and
  the TCP/TLS fallback can also listen on additional ports at the same time
  (`extra_listen_ports`/`extra_tcp_listen_ports`, or Settings → Underlay in
  the web UI) — best-effort, applied live, so a peer behind a restrictive
  firewall can reach this node on whichever well-known port gets through.
  Extra ports are advertised to peers too (handshake and gossip, alongside
  the existing per-peer TCP fallback port advertisement), so the mesh
  actually tries them: TCP dials every advertised candidate for a peer in
  parallel, and extra UDP ports join the same seed pool ordinary bootstrap
  addresses use.
- **Handshake & sessions** — PSK-authenticated X25519, O(1) index demux,
  NAT-roaming endpoints, key-slot cycling, brute-force banning.
- **Mesh formation** — peer-list gossip (auto full-mesh from one seed) and
  PING/PONG keepalives; dead-peer pruning; unreachable-seed cooldown.
- **Overlay addressing** — subnet advertised in the handshake; random pick +
  duplicate-address detection before claiming and announcing an address.
- **Distributed control plane** — `ban`/`unban`/`list` over a control socket.
  Bans flood with the origin recorded and are origin-only to lift; they now
  carry a TTL and are refreshed by a live origin, so a departed origin's bans
  self-heal. Multiple admins can ban the same node (union), and a `-force`
  break-glass clears a departed origin's ban. Hostnames sync into the OS hosts
  file; routes redistribute with longest-prefix matching and reject rules.
- **Relaying** — when two nodes can't connect directly, traffic flows through a
  willing third node, end-to-end encrypted (the relay sees only ciphertext).
- **Broadcast / multicast** — overlay broadcast and multicast frames flood to
  the mesh, rate-limited by a per-class token bucket (storm control).
- **Bandwidth throttling** — per-network rate caps in either direction: egress
  is shaped (queued and paced to the up-rate), ingress is policed (inbound over
  the down-rate is dropped). Control traffic is exempt.
- **Quality of service** — outbound traffic is classified (by protocol, port, or
  DSCP) into priority classes and scheduled strict-priority within the throttled
  link, so important traffic goes first under contention.
- **Firewalling** — a per-network ordered rulebase with a default-allow policy:
  add, reorder, delete, and cut/copy/paste rules live over the control socket;
  rules match on direction, protocol, address, and port. Stateful by default —
  rules describe new connections and replies to flows you initiated come back
  automatically (a lone `deny in` blocks unsolicited inbound). Reads are
  lock-free. Firewall direction is relative to the tunnel (mesh→TUN /
  TUN→mesh), so traffic this node forwards between the mesh and another
  interface is covered by the same rules as its own traffic — there's no
  separate "forwarded" rule class. A **node-global allow list**
  (management, BGP, OSPF, RIP by default) sits outside every network's
  rulebase entirely, so a broad `deny` can't lock out management or routing
  protocols — `fw exempt` on the CLI, or the **Allow List** tab next to
  Rules in the web UI.
- **IP forwarding** — enables IPv4 and IPv6 forwarding on the host at
  startup by default (`ip_forwarding: false` to opt out), the on-ramp that
  lets redistributed routes and NAT actually carry traffic to and from
  other interfaces. Best-effort per family — a missing IPv6 knob or
  insufficient privilege doesn't block the other — and restored to its
  prior value on clean shutdown.
- **NAT** — stateful source NAT/masquerade (with port translation) and
  destination NAT/port-forward on the overlay data path, with connection
  tracking that reverse-translates replies; IPv4 + TCP/UDP checksums recomputed.
- **Web admin** — an HTTPS UI + JSON API over the running engine (peers, bans,
  routes, firewall) with session login and a 3-fails/minute → 15-minute lockout.
  **Enabled by default** on `127.0.0.1:8443` with self-signed TLS. Authenticates
  against real system accounts: **PAM** on Linux/macOS/FreeBSD, **`login_passwd`**
  (BSD auth) on OpenBSD, and **`LogonUser`** on
  Windows (optionally restricted to an `allow_users` list), or PBKDF2 local users
  (`gravinet genpass`) when system auth isn't compiled in.
- **Runs as a service** — generates a `Type=notify` systemd unit (with
  sd_notify), a launchd plist, or `sc.exe` commands via `gravinet service`, and
  runs under the Windows SCM. macOS (`utun`) and Windows (Wintun, with the
  signed driver DLL embedded into the binary) TUN backends
  join the Linux one.
- **CLI/daemon** — `run`, `genkey`, `genpass`, and a structured config surface:
  `network`, `route`, `seed`, `key`, `nat`, `qos`, `bandwidth`, `host`, `fw`
  (including `fw exempt` for the global allow list), `ban`/`unban`,
  `managed`/`manager`, `list` (whole config), `status` (live peers/bans/routes),
  and `service <print|install|uninstall>`. Config commands edit the config file and
  ask a running daemon to reload — so **every change is saved** and survives a
  restart (see "Managing the config" below).

Tested across the tree and under `-race`, including: end-to-end relay through a
blocked underlay, broadcast delivery + storm limiting over real UDP, the ban
TTL/refresh/adopt/force-unban state machine (plus a live three-daemon
force-unban), bandwidth shaping, QoS priority scheduling, the firewall (full
lifecycle + a data-path drop + automatic stateful return traffic), NAT (a live
masquerade round-trip), the web admin (a live HTTPS login + authed API), the
service layer (sd_notify round-trip, unit generation, graceful SIGTERM), and a
hardening pass — native fuzzing of every wire decoder and the `OnPacket` entry
point (no panics over hundreds of thousands of executions), AEAD tamper/replay
rejection, and the brute-force join-throttle ban. `scripts/build-release.sh`
cross-compiles the full static matrix (linux/windows/darwin/freebsd across
amd64/arm64/arm) with SHA-256 checksums.

## Build

```sh
# Default build — cgo on, so the web admin's PAM login works (Linux/macOS):
CGO_ENABLED=1 go build -o gravinet ./cmd/gravinet
```

On Linux this needs the PAM headers (`libpam0g-dev` on Debian/Ubuntu,
`pam-devel` on RHEL/SUSE) and a C compiler; macOS has them in the SDK. **Build
with `CGO_ENABLED=1`** unless you have a reason not to: a `CGO_ENABLED=0` binary
is fully functional but has no PAM, so the web admin falls back to local PBKDF2
users (`gravinet genpass`) instead of your system login. Windows web-admin auth
uses `LogonUser` and needs no cgo; OpenBSD likewise authenticates system
accounts without cgo, via `login_passwd(8)` (BSD auth) — so its static build
keeps system login.

Cross-compile (pure-Go core; cgo can't cross-link libpam, so these are the
no-PAM, local-auth variant):

```sh
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o gravinet.exe ./cmd/gravinet
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o gravinet      ./cmd/gravinet
```

### Release builds (all targets, dependencies bundled)

```sh
./scripts/build-all.sh           # fetch+verify Wintun, vet, test, build every target
VERSION=1.0.0 ./scripts/build-all.sh
```

One command produces a self-contained binary for linux (amd64/arm64/arm),
windows (amd64/arm64), darwin (amd64/arm64), freebsd/amd64, and
openbsd/amd64/arm64, plus `SHA256SUMS`. The binary for the **host** platform is
built with cgo (PAM enabled, tagged `(PAM)`); cross-compiled targets are static
and use local web-auth. OpenBSD is the exception among the static targets: it
has no PAM, but its web admin still authenticates system accounts — by shelling
out to `login_passwd(8)` (BSD auth), which needs no cgo — so no separate
system-login build is required for it. It downloads and checksum-verifies the signed Wintun driver and
embeds it into the Windows binaries (the only non-Go runtime dependency). Use a
prefetched copy with `WINTUN_DIR=/path/to/wintun/bin`, require it with
`REQUIRE_WINTUN=1`, or skip the driver to build with a side-by-side `wintun.dll`
fallback. The Windows kernel driver still installs at runtime — that's a Windows
platform requirement, not a packaging one.

## Install

Each release bundle includes platform installers that place the binary, scaffold
a config, register the daemon (systemd / launchd / Windows service), and **start
it**. Re-running an installer upgrades in place: it stops the running daemon,
replaces the binary, and restarts it.

When no prebuilt binary is present (e.g. installing from the source tarball),
each installer **builds from source**: it installs a Go toolchain if the host
lacks one, builds the binary, and installs it. On Windows it also fetches the
signed Wintun driver to ship beside the executable, and adds an inbound
Windows Defender Firewall rule scoped to the gravinet binary — a Windows
service has no desktop to show Firewall's "allow this app" prompt on, so
without that rule peers would have no way to reach this node even though the
daemon and TUN interfaces come up looking perfectly healthy.

```sh
# Linux (systemd) — builds from source if no prebuilt binary is present,
# installing a Go toolchain first when the host lacks one
sudo ./install-linux.sh

# macOS (launchd) — same build-from-source behavior
sudo ./install-macos.sh

# FreeBSD (rc.d) — same build-from-source behavior; the base system already
# ships a C compiler and PAM headers, so (unlike macOS) there's no separate
# SDK download needed to get a PAM-enabled build
sudo ./install-freebsd.sh

# OpenBSD (rc.d via rcctl) — same build-from-source behavior. OpenBSD has no
# PAM, so the build is CGO_ENABLED=0; the web admin logs in system accounts
# via login_passwd(8) (BSD auth, no cgo). auth_mode "local" is available too.
doas ./install-openbsd.sh
```

On Windows, the easiest path is `install-windows.bat`: just double-click it. No
PowerShell prompt to open yourself, no execution-policy fiddling — it
relaunches itself elevated if needed, sets the execution policy to Bypass for
just that one process, and runs `install-windows.ps1`.

```
# Windows — double-click, or run from any (non-admin) cmd/PowerShell prompt.
# Installs Go + Wintun and builds from source when no prebuilt binary is
# present. Arguments pass straight through to install-windows.ps1.
.\install-windows.bat
.\install-windows.bat -NoStart -NoNpcap
```

```powershell
# Or drive install-windows.ps1 directly from an elevated PowerShell — the
# same install logic install-windows.bat calls under the hood.
.\install-windows.ps1
```

Cutting a release yourself (all platform binaries, one version-stamped batch
in `dist/`): `VERSION=1.2.3 ./scripts/release.sh`.

Pass `--no-start` (PowerShell: `-NoStart`) to install without starting. To remove
gravinet, either re-run the installer with `--uninstall` (`-Uninstall`) or use the
dedicated uninstaller for your platform:

```sh
sudo ./uninstall-linux.sh            # or uninstall-macos.sh, uninstall-freebsd.sh, or (doas) uninstall-openbsd.sh
sudo ./uninstall-linux.sh --purge    # also delete /etc/gravinet (freebsd: /usr/local/etc/gravinet)
```
```
.\uninstall-windows.bat              # double-click, or run from any (non-admin) prompt
.\uninstall-windows.bat -Purge       # also deletes %ProgramData%\gravinet
```
```powershell
.\uninstall-windows.ps1              # the same uninstall-windows.bat calls under the hood
```

The uninstallers stop and remove the service, binary, PAM file (and the Wintun
DLL on Windows), leaving your config in place unless you pass `--purge`/`-Purge`.
The raw daemon definitions ship alongside as `gravinet.service`,
`com.gravinet.daemon.plist`, and `windows-service.txt`. The freshly scaffolded
config is runnable, so the service comes up immediately; to join a mesh, generate
keys (`gravinet genkey`), edit the config, and restart the service.

## Web admin

The admin UI is **on by default** at `https://127.0.0.1:8443` (self-signed TLS,
so accept the browser warning once — the cert is persisted next to the config and
reused across restarts, so you won't be re-prompted). Log in with a **system
account**: a PAM-accepted
user on Linux/macOS/FreeBSD, a `login_passwd`/BSD-auth account on OpenBSD, or a
Windows account on Windows. No separate admin password
to create — it's your OS login.

The UI has a left sidebar grouped into **Mesh** (Networks, Keys, Seeds, Peers,
Bans), **Traffic** (Routes, Firewall, NAT, QoS, Bandwidth), **Naming** (DNS,
Hosts), and **Monitor** (live metrics, mesh peer detail, packet capture,
speedtest, latency, the live kernel route table, hosts file, DNS state, and
logs), and **Info** (this README, the license, and build/host details) — plus
Settings and Sign out pinned at the bottom, and a light/dark
theme toggle in the top bar. A **global search box** next to the node picker
searches everything — every route, host, rule, key, seed, peer, ban, and
setting, across every network — and clicking a result jumps straight to it,
scrolled into view and briefly highlighted. **Every section is editable** —
anything you can do from the CLI you can do
here: create/join/enable/disable/delete networks (with your own subnets),
redistribute/reject/remove routes, add/remove/toggle NAT, QoS, and firewall
rules, ban/unban peers, and set bandwidth limits. NAT, QoS, bandwidth, and
firewall changes apply live; structural changes (networks, routes, addressing)
save immediately and show a **Restart now** button to bring them into effect.

For a headless/remote host it binds to localhost only, so tunnel in:

```sh
ssh -L 8443:127.0.0.1:8443 user@your-server
# then open https://localhost:8443
```

Restrict which system users may log in by listing them under
`web_admin.allow_users` (empty = any account the system accepts). To expose it on
the network instead of via a tunnel, set `web_admin.listen` to `0.0.0.0:8443`
(then firewall it and supply a real cert in `tls_cert`/`tls_key`). System auth
needs the binary built with it: the installers do this (PAM via cgo on
Linux/macOS, `LogonUser` on Windows). If you run a plain cross-compiled
`CGO_ENABLED=0` Linux/macOS binary, PAM isn't present and the UI falls back to
local PBKDF2 users — create one with `gravinet genpass`, set `auth_mode` to
`local`, and add it to `web_admin.users`.

**Can't log in?** Check the daemon's startup log. The line `webadmin: listening …
(auth=pam)` means PAM is active; `(auth=local)` means PAM wasn't compiled into
this binary, so system logins can't work — the log will say so explicitly and
tell you to reinstall from source (so cgo builds in PAM) or switch to a local
user. The other common cause is a missing `/etc/pam.d/gravinet` service file,
which the daemon now warns about at startup with the exact command to create it.
The installers verify the binary actually links libpam and rebuild a PAM-enabled
one when needed, warning loudly if they can't.

## Quick start

For the full walkthrough — installing, then almost everything else from the
web UI (creating and joining networks, DNS, routes, clustering, keys,
firewall, NAT, QoS, shaping, seeds, peers, bans, monitoring) — open the web
admin and go to **Info → Getting Started**. This is the bare CLI path:

```sh
gravinet run -config ./config.json -init   # scaffold a base config (no networks yet)
gravinet network add corp                   # create your first network (random id + key)
gravinet run -config ./config.json          # run the daemon
```

`-init` is optional: if the config file doesn't exist, `gravinet run` writes a
default one (fresh node id, no networks) and starts anyway, so the service comes
up cleanly on a brand-new host with nothing pre-configured.

There is **no default network** — a fresh config has none, and you create one
explicitly with `network add` (which also mints its first key). This keeps the
config honest: a network exists only because you made it.

## Managing the config

Everything is driven by a single JSON config file. You can edit it by hand, but
the CLI gives you structured, validated commands that edit the file in place and
then ask a running daemon to reload — so **changes are always saved** and take
effect without hand-editing JSON:

```sh
gravinet network add corp                          # new network (random id + key)
gravinet network join e4ba5a47668a1465 key <KEY> peer 198.51.100.7   # join an existing mesh by its id
                                                        # (id comes from 'network list' on a node
                                                        #  already in it; the network's name and
                                                        #  subnet are learned from the seed. no port
                                                        #  = tries 65432 + fallbacks; restarts svc)
gravinet network enable corp
gravinet network disable corp                      # leave but keep config
gravinet network rename corp office                # change the label (id stays; no restart)
gravinet network subnet corp subnet 10.80.0.0/16   # change overlay subnet(s); 'none' clears a
                                                   # family (restart; apply on every node)
gravinet network delete corp                       # leave and remove it
gravinet network list

gravinet route add 10.1.1.0/24                     # redistribute a local route
gravinet route redistribute 0.0.0.0/0              # advertise a default route
gravinet route reject 10.99.0.0/16                 # refuse a peer's advertisement
gravinet route delete 10.1.1.0/24
gravinet route list

gravinet seed add 198.51.100.7 -net corp -notes "office gateway"  # bootstrap address
gravinet seed remove 198.51.100.7 -net corp
gravinet seed list -net corp

gravinet key list -net corp                        # show the 8 join-key slots
gravinet key generate -net corp -label rot2        # mint a new key into a free slot
gravinet key show -net corp -slot 1                # reveal a key to hand to joiners
gravinet key set <KEY> -net corp -slot 2           # import an existing key
gravinet key disable -net corp -slot 0             # retire an old key (rotation)
gravinet key delete -net corp -slot 0

gravinet nat add eth0                              # masquerade overlay out eth0
gravinet nat enable
gravinet nat list

gravinet qos add tcp 3389 priority highest         # prioritise RDP
gravinet qos add udp 53 priority normal
gravinet qos list

gravinet bandwidth up 150mbps interface tun0       # shape a network's uplink
gravinet bandwidth both 1gbps
gravinet bandwidth both 0                           # unlimited (removes the cap)
gravinet bandwidth disable                          # lift the cap but keep the rate
gravinet bandwidth enable                           # reapply the kept rate
gravinet bandwidth list

gravinet host add nas 10.0.0.5 -net corp           # advertise a hostname record
gravinet host reject printer -net corp             # refuse a peer's advertised host
gravinet host list -net corp

gravinet fw add -action deny -proto tcp -dport 23  # firewall (live + saved)
gravinet fw exempt list                            # the node-global allow list (management,
                                                    # BGP, OSPF, RIP by default — never subject
gravinet fw exempt add -name ospf -proto tcp -port 179  # to any network's rulebase; see Firewalling
gravinet fw exempt reset                           # below), or the Allow List tab in the web UI

gravinet managed on                                # accept management from Manager-mode peers
gravinet manager on                                # let this node manage other Managed peers

gravinet ban add <node>                            # distributed ban (runtime)

gravinet list        # print the whole config
gravinet status      # live peers, bans, and routes from the running daemon
```

Most commands take an optional `-net NAME` (only needed when you run more than
one network) and `-config PATH` (default `/etc/gravinet/config.json`). `-net`
accepts a network **name or id**, and that's true everywhere — including the live
commands (`ban`, `unban`, `fw`) that talk to the running daemon, not just the
config commands.

Per-network DNS forwarding (conditional domains → mesh DNS servers) has no CLI
of its own yet — configure it from the web UI's **DNS** section, or by editing
`dns_advertise`/`dns_reject` in the JSON config directly.

**Multiple networks.** A host can belong to as many networks as you like — each
is an independent overlay with its own key, interface, and subnet, all
multiplexed over the one UDP port. Everything per-network is fully scoped to one
network: **bans, routes, firewall, NAT, QoS, and bandwidth all belong to a
specific network**, never the host as a whole. In the CLI you pick it with
`-net NAME` (required once you have more than one, with a helpful error if you
omit it); in the web GUI every section shows one card per network, so each edit
targets that network with no ambiguity. Just `network add`/`join` more of them;
with no subnet given, each new network is auto-assigned a non-overlapping pair
(`10.42.0.0/16`, `10.43.0.0/16`, …, and matching `fd00:N::/64`) so they never
collide, and `network delete`/`disable` removes one without touching the others.
A network's **id is permanent** (it's the on-the-wire identity every peer demuxes
on), but its **name and subnets are editable** after creation — `network rename`
changes the local label live, and `network subnet` changes the overlay range
(restart required, and apply the same change on every node in that network).

**Keys & rotation.** Each network has **8 key slots**. Every enabled, non-empty
key authenticates joiners, so any of them can be used to join — which is what
makes seamless rotation possible: `key generate` a new key, distribute it (`key
show` reveals it), let both run while nodes migrate, then `key disable`/`key
delete` the old one. The CLI guards against disabling or deleting the *last*
enabled key (which would lock the network), and the web GUI has a **Keys**
section that does all the same things. In the GUI the **Peers**, **Keys**, and
**Bans** tables are multi-select: tick one or more rows (or the header box for
all) and use the single button row above the table — Ban for peers; Unban for
bans; Enable / Disable / Reveal / Copy / Delete for keys — so the same action
applies to a whole selection at once. Per-feature on/off (a network, NAT, QoS, a
bandwidth cap) is toggled by **double-clicking its enabled/disabled tag**. Key
changes apply on restart (keys bind into the engine at startup), so
structural-change rules apply: the change saves immediately and the web UI offers
**Restart now**.

**Choosing your own subnets.** Pin the overlay range when you create or join a
network with `subnet` (IPv4) and/or `subnet6` (IPv6) — bare keyword or `-`flag,
your choice:

```sh
gravinet network add corp subnet 10.50.0.0/16 subnet6 fd00:abcd::/48   # dual-stack
gravinet network add v4net subnet 172.16.0.0/12                        # IPv4-only
gravinet network add v6net subnet6 fd00:6::/64                         # IPv6-only
gravinet network add lab -subnet 10.80.0.0/16 --subnet6=fd00:80::/64   # flag form
```

Give both for dual-stack, or just one for a single-family network. Omit them and
you get the auto-assigned dual-stack pair.

**What applies live vs. on restart.** Firewall rules, **NAT, QoS, and bandwidth
throttles**, and managed/manager mode all apply to the running daemon immediately —
including turning any of them on or off — *and* are written to the config
file, so they survive a restart. (Firewall edits made in the web UI persist
the same way.) Structural changes — adding, removing, enabling, disabling, or
joining a network — need the
daemon to rebuild interfaces and sessions, so the `network` commands **restart
the service for you** (via systemd/launchd/SCM); pass `--no-restart` to skip that.
The CLI tells you which case you're in after each change.

Seed peers can be given without a port (`peer 198.51.100.7`). gravinet binds its
primary port (65432, or your `primary_port`) first and falls back to well-known
ports — 443, 4500, 3478, 1194, 500, 53 — when that's blocked, so a port-less seed
is tried on **all** of them, since the remote may have landed on any. Give an
explicit `host:port` only to pin a node to one port.

## Test

```sh
go test ./...
go vet ./...
```

## Status

All 15 roadmap steps are complete — see `docs/ARCHITECTURE.md` for the full
design and the honest list of platform-bound seams (PAM/Windows auth, the
macOS/Windows TUN backends, and the Windows service dispatcher) that are written
and cross-compile but can only be exercised on their target operating systems.

## License

[gravinet] is free software, licensed under the **GNU General Public License,
version 3** (GPLv3). See the `LICENSE` file for the full text.

The core is pure Go with no third-party module dependencies. On Windows the
signed Wintun driver DLL is bundled into the binary; the prebuilt Wintun
binaries are distributed by their authors under a permissive license (not the
GPLv2 that covers the Wintun source), so embedding them is compatible with
GPLv3. That license and attribution are kept in `third_party/wintun/`, and
the installers/release bundles ship it alongside the driver. The Wintun
kernel driver is loaded at runtime as a separate component.
