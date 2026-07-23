# gravinet HTTP/JSON API

Every button, toggle, and table in the web admin UI is backed by a plain
JSON HTTP endpoint under `/api/`. This document is the reference for that
API — everything the UI does, a script can do too.

This is a from-source reference: it was written by reading the handlers in
`internal/webadmin/` directly, not from a spec that could have drifted from
the code. If something here disagrees with what a node actually does, the
code is correct and this file has a bug — please report it.

- [Overview](#overview)
- [Authentication](#authentication)
- [Conventions](#conventions)
- [Quick reference](#quick-reference)
- [Status & configuration](#status--configuration)
- [Networks](#networks)
- [Peers & bans](#peers--bans)
- [Firewall](#firewall)
- [Routes](#routes)
- [Keys](#keys)
- [Seeds & endpoint lookups](#seeds--endpoint-lookups)
- [Hosts & DNS](#hosts--dns)
- [NAT, QoS & bandwidth](#nat-qos--bandwidth)
- [Always-allowed traffic (exempt list)](#always-allowed-traffic-exempt-list)
- [Transport & networking settings](#transport--networking-settings)
- [BGP & BFD](#bgp--bfd)
- [Fleet management (Managed/Manager)](#fleet-management-managedmanager)
- [Remote shell](#remote-shell)
- [Upgrades](#upgrades)
- [Diagnostics & monitoring](#diagnostics--monitoring)
- [System & service](#system--service)
- [Documentation endpoints](#documentation-endpoints)
- [Data type reference](#data-type-reference)
- [Examples](#examples)
- [Known gaps](#known-gaps)

## Overview

The API is served by the same HTTPS listener as the web admin UI itself
(`web_admin.listen` in config, `127.0.0.1:8443` by default). There is no
separate API port — the browser's own requests to `/api/*` are exactly
what's documented here.

The listener uses a self-signed certificate (persisted next to the config
file, so it survives restarts). A script talking to it needs to either
trust that certificate or skip verification (`curl -k`, or the equivalent
in your HTTP client) the same way the admin UI's own JavaScript would fail
to if the browser hadn't already accepted the cert.

All request and response bodies are JSON. There is no XML, no form
encoding, and no versioning scheme in the URL — `/api/status` today is
`/api/status` on the next release too; a breaking change to a response
shape would show up in [`docs/changelog.md`](changelog.md).

By default the listener is bound to `127.0.0.1` only, so out-of-the-box
this API is reachable only from the node itself. Reaching it from another
machine requires either changing `web_admin.listen` to a non-loopback
address (at the operator's own risk — see the security notes in
`internal/config`), or going through a peer's mesh-authenticated
[management proxy](#the-management-proxy) instead.

## Authentication

### Logging in

```
POST /api/login
{"user": "admin", "pass": "hunter2"}
```

On success, the response sets an HTTP-only, `Secure`, `SameSite=Strict`
cookie named `gravinetadmin` and returns:

```json
{"ok": true, "user": "admin"}
```

The cookie is a signed, self-contained session token (HMAC-SHA256 over the
username and an expiry, using a key persisted alongside the TLS
certificate) — there's no server-side session store to look up. It's valid
for **8 hours** from login. Every subsequent request should send this
cookie; that's the whole of the authentication story for a script driving
one node directly (curl's `-b`/`-c` cookie-jar flags, or your HTTP
client's equivalent, are the easiest way to carry it across requests).

Failure responses:

| Status | Body | Meaning |
|---|---|---|
| `401` | `{"error": "invalid credentials"}` | Wrong username/password |
| `429` | `{"error": "too many failed logins; locked out", "retry_after_seconds": N}` | This source IP is locked out after repeated failures |
| `503` | `{"error": "the server has no working authentication configured — ..."}` | No auth backend is usable at all (misconfiguration) |

Who can log in, and how, is set by `web_admin.auth_mode` in config:

- `local` — a password checked against a PBKDF2 hash stored in
  `web_admin.users`. Create or update a local user from the command line
  with `gravinet genpass` (there is no API endpoint that creates a user —
  intentionally, since the whole point is a credential the API itself
  can't be used to mint).
- `pam` (Linux/macOS/FreeBSD) — the host's own PAM stack, i.e. the same
  username/password as logging into the machine.
- `system` (OpenBSD) — `bsd_auth(3)`, OpenBSD's native equivalent.
- `windows` — native Windows account logon.

For `pam`/`system`/`windows`, `web_admin.allow_users` can restrict which
system accounts are accepted (empty means any account that authenticates
successfully is let in).

### Logging out

```
POST /api/logout
```

Clears the cookie client-side and revokes the token server-side for the
remainder of its 8-hour lifetime (so a copy of the cookie captured before
logout can't be replayed against this process). Always returns
`{"ok": true}`.

### Liveness check

```
GET /api/ping
```

The one endpoint that needs no authentication at all — cheap and always
answers. Returns `{"ok": true, "boot": "<boot id>"}`. The `boot` value
changes across a restart, which is what the admin UI uses to detect "the
daemon restarted since I last checked" even if it comes back faster than
the UI's own poll interval.

### Authenticating as a fleet manager

A node in [Manager mode](#fleet-management-managedmanager) can act on a
directly-connected peer that's in Managed mode *without* logging into that
peer separately — the peer accepts the request because it recognizes the
Manager's mesh session, not because of a cookie. See
[Fleet management](#fleet-management-managedmanager) for the full model;
everywhere else in this document, "authenticated" means "holds a valid
session cookie" unless stated otherwise.

## Conventions

**Request bodies.** POST endpoints that take input expect a JSON object in
the body. Field names are matched case-insensitively (Go's `encoding/json`
default), but the web UI itself always sends lowercase keys (`net`,
`node`, `op`, ...), and this document follows that convention throughout.
A `Content-Type: application/json` header is good practice but not
enforced by the server.

**The `net` field.** Almost every per-network endpoint takes a `net`
field identifying which network to act on. It accepts either:

- the network's configured **name**, or
- its **hex ID** (the 16-hex-character identifier shown everywhere in the
  UI and in `/api/status`/`/api/config`), matched exactly or in its
  zero-trimmed numeric form.

If `net` is omitted (empty string) **and this node has exactly one
network configured**, that network is used automatically. With zero or
more than one network configured, an omitted `net` is an error. This is
the same resolution logic the CLI uses, so a script and `gravinet net ...`
on the command line agree about what a bare network reference means.

**Response envelope.** There's no single universal envelope, but two
shapes recur constantly:

- A mutation that succeeded: `{"ok": true, ...}`, often with a `restart`
  boolean — see below.
- A mutation or lookup that failed: `{"error": "human-readable message"}`,
  usually with HTTP status `400`. A handful of read-only endpoints that
  report the state of an optional subsystem (BGP, metrics, an upload) use
  `{"available": false, "reason": "..."}` instead of an HTTP error, so a
  missing optional feature doesn't look like a broken request — check the
  endpoint's own section for which shape applies.

**The `restart` flag.** Most config changes are applied live — the running
daemon reloads its config and the effect is immediate, no restart needed.
A handful of changes (re-addressing a network's overlay IP, flipping
`allow_remote_shell`, toggling Geo-IP lookups, enabling UPnP) can't take
effect until the process restarts; those endpoints reply with
`"restart": true` so the caller (the UI, or a script) knows to prompt for
or trigger one via [`POST /api/restart`](#post-apirestart). Where a
response always carries the same value for a given endpoint, that's noted
in its own section instead of being treated as something to check per
call.

**HTTP methods.** Where an endpoint supports both reading and writing, it
uses a plain `GET` for the read and `POST` for the write, on the *same*
path — there's no separate `/api/x/get` and `/api/x/set`. Endpoints that
are POST-only reject other methods with `405 Method Not Allowed`; a few
older-style handlers don't explicitly check the method and simply treat
any non-POST body as empty, which is why this document is explicit about
which verb each endpoint expects.

**Status codes.** `200` for success and for the "degraded, here's why"
shape described above; `400` for a rejected request (bad input, an
operation that doesn't apply); `401` for missing/invalid authentication;
`403` for something authenticated but not permitted (e.g. a setting that's
local-only being reached through the fleet proxy); `404`/`405` from the
standard library for unknown paths/methods; `500` only for genuine server
faults.

**Caching.** Every authenticated endpoint sends `Cache-Control: no-store`.
Don't rely on `ETag`/`If-Modified-Since` — there isn't any.

## Quick reference

Unless noted, every path below requires authentication (a valid session
cookie, or a qualifying fleet-manager mesh session — see
[Authentication](#authentication)).

| Method | Path | Purpose |
|---|---|---|
| POST | `/api/login` | Log in, start a session |
| POST | `/api/logout` | Log out, end the session |
| GET | `/api/ping` | Liveness check (no auth) |
| GET | `/api/status` | Live per-network state: peers, bans, routes, firewall |
| GET | `/api/config` | Stored (secret-free) configuration |
| POST | `/api/network` | Add/delete/enable/disable/rename/... a network |
| POST | `/api/network/token` | Mint a join token for a network |
| POST | `/api/network/reset` | Drop and redial all sessions on a network |
| POST | `/api/peer` | Enable/disable/annotate a peer locally |
| POST | `/api/ban` | Ban a node mesh-wide |
| POST | `/api/ban/notes` | Edit the notes on a ban you issued |
| POST | `/api/unban` | Lift a ban |
| GET/POST | `/api/firewall` | Read or edit a network's firewall rules/objects/services |
| POST | `/api/route` | Add/edit/remove an advertised or rejected route |
| GET/POST | `/api/routeadv` | Route re-advertisement interval |
| POST | `/api/key` | Manage a network's join/rotation key slots |
| POST | `/api/seed` | Add/edit/remove a network's bootstrap seeds |
| POST | `/api/seed-info` | DNS/WHOIS/Geo-IP lookup on a seed address |
| POST | `/api/peer-info` | Same lookup, for a connected peer's endpoint |
| POST | `/api/host` | Advertise/reject custom hostname records |
| POST | `/api/dns` | Advertise/reject conditional DNS forwards |
| POST | `/api/nat` | Add/edit/remove NAT rules |
| POST | `/api/natstate` | Global NAT state-table timeout |
| POST | `/api/qos` | Add/edit/remove QoS classification rules |
| POST | `/api/bandwidth` | Set per-network bandwidth caps |
| GET/POST | `/api/exempt` | Node-global always-allowed traffic list |
| GET/POST | `/api/keepalive` | NAT keepalive interval |
| GET/POST | `/api/peertimeout` | Dead-session timeout |
| POST | `/api/port` | Set the UDP underlay port(s) |
| POST | `/api/tcpport` | Set the TCP/TLS fallback port(s) |
| POST | `/api/geoip` | Toggle Geo-IP lookups in the info panel |
| POST | `/api/upnp` | Toggle UPnP port mapping |
| GET | `/api/interfaces` | List host network interface names |
| GET | `/api/bgp` | Live BGP peer summary (via FRR) |
| GET | `/api/bfd` | Live BFD session summary (via FRR) |
| GET | `/api/bgp/table` | Raw `show bgp all` text |
| GET | `/api/bgp/redistribute-options` | CIDRs available to redistribute |
| GET/POST | `/api/bgp/config` | Read/write gravinet's BGP configuration |
| GET | `/api/bgp/import` | Import a pre-existing FRR BGP config |
| GET/POST | `/api/managed` | This node's Managed-mode state |
| GET/POST | `/api/manager` | This node's Manager-mode state |
| GET/POST | `/api/upgrade/accept-manager` | Opt in/out of Manager-pushed upgrades |
| GET | `/api/cluster` | List managed peers this node currently sees |
| ANY | `/api/proxy` | Forward an API call to a managed peer |
| GET/POST | `/api/shell/setting` | Toggle remote shell access (session-only) |
| GET (WS) | `/api/shell/ws` | Browser-facing remote shell WebSocket |
| POST | `/api/shell/hijack` | Peer-facing shell relay target (not for direct use) |
| GET | `/api/upgrade` | This node's upgrade state |
| POST | `/api/upgrade/source` | Upload source, build, and apply an upgrade |
| POST | `/api/upgrade/rollback` | Roll back an already-applied upgrade |
| POST | `/api/upgrade/push` | Push a source archive to managed peers |
| POST | `/api/upgrade/remote-apply` | Accept a Manager-pushed archive (peer-facing) |
| GET | `/api/metrics` | CPU/mem/disk/throughput history |
| GET | `/api/capture/interfaces` | Interfaces available for packet capture |
| POST | `/api/capture/start` | Start a packet capture |
| POST | `/api/capture/stop` | Stop the active capture |
| POST | `/api/capture/clear` | Clear the capture buffer |
| GET | `/api/capture/packets` | Poll captured packet summaries |
| GET | `/api/capture/pcap` | Download the capture buffer as a `.pcap` |
| GET | `/api/speedtest/source` | Download-test data source (peer-facing) |
| POST | `/api/speedtest/sink` | Upload-test data sink (peer-facing) |
| POST | `/api/speedtest/run` | Run a two-way speed test against a peer |
| GET | `/api/localroutes` | This host's kernel routing table |
| GET | `/api/localhosts` | This host's `/etc/hosts` (or platform equivalent) contents |
| GET | `/api/localdns` | This host's live OS resolver registration, per network |
| GET | `/api/latency` | Ping every peer on every network |
| GET | `/api/logs` | Tail (or download) the daemon log |
| POST | `/api/logs/clear` | Truncate the daemon log |
| GET/POST | `/api/loglevel` | Read/set the daemon's log level |
| GET/POST | `/api/logsize` | Read/set the log file's size cap |
| POST | `/api/restart` | Restart the gravinet service |
| GET | `/api/about` | Version/OS/architecture/Go runtime info |
| GET | `/api/readme` | This node's bundled README.md |
| GET | `/api/license` | This node's bundled LICENSE |
| GET | `/api/getting-started` | This node's bundled getting-started.md |

## Status & configuration

### `GET /api/status`

The live, moment-to-moment state of every network this node has: connected
peers, active bans, locally-disabled peers, redistributed routes, firewall
rules, and this node's own identity on each network. This is what the
Peers/Bans/Firewall pages poll continuously.

```json
{
  "nets": [
    {
      "id": "000000000000002a",
      "peers": [ /* PeerInfo, see Data type reference */ ],
      "bans": [ /* BanInfo */ ],
      "disabled_peers": [ /* DisabledPeerInfo */ ],
      "routes": [ /* RouteInfo */ ],
      "firewall": [ /* FirewallRule, with live ids and hit counters */ ],
      "self": { /* PeerInfo — this node's own row */ }
    }
  ],
  "nat_class": "full-cone",
  "public": "203.0.113.9:51820"
}
```

`nat_class` and `public` describe this node's own NAT situation (its
detected NAT type and its observed public underlay endpoint), not
per-network.

### `GET /api/config`

The stored, secret-free configuration — network addressing, seeds,
firewall/NAT/QoS/throttle settings, key *metadata* (never key material
itself; see [Keys](#keys) for how to reveal an actual key), plus node-wide
settings. This is what every editor page loads before the operator makes
a change; a script driving the API the same way should generally read this
first to see current values before deciding what to send.

```json
{
  "nets": [
    {
      "id": "000000000000002a",
      "name": "office",
      "enabled": true,
      "notes": "",
      "subnet4": "10.42.0.0/16",
      "subnet6": "fd00:42::/64",
      "address4": "10.42.0.1",
      "address6": "fd00:42::1",
      "seeds": [ {"address": "seed.example.com:51820", "notes": ""} ],
      "routes": [ /* config.Route */ ],
      "route_reject": [ /* config.RejectRoute */ ],
      "redistribute_bgp_routes": [],
      "redistribute_bgp_metric": 0,
      "nat": { /* config.NAT */ },
      "qos": { /* config.QoS */ },
      "throttle": { /* config.Throttle */ },
      "firewall": { /* config.Firewall */ },
      "hosts_advertise": [ /* config.HostRecord */ ],
      "hosts_reject": [ /* config.HostReject */ ],
      "dns_advertise": [ /* config.DNSForward */ ],
      "dns_reject": [ /* config.DNSReject */ ],
      "keys": [
        {"slot": 0, "label": "2026-Q1", "enabled": true, "set": true,
         "expires": "", "distributed": false, "notes": ""}
      ]
    }
  ],
  "primary_port": 51820,
  "tcp_fallback_port": 51821,
  "tcp_fallback_disabled": false,
  "extra_listen_ports": [],
  "extra_tcp_listen_ports": [],
  "nat_state_timeout": 120,
  "geoip_lookup": true,
  "enable_upnp": false,
  "allow_remote_shell": false,
  "shell_supported": true,
  "bgp_supported": true,
  "log_level": "info",
  "log_max_size": "200M",
  "firewall_objects": [ /* config.FirewallObject, node-global */ ],
  "firewall_services": [ /* config.FirewallService, node-global */ ],
  "firewall_objects_seeded": true,
  "firewall_services_seeded": true
}
```

`keys[].set` reports whether a slot holds a key at all, without revealing
it; `distributed` mirrors whether this slot is currently being kept in
sync mesh-wide (see [Keys](#keys)).

## Networks

### `POST /api/network`

```json
{"op": "add", "net": "office", "subnet4": "", "subnet6": ""}
```

`op` selects the operation; other fields are used or ignored depending on
which:

| `op` | Fields used | Effect | Live or restart? |
|---|---|---|---|
| `add` | `net` (new name), `subnet4`, `subnet6` (optional — auto-picked if blank) | Create a network | Live |
| `delete` / `del` / `remove` | `net` | Remove a network entirely | Live |
| `enable` | `net` | Bring a disabled network up | Live |
| `disable` | `net` | Take a network down without deleting it | Live |
| `rename` | `net`, `newname` | Rename | Live |
| `notes` | `net`, `notes` | Set the operator note | Live |
| `subnet` | `net`, `subnet4`, `subnet6` | Change the overlay subnet(s) | **Restart** |
| `address` | `net`, `address4`, `address6` | Change this node's own overlay address | **Restart** |
| `join` | `id`, `key`, `peer`, `subnet4`, `subnet6` | Join an existing network by ID/key/seed | Live |
| `join-token` | `token` | Join using a token minted by `/api/network/token` | Live |
| `redistribute-bgp` | `net`, `routes` (string array of CIDRs), `metric` | Select which BGP-learned routes this network redistributes into the mesh | Live |

Response: `{"ok": true, "restart": <bool>}` or `{"error": "..."}`.

### `POST /api/network/token`

Mints a shareable join token for a network — bundles the network ID,
enabled key(s), subnets, and seeds so another node can join by pasting the
token into `/api/network` (`op: "join-token"`). Read-only: doesn't change
config.

```json
{"net": "office", "addr": "203.0.113.9:51820", "expires": "24h"}
```

`addr` (optional) advertises this node's own reachable address as an extra
seed inside the token. `expires` (optional) is a Go duration string
(`"24h"`, `"72h"`, ...); omitted means the token never expires.

Response: `{"token": "...", "seeds": 3}` (`seeds` is how many bootstrap
addresses the token carries) or `{"error": "..."}`.

### `POST /api/network/reset`

Drops every current peer session on a network and clears seed retry
backoff, so the engine immediately redials everything instead of waiting
out existing timeouts. A live, in-place action — no config change.

```json
{"net": "office"}
```

Response: `{"ok": true}` or `{"error": "..."}`.

## Peers & bans

### `POST /api/peer`

Enables, disables, or annotates a peer **locally** — unlike a ban, this
only affects whether *this* node connects to that peer; it isn't flooded
to the mesh.

```json
{"net": "office", "node": "<node id>", "op": "disable", "notes": ""}
```

`op` is one of `enable`, `disable`, `notes` (with `notes` also carrying
the new note text). Applied live: a newly-disabled peer is disconnected
immediately; a re-enabled one is redialed by the maintenance loop.

Response: `{"ok": true, "restart": false}` or `{"error": "..."}`.

### `POST /api/ban`

Bans a node **mesh-wide** — floods the ban to every reachable peer.

```json
{"net": "office", "node": "<node id>", "notes": "compromised key"}
```

Response: `{"ok": true}` or `{"error": "..."}`.

### `POST /api/ban/notes`

Edits the notes on a ban **this node originated**, and re-floods the
update. Only the origin node can edit its own ban.

```json
{"net": "office", "node": "<node id>", "notes": "updated reason"}
```

### `POST /api/unban`

```json
{"net": "office", "node": "<node id>", "force": false}
```

`force: true` uses `ForceUnban` instead of the ordinary path — for a ban
state that's otherwise stuck. Response: `{"ok": true}` or `{"error": "..."}`.

## Firewall

### `GET /api/firewall?net=office`

Returns the live rule list for a network:

```json
{"rules": [ /* FirewallRule, with engine-assigned ids and hit counters */ ]}
```

### `POST /api/firewall`

One endpoint, many operations, selected by `op`:

| `op` | Fields | Effect |
|---|---|---|
| `add` | `net`, `rule` (a `FirewallRule` object), `at` (insert index, optional) | Add a rule |
| `del` | `net`, `ids` (array of rule IDs) | Remove rule(s) |
| `move` | `net`, `ids` (exactly one ID), `to` (target index) | Reorder a rule |
| `reset-counters` | `net`, `ids` (empty = all) | Zero hit-count tallies |
| `enable` / `disable` | `net` | Turn the whole firewall for that network on/off |
| `rule-enable` / `rule-disable` | `net`, `ids` (one rule ID) | Toggle a single rule |
| `objects` | `objects` (array of `FirewallObject`) | Replace the node-global address-object catalog |
| `services` | `services` (array of `FirewallService`) | Replace the node-global service catalog |
| `mark-objects-seeded` / `mark-services-seeded` | — | Record that the UI's one-time catalog auto-populate has run (see `/api/config`'s `*_seeded` flags) |

`objects`/`services`/the catalog-seeded markers are **node-global** — not
scoped to one network, since every network's rules resolve `src`/`dst`/
`services` references against the same shared catalog.

Response for most ops: `{"ok": true, "restart": false}`; `add` additionally
returns nothing extra beyond `ok`/`restart` (the new rule's assigned ID
shows up on the next `GET`). Errors: `{"error": "..."}`.

## Routes

### `POST /api/route`

```json
{"op": "add", "net": "office", "cidr": "192.168.50.0/24", "metric": 0}
```

| `op` | Extra fields | Effect |
|---|---|---|
| `add` / `advertise` / `redistribute` | `cidr`, `metric` | Advertise a CIDR into the mesh |
| `delete` / `del` / `remove` | `cidr` | Stop advertising it |
| `reject` | `cidr`, `inclusive` (bool) | Locally refuse a CIDR advertised by a peer |
| `enable` / `disable` | `cidr` | Toggle an advertised route |
| `reject-enable` / `reject-disable` | `cidr` | Toggle a reject entry |

Applied live in every case. Response: `{"ok": true, "restart": false}` or
`{"error": "..."}`.

### `GET/POST /api/routeadv`, `/api/keepalive`, `/api/peertimeout`

Three timing settings share one GET/POST shape (all values in seconds; all
applied live):

- **`/api/routeadv`** — how often advertised routes are re-sent.
- **`/api/keepalive`** — the NAT-keepalive cadence.
- **`/api/peertimeout`** — how long without contact before a session is
  considered dead.

```
GET /api/keepalive
→ {"interval": 25, "effective": 25, "peer_timeout_now": 90}

POST /api/keepalive
{"interval": 20}
→ {"ok": true, "restart": false}
```

`effective` is the value actually in force (accounting for the built-in
default when `interval` is 0). `/api/keepalive`'s GET additionally reports
`peer_timeout_now`, since an explicit peer-timeout below the (possibly
just-changed) keepalive interval is silently clamped up to it — this lets
the UI show that shift immediately. Valid range for `interval` on all
three: `0`–`86400`.

## Keys

### `POST /api/key`

Manages a network's join/rotation key slots. All ops take `net` and
`slot` (the slot index) at minimum.

| `op` | Extra fields | Effect | Response adds |
|---|---|---|---|
| `generate` | `label` | Generate a new random key into the slot | `slot`, `key` |
| `set` / `import` | `key`, `label` | Import an existing key | — |
| `label` | `label` | Rename the slot; re-propagates to peers if the slot is `distributed` | — |
| `expiry` / `expires` | `expires` (RFC3339, or empty for never) | Change expiry; also re-propagates if distributed | — |
| `notes` | `notes` | Local-only note (never distributed) | — |
| `enable` / `disable` | — | Toggle whether the slot authenticates sessions | — |
| `delete` / `del` / `remove` | — | Remove the slot; **retracts** it from every peer if it was distributed | — |
| `reveal` | — | Return the actual key material for this slot | `key` |
| `distribute` | — | Push an enabled key out to every currently-connected peer over the encrypted mesh | `peers` (count reached) |
| `undistribute` | — | Stop mesh-wide management of this slot (does **not** retract it — peers keep their existing copy) | — |

`reveal` and `distribute` both require `configPath` to be set on this node
(true for any normally-installed daemon) and both touch live key material
— restrict who can reach this endpoint accordingly.

Key changes apply live (`"restart": false` in every response) — keys bind
into new sessions immediately, no daemon restart needed.

## Seeds & endpoint lookups

### `POST /api/seed`

```json
{"op": "add", "net": "office", "addr": "seed.example.com:51820"}
```

| `op` | Fields | Effect |
|---|---|---|
| `add` | `addr` | Add a bootstrap seed |
| `remove` / `delete` | `addr` | Remove one |
| `notes` | `addr`, `notes` | Set its note |
| `update-addr` | `addr`, `newaddr` | Change a seed's address in place |

Adding a seed dials it live; removing one takes effect on the next
restart.

### `POST /api/seed-info`

Runs forward DNS, reverse DNS, WHOIS, and (if `geoip_lookup` is on)
Geo-IP against a seed's host — the "🛈" button next to a seed in the UI.

```json
{"net": "office", "addr": "seed.example.com:51820"}
```

Response is a `seedInfoResult` — see
[Data type reference](#data-type-reference). Every section
(forward/reverse/whois/geo) is independent and best-effort: one failing
doesn't block the others.

### `POST /api/peer-info`

Same lookup, run against a **connected peer's** currently-observed
underlay endpoint rather than a configured seed address (the peer analog
of `/api/seed-info`).

```json
{"net": "office", "node": "<node id>", "endpoint": "203.0.113.9:51820"}
```

`endpoint` is the address the caller just saw for that peer in
`/api/status` — it isn't looked up server-side, since a peer's underlay
endpoint isn't config.

## Hosts & DNS

### `POST /api/host`

Custom `name → IP` records advertised mesh-wide, plus a local refuse-list
for records *other* peers advertise.

```json
{"op": "add", "net": "office", "name": "printer", "ip": "10.42.0.50"}
```

| `op` | Fields | Effect |
|---|---|---|
| `add` | `name`, `ip` | Advertise a record |
| `update` | `name`, `newname`, `ip` | Edit one |
| `remove` / `delete` | `name` | Stop advertising |
| `enable` / `disable` | `name` | Toggle |
| `reject` | `name` | Refuse this hostname if a peer advertises it |
| `reject-remove` | `name` | Remove a reject entry |
| `reject-enable` / `reject-disable` | `name` | Toggle a reject entry |

All applied live.

### `POST /api/dns`

Conditional DNS forwarding domains, mirroring `/api/host`'s shape exactly,
with a comma-separated server list instead of a single IP:

```json
{"op": "add", "net": "office", "domain": "corp.internal", "servers": "10.42.0.10,10.42.0.11"}
```

Same op set as `/api/host` (`add`, `update` — with `newdomain` — ,
`remove`/`delete`, `enable`, `disable`, `reject`, `reject-remove`,
`reject-enable`, `reject-disable`). An advertised domain also becomes a
plain search-domain suffix for this node's own resolver automatically —
there's no separate toggle for that.

## NAT, QoS & bandwidth

### `POST /api/nat`

```json
{"op": "add", "net": "office", "iface": "eth0"}
```

| `op` | Fields | Effect |
|---|---|---|
| `add` | `iface` alone → masquerade shorthand; or `source`/`dest`/`translate` (+ optional `iface`) → a full rule | Add a NAT rule |
| `update` | `index`, `source`, `dest`, `translate`, `iface` | Edit rule at `index` |
| `delete` / `del` / `remove` | `iface` (removes the whole masquerade for that interface) or `index` | Remove |
| `enable` / `disable` | — | Toggle NAT for the whole network |
| `rule-enable` / `rule-disable` | `index` | Toggle a single rule |

`translate` encodes both *what* to rewrite to and *which direction*:
`"masquerade"` (SNAT via `iface`'s address), a literal IPv4 (static SNAT),
or `"port-forward:<ipv4>"` (DNAT). See `NATRule` in
[Data type reference](#data-type-reference).

### `POST /api/natstate`

Sets the global NAT connection-state timeout, in seconds (`0` restores
the 120s default). **POST-only** — there's no GET; the current value is
reported by `/api/config`'s `nat_state_timeout` field.

```
POST /api/natstate
{"timeout": 180}
→ {"ok": true, "restart": false}
```

### `POST /api/qos`

```json
{"op": "add", "net": "office", "proto": "udp", "port": 51820, "class": 0}
```

| `op` | Fields | Effect |
|---|---|---|
| `add` | `proto`, `port`, `services` (array of named services), `class` | Classify matching traffic into `class` (0 = highest priority) |
| `delete` / `del` / `remove` | `proto`, `port`, `services` | Remove a matching rule |
| `enable` / `disable` | — | Toggle QoS for the network |
| `rule-enable` / `rule-disable` | `proto`, `port`, `services` | Toggle a specific rule |
| `mark` | `class`, `dscp` | Set a class's outbound DSCP mark |
| `unmark` | `class` | Clear a class's DSCP override (revert to the built-in default) |

`services` names entries from the node-global service catalog (same one
firewall rules use — see `/api/firewall`'s `objects`/`services` ops),
unioned with the literal `proto`/`port`.

### `POST /api/bandwidth`

```json
{"op": "enable", "net": "office"}
```
```json
{"net": "office", "dir": "up", "bps": 10000000}
```

`op: "enable"`/`"disable"` toggles throttling for the network; any other
call sets a directional cap (`dir` one of `"up"`/`"down"`/`"both"`, `bps`
in bytes/sec, `0` = unlimited).

## Always-allowed traffic (exempt list)

### `GET /api/exempt`

The **node-global** (not per-network) list of traffic classes that always
pass regardless of firewall rules — the safety net that keeps management
traffic reachable even behind a broad deny-everything rule.

```json
{
  "exempt": [
    {"name": "remote management", "proto": "tcp", "port": 8443, "mgmt": true},
    {"name": "BGP", "proto": "tcp", "port": 179}
  ],
  "default": true,
  "mgmt_port": 8443
}
```

`default: true` means this node hasn't customized the list yet and is
seeing the built-in defaults. An `mgmt: true` entry's `port` is reported
as this node's *actual* live web-admin port, not a placeholder.

### `POST /api/exempt`

Three shapes, by which fields are set:

```json
{"reset": true}
```
Restores the built-in default list.

```json
{"op": "enable", "index": 2}
```
Toggles one entry by its position in the list (`op` is `"enable"` or
`"disable"`).

```json
{"exempt": [ {"name": "custom", "proto": "tcp", "port": 22} ]}
```
Replaces the entire list. Each entry: `name`, `proto` (empty = any),
`port` (`0` = matches source *or* destination port being any — used for
port-less protocols like OSPF), `mgmt` (bool — follow the live admin port
instead of `port`), `disabled` (bool).

## Transport & networking settings

### `POST /api/port`

Sets the UDP underlay listen port(s). The first port in the list is the
primary (used for outbound and advertised to peers); any additional ports
are extra listen-only ports.

```json
{"ports": [51820, 51821]}
```
```json
{"disabled": true}
```

`disabled: true` turns UDP off entirely (refused if the TCP/TLS fallback
is also off — a node needs at least one transport reachable). Applied
live; the old port keeps serving briefly during the switch.

### `POST /api/tcpport`

The TCP/TLS-fallback counterpart to `/api/port`, same shape: first port in
the list is the fallback listener itself, the rest are extra TCP/TLS
ports; `disabled: true` turns the fallback off (refused if UDP is also
off).

### `POST /api/geoip`

Toggles Geo-IP lookups in the peer/seed info panel. **POST-only** —
there's no GET; the current value is reported by `/api/config`'s
`geoip_lookup` field. **Needs a restart** — unlike most settings, this one
is captured into the running server once at startup rather than re-read
live.

```
POST /api/geoip
{"on": true}
→ {"ok": true, "restart": true}
```

### `POST /api/upnp`

Toggles gravinet's own best-effort UPnP IGD port-mapping helper (off by
default). **POST-only** — there's no GET; the current value is reported
by `/api/config`'s `enable_upnp` field. **Needs a restart** — the UPnP
manager is only ever started once, alongside the daemon's listen ports.

```json
{"on": true}
```

### `GET /api/interfaces`

```json
{"interfaces": ["eth0", "eth1", "wlan0"]}
```

This host's non-loopback network interface names, for populating a
masquerade/NAT interface picker. No POST.

## BGP & BFD

BGP support is a thin, read-mostly window onto FRR (`vtysh`) — gravinet
doesn't implement BGP itself. Every endpoint here is gated on FRR/`vtysh`
being installed on the host (`bgp_supported` in `/api/config`); when it
isn't, or FRR isn't currently running, these degrade to
`{"available": false, "reason": "..."}` with `200 OK` rather than an HTTP
error.

### `GET /api/bgp`

```json
{"available": true, "router_id": "10.42.0.1", "local_as": 4200000042,
 "peers": [ {"peer": "10.42.0.2", "remote_as": 4200000043, "state": "Established",
             "uptime": "01:23:45", "prefixes_received": 4, "afi": "ipv4"} ]}
```

### `GET /api/bfd`

Same availability shape; `"peers"` is the current BFD session table.

### `GET /api/bgp/table`

Raw text of FRR's `show bgp all` — the full BGP table, left exactly as FRR
renders it rather than reparsed:

```json
{"available": true, "text": "BGP table version is 12, ...\n"}
```

### `GET /api/bgp/redistribute-options`

What's currently available to pick from when configuring redistribution:

```json
{
  "available": true,
  "connected_routes": ["192.168.1.0/24"],
  "static_routes": ["10.99.0.0/16"],
  "bgp_learned_routes": ["203.0.113.0/24"]
}
```

`connected_routes`/`static_routes` exclude gravinet's own mesh interfaces
automatically, so the mesh's internal addressing never shows up here as if
it were a real external network.

### `GET/POST /api/bgp/config`

```
GET /api/bgp/config
→ {"bgp": { /* config.BGPConfig */ }, "installed": true, "supported": true,
   "active": true, "mesh_routes": ["192.168.50.0/24"]}
```

`active` reports whether gravinet is actively driving a BGP session
(`enabled` and a non-zero `asn`); `mesh_routes` is what the "redistribute
mesh routes" option would currently carry into BGP, shown regardless of
whether it's turned on. This GET never touches `vtysh`, so it always loads
instantly — see `/api/bgp/import` for reflecting a live FRR state.

```
POST /api/bgp/config
{ /* a full config.BGPConfig object */ }
→ {"ok": true, "applied": true, "note": ""}
```

Persists first, then reconciles FRR (renders `frr.conf`, syncs the daemon
set, reloads FRR). `applied: false` with a `note` means the save succeeded
but FRR couldn't be reconciled (most often: not installed) — the config is
still saved for whenever FRR becomes available.

### `GET /api/bgp/import`

Reads whatever BGP configuration FRR is **currently running** (configured
outside gravinet, e.g. by hand) and returns it, so the editor can reflect
a pre-existing setup instead of showing an empty form while peers are
actually live. Read-only — never writes anything; the operator adopts by
saving through `/api/bgp/config`.

```json
{"imported": true, "imported_has_passwords": false, "reason": "",
 "bgp": { /* config.BGPConfig */ }}
```

`imported: false` with a `reason` (and `installed`) means there was
nothing to import — check those before assuming the editor should be
empty.

## Fleet management (Managed/Manager)

gravinet supports driving several nodes from one console. Two independent
flags control it:

- **Manager** (`GET/POST /api/manager`, body `{"on": bool}`) — this node
  can *drive* other nodes. A Manager peer's authenticated mesh session is
  itself the credential it presents to a Managed peer — see below.
- **Managed** (`GET/POST /api/managed`, body `{"on": bool}`) — this node
  *accepts* being driven by a directly-connected Manager peer.

Both are plain `{"managed": bool}`/`{"manager": bool}` GET/POST pairs,
applied live, and — critically — **local-only**: neither can be flipped
remotely through the fleet proxy, on purpose (see `/api/proxy` below).

### How the trust bypass works

Every ordinary authenticated endpoint's auth check (`authed()` internally)
accepts a request two ways: a valid session cookie, **or** a connection
that (a) arrives over the overlay mesh itself, (b) resolves to an address
genuinely inside one of this node's overlay subnets (not merely something
the registry has heard advertised — an SSRF/spoofing guard), and (c)
belongs to a peer currently advertising Manager mode. Reaching a node over
the encrypted overlay at all already required the mesh's own pre-shared
key, so this bypass rides on a real cryptographic trust boundary, not a
bare IP check. A merely-Managed (not Manager) peer, or a Manager known
only through mesh gossip rather than a live direct session, does **not**
qualify for this bypass — some sensitive actions (see below) additionally
require a genuine *direct* session, not just "somewhere on an overlay
Manager advertises from."

### `GET /api/cluster`

Lists every managed peer this node has heard from recently (within a 90s
TTL), plus this node's own managed/manager state:

```json
{
  "managed": false, "manager": true,
  "self_hostname": "manager-1", "self_id": "<node id>",
  "self_overlay": "10.42.0.1", "self_web_port": 8443,
  "peers": [
    {"node_id": "<id>", "hostname": "office-router", "overlay": "10.42.0.5",
     "web_port": 8443, "age_seconds": 12, "connected": true,
     "manageable": true, "manager": false, "version": "587"}
  ]
}
```

`manageable` means this node has enough information (a reachable overlay
address and web port) to actually proxy to that peer right now.

`version` is the gravinet build that peer reports for itself — surfaced
here so a fleet operator can see which nodes are behind before pushing an
upgrade to them. It's advertised by the peer in its own mesh handshake and
relayed onward through peer-list gossip, so it's populated for peers known
only indirectly too, not just direct neighbours. Omitted entirely for a
peer running a build old enough to predate the field; treat that as
unknown rather than as "no version".

### The management proxy

```
ANY /api/proxy?node=<peer node id>&path=/api/status
```

Forwards a request to a managed peer's own web admin over the encrypted
overlay — the browser (or a script) stays pointed at the node it's logged
into; selecting a different node in the UI is just adding `?node=...` here.
The response is relayed back verbatim (status code, `Content-Type`, body).

`path` must be a `/api/...` path (traversal — literal or percent-encoded —
is rejected outright). The target must currently appear in this node's own
managed-peer set (from `/api/cluster`) and resolve to a genuine overlay
address; a stale or spoofed target is refused with `502`/`403`/`503`
depending on which check failed (unreachable vs. non-overlay vs. this
node's own overlay data plane being down — each gets a distinct message).

**Endpoints the proxy refuses to forward, on purpose:**

- `/api/managed`, `/api/manager`, `/api/shell/setting`,
  `/api/upgrade/accept-manager` — always local-only; toggling any of these
  through a proxy hop would silently apply to the wrong node from the
  operator's point of view.
- `/api/upgrade/push` — a fleet-driving action must originate from the
  node actually being looked at, not be re-triggered one hop further out.

Everything else — firewall, routes, NAT, per-node upgrade state, shell
sessions, speed tests, and so on — proxies normally.

## Remote shell

Off by default (`web_admin.allow_remote_shell`). A real OS shell/PTY
reachable through the browser.

### `GET/POST /api/shell/setting`

```
GET  → {"allow_remote_shell": false, "supported": true}
POST {"on": true} → {"ok": true, "allow_remote_shell": true, "restart": true}
```

Unlike most toggles, this endpoint accepts **only** a genuine local
session cookie — never the Manager-overlay bypass, and it's also
explicitly blocked at the fleet proxy — so a Manager can open a shell
*through* a peer that already has this on, but can never be the one to
turn it on. Needs a restart to take effect.

### `GET /api/shell/ws` (WebSocket)

```
GET /api/shell/ws?node=<id|self>&rows=24&cols=80
```

Upgrades to a standard WebSocket (RFC 6455). Requires a local session
cookie (same restriction as `/api/shell/setting`). `node` selects the
target: omitted or this node's own ID spawns a shell locally; any other
value must be a currently-managed peer, and the session is relayed there
over a second, internal hop.

Protocol once upgraded:
- **Binary frames** carry raw PTY bytes in both directions.
- **Text frames** carry small JSON control messages:
  `{"type":"resize","rows":N,"cols":N}` (client → server),
  `{"type":"exit","code":N}` (server → client, session ended),
  `{"type":"error","message":"..."}` (either direction).

### `POST /api/shell/hijack`

The inner relay target used when one node forwards a shell session to a
managed peer. Not meant to be called directly by a script or browser —
it's reached over the overlay by another gravinet node acting as the
relay for `/api/shell/ws`, using a raw hijacked connection and a private
framing format, not ordinary HTTP request/response semantics. Documented
here only so its presence in the route table isn't a mystery.

## Upgrades

Upgrades are always **from source** — a node compiles and applies its own
binary with its own Go toolchain; there is no prebuilt-binary artifact
involved anywhere in this flow. Every endpoint except `remote-apply`
requires a genuine local session and explicitly refuses the Manager-overlay
bypass other endpoints accept (see each one's note).

### `GET /api/upgrade`

This node's own upgrade state:

```json
{
  "enabled": true, "version": "586", "target": "/usr/local/bin/gravinet",
  "state_dir": "/etc/gravinet/upgrades", "pam": true,
  "phase": "idle", "from": "", "to": "", "boots": 0,
  "pre_peers": 0, "peers_now": 4, "last_error": "",
  "confirm_seconds": 90, "rollback_available": false
}
```

`enabled: false` (with a `reason`) means the upgrade machinery failed to
initialize on this node at all — everything else in this section will
refuse with an error.

### `POST /api/upgrade/source`

Uploads a gravinet source archive (`.tgz`/`.tar.gz` or `.zip`, detected by
content) as the **raw request body** — not multipart — and this node
builds it with its own toolchain, preflights the result against its own
config, and swaps it in behind a confirm-or-rollback guard: if the new
binary can't rejoin the mesh within `confirm_seconds`, it reverts itself
automatically.

```
POST /api/upgrade/source
Content-Type: application/octet-stream

<raw archive bytes>
```

Response is whatever the daemon's own `apply` control operation returns
(relayed verbatim), typically including the new phase/version on success,
or `{"error": "..."}` on a build/preflight failure.

### `POST /api/upgrade/rollback`

Backs out an upgrade that already committed but turned out bad in a way
the automatic guard didn't catch (a regression a health check has no
opinion on).

```json
{"ok": true, "rolling_back_to": "585", "restarting": true}
```

The rollback and restart happen shortly after the response is sent.

### `POST /api/upgrade/push`

Distributes one uploaded source archive to several managed peers at once
— the fleet-wide rollout action. Multipart POST with two parts:

- `nodes` — a JSON array of peer node IDs (must arrive first)
- `source` — the archive file

```json
{"sha256": "a1b2c3...", "pushed": 3,
 "results": [
   {"node": "<id1>", "ok": true, "status": 200},
   {"node": "<id2>", "ok": false, "error": "does not accept Manager-pushed upgrades"}
 ]}
```

Each target peer only accepts the push if it has separately opted in via
`/api/upgrade/accept-manager` — see below. Up to 4 peers are built
concurrently. This endpoint is local-only and is one of the paths the
fleet proxy explicitly refuses to forward (see
[Fleet management](#fleet-management-managedmanager)).

### `GET/POST /api/upgrade/accept-manager`

A peer's opt-in to accept a source push from a directly-connected Manager.
**Off by default**; while off, `/api/upgrade/remote-apply` behaves as if
it doesn't exist.

```json
{"on": true}
```

Local-only, like `/api/managed`/`/api/manager` — never toggleable through
the fleet proxy, since remotely enabling "let a peer run code on me" would
defeat the point of it being opt-in.

### `POST /api/upgrade/remote-apply`

The **one** upgrade endpoint a peer can reach at all — and only when the
target has opted in via `/api/upgrade/accept-manager`, and only from a
Manager it holds a **live direct** mesh session with (not one known only
through gossip). This is what `/api/upgrade/push` calls on each target
peer; there's normally no reason to call it directly.

Multipart POST: a `sha256` field (must arrive first) declaring the
archive's digest, then a `source` field with the archive itself. The
digest is checked before anything is extracted or built; a mismatch is
refused outright. On success, the archive goes through the exact same
build/preflight/confirm-or-rollback path as a local upload.

## Diagnostics & monitoring

### `GET /api/metrics?minutes=60`

Rolling CPU/memory/disk and per-interface throughput history (clamped to
1–60 minutes; default 60).

```json
{
  "available": true, "sample_interval": 5, "server_now": 1753142400,
  "cpu": [ {"t": 1753142395, "v": 12.4} ],
  "mem": [ {"t": 1753142395, "v": 41.2} ],
  "disk": [ {"t": 1753142395, "v": 63.0} ],
  "disk_path": "/",
  "ifaces": [ {"network": "office", "iface": "gravinet0",
               "rx": [ {"t": 1753142395, "v": 1024.5} ], "tx": [...] } ],
  "uptime_seconds": 934211
}
```

`uptime_seconds` is omitted (not zeroed) when this platform's reader
couldn't get a value, so a client can tell "not available" apart from
"just booted."

### Packet capture

A `tcpdump`-equivalent diagnostic, read-only (no injection), gated behind
the same auth as everything else.

- **`GET /api/capture/interfaces`** → `{"interfaces": [{"name":"eth0","up":true}], "supported": true}`
- **`POST /api/capture/start`** `{"iface": "eth0"}` → `{"ok": true, "iface": "eth0"}`
- **`POST /api/capture/stop`** → `{"ok": true}`
- **`POST /api/capture/clear`** → `{"ok": true}` (empties the buffer without stopping)
- **`GET /api/capture/packets?since=<cursor>`** → poll for new packets since
  a cursor value:
  ```json
  {"running": true, "iface": "eth0", "cursor": 42, "supported": true,
   "packets": [ {"seq": 41, "time": "15:04:05.000", "summary": "10.42.0.1 → 10.42.0.2 UDP 51820"} ]}
  ```
- **`GET /api/capture/pcap`** → downloads the rolling buffer (up to 5000
  packets / 32 MiB, whichever comes first) as a standard `.pcap` file
  (`Content-Type: application/vnd.tcpdump.pcap`).

### Speed test

Measures overlay throughput between two managed peers. `source`/`sink`
are the low-level data-plane endpoints a *target* peer exposes for a test
another node is running against it — not meant to be called directly.

**`POST /api/speedtest/run`** (called on the *client* node):

```json
{"target_ip": "10.42.0.5", "target_port": 8443, "target_hostname": "office-router"}
```

```json
{
  "download": {"samples": [{"t":0.25,"mbps":94.2}], "avg_mbps": 91.7,
               "bytes": 45800000, "duration_sec": 4.0,
               "packets_per_sec": 8120, "packet_samples": [...]},
  "upload": { /* same shape */ }
}
```

`packets_per_sec`/`packet_samples` are omitted when the target couldn't be
matched in this node's own peer list at measurement time — treat as "not
available," not zero.

### `GET /api/localroutes`

This host's real kernel routing table. Structured on Linux (parsed
directly from `/proc/net/route` and `/proc/net/ipv6_route`); on other
platforms, the raw text of the OS's native route-listing command.

```json
{"entries": [ {"Dest":"10.42.0.0/16","Gateway":"","Iface":"gravinet0","Metric":0,"Family":4} ], "os": "linux"}
```

On non-Linux: `{"entries": [], "text": "...", "os": "darwin"}` (or
similarly for other platforms), possibly with an `error` field.

### `GET /api/localhosts`

Raw contents of this host's hosts file (the same file the daemon writes
peer/advertised records into): `{"text": "127.0.0.1 localhost\n...", "path": "/etc/hosts"}`.

### `GET /api/localdns`

What's actually registered with this host's OS resolver right now, per
network — read live (`resolvectl` on Linux, `/etc/resolver` on macOS, NRPT
on Windows, local-unbound forward zones on FreeBSD), not from anything
gravinet remembers applying:

```json
{"networks": [ {"name": "office", "iface": "gravinet0", "text": "..."} ], "os": "linux"}
```

### `GET /api/latency`

Pings every peer on every network over the overlay (not the underlay) —
so it measures the mesh path itself, concurrently, bounded by one ping
timeout rather than the sum of all pings:

```json
{"networks": [
  {"name": "office", "peers": [
    {"node_id": "<id>", "hostname": "office-router", "overlay": "10.42.0.5",
     "rtt_ms": 4.2, "ok": true}
  ]}
]}
```

A peer with no overlay address yet (still handshaking) appears with
`"ok": false` and an `error`, rather than being silently skipped.

### `GET /api/logs`

```
GET /api/logs?n=1000
GET /api/logs?download=1
```

Tails the daemon log. `n` (default 1000, max 10000) caps the number of
lines returned as `{"lines": [...], "path": "...", "enabled": true}`.
`download=1` instead returns the whole file (capped at 64 MiB) as
`{"text": "...", ...}`. `enabled: false` means file logging isn't
configured on this node at all.

### `POST /api/logs/clear`

Truncates the active log file (only when file logging is enabled):
`{"ok": true}`, or `{"ok": false, "error": "file logging is disabled"}`.

### `GET/POST /api/loglevel`

```
GET  → {"level": "info", "levels": ["error","warn","info","debug"]}
POST {"level": "debug"} → {"ok": true, "level": "debug", "restart": false}
```

Applied live — useful for turning up verbosity mid-investigation without
losing mesh session state to a restart.

### `GET/POST /api/logsize`

```
GET  → {"size": "200M"}
POST {"size": "1G"} → {"ok": true, "size": "1G", "restart": false}
```

`size` accepts a human size (`"200M"`, `"1G"`, `"99K"`) or a bare byte
count. Applied live — shrinking the cap trims the on-disk file
immediately.

## System & service

### `POST /api/restart`

Restarts the gravinet service via the platform's own service manager
(`systemctl`, `launchctl`, `Restart-Service`, `service(8)`). Replies
first, then restarts about 700ms later (so this process is about to
disappear):

```json
{"ok": true, "restarting": true}
```

or, if this platform/install has no way to self-restart,
`{"error": "..."}` with a hint.

### `GET /api/about`

```json
{"gravinet_version": "586", "gravinet_commit": "a1b2c3d",
 "os": "linux", "os_version": "Ubuntu 24.04.1 LTS (kernel 6.8.0)",
 "arch": "amd64", "go_version": "go1.23.4"}
```

## Documentation endpoints

Three endpoints serve this node's own bundled documentation files as
JSON, read fresh from disk each request (so an updated file shows without
a restart):

- **`GET /api/readme`** — the project README.
- **`GET /api/license`** — the project LICENSE.
- **`GET /api/getting-started`** — `getting-started.md`.

All three share one shape:

```json
{"text": "# gravinet\n\n...", "path": "/usr/local/share/gravinet/README.md", "available": true}
```

`available: false` (with an empty `text`) means the file wasn't found or
this node wasn't told where to find it — not an HTTP error.

## Data type reference

Full field lists for the recurring object shapes referenced above.

**`PeerInfo`** (a connected peer, in `/api/status`):
`node_id`, `hostname`, `overlay4`/`overlay6`, `endpoint` (observed
underlay source), `relayed` (bool), `relay_via`, `bgp_asn`, `version` (the
gravinet build the peer reports for itself; omitted when the peer predates
the field — render as unknown, not as "no version"), `tx_bytes`/
`rx_bytes`, `tx_packets`/`rx_packets`, `rtt_ms`, `notes`, `key_label`,
`transport` (`"udp"`/`"tcp"`), `established_at_unix_nano`, `path_mtu`,
`frags_sent`/`frag_send_drop`/`frags_rcvd`/`reasm_ok`/`reasm_drop`/
`spoof_drop`.

**`BanInfo`**: `target`, `hostname`, `origin` (node id that issued it),
`origin_hostname`, `notes`, `at_unix_nano`, `mine` (bool — can this node
unban it).

**`DisabledPeerInfo`**: `node_id`, `hostname`, `notes`.

**`RouteInfo`**: `cidr`, `via` (origin node id), `metric`.

**`FirewallRule`**: `id`, `disabled`, `action` (`allow`/`deny`),
`direction` (`in`/`out`/`both`), `proto` (`tcp`/`udp`/`icmp`/`any`),
`src`/`dst` (CIDR, host, `"any"`, or an object name), `src_negate`/
`dst_negate`, `sport_min`/`sport_max`, `dport_min`/`dport_max`,
`services` (named service-catalog entries, unioned with `proto`/ports),
`services_negate`, `log`, `notes`, and (read-only) `packets`/`bytes` hit
counters.

**`FirewallObject`** (node-global address catalog): `name`, `kind`
(`host`/`subnet`/`range`/`fqdn`/`group`), `addresses` (literals/CIDRs/
ranges/FQDNs — non-group kinds), `members` (other object names — `group`
kind), `notes`.

**`FirewallService`** (node-global service catalog): `name`, `ports`
(array of `{"proto":"tcp","port_min":53,"port_max":53}`), `notes`.

**`FirewallExempt`**: `name`, `proto`, `port` (`0` = any/port-less,
matches source or destination), `mgmt` (bool — follow the live admin
port), `disabled`.

**`Route`** (advertised): `cidr`, `metric`, `enabled`.

**`RejectRoute`**: `cidr`, `inclusive`, `disabled`.

**`Seed`**: `address`, `notes`.

**`HostRecord`**: `name`, `ip`, `disabled`.

**`HostReject`**: `name`, `disabled`.

**`DNSForward`**: `domain`, `servers` (array of resolver IPs, tried in
order), `disabled`.

**`DNSReject`**: `domain`, `disabled`.

**`NATRule`**: `source`, `dest` (CIDRs, blank = any), `translate`
(`"masquerade"` / a literal IPv4 / `"port-forward:<ipv4>"`), `interface`
(egress interface for masquerade), `enabled`.

**`QoSRule`**: `protocol` (`tcp`/`udp`/`icmp`/`any`), `port_min`/
`port_max`, `services` (named catalog entries), `dscp` (nullable — `null`
= any), `class`, `disabled`.

**`BGPConfig`**: `enabled`, `asn`, `router_id`, `neighbors` (array of
`BGPNeighbor`), `networks` (array of CIDR strings originated directly),
`redistribute_connected_routes`, `redistribute_static_routes`,
`redistribute_mesh_routes` (all arrays of CIDR strings),
`as_prepend` (bool), `keepalive_time`/`hold_time` (seconds; `0` = FRR
default), `auto_bgp` (bool — self-numbering, self-peering mode; see the
field's own extensive doc comment in `internal/config/config.go` if
driving this by hand).

**`BGPNeighbor`**: `peer` (address), `remote_as`, `description`,
`password` (MD5 session password), `bfd` (bool), `shutdown` (bool —
administratively holds this one session down without removing it).

**`seedInfoResult`** (from `/api/seed-info`/`/api/peer-info`): `host`,
`isIP`, `forward` (array)/`forwardErr`, `reverseTarget`/`reverse`
(array)/`reverseErr`, `whoisTarget`/`whois`/`whoisErr`, `geoEnabled`
(bool)/`geoTarget`/`geo` (object)/`geoErr`. Every `*Err` field is populated
only on failure; a successful section omits its error field.

## Examples

All examples assume a node reachable at `https://127.0.0.1:8443` (the
default) and use `-k` to trust its self-signed certificate. Log in once
and reuse a cookie jar for every other call:

```bash
HOST=https://127.0.0.1:8443

curl -sk -c cookies.txt -X POST $HOST/api/login \
  -d '{"user":"admin","pass":"hunter2"}'
```

Every example below assumes that cookie jar (`-b cookies.txt`) exists.
Replace `office`, node IDs, addresses, etc. with values from your own
node — most examples read more clearly after a look at `/api/status` or
`/api/config` first.

### Check node status

```bash
curl -sk -b cookies.txt $HOST/api/status | jq '.nets[0].peers'
```

### Read the stored configuration

```bash
curl -sk -b cookies.txt $HOST/api/config | jq .
```

### Add a network

```bash
curl -sk -b cookies.txt -X POST $HOST/api/network \
  -d '{"op":"add","net":"office"}'
```

Subnets are auto-picked if left blank; to control them explicitly, add
`"subnet4":"10.50.0.0/16","subnet6":"fd00:50::/64"` to the body.

### Mint a join token and use it on another node

```bash
curl -sk -b cookies.txt -X POST $HOST/api/network/token \
  -d '{"net":"office","addr":"203.0.113.9:51820","expires":"24h"}' | jq -r .token
```

Paste the resulting token into the *other* node's own login session:

```bash
curl -sk -b other-node-cookies.txt -X POST https://<other node>:8443/api/network \
  -d '{"op":"join-token","token":"<token from above>"}'
```

### Ban and unban a peer

```bash
curl -sk -b cookies.txt -X POST $HOST/api/ban \
  -d '{"net":"office","node":"<node id>","notes":"compromised key"}'

curl -sk -b cookies.txt -X POST $HOST/api/unban \
  -d '{"net":"office","node":"<node id>"}'
```

### List, add, and remove firewall rules

```bash
curl -sk -b cookies.txt "$HOST/api/firewall?net=office" | jq '.rules'

curl -sk -b cookies.txt -X POST $HOST/api/firewall \
  -d '{"net":"office","op":"add","rule":{"action":"deny","direction":"both","src":"10.42.0.99"}}'
```

Removing a rule needs its `id` from the list above:

```bash
curl -sk -b cookies.txt -X POST $HOST/api/firewall \
  -d '{"net":"office","op":"del","ids":[<id>]}'
```

### Advertise a route across the mesh

```bash
curl -sk -b cookies.txt -X POST $HOST/api/route \
  -d '{"op":"add","net":"office","cidr":"192.168.50.0/24","metric":0}'
```

### Give overlay traffic internet access (NAT masquerade)

```bash
curl -sk -b cookies.txt -X POST $HOST/api/nat \
  -d '{"op":"add","net":"office","iface":"eth0"}'
```

### Cap a network's bandwidth

```bash
curl -sk -b cookies.txt -X POST $HOST/api/bandwidth \
  -d '{"net":"office","dir":"up","bps":10000000}'
```

10,000,000 bytes/sec ≈ 80 Mbit/s. Use `"dir":"down"` or `"dir":"both"` for
the other directions; `{"op":"enable"}` / `{"op":"disable"}` turns
throttling on/off without touching the cap itself.

### Generate a new rotation key and reveal it

```bash
curl -sk -b cookies.txt -X POST $HOST/api/key \
  -d '{"net":"office","op":"generate","slot":1,"label":"2026-Q3"}'

curl -sk -b cookies.txt -X POST $HOST/api/key \
  -d '{"net":"office","op":"reveal","slot":1}' | jq -r .key
```

### Turn up logging while chasing a bug, then back down

```bash
curl -sk -b cookies.txt -X POST $HOST/api/loglevel -d '{"level":"debug"}'

curl -sk -b cookies.txt "$HOST/api/logs?n=200" | jq -r '.lines[]'

curl -sk -b cookies.txt -X POST $HOST/api/loglevel -d '{"level":"info"}'
```

### Run a speed test against a peer

Get the peer's overlay address and web port from `/api/cluster` first:

```bash
curl -sk -b cookies.txt $HOST/api/cluster | jq '.peers[] | {node_id,hostname,overlay,web_port}'

curl -sk -b cookies.txt -X POST $HOST/api/speedtest/run \
  -d '{"target_ip":"10.42.0.5","target_port":8443}' \
  | jq '{download_mbps: .download.avg_mbps, upload_mbps: .upload.avg_mbps}'
```

### See which nodes are running which build

```bash
curl -sk -b cookies.txt $HOST/api/cluster \
  | jq -r '.peers[] | "\(.hostname) - \(.node_id[0:8]) - v\(.version // "?")"'
```

Every peer reports its own build version, relayed across the mesh, so this
answers "what's behind?" for the whole fleet from one node without logging
into each one. The same values back the version column in
Monitor → Mesh Peers and the labels in the upgrade peer picker.

### Drive a peer through the fleet proxy

With Manager mode on locally and the target already in Managed mode (see
[Fleet management](#fleet-management-managedmanager)):

```bash
curl -sk -b cookies.txt -X POST $HOST/api/manager -d '{"on":true}'

curl -sk -b cookies.txt "$HOST/api/proxy?node=<peer node id>&path=/api/status" | jq .
```

Any `/api/...` path can follow `path=`, so this is also how to, say, add a
firewall rule on a peer without logging into it separately — just proxy a
normal `POST /api/firewall` body through.

### Push a source upgrade to several peers at once

Each target must have separately opted in
(`POST /api/upgrade/accept-manager {"on":true}` run *on that peer*) before
this will do anything:

```bash
curl -sk -b cookies.txt -X POST $HOST/api/upgrade/push \
  -F 'nodes=["<peer1 id>","<peer2 id>"]' \
  -F 'source=@gravinet-src.tgz' | jq .
```

### Restart the service

```bash
curl -sk -b cookies.txt -X POST $HOST/api/restart
```

### Log out

```bash
curl -sk -b cookies.txt -X POST $HOST/api/logout
```

## Known gaps

`internal/webadmin/cluster.go`'s management-proxy blocklist refers to
three paths — `/api/upgrade/rollout`, `/api/upgrade/stage`, and
`/api/upgrade/fleet` — that are **not** registered anywhere in the route
table (`internal/webadmin/webadmin.go`'s `handler()`). The actual
fleet-push endpoint is `/api/upgrade/push`, documented above under
[Upgrades](#upgrades). Those three other paths appear to be leftover
references to an earlier or planned naming that never shipped (or was
renamed away from); calling them returns a plain 404, not a documented
behavior. This document only describes endpoints that actually exist.
