# Changelog

## About this document

This changelog was reconstructed by searching back through past conversation
history, not by walking a version-control log — [gravinet] doesn't have one
available to this tool. That method has real limits worth stating plainly:

- **Search returns snippets, not full transcripts.** Even for a conversation
  this found, it may only surface part of what was discussed or changed.
- **Coverage is uneven.** The version counter (the `version` string in
  `cmd/gravinet/main.go`) is currently at **353**, and jumps by exactly one
  on every recorded change — but only a fraction of those ~270-odd increments
  turned up specific, citable detail. Entries below with a version number
  are ones a past conversation explicitly named; the gaps between them are
  real gaps in what's recoverable this way, not evidence that nothing
  happened in between. A second search pass (this session) went further back
  than the first and recovered a fair number of versions the original pass
  missed — v202, v203, v207–209, v241–245, v259–262 below are new this
  round. **v273 specifically could not be recovered**: the version string
  jumps straight from 272 to 273 with no search result describing what
  changed, so it's left as an acknowledged gap rather than a guess.
- **This project shares history with a separate, unrelated project** (a
  Rust-based firewall console called `parapet` / `rampart`, and a ZFS
  management tool) that came up in some of the same search results.
  Anything that couldn't be clearly confirmed as [gravinet] specifically was
  left out rather than guessed at.

Versions **263–272** are from a single long conversation and are complete
and precise — every change in that range was made directly in that session,
not reconstructed from a search snippet. **v274** and **v306–353** are each
their own session, also direct and precise. Everything else is a
best-effort reconstruction. If a
specific version or feature isn't listed here and you
want it filled in, it's worth asking to search for it directly rather than
assuming it didn't happen.

---

## v515 — 2026-07-18

**Simplified the "Redistribute mesh routes" toggle's description text
(Traffic › BGP).**

"...Advertise table (none right now), not every kernel route on this
host." is now just "...Advertise table (0)" — a plain count, no "right
now"/"none right now" phrasing and no trailing kernel-route clause.

---

## v514 — 2026-07-18

**Strengthened the confirmation shown when editing a network's subnet4/
subnet6 in place (Mesh › networks) to explain the actual failure mode, not
just that a restart happens.**

Changing a network's subnet was already allowed and already forced a
restart with a confirm() first — but the confirm only said "the same
change must be made on every other node," which understates the risk:
gravinet has no protocol-level check for a fleet that's only partway
migrated. Each node installs its own on-link kernel route scoped to its
own configured subnet (`assignAddr`, `internal/mesh/addressing.go`), so a
peer still on the old range simply stops being reachable from a node
that's moved to the new one — no error, nothing in the logs pointing at
the mismatch, it just looks like that peer dropped off. The confirm()
text (`startInlineEdit`, `ui.go`) now says so explicitly, so a mismatch
gets diagnosed in seconds instead of an hour of debugging a "dead" peer.
address4/address6's own confirm (a plain restart notice — re-addressing a
live interface isn't hot-reloadable, but there's no cross-node
coordination risk to explain there) is unchanged.

New `TestSubnetChangeWarnsOfSilentPeerMismatch`
(`ui_dom_helper_test.go`) scans for the specific wording, in the same
style as the existing JS-behavior guard tests (this package has no JS
runtime in its test suite).

---

## v513 — 2026-07-18

**Fixed: a long IPv6 address in Mesh › peers / Monitor › mesh peers got cut
off with no way to see the rest, and endpoints showed with brackets around
the address.**

Two related display bugs in the peer tables that v511 (dual-stack overlay
display) made visible for the first time — a v4-only peer's short address
never hit either:

1. **Truncation.** `table.peers-table` (`ui.go`) is `table-layout:fixed`
   with every cell defaulting to `overflow:hidden; text-overflow:ellipsis;
   white-space:nowrap`, so the network's cards line up column-for-column
   regardless of content — but a full IPv6 address, or one paired with a
   port, is easily wider than the 13%/19% given to the overlay/endpoint
   columns, and the overflow was silently hidden behind an ellipsis with no
   tooltip or other way to read the rest. The overlay and endpoint `<td>`s
   in both `secPeers` (Mesh › peers) and `infoMeshPeers` (Monitor › mesh
   peers) now carry `ov-cell`/`ep-cell` classes with a wrapping override
   (`overflow-wrap:anywhere`), so a long value wraps onto a second line
   instead of being clipped — a short one still renders on one line, since
   the wrap only kicks in once content actually doesn't fit.

2. **Brackets.** The underlay endpoint (`p.endpointText`, and the "This
   node: … public endpoint" line's `state.nat.public`) comes straight from
   Go's `netip.AddrPort.String()`, which brackets an IPv6 host exactly the
   way `net.JoinHostPort` does — `"[fd00::2]:51820"` — correct and
   necessary for anything that gets reparsed (the `/api/peer-info` request
   body, seed-address matching), wrong for a read-only table cell. New
   `dispAddr` (`ui.go`) strips the brackets for display only, reusing
   `splitHostPort`'s own bracket-aware parsing so the two can't disagree
   about where the address ends and the port begins; applied at every
   display site — both tables' endpoint cells, the NAT banner's public
   endpoint, and the peer-info lookup dialog's "looking up …" line — while
   the raw `p.endpoint` passed to the API call itself is untouched.

`ui_dom_helper_test.go` gained two scan-based guards
(`TestPeerAddressCellsWrapInsteadOfTruncating`,
`TestPeerAddressDisplayStripsIPv6Brackets`) in the same style as v511's
dual-stack-overlay one, since this package has no JS runtime in its test
suite.

---

## v512 — 2026-07-18

**Added: a "Redistribute mesh routes" toggle in Traffic › BGP, scoped to
exactly the Mesh Routes page rather than the whole kernel routing table.**

A mesh-learned route is installed into the OS routing table like any other
kernel route (`internal/mesh`'s `syncRoute`/`AddRoute`) — so FRR's own
`redistribute kernel` was never a safe way to put the mesh into BGP: it
would sweep in *every* kernel-table entry on the box, mesh-originated or
not (a manually-added static route, another VPN's routes, whatever else is
there). The new toggle takes a narrower, explicit path instead: gravinet
renders one `network <cidr>` statement per CIDR currently on the Mesh
Routes page's Advertise table (`meshRouteCIDRs`, `internal/webadmin/bgp.go`)
— the same mechanism `BGPConfig.Networks` already uses for manually-typed
prefixes (and deduplicated against it, `effectiveBGPNetworks` in
`frr.go`), just sourced live from config instead of typed in by hand.
Disabled routes and routes on a disabled network are excluded, matching
what the mesh engine itself is actually carrying.

This has to stay live, not just apply at the next BGP save: adding,
removing, or enabling/disabling a route on the Mesh Routes page (`/api/route`)
now also reconciles FRR when RedistributeMesh is on
(`reconcileMeshRedistribute`), and the same applies to a network being
enabled, disabled, or deleted out from under its routes. A new
`meshRedistributeRemovesSomething` (`frr.go`) extends the existing
remove-forces-restart logic (`bgpConfigRemovesSomething`) to this
mesh-derived side: a `vtysh -b` reload can only ever add lines from a
freshly-written frr.conf into the running daemons, never retract one that's
no longer there, so a mesh route dropping off the page forces a real
restart the same way a manually deleted network already did.

`config.BGPConfig` gains `RedistributeMesh bool`
(`redistribute_mesh` in JSON). The BGP editor shows the toggle beside the
existing Redistribute connected/static rows, with a live count of how many
CIDRs are currently on the Mesh Routes page. `GET /api/bgp/config` now also
returns `mesh_routes` (the same list) so the UI has it without a second
request. Imported/reflected FRR configs (`handleBGPImport`) never set this
field — it has no FRR keyword of its own, only the `network` lines it
causes, indistinguishable on import from manually-typed ones — but that
path only ever runs before gravinet is managing BGP in the first place, so
there's nothing of this feature's for it to have produced yet.

---

## v511 — 2026-07-18

**Fixed: a dual-stack peer's IPv6 overlay address never showed up in
Mesh > peers or Monitor > mesh peers — only its IPv4 address did.**

`mesh.PeerInfo` (the struct behind every peer entry in `/api/status`)
already carries both `overlay4` and `overlay6` as independent fields —
the handshake genuinely exchanges both, and the backend was never the
problem. The bug was entirely in the admin UI: `peerRowsForNet` (`ui.go`)
folded them into a single `p.overlay` value with
`Overlay4||...||Overlay6`, picking v4 whenever a peer had one at all, and
every table that showed a peer's overlay column rendered only that one
value. Only a genuinely v6-only peer (no v4 assigned) ever showed v6 —
which is exactly why this shipped unnoticed: nothing looks broken unless
you're specifically checking a peer that has both.

`peerRowsForNet` now carries `overlay4`/`overlay6` as their own fields on
every row (self included), independent of `p.overlay` — which
deliberately stays a single value, since `peerOverlayEdit`'s inline
editor only ever targets one family's config field at a time (typing a
combined string there would break the "does this look like a v6 address"
heuristic that already decides which field to save). A new shared
`overlayCellHTML(p)` helper renders both, stacked (v4 on the first line,
v6 dimmed below it) when a peer has both, or whichever one it has
otherwise — used by both known render sites: Mesh > peers' overlay cell
(both its editable-peer and read-only branches) and Monitor > mesh
peers'. The search index (⌘K-style peer search) now matches on both
addresses too, not just whichever `p.overlay` picked.

New test `TestDualStackOverlayAddressNotCollapsedToOneFamily`
(`ui_dom_helper_test.go`) scans the served page directly for this wiring
— this package has no JS runtime in its test suite, same reasoning as
the existing `TestNoStandaloneTrOrTdViaInnerHTML` guard — checking that
`overlayCellHTML` still exists, is still called from all 4 places (its
own definition plus 3 call sites), and that `peerRowsForNet` still
carries `overlay4`/`overlay6` on both self and peer rows. Verified
meaningful by temporarily reverting one call site back to `esc(p.overlay)`
and confirming the test fails, then restoring and confirming it passes.

---

## v510 — 2026-07-18

**Fixed a flaky test in v509's multi-port UPnP suite** —
`TestManagerPartialFailureStillMapsTheRest` (and, latently, a couple of
its neighbors) could fail under load with a spurious "context canceled"
on an otherwise-successful mapping. The test itself, not `Manager`, was
at fault: it waited for `Manager` to finish processing a mapping by
polling the *fake server's* request counter, but that counter increments
the instant the server receives a request — strictly before the client
finishes reading the response and `mapAll` records success in
`m.mapped`. A `Stop` call landing in that narrow window can cancel the
still-in-flight response read, so the mapping legitimately never gets
recorded as live even though the server had already handled it
correctly — and `Stop` then rightly skips deleting a mapping it never
believed existed. `Manager`'s own behavior here is correct and desired
for a real shutdown race (the mapping just expires via its own lease);
only the test's synchronization was wrong.

New test helper `Manager.mappedSnapshot()` returns the mapping set under
`m`'s own mutex — the same state and lock `Stop` itself reads — so tests
can wait on "`Manager` has durably recorded this result" instead of a
proxy that can observe an in-flight, not-yet-fully-processed request.
Applied to `TestManagerMapsMultiplePortsOnStartAndRemovesOnStop`,
`TestManagerPartialFailureStillMapsTheRest`, and
`TestManagerStopIsIdempotent`. Confirmed with 15 consecutive full-suite
runs under `-race` plus a `-cpu=1,2,4` sweep, all clean, after previously
reproducing the failure once under full-suite load.

---

## v509 — 2026-07-18

**UPnP (v508) now covers every port this node listens on, not just the
primary UDP port: the TCP/TLS fallback port and any configured extra UDP
or TCP listen ports are mapped too.**

`internal/upnp.Manager` was rearchitected around a *set* of port mappings
sharing one discovered gateway, rather than one Manager per port: `New`
`Manager` now takes `[]PortMapping{Port, Protocol}` instead of a single
port/protocol pair. One shared SSDP discovery for the whole set is both
more polite to the router than a discovery burst per port and means a
single `Manager` instance is still the one thing `cmd/gravinet/main.go`
drives. Mappings are independent of one another — if the router rejects
one (a conflicting entry, say), the rest still proceed and stay mapped;
only a cycle where *none* of them succeed backs off and rediscovers from
scratch. `Manager.Stop` only issues `DeletePortMapping` for whichever
mappings actually went live, never for ones that were only ever attempted
and rejected.

`cmd/gravinet/main.go` now builds that set from four sources: the primary
UDP port and the TCP/TLS fallback port use the port that *actually bound*
(`tr.Port()`/`tlsTr.Port()`, gated on `tlsTr.HasPrimary()`) rather than
the configured value — the UDP primary in particular can silently fall
back to a different port than `cfg.PrimaryPort` (see `transport.Open`'s
doc comment and `config.FallbackUDPPorts`), and mapping a
configured-but-never-bound port would forward WAN traffic at a port
nothing is listening on. `cfg.ExtraListenPorts`/`cfg.ExtraTCPListenPorts`
don't have an equivalent "what actually bound" accessor exposed by
`internal/transport`, so those are mapped straight from config — the same
best-effort spirit their own local bind already has (see those fields'
doc comments): a port that never actually bound locally just wastes a WAN
mapping nothing answers, not a hazard.

Settings > NAT > UPnP's description and the settings-search index entry
were reworded to reflect the wider coverage; `config.Config.EnableUPnP`
and `handleUPnPSetting`'s doc comments updated the same way.

New/changed tests in `internal/upnp/manager_test.go` (rewritten for the
new `[]PortMapping` API): `TestNewManagerDropsInvalidAndDuplicateMappings`,
`TestManagerMapsMultiplePortsOnStartAndRemovesOnStop` (same port mapped
under both UDP and TCP counts as two independent mappings, not a
duplicate), `TestManagerPartialFailureStillMapsTheRest` (one rejected
port doesn't block the others, and `Stop` deletes only what actually
mapped), `TestManagerRetriesWhenEveryMappingFails` (renamed/generalized
from v508's single-port failure test), and `TestManagerWithNoMappingsIsInert`
(UPnP on but nothing to map — e.g. UDP off with no extra ports — is a
safe, prompt no-op, not a hang). All race-clean under `-race`.

---

## v508 — 2026-07-18

**Added UPnP support: a Settings toggle (Traffic-adjacent Settings > NAT
> UPnP) that asks the LAN router to auto-forward the primary UDP port to
this node.** Off by default. This is the standard "auto-configure my
router" NAT-traversal convenience — a node behind a home/office router
with no manual port forward can still be reached directly by peers,
without the operator ever opening the router's own admin UI.

New package **internal/upnp** (no third-party dependencies):

- `client.go` — a minimal UPnP Internet Gateway Device client:
  `Discover` does SSDP multicast discovery (M-SEARCH to
  239.255.255.250:1900) and resolves the router's port-mapping control URL
  from its device description document, checking for a WANIPConnection
  service first and falling back to the older WANPPPConnection (DSL/PPPoE
  routers). `Gateway.AddPortMapping`/`DeletePortMapping`/
  `GetExternalIPAddress` are the three SOAP actions gravinet needs,
  hand-built over `net/http` — a SOAP Fault (which arrives as an HTTP 500,
  per SOAP 1.1) is parsed for the UPnP error code/description rather than
  treated as an opaque HTTP failure.
- `manager.go` — `Manager` owns a mapping's background lifecycle: discover
  once, map, renew every 25 minutes (well under the 1-hour lease — plenty
  of consumer routers mishandle a "forever" lease request, so a finite
  lease with renewal is safer than relying on 0), and best-effort remove
  the mapping on `Stop`. Every discovery/mapping failure is logged and
  retried in the background (a router with UPnP off, or absent entirely,
  is an expected, silent no-op — never fatal to startup).

Wired into **config.Config** as `EnableUPnP` (`enable_upnp`, `omitempty` —
absent from a saved file decodes to the off default) and into
**cmd/gravinet/main.go**: a `Manager` is created and started once at
startup, right after the primary UDP transport is attached, when
`cfg.EnableUPnP && cfg.PrimaryPort > 0`. Deliberately *not* wired into
`reloadFn`'s live-config-diff logic — like `handleGeoIPSetting`'s
GeoIP-lookup toggle, this is a "takes effect on next restart" setting;
unlike GeoIP's reason (a startup-frozen `s.cfg` copy), here it's that the
`Manager` itself is only ever started once, not something a live reload
currently knows how to start/stop mid-run. On shutdown, `Manager.Stop` is
called with a 1.5s bound (one of several steps sharing the 5s
`shutdownGrace` budget) so a router that's gone unreachable can't hang
process exit; the mapping just lingers until its own lease expires in
that case.

**webadmin**: new `handleUPnPSetting` (`POST /api/upnp`, restart-required
response, same shape as `handleGeoIPSetting`), `enable_upnp` added to the
`/api/config` response (read fresh from disk, like the port fields
alongside it — not from the startup-frozen `s.cfg`, since `EnableUPnP`
isn't a `WebAdmin`-scoped field), and a new toggle row in Settings > NAT,
right below NAT state timeout, with the same restart-triggering UX as the
Geo-IP toggle in Settings > Privacy.

**Scope note:** only the primary UDP port is mapped — the TCP/TLS
fallback port and any configured extra listen ports are not (yet)
auto-mapped by this first version. Happy to extend `Manager` to cover
those too if wanted.

New tests: `internal/upnp/client_test.go` (SSDP LOCATION parsing across
header casings, device-description parsing including realistic
multi-level `deviceList` nesting and the WANIPConnection-preferred-over-
WANPPPConnection case, control-URL resolution, and SOAP call/response
handling — including a SOAP Fault surfacing its UPnP error code — against
real `httptest` servers rather than hand-rolled stubs);
`internal/upnp/manager_test.go` (start/map/renew/stop lifecycle, retry
behavior on both discovery and mapping failure, idempotent `Stop`, and
`Stop`-before-`Start` as a safe no-op, all exercised through an injectable
`discover` seam — same pattern as this codebase's existing `statFile` seam
— against a fake IGD HTTP server, all race-clean under `-race`); and
`internal/webadmin/upnp_handler_test.go` (mirrors the existing
`TestHandleGeoIPSetting`, adjusted for `enable_upnp` being read fresh
from disk rather than from a frozen `s.cfg`).

---

## v507 — 2026-07-18

**Added an `address-family ipv6 unicast` block, so a deactivated-in-ipv4
IPv6 neighbor (v506) actually exchanges routes instead of coming up idle.**
v506 correctly deactivated IPv6 peers in `address-family ipv4 unicast`, but
gravinet never rendered any other address-family block for them to be
activated in — leaving a v6 session established with nothing activated
anywhere, and therefore no routes exchanged over it. `renderFRR` (`frr.go`)
now:

- Splits `b.Neighbors` into v4/v6 by peer address (`isIPv6Peer`) and emits
  a plain `neighbor <peer> activate` for v6 peers under a new
  `address-family ipv6 unicast` block instead of the `no activate` it gets
  in the ipv4 block.
- Splits `b.Networks` by prefix family (new `isIPv6Network`) so a v6
  advertised prefix renders under `network` in the ipv6 block, never the
  ipv4 one — FRR rejects a `network` statement whose prefix doesn't match
  its enclosing address-family, and previously every prefix in the list
  was dumped into ipv4 unicast regardless.
- Mirrors `redistribute connected`/`redistribute static` into the ipv6
  block when it's emitted — the toggles aren't family-specific, and FRR
  needs the directive present in whichever address-family should carry it.
- Only emits the ipv6 block at all when there's an actual v6 peer or v6
  network to put in it.

Also fixed along the way: the separator between the two address-family
blocks needed to be an *indented* `" !\n"`, not the column-0 `"!\n"` the
single-address-family version used to close out the whole stanza — a
column-0 `!` is (correctly) read by `parseRunningConfigBGP`'s stanza-end
check as ending the `router bgp` block entirely, matching what real FRR
`show running-config` output does. Emitting it between the two blocks was
silently truncating the ipv6 block right out of anything read back via
import — caught by a new render→parse round-trip test before it shipped.
The lone stanza-terminating `!` now moves to the very end, after whichever
blocks were actually emitted.

No parser changes were needed: `parseRunningConfigBGP` never branched on
which `address-family` stanza a line was nested under, and already
skipped any `no `-prefixed line as a negation — so both blocks, and the
`no neighbor <v6> activate` line from v506, round-trip through it as-is.

New tests: `TestFRRIPv6NeighborDeactivatedInIPv4ActivatedInIPv6`,
`TestFRRNoIPv6AFBlockWhenUnneeded`, `TestFRRIPv6AFBlockFromNetworkAlone`,
`TestFRRNetworksSplitByFamily`, `TestFRRIPv6RenderParseRoundTrip`. The
existing v506 test (`TestFRRIPv6NeighborDeactivatedInIPv4AF`) was replaced
since its old assertion — that a v6 peer should never get a plain
`activate` line anywhere — is no longer the intended behavior.

---

## v506 — 2026-07-18

**IPv6 BGP neighbors are now explicitly deactivated in
`address-family ipv4 unicast`.** FRR (mirroring Cisco here) activates every
configured neighbor under `address-family ipv4 unicast` by default,
regardless of the peer's own address family — so an IPv6-addressed
neighbor was getting swept into that same implicit IPv4 unicast activation
alongside its own session. `renderFRR` (`frr.go`) now detects an IPv6
literal peer (new `isIPv6Peer`, via `net.ParseIP`) and emits
`no neighbor <peer> activate` for it in that address-family block instead
of `neighbor <peer> activate`; IPv4 peers are unaffected. The FRR-config
import parser already treats any `no `-prefixed line as a skipped
negation, so reading this back in doesn't need a matching change.
New test: `TestFRRIPv6NeighborDeactivatedInIPv4AF`.

---

## v505 — 2026-07-17

**Set `autocomplete="off"` on the BGP neighbor edit fields (Traffic › BGP).**
Browsers were autofilling the neighbor row's inputs — most visibly the
description and MD5 password, which a browser sees as a generic text field
and a password field and offers saved values for. That put values into
fields the operator never typed, which is misleading for a router-config
form (the app itself never pre-fills these; a brand-new row's inputs are
empty). All four inputs — peer address, remote AS, description, and MD5
password — now carry `autocomplete="off"`, in both the add-a-row path
(`nbrAddRow`) and the in-place edit path (`startNbrEdit`). UI-string-only
change in `ui.go`; no behavior change to what's saved or rendered.

---

## v504 — 2026-07-17

**Fixed: FRR successfully installs on FreeBSD but never actually runs.**
Two separate gaps, both found by reading FRR's actual FreeBSD rc.d script
(`net/frrN/files/frr.in`) rather than guessing:

1. **Nothing was enabling FRR in the first place.** v500's
   `ensureFRRDaemonsEnabled` was a deliberate no-op on FreeBSD, on the
   reasoning that `syncDaemons` already computes the full daemon set from
   scratch on every BGP config save, so there was no Linux-style "flip two
   defaults-off flags" gap to fill. That reasoning missed what
   `ensureFRRDaemonsEnabled` actually *does* on Linux: it doesn't just edit
   a file, it forces FRR to start running (`restart` if anything changed)
   the moment FRR is detected — independent of whether anyone has saved a
   BGP config yet. On FreeBSD, `frr_enable` defaults to `"NO"` in the rc.d
   script, and with the old no-op, nothing ever set it to `"YES"` until an
   operator saved a config through Traffic › BGP. `ensureFRRDaemonsEnabled`
   now does real work here: sets `frr_daemons` to a minimal baseline
   (`mgmtd zebra staticd bgpd bfdd` — not the port's own default of nearly
   every protocol daemon it ships), sets `frr_enable="YES"`, and restarts
   FRR — mirroring Linux's actual effect, not just its mechanism.

2. **`/var/lib/frr` never got created.** FreeBSD's frr rc.d script does
   `mkdir -p /var/lib/frr; chown -R frr:frr /var/lib/frr` — FRR's runtime
   state directory, needed for daemons to fully start — but only inside its
   `start` command, never `restart`. gravinet's `applyFRRService` never
   calls `start` at all (always `restart`, even for what's effectively a
   first activation — see v500's reload-footgun writeup for why). A box
   that's never had a bare `service frr start` run against it by a human
   would have its daemons trying to come up with nowhere to write their
   state. New `frrVarLibBootstrap` (`frr_freebsd.go`) does this step
   itself: called from `ensureFRRDaemonsEnabled` at startup (failure
   logged) and from `syncDaemons` on every config save (best-effort,
   doesn't block the save).

Both are `frr_freebsd.go`-only changes; Linux is unaffected.

---

## v503 — 2026-07-17

**Removed the "No existing FRR BGP configuration was imported" banner from
Traffic › BGP.** This showed whenever FRR was present but gravinet wasn't
managing BGP yet and there was nothing existing to import — the normal
state for a freshly-installed FRR, not an error — so it read like a warning
for what's actually just the default starting point. The editor still
renders normally in that case; it just no longer says anything about the
(non-)import. The other branch of the same code path — reflecting a config
that *was* found on FRR — is unchanged.

---

## v502 — 2026-07-17

**Fixed: `install-freebsd.sh` was silently failing to install FRR.**

Reported as "the installer is supposed to install FRR — why does gravinet
still say it's not installed?" The root cause was in `frr_pkg_name`
(introduced in v499): it ran `pkg search -q '^frr[0-9]+$'` to find the
current `frrN` package, but `pkg search`'s default matched/printed field is
`pkg-name` — name **and** version together, e.g. `frr10-10.5.1_2` — not the
bare port name. An anchored pattern requiring the *whole* field to be just
`frr` + digits, with no version suffix, could never match anything real, so
that search silently returned empty on every single run, on every host,
regardless of what was actually in the repo. Every install therefore fell
back to `frr_pkg_name`'s one hardcoded guess (`frr10`) — and if that
particular version wasn't available on a given FreeBSD release/quarterly
branch (the port gets renamed/retired every major FRR release), `pkg
install` failed, the warning went to stderr where it was easy to miss
scrolling past the rest of the install output, and FRR quietly never got
installed at all.

- Replaced `frr_pkg_name` (the search-based guess) with `FRR_PKG_CANDIDATES`
  (`frr12 frr11 frr10 frr9 frr8`) and a loop in `ensure_frr` that tries a
  real `pkg install -y` against each in turn, newest first, stopping at the
  first one that actually installs. This depends on nothing but the
  install's real exit code — no more guessing at `pkg search`'s exact
  output shape. A losing candidate is a quick "no such package" from pkg,
  not a real download attempt.
- Added a clear `FRR: installed` / `FRR: NOT installed` line to the
  install's final summary (where the version/web-admin-URL info already
  is), rather than leaving FRR's status to whatever `ensure_frr` printed
  mid-scroll earlier — that's exactly how the original failure went
  unnoticed.
- `install-linux.sh`'s `ensure_frr` was not affected: it installs a fixed
  `frr` package name via each distro's own package manager (apt/dnf/zypper/
  pacman), with no version-discovery search involved.

If you hit this on an existing install, the fix is either to re-run
`install-freebsd.sh` (now v502), or manually: `pkg search -S name -x
'^frr[0-9]+$'` to see what your repo actually has, then `pkg install
frrNN`.

---

## v501 — 2026-07-17

**Removed the "BFD on all neighbors" global toggle from Traffic › BGP.** BFD
is now purely a per-neighbor setting, as it already was for anyone who'd
opted a specific peer in independently of the global switch. A brand-new
neighbor row now defaults to BFD on (previously this default came from the
global toggle, which itself defaulted on for a fresh configuration); an
existing neighbor keeps whatever it already has.

- `config.BGPConfig.BFD` removed. `BGPNeighbor.BFD` (per-neighbor) is
  unchanged.
- `renderFRR` emits `neighbor <peer> bfd` from each neighbor's own flag
  only — no more `n.BFD || b.BFD`.
- `neededDaemons` requests `bfdd` when any neighbor has BFD on — no more
  global-toggle shortcut.
- Editor: the "BFD on all neighbors" row is gone. Adding a neighbor now
  defaults its BFD tag to on (previously off, relying on the global toggle
  to cover the common case); editing an existing neighbor still leaves its
  BFD setting untouched unless explicitly toggled.
- No migration needed: a config saved with the old global toggle on but
  individual neighbors off would have rendered BFD for every neighbor
  anyway (the toggle was equivalent to setting each neighbor's flag); the
  stored JSON's `bfd` field at the top level is simply ignored by the new
  binary rather than causing an error, so existing configs keep working,
  just without the field having any further effect. Per-neighbor `bfd`
  values, which is what determines behavior now, are untouched by this
  change either way.
- Updated `TestFRRBGPPasswordAndBFD` (now tests independent per-neighbor
  BFD instead of the global toggle implying it), `TestSyncDaemonsContent`,
  `TestFreebsdNeededDaemons`, and `config.TestBGPRoundTrip` accordingly.

---

## v500 — 2026-07-17

**Full BGP/BFD support on FreeBSD** — Traffic › BGP now actually configures
FRR there, not just Monitor › BGP Peers' read-only view. Follow-up to v499,
which only installed FRR without gravinet recognizing it.

FRR's FreeBSD packaging (`net/frr9`, `net/frr10`, ...) is a materially
different shape from Debian/RHEL's: config lives under `/usr/local/etc/frr`
instead of `/etc/frr`, there's no single per-daemon on/off file — rc.conf's
`frr_daemons` variable lists every daemon to run, in one line, with `mgmtd`
and `zebra` required first — and it's driven through `service(8)`, not
systemd. None of this was guessed: it's confirmed against the actual rc.d
script FreeBSD ships (`net/frrN/files/frr.in` in the freebsd-ports tree),
including two details that would otherwise have been easy to get wrong or
miss entirely:

- **`vtysh` lives at `/usr/local/bin/vtysh`**, confirmed from the script's
  own `vtysh -b` invocation. Added to `bgpVtyshPaths` (`bgp.go`) alongside
  `/usr/local/sbin/vtysh` as a harmless extra — this is what actually makes
  `bgpSupported()` (and so the whole Traffic › BGP / Monitor › BGP Peers UI)
  light up on FreeBSD at all.
- **FreeBSD's `service frr reload` can silently no-op and still report
  success.** The rc.d script's reload case checks for
  `frr-reload.py` (the separate `frrN-pythontools` package, which
  gravinet's installer doesn't pull in) and, if it's missing, prints a
  message and *exits 0* — having done nothing. Trusting that exit code
  would mean gravinet believes a config change was applied when it
  silently wasn't. So on FreeBSD, `applyFRRService` never calls reload at
  all — every apply is a restart, unconditionally, the same posture v498
  already took for *removals specifically* on Linux (where the equivalent
  gap is `frr-reload.py`/`frr-pythontools` not being installed) — see
  `applyFRRService` in the new `frr_freebsd.go`.
- The rc.d script only writes the "integrated config" marker
  (`/usr/local/etc/frr/vtysh.conf`) on a plain `service frr start`, never on
  `restart` — and gravinet's FreeBSD path always uses restart (previous
  point), so a box that's never had a bare `start` would otherwise never
  get it, and every daemon would silently ignore gravinet's `frr.conf` and
  look for its own `bgpd.conf`/`zebra.conf` instead. `syncDaemons` writes
  that marker itself instead of relying on the rc.d script's bootstrap.

**Code structure**: `internal/webadmin/frr.go`'s OS-specific pieces
(`frrDir`/`frrConf`, `syncDaemons`, `applyFRRService` — renamed from
`runSystemctl` — `ensureFRRDaemonsEnabled`, `managedDaemonSet`) moved into
two new per-OS files following this codebase's existing convention (see
`metrics_linux.go`/`metrics_freebsd.go`/etc.): `frr_default.go`
(`//go:build !freebsd` — Linux is the only platform any of this ever really
ran on; Windows/macOS/OpenBSD harmlessly share its definitions since vtysh
is never found there) and `frr_freebsd.go` (`//go:build freebsd`). `frr.go`
itself is now purely the shared shape (`renderFRR`, `applyBGP`'s
orchestration, `bgpConfigRemovesSomething`, etc.) that calls into whichever
side got compiled in through identically-named functions.

**`daemonAlive` (frr.go) is now portable** rather than Linux-only: it used
to check for a live `/proc/<pid>` directory, which silently returned false
on every non-Linux platform (FreeBSD included) for lack of a mounted
procfs — meaning every FreeBSD config change was forced through a full
daemon restart even for a one-line edit, since "can't confirm alive" reads
the same as "needs a restart" to `applyBGP`. Replaced with `os.FindProcess`
+ sending it signal 0 (delivers nothing, just checks the pid exists) — the
standard portable Unix liveness probe, working identically on Linux and
FreeBSD. Verified this (and the rest of the split) actually compiles on
every platform gravinet targets, not just the two directly involved:
cross-compiled clean for `linux`, `freebsd`, `darwin`, `windows`, and
`openbsd`.

**`install-freebsd.sh`'s FRR step and help text updated** to say what's now
true — FRR being installed means Traffic › BGP works, the same as Linux —
replacing v499's "installed but gravinet won't recognize it yet" notes.

New tests: `frr_freebsd_test.go` (`//go:build freebsd`) covers
`freebsdNeededDaemons`'s required `mgmtd`/`zebra`-first ordering across
BGP-disabled/enabled/BFD-enabled configs, that it never mutates or aliases
`freebsdBaseDaemons`, and that `managedDaemonSet` includes the baseline
daemons. Cross-compiled and `go vet`-clean for FreeBSD; not executed here
(no FreeBSD host in this environment) — flagged as the one part of this
change that's verified by careful reading of FRR's actual rc.d script and
compilation rather than an end-to-end run against real FreeBSD packaging,
so it's worth a first real-world check before relying on it.

---

## v499 — 2026-07-17

**The FreeBSD installer now installs FRR automatically if it isn't already
present** — mirroring v498's `install-linux.sh` change.

`install-freebsd.sh` gained the same `ensure_frr` step, adapted to this
platform: `pkg install`s whichever `frrN` package (`net/frr9`, `net/frr10`,
...) is newest in the host's repo — FreeBSD's ports tree retires the old
port name every time a new FRR major version ships, unlike Linux distros
which just call the package `frr`, so the newest `frrN` is found with
`pkg search` rather than a version number hardcoded here that would go
stale the next time that happens. New `--no-frr` flag to opt out, same as
the Linux installer.

**Answering "will gravinet show the BGP sections on FreeBSD if FRR is
installed?": not yet, even after this change** — and this is a real gap
worth fixing, not just a detection tweak, so it's called out explicitly
rather than silently patched halfway:

- `bgpSupported()` (`internal/webadmin/bgp.go`), which gates whether
  Traffic › BGP and Monitor › BGP Peers even appear, only looks for `vtysh`
  at Linux FHS paths (`/usr/bin`, `/usr/sbin`, `/bin`). FreeBSD's pkg
  installs it under `/usr/local/{sbin,bin}` instead, so today FRR being
  present on a FreeBSD host is invisible to gravinet regardless of this
  installer change — the same as before.
- Fixing detection alone would be a small, safe change (just more
  candidate paths), and it's enough to make the read-only Monitor pages
  (BGP peers, BFD neighbors, the BGP Table added in v497) work correctly,
  since those only ever shell out to `vtysh -c "..."`.
- The editable Traffic › BGP side is the bigger gap: `applyBGP` writes
  `/etc/frr/frr.conf`, toggles daemons in `/etc/frr/daemons`, and drives
  FRR through `systemctl` — none of which is how FRR works on FreeBSD
  (config under `/usr/local/etc/frr`, daemon selection via rc.conf's
  `frr_daemons`, control via `service(8)`). Detection alone would make
  that card appear but silently fail to push saved config to FRR while
  showing a misleading "FRR is not installed" banner.
- Full support needs `frrDir`/`frrConf`/`frrDaemons`, `syncDaemons`,
  `runSystemctl`, and `daemonAlive`'s pid-file path made OS-aware — several
  of which (particularly the pid-file location under FreeBSD's `frr` rc.d
  wrapper) aren't things this could verify without a real FreeBSD host
  running the packaged FRR, so it's flagged for a follow-up decision on how
  far to take it rather than shipped as an unverified guess.

`install-freebsd.sh`'s new FRR step prints this same explanation to anyone
who runs it, so installing FRR there doesn't quietly imply gravinet is
about to manage it.

---

## v498 — 2026-07-17

**Confirmed: removing an advertised network under Traffic › BGP is already
fixed.**

This was reported as a separate bug from the neighbor-deletion one below,
but it's the same underlying fix, already shipped in v497:
`bgpConfigRemovesSomething` (used by `applyBGP` to decide restart vs.
reload) diffs both `Neighbors` and `Networks` between the previous and new
config, so a dropped network forces the same real `systemctl restart frr`
a dropped neighbor does — see `TestBGPConfigRemovesSomething`'s "network
removed" case. No code change was needed; noting it here since it was
asked about as if it were still open.

**Removed the redundant "BGP configuration" heading and imported-config
banner from the Traffic › BGP editor card.**

The page already opens with a description paragraph (via `secHint`)
explaining what the section does and how to use it; the card immediately
below it repeated an "BGP configuration" heading and, when a config had
been reflected from FRR's running state, a banner explaining that and
warning that neighbor MD5 passwords weren't imported. Removed both — the
card now starts directly with the "FRR is not installed" banner (when
applicable) or the settings rows. The background import-reflection code
that used to insert a "no existing FRR config was found" notice right
after the card's `<h3>` now inserts it at the top of the card instead,
since there's no longer an `<h3>` to anchor on. `renderBgpEditor` dropped
its now-unused `importedHasPasswords` parameter accordingly.

**The Linux installer now installs FRR automatically if it isn't already
present.**

Previously the installer never touched FRR at all — Traffic › BGP would
render and let you author a config, but bgpd/vtysh simply weren't there
until an operator separately discovered and installed FRR themselves.
`install-linux.sh` now has an `ensure_frr` step (same best-effort shape as
the existing `ensure_resolved` for systemd-resolved): checks for `vtysh`
first — the same check gravinet's own webadmin uses to decide whether the
BGP pages have anything to show — and if it's missing, installs the `frr`
package through whichever package manager the host has. This just works on
Debian/Ubuntu, Fedora, openSUSE, and Arch, where `frr` ships in the
distro's own repos; RHEL/Rocky/Alma/CentOS don't carry it in their base
repos at all (it needs FRR's own repo, rpm.frrouting.org), so there it
prints a clear pointer instead of guessing at a repo URL/GPG key per EL
version. New `--no-frr` flag to opt out, matching the existing
`--no-firewall` / `--no-systemd-resolved` pattern.

---

## v497 — 2026-07-17

**Fixed IPv6 BGP neighbors not appearing under Monitor › BGP Peers.**

The live BGP status query was `show ip bgp summary json` — the `ip` keyword
restricts FRR to the IPv4-unicast address family only, so an IPv6 session
never appeared in the JSON no matter how `parseBGPSummary` walked it (it
already handled an `ipv6Unicast` block correctly; that block just never
arrived). Switched to `show bgp summary json`, which returns every
configured AFI, in both places this query runs: the live peers endpoint
(`handleBGP`) and the BGP-config importer's summary enrichment
(`importBGPFromFRR`). No parsing logic changed — `parseBGPSummary` and
`summaryToBGPConfig` were already correct; they just needed the right
input.

**Added a "BGP Table" card to Monitor › BGP Peers.**

New third card on that page (after BGP Neighbors and BFD Neighbors) showing
the live output of FRR's `show bgp` — the full prefix/next-hop/AS-path
table, one level below the per-peer summary the existing card shows.

- New `GET /api/bgp/table` (`handleBGPTable`), gated on vtysh presence like
  every other BGP/BFD endpoint, degrading to `available:false` with a human
  reason rather than an error when vtysh is absent or FRR isn't answering.
  `show bgp` has no JSON form, so the response carries FRR's text output
  verbatim (`text`) rather than a reshaped struct.
- Rendered in a `<pre class="mono-block">`, same pattern as the local
  hosts-file view, with the same line-filter box for finding a prefix in a
  large table.
- New `TestHandleBGPTableNoVtysh` covers the degrade path.

**Fixed deleting a BGP neighbor (or network) in Traffic › BGP not actually
removing it from FRR's running configuration.**

`renderFRR` regenerates `frr.conf` from scratch on every save, so the
deleted neighbor was always correctly absent from the file on disk — the
bug was downstream, in how that file gets pushed into the running daemon.
A plain edit (add a neighbor, tweak a field) only ever needed a *reload*,
which the code got right; but removing something needs FRR to actually
retract a line it's already running, and neither of the two mechanisms
`applyBGP` had for that can do it: `systemctl reload frr` only performs a
real diff-and-retract when the host has `frr-reload.py`
(`frr-pythontools`) installed — not something gravinet requires or
checks for — and the `vtysh -b` fallback is documented (in this codebase's
own comments) as push-only, integrating whatever lines the file currently
has into the running daemons without ever retracting a line that's gone.
So a host without `frr-pythontools`, or one where reload silently didn't
diff, kept the "deleted" neighbor running indefinitely.

- New pure function `bgpConfigRemovesSomething(prev, next)`: true when
  `next` drops a neighbor or network `prev` had, or tears down the whole
  `router bgp` stanza (BGP disabled, or the ASN changed).
- `applyBGP` now takes `prev` (the config being replaced) alongside the new
  one, and folds `bgpConfigRemovesSomething` into the same decision that
  already forces a restart over a reload for a daemon-set change — so a
  removal always gets a real `systemctl restart frr` (which re-reads
  `frr.conf` from scratch, unconditionally correct regardless of what
  optional FRR tooling is present) instead of a reload or the additive-only
  `vtysh -b`.
- `handleBGPConfig` captures the previous stored `BGPConfig` inside the same
  `mutateConfig` transaction that overwrites it, so `applyBGP` always diffs
  against exactly what was persisted a moment before — no separate read, no
  race with a concurrent save.
- New `TestBGPConfigRemovesSomething` covers: an identical config,
  neighbor/network removed, neighbor added, an in-place field edit,
  BGP disabled outright, an ASN change, and the first-ever save.

---

## v496 — 2026-07-17

**Traffic › BGP › Neighbors: added a "state" column (enabled/disabled).**

New last column on each neighbor row, same double-click-to-toggle
tag-toggle shape as the existing BFD column and every other table's state
column in the app (NAT/QoS rule state, etc.) — the pattern the request
pointed at directly. Disabling a neighbor emits FRR's `neighbor <peer>
shutdown` (the session stays fully configured, just held administratively
down); enabling removes that line. New neighbors default to enabled.

- `config.BGPNeighbor` gained a `Shutdown bool` field.
- `renderFRR` emits `neighbor <peer> shutdown` when set.
- `parseRunningConfigBGP` recognizes the line on import, so a neighbor
  someone shut down directly in FRR reflects as disabled here too.
- New test `TestFRRNeighborShutdown` covers both directions (render and
  round-trip through the importer) and confirms an unrelated neighbor's
  activation/other directives are untouched.
- Verified the full column in a real DOM: default state, toggling in both
  directions, and that a brand-new neighbor POSTs `shutdown:false`.

Caught and fixed a real bug before shipping this: an explanatory code
comment used backticks around `neighbor <peer> shutdown`, which — since the
entire embedded UI script lives inside a single Go raw-string literal —
prematurely closed that string and broke the build. Fixed by not using
backticks anywhere in the embedded script's source (comments included); full
build/vet/test all pass, and every earlier reproduction test was re-run
against the fixed file to confirm nothing else was affected.

## v495 — 2026-07-17

**Monitor › BGP Peers: the BGP Neighbors table shows "down" instead of
"Active" for FRR's Active FSM state.**

"Active" is technically correct — the FSM is retrying the TCP connection to
the peer — but it reads as though something's actively working, when what it
actually means is just that the session isn't up. Every other non-Established
state already reads as clearly-not-up on its own (Idle, Connect, OpenSent,
OpenConfirm), so Active was the one genuinely misleading label. The raw FRR
state is still available as a tooltip on the pill for anyone who wants the
precise value. New helper `bgpStateLabel`; verified in a real DOM against
Established/Active/Idle/Connect that only Active gets relabeled and the
tooltip carries the original state.

## v494 — 2026-07-17

**BGP editor: three small UI fixes.**

- The MD5 password reveal toggle (both the read-mode row and the row-edit
  form) now shows an eye glyph (\ud83d\udc41\ufe0f) instead of the word "show"/"hide" —
  the glyph stays constant on click; only its tooltip text and the masked
  value underneath change.
- The success status after saving no longer appends the server's apply note
  (e.g. "3 daemon(s) configured, reloading FRR in background") — it's
  routine detail on every single save. Now just "Saved." The one case
  actually worth surfacing, FRR not installed, already has its own
  persistent banner elsewhere in the card, so nothing is lost.
- The Local AS number field is now the same width as Router-id (180px,
  was 160px).

## v493 — 2026-07-17

**FRR config: always emit five global BGP session-level directives.**

`renderFRR` fully regenerates `frr.conf` from scratch on every apply rather
than patching an existing file (its own header says as much: "Do not edit by
hand"), so "add if not present" means unconditionally rendering these inside
the `router bgp` block whenever BGP is enabled:

- `bgp log-neighbor-changes`
- `no bgp ebgp-requires-policy`
- `bgp deterministic-med`
- `bgp bestpath as-path multipath-relax`
- `bgp conditional-advertisement timer 10`

New test (`TestFRRGlobalBGPDirectives`) checks all five are present on both a
minimal and a fully-populated config, and entirely absent when BGP is
disabled (no `router bgp` block at all).

**Monitor › BGP Peers: renamed to "BGP Neighbors", added a "BFD Neighbors"
card.**

The existing live-peers card is now titled BGP Neighbors. A new BFD
Neighbors card sits below it, backed by a new read-only `/api/bfd` endpoint
(`handleBFD`, gated on the same vtysh presence as `/api/bgp`) that runs
`show bfd peers json` and reports peer, local address, interface, status,
and up/down time — a BFD session isn't itself BGP-specific (it can back an
OSPF adjacency or a monitored static route too), hence its own card rather
than folding into the BGP table. Field names (`peer`, `local`, `interface`,
`status`, `uptime`, `downtime`, `diagnostic`) were taken directly from FRR's
own `bfdd_vty.c` source (`__display_peer_json`) rather than guessed at, and
`TestParseBFDPeers` checks parsing against a realistic sample built from
those exact fields. Verified the full card renders correctly end-to-end in a
real DOM, including that a raw seconds-elapsed uptime/downtime value comes
out through the existing `fmtElapsed` duration formatting correctly (e.g.
125s \u2192 "2m 5s").

## v492 — 2026-07-17

**BGP editor: Neighbors and Advertised networks are now real tables, matching
Networks/Keys/Seeds/Firewall/NAT/QoS.**

Both were previously bespoke, always-live-input widgets (a raw list of input
rows for neighbors; a plain div-per-row list, not even a `<table>`, for
networks) that autosaved on every keystroke — the only section in the app
built that way. Replaced with the same interaction model every other
list-editing section uses: a checkbox-select column, a `+`/`\u2212` toolbar
(`enhanceTable`'s standard `table._rowAdd`/`_rowRemove`), double-click a field
to edit a row in place with save/cancel, tick rows and `\u2212` to remove.
BFD is a separate double-click-to-toggle tag on each neighbor row (the same
pattern NAT/QoS use for a rule's enabled/disabled state), immediate like
before. The scalar fields above the tables (Enable BGP, AS number, router-id,
BFD-for-all, redistribute, timers) are unchanged — still debounced autosave.

Since `secBgp`'s data loads asynchronously (outside `renderSection()`'s own
blanket `enhanceTable` pass over the page), `renderBgpEditor` now calls
`enhanceTable` on its own two tables directly rather than relying on that.

**Added a way to see a neighbor's MD5 password.**

There was previously no way to view a saved neighbor password at all — the
field was a permanently-masked `<input type="password">` with nothing to
reveal it. Each neighbor row now shows a masked placeholder with a show/hide
toggle right in the table (no round trip needed \u2014 the plaintext is
already in the config the page loaded, the mask was purely a display choice),
and the row-edit form's own password field has the same show/hide toggle
while typing.

Verified in a real DOM (jsdom): rendered a two-neighbor config, revealed and
re-masked a password, toggled BFD, edited a row's description in place, added
a row, removed a row via checkbox+toolbar \u2014 confirmed each one actually
POSTs the right resulting config. Re-ran the v490/v491 reproduction tests
(the render-crash repro and the Monitor\u2009\u203a\u2009BGP\u2009Peers
click-through) against the restructured tables to confirm no regression.

## v491 — 2026-07-17

**Remove the now-redundant "Check FRR's live configuration" button.**

It existed to work around the v490 render crash (blank fields / stuck
"Checking…") by giving the operator a way to retry and see a concrete
failure reason. With that crash actually fixed at the root, the automatic
background import works correctly again on its own, so the manual button was
just clutter. Removed, along with its now-unused 25s request timeout on that
one call site (`withTimeout` itself stays — the config GET still uses it).

**Monitor › BGP Peers: clicking a peer now jumps to its definition under
Traffic › BGP.**

Peer addresses in the live BGP peers table are now links (same `.peer-link`
pattern already used for mesh-peer names in the latency table). Clicking one
switches to Traffic › BGP and scrolls to/flashes the matching neighbor row —
`gotoBgpNeighbor` sets `state.pendingBgpHighlight` before navigating and
`renderBgpEditor` consumes it once its card actually attaches, since the BGP
editor's own data load is async and not something `refresh()` waits on (same
reason `gotoMeshPeer`/`gotoNetwork` seed selection state before navigating
rather than searching for the row immediately after). No-op-safe: a live peer
that isn't in gravinet's own stored neighbor list (BGP configured outside
gravinet) still lands on the BGP card itself rather than nowhere.

Verified end-to-end in a real DOM (jsdom): rendered the live peers table,
simulated a click on a peer address, and confirmed the resulting page lands
on Traffic › BGP with that exact neighbor's row flashed and its fields
(peer/AS/description) intact.

## v490 — 2026-07-17

**The actual bug behind "BGP fields are blank" / "stuck on Checking… forever":
a bad `$()` call silently crashed the neighbors table renderer.**

Server logs confirmed the import itself was working the whole time — reading
`/etc/frr/frr.conf`, finding a real ASN and neighbor, and returning them
successfully. The bug was entirely client-side, and entirely unrelated to
networking, timeouts, or auto-import gating (the last several fixes address
real, separate issues, but not this one): `renderNbrs()` built each neighbor
row with `$('<tr></tr>')` and each cell with `$('<td></td>')`. `$()` parses
its argument as `innerHTML` on a plain `<div>` — and every browser's HTML
parser silently drops a bare `tr` or `td` element there, since they're only
valid inside a real `<table>` context. Both calls returned `null`; the very
next `.appendChild` on that `null` threw; and because this happens deep
inside a synchronous render call with nothing above it to catch the
exception, the whole render aborted before the finished card was ever
attached to the page — leaving exactly whatever was on screen a moment
before (a blank stored-config form, or, after v488's button, a permanently
disabled "Checking…" label) with no visible error at all.

This only shows up once a real neighbor reaches that table — a brand-new,
empty BGP config never exercises it, which is how it shipped unnoticed.
Reproduced directly by extracting the embedded script and running it against
the exact data the user's own server logged (asn=65001, 1 neighbor) in a real
DOM implementation (jsdom): confirmed the crash, fixed it by building both
elements with `document.createElement` instead (no table-context requirement
there), and confirmed the fix — the same harness now renders successfully and
every field (AS number, router-id, the neighbor's peer/AS/description, the
network) lands correctly in the resulting form.

Added `TestNoStandaloneTrOrTdViaInnerHTML`, a lightweight regression guard
that scans the served page for this exact anti-pattern so it can't quietly
come back, here or in any future table built the same way.

## v489 — 2026-07-17

**Fix: the v488 "Check FRR's live configuration" button could hang on
"Checking…" forever.**

Reported symptom: clicking the new button never resolved — no error, no
result, just a permanent "Checking…". Root cause was two missing bounds:

- **Client side:** the button's request used the plain `api()` helper
  directly, which wraps a bare `fetch()` with no timeout at all. Every other
  call in this handler either goes through a bounded helper or hits a
  same-host endpoint that answers fast; this one was the one place calling
  out to `/api/bgp/import` — which genuinely can take several seconds since it
  touches FRR — without any client-side bound. `withTimeout()` (previously a
  helper local to the config-load function) is now a shared top-level
  function, and the button's request is wrapped in it at 25s. A stuck request
  now surfaces "Request failed — timed out after 25s" instead of sitting on
  "Checking…" with no way to tell a slow-but-working call from a truly wedged
  one.

- **Server side:** `handleBGPImport` relied entirely on `runVtysh`'s own
  internal timeout to keep each of its (up to two, sequential) vtysh calls
  bounded — there was no bound on the handler's *total* work. The
  goroutine/select "abandon a wedged call" pattern `runVtysh` already uses for
  a single call is now pulled out into `boundedBGPImport` and applied around
  the whole import, independent of whatever's happening inside it. New tests
  (`TestBoundedBGPImportTimesOut`, `TestBoundedBGPImportReturnsPromptly`)
  cover both the timeout path and the plain fast path.

Together these guarantee the request the button makes returns within 25s no
matter what — either with real data, a concrete "nothing found" reason, or a
"timed out" message — never an indefinite hang.

## v488 — 2026-07-17

**BGP editor: add a manual "Check FRR's live configuration" button.**

Reported symptom: a real host with a real, independently-configured FRR BGP
setup opened the Traffic › BGP page and got a blank form — AS number,
router-id, neighbor, and network fields all empty, no error shown.

The parsing itself checks out: fed the exact reported config lines
(`router bgp <asn>`, `bgp router-id`, `neighbor ... remote-as`, `network`)
through both the bare parser and the full `importBGPFromFRR` pipeline in an
isolated test, it correctly produces enabled/ASN/router-id/neighbor/network —
so the mapping logic wasn't the bug. The likely gap is upstream of it: the
page's background import (in `secBgp`'s `load()`) only ever runs once per
page load, and only when gravinet doesn't yet consider itself the active BGP
manager (`!active`, i.e. it has no BGP already enabled+saved locally). Once
BGP has ever been enabled and saved through gravinet — even from an old or
abandoned attempt — the page stops looking at FRR's live config on its own,
silently, with nothing on screen to explain why the fields are empty. A
failed import (permission denied reading the config file, vtysh present but
not answering, no `router bgp` stanza at the expected path, …) fails the same
way: quietly, into a blank form.

The BGP configuration card now always shows a "Check FRR's live
configuration" button (whenever FRR is installed on the host, regardless of
`active`). Clicking it calls `/api/bgp/import` directly and either re-renders
the form with what it found, or shows the concrete reason nothing came back —
turning an unexplained blank page into a diagnosable one. Nothing is written
until the operator actually edits a field afterward (same autosave-on-touch
behavior as the rest of the form), so pulling in FRR's live state this way is
always safe to look at even when gravinet already has its own saved config.

## v487 — 2026-07-17

**Fix: stale BGP nav items survive a managed-peer switch on a host without FRR.**

Picking a peer from the managed-node list (the header's node picker) refreshes
`state.bgpSupported` from that peer's own `/api/config` on every switch, but the
left-rail nav itself (`buildRail()`) only ran once, at page load. Switching from
a peer that has FRR to one that doesn't left that peer's Traffic › BGP and
Monitor › BGP Peers tabs sitting in the rail — still visible, still clickable —
on a target that can't serve either. Clicking one didn't error; it just fell
through `renderSection()`'s existing `sectionVisible` backstop straight to
Networks, which read as a broken link rather than a hidden one.

The rail now always creates every nav button (previously a gated section with
no capability was skipped entirely, so a target that *gained* the capability
later had no button to ever show), sets its initial visibility from
`sectionVisible()` at build time, and a new `syncRailGating()` re-applies that
check to the buttons already on screen after every `load()` — so switching
targets shows and hides the BGP tabs live, matching whichever node was just
selected.

**BGP editor: drop two lines of static boilerplate.**

Removed the "Changes save and apply automatically." and "Live peer status is
under Monitor › BGP Peers." hints from the bottom of the Traffic › BGP editor.
The status line itself stays — it's reused to surface save-in-progress, saved,
and error text — just without the static first line.

(No change to FRR-config import: `router bgp <asn>` → Enable BGP + Local AS,
`bgp router-id <ip>` → Router-id, `neighbor <peer> remote-as <as>` → the
neighbor table, and `network <prefix>` → advertised networks was already
exactly `parseRunningConfigBGP`'s behavior; confirmed against its tests rather
than re-implemented.)

## v486 — 2026-07-17

**Fix: BGP section stuck on "loading configuration…" (regression from v485).**

The v485 autosave rewrite dropped the one line at the end of `renderBgpEditor`
that actually attaches the built form to the page and clears the loading
placeholder (`host.innerHTML=''; host.appendChild(card)`). The result: the whole
editor was constructed correctly but never inserted, so the BGP section sat on
"loading configuration…" forever. The line is restored. This slipped through
because the editor lives in embedded UI JavaScript inside a Go string, which
`go build`/`vet`/`test` don't parse; the extracted script now passes a Node
syntax check as part of verifying this build.

The BGP section is also hardened so a failure like this can't present as a silent
infinite spinner again: the config load is wrapped with a 12s timeout and
try/catch, the render call is guarded, and any failure now shows the actual
reason (request error, timeout, non-OK response, or an editor exception) with a
**Retry** button instead of an unbounded "loading…". (The specific v485 bug
wasn't an exception or a hang, so only restoring the attach line fixes it — but
the guardrails turn any future load failure into a visible, recoverable message.)

## v485 — 2026-07-17

**BGP editor is now fully autosave (no Save button), and the initial FRR config
import says why it found nothing.**

- **Autosave.** v484 only renamed the BGP form's button to "Save"; this removes
  it entirely. The Traffic › BGP editor now persists — and applies to FRR —
  automatically, the same as every other form in the app. Toggles and
  neighbor/network add-or-remove save immediately; the text fields (AS number,
  router-id, timers, per-neighbor fields, network prefixes) debounce so we're
  not POSTing on every keystroke. An intermediate invalid state (BGP enabled
  with no AS number, or a hold timer at or below the keepalive) is held back with
  an inline hint instead of being POSTed as a guaranteed error — the next valid
  edit saves it — so autosave never pushes a config FRR would reject. Concurrent
  saves are sequence-guarded so a slower earlier save can't clobber the status of
  a newer one.

- **Import diagnostics.** When gravinet isn't managing BGP yet and tries to
  reflect FRR's existing config, a failure used to leave the editor silently
  empty — indistinguishable from a broken import, which is what made this hard to
  chase across versions. `importBGPFromFRR` now returns a diagnostic reason
  describing which source it read from, or, when nothing worked, why each source
  came up empty: file missing vs present-but-unreadable (the classic
  permissions-on-`/etc/frr` case is now called out specifically), no `router bgp`
  stanza, or vtysh absent/not answering (bgpd down). That reason is logged by the
  daemon (`bgp import: …`) and shown in the editor, so an empty form now explains
  itself instead of looking broken.

- **CRLF hardening.** `parseRunningConfigBGP` now normalizes CRLF/CR line
  endings before parsing. A config that had ever passed through a Windows editor
  left a trailing `\r` on the ASN token (`65001\r`), which failed to parse and
  silently yielded "no stanza" — a real import failure that was invisible in the
  UI. Covered by a new regression test, alongside the import-reason assertions.

## v484 — 2026-07-17

**BGP form now saves-and-applies like every other form; FRR's bgpd/bfdd are
auto-enabled on detection.**

Two changes to how gravinet drives FRR:

- The Traffic › BGP editor's **Save & apply** button is gone, replaced by a
  plain **Save**. Nothing about the behaviour changed — saving already
  persisted the config and reconciled FRR in one step — but the old label
  implied "apply" was a separate action the operator had to opt into, out of
  step with every other form in the app, where saving *is* applying. The
  success line no longer frames applying as a distinct ceremony either; it just
  reports what reaching FRR did (reloading in the background, or that FRR is
  absent so there was nothing to push).

- On startup, if FRR is detected on the host, gravinet now checks
  `/etc/frr/daemons` for `bgpd=yes` and `bfdd=yes`. If either is disabled
  (`=no`), it enables it and restarts the FRR service so the change takes
  effect. A stock FRR install ships with every optional daemon off, so BGP and
  BFD would otherwise never come up until something turned them on; this is that
  something. It only ever flips `=no` to `=yes` for those two daemons — it never
  disables anything and never touches other lines — so it doesn't interfere with
  the config-driven daemon reconciliation that already happens when a BGP config
  is saved (that path still owns turning unused daemons back off). It's a no-op
  when FRR isn't installed, and idempotent: once the two daemons read `=yes`,
  later boots change nothing and FRR is not restarted. Runs in the background so
  a slow `systemctl restart frr` can't hold up the admin server coming up. New
  `enableDaemonsContent` unit test covers the enable-only, idempotent, and
  mixed (one already on) cases.

## v483 — 2026-07-16

**Fix (for real this time): an existing FRR BGP config now imports from the
config file, not the live daemon.** Prior attempts read the pre-existing BGP
config through vtysh (`show ip bgp summary json`, then `show running-config`).
Both depend on bgpd being up and answering — so on a host whose peer session
wasn't established, or whose daemon wasn't fully responsive, the import returned
nothing and the editor stayed empty even though `/etc/frr/frr.conf` held the
whole configuration. That was the wrong source.

The import now reads FRR's config file directly (`/etc/frr/frr.conf`, then
`/etc/frr/bgpd.conf`) and parses the `router bgp` stanza — local AS, neighbors
(with remote-AS and BFD), advertised networks, redistribute, and timers — with
no dependency on the daemon running at all. vtysh is now only a fallback (config
held solely in a running daemon, or a nonstandard path) and a source for the
live router-id/AS when the file omits them. A new end-to-end test imports a
real parapet-managed `frr.conf` — an unestablished neighbor, trailing
whitespace, password line and all — from the file alone with vtysh absent, and
asserts the AS, neighbor, BFD, and both networks come through. Passwords are
still flagged-but-not-imported (re-enter before saving).

## v482 — 2026-07-16

**Windows: the service no longer stops-and-stays-down on a settings change, and
now auto-restarts on failure.** Two related fixes.

Root cause of the stuck service: a settings change that needs a full restart ran
the same path as every other OS — shut down, then ask the service manager to
restart us. On Windows that meant the service calling `Restart-Service` on
*itself*, which deadlocks: `Restart-Service` waits for the service to report
STOPPED, but the service can't report STOPPED because it's blocked inside that
very call. It timed out stopped and never came back. Now, when a restart is
needed and we're running under the SCM, the service instead reports a non-zero
*failure* exit and returns, letting the SCM's recovery action restart it
cleanly — the supported pattern, no self-deadlock.

Service recovery is now configured: **restart on the first, second, and
subsequent failures**, with a short escalating backoff (5s / 10s / 30s) and the
failure count reset after a day of stability. Crucially the failure-actions flag
is set too, so recovery fires not only on a hard crash but also on the non-zero
exit above. This is applied by the installer (`sc.exe failure` /
`sc.exe failureflag`, and in `install-windows.ps1`) and, so existing installs
repair themselves without a reinstall, re-applied by the daemon itself at every
service start (best-effort, idempotent). Together this means the broader
complaint — the service stopping and not restarting — is now covered for any
unexpected stop, not just the settings-change path: a crash, a stuck-shutdown
force-exit, or a restart request all bring the service back automatically. A
deliberate operator stop still exits cleanly (exit 0) and is left stopped, as it
should be.

## v481 — 2026-07-16

**BGP keepalive/hold timers, and Live Peers moved to its own Monitor section.**

Two additions to the BGP configuration: a **Keepalive timer** and a **Hold
timer**, rendered as FRR's `timers bgp <keepalive> <hold>`. A new config now
defaults these to a fast **4s / 12s** (versus FRR's own 60s/180s), so a dropped
peer is detected in seconds rather than minutes; the 3:1 ratio is conventional.
Existing and imported configs keep their actual values (0 means "unset — use
FRR's defaults"). Validation requires the hold timer to exceed keepalive and
clear FRR's 3s floor, enforced both client-side and on save. The values also
round-trip through the FRR-config import.

**Live Peers moved from Traffic › BGP to a new Monitor › BGP Peers section.**
The read-only live BGP session table is observational state, not configuration,
so it now sits alongside the other live views (Monitor › Route Table, Mesh
Peers, DNS State) rather than under the config editor — keeping Traffic › BGP
purely for editing, consistent with how the rest of the app separates
"configure" from "observe." The new section is gated on vtysh the same way, and
the config page carries a one-line pointer to it. (Easy to fold back into the
editor if co-locating configure-and-observe proves more convenient in practice.)

## v480 — 2026-07-16

**Fix: importing an existing FRR BGP config now actually populates the editor.**
On a host already running BGP, the config editor stayed empty even though the
Live Peers table showed the session (local AS, router id, and an established
neighbor). Two problems: the import parsed `show running-config`, which can fail
or be restricted on some hosts (producing exactly the "peers live, editor empty"
mismatch), and the import was gated on `/etc/frr` existing rather than on FRR
being reachable.

The import now builds primarily from `show ip bgp summary json` — the very query
the Live Peers panel uses, so wherever peers appear, the editor now reflects
them: local AS, router id, and the neighbor list (peer + remote AS), including
32-bit ASNs (which are preserved, not truncated) and dual-stack peers (deduped
to one entry). `show running-config` is still consulted, but only as best-effort
enrichment for what the summary doesn't carry (advertised networks,
redistribute, per-neighbor description/BFD); if it's unavailable the core config
still imports. The background import is also now gated on vtysh being present
(the same thing that makes the peers panel work) rather than on the `/etc/frr`
directory, so it's no longer skipped on hosts where FRR runs but that directory
isn't where we looked. New tests cover the summary-to-config mapping with the
reported 32-bit-ASN data, dual-stack dedup, and the no-BGP case.

## v479 — 2026-07-16

**BGP page: simplified intro and BFD on by default for new configs.** The
Traffic › BGP section header text is now just "BGP configuration for dynamic
routing." in place of the previous multi-sentence explanation. And a brand-new
BGP configuration now starts with "BFD on all neighbors" enabled by default —
sub-second neighbor-failure detection is the better baseline. This applies only
to a fresh config; an existing one (already stored by gravinet, or imported live
from FRR) keeps whatever BFD setting it actually has, so no peer's real setting
is ever silently changed.

## v478 — 2026-07-16

**Fix: BGP page could hang on "loading configuration…".** The v476 FRR import
ran `vtysh` synchronously inside the `/api/bgp/config` GET that the editor waits
on, so anything that made `vtysh` slow or wedged blocked the whole page from
rendering — and `os/exec`'s `Output()` can stall past its context deadline if
`vtysh` (or a child that inherited its stdout pipe) doesn't exit on the kill, so
the load could hang indefinitely rather than time out.

Two changes fix it. First, the editor no longer blocks on FRR at all: the config
GET now returns only gravinet's stored config (a fast file read, no subprocess),
so the editor renders immediately. Reflecting a pre-existing FRR configuration
moved to a separate `/api/bgp/import` endpoint the UI calls *after* the editor is
on screen; if it's slow or never answers, the editor is already usable and the
imported values simply don't swap in (instead of a stuck page). Second, every
`vtysh` call now goes through a bounded runner that enforces a hard wall-clock
limit even when the process ignores the context kill — a wedged `vtysh` can leak
at most one goroutine/process but can never block an HTTP handler. The live-peer
status endpoint uses the same runner, so it's protected too. Behavior is
otherwise unchanged: an existing FRR config still populates the editor with a
"read from FRR" banner, still adopt-on-save, still no passwords imported. Added a
test that the bounded runner returns promptly (never spawns or blocks) when
vtysh is absent.

## v477 — 2026-07-16

**Faster reconnection after the host wakes from sleep: TLS-fallback idle timeout
lowered from 90s to 45s.** When a machine suspends, its connections go silent;
on resume, a peer reached over the TCP/TLS fallback path can be masked as
"reachable via a dead pipe" — the stale connection stays registered, so the
engine believes it still has a working fallback and won't redial — until the
transport's read-idle deadline tears it down. That deadline was 90s, which set
the worst-case reconnection ceiling for such peers. It's now 45s, roughly
halving that ceiling.

The value is still comfortably above the mesh's own liveness contract, which is
what keeps it safe: the mesh declares a peer dead after 20s of silence
(`defaultPeerTimeout`) and a live session emits a keepalive every 10s
(`defaultKeepaliveInterval`), resetting this rolling deadline each time. At 45s
— ~2x peerTimeout, ~4x the keepalive — a live-but-quiet connection (even across
several lost keepalives) is never dropped by the transport; only a genuinely
idle one, which the mesh has already given up on, is reclaimed. The invariant
(idle timeout fires after the mesh's own peer-timeout, never before) is
preserved. Also corrected stale figures in the surrounding comments, which still
referenced an older 20s-keepalive/75s-peer-timeout contract; the code now
documents the actual 10s/20s values. No config or API change.

## v476 — 2026-07-16

**BGP page now reflects an existing FRR configuration instead of showing empty.**
Fixes a bug where, on a host that already ran BGP (configured outside gravinet —
a hand-edited `frr.conf`, another tool, or a prior install), the Traffic › BGP
editor showed zero configuration even though live peers were established, because
it only ever read gravinet's own `config.json`. Now, when gravinet isn't yet
managing BGP (its stored config is empty/disabled), the page reads what FRR is
*actually running* — via `vtysh -c "show running-config"` — and populates the
editor from it: local AS, router-id, neighbors (peer, remote AS, description,
per-neighbor BFD), advertised networks, and redistribute settings. The editor
shows a banner noting the settings were read from FRR and that saving adopts them
into gravinet's management.

The import is strictly read-only — it never writes `frr.conf` or `config.json`;
gravinet takes ownership only when the operator explicitly saves. This
deliberately avoids seeding gravinet's config (or rewriting a hand-tuned
`frr.conf`) behind the operator's back at daemon startup, which could disrupt a
working session; reflecting the live config on load fixes the visible problem
without that risk. Precedence is explicit: a BGP config gravinet is actively
managing always wins; the FRR import is only a fallback when it isn't.

Neighbor MD5 passwords are intentionally not imported — FRR may hold them
encrypted, and re-emitting them verbatim on a later save would corrupt the
session — so the page reports whether any neighbor had a password and warns to
re-enter it before saving. New unit tests cover the running-config parser: a full
stanza with router-id/neighbors/BFD/networks/redistribute (asserting `activate`
lines don't create phantom neighbors and passwords aren't captured), the
no-stanza case, and the stanza boundary (a following `router ospf` block must not
leak into the BGP import).

## v475 — 2026-07-16

**Traffic menu reordered and Routes renamed to "Mesh Routes."** The Traffic
group now reads firewall → nat → qos → shaping → mesh routes → bgp, grouping the
enforcement/shaping items ahead of the routing items (mesh routes and BGP). The
Routes section is now labelled **Mesh Routes** everywhere the label is used —
the left rail, global search, and the section heading — to distinguish
mesh-distributed subnets from the read-only system Route Table under Monitor.
No behavior change: the section id (`routes`), its API, and its contents are
unchanged; only ordering and the display label moved.

## v474 — 2026-07-16

**Traffic › BGP is now a full BGP + BFD control panel — gravinet owns the
configuration and drives the FRR daemon.** v473 added a read-only view of the
sessions FRR already held; this makes the section read/write. gravinet now
stores its own BGP/BFD configuration (node-global, in `config.json` under a new
`bgp` block) and reconciles FRR with it on save: it renders an integrated
`/etc/frr/frr.conf`, rewrites `/etc/frr/daemons` so exactly the daemons the
config needs are enabled (`bgpd` when BGP is on, `bfdd` when BFD is on globally
or for any neighbor, `staticd` always), and reloads FRR — restarting when a
daemon has to change state, otherwise reloading, with a `vtysh -b` fallback and
a single retry, all dispatched to the background so the HTTP save returns
immediately. gravinet doesn't speak BGP itself; it owns the config and the
daemon lifecycle and lets FRR run the sessions. This is a port of parapet's
`frr.rs`, narrowed to the BGP/BFD surface the request named.

The editor exposes: enable toggle, local AS number, router-id, **BFD on all
neighbors** (Bidirectional Forwarding Detection — sub-second neighbor-failure
detection), redistribute connected/static, a **neighbors** table (peer address,
remote AS, description, MD5 session password, and a per-neighbor BFD toggle),
and an **advertised networks** list. Saving persists to gravinet's config
(validated: enabling BGP requires a local AS) and then applies to FRR; the live
peer table beneath refreshes to show FRR converging. On a host without FRR the
config still saves — it just isn't pushed to a daemon that isn't there, and the
editor says so — exactly as parapet behaves.

Config generation is injection-safe: every peer address, network prefix, and
router-id passes a `safeToken` allow-list before it's spliced into a conf line
(anything with whitespace or shell/comment metacharacters is silently dropped,
never emitted); descriptions and passwords are single-line-filtered and
length-capped, and passwords with embedded whitespace have it stripped. New unit
tests port parapet's render/daemon coverage: the minimal runnable block,
password+BFD emission, per-neighbor vs global BFD requesting `bfdd`,
whitespace-stripped passwords, injection filtering, invalid-neighbor skipping,
the `/etc/frr/daemons` reconciliation (including idempotency and turning BGP back
off), `safeToken` itself, and config-level BGP validation and JSON round-trip.
The `/api/bgp` read-only status endpoint and the `vtysh`-presence menu gate from
v473 are unchanged; a new `/api/bgp/config` endpoint backs the editor.

## v473 — 2026-07-16

**Traffic › BGP: live BGP peer status from FRR, shown only where it applies.**
gravinet can now surface the BGP sessions FRR holds on a host, read live through
FRR's CLI (`vtysh`) and displayed as a read-only table under Traffic › BGP. This
is a port of how the separate `parapet` project exposes dynamic-routing
adjacencies: gravinet does **not** run BGP or manage FRR's configuration — it
asks `vtysh` for `show ip bgp summary json` and reshapes the result (peer
address, remote AS, session state, uptime, prefixes received, address family)
into a table, across both IPv4 and IPv6 unicast, with the local AS and router id
shown above it. Sessions that FRR reports as established are marked as such;
where FRR omits the state field it's inferred from a positive peer uptime, the
same rule parapet uses.

The section is **capability-gated on `vtysh` being present at runtime**. A new
`bgpSupported()` check probes the standard FRR install paths
(`/usr/bin/vtysh`, `/usr/sbin/vtysh`, `/bin/vtysh`) and is surfaced as
`bgp_supported` in `/api/config`. When `vtysh` is installed, Traffic › BGP
appears in the left rail (and in global search); when it isn't — every Windows
host, and any Unix host without FRR — the entry is hidden entirely rather than
offering a page that could only say "not installed." The gating is enforced in
three places (the rail, the search index, and a `renderSection` backstop) so a
hidden section can't be reached by clicking or searching. Because the flag is
read per-load, browsing to manage a remote peer reflects *that* peer's
capability, not this node's. The backing endpoint (`/api/bgp`) mirrors parapet's
response shape: `{available, reason?, peers[]}`, returning `available:false` with
a human reason (`"FRR/vtysh is not installed"`, or `"FRR is not running"`) when
the query can't be served, so the UI degrades to an explanatory line instead of
an error. The `vtysh` call is bounded by an 8s context deadline — the idiomatic,
portable equivalent of the external `timeout` guard parapet wraps the same call
in — so a slow FRR socket right after boot can't hang the request. New unit
tests cover the summary parser (v4/v6, established/idle/inferred-state peers, and
empty/garbage input) and the `vtysh` detection seam.

## v472 — 2026-07-16

The build-from-source installers now delete the build scratch directory
(`BUILD_TMP`) immediately after the binary is installed, instead of holding it
until the end of the run. `BUILD_TMP` holds the Go build and module caches
(hundreds of MB) that `build_from_source` redirects there via GOCACHE/GOPATH;
previously it was only removed by the EXIT trap at the very end, so it sat on
disk through the whole service/firewall/DNS setup, and any run that accumulated
these across repeated installs/upgrades could fill a small partition — the
"no space left on device" build failure seen on OpenBSD, whose `/tmp` is
typically small. Freeing it right after `install`ing the binary keeps the
footprint transient. The EXIT trap stays as the catch-all for exit paths that
never reach the install step; the explicit cleanup is guarded and clears
`BUILD_TMP` so the trap's later pass is a harmless no-op. Applied identically to
all four build-from-source installers (OpenBSD, FreeBSD, Linux, macOS). No code
changes; `sh -n`/`dash -n` clean.

## v471 — 2026-07-16

Settings has a new **Logging** card directly beneath Cluster. **Log level** moved
into it (out of Cluster, where it had been sitting), and a new **Log size**
setting sits alongside it: a text box that caps the log file at a size you type
as `200M`, `1G`, `99K`, or a bare byte count (default 200M). Once the file
reaches the cap it runs FIFO — the oldest lines are dropped from the front to
make room for new ones — so it's a rolling window of the most recent activity
rather than an ever-growing file or a periodic hard wipe.

Backend: `RotatingFile` gained a single-file FIFO mode (`NewFIFOFile`) beside the
existing numbered-backup rotation. When full it rewrites the file keeping only
the newest content, trimmed to whole lines and amortized (it keeps ~half the cap
per trim, so writing past a full file stays cheap rather than rewriting on every
line). Config gained `log_max_size` with a `ParseSize`/`FormatSize` pair
(binary K/M/G/T, optional `B`/`iB` suffix, fractional values); `LogMaxBytes`
now prefers it, defaults to 200 MiB, and floors at 64 KiB, with the legacy
`log_max_mb`/`log_keep` numbered-rotation path kept for older configs.
`Config.Validate` rejects an unparseable size at save time.

Both logging settings apply live, no restart: the daemon's reload path already
updated the running log level, and now also calls the rotating file's new
`SetMaxBytes`, which trims the on-disk file immediately on a shrink (so lowering
the cap takes hold at once instead of only after the file is next written past
the old size). A new `/api/logsize` endpoint reads/writes the cap through the
same `mutateConfig` plumbing as `/api/loglevel`, is proxied the same way (a
Manager can set a peer's cap), and `/api/config` now reports `log_max_size` so
the box populates on load. Both rows are also registered in the Settings search
index. Verified with `go build`, `go vet`, a `node --check` of the extracted
client script, the config/logx/webadmin test suites, and new tests covering the
FIFO rolling window, live shrink, oversized-on-open trim, and size
parse/format/precedence/validation.

## v470 — 2026-07-16

Monitor > logs entries are now clickable. A network id or network name in a log
line jumps to Mesh > networks and ticks that network (clearing any other tick);
a peer id, peer hostname, or seed ip address jumps to Monitor > mesh peers and
ticks that peer. A seed ip resolves to the peer currently answering on that
underlay endpoint; if none is connected it still lands on mesh peers with the
selection cleared.

Rather than parsing the many (and changing) daemon log line formats, the
linkifier matches log text literally against the known entities in state:
`logLinkTokens()` collects each network's id/name, each peer's id/hostname, and
each configured seed address (full `host:port` and bare host) from `state.cfg`
and `state.status`, and `linkifyLog` scans the raw message left-to-right,
emitting a link only for a token that sits on non-alphanumeric boundaries
(so a name isn't matched mid-word and an id isn't matched inside a longer hex
run) and escaping everything else a character at a time. Longest-token-first
ordering makes the most specific match win (a full seed `host:port` over its
bare host, a hostname over a shorter network name it contains). Because it only
ever links strings that map to a real entity, there's nothing to navigate to
that doesn't exist.

Networks and mesh peers tick through their existing, different selection
machinery: mesh peers keeps a persistent `selection.mpeers` set (seeded before
refresh, restored by `wireSelectable` — same path v469 added), while the
networks table has no such set and its ticks live only in the rendered
checkboxes, so `gotoNetwork` ticks the DOM after the refresh instead. Link
click handlers are wired after the rows land in the DOM, the same deferred
wiring the latency table uses; the filter box only hides rows, so they survive
filtering, and a Refresh re-wires from scratch. Verified with `go build`,
`go vet`, a `node --check` of the extracted client script, the webadmin test
suite, and a standalone unit test of the match/escape logic (boundary
rejection, longest-match, HTML escaping).

## v469 — 2026-07-16

Clicking a peer name in Monitor > latency now also *ticks* that peer in
Monitor > mesh peers and clears every other tick, so you land with exactly the
clicked peer selected. Previously the jump only scrolled and flashed the row;
the selection was left untouched, so the shell/info buttons added in v467
would act on whatever happened to be ticked before (or nothing).

The selection is set in `gotoMeshPeer` *before* the `refresh()`, by clearing
`selection.mpeers` and adding `selKey(netId, nodeId)` — the render's own
`wireSelectable` then restores the tick from the set, so this reuses the one
selection source of truth every other path already uses rather than poking
checkboxes after render. Keying the tick by id means it's harmless if the row
never renders (peer dropped between click and re-render): the landing still
falls back to the network card, as before. Verified with `go build`, `go vet`,
a `node --check` of the extracted client script, and the webadmin test suite.

## v468 — 2026-07-16

Peer names in Monitor > latency are now clickable: clicking one jumps to that
peer in Monitor > mesh peers (switches the section, scrolls the row into view,
and gives it the same brief highlight flash the global search uses).

Implementation reuses the existing search-navigation plumbing rather than
adding a parallel one: mesh-peers rows gained a `data-peer` attribute so a row
can be pinned down, and a small `gotoMeshPeer(netId, nodeId)` helper switches
sections and calls the shared `flashAndScroll`. The latency table is keyed by
network *name* only (the /api/latency response carries no network id), so the
name link resolves the id from `state.cfg` before navigating; if the name
isn't found the name stays plain text, and if the target row can't be located
(peer dropped between click and re-render) it lands on the network card
instead. Links are re-wired on every latency poll, since the poll rebuilds the
table wholesale. Verified with `go build`, `go vet`, a `node --check` of the
extracted client script, and the webadmin test suite.

## v467 — 2026-07-16

Monitor > mesh peers now has the same open-shell (`\u25a0`) and info (`\ud83d\udec8`)
buttons that Mesh > peers has. Previously the monitor view was deliberately
selection-free (read-only health only); it now lets you tick a row and open a
shell on that peer, or look it up, without leaving the diagnostics page.

The two buttons reuse the exact gate from Mesh > peers rather than a second
copy: the shell handler was extracted into a shared `openPeerShellFromSel(n,
sec)` (same self-vs-remote / Manager-mode / connected checks), and
`peerInfoRow` gained a selection-namespace parameter. State toggling and Ban
are still absent here by design, so there's no mutating double-click on the
health page.

Selection uses its own namespace (`mpeers`) rather than sharing Mesh > peers'
`peers` set, so a tick made while glancing at health can never carry over and
feed the operate view's Ban button. Verified with `go build`, `go vet`, a
`node --check` of the extracted client script, and the webadmin test suite.

## v466 — 2026-07-16

Copy edit, no behavior change: removed every em-dash from the web-admin UI
strings in `internal/webadmin/ui.go`. This started with the **Log level**
setting description (which also had a rendering bug: that one line encoded the
dash as `\\u2014`, a double backslash, so it showed the literal text
`\u2014` in the browser instead of a dash) and then swept the remaining 82
occurrences across the file.

Prose dashes were reworded rather than deleted, so grammar stays intact:
parenthetical asides became commas or parentheses, appositives and
elaborations became colons, and clause breaks became semicolons or full
stops. Decorative placeholders like the `\u2014 disabled \u2014` endpoint text
were unwrapped to just the label (`disabled`, `connecting\u2026`). The five
empty-cell "no value" glyphs (peer notes, gateway, iface, overlay, and the
generic key/value renderer) were switched from an em-dash to an en-dash
(`\u2013`), the conventional mark for absent tabular data; the three
pre-existing en-dashes elsewhere are numeric ranges and were left alone. No
logic, selectors, or string quoting changed.

## v465 — 2026-07-16

The actual terminal-stuck bug, finally found — from a goroutine dump of a
wedged daemon, not another guess. Every roam-detection change from v456–v464
was chasing the wrong layer; the dump made that unambiguous.

What the dump showed: **no deadlock** (zero goroutines blocked on a mutex),
and all three mesh loops (maintLoop, initLoop, pmtuLoop) alive and idle at
their tickers. So detection and recovery were never wedged — I'd been
debugging a lock/detection problem that didn't exist. What the dump *did*
show: **30 outbound TLS `Dial` read goroutines (plus 6 inbound) all parked in
`transport.readFrame` for 5+ minutes** — roughly 3x the ~13-peer count, and
climbing with each roam.

Root cause, in `internal/transport/tcptls.go`: both the outbound (`Dial`) and
inbound (`readConn`) TLS read loops call `readFrame`, which blocks in
`io.ReadFull` with **no read deadline** — and both loops explicitly clear the
deadline (`SetDeadline(time.Time{})`) after the TLS handshake. After a roam,
the fallback path dials TCP/TLS to peers; a dial to a peer's now-stale
post-roam endpoint can complete the TCP/TLS handshake (to something still
listening, or a connection that lingers) yet never carry another mesh frame.
Its read goroutine then parks in `readFrame` *forever*. Worse, the connection
stays registered in the transport's conn table, so `HasConn` reports it live
and the engine believes it still has a working fallback to that peer — so it
never redials. Across repeated roams these dead-but-registered connections
accumulate, every affected peer is masked as "reachable" over a pipe that
will never deliver anything, and the mesh stays partitioned until a restart
clears the whole table. That is the terminal state — no amount of further
roaming recovers it, because the stuck connections look healthy to every code
path that would otherwise trigger a redial. It explains every observation:
terminal, restart-only, "all peers no reply," and the total absence of
roam/recovery log activity (the loops were fine; the *transport* was full of
zombies).

**Fix:** a rolling idle read deadline on both TLS read loops (`readFrameIdle`,
resetting `tlsReadIdle` = 90s before every frame read). A connection carrying
a live session sees a keepalive every 10s (defaultKeepaliveInterval) and
resets the deadline long before it expires, so healthy peers — including
legitimately relayed/fallback ones — are never affected. A connection that
produces no frame for 90s is, by the mesh's own liveness contract (10s
keepalive, 75s peerTimeout), dead: the read returns i/o timeout, the loop
exits, and its deferred `unregister` closes the socket and removes it from the
conn table — which frees the peer to be redialed (`ensureFallback` sees
`HasConn` false and dials again). No more zombie fallback connections, no more
accumulation, no terminal partition.

**What changed:** `internal/transport/tcptls.go` — new `tlsReadIdle` window
and `readFrameIdle` helper; both the `Dial` and `readConn` read loops use it
instead of the deadline-less `readFrame`.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, full `go test ./internal/mesh/... -short` (143s), transport suite
under `-race`, all pass. Added `TestTLSIdleConnectionTimesOut` (shortens
`tlsReadIdle`, dials, sends nothing, asserts the connection is torn down and
`HasConn` goes false rather than lingering) — reverting to the deadline-less
`readFrame` reproduces the leak (the test hangs the full window and fails),
verified by hand.

Process note, recorded because it matters: this took a goroutine dump to
find, and I burned several releases (v459, v463 especially) guessing at
layers the evidence later ruled out. The dump distinguished "loops wedged on a
lock" (what I kept assuming) from "loops fine, transport full of leaked read
goroutines" (the truth) in one read. A built-in, non-fatal stack-dump trigger
would have gotten here far sooner; adding one (SIGUSR1 → dump to log) is worth
doing as a follow-up so the next hard hang doesn't require SIGQUIT-killing a
production daemon to diagnose.

---



Fixes a regression I introduced in v463. v463 added a physical-default-gateway
roam signal (1b) to catch same-subnet roams — but it made things *worse*, not
better: where v462 recovered, v463 didn't recover at all. The report was
direct and correct ("462 recovers, 463 does not"), and the cause was a
self-inflicted loop that my v463 change created and my v463 tests didn't
catch.

The bug: signal 1b read the "lowest-metric default route" via
`defaultGatewayFn(AF_INET, 0)` — excluding nothing. But in full-tunnel mode
gravinet installs its *own* default route via a tun device and demotes the
physical route's metric (`demotePhysicalDefaultRoute`). So the lowest-metric
default route flips between the physical gateway and gravinet's own tunnel
gateway depending on demotion state. And here's the loop: a detected gateway
change triggers recovery → recovery reasserts full-tunnel OS state → that
demotes/reinstalls the default route → the next per-second check reads a
*different* "best default" → signal 1b fires again → recovery again, roughly
once a second, forever. Each cycle ages and re-dials every peer, so no
handshake ever gets the few seconds it needs to complete. Net effect: the
mesh became *permanently* unrecoverable — strictly worse than not having the
signal at all. (My v463 changelog comment even said reading gravinet's own
tunnel route "errs toward recovering rather than missing a roam" — that
reasoning was wrong; erring toward recovering every second is fatal, not
safe.)

**Fix:** signal 1b now reads the *physical* default gateway via a new
`physicalDefaultGateway` helper that excludes every gravinet tun interface and
rejects any candidate gateway sitting on one of them. With the tun routes
filtered out, the read returns the stable physical gateway regardless of
gravinet's own demotion state — the physical route still exists (just at a
higher metric) and is the only non-tun default — so signal 1b changes only on
a real underlay move, not on gravinet's own routing churn. The same-subnet
roam detection that was the *point* of v463 still works; it just no longer
self-triggers on gravinet's tunnel route.

**What changed:** `internal/mesh/pmtu.go` — new `physicalDefaultGateway`
(enumerates this engine's tun ifindexes, excludes them from the gateway
lookup, rejects a gateway that lands on a tun interface); `checkUnderlayChange`
signal 1b now calls it instead of the naive lowest-metric read. No change to
signals 1 or 2, to the v462 endpoint re-arming, or to recovery itself.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, full `go test ./internal/mesh/... -short` (143s) passes, detection
paths clean under `-race`. Added
`TestCheckUnderlayChangeIgnoresOwnTunnelDefaultRoute` — the regression guard
that was missing in v463: it feeds signal 1b a gateway sitting on gravinet's
own tun ifindex, flipping address each call, and asserts recovery *never*
fires. Reverting `physicalDefaultGateway` to the naive read reproduces the
loop (the test sees the "default gateway changed ... recovering" log fire
repeatedly on the tun ifindex) and fails — verified by hand. The v463
same-subnet-roam test (`...DetectsRoamViaGatewayWhenSourceIPUnchanged`) and
all earlier detection tests still pass.

Lesson recorded honestly: v463's gateway signal was the right idea
(same-source-IP roams are real and were the actual remaining gap), but shipped
without a test for the interaction between "read the default route" and
"gravinet changes the default route," which is exactly where it broke. That
test now exists.

---



v462 helped — a stuck mesh now recovers when you roam to a *different*
network, where before it was terminal until a restart — but it still dies
after 3-4 rapid roams and then won't recover no matter how many more times
you roam within the same set of networks. The "recovers on a *different*
network but not by re-roaming the same ones" detail is the exact tell, and it
points at a hole in roam *detection*, not recovery.

Root cause: roam detection had two signals, and both can be simultaneously
blind to a same-subnet roam. Signal 1 (v456) compares the local source IP the
default route picks; signal 2 (the original) compares the source used to reach
a live peer. Now consider roaming between two networks that hand out the SAME
local IP — two APs on one 192.168.203.x subnet, or the same SSID rejoined with
the same DHCP lease (and every host in this deployment is on 192.168.203.x, so
this is the common case, not a corner one). Signal 1's source address is
unchanged, so it never fires. And once a prior roam has already killed every
peer session, signal 2 has no live peer to measure against, so it contributes
nothing either. The underlay genuinely changed — different gateway, different
L2, old peer endpoints unreachable — but *neither signal can see it*, so no
recovery runs. And because detection is what gates recovery, no subsequent
same-subnet roam re-triggers anything: the mesh stays partitioned until you
land on a network that finally hands out a different IP (signal 1 fires) or a
peer happens to re-handshake in. Exactly the reported behavior.

**Fix:** a third detection signal (1b) — the physical default gateway. Even
when two networks give the same local IP, the gateway's address or its egress
interface index almost always differs across the move.
`checkUnderlayChange` now reads the physical default gateway
(`tun.DefaultGateway`, IPv4, excluding no interface) each check and triggers
the same full recovery when its address or ifindex changes, independent of the
source-IP and peer signals. So a same-subnet roam that both existing signals
miss is now caught by the gateway change. The three signals are complementary:
1 catches a source-IP change with no reachable peer; 1b catches a same-source-IP
roam; 2 catches a moved path to a still-live peer and same-gateway
reconfigurations.

**What changed:** `internal/mesh/engine.go` — new `defaultGW`/`defaultGWIf`/
`haveDefaultGW` fields under `underlayMu`. `internal/mesh/pmtu.go` —
`checkUnderlayChange` adds the gateway signal (signal 1b) and recovers on it
too; imports `syscall` for `AF_INET`. The gateway read is a cheap netlink
route dump once per second, comparable to the per-second source-IP dial
signal 1 already does.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, full `go test ./internal/mesh/... -short` (143s) passes, and the
detection/reconnect paths pass under `-race`. Added
`TestCheckUnderlayChangeDetectsRoamViaGatewayWhenSourceIPUnchanged`: pins the
default-path source constant while flipping the gateway address+ifindex, and
asserts recovery still fires — reverting the gateway signal reproduces the
undetected same-subnet roam and fails the test (verified by hand). All prior
detection tests (`...AnchorStableDoesNotFire`, `...DetectsRoamWithNoUsablePeer
Endpoint`, `...IgnoresReferencePeerSwitch`, `...ReconnectsAllPeers`) still pass
unchanged, and `...AnchorStableDoesNotFire` passing confirms the real
per-second gateway read doesn't spuriously fire on a stable gateway.

Platform note: `tun.DefaultGateway` has a real backend on Linux (netlink) and
is stubbed elsewhere today, so signal 1b is active on Linux and simply
contributes nothing on the other platforms (their gateway read errors out and
the branch is skipped) — no regression there, they keep signals 1 and 2. This
is the third layer of the roam story: v456 detect-with-no-peer, v462 retain-
endpoints-for-redial, v463 detect-a-same-IP-roam.

---



The roam fix from v457 works most of the time now — reported failure rate
dropped from ~80% to ~20% — but a new, sharper symptom surfaced underneath
it: once a roam *does* fail, the mesh never recovers no matter how many more
times you roam, until the service is restarted. That "terminal, roam-
independent, restart-only" shape is the tell, and it points at recovery
having nothing left to act on rather than failing to detect the roam.

Traced it end to end. The roam recovery (`reconnectAllPeers`) ages every
session so the maintenance tick's `pruneDead` tears them down — but
`pruneDead` deletes a reaped session *outright*: the node, its routes, and its
underlay endpoint, all gone. That's fine for a peer that's a configured seed
(initLoop keeps dialing seeds) or one still reachable via a peer we're
gossiping with. But a peer only ever learned via gossip, once its session is
pruned, has **nothing left to dial** — its endpoint was just discarded.
So after a roam that prunes every session, recovery depends entirely on a
configured *seed* being reachable on the new underlay. During a clean roam it
usually is (hence the 80%→20% improvement). During a lossy roam — the ~20%
where the switch is rough and the seeds are momentarily unreachable — every
session gets pruned, nothing gets re-dialed, and the session table empties and
*stays* empty. Every subsequent roam then ages an already-empty table and does
nothing, which is exactly why no amount of further roaming brings it back: the
recovery mechanism only operates on existing sessions, and there are none
left. Only a restart, which re-reads the seed list from config and retries
from scratch, recovers it.

The existing admin "reset" path (`ResetNetwork`) and the ban/unban `redial`
path had already solved this same problem for their cases — they tear a
session down via `localDisconnect`, which explicitly *retains the endpoint*
and re-arms it as a redial target — but the roam path never adopted that,
relying on the hard-pruning `pruneDead` instead.

**Fix:** `reconnectAllPeers` (`internal/mesh/control.go`) now captures each
live peer's last-known underlay endpoint *before* aging the session out, and
re-arms it as a node-tagged redial target (`AddSeedFor`, clearing any seed
backoff so it dials on the next tick). Every former peer — not just the
configured seeds — becomes a standing dial target the maintenance loop keeps
retrying on whatever network we land on. So a lossy roam that prunes every
session no longer empties the mesh permanently: initLoop keeps hammering every
known endpoint until one answers, and recovery no longer hinges on a seed
happening to be reachable at the exact instant of the roam. The node-tagging
means `install()` prunes each re-armed entry cleanly once that peer actually
reconnects, so the seed list doesn't accumulate stale entries, and duplicates
are deduped by `addSeed`. This applies to the suspend/resume path too (both
share `reconnectAllPeers`), which had the same latent gap.

**What changed:** `internal/mesh/control.go` — `reconnectAllPeers` re-arms
reaped peers' endpoints for redial before aging them; expanded doc comment
explaining the terminal-partition mechanism.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, full `go test ./internal/mesh/... -short` (143s) passes, and the
reconnect/roam paths pass under `-race`. Added
`TestReconnectAllPeersRearmsEndpointsForRedial`: a gossip-learned (non-seed)
peer, a roam reconnect, a prune, and an assertion that the reaped peer's
endpoint is now a standing redial target — reverting the re-arm reproduces the
"endpoint gone forever" state and fails the test (verified by hand). Existing
`TestOnResumeForcesReconnect`, `TestCheckUnderlayChangeReconnectsAllPeers`, and
the roam-detection tests still pass unchanged.

Note this is the recovery-durability half; detection (v456's peer-independent
anchor signal) and the initial reconnect (v457) are unchanged. Together: a
roam is detected even with no reachable peer, every peer is torn down and
re-dialed, and — now — every peer's endpoint keeps being retried until it
answers instead of being discarded after one pruning cycle.

---



Consistency tweak: `RestartSec` 5s→8s in both the shipped
`install/gravinet.service` and the runtime-generated unit
(`internal/service/service.go`), matching `TimeoutStopSec=8`. Only that one
line changed; `Restart=always`, `TimeoutStopSec=8`, `SendSIGKILL=yes`
(`[Service]`) and `StartLimitIntervalSec=0` (`[Unit]`) are unchanged, as is
the daemon's 5s `shutdownGrace`. No logic or ordering impact — `RestartSec` is
just the delay before systemd starts the replacement after the process exits,
independent of the stop-phase/watchdog timing.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, and `TestSystemdUnit` pass. Same `daemon-reload` reminder for the
unit-file change to take effect on an existing host.

---



Tuning follow-up to v459: raise the in-daemon `shutdownGrace` watchdog from 4s
to 5s and the systemd `TimeoutStopSec` from 5s to 8s. Ordering preserved —
`shutdownGrace` (5s) stays below `TimeoutStopSec` (8s), so on a wedged teardown
the daemon still force-exits cleanly (with its log line) at 5s and systemd
restarts it, with the 8s `TimeoutStopSec`/SIGKILL as the outer backstop for the
case where even os.Exit can't run. The extra headroom gives a legitimately
slow-but-not-hung teardown (large nftables ruleset, many peers draining under
load) a bit more room to finish cleanly before either mechanism cuts it off.

**What changed:** `cmd/gravinet/main.go` — `shutdownGrace` 4s→5s.
`install/gravinet.service` and `internal/service/service.go` —
`TimeoutStopSec` 5s→8s (both units kept in sync). No logic changes; the
watchdog and unit-generation mechanics are unchanged from v458/v459.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, and `go test ./cmd/gravinet/... ./internal/service/...` (incl. the
`TestShutdownWatchdog*` and `TestSystemdUnit` assertions) all pass. Same
`daemon-reload` reminder as v458/v459 for the unit-file half to take effect on
an existing host.

---



Operator request: add `Restart=always`, `RestartSec=5`,
`StartLimitIntervalSec=0`, and `TimeoutStopSec=5` to the systemd unit, to make
the daemon come back aggressively and never wedge a restart. Applied to both
the shipped `install/gravinet.service` and the unit `gravinet install`
generates at runtime (`internal/service/service.go`), kept in sync.

Two adjustments to what was literally asked, both called out here:

- **`StartLimitIntervalSec=0` goes in the `[Unit]` section, not
  `[Service]`.** It's a unit-level directive; systemd ignores it (with a
  warning) if placed under `[Service]`. Put it in `[Unit]` so it actually
  takes effect. `TestSystemdUnit` now asserts it appears before the
  `[Service]` header so this placement can't regress.
- **Lowered the daemon's own `shutdownGrace` watchdog from 15s to 4s**
  (`cmd/gravinet/main.go`) to stay under the new `TimeoutStopSec=5`. v458 set
  the watchdog to 15s and the unit's stop timeout to 30s so the daemon's clean
  force-exit (with a log line) always beat systemd's SIGKILL. With
  `TimeoutStopSec=5`, a 15s watchdog would never get to fire — systemd would
  SIGKILL at 5s first, making the watchdog dead weight on systemd and losing
  the clean "forcing exit" log line and any last-ditch cleanup. Dropping it to
  4s restores that ordering: on a wedged teardown the daemon logs and
  `os.Exit(1)`s at 4s, systemd sees the exit and restarts, and the 5s
  `TimeoutStopSec`/SIGKILL remains the outer backstop for when even os.Exit
  can't run. The shutdownGrace doc comment now notes it must track
  TimeoutStopSec if that's changed.

Effect of the full set: `Restart=always` restarts on any exit, not just
failures (a `systemctl stop` is still a clean operator stop and does not
loop-restart). `StartLimitIntervalSec=0` disables the start-rate limiter so a
crash/restart loop never lands the unit in a permanent failed state — it keeps
retrying every `RestartSec=5`. `TimeoutStopSec=5` bounds the stop phase
tightly; combined with the 4s in-daemon watchdog and `SendSIGKILL=yes`
(retained from v458), a stuck teardown can no longer hang a restart, and the
process is guaranteed gone within ~5s either way.

Trade-off worth knowing: `TimeoutStopSec=5` (and the 4s watchdog) means a
teardown that is legitimately slow-but-not-hung — e.g. clearing a large
nftables ruleset or draining many peers under load — could be cut off before
it finishes cleaning up OS state. gravinet reconciles most of that state on
the next startup anyway (stale hosts/DNS entries are cleared on boot, routes
and nftables rules are rebuilt), so a truncated teardown is recoverable rather
than corrupting, but if you run very large meshes and would rather trade
restart latency for guaranteed-complete cleanup, raise both `TimeoutStopSec`
and `shutdownGrace` together (keeping the latter below the former).

**What changed:** `install/gravinet.service` and
`internal/service/service.go` — `[Unit]` gains `StartLimitIntervalSec=0`;
`[Service]` `Restart=on-failure`→`Restart=always` and
`TimeoutStopSec=30`→`TimeoutStopSec=5` (`RestartSec=5`, `SendSIGKILL=yes`
unchanged). `cmd/gravinet/main.go` — `shutdownGrace` 15s→4s.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for all five
platforms, and `go test ./... -short` (with `-race` on cmd/gravinet and
internal/service) all clean. `TestSystemdUnit` extended to assert
`Restart=always`, `StartLimitIntervalSec=0`, and its `[Unit]`-section
placement; the watchdog tests (`TestShutdownWatchdog*`) still pass with the
shorter grace since they pass their own durations.

Reminder from v458, still applies: a unit-file change only takes effect after
`systemctl daemon-reload` (then `systemctl restart gravinet`); an existing box
needs the reload to pick up these directives. The `shutdownGrace` change is in
the binary and applies as soon as the v459 build runs.

---



Report: `systemctl restart gravinet` sometimes hangs. Wanted a way to
guarantee the restart actually happens.

Cause: the systemd unit is `Type=notify`, which means systemd treats the
stop phase as complete only when the daemon process actually exits — and on a
restart it won't start the replacement until the old process is gone. The
daemon's graceful shutdown runs a sequence of teardown steps
(`engine.Stop()` waiting on the TUN read loops, device and transport closes,
the nftables ruleset `Clear()`, DNS/hosts cleanup) and every one of them can,
in the wrong circumstances, block indefinitely on the kernel or on a
shelled-out subprocess. If any step wedges, the process never exits, and
`systemctl restart` waits on it — with no bound, because the shipped unit set
no `TimeoutStopSec` (it inherited only whatever distro-global default
existed, e.g. the `10-timeout-abort.conf` drop-in seen on the reporting Fedora
box, which may be generous or absent). That's the intermittent hang: it
depends on whether a given stop happened to hit a teardown step that blocked.

Fixed with two independent guarantees, either of which alone terminates the
stop:

1. **In-daemon shutdown watchdog** (`cmd/gravinet/main.go`). The moment
   graceful shutdown begins, a background timer is armed; if teardown hasn't
   completed within `shutdownGrace` (15s — far longer than a healthy
   shutdown), the process force-exits (`os.Exit(1)`, non-zero because a
   forced stop isn't clean). A shutdown that finishes first disarms it, so
   the hard exit only ever fires on a genuine hang. This makes the daemon
   responsible for its own bounded exit rather than depending on the service
   manager's patience, and it covers the self-restart path too: if the
   post-roam/-resume `selfRestart` re-exec is preceded by a wedged teardown,
   the watchdog exits with failure and systemd's `Restart=on-failure` brings
   it back rather than leaving it hung.
2. **systemd stop timeout + kill escalation** (`install/gravinet.service`
   and the runtime-generated unit in `internal/service/service.go`).
   `TimeoutStopSec=30` (above `shutdownGrace`, so the daemon's own clean-ish
   exit wins the race when it can) bounds the stop from systemd's side, and
   `SendSIGKILL=yes` ensures the final SIGTERM→SIGKILL escalation is never
   disabled. This is the outer backstop for the pathological case where even
   `os.Exit` can't run (e.g. the process stuck in an uninterruptible kernel
   wait).

Both the shipped unit file and the unit `gravinet install` generates at
runtime get the timeout directives, so existing and fresh installs are both
covered. (Note: a unit-file change only takes effect after `systemctl
daemon-reload` — the changelog / install docs mention this, but a box that
installed an older unit needs the reload or a reinstall to pick up the
backstop; the in-daemon watchdog, by contrast, is in the binary and applies
as soon as the new build is running.)

**What changed:** `cmd/gravinet/main.go` — new `shutdownGrace` const and
`armShutdownWatchdog` helper (extracted so the arm/disarm race is testable
without exiting the process), with the daemon's `shutdown()` arming it around
the whole teardown. `install/gravinet.service` and
`internal/service/service.go` — `TimeoutStopSec=30` and `SendSIGKILL=yes`
added to the `[Service]` section, kept in sync between the two.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for
linux/darwin/windows/freebsd/openbsd, and full `go test ./... -short` (plus
`-race` on cmd/gravinet and internal/service) all clean. Added
`TestShutdownWatchdogDisarmedBeforeGraceDoesNotFire`,
`TestShutdownWatchdogFiresWhenNotDisarmed`, and
`TestShutdownWatchdogDisarmIdempotent` (`cmd/gravinet`), covering the clean
path (disarm cancels the force-exit), the wedged path (force-exit fires), and
idempotent disarm. Extended `TestSystemdUnit` (`internal/service`) to assert
`TimeoutStopSec=` and `SendSIGKILL=yes` are present so the backstop can't
silently regress.

Caveat on scope: this guarantees the *process exits* so the restart can
proceed — it does not make a wedged teardown itself clean up gracefully. If a
step is regularly hitting the 15s watchdog, that underlying hang is still
worth chasing (it'll show as the "graceful shutdown exceeded 15s — forcing
exit" warning in the log); the watchdog just ensures it can no longer take the
restart down with it.

---



The actual roam bug — not detection this time (v456 fixed that), the
recovery itself. After a roam, every peer reads "no reply" except the odd one
that happens to re-handshake toward this host on its own (in the report,
gn-macos, freshly 3.5ms while all 12 others were dead). That one-peer-alive
detail is the tell: the overlay works, the transport socket works (it's
wildcard-bound, so it survives the underlay change), and inbound handshakes
land fine. What's broken is that this host never re-dials its existing peers
after the roam — so every session stays pointed at an endpoint that was only
reachable on the old underlay, and black-holes.

Root cause, traced end to end: recovery from a roam and recovery from a
laptop wake are supposed to be the same thing — tear every session down so
the maintenance loop re-dials every peer from scratch — but only the wake
path actually did it. `onResume` (suspend/resume) ages every session past the
peer timeout so that tick's `pruneDead` reaps them all, freeing each peer's
endpoint for `initLoop` to re-dial. `checkUnderlayChange` (a live roam) did
NOT: it only reset path-MTU and re-asserted OS routes, then fired the restart
hook and hoped. Resetting PMTU does nothing for a session whose endpoint is
now unreachable, and re-asserting routes doesn't re-dial anyone. So the
sessions just sat there. And critically, a peer learned via gossip (i.e. not
a configured seed) has no independent re-dial trigger at all until its
session is pruned — and nothing was pruning it, because `pruneDead` only
reaps sessions already silent past the timeout and a roam doesn't age them.
The only peers that recovered were seeds (re-dialed by `initLoop` once their
sessions eventually timed out on their own) and peers that re-handshook
toward us first. Everyone else stayed dead until the peer-timeout elapsed,
and then only if they were seeds. That's the whole "no reply to everyone but
one, doesn't self-heal" picture.

This is also why every prior release in this series missed it: v451–v456 were
all upstream of this point (making the roam get *detected* and the restart
*fire*), but the in-process recovery that runs on detection never contained
the one step — re-dial every peer — that a roam actually needs, so unless the
restart hook fired and fully rebuilt the process, detection led to a
recovery that didn't recover.

**Fix:** the roam path now performs the same full-peer reconnect the wake
path does. Extracted `onResume`'s session-aging teardown into a shared
`reconnectAllPeers` (internal/mesh/control.go) and call it from
`checkUnderlayChange` on any detected roam, for every network, before the
existing PMTU-reset and OS-reassert steps. Every peer — seed or gossip-
learned — is now aged, pruned this same maintenance cycle, and re-dialed
from a clean slate against whatever endpoint gossip currently has, exactly as
after a laptop wake. The grace-gated one-shot restart hook stays as a
belt-and-braces fallback, but is no longer load-bearing: a mesh with
restart_on_underlay_change disabled now recovers from a roam on its own
instead of staying partitioned until session timeout.

**What changed:** `internal/mesh/control.go` — `onResume` refactored to call
the new `reconnectAllPeers` (identical behavior; it's the extracted core
plus the OS-state reassert it always did). `internal/mesh/pmtu.go` —
`checkUnderlayChange`'s recovery block calls `reconnectAllPeers` for every
network first, so a detected roam re-dials all peers rather than only
resetting PMTU/routes and deferring to the restart hook.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for
linux/darwin/windows/freebsd/openbsd, full `go test ./... -short` (mesh +
webadmin + everything), and `-race` on the underlay/resume/reconnect paths
all clean. Added `TestCheckUnderlayChangeReconnectsAllPeers`: a live,
non-seed peer session pointed at an old-underlay endpoint, a detected roam
(via the v456 anchor signal, so no live peer endpoint is needed to detect
it), and an assertion that the session is aged past the timeout and reaped by
`pruneDead` this cycle — i.e. it will be re-dialed. Reverting the
`reconnectAllPeers` call reproduces the black-hole (session stays fresh,
never pruned) and fails the test, verified by hand. Existing
`TestOnResumeForcesReconnect` still passes, confirming the refactor left the
wake path's behavior intact.

---



Report: after switching underlay networks (with or without a default route),
the peer-latency view sometimes shows "no reply" for *every* peer at once,
and stays that way — the same fully-partitioned-after-roam symptom v451's
recovery path was meant to catch, but intermittently slipping through it.
"Sometimes, not always" was the clue to the exact gap.

Root cause: roam detection (`checkUnderlayChange` in
`internal/mesh/pmtu.go`) had only one signal, and it was anchored to a
*peer's* reachability. It picks a directly-connected peer, asks the kernel
"what local source address would I use to reach that peer's underlay
endpoint?" (`localSourceIP`, a routeless UDP connect), and treats a change in
that answer as a roam. The flaw: that lookup is against the peer's *stored,
pre-roam* endpoint. When the roam lands you on a network with no route back
to those old endpoints — precisely the case that partitions the whole mesh
and is the entire reason recovery needs to run — the lookup either can't
resolve a source or the reference peer's endpoint isn't usable, and
`checkUnderlayChange` returned early without detecting anything. No PMTU
reset, no OS-state reassert, and critically no restart-hook fire. Whether it
detected the roam came down to whether, at the instant of the 1-second check,
*some* peer's old endpoint still happened to be routable on the new
network — which is exactly the observed "sometimes."

Fix: add a second, **peer-independent** roam signal that doesn't depend on
any peer's stored endpoint. `defaultPathSourceIP` asks the kernel "what
source address does my current default route pick for a generic off-subnet
destination?" — using fixed documentation-reserved anchors (TEST-NET-1
`192.0.2.1` and `2001:db8::1`, RFC 5737/3849, never real hosts, never
actually contacted — it's a pure route lookup). That answer changes exactly
when the host's default egress path changes, i.e. on a roam, regardless of
whether any peer is currently reachable. `checkUnderlayChange` now runs both
signals every check and triggers recovery if *either* fires; the original
peer-anchored signal is kept because when it does fire it's more direct
evidence a real peer path moved, and can catch same-default-source
reconfigurations the anchor wouldn't. The peer-independent signal is what
closes the "everything unroutable" hole.

**What changed:** `internal/mesh/pmtu.go` — new `defaultPathSourceIP` (+
`defaultPathAnchors`), and `checkUnderlayChange` restructured to run the
anchor signal and the peer signal independently and recover on either.
`internal/mesh/engine.go` — new `defaultPathSrc`/`haveDefaultPath` fields
tracking the anchor source across checks, under the existing `underlayMu`.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for
linux/darwin/windows/freebsd/openbsd all clean. Full `go test
./internal/mesh/... -short` passes; the underlay-change paths also pass under
`-race`. Added `TestCheckUnderlayChangeDetectsRoamWithNoUsablePeerEndpoint`
(injects a flipping default-path source with zero usable peers and asserts
the restart hook fires — reverting the anchor signal reproduces the missed
roam and fails it, verified by hand) and
`TestCheckUnderlayChangeAnchorStableDoesNotFire` (a steady anchor source must
not spuriously trigger recovery now that the check runs every second). The
existing peer-anchored tests (`TestCheckUnderlayChangeIgnoresReferencePeer
Switch`, `TestCheckUnderlayChangeRunsWithPMTUDiscoveryDisabled`, the
notify-once/grace tests) still pass unchanged.

Note this makes detection far more robust but recovery itself is still the
same v451 machinery (in-process PMTU/OS-state reassert, plus a grace-gated
one-shot restart hook). If a roam is now detected but a given host still
doesn't fully recover without the restart, that restart is what rebuilds it —
so `restart_on_underlay_change` being enabled matters for the worst cases,
same as before.

---



The macOS Metrics gap, actually diagnosed this time — and with a correction
to v454, which made one part of it worse. The deciding evidence was in the
screenshots: the per-interface **rx** line is jagged, varied, real
per-sample data, and it stops short of the right edge at *exactly* the same
x-position as the flat CPU and disk lines. Every series has plenty of
points; they all just end at the same spot, a fixed distance from the edge.
That rules out everything the last three releases chased — a slow reader, a
flaky reader, clock skew — because none of those would land every series'
newest point at the identical short position. It's not a freshness problem
and not a per-reader problem. It's that the newest point's *timestamp* is a
fixed amount older than where the chart draws "now", uniformly, because of
two concrete mechanical facts:

1. **Points were timestamped before their readers ran, not after.**
   `sample()` captured `now = time.Now()` at the top, then blocked on the
   readers. On macOS the CPU reader shells out to `top -l 1`, which by
   design waits ~1s collecting an initial reference frame before it emits
   even a single sample — so every point was stamped ~1s+ earlier than the
   moment it actually represented. On Linux/BSD the readers are effectively
   instant, so `now`-at-top and `now`-after-read are the same instant and
   nothing showed. This is the macOS-specific part.

2. **v454's `server_now` edge anchor guaranteed a gap.** v454 (correctly
   diagnosing that the browser clock was the wrong reference) switched the
   chart's right edge to the server's clock — but read *fresh* in
   `snapshot()` at request time, which is strictly newer than any data
   point, so the freshest possible point still couldn't reach the edge.
   Combined with (1), the gap was (top's ~1s delay) + (time since last
   sample). v454 didn't cause the macOS gap, but its anchor made even a
   correctly-timestamped point unable to close it.

**The fix, in three parts:**

- `internal/webadmin/metrics.go`: `sample()` now captures its timestamp
  *after* the readers return, so a point is dated when it was actually
  collected. (This is the direct fix for fact 1.)
- `internal/webadmin/ui.go`: the chart's right edge is now anchored to the
  **newest actual sample timestamp across all series**, not to any
  wall-clock (`renderMetricGraphs`). The line's last point is drawn at its
  own timestamp's x-position, so anchoring the edge there is what makes the
  line reach the end — on every platform, regardless of how old that newest
  point is relative to real time. This is what instant-reader platforms were
  already effectively getting for free; macOS now gets it explicitly. "now"
  on the axis means "the most recent reading," which is what a live chart's
  edge should represent. (This corrects fact 2. `server_now` is kept only as
  the fresh-boot fallback when there are no points yet, then `Date.now()`.)
- `internal/webadmin/metrics.go`: `run()` now dispatches each `sample()` in
  its own goroutine (with an atomic in-flight guard so they can't pile up)
  instead of calling it inline, so a ~1s macOS sample no longer stretches
  the 2s ticker cadence out to ~3s and thins the point density. Secondary to
  the two above, but it's why macOS also had visibly fewer points per
  window.

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for
linux/darwin/windows/freebsd/openbsd, and `node --check` on the extracted
`<script>` body all clean. Full `go test ./internal/webadmin/... -short
-race` passes (including the new goroutine-dispatched sampling path under the
race detector). Added `TestSampleTimestampsPointsAfterReadersFinish`, which
gives the CPU reader a 1.2s artificial delay and asserts the resulting
point's timestamp falls after the readers finished, not before — reverting
the timestamp fix reproduces the pre-read stamp and fails the test (verified
by hand). Existing `server_now` assertions retained (it's still emitted for
the fallback path).

Unlike v452–v454, this isn't reasoning from indirect signals: the "jagged
line stops at the same x as the flat lines" detail in the screenshots is
only consistent with a uniform timestamp-vs-edge offset, and both
contributing offsets (pre-read stamp; fresh-`server_now` edge) are now
removed at the source. If anything still stops short after this, it would
have to be a point actually carrying a stale timestamp out of `sample()`,
which the new test rules out for the reader path.

---



Third round on the macOS Metrics-tab report: back to all four graphs — CPU,
memory, disk, and network — not just memory. That's the detail that finally
points at the right layer: v452 (concurrent readers) and v453 (carry-forward
on failure) each targeted a specific way *one* series' data could lag or
gap, but neither touches how the chart decides where "now" is in the first
place — and if every series is behind by the same amount regardless of which
reader produced it, the bug was never in the readers at all.

Every point plotted, on every graph, carries the server's own timestamp
(`sample()`'s `now`, `time.Now().Unix()`). But the frontend never uses that
timestamp to decide where the chart's right edge is — `chartLayers` and the
hover handler in `ui.go` each independently compute `Math.floor(Date.now()
/1000)`, i.e. the *browser's* clock, and draw "now" there. Those are only
the same instant if the browser's machine and the gravinet host's clock
agree. If they don't — even by a handful of seconds, which is all it takes
to see at the 1-minute zoom this was reported at — every series falls short
of the edge by exactly that gap, uniformly, no matter how fresh the
underlying reader actually was. Unlike v452/v453, this explanation doesn't
require the macOS readers to be doing anything wrong at all, which fits: two
rounds of legitimate fixes to those readers changed nothing about the
report.

Didn't get to confirm the two clocks actually disagree on the reporting
box — that needs `date +%s` run at the same moment on both ends, which
wasn't available this round either. But the fix doesn't require confirming
that first: making the chart's "now" reference the server's own clock
instead of the browser's is correct regardless of whether these two
particular clocks are in sync, and removes the dependency entirely rather
than working around a specific skew.

**What changed:** `internal/webadmin/metrics.go`'s `snapshot()` now includes
`server_now` (the same `time.Now().Unix()` used as every point's own
timestamp) in the `/api/metrics` response. `internal/webadmin/ui.go`'s
`renderMetricGraphs` reads it and threads it through as `nowRef` to
`graphCard`, `chartLayers`, and `chartSVG` — replacing their own
`Date.now()` calls — and the hover crosshair (`attachChartHover`) now reads
the same value back off the card's state (`hs.now`, updated on every redraw)
instead of taking a fresh, separately-sourced `Date.now()` reading of its
own, so the crosshair always agrees with wherever the line was actually
drawn. Falls back to the browser's clock only if a response somehow lacks
`server_now` (defensive; shouldn't happen going forward).

**Verified:** `go build ./...`, `go vet ./...`, cross-compiles for
linux/darwin/windows/freebsd/openbsd, and `node --check` against the
extracted `<script>` body (ui.go embeds it as one Go string; this file has
no separate `.js` to run through a normal toolchain) all clean. Full
`go test ./internal/webadmin/... -short -race` passes. Extended
`TestMetricsCollectorSample` and `TestHandleMetrics` to assert `server_now`
is present, current, and survives the real JSON wire format (not just the
in-process map).

Same as v453: haven't been able to confirm this resolves what's actually
happening on the reporting Mac, since that still needs eyes on that specific
box. If the graphs still don't reach "now" after this, the next useful thing
to check is exactly the `date +%s`-on-both-ends comparison this round didn't
have — at that point either the clocks genuinely disagree (and this fix
should have closed the gap) or they don't (and the cause is neither clock
skew nor anything the last three releases addressed, which would mean
looking somewhere new rather than at metrics.go/ui.go again).

---



Second follow-up on the macOS Metrics-tab lag (v452): after that fix, CPU
now visibly reaches "now" — it's live, moving, right up to the edge — but
Memory still doesn't, still flat, still stopping short. That's a useful data
point in itself: v452's fix (running the readers concurrently) helped the
graph that was slow because *everything* was slow. Memory not improving
means it isn't "everything is slow" anymore — it's something specific to the
memory reader.

Tried to get a live `/api/metrics` payload or a DevTools capture to see
`mem`'s actual timestamps against the real clock and pin this down properly
rather than guess again; wasn't able to get that this round. Reasoned it out
from the code instead: CPU and Memory are both written inside the same
`sample()` call, from the same `now` variable — so on any tick where *both*
successfully update, they get the exact same timestamp, by construction. The
only way CPU's newest point can be visibly more recent than Memory's is if
there have been ticks where CPU's reader (`readCPUTotals`, one subprocess:
`top`) succeeded while Memory's (`readMemUsedPct`, two subprocesses:
`sysctl` then `vm_stat`) didn't — which, since a manual `vm_stat; sysctl -n
hw.memsize` on the actual box runs fine and returns cleanly parseable
output, points at something about the *daemon's* invocation of these
specifically that a manual interactive shell doesn't hit — model differences
in a launchd-managed process (environment, resource limits, concurrent-fork
timing now that v452 fires 5 subprocess spawns from 5 goroutines at once
instead of one at a time) are plausible candidates, but this couldn't be
confirmed without a real Mac or the diagnostic output this round didn't turn
up.

Rather than guess at which of those and patch that one guess, made the
collector resilient to the underlying failure mode instead of trying to
prevent it: if any reader fails on a tick after previously succeeding, the
affected graph now carries its last known value forward (a new point, at the
current tick's timestamp, repeating the last good reading) instead of just
silently skipping that tick. A graph that's briefly flat because its last
real reading hasn't changed yet is a much smaller problem than one whose
newest point quietly falls further and further behind "now" with every
failed tick — which is exactly the shape of what's been reported. This holds
regardless of what's actually causing the macOS memory reader to fail, so it
should genuinely help whether or not the reasoning above is exactly right.

Also added logging for this, specifically so the *next* report doesn't have
to end in "wasn't able to get that this round": every reader now logs (once,
on the true→false transition — not spammed every 2s) when it starts failing
after a run of successes, and again when it recovers. That's a plain
`tail /var/log/gravinet.err.log` away — no browser, no DevTools, no session
cookie — if this comes back.

**What changed:** `internal/webadmin/metrics.go` — `metricsCollector` tracks
the last successfully-computed CPU/memory/disk percentage; `sample()` now
re-appends that value at the current timestamp when the corresponding reader
fails after previously succeeding, and logs (`Warnf`/`Infof`) on every
failing/recovered transition. Deliberately not applied to per-interface
rx/tx throughput — a carried-forward byte rate is a much less honest
stand-in than a carried-forward percentage, so a `netstat` hiccup still just
skips that tick there, same as before.

**Verified:** `go build ./...`, `go vet ./...`, and cross-compiles for
linux/darwin/windows/freebsd/openbsd all clean. Full
`go test ./internal/webadmin/... -short -race` passes. Added
`TestSampleCarriesLastValueForwardOnFailure`, which fails a fake memory
reader after one successful sample and confirms `sample()` keeps appending
the carried-forward value (with an advancing timestamp) through the failure
and resumes real readings on recovery — reverting the carry-forward logic
reproduces the original "stuck at one point forever" shape and fails the
test (verified by hand). Log-transition messages confirmed present in that
test's own output.

Have not been able to confirm this actually resolves the report — that
needs the same real Mac. If the line still doesn't reach "now" after this,
the `/var/log/gravinet.err.log` tail mentioned above should say why, which
would move this from reasoning to evidence.

---



Follow-up report on the Metrics tab, macOS only: at the 1-minute zoom, the
graph lines visibly stop short of the chart's right edge instead of running
up to "now". First reported as a Memory-only issue; turned out to be every
graph — CPU, memory, disk, and every interface's throughput — once looked at
more carefully.

That "every graph, all at once" shape is the tell. All of them are populated
by one shared function, `metricsCollector.sample()`, on a single 2-second
ticker (`metricSampleInterval`). On Linux/FreeBSD/etc. its readers are cheap
proc-file or syscall reads — sub-millisecond. On macOS
(`internal/webadmin/metrics_darwin.go`) most of them shell out instead: `top`
for CPU, `sysctl` + `vm_stat` for memory, `sysctl` again for uptime, `netstat`
for interface counters — five subprocess spawns a sample, and `top -l 1`
alone has its own ~1s internal sampling delay by design (see its doc
comment). Run one after another, as `sample()` did, that easily adds up to
more than the 2-second budget on a loaded or throttled Mac. Since every
series only ever gets a new point from that one function, a slow run doesn't
delay one graph — it pushes CPU, memory, disk, and every interface's newest
point behind actual wall-clock time by the same margin, together. The
frontend (`ui.go`'s `renderMetricGraphs`) draws the chart's right edge from
the browser's own `Date.now()`, not from whatever the newest point happens to
be, so that lag is exactly what shows up as every line falling short of
"now" — worse the finer the zoom, since a few seconds of staleness is a much
bigger fraction of a 1-minute window than a 60-minute one, which is why 1 min
was where this was actually visible.

Holding `m.mu` for the full, now-serialized-and-slow reader sequence
compounded it: a concurrent `/api/metrics` request had to wait out the same
delay just to read data that was already stale by the time the lock finally
freed.

**What changed:** `internal/webadmin/metrics.go`'s `sample()` now runs the
CPU/memory/disk/uptime/interface readers concurrently and only takes `m.mu`
once they've all returned, so wall time is bounded by the slowest single
reader (in the worst case, `top`'s ~1s) rather than their sum — and a
`/api/metrics` request is never stuck behind the collection cycle. No
behavior change on platforms where these were already fast; this is purely
about not letting several independently-slow readers serialize on macOS.

**Verified:** `go build ./...`, `go vet ./...`, and cross-compiles for
linux/darwin/windows/freebsd/openbsd all clean. Full `go test ./... -short
-race` for `internal/webadmin` passes. Added
`TestSampleRunsReadersConcurrently` (`internal/webadmin/metrics_test.go`),
which substitutes two independent 200ms-delayed fake readers (memory and
network — swappable via the new `readMemUsedPctFn`/`readNetDevFn` package
vars, same pattern as `internal/mesh/fulltunnel.go`'s `defaultGatewayFn`) and
asserts `sample()` returns in well under the 400ms two sequential delays
would take; reverting to sequential calls reproduces the original
symptom — the test then fails at ~400ms (verified by hand).

---



Field report: a node roamed (took a new default route after switching Wi-Fi
SSIDs) and never recovered on its own — mesh sessions stayed dead and the
web admin's own control port dropped off entirely (`netstat` showed nothing
on 8443, not even the loopback listener), for as long as the process was
left alone. `systemctl status` kept reporting the *original* boot's PID and
start time throughout — the daemon wasn't crashing or exiting, it just sat
there, deaf, until a manual `systemctl restart gravinet` brought it back.
Two independent bugs in the roam-recovery path, both found while tracing
that report through to its actual root cause rather than the mesh-level
symptom in the logs (`rejecting advertised route 0.0.0.0/0`, repeated
identically many times) that looked like the obvious suspect at first. That
turned out to be a separate, pre-existing, and much smaller issue —
`onRouteAdd` (routes.go) logs a reject on every single re-advertisement of an
already-rejected route with no dedup, so a peer's routine periodic
re-gossip of a route this node rejects produces exactly that kind of
repeated-line noise at DEBUG level. Noisy, but not what caused the outage;
left alone here rather than folded into this fix.

**Bug 1 — restart-on-underlay-change could deadlock the daemon against
itself.** `cmd/gravinet`'s roam/suspend-resume recovery path tears the
process down cleanly and calls `selfRestart` (an in-place `syscall.Exec`).
If that ever fails, it falls back to asking the platform service manager to
restart gravinet — `internal/service.Restart()`. On Linux, macOS, FreeBSD,
and OpenBSD that fallback ran `systemctl restart` / `launchctl kickstart -k`
/ `service gravinet restart` / `rcctl restart` synchronously
(`exec.Command(...).Run()`). Every one of those is a stop-then-start cycle
whose stop half waits for gravinet's own main process to exit — but by the
time this fallback runs, that process already shut everything down (closing
the web admin — explaining the missing 8443 listener — and the mesh
sessions) in the very same goroutine that's about to block waiting for the
restart command. The stop phase waits on this goroutine; this goroutine
waits on the stop phase. Neither side can move. The process doesn't exit or
crash — the OS and service manager keep reporting it "running" — it just
sits there inert until something external (an operator, or the service
manager's own stop timeout escalating to SIGKILL) intervenes. The `windows`
branch had already been written to avoid exactly this (see its doc
comment — PowerShell's `Restart-Service` has the identical hazard), and the
FreeBSD rc.d script's bounded `stop_cmd` override (v-whatever added it after
a similar hang during upgrades) mitigates it for that one path — but
`service.Restart()` itself, used directly by this recovery path and by the
web admin's own Restart button, never got the same treatment on the other
four platforms.

**Bug 2 — checkUnderlayChange (roam detection itself) was silently gated
behind PMTU discovery being enabled.** `checkUnderlayChange` — which detects
the roam, resets path MTU, re-asserts OS state, and invokes the
restart-on-underlay-change hook — only ever ran from inside `pmtuLoop`,
which returned before its first tick whenever PMTU discovery was disabled
(`underlay_mtu_max <= underlay_mtu`, or `pmtu_discovery: false`). That
silently took the entire roam-recovery mechanism down with it, even though
`restart_on_underlay_change` is documented as its own independent config
knob — an operator could reasonably leave restart-on-roam on while turning
probe-based discovery off (e.g. to cut probe traffic on a metered link,
which is of course exactly the kind of link most likely to roam) and get
none of the protection they asked for, with nothing anywhere telling them
so.

Neither bug alone is fully confirmed as what happened on the reporting
node — its config wasn't available to check — but both are real,
independently reproducible, and sit squarely in the one code path a Wi-Fi
roam exercises. Fixed both rather than guess between them.

**What changed:** `internal/mesh/pmtu.go`'s `pmtuLoop` no longer returns
early when PMTU discovery is disabled; it keeps ticking once a second and
still calls `checkUnderlayChange` every tick, skipping only the per-peer
`pmtuTick` calls that discovery itself needs. `internal/service/service.go`
gets a new `detachedRestart` helper (`Start`, not `Run`) that every platform
branch of `Restart()` now goes through — including `windows`, which already
did the equivalent inline; that branch now just calls the shared helper.

**Verified:** `go build ./...` and `go vet ./...` clean. Full test suite
(`go test ./... -short`) passes. Added
`TestCheckUnderlayChangeRunsWithPMTUDiscoveryDisabled`
(`internal/mesh/pmtu_reset_test.go`), which spins up a real engine with
`UnderlayMTUMax == UnderlayMTU` and a connected peer and confirms
`checkUnderlayChange` still rebases `underlayRefNode` within a few ticks;
reverting the `pmtuLoop` fix reproduces the original failure (verified by
hand — the test hangs at "never ran" instead). Added
`TestDetachedRestartDoesNotBlock` and `TestDetachedRestartReportsLaunchFailure`
(`internal/service/service_test.go`): the first launches a 5-second child
through `detachedRestart` and asserts the call itself returns in well under
a second (reverting to `.Run()` makes it block for the full 5s and fail —
verified by hand); the second confirms a command that can't even launch
still reports an error rather than being silently swallowed.

---



Fixed sidebar sub-menu indent for real this time. v448's fix (`30px` →
`34px`) treated the problem as "a few pixels off" when it was actually a
wrong reference point: a group label's text (e.g. "MESH") starts at `12px`
(label padding) + `24px` (chevron box) + `7px` (flex gap) = `43px` from the
rail's left edge, and `34px` — even after that bump — put child items to
the *left* of that, not indented under it. Screenshot from the field showed
`networks`/`keys`/`seeds`/`peers`/`bans` starting almost directly under the
chevron instead of under the "MESH" label. `.rail-group .rail-tab`'s
`padding-left` is now `58px` — `43px` (where the label text starts) plus
roughly two more monospace character-widths, so children read as clearly
nested under their group's label rather than sitting beside its chevron.

**What changed:** one CSS rule in `internal/webadmin/ui.go`'s embedded
`indexHTML` stylesheet — `.rail-group .rail-tab { padding-left:34px; }` →
`padding-left:58px;`. No JS or markup changes.

**Verified:** mocked the label/chevron/item layout pixel-for-pixel (same
padding/gap/font-size values as the real CSS) with PIL and rendered it
side-by-side with the reported screenshot to confirm items now sit visibly
right of "MESH" before shipping, rather than trusting the arithmetic alone.
`go build ./...` and `go vet ./...` clean; extracted the embedded script and
ran `node --check` clean; backtick count in the raw string still exactly 2.

---

## v449 — 2026-07-15

QoS rules can now be defined by named service, same as firewall rules — per
feedback that firewall's move to a service catalog made QoS's own separate
proto/port fields feel inconsistent. A `QoSRule` gained a `Services []string`
field that names entries from the same node-global catalog firewall rules
already resolve their own `Services` field against (`Config.FirewallServices`
— there's exactly one catalog, shared by both), unioned with the rule's
literal `Protocol`/`PortMin`/`PortMax` leg exactly the way `FirewallRule`
unions its inline proto/port with its named services: a rule can carry a
literal leg, any number of named services, or both, and traffic matching any
of them lands in `Class`. A rule with none of those set still matches
everything, unchanged from before `Services` existed — so every pre-existing
QoS rule keeps working without modification.

Threading a named reference through meant widening the match key `QoSAdd`/
`QoSDelete`/`QoSRuleSetEnabled` use to find "the same rule" again: it used to
be `(proto, port)`; a services-only rule has neither, so two such rules would
otherwise collide on `("", 0)`. All three now take a `services []string`
alongside proto/port, compared order- and case-insensitively
(`sameServiceSet`) so a round trip through the UI's comma-separated field
still finds the rule it means to.

Resolution against the catalog happens where the classifier is actually
built (`qosClassRules`, called from `fillRuntimeSpec` on every reload — QoS
has no live incremental engine object the way firewall does, so there's
nothing to recompile incrementally; reload already rebuilds it from scratch
every time), not at config-save time — same reason firewall defers its own
resolution to `compileRule`: a service can be renamed or have its ports
edited after a rule references it, and the rule should pick up the change
without needing to be re-saved itself. A rule naming an unknown service
skips just that reference (logged) rather than falling back to matching
everything, which would turn a typo or a since-deleted service into a
silent catch-all — that fail-closed shape mirrors firewall's own compile
error for an unknown service, just non-fatal here since a reload can't
reject one bad rule out of a whole config the way a single API call can.

**What changed:** `QoSRule.Services` added (`internal/config/config.go`).
`QoSAdd`/`QoSDelete`/`QoSRuleSetEnabled` take a `services []string` param
(`internal/config/ops.go`), matched via new `qosRuleKeyMatches`/
`sameServiceSet` helpers. `qosClassRules` (`cmd/gravinet/main.go`) now takes
the firewall service catalog and a network name (for the log line), resolves
`Services` into one `mesh.ClassRule` leg per resolved service port (new
`qosServiceCatalog`/`qosLeg`, mirroring mesh's own unexported `svcLegs`),
and unions them with the literal leg. `protoNumber` extended to accept a raw
numeric protocol, matching what `FirewallServicePort.Proto` already
documented. `fillRuntimeSpec` gained a `fwServices` parameter, passed from
both call sites' already-in-scope `Config.FirewallServices`. CLI: `gravinet
qos add|delete|enable-rule|disable-rule` accept `service NAME[,NAME2,...]`
as an alternative to `PROTO PORT` (new `parseQoSMatch`/`qosRuleMatchLabel`
helpers, `cmd/gravinet/cli_config.go`). Webadmin: `/api/qos` request struct
gained `Services`, threaded through every op. UI: the QoS table's separate
`protocol`/`port` columns became one `match` column showing the same
combined proto/port-plus-services text firewall rules show, edited with the
same combined field, service-catalog combobox, and parser (`fwParseSvc`/
`fwCatalogCombobox`, reused verbatim rather than reimplemented) firewall
rules already use; `qosProtoOpts`/`qosValidatePort` (the old two-widget
editor) removed as dead code. Global search's QoS index entry and
jump-to-row selector now include services.

**Verified:** `go build ./...` and `go vet ./...` clean. `go test
./internal/config/... ./cmd/gravinet/... ./internal/webadmin/...` all pass,
including new coverage: config-layer add/delete/enable-disable by service
set (order/case-insensitive keying, `TestQoSRuleServices`), classifier
resolution (service-alone expansion to multiple legs, service-plus-literal
union, unknown-service skip without a catch-all fallback, and the
no-match-fields catch-all still working, `TestQoSClassRulesServices`/
`TestQoSClassRulesCatchAll`), and the webadmin endpoint end-to-end
(`TestHandleQoSServices`). Extracted the embedded UI script and ran `node
--check` clean; confirmed the raw-string backtick count is still exactly 2.

---

## v448 — 2026-07-15

Fixed sidebar sub-menu alignment: when a `.rail-group` (a collapsible
section under the chevron toggle, e.g. Settings) is expanded, its child
`.rail-tab` items sat too close to the group label's left edge and looked
misaligned against the chevron/label above them. Bumped
`.rail-group .rail-tab`'s `padding-left` from `30px` to `34px` in
`internal/webadmin/ui.go` so the sub-items sit 4px further right.

**What changed:** one CSS rule in the embedded `indexHTML` stylesheet
(`internal/webadmin/ui.go`) — `.rail-group .rail-tab { padding-left:30px; }`
→ `padding-left:34px;`. No JS or markup changes; the chevron rotation and
collapse/expand behavior are untouched.

---

## v447 — 2026-07-15

Moved the NAT table's state column from last to first (right after the
checkbox column, before source), matching the Rules table's column
order from v443. `startNATEdit`/`natAddRow` select cells by class, not
position, so reordering the `<td>`s in `secNAT` was the only strictly
required change — but that alone would have put the add/edit save-cancel
buttons (which the state cell used to host, being last) in the *first*
column instead, which would have been inconsistent with the Rules
table's own convention: there, the state cell is a pure indicator/toggle
that inline editing never touches, and save/cancel always live in the
last field cell. Matched that exactly rather than leaving NAT's edit
flow with mismatched button placement between add and edit: `natAddRow`
now shows a static "enabled" span in the (now first) state cell and
save/cancel in the translate cell; `startNATEdit` no longer touches
`.nat-state` at all — the enabled/disabled tag stays untouched with its
own separate double-click toggle during inline editing, same as the
Rules table already does for `.fw-state`.

**What changed:** `secNAT`'s header and row-building strings reordered
(state second, before source). `natAddRow` rebuilt: static "enabled"
indicator in the state cell, save/cancel moved into the translate cell.
`startNATEdit` rebuilt: dropped its `stCell` lookup and the innerHTML
swap that used to turn the state cell into save/cancel buttons; both
buttons now render as part of the translate cell's edit markup instead.

**Verified:** `go build ./...` (JS lives in a Go raw string; the package
still needs to compile). Embedded-script `node --check` and the
backtick-count sanity check (v433, exactly 2) both clean. Parsed the
rendered header and a sample row through jsdom and confirmed the header
reads `state, source, dest, translate` and the row's cell classes land
in `nat-state, nat-src-cell, nat-dst-cell, nat-tr-cell` order — actually
checked the DOM output rather than just eyeballing the template strings.

---

## v446 — 2026-07-15

Removed NAT's `direction` field per feedback questioning why it existed
at all alongside `source`/`dest`. Checked before touching anything: the
engine only ever read `direction` to pick SNAT vs DNAT
(`internal/mesh/nat.go`), and of its three values —
`overlay2underlay`/`underlay2overlay`/`overlay2overlay` — two
(`overlay2underlay` and `overlay2overlay`) produced the exact same SNAT
behavior. The field meant to distinguish them, `DestNetwork` ("used for
overlay-to-overlay"), was declared on the config struct and never read
anywhere — not passed into the mesh spec, not touched by the NAT engine.
Picking `overlay2overlay` in the UI did something other than what its
name implied.

Per follow-up direction (find the real bit of information `direction`
carried — SNAT vs DNAT, i.e. which side of a packet gets rewritten — and
fold it into `translate`, which already has to name the rewrite target
regardless): `translate` now recognizes three forms instead of two —
`masquerade` (or blank, with an interface) for SNAT via that interface's
address, a literal IPv4 for a fixed SNAT target, and new
`port-forward:<ipv4>` for DNAT to that address. There's no longer a
separate field or column for the mode; naming the target *is* choosing
the mode, the same way `masquerade` already doubled as both a value and
a mode before this.

One thing this surfaced that wasn't obvious from the userspace engine
alone: `cmd/gravinet/main.go`'s `kernelNATRules` — which derives the
*host-kernel* netfilter rules, separate from the userspace overlay NAT
path — used `direction` too, and specifically skipped installing a
kernel rule for `overlay2overlay` (traffic assumed to never cross a
physical interface, so kernel-level conntrack NAT doesn't apply).
Dropping the concept meant deciding what happens to that skip. Reasoned
through it rather than guessing: a masquerade/SNAT kernel rule is scoped
to a specific `OutIface`, so installing one for traffic that in fact
never egresses that interface is inert — it simply never matches
anything, not a correctness bug. So every masquerade/SNAT-style rule now
always gets a kernel-side attempt, and the old skip (along with the
distinction that motivated it) is gone. Wrote `TestKernelNATRulesModes`
to lock this reasoning in as an explicit, tested behavior rather than an
unstated side effect of the refactor — this function had no test
coverage at all before this change.

Existing configs with the old field aren't silently broken: `Direction`
stays on `NATRule` (deprecated, `omitempty`) purely so old JSON still
parses, and `Config.Validate` migrates it on load — an
`"underlay2overlay"` rule gets `port-forward:` prefixed onto its
`Translate` value so it keeps meaning DNAT, `"overlay2underlay"`/
`"overlay2overlay"` just drop the field (both already meant plain SNAT),
and `Direction` is cleared afterward either way so it's never written
back out. The one edge case with no clean equivalent — an
`underlay2overlay` rule left as `masquerade`/blank, meaning "DNAT to
whatever address the interface itself resolves to" — falls back to
plain masquerade/SNAT rather than guessing at an address; this
combination has no evidence of ever being used and no sensible default
target to invent for it.

**What changed:** `internal/mesh/nat.go` — `NATRuleSpec` lost `Direction`;
`toRule()` detects a `port-forward:` prefix on `Translate` (new
`cutPrefixFold` helper, case-insensitive) to pick `dnatAction` instead of
reading a separate field. `internal/config/config.go` — `NATRule` lost
`DestNetwork` outright and kept `Direction` only as deprecated/
`omitempty`; `NATDirection` and its three constants are gone entirely;
new migration block in `Validate` (next to the existing NAT
`StateTimeout` migration, same established pattern). `internal/config/
ops.go` — `buildNATRule`/`NATRuleAdd`/`NATRuleUpdateAt` lost their
`direction` parameter; `buildNATRule` now parses the `port-forward:`
prefix itself (new `natPortForwardPrefix` const, `cutNATPortForwardPrefix`
helper) and requires a valid IPv4 target after it. `internal/webadmin/
edit.go` — the `/api/nat` handler's request struct lost `Direction`.
`cmd/gravinet/main.go` — the NAT spec conversion lost `Direction`;
`kernelNATRules` rewritten per the reasoning above (own copy of the
prefix constant, since `cmd/gravinet` doesn't import `internal/config`'s
or `internal/mesh`'s private helpers for one shared keyword).
`cmd/gravinet/cli_config.go` — `nat add`'s `direction` keyword arg and
usage string are gone; `nat list`'s direction column is gone.
`internal/webadmin/ui.go` — the NAT table lost its direction column;
`natAddRow`/`startNATEdit` lost the direction `<select>`, translate
inputs widened slightly and gained a title tooltip listing the three
accepted forms; the global search index's NAT-rule label no longer
includes direction; hint text rewritten to describe `translate`'s three
forms instead of two.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean.
`go test ./internal/config/... ./internal/webadmin/... ./cmd/gravinet/...`
clean, plus the mesh package's `NAT`/`Reload`/`AddNetwork`/`LiveNet`
groups (16 tests). New/updated coverage: `TestNATRuleDirectionMigration`
(config) drives `Validate` over five rules spanning all three legacy
direction values, mixed casing, and the DNAT-to-self fallback case,
asserting both the resulting `Translate` values and that `Direction` is
cleared on every one. `TestNATRulePortForwardPrefixCaseInsensitive` and
an expanded `TestNATRuleAddRejectsBadInput` (missing target, non-IP
target, IPv6 target all rejected) cover `buildNATRule` directly.
`TestNATRuleSpecToRuleDetectsMode` (mesh, new, table-driven) is
`toRule()`'s first direct unit test — SNAT vs DNAT, case-insensitivity,
whitespace handling, and each rejection case, isolated from the
end-to-end engine tests that exercised this only indirectly before.
`TestKernelNATRulesModes` (cmd/gravinet, new — this function had zero
prior test coverage) covers all three `Translate` forms producing the
right `netfilter.Rule` `Kind`, a malformed port-forward target being
skipped rather than failing the whole pass (matching the old code's
behavior for a malformed DNAT), every disabled/excluded case, and
explicitly pins the "no more overlay2overlay carve-out" behavior as
intentional. `nat_rule_handler_test.go`/`nat_update_handler_test.go`
(webadmin, existing, updated) confirm the HTTP layer end-to-end,
including editing a rule from masquerade to `port-forward:` through the
same API path the UI uses. Embedded-script `node --check` and the
backtick-count sanity check (v433, exactly 2) both clean.

---

## v445 — 2026-07-15

Shortened the Info \u2192 Upgrade source-upload hint per feedback — v444's
spelled out both accepted formats and namechecked GitHub inline; that's
what the file picker's accept list and the error messages are for, not
the one-line hint above it.

**What changed:** ui.go's source-upload hint text is now "Upload a
source archive and it\u2019s built and applied automatically." — down from
naming .tgz/.tar.gz/.zip and GitHub's Download ZIP explicitly. Nothing
else from v444 changed: the file picker still accepts .tgz/.tar.gz/.zip,
`extractSourceArchive` still sniffs the format server-side, the
validation alert still names both extensions.

**Verified:** `go build ./...`; embedded-script `node --check` and the
backtick-count sanity check (v433, exactly 2) both clean.

---

## v444 — 2026-07-15

GitHub's "Download ZIP" button — the most likely way to get this
project's source onto a box without a git client already on it —
produces a .zip, not a .tgz. Before this, Info \u2192 Upgrade's source
upload only understood gzip-compressed tar, so that download couldn't be
used as an upgrade source at all: `extractSourceTarGz` would fail on the
very first `gzip.NewReader` call with a "not a valid gzip-compressed tar
archive" error that had nothing to do with what was actually wrong.

New `extractSourceArchive` sits in front of extraction and sniffs the
uploaded body's format from its content — 0x1f 0x8b for gzip, "PK" plus
a third byte of 0x03/0x05/0x07 for zip's few defined first-record types
— since this handler's caller posts a raw request body with no filename
or Content-Type attached at all, so there's nothing else to go on
(consistent with how tgz-vs-extension was already decided here before
this change). Both formats need the body spooled to a temp file first:
zip's central directory lives at the end of the stream and needs
`io.ReaderAt` plus a known length to parse, which a raw HTTP body
reader can't offer, and detecting the format in the first place means
reading it before committing to either parser anyway — so one temp
file, one read, then whichever extractor applies. New `extractSourceZip`
is `extractSourceTarGz`'s zip-format counterpart, same contract and the
same safety checks ported over entry-by-entry: path-escape rejection
(`../` and absolute-path entries refused, resolved path re-checked
against the destination boundary), symlink rejection (checked via the
Unix mode bits zip's external file attributes carry, the same
S_IFLNK Info-ZIP and every compatible tool round-trip a symlink
through), and per-entry plus cumulative extracted-size enforcement by
counting bytes actually written rather than trusting the (attacker-
controlled, unverified) header value — none of that is new reasoning,
it's the existing tar-path reasoning applied to zip's equivalent
hazards.

Also now accepted: the Info \u2192 Upgrade page's file picker
(`accept=".tgz,.tar.gz,.zip,..."`) and its hint text, and the
error message pointing at `/api/upgrade/stage-source` from the
signed-manifest upload path when no manifest was supplied.

**What changed:** `extractSourceArchive` (spools to a temp file, sniffs,
dispatches) and `extractSourceZip` (upgrade_source.go) are new;
`stageFromSource` calls `extractSourceArchive` instead of
`extractSourceTarGz` directly. `maxSourceUploadSize`'s doc comment,
`handleUpgradeStageSource`'s doc comment, and the "upload a manifest
first" error message (upgrade.go) all updated to describe both formats
instead of just tgz. ui.go's source-upload `<input>` accept list, hint
text, and validation alert now mention .zip; the stale "no format
choice, no sniffing needed" comment there is gone, since sniffing is
exactly what happens now, just server-side.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean;
`go test ./internal/webadmin/...` clean (82s). New tests:
`TestExtractSourceZipRejectsPathTraversal`/`RejectsSymlink`/
`HappyPath` mirror the existing tar-format tests entry-for-entry against
`extractSourceZip` directly. `TestExtractSourceArchiveDetectsFormatByContent`
drives the actual dispatcher — confirms a tgz body routes to the tar
extractor, a zip body routes to the zip extractor, a body that's neither
gets a clear rejection instead of a confusing one from whichever parser
happened to run first, and a body too short to contain either signature
is rejected before either parser sees it. `TestStageFromSourceAcceptsZip`
is the end-to-end proof: packages this repository's own actual current
source as a .zip (new `zipOfDir` helper, mirroring the existing
`tarGzOfDir`'s file selection) and runs it through the real
`stageFromSource` pipeline — extract, `go build`, probe, ingest — the
same path `handleUpgradeStageSource` calls, not a synthetic shortcut;
passed in 28s alongside the pre-existing tgz equivalent
(`TestStageFromSourceBuildsWithGoOffPATH`, 27s) run in the same pass.

---

## v443 — 2026-07-15

Three requested changes to the Rules tab, all UI-only (no engine/config
changes this time):

**Column order.** The rules table now reads state, source, destination,
services, action, log, hits, notes — action moved from 2nd to 5th
column, right before log. The `src`/`dst` header labels are spelled out
as "source"/"destination" now too. Purely a rendering-order change:
`startFwEdit` finds each cell by class (`.fw-action`, `.fw-src`, etc.),
not position, so reordering the `<td>`s in the table-building loop was
enough — nothing downstream needed to change to match.

**Filter dropdown on source/destination/services.** These three fields
used to autocomplete via a plain HTML `<datalist>`, whose filtering
behavior is inconsistent across browsers (some only prefix-match, some
don't filter at all as you type). New `fwCatalogCombobox(input,
getNames)` replaces that with a real filterable dropdown — reusing the
`.ss-list`/`.ss-opt` styling `buildListPicker` already established
elsewhere in this UI rather than inventing new dropdown chrome. Typing
narrows the list by case-insensitive substring match against this node's
object/service catalog (`state.fwObjects`/`state.fwServices`, re-read
fresh on every open so an edit made on the Objects/Services tab shows up
without needing the row re-rendered); click or Enter picks one; Escape or
clicking away closes it. Unlike `buildListPicker` this stays a genuine
free-typing text input the whole time — a literal CIDR or `proto/port`
isn't in any catalog and is still a perfectly valid value for these
fields, so there's no separate filter row or button standing in for the
input the way `buildListPicker`'s trigger-button-plus-list does; the
input itself is both the value and the filter. Dropped the per-tab
`<datalist>` element pair (`objNames`/`svcNames` building in
`secFirewall`) entirely along with the `list=` attributes and
`table._dlObj`/`_dlSvc` plumbing that wired rows to them — no longer
needed now that `fwCatalogCombobox` is wired directly off `state`
instead.

**Wider fields.** `fwe-src`/`fwe-dst` 110px → 150px, `fwe-services` 140px
→ 180px, `fwe-notes` 100px → 140px, in both `fwAddRow` and `startFwEdit`
(they'd drifted out of sync before; both are 150/150/180/140 now).

**What changed:** `secFirewall`'s table-header and row-building strings
reordered; hint text reworded for the new source/destination labels and
to describe the filter dropdown instead of a "well-known object" note
that had drifted out of date anyway. New `fwCatalogCombobox`. `fwAddRow`
rebuilt with the new column order, widths, and combobox wiring in place
of `list=`; `startFwEdit` likewise, and lost its now-pointless
`table`/`dlObj`/`dlSvc` lookups (`table` wasn't used for anything else).

**Verified:** `go build ./...` (JS lives in a Go raw string; the package
still needs to compile), `go vet ./...`, `gofmt -l` clean;
`go test ./internal/webadmin/...` clean, including the firewall-specific
tests, which don't touch rendering but confirm the API contracts these
handlers still exercise are unaffected. Embedded-script `node --check`
and the backtick-count sanity check (v433, exactly 2) both clean.
Installed jsdom and drove `fwCatalogCombobox` against a real DOM rather
than reasoning about it statically: focus opens the list with every
name; typing "goog" narrows to the two matching entries by substring
(not just prefix); a query matching nothing hides the list without
erroring; a literal CIDR typed into the field is left alone (no match,
no crash) exactly as a raw value should be; clicking a filtered option
sets the input's value and closes the list. Visually confirmed the
rendered header string reads `state, source, destination, services,
action, log, hits, notes` and that each data row's `<td>` sequence
matches it exactly.

---

## v442 — 2026-07-15

Two fixes per direct feedback on v441, and they turned out to be the same
underlying problem. First: "why once per network? objects and services
are shared between networks" — they weren't. `Firewall.Objects`/
`Services` lived on each `Network` in config, so a node running three
networks kept three independent copies of what was supposed to be one
reusable catalog; editing "google.com" on one network never touched the
other two. Second, a direct consequence of the fix for the first: once
the catalog is genuinely one node-global list, "populate once" needs a
real persisted marker, not a per-network guess — v441's auto-populate had
no seeded flag at all, so it re-added anything missing by name on every
single render, silently resurrecting a deliberately deleted well-known
entry the next time anyone opened a firewall tab.

**The catalog is node-global now.** `Config.FirewallObjects`/
`FirewallServices` replace the old per-`Network` `Firewall.Objects`/
`Services` fields — one catalog, shared by every network on this node,
edited once and usable everywhere. In the engine, `Engine.fwObjects`/
`fwServices` (guarded by `fwCatalogMu`) are the live equivalent:
`SetFirewallObjects`/`SetFirewallServices` (now `(objs)`/`(svcs)`, no
`networkID`) push the same list into every currently-running network's
`*firewall` instance in one call, and `newNetState` seeds a
newly-built network (at boot via `Options.FirewallObjects`/
`FirewallServices`, or later via `AddNetwork`) from that same shared
state — so a network created after the catalog was last edited starts
with the real thing, not empty. `NetSpec.FirewallObjects`/`Services` are
gone; a per-network spec has no business carrying node-global data.
`/api/firewall`'s `objects`/`services` ops (and the new
`mark-objects-seeded`/`mark-services-seeded`, below) dropped their `net`
requirement to match — they're handled before net resolution now, the
same tier as `enable`/`disable`. `/api/config`'s response moved
`firewall_objects`/`firewall_services` to the top level, sitting next to
`nets` rather than nested inside any one of them; the client's `state.cfg`
lost `cf.firewall.objects`/`services` in favor of top-level
`state.fwObjects`/`state.fwServices`. The Objects/Services tabs
(`secFwObjects`/`secFwServices`) went from one table per network to one
table, period — and, since a catalog entry no longer belongs to any
particular network, they're usable (and, per the point below, get
auto-populated) even with zero networks configured yet, matching how the
Allow List tab has always worked.

**Populate-once is now a real once, backed by disk.** New
`Config.ObjectsCatalogSeeded`/`ServicesCatalogSeeded` (node-global, next
to `FirewallObjects`/`Services`) record that the well-known catalog has
already been populated; new config methods
`FirewallMarkObjectsCatalogSeeded`/`FirewallMarkServicesCatalogSeeded` set
them, exposed as the `mark-objects-seeded`/`mark-services-seeded`
ops — plain `mutateConfig` mutations, same tier as `enable`/`disable`,
sequenced by the client to run right after an `objects`/`services` save
that filled any gaps (both take the same per-config-path lock as the
engine's persist hook, so the flag is never written ahead of the catalog
it's describing). `fwAutoPopulateCatalog` now checks
`state.fwObjectsSeeded`/`fwServicesSeeded` before doing anything: seeded
already → it does *nothing at all*, not even the "missing" diff v441 ran
on every render. Not seeded yet → fill any gaps, then mark seeded, once,
ever, for this node. A well-known entry deleted after that point simply
stays deleted — there's no more per-render "missing" check to resurrect
it, because the whole point of the seeded flag is that nothing runs that
check again.

**What changed (Go):** `Config.Firewall` lost `Objects`/`Services`;
`Config` gained `FirewallObjects`/`FirewallServices`/
`ObjectsCatalogSeeded`/`ServicesCatalogSeeded`. `validateFirewallCatalog`
takes `(objects, services)` directly instead of a per-network `Firewall`,
called once in `Config.Validate` instead of once per network.
`FirewallMarkObjectsCatalogSeeded`/`FirewallMarkServicesCatalogSeeded`
(ops.go) dropped their `netName` parameter. `mesh.Options` gained
`FirewallObjects`/`FirewallServices` (seeds the engine's global catalog
before any initial network is built); `mesh.NetSpec` lost the equivalent
per-network fields. `Engine.SetFirewallObjects`/`FirewallObjectsList`/
`SetFirewallServices`/`FirewallServicesList` are global now (no
`networkID`); new `Engine.firewallCatalogSnapshot()` backs both
`newNetState` (network construction) and `ReloadRuntime` (runtime
reload, reload.go). `webadmin.Backend`'s four matching methods updated
to suit. `handleFirewall` (webadmin.go) handles `objects`/`services`/
`mark-objects-seeded`/`mark-services-seeded` before net resolution;
`handleConfig` moved the catalog + seeded flags to the top-level JSON
response. `cmd/gravinet/main.go`: the persist hook now syncs the global
catalog once per call (unconditionally, before the per-network loop)
instead of once per matched network; `fillRuntimeSpec` dropped the
per-network object/service population it used to do; new
`toMeshFirewallObjects`/`toMeshFirewallServices` convert config's global
catalog to the engine's `Options` shape at startup.

**What changed (JS):** `refresh()` (ui.go) reads the new top-level
`firewall_objects`/`firewall_services`/`firewall_objects_seeded`/
`firewall_services_seeded` into `state.fwObjects`/`fwServices`/
`fwObjectsSeeded`/`fwServicesSeeded`. `fwAutoPopulateCatalog` reworked
around those two seeded flags instead of a per-network "missing" scan.
`secFwObjects`/`secFwServices` render one table from `state.fwObjects`/
`fwServices`, no per-network loop, no `net` argument threaded through
`objSave`/`svcSave`/`objAddRow`/`svcAddRow` anymore. `secFirewall`'s Rules
tab builds one shared pair of datalists (fixed ids `FW_DL_OBJ`/`FW_DL_SVC`)
instead of one pair per network.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean.
`go test ./internal/config/... ./internal/webadmin/... ./cmd/gravinet/...`
clean, including `firewall_catalog_test.go` rewritten for the
`Config`-level catalog shape and a new `TestFirewallCatalogGlobalOpsNotNetScoped`
that posts `objects`/`services`/`mark-*-seeded` with no `net` field at
all against a two-network config, then reloads the config file from disk
(not just in-memory state) to confirm both seeded flags actually
persisted and neither network's own `Firewall.Rules` was touched. In
`internal/mesh`, ran the full `Firewall`/`Reload`/`AddNetwork`/`LiveNet`
test groups clean, plus two new tests:
`TestFirewallCatalogSharedAcrossNetworks` (one `SetFirewallObjects`/
`SetFirewallServices` call, two networks, a rule on each referencing the
same object and service by name — both compile and both actually deny
the same traffic, not just one of them) and
`TestFirewallCatalogSeedsNetworkAddedAfterSet` (a network built via
`newNetState` *after* `SetFirewallObjects` already ran still resolves
the shared object, proving the catalog reaches networks that didn't
exist yet when it was last set, not just ones that did). Embedded-script
`node --check` and the backtick-count sanity check (v433, exactly 2)
both clean. Extracted the rewritten `fwAutoPopulateCatalog` into an
isolated Node harness again: a first pass (unseeded) populates both
lists and marks both seeded in one batch, plus one `refresh()`; a second
pass makes zero calls; and — the actual bug — deleting an entry *after*
seeding and running another pass makes zero calls and the deleted entry
stays gone.

---

## v441 — 2026-07-15

Dropped v440's "add all" button too, per direct follow-up feedback: a
button is still a click, and the ask was zero clicks. Every configured
network's real object and service catalog is now kept fully stocked with
the entire well-known set automatically, no interaction required at all.

`fwAutoPopulateCatalog` runs at the top of `secFirewall` — so on any visit
to Rules, Objects, Services, or Allow List — and diffs each network's real
catalog against `FW_COMMON_WILDCARD_OBJECTS`/`FW_COMMON_SERVICES`. Any
network missing entries gets them saved in via the same `op:objects`/
`op:services` calls the removed button used to make, then one `refresh()`
at the end. A `fwAutoAddBusy` guard stops overlapping runs if the section
re-renders again before a prior pass finishes (e.g. clicking between tabs
quickly); once a network is fully stocked, a pass is just the "missing"
diff against two in-memory arrays with no network calls, so running it on
every render costs nothing once it's caught up. Removed the buttons from
`secFwObjects`/`secFwServices` and their hint text along with them —
Objects/Services now just show real rows, always complete, nothing to
click.

**Worth knowing, stated once and not re-litigated:** this only fills
gaps by name — an entry already present (including one edited under its
original name) is left alone, never overwritten. It does mean that
deleting a well-known object/service without also excluding it some other
way won't stick across a fresh page load; the next visit to a firewall
tab re-adds anything missing by name, same as a first visit would. That
follows directly from "no action required" — a catalog entry only
matters once a rule references it, so this trades "deletions of unused
catalog entries persist" for "nothing to ever set up." The gossip-cost
reasoning `FW_COMMON_WILDCARD_OBJECTS`/`FW_COMMON_SERVICES`'s doc
comments describe is why those lists stay curated at a few dozen to a
hundred-odd entries rather than parapet's much larger Tranco-derived
one — that's what keeps "every network, always fully populated" affordable
rather than turning every config sync into a large gossip payload.

**What changed:** new `fwAutoPopulateCatalog` (async, guarded by
`fwAutoAddBusy`) and its call in `secFirewall`. `secFwObjects`/
`secFwServices` lost their `missing`-driven button and reverted to a
plain real-rows-only render (empty-state text back to "click + to add
one," dropping v440's "or use add all above"). Updated the Rules hint,
the datalist-building comment, and the `FW_COMMON_WILDCARD_OBJECTS`/
`FW_COMMON_SERVICES` doc comments to describe automatic, unconditional
population instead of v434–440's various opt-in mechanisms.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean;
`go test ./internal/webadmin/... ./cmd/gravinet/...` clean. Embedded-script
`node --check` and the backtick-count sanity check (v433, exactly 2) both
clean. Extracted `fwAutoPopulateCatalog` and its dependencies into an
isolated Node harness with a mocked `api`/`refresh`/`state` and confirmed
by direct execution: a first pass populates every network missing
entries in one batch of calls plus a single `refresh()`; a second pass
immediately after makes zero calls (no infinite loop, no thrash); a
well-known entry present under its own name is never duplicated.

---

## v440 — 2026-07-15

Reverted v439's approach per direct feedback: materializing a well-known
object/service into the real catalog only when a rule happened to
reference it was still a lazy, indirect trick — an object wasn't real
until something else caused it to become real, whichever mechanism did
that. The ask was blunter than that: they should just already be real.

Deleted the lazy stuff (`fwEnsureRuleCatalog`, `fwMaterializeObjs`,
`fwMaterializeSvcs`, `fwObjCatalogDef`, `fwSvcCatalogDef`, and their call
sites in `fwAddRow`/`startFwEdit`) along with v434–439's whole
suggestion-row apparatus (`fwSuggestRows`, the dimmed/extra
`data-def-idx` rows, the per-cell `promote()` closures). In its place:
each network's Objects and Services tables are back to showing only real,
saved entries — nothing conjured, nothing pretending — plus one plain
button above each table, "add all N well-known objects/services", that
does a single `objSave`/`svcSave` call appending every
`FW_COMMON_WILDCARD_OBJECTS`/`FW_COMMON_SERVICES` entry the network
doesn't have yet. Click it once and every one of them is a real,
persisted, editable, deletable row — same as anything typed in by hand —
and from that point on Rules' src/dst/services autocomplete (back to
listing only the real catalog, dropping v439's union-with-catalog
datalist trick too) has all of them because they're actually there.
Editing one first (renaming it, adjusting its addresses/ports/notes)
still works the same as always via double-click on its row, for anyone
who wants to customize before adding rather than after.

The gossip-cost trade-off `FW_COMMON_WILDCARD_OBJECTS`/
`FW_COMMON_SERVICES`'s doc comments describe (why these aren't baked into
every fresh network's config automatically) hasn't changed — a network
that never clicks "add all" still pays nothing extra. What changed is
that reaching "fully populated, all real" now takes one click instead of
~190 double-clicks or a scattering of coincidental rule edits.

**What changed:** `secFirewall`'s datalists reverted to real-catalog-only.
`secFwObjects`/`secFwServices` each gained a `missing` computation (this
network's real names vs. the full built-in catalog) and, when non-empty,
a button rendered above the table that appends the missing defs in their
default (unedited) shape and calls `objSave`/`svcSave` once. Both
functions' suggestion-row rendering, wiring, and `fwSuggestRows` itself
are gone.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean;
`go test ./internal/webadmin/... ./cmd/gravinet/...` clean. Embedded-script
`node --check` and the backtick-count sanity check (v433, exactly 2) both
clean. Grepped for every removed identifier
(`fwEnsureRuleCatalog`/`fwMaterializeObjs`/`fwMaterializeSvcs`/
`fwObjCatalogDef`/`fwSvcCatalogDef`/`fwSuggestRows`/`data-def-idx`/
`promote(`) to confirm no dangling references survived the cut.

---

## v439 — 2026-07-15

Fixed the actual complaint underneath v434–438's run at the Objects/
Services suggestion rows: none of that touched the Rules tab, so an
object or service that hadn't been double-clicked into existence yet —
whether it was one of your own or one of the ~150/~40 well-known catalog
defs — simply didn't appear in the src/dst/services autocomplete there.
From the Rules tab, using one still meant a detour to Objects or Services
first to "promote" it, sight unseen, before it would even show up as an
option. Per feedback: no promoting — an object or service should just be
one that can be used in a rule, full stop.

Rules' src/dst/services datalists now list every well-known catalog def
alongside the network's real (saved) objects/services, not just the real
ones — so they show up to autocomplete against whether or not they've
ever been touched in the Objects/Services tabs. And saving a rule that
names one is now sufficient to add it for real: `fwEnsureRuleCatalog`
runs before the rule itself is posted, diffs the rule's src/dst/services
against the network's actual catalog, and — for any referenced name that
matches a well-known def but isn't saved yet — pushes it into the real
object/service list first. The double-click-to-add gesture in the
Objects/Services tabs (v434–438) still works and is still there for
renaming or tweaking a def's addresses/ports/notes before using it, but
it's no longer a prerequisite: naming a def in a rule adds it unedited,
the same way, from wherever the rule is written. A name that's neither
already saved nor a recognized well-known def (a typo, a bare CIDR, a
made-up service name) is left alone by this and still fails at the server
with the same "not a known object" / "unknown service" error as before —
this only removes the friction for names that were always going to
resolve, not the validation for ones that wouldn't.

**What changed:** `secFirewall`'s per-network datalist build now unions
`cf.firewall.objects`/`services` names with `FW_COMMON_WILDCARD_OBJECTS`/
`FW_COMMON_SERVICES` names not already present, instead of listing only
the real catalog. New `fwObjCatalogDef`/`fwSvcCatalogDef` (name lookup)
and `fwMaterializeObjs`/`fwMaterializeSvcs` (diff a rule's referenced
names against the real catalog, return an updated list or `null` if
nothing's missing) back a new `fwEnsureRuleCatalog(net, rule)`, called
from both `fwAddRow`'s and `startFwEdit`'s save handlers right after
`fwValidateNegate` and before the rule POST — it does the `op:objects`/
`op:services` saves first (bailing with the same alert-on-failure pattern
as everything else here) if the rule needs them, then lets the caller's
existing rule-save proceed. Reworded the Rules hint and the Objects/
Services suggestion-row hints to describe the new behavior instead of the
old "double-click to add" as the only way in.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean;
`go test ./internal/webadmin/...` clean, including
`TestFirewallObjectsServicesCounters` and both firewall live-apply tests.
Embedded-script `node --check` and the backtick-count sanity check (v433,
exactly 2) both clean.

---

## v438 — 2026-07-15

Made Objects/Services suggestion rows fully identical to real rows, per
feedback that there should be no difference at all — the catalog isn't
static, it should be completely user-serviceable. Dropped the dedicated
`fw-suggest-check` class from v437: the checkbox is now a plain `selbox`,
indistinguishable from — and included in — `select all` and the real
rows' bulk-remove (ticking one alone still does nothing, same as ticking
a real row; clicking `\u2212` on a ticked suggestion row is a harmless
no-op, since it was never in the saved list the removal filter operates
on). Double-click any cell to edit it — same gesture, same title text,
same `inlineCellEdit` call as any real row — and editing it (even
committing the same value back unchanged) is what adds it: a `promote()`
closure builds a fresh entry from the catalog def with the one edited
field applied, appends it to the saved list, and saves. No dedicated CSS
left at all (dropped the hover-only rule too, since real rows don't get
one either — a difference of its own, once pointed out).

**What changed:** `fwSuggestRows` dropped the `fw-suggest` class and
`title` attribute, keeping only `data-def-idx` to mark a row and a
`selbox` checkbox matching real rows exactly. `secFwObjects`/
`secFwServices`'s `renderCells` callbacks now emit the same field classes
(`ob-name`/`ob-kind`/`ob-val`/`ob-notes`, `sv-name`/`sv-ports`/
`sv-notes`) real rows use, and each function gained a second wiring block
— `tr[data-def-idx]` instead of `tr[data-idx]` — that's structurally the
same double-click-editor setup as the real-row block just above it, only
building-and-appending instead of mutating-in-place. Both hints reworded
from "ticking adds it right away, unlike your own rows" to "editable
exactly like your own — double-click any cell to add it," since there's
no longer a special case to warn about.

**Verified:** `fwSuggestRows`'s output checked directly — checkbox is
`class="selbox"`, no `fw-suggest` class anywhere in the markup,
already-added filtering and `data-def-idx` still correct. `go build
./...`, `go vet ./...`, `gofmt -l` clean; embedded-script `node --check`
and the backtick-count sanity check (v433) both clean.

---

## v437 — 2026-07-15

Replaced v434's double-click-to-add on Objects/Services suggestion rows
with an ordinary checkbox in selcol — per feedback that needing to
double-click before a row got a tickbox was weird. Ticking it now adds
the entry immediately (`onchange`, guarded to fire only when checked,
not on an uncheck). It's a distinct class, `fw-suggest-check`, not
`selbox` — deliberately, so `select all` and the real rows' bulk-remove
never sweep these up too; accidentally wiring that shared would mean one
click on the header checkbox mass-adds every one of the ~150 suggestion
entries in the table at once. Confirmed `selAllWire`/`selCheckedRows`
both query `.selbox` specifically, unaffected.

**Same gesture, opposite meaning — called out explicitly:** ticking a
real row's checkbox stages it for removal via the toolbar's `−` button
(nothing happens until that's clicked); ticking a suggestion row's
checkbox adds it right away, no second click needed. Both hints now spell
out that distinction rather than just saying "tick to add," since the
same tick meaning two different things depending on which kind of row
it's on is exactly the kind of thing worth being explicit about.

**Verified:** `go build ./...`, `go vet ./...`, `gofmt -l` clean;
embedded-script `node --check` and the backtick-count sanity check (v433)
clean. Manually traced `selAllWire`/`selCheckedRows` against the new
class name rather than assuming the separation held.

---

## v436 — 2026-07-15

Dropped the literal bare-domain entry v435 kept in every
`FW_COMMON_WILDCARD_OBJECTS` entry — `wcoDef` now seeds only
`*.google.com`, not `google.com, *.google.com` — per explicit request.
The freshness trade-off v435 accepted the literal *for* (proactive
periodic resolution of the bare domain vs. passive-only wildcard
coverage) wasn't the deciding factor here; the reason given this time is
that these ~150 entries double as templates; whatever shape they're
seeded in is the shape someone copies by hand for their own domain, and
seeding both taught "you need both" when the wildcard alone is now
sufficient for matching. `wcoDef`'s doc comment rewritten to name the
trade-off explicitly rather than silently drop it — a freshly-added
object's bare domain isn't enforceable until traffic to it is actually
observed, same as its subdomains always were — and to say what to do
about it (add the literal back into that one object by hand) rather than
just note the gap.

**Verified:** `wcoDef('google.com', ...).addresses` checked directly —
`['*.google.com']`, not the two-element form. `go build ./...`,
`go vet ./...`, `gofmt -l` clean; embedded-script `node --check` and
backtick-count sanity check (v433) both clean.

---

## v435 — 2026-07-15

Changed wildcard fqdn matching semantics, per explicit request:
`*.example.com` now matches `example.com` itself, not just its strict
subdomains. Previously this was a deliberate exclusion (mirroring TLS
wildcard-cert SAN conventions — a cert for `*.example.com` doesn't cover
the bare domain either) with its own doc comment explaining the choice;
overridden here on direct instruction, not a walk-back of the original
reasoning being wrong.

**What changed** (`internal/mesh/firewall_dns_sniff.go`,
`fqdnPatternMatch`): the wildcard branch now returns true outright when
the observed name equals the pattern's suffix exactly (`name == suffix`),
in addition to the existing strict-subdomain check. Doc comment rewritten
to describe "this domain and everything under it" rather than "everything
under it, excluding itself."

**Downstream effect, not just the unit-level matcher:** this function is
what the passive DNS sniffer (same file) uses to decide whether an
observed live DNS answer belongs to a wildcard fqdn object's address set.
A `*.example.com`-only object now starts absorbing `example.com`'s own
resolved address too, once traffic naming the bare domain is actually
observed on the wire — confirmed with a full pipeline test, not just the
pattern-match unit test.

**What didn't change:** `FW_COMMON_WILDCARD_OBJECTS`/`wcoDef`
(`internal/webadmin/ui.go`) still seeds both the literal bare-domain
entry and the wildcard form in every common-domain object, even though
the literal is no longer needed for *matching* purposes. Kept for a
different, still-live reason: a literal entry gets proactively resolved
by the periodic resolver (`firewall_fqdn.go`) on a schedule, independent
of live traffic; a wildcard entry's address set only grows when the
passive sniffer happens to observe real traffic naming that domain.
Dropping the literal would mean `google.com` itself has no known,
enforceable address until some connection to the bare apex is actually
seen on the wire — the literal entry is what makes it enforceable from
the moment the object is added. Comment above `wcoDef` rewritten to
explain this distinction plainly, since the old rationale (avoiding
redundant coverage) no longer applies but a different one does.

**Verified:** `TestFqdnPatternMatch`'s bare-parent case flipped
false→true; `TestFirewallWildcardFQDNEndToEnd`'s bare-domain case flipped
allow→deny, both with updated comments explaining why. Full firewall/fqdn
test group in `internal/mesh` (43 tests) green, `internal/config` green.
`go build ./...`, `go vet ./...`, `gofmt -l` clean on every touched file.
Grepped the whole embedded-UI file for backtick count (see v433) before
packaging — still exactly the one legitimate open/close pair.

---

## v434 — 2026-07-15

Removed the `+` button and category divider rows v431 added to the
Objects/Services suggestion rows — per feedback. Adding a not-yet-added
common entry is now a double-click on its row, same gesture already used
for editing every other cell in these tables, rather than a dedicated
button; suggestions render as plain rows with no heading between
categories. `fwSuggestRows` dropped its `colspan` parameter (nothing left
that needs one) and no longer tracks/emits a running category; the two
call sites' `.fw-suggest-add` click handlers became `.fw-suggest`
dblclick handlers on the row itself. CSS lost `.fw-suggest-cat`/
`.fw-suggest-add`, kept only a hover highlight. Both section hints
updated accordingly.

**Verified:** `fwSuggestRows` re-tested directly — no divider markup, no
button markup, already-added filtering and per-entry `data-def-idx`
(including across a skipped entry) still correct. `go build ./...`,
`go vet ./...`, and `gofmt -l` on both touched files, plus the
first-to-last backtick count check that catches what those don't (see
v433) — still exactly the one legitimate open/close pair.

---

## v433 — 2026-07-15

Build fix: v431's `fwSuggestRows` doc comment used backtick-quoted
identifiers ("`existing`", "`renderCells(def)`") as inline-code
formatting — two stray backticks inside what is, at the Go level, one
giant raw-string literal (`` const indexHTML = `...` ``) spanning the
entire embedded admin UI. Each stray backtick closes that string early;
the second one reopens a new one that never closes, so everything after
it — the rest of the file — gets parsed as literal top-level Go source
instead of string content. Hence the exact failure: `unexpected name
existing after top level declaration` at the first identifier Go's
parser hit once outside the string. Fixed by switching those two spans
to plain single-quotes; grepped the whole file afterward to confirm
exactly two backticks remain — the real open/close pair at the
declaration's start and end — since that count is the actual invariant
that matters here, not just "these two lines look right now."

**Why v431/v432 didn't catch this.** Both were checked with `node
--check` against a Python-re-extracted `<script>...</script>` slice, not
an actual Go build. That extraction takes the file's first backtick to
its last — which is exactly the pair a stray backtick in the middle
corrupts, so the heuristic can't see its own blind spot; the extracted
JS was self-consistent (valid JS in, valid JS out) even though the Go
source underneath was already broken.

**Process change:** installed a real Go 1.22 toolchain (`golang-go` +
`libpam0g-dev` for the cgo PAM auth backend, both from the Ubuntu
archive) rather than continuing to lean on the extraction heuristic.
`go build ./...`, `go vet ./...`, and `gofmt -l` on every touched file
now run before any `internal/webadmin/ui.go` change ships, the same bar
this changelog's own entries have been describing since v425 but that
wasn't actually being met the last few versions. All three clean as of
this version.

---

## v432 — 2026-07-15

Dropped v431's `color:var(--mut)` dimming on Objects/Services suggestion
rows — per feedback it looked off. They render at full text strength now,
same as a real row; the category divider above them and the `+` in place
of a checkbox are what mark a row as not-yet-added, no color treatment
needed on top. Updated the CSS comment and both section hints ("dimmed
rows are..." → "rows under a category heading are...") to match.

---

## v431 — 2026-07-15

Removed the Objects/Services tabs' "common…" picker modal (per-request:
"put them in the list with all the other services and objects" — the
modal was an extra click-through hiding a catalog that belonged in the
table itself). `FW_COMMON_WILDCARD_OBJECTS`/`FW_COMMON_SERVICES` entries
not yet in a network's real catalog now render as dimmed rows appended
directly after the real ones in the same table, grouped under a category
divider, each ending in a lone **+** button — clicking it is the entire
add action, no modal, no multi-select, no separate filter box (the
table's existing "filter all columns…" box already covers these rows
too, for free, since they're just `<tr>`s like any other).

**Didn't change:** nothing is written to a network's config — and
therefore nothing enters the gossiped payload every peer receives on
every change — until that row's own + is clicked. `FW_COMMON_SERVICES`'s
existing doc comment spells out exactly why that boundary exists (parapet
bakes a 1,000+-entry seed list into every fresh config; gravinet
deliberately doesn't, because that config gets gossiped network-wide on
every change, not stored once locally like parapet's). The picker modal
was already respecting that — opt-in per entry — so moving where the
opt-in click happens doesn't touch it. New rows only ever get created one
network-catalog-write at a time, exactly as before.

**What changed:** new shared `fwSuggestRows(catalog, existing, colspan,
renderCells)` builds the suggestion rows for both tables (existing =
name→bool of what's already present, case-insensitive, same check the
modal used for its "already added, disabled" rows); `secFwObjects`/
`secFwServices` call it and wire each row's own `+` directly to
`objSave`/`svcSave` with a one-entry list. `openCommonWildcardObjectsPicker`
and `openCommonServicesPicker` — and their `table._rowButtons` "common…"
wiring, and the now-orphaned `.pick-cat-*` modal CSS — removed. New
`.fw-suggest`/`.fw-suggest-cat`/`.fw-suggest-add` CSS for the muted rows,
category dividers, and the add button (kept full-strength against the
dimmed row so it still reads as the obvious click target). Both section
hints updated to describe the dimmed rows instead of the button that no
longer exists; the empty-catalog placeholder ("no objects/services —
click + to add one") now only shows if there's truly nothing to add,
custom or suggested.

**Verified:** `fwSuggestRows` unit-tested directly (same extraction
approach as prior entries) — an already-added entry correctly excluded,
a not-yet-added one included, a category divider per distinct remaining
category even across a skipped entry (confirmed the divider logic
re-fires correctly on the next differing category, not just the next
row), the per-entry `data-def-idx` surviving a skip so the click handler
still indexes the right catalog def, and the fully-covered case (every
entry already added) rendering nothing at all rather than an empty
divider. `node --check` on the re-extracted embedded script, caught and
fixed one real bug first: two hint strings used a bare apostrophe inside
a single-quoted JS string ("this network's catalog"), a silent syntax
break the extraction/`node --check` step exists specifically to catch
before it ships.

---

## v430 — 2026-07-15

Applied the same proto+port merge from v428's firewall rules table to the
Allow List (the global always-allowed exemption list, `/api/exempt`). A
`FirewallExempt` is exactly one proto+port leg, never a list — no named-
service catalog exists at this layer — so unlike the rules table this is
strictly proto/port, no service-name fallback. New `exParseSvc` accepts
`proto` or `proto/port`, where proto is `tcp`/`udp`/`icmp`/`ospf`/`any` or
a raw 0-255 protocol number (matching `config.ParseExemptProto` exactly)
and port is 0-65535 with 0/blank meaning any — notably *0 is a valid
explicit port* here, unlike the rules table's 1-65535 (matches
`validateExempt`'s existing range, unchanged). `exSvcLabel` is the
inverse, and resolves a management entry's live admin port for display
the same way the old separate port cell did.

**Behavior change worth flagging:** the old proto and port cells were
independently editable — changing proto never touched port, and vice
versa, except that saving the port cell always cleared `.mgmt` (even
back to blank) while saving proto never did. Merged into one field, that
asymmetry can't be preserved: proto and port are now edited together, so
*any* save through the combined field clears `.mgmt`, including a
proto-only-intended edit that previously could leave a management entry
tracking the live admin port. Reset-to-defaults remains the way back if
that's hit by accident.

**What changed:** header/row/`exemptAddRow` collapsed `proto`+`port` into
one `service` column (`ex-service`/`exa-service`, `colspan` on the empty
list 5→4); the two `ondblclick` handlers on `protoTd`/`portTd` became one
on `svcTd` using `exParseSvc`, alerting and restoring the row on a bad
entry the same way `exemptAddRow`'s save button already did.

**Verified:** `exParseSvc`/`exSvcLabel` unit-tested directly (same
extraction approach as v428/v429) — blank as any/any, proto/port, bare
proto, a raw protocol number alone and with a port, protocol number over
255 rejected, port over 65535 rejected, port 0 explicitly accepted, an
unrecognized proto like "bogus" rejected outright (no named-service
fallback exists at this layer, unlike the rules table), whitespace/case
handling, a management entry's label resolving the live port instead of
its stored one, and a label→parse round trip. `node --check` on the
re-extracted embedded script.

---

## v429 — 2026-07-15

Firewall rules table: reordered columns to the conventional
source/destination/services order (`action, src, dst, services, ...`,
was `action, services, src, dst, ...` since v428 merged proto/port into
the services field). Header, read-only row, and `fwAddRow`'s editor row
all reordered to match. `startFwEdit` needed no change — it targets cells
by class (`.fw-src`, `.fw-dst`, `.fw-services`) rather than position, so
it was already order-independent. The generic table filter/sort bar is
also order-independent (reads `header.cells` at render time), confirmed
by inspection rather than assumption.

---

## v428 — 2026-07-15

Firewall rule editor: merged the separate **proto** dropdown, **port**
input, and **services** field into one combined **services** column/field.
This was a UI simplification, not an engine change — `resolveLegs`
(`internal/mesh/firewall.go`) already unions a rule's inline proto/port
into the same `legs` list as every named service it references, matched
with OR semantics; the three widgets were just three ways of feeding that
one list, shown separately.

**Syntax.** The combined field takes a comma-separated mix of named
services and raw `proto` / `proto/port` entries — `https, tcp/8443,
udp/53`. New `fwParseSvc` splits on comma and classifies each token:
`any`, `tcp`, `udp`, or `icmp`, optionally with `/<port>` (1-65535),
is the inline leg; anything else is passed through as a named-service
reference exactly as before (still resolved/validated server-side against
the catalog — the datalist was always a suggestion, never a client-side
filter). At most one raw-leg token is allowed per rule, matching
`FirewallRule`'s single inline-leg slot; a second one is rejected
client-side before save with a message pointing at grouping extra ports
into a named service instead. A bare `any` with no port is a no-op, same
as leaving it out. New `fwSvcLabel` is the inverse — renders a rule's
inline leg plus its services into that same combined string, used for
both the read-only table cell and to prefill the editor field on
double-click.

**What changed:** table header and read-only row lost their separate
`proto`/`port` cells (`colspan` on the empty-rules placeholder updated
9→ from 11); `fwAddRow`/`startFwEdit` lost the `.fwe-proto`
`<select>`/`.fwe-port` `<input>`, replaced by one wider (100px→140px)
`.fwe-services` field; `fwCollectRule` now parses that one field via
`fwParseSvc` and returns `null` (after alerting) on a bad entry rather
than the old separate `fwPort`-returns-null pattern; `fwValidateNegate`
dropped its separate `port` parameter, deriving "any leg at all" from
the already-parsed rule instead — its alert wording also updated from
"ticked"/"untick" to "is on"/"turn ... off" to match v427's Ø-button
redesign (missed in that pass). The global quick-search index
(`cf.firewall.rules` entries) now builds its label from `fwSvcLabel` too,
so a rule identified only by a named service (no raw proto/port) is
findable by that service's name — it previously fell back to "any" and
was invisible to search on that term. `fwProtoOpts`/`fwPort` (now unused)
removed.

**Verified:** `fwParseSvc`/`fwSvcLabel` checked directly (extracted from
the embedded `indexHTML` string with the same script-extraction approach
used for the v427 syntax check) — bare named service, raw proto/port
alone, proto with no port, one raw leg mixed with several named services
in either order, two raw legs correctly rejected, `any` as a no-op alone
and combined with a service, `any/<port>` (wildcard proto, specific
port), out-of-range port rejected, empty field, case/whitespace handling,
and a label→parse round trip for a representative rule. Caught and fixed
one bug this way before shipping: `port` defaulted to `''` instead of
`0` when a field held only named services, which `fwCollectRule`'s
`parsed.port || 0` happened to paper over but the unit tests wouldn't
have — fixed at the source instead of relying on the call site's
fallback. `node --check` on the re-extracted embedded script, both before
and after the fix.

---

## v427 — 2026-07-15

Firewall rule editor: each dimension's NOT toggle (`src`/`dst`/`services`)
was a separate checkbox+label sitting next to its input
(`.fwe-neg-l`/`fwNegLabel`) — replaced with a small **Ø** button layered
inside the input itself (`.fwe-field`/`fwNegToggle`), toggled by click
rather than by checking a box. `.active` on the button is now the only
state: `fwCollectRule` reads it directly (`classList.contains('active')`)
instead of a checkbox's `.checked`, and `startFwEdit` sets it from the
row's `data-*-negate` attributes the same way it always populated the old
checkbox. Off is dim (`var(--mut)`), matching a placeholder; `.active`
switches to `var(--danger)` so it visually agrees with the same dimension's
leading **!** in the read-only table view. No wire/API change — still the
same `src_negate`/`dst_negate`/`services_negate` booleans on the rule,
`fwValidateNegate`'s empty-dimension guard untouched. Add-row and
inline-edit both wired through the new shared `fwWireNegToggles`. The
section hint text updated from "tick it" to "click it" to match. Embedded
admin JS re-checked with `node --check` after the change — parses clean.

---

## v426 — 2026-07-15

Upload page's file input(s) (`.up-file`, plus `.up-bin`/`.up-man` in signed
mode) widened so a full filename — source tarball, or a signed manifest —
is readable without truncating. First pass bumped both padding dimensions
(`padding:14px 16px`), which also grew the input's height past every other
field on the page; corrected to `min-width:420px` alone, leaving the global
`input,select` padding — and therefore height — untouched.

---

---

## v425 — 2026-07-14

Firewall rules can now negate source, destination, and services
independently — "match anything EXCEPT this" for each dimension, matching
what parapet offers on its src/dst editor (gravinet extends the same idea
to services too, which parapet doesn't have a negate toggle for).

**Semantics.** `SrcNegate`/`DstNegate`/`ServicesNegate` each flip what
their dimension's match means, applied *after* the field resolves exactly
as it always did — a raw CIDR, an object reference, "any", inline
proto/ports, and named services all negate the same way. The three
dimensions still combine with the rulebase's existing AND semantics
(unchanged): a rule matches only when every one of its dimensions holds,
negated or not, so two negated dimensions on the same rule only both apply
— by De Morgan's law — when a packet is outside *both* exemptions
simultaneously (verified explicitly: being inside either one alone is
already enough to keep the rule from matching).

**The one deliberate non-special-case:** negating an "any"/empty
dimension is accepted exactly as written rather than being rejected or
silently reinterpreted — the universal set, negated, is the empty set, so
that dimension then matches nothing. Correct, if rarely useful on purpose.
Since reaching that state from an untouched empty field plus a stray
checkbox tick is almost always a *mistake*, though, the web UI catches it
before saving: a NOT toggle ticked over an empty field trips a plain-
language alert ("...that would match nothing; set src or untick NOT") and
refuses to save, rather than the engine silently accepting a rule that can
never fire. The engine itself imposes no such restriction — the CLI and
the control-plane API both accept it as written, since an operator
scripting a rule presumably meant what they typed.

**What changed:**
- `mesh.FirewallRule` / `config.FirewallRule`: three new `omitempty` bool
  fields (`src_negate`/`dst_negate`/`services_negate` on the wire) —
  confirmed an ordinary non-negated rule's JSON grows no new keys at all,
  so existing configs and existing peers on a mixed-version mesh read
  identically.
- `fwRule.match`: each dimension now computes its positive match first,
  flips it if negated, same short-circuit-on-false structure as before —
  the hot path's cost for a non-negated rule (the overwhelming majority)
  is one extra `if` per dimension that never branches.
- The persist-hook (mesh → config, on every live edit) and the config-load
  path (config → mesh, on startup) both carry the three fields through —
  found by grep, since these are hand-written field-by-field struct
  literals in `cmd/gravinet/main.go`, not something a JSON round-trip
  would've carried automatically.
- `gravinet fw add` gained `-src-negate`/`-dst-negate`/`-services-negate`
  flags, and `gravinet fw list` now prefixes a negated src/dst with `!`
  and appends `!svc` for a negated service dimension — the same `!`
  convention CLI firewall tools already use for "not this."
- Web UI: each of the src/dst/services editors (add-row and inline edit)
  gained a small "NOT" checkbox next to its input, pre-populated correctly
  when editing an existing negated rule; the rules table shows a leading
  **!** on a negated cell with a tooltip spelling out what it means.

**Verified:** unit tests for each dimension negated alone, the documented
any-negated-matches-nothing edge case, negation through an object
reference (not just a raw CIDR), the two-dimensions-negated-together De
Morgan's-law interaction (all four quadrants checked explicitly), and spec
round-trip. Confirmed the tests actually catch a real regression — not
just passing vacuously — by deliberately dropping the src-negate flip
locally, watching three different tests fail for three different reasons,
then restoring the fix. `config.Firewall`'s JSON round-trip test extended
to cover negate fields and to assert the omitempty/no-new-keys claim
above. The web UI's full flow — negated-rule rendering, pre-checked
checkbox on edit, the validation alert blocking an invalid save, and a
valid negated add posting exactly the right payload — checked end-to-end
with a headless jsdom harness against the real extracted `indexHTML`.

`go build ./...` / `go vet ./...` clean; `gofmt -l` clean on every touched
file. Full `internal/mesh` (firewall/fqdn suites), `internal/config`,
`internal/webadmin`, and `cmd/gravinet` test suites green.

---



Wildcard fqdn support, matching what gravinet's sibling project parapet
offers: an fqdn-kind address object's Addresses can now include entries
like `*.example.com` — covering every subdomain of example.com — alongside
(or instead of) literal names.

**Why this needed new machinery, not just a new string format.** There is
no DNS query that means "give me every address under this domain" — a
wildcard pattern isn't a name you can look up, it's a *filter over answers
you observe*. parapet handles this by sitting on the LAN interface and
passively watching real DNS response traffic go by. gravinet doesn't have
that exact vantage point (it's a mesh VPN, not a LAN gateway product), but
it has something arguably better for this: `firewall.allow(dir, pkt)`
already receives every packet's raw bytes at the exact enforcement point,
for both directions (`fwIn`: mesh → local TUN, `fwOut`: local TUN → mesh),
since gravinet's firewall matching is Go code operating on packet bytes
directly rather than a delegated OS ruleset. Any DNS response addressed to
or from a network this node participates in is already being handed to
Go here — no raw-socket capture, no separate sniffer process, no new
capability requirement.

**What got built** (`internal/mesh/firewall_dns_sniff.go`, new file):

- A minimal, hand-rolled DNS wire-format parser (`dnsParseResponse`) —
  header, question-skip, and A/AAAA answer records, including compression-
  pointer decoding (the overwhelmingly common real-world shape; every
  answer name in a typical response is a pointer back into the question
  section, not a fresh label sequence). No external dependency, matching
  the rest of the module — this project vendors nothing. Pointers are
  capped at 128 hops and rejected outright unless they point strictly
  backward, so a truncated, malformed, or deliberately hostile message
  (this parses bytes taken directly off the wire, from any peer) can only
  ever fail to parse — never panic, never loop.
- `fqdnPatternMatch`, with the exact semantics parapet settled on:
  `*.example.com` matches any strict subdomain but **not** example.com
  itself — a DNS wildcard label has no meaning to a real resolver, so that
  name never actually appears as an answer to that pattern on the wire.
  List the bare domain separately (same object, comma-separated) to cover
  both.
- A per-object TTL cache (`wildcardFQDNCache`) — each learned address
  expires on its own DNS record's TTL, the same idea as parapet's nft
  `flags timeout` sets, done here with an explicit map since Go firewall
  code doesn't get expiry for free from a kernel ruleset. The hot packet
  path only ever does a cheap map update under its own mutex; a periodic
  sweep (wired into the existing 5-second maintenance tick, right next to
  the conntrack and NAT sweeps already there) drops expired entries and
  promotes the current live set into the firewall's actual catalog —
  decoupled on purpose, so a burst of DNS traffic can't turn into a burst
  of firewall recompiles.
- Literal (non-wildcard) fqdn entries are completely unaffected — they
  keep using the existing 60-second poll resolver exactly as before, which
  now explicitly skips wildcard entries (`literalFQDNNames`) rather than
  wasting a guaranteed-to-fail DNS query on `*.example.com` every tick. An
  object naming both kinds of entry gets contributions from both paths,
  unioned (`mergedFQDNLocked`) — verified with a test that resolves one
  entry via `setFQDN` (simulating the poller) and the other via a real
  sniffed packet, and confirms a rule referencing that one object matches
  traffic to *both* resulting addresses.
- The packet path's own hot-path cost for a network with no wildcard fqdn
  objects configured: one atomic pointer load before touching anything
  else. `wildcardPatterns` is refreshed only on object edits
  (`refreshWildcardPatternsLocked`, called from the existing
  `rebuildCatalogLocked`), read lock-free on every packet — matching the
  rest of this file's stated data-path discipline.

**Add the wildcard fqdn objects, too** — the Objects tab (Firewall >
Objects) now has a "common…" button next to the existing +/−, mirroring
last version's Services-tab catalog: a curated, 83-entry list of
well-known domains (Google, Microsoft, Amazon, Apple, GitHub, Netflix, and
so on, grouped by category) ready to add as fqdn objects. Each entry seeds
*both* the bare domain and its `*.`wildcard in one object — "block/allow
Google" means both the domain itself and everything under it to most
people, and it doubles as a working example of the literal+wildcard
mixing described above.

Deliberately **not** a mechanical port of parapet's Tranco-top-N seed list
(the actual ask): gravinet's object catalog lives in each network's
config, and that config is gossiped to every peer on every change. A
standalone router pays only its own local storage for a 1,000+ or
10,000+-entry seed list; gravinet would pay that cost network-wide, on
every sync. A hand-curated few dozen to ~80 entries for the services
people actually write rules against most often gets most of the real
value without turning routine config edits into large gossip payloads —
and, same as the Services-tab catalog, it's opt-in per network from the
picker, not baked into every fresh config. Nothing stops adding
less-common wildcard objects by hand the ordinary way; this is a
convenience for the common case, not a ceiling.

Verified thoroughly, given how much of this is parsing untrusted bytes off
the wire: unit tests for the pattern matcher (including the bare-parent-
exclusion and case/trailing-dot handling), the DNS parser (basic, multi-
answer, AAAA, compression pointers, query-rejection, and a full
0..len(message) truncation sweep asserting no panic at any cut point), the
TTL cache (expiry, and that a later longer-TTL observation of the same
address extends rather than shortens its life), and an end-to-end test
that runs a real crafted DNS response packet through `firewall.allow`,
sweeps, and confirms a rule referencing the wildcard object matches
exactly the sniffed address — not the unrelated one, not the bare parent
domain from a separate response, and not an address from a same-port
packet that only looks like a response but has QR unset. Confirmed each
of those tests actually catches a real regression (not just passing
vacuously) by deliberately breaking the parent-domain exclusion locally,
watching both the unit test and the end-to-end test fail, then restoring
the fix. The UI catalogs (both this one and Services') were checked
end-to-end with a headless jsdom harness against the real extracted
`indexHTML` — categorized rendering, already-present-name disabling
(confirmed it doesn't clobber a customized existing object), filtering,
and the exact payload handed to `objSave`/`svcSave` on "Add selected".

`go build ./...` / `go vet ./...` clean; `gofmt -l` clean on every file
this touched (one pre-existing, unrelated misalignment in `firewall.go`'s
`FirewallRule` struct — present in the original upload, untouched here —
left as found rather than folded into an unrelated diff). Full
`internal/webadmin` and the relevant `internal/mesh` suites green,
including a live two/three/four-node network simulation
(`TestRouteFailoverBetweenTwoOrigins`) exercising the maintenance loop the
new sweep now runs on every tick, to catch anything the sweep itself might
have disturbed there.

---



Firewall > Services now has a "common…" button (next to the existing +/−)
that opens a picker over a curated, 63-entry catalog of well-known
protocol/port bundles — grouped into Web, Remote access, Name & directory,
DHCP, Time, Mail, File transfer, Databases, VPN & tunneling, Voice &
streaming, Monitoring & management, DevOps & observability, Routing
protocols, Diagnostics, and Wildcards — so populating a network's service
catalog no longer means hand-typing "tcp/80" for HTTP, "udp/500, udp/4500"
for IPsec IKE, and so on for everything the network actually needs. This is
the same convenience gravinet's sibling project parapet gives its users via
a large pre-filled default service list.

Deliberately **not** implemented the way parapet does it, though: parapet
bakes its catalog straight into every fresh config's default service list.
That's a lot of unused entries in a config the moment it's created, and
there's no clean way to walk it back once it's there. Here it's opt-in per
network, from the Services tab: tick what you want, leave the rest, add
more later. An entry whose name already exists in that network's catalog
(case-insensitive) is shown but disabled — re-opening the picker after
adding some doesn't invite creating a duplicate or silently clobbering a
service you've since customized (verified: an existing "SSH" on a
non-standard port is left completely untouched by the picker regardless of
what's ticked).

Every entry uses only protocols `internal/mesh/firewall.go`'s `protoNum`
actually matches on: `tcp`/`udp` by port, `icmp`/`icmpv6`/`ospf` by name,
and the *raw IP protocol number as a string* for anything else (EIGRP 88,
VRRP 112, PIM 103, GRE 47, ESP 50) — `protoNum` silently treats an
unrecognized *name* as "any" rather than erroring, so a plausible-looking
but wrong string (e.g. "eigrp") would have been a silent catalog bug, not a
build failure. Checked every entry's protocol against that function's
switch statement before including it.

Verified end-to-end with a headless jsdom harness (loading the real
extracted `indexHTML`, not a reimplementation): catalog has no duplicate
names and no unrecognized non-numeric protocols; the picker renders one row
per catalog entry; a pre-existing "SSH" is correctly shown disabled with
its custom port untouched after the add; filtering for "routing" correctly
narrows to just the Routing protocols section; selecting HTTP, HTTPS, and
OSPF and clicking "Add selected" calls the existing `svcSave` with exactly
those three appended to the network's existing catalog, OSPF's entry
carrying `{proto:"ospf", port_min:0, port_max:0}` (matches the protocol
regardless of port, correct for a raw-IP-protocol match), and the modal
closing on success.

`go build ./...` / `go vet ./...` clean; full `internal/webadmin` test
suite green (unrelated to this change — it exercises Go-side handlers, not
the embedded JS — but confirms nothing else regressed).

---



Follow-up to v421: FreeBSD was still reporting "no Go toolchain found on
this node (checked PATH and /usr/local/go/bin)" after that fix landed.

v421's `locateGo()` fallback only checked `/usr/local/go/bin` — where
`official_install_go()` unpacks the go.dev tarball. But
`install-freebsd.sh`'s `ensure_go()` doesn't go straight to that tarball:
it tries `pkg install go` *first*, and only falls back to the tarball if
that fails. `pkg` installs into the ports prefix's `bin/` directly —
`/usr/local/bin/go`, not `/usr/local/go/bin/go` — since `pkg`-installed
software has no reason to nest itself under a private subdirectory the way
a hand-unpacked tarball does. On a FreeBSD box where `pkg install go`
succeeded (the common case — `pkg` is virtually always available and is
tried before any network fetch from go.dev), the toolchain was sitting
exactly where the installer itself put it, and `locateGo()` still didn't
know to look there. Same root cause as v421, just one directory short of
covering it. OpenBSD's `pkg_add go` installs to the same `/usr/local/bin`
layout, so this was a latent gap there too, just not yet reported.

Fixed: `goInstallDirs` now checks `/usr/local/bin` in addition to
`/usr/local/go/bin`. The "no Go toolchain found" error message now lists
whichever directories were actually checked instead of hardcoding just the
one, so a future gap like this is visible in the error itself rather than
requiring a source read to diagnose.

Verified: `TestLocateGoFindsToolchainOutsidePATH` is now table-driven over
both fallback slots (confirmed it fails for the pkg-install slot against
the v421 code, passes against this fix). Added
`TestGoInstallDirsCoversPkgInstallLocation`, which asserts against the real
production `goInstallDirs` value (not a test double) that both paths are
present — deliberately reverted it locally to confirm it fails against the
v421 list before restoring the fix. `go build ./...` and `go vet ./...`
clean; `internal/upgrade` and `internal/webadmin` tests green, including
the pre-existing end-to-end `TestStageFromSourceBuildsWithGoOffPATH`.

---



Four upgrade-pipeline bugs, all reported directly by an operator running
upgrades across a mixed macOS/Windows/FreeBSD/OpenBSD fleet, all in the
build-from-source path the web admin uses in unsigned/local-only mode
(`internal/webadmin/upgrade_source.go`) or the platform installers'
own build_from_source() shell functions.

**macOS and FreeBSD: "no Go toolchain found" when one was clearly
installed.** `buildFromSource` located `go` with a bare `exec.LookPath("go")`
against the *daemon's own* environment. That's correct for a user's
interactive shell, but the web admin runs inside the gravinet daemon, which
launchd (macOS) and rc.d (FreeBSD) start with their own minimal, inherited
PATH — not the PATH an operator had open in Terminal when they, or an
earlier run of install-macos.sh/install-freebsd.sh, put Go at
`/usr/local/go/bin` (every platform installer's own `ensure_go()` unpacks
Go there and checks that exact path as a fallback before re-fetching
anything). The daemon's PATH never had it, so the daemon never found it,
regardless of what `go version` reported from the operator's own terminal.

Fixed: `locateGo()` now checks this process's PATH first (so a
package-manager install already on PATH, or an operator-customized service
PATH, is always respected), then falls back to `/usr/local/go/bin`,
matching exactly what the installers themselves already do. Verified with a
test that clears `PATH` entirely, points the fallback at a toolchain that's
only reachable that way, and runs the real `stageFromSource` pipeline
against gravinet's own current source tree end to end — build, self-probe,
and ingest all succeed, reproducing and closing the exact reported failure.
Confirmed the new test actually catches the old bug by reverting the fix
locally and re-running it (fails with the original "no Go toolchain found"
error, as expected) before restoring the fix.

**Windows: "built a binary but could not identify it."** The build
succeeded — the failure was one step later, in `ProbeBinary` trying to run
the freshly built candidate to read back its own version. The output path
was hardcoded to `gravinet-built`, no extension. Windows requires a
recognized extension (`.exe`, `.bat`, ...) before `os/exec` will run a file,
*even given its exact, unambiguous full path* — a well-documented Go/Windows
gotcha, not a corrupt or wrong-arch binary. The resulting error ("executable
file not found in %PATH%") was actively misleading: a message that reads
like a PATH problem, for a binary that was never looked up on PATH at all.
`install-windows.ps1`'s own from-source build path already knew this — it
unconditionally names its output `gravinet.exe`. The web admin's Go-side
build path just never got the same treatment.

Fixed: `stageFromSource` now names the candidate `gravinet-built.exe` on
`runtime.GOOS == "windows"`, `gravinet-built` everywhere else.

**OpenBSD: `~/.cache/go-build` left behind after every install, eventually
filling the filesystem.** `install-openbsd.sh`'s `build_from_source()`
already stages its build output under a `mktemp -d` (`BUILD_TMP`) that an
`EXIT` trap cleans up on every exit path — but `go build` itself was left to
default `GOCACHE`/`GOPATH` to under `$HOME` (root's, since installs run as
root), and go build never cleans that up itself; it's a persistent cache, by
design, for a machine that expects to keep rebuilding the same module. On a
one-shot install/upgrade that isn't going to happen again anytime soon,
that's not a cache, it's litter — a fresh multi-hundred-MB copy on every
single run, on a platform whose default disklabel doesn't give the root
partition much room to begin with.

Fixed: `GOCACHE` and `GOPATH` are now redirected under the same `BUILD_TMP`
the EXIT trap already removes, so the build gets its own scratch space and
nothing outlives the run. Applied the identical fix to
install-macos.sh, install-freebsd.sh, and install-linux.sh too — same
`build_from_source()` shape, same unset-GOCACHE default, same latent leak;
only OpenBSD's smaller default root partition had made it visible yet.

All four fixes verified: `go build ./...` and `go vet ./...` clean across
the whole module; `go test` green for `internal/upgrade` and
`internal/webadmin` (the packages touched), including the new
PATH-cleared/fallback-only tests above; all four edited install scripts
pass `bash -n`.

---

## v420 — 2026-07-14

Fixed the "systemd-resolved is unavailable on this host" warning on Debian
13 (trixie) — a real installer bug, not a Debian quirk to route around.
`pkg_install()`, the helper `ensure_resolved()` calls to get resolved onto a
host that doesn't have it, only ever knew `dnf`/`yum`. On Debian/Ubuntu it
silently fell through to `return 1` — no `apt-get` branch existed at all —
so the "not installed; installing it" log line was a lie: nothing was
attempted, and the follow-up check predictably still found no unit,
producing the warning.

This one was invisible on Debian ≤12 and current Ubuntu because
systemd-resolved shipped bundled inside the base `systemd` package there —
`ensure_resolved`'s own pre-check already found the unit and never called
`pkg_install` at all. Debian 13 split it into its own `systemd-resolved`
package that isn't pulled in by default (verified against the actual trixie
archive: 257.13-1~deb13u1), which is exactly the case that walks straight
into the missing branch. `install_build_deps`, a few dozen lines away in the
same file, already had full apt/dnf/yum/pacman/zypper/apk coverage for a
different purpose (the C toolchain + PAM headers) — `pkg_install` just never
got the same treatment when it was written.

Fixed: added `apt-get`, `zypper`, and `pacman` branches, matching
`install_build_deps`'s existing multi-distro pattern. Verified for real, not
just parsed — extracted the exact function and ran it standalone against
the actual `systemd-resolved` package on this environment's Ubuntu 24.04
sandbox (which has `apt-get`, exercising the same branch Debian 13 needs):
clean install, unit created, symlinks correct, package purged afterward to
leave the sandbox as found. Also corrected the doc comment above it, which
had claimed resolved was simply "the default resolver on Debian/Ubuntu" —
true enough historically that it's exactly what made this bug easy to not
notice, no longer true for Debian 13 specifically.

---

## v419 — 2026-07-14

Real gap, fairly called out: `/api/upgrade/*` has always been in `LOCAL_API`
— every call goes to the node you're actually logged into, never to
whichever peer is selected in the header — but the Upgrade page never said
so. Select a Managed peer, and the page rendered completely normally: same
hint, same file picker, same Upgrade button, giving no indication that
clicking it would upgrade the node you're on, not the one named in the
dropdown.

Fixed the same way the Settings page already handles Managed/Manager/Remote-
shell toggles being remote-uneditable (`local-only-disabled`, previously
scoped to `.settings-row`, now general enough to use on the Upgrade card
too): when a peer is selected, the whole Upload card dims, every input and
button in it gets `disabled`, and a message names the selected peer
explicitly and says what to do instead — pick "This node," or log into that
peer's own web admin. Re-evaluated fresh on every peer switch, same as the
rest of the page, since `drawUpgrade` already re-runs on `refresh()`.

`docs/UPGRADES.md` updated to describe the visible behavior, not just the
underlying enforcement.

---

## v418 — 2026-07-14

Chevrons were centering their *box* in the row fine (the parent's
`align-items:center` handled that) but not the *glyph* within that box —
`line-height:1` places a character on its normal font-metric baseline, which
for a small triangle symbol sits noticeably low, not mid-box. Invisible at
12px, obvious at the 24px v416 doubled it to. `.rail-chevron` is now itself
a small centering flex box (`inline-flex; align-items:center;
justify-content:center`) over a fixed 24×24 square, so the glyph centers on
both axes regardless of the font's own baseline metrics for that character.

---

## v417 — 2026-07-14

Unsigned-mode upload row, per exact spec: hint text simplified to "Upload
the source tarball and it's built and applied automatically." (dropped the
`.tgz`/`.tar.gz` parenthetical from the sentence and the separate label
line under it — redundant with the sentence itself and the file input's
own `accept` filter). File picker and Upgrade button back on one row
(reverting v415's stacked layout) with 16px of explicit space between them.

---

## v416 — 2026-07-14

Sidebar nav-group chevrons (`.rail-chevron`, the ▾ next to MESH/TRAFFIC/
NAMING/MONITOR/INFO) doubled in size: 12px → 24px, font-size and box width
both, so the rotated glyph doesn't clip. Only place this class is defined;
nothing else needed touching.

---

## v415 — 2026-07-14

Layout only: the file picker and the Upgrade button were side by side in
the same flex row (`.tbar`, shared with signed mode's binary+manifest+Stage
layout). Unsigned mode now stacks them — label, then the picker on its own
line, then Upgrade directly below it — by giving that branch its own block
container instead of appending straight into the flex row. Signed mode's
layout is untouched.

---

## v414 — 2026-07-14

The trial banner and the Roll back button are both gone from the Upgrade
page. Neither survives contact with the actual problem: `drawUpgrade`
renders once, on load, from whatever `/api/upgrade` returned at that
instant, and never again — there's no polling, no re-render, nothing
watching the guard's phase change underneath it. "Trial in progress: 413
\u2192 413, 90s to prove itself" wasn't wrong when it was drawn; it was just a
snapshot that had no way to become "commited" or disappear once the 90
seconds actually passed, so it sat there looking like a live status forever
after it stopped being one. Reapplying the same version making it read
"413 \u2192 413" was the same underlying issue wearing a different, more
obviously broken face — nothing about the display was ever going to be
trustworthy for something time-based drawn exactly once.

The fix isn't a smarter banner, it's no banner. The unsigned path is now
literally: hint text, a file input, an Upgrade button. Nothing reads
`u.phase`, `u.from`, `u.to`, `u.confirm_seconds`, `u.last_error`, or
`u.rollback_available` anymore. Signed mode (binary + manifest + Stage) is
unaffected — it never had any of this.

Roll back and the guard's actual trial/confirm/revert mechanics are
untouched server-side — `gravinet upgrade rollback` from the CLI still
works exactly as before, and a real second apply still gets refused while
one's already mid-trial (the v410 guard). Only the GUI's now-provably-unable-
to-stay-honest display of that state is gone. `docs/UPGRADES.md` updated to
match.

---

## v413 — 2026-07-14

Trimmed the Upload hint further: "Upload the source (.tgz/.tar.gz) and it's
built and applied automatically." No more key/signing mention on the page
itself — still in docs/UPGRADES.md if needed.

---

## v412 — 2026-07-14

Default (unsigned) mode no longer accepts a pre-built binary at all —
source only. Picking a `.tgz`/`.tar.gz` was already the common case (it's
what every fresh checkout of this project is), and the "either works, it
figures out which one you gave it" framing for the rarer binary-upload case
was adding a whole branch of code and a paragraph of explanation for
something not worth the complexity.

Removed: `sniffAndStage` (the gzip-magic-byte sniffer that told a tarball
from a binary) and `stageUnsignedArtifact` (the raw-binary probe-and-ingest
path) — both gone from `internal/webadmin/upgrade.go`, along with the test
that covered the sniffing decision, since there's no decision left to make.
`handleUpgradeStage` goes back to always requiring a manifest, uniformly —
it no longer special-cases unsigned mode at all; Store.Verify was already
the thing deciding whether that manifest needed a real signature or just
had to be well-formed, so the handler didn't need its own copy of that
logic. The GUI's default upload now goes straight to
`/api/upgrade/stage-source`, and does it as a raw request body — `fetch(url,
{body: file})` — rather than wrapping a single file in a multipart form for
an endpoint that only ever read one stream anyway. Verified with a real
HTTP round-trip through the full stack (auth, routing, extraction, build,
ingest) using the exact request shape the browser now sends, not just a
unit test of the handler in isolation.

Also cut the paragraph explaining the binary-or-source choice — down to one
sentence, since the choice it was explaining doesn't exist anymore:

> No signing key required. Upload the source (.tgz/.tar.gz) and it's built
> and applied automatically. To require signed builds instead, set
> upgrade.trusted_keys in the config.

`docs/UPGRADES.md` updated to match, and a second inaccuracy caught while
in there: it claimed `gravinet upgrade stage -bin ./gravinet` was the CLI
equivalent of the GUI's key-free upload. It isn't — `cmdUpgradeStage` has
always unconditionally required a manifest file to exist, signed or not,
with no fallback. There is currently no CLI path for what the GUI's
build-from-source flow does; the doc now says so instead of giving a
command that fails.

Signed mode (`trusted_keys` configured) is untouched — binary + manifest,
two fields, same as it's been since v403.

---

## v411 — 2026-07-14

The staged-artifacts table is gone. v410 fixed it so the *currently running*
version couldn't show up in it looking actionable; a screenshot right after
showed the actual remaining problem — an old, already-superseded build
(409, while the node ran a later version) still sitting there mid-card with
its own Dry run / Apply here buttons, above a second, separate upload
control. Filtering which rows appeared was the wrong fix. The table itself
was the thing that didn't belong: the GUI's job is upload-and-apply, and a
list of old binaries with their own competing set of actions was never
that.

Removed outright — not filtered, not hidden-when-empty, gone — along with
everything that only existed to feed it: `fmtBytes()`, and `localApply()`
(which existed to serve both the table's per-row buttons and the main
Upgrade button; with the table gone it had exactly one caller and one
argument shape, so it's inlined into that one call site instead of staying
a function with parameters nothing uses anymore, `dryRun`/`skipConfirm`
included). `handleUpgradeHome` also stopped computing and returning the
`staged` list in the `/api/upgrade` JSON — nothing reads it now, and
building it meant listing and parsing every manifest in the store on every
single page load for data nobody was looking at.

An old or interrupted artifact sitting in the store hasn't become
unreachable, just no longer something the GUI shows you unprompted:
`gravinet upgrade list` / `apply -id ID` from the CLI is how you finish or
revisit one now, same as it always could. `docs/UPGRADES.md` updated to
match — described accordingly instead of pointing at a table that isn't
there anymore.

---

## v410 — 2026-07-14

A screenshot showed the actual bug behind "this is confusing": right after a
successful one-click Upgrade, with a "Trial in progress: 408 \u2192 409" banner
on screen, the page *also* showed 409 sitting in the staged-artifacts table
with its own Dry run / Apply here buttons, next to a second, entirely
separate file-picker-and-Upgrade-button control below it. Two different
action areas, one of them offering to "apply" the exact binary the banner
said was already applied and currently proving itself.

Root cause: the staged table was never "things pending an action," it was
"whatever's sitting in the local artifact store" — which always includes
the just-applied version, because `GC(keep)` only trims down to the last 3,
it doesn't remove the current one. The empty-when-nothing-staged logic from
v407 assumed those were the same thing. They aren't.

Two fixes:

- The staged table now filters out any artifact whose version matches what's
  currently running. An old, genuinely different build still shows (that's a
  real choice: re-apply or inspect it); the one you're already running does
  not.
- While a trial is actually in progress, the page shows only the trial
  banner and a "Roll back now" button \u2014 no staged table, no upload form.
  There is nothing else sensible to do while a swap is being timed; offering
  to start another one just invites exactly the confusion in the screenshot.

That second point exposed a real backend gap while fixing it: nothing
stopped a second `apply` from re-arming the guard while one was already
mid-trial, which would have silently overwritten the confirm window and
boot count the first trial was being judged against. `controlOp`'s `apply`
case now refuses a non-dry-run apply while `Guard.Load().Phase ==
PhasePending`, with a clear error naming the trial already in flight.
Dry-run stays unrestricted \u2014 it never touches guard state, so there's
nothing to protect against there.

---

## v409 — 2026-07-14

Rewrote every description on the Upgrade page in plain English — shorter
sentences, no stacked em-dash asides, no jargon that wasn't earning its
place.

That surfaced a real bug, not just a style problem: the page's top-level
summary still said *"This node accepts only artifacts signed by a key in
its upgrade.trusted_keys ... and it fails closed with none configured"* —
describing the old signed-only default, directly under the Upload card
which correctly says the opposite ("trusts no release keys ... unsigned").
Two blocks on the same page contradicting each other, because the top
summary was written before local-only-unsigned became the default in v403
and never got revisited when the actual behavior changed underneath it.
Fixed: the top hint is now a short, always-true description of what the
page does; the specifics of signed vs. unsigned live only in the Upload
card's hint, which was already correctly conditional on
`signing_required`.

Also simplified: the file-picker label, the "pick a file" alert, and the
upload confirmation dialog.

---

## v408 — 2026-07-14

Two fixes to the Upgrade page.

**The "This node" card is gone.** Version, phase, peer count, install path,
and store path — a whole card and table for information that mattered a lot
more when this page also had a Fleet view to cross-reference against. Roll
back moved out of it and sits next to the Upgrade button instead, since it's
the one thing in that card that was an action, not a status readout. What's
left of the status — a live trial in progress, or a self-revert — now
surfaces as a single warning line above the upload controls, and only when
there's actually something to say; the ordinary idle state says nothing.
`phaseBadge()` and the Refresh button went with it — nothing left calls
either.

**Every text-labeled button on the page was using the wrong CSS class.**
`.tbar-btn` is a 28×25px fixed-size square meant for a single glyph — every
other use of it in the app is a bare `+`, `−`, `=`, or similar icon. Upgrade,
Roll back, Stage, Dry run, and Apply here all had real text labels forced
into that box, which is exactly the cramped, overlapping look from the
screenshot a few versions back. Switched all five to plain `.sm`, the class
already used correctly for every other text button in the app (Restart now,
Run, Start). This bug predates this session's changes — it was already
there in the original page — but every rewrite of this page across the last
several versions copied the class forward without anyone questioning it
until now.

---

## v407 — 2026-07-14

Fleet and Rollout are gone — not hidden, not disabled, deleted. Both were
UI for mesh-wide binary distribution, and neither has been able to do
anything since upgrades went local-only: a rollout that stops at the first
target, forever, isn't a degraded feature, it's dead code with a page on top
of it. A screenshot of the Upgrade page showed exactly that — a Fleet row
reading `peer returned 403 Forbidden`, which was the local-only lockdown
working as intended, not a bug, but also not a reason to keep the view that
surfaced it.

Removed outright: the Fleet and Rollout cards; `gravinet upgrade fleet` /
`rollout` on the CLI; `internal/upgrade/rollout.go` and `fetch.go` in full
(`Rollout`, `Plan`, `Target`, `Source`, `NodeState`, `ApplyRequest`,
`OverlayClient`, the peer-to-peer artifact fetch); `handleUpgradeFleet`,
`handleUpgradeRollout`, `handleUpgradeBlob`, and the old manifest+sources
`handleUpgradeApply` in webadmin; `overlayFleet`, `targets()`, `unmanaged()`,
`selfSource()`, and the rollout half of `controlOp` in the daemon; the
`AcceptsRemote`/`ServeToPeers` fields and config knobs that existed to gate
a channel that's now just gone rather than gated. ~1,600 lines removed
across the tree. `internal/upgrade`'s own test suite runtime dropped from
~35s to ~2s as a direct consequence — that was rollout's wave-timing tests,
gone with the code they tested.

The upload flow itself is also down to one control: the two-button
Upload-vs-"review first" split from v405 collapsed into a single
**Upgrade** button (upload → build if needed → apply → restart, one
confirmation), with the staged-artifacts table now only rendering when
something is actually sitting there unapplied — normally, nothing is. "This
node" (version/phase/peers) and Roll back stay: unlike Fleet/Rollout, both
are genuinely load-bearing for a single-node deployment, not leftovers of
the distribution model.

`healthy()` in `cmd/gravinet/upgrade.go` survived a first-pass deletion by
accident — a `grep` for `.healthy(` missed its actual call site,
`upg.guard.Watch(upg.healthy)` in `main.go`, which passes it as a method
value rather than invoking it directly. It's the boot-time watchdog that
reverts a bad upgrade if peers don't come back; the build catching the
resulting `undefined: healthy` is the reason it's back rather than actually
gone. Worth recording plainly rather than only fixing quietly.

Also fixed in passing: `controlOp`'s `"list"` op (`gravinet upgrade list`)
had the same unguarded `m.Signer[:16]` panic-on-unsigned-manifest bug that
`handleUpgradeHome` was fixed for back in v403, when unsigned manifests
first became possible. Same root cause, different call site — missed the
first time because that fix was scoped to the web admin handler without
checking the CLI's own copy of the same formatting logic.

Docs (`docs/UPGRADES.md`) rewritten again to match: no Fleet/Rollout section
at all now, described as removed rather than present-but-inert; the
single-button upload flow described accordingly.

---

## v406 — 2026-07-14

Every user-facing string introduced for the source-upload feature — the
Upgrade page's hint text, its two alert messages, three doc comments, and a
paragraph in `docs/UPGRADES.md` — said "`.tar.gz`" exclusively. The file this
project actually ships as (and that every tarball handed over this session
was named) is `.tgz`. Detection itself was never affected — `sniffAndStage`
and `extractSourceTarGz` go by the gzip magic bytes, not the filename, so a
`.tgz` always worked — but telling someone to upload a `.tar.gz` while
handing them a `.tgz` is exactly the kind of small mismatch that reads as
"this doesn't do what it says." Fixed everywhere: UI copy, error messages,
and doc comments now say `.tgz`/`.tar.gz` together and note they're the same
format, checked by content rather than extension.

---

## v405 — 2026-07-14

The Upgrade page's local-only-unsigned upload was two separate forms — a
binary picker with its own Stage button, and a second "or build from source"
row with its own file picker and two more buttons (Build & stage, Build &
install) — for what an operator experiences as one action: "I have a file,
get it running on this node." Collapsed to one file input and one primary
button (**Upload & install**), plus a quieter secondary one (**Upload only
(review first)**) for anyone who wants to look before restarting.

The server does the work the two-picker UI used to make the browser
responsible for: `sniffAndStage` peeks the first two bytes of the upload for
the gzip magic (`0x1f 0x8b`) and routes to the existing source-build path or
the existing raw-binary path accordingly, so `handleUpgradeStage`'s single
"artifact" field now accepts either interchangeably. `POST
/api/upgrade/stage-source` still exists for anyone scripting against it
directly and wanting to be unambiguous rather than relying on sniffing, but
nothing in the UI calls it anymore.

`localApply` takes an optional `skipConfirm` so the unified Upload & install
button's own upfront confirmation (covering upload, build, apply, and
restart as one described action) doesn't stack a second, redundant one right
after — the per-artifact Dry run / Apply here buttons in the table below are
unaffected and still confirm individually, since those are deliberate,
one-off actions taken well after something's already staged.

Signed mode (`trusted_keys` configured) is unchanged: binary + manifest,
two fields, Stage — there's a real reason those stay separate (the manifest
has to be verified before a byte of the artifact is accepted), so it wasn't
folded into the single-button flow.

---

## v404 — 2026-07-14

`docs/UPGRADES.md` still described v403's predecessor: mandatory
`trusted_keys`, mesh-wide distributed rollout, "Managed implies upgradable."
None of that has been true since v403 landed the local-only rework, and the
doc never got updated alongside the code — which is exactly the kind of gap
that sends someone down a wrong path (in this case: reading it and
concluding a release key was still required, when it hasn't been since
v403). Rewritten to match what the code actually does: no key needed by
default, local-only from any node regardless of Managed/Manager state, three
ways to get a binary staged (upload, build-from-source, or signed manifest if
`trusted_keys` is configured), and the Fleet/Rollout view called out
explicitly as present-but-inert rather than left to look like it still works.
No code changes this version — documentation catching up to v403's actual
behavior.

---

## v403 — 2026-07-14

Upgrades are now genuinely local-only, and no longer need a release key to
work at all. Both halves of that sentence are changes; neither was true this
morning.

**Local-only, actually.** v402 made `config.UpgradeAcceptsRemote()`
hard-locked `false`, but that check was only ever read inside
`handleUpgradeApply`. Every other upgrade endpoint —
`stage`, `local-apply`, `rollback`, `rollout`, `fleet`, `state`, and the
status page itself — had no equivalent check and relied solely on
`authed()`'s general Managed/Manager overlay bypass, the one that's correct
for the rest of the admin surface (firewall, routes, NAT, ...) precisely
*because* those are meant to stay peer-administrable. Upgrades opted out of
that model; the code just didn't say so consistently. A Manager peer could
still stage an artifact, apply one already staged, roll one back, or start a
rollout on a Managed node, entirely independent of what `AcceptsRemote`
reported. Fixed with one gate — `upgradeLocalOnly` — called first, before
even the "is this feature configured" check, by every handler in
`internal/webadmin/upgrade.go` except `handleUpgradeBlob` (bytes-only, its
own bespoke peer-facing auth, unchanged). `TestUpgradeEndpointsRejectOverlayBypass`
drives a simulated Manager-peer request at the whole surface and asserts 403;
it fails against the old code.

**No key required.** `UpgradeEnabled()` is `true` unconditionally now — the
feature no longer disappears behind "go run `gravinet upgrade genkey`" on a
fresh node. Whether an artifact needs a valid signature is answered
separately, by `Store.Verify`: with `upgrade.trusted_keys` configured, it's
the exact old behavior (signature-checked, no exceptions); with none
configured, a manifest only has to be structurally sound, since the only door
into the store (`Ingest`) is now local-only anyway — there's no peer this
could ever be exposed to. This is a real trust decision, not an oversight:
the [gravinet] project ships as source only, with no separate prebuilt
release artifact, so requiring a signature to use the upgrade feature at all
meant every fresh install needed a keypair before it could ever be used
locally, for a channel that (as of the point above) no peer could reach
regardless.

**GUI upload, three ways, all local-only-unsigned only:**
- **Binary, no manifest.** `handleUpgradeStage`'s "artifact" part no longer
  requires a preceding "manifest" part when the node trusts no keys — the
  binary is streamed to disk, identified with the same `ProbeBinary`
  (`gravinet version`) introspection `upgrade sign` already did on a build
  host, and ingested unsigned.
- **Source, built here.** New `POST /api/upgrade/stage-source`: upload the
  project's own `.tar.gz`, and the node runs the same `go build` the platform
  installers already run (cgo/PAM first, falling back to a static build the
  same way `install-linux.sh`'s `build_from_source` does) before probing and
  ingesting the result. Does not attempt to install a missing Go toolchain or
  C/PAM headers itself — fetching and installing a toolchain from an HTTP
  handler is a materially bigger capability than "compile the source you
  handed me," so a missing `go` on PATH fails with a clear message instead of
  the daemon reaching out to a package manager on its own initiative.
  Extraction (`extractSourceTarGz`) rejects symlinks/hardlinks and any entry
  resolving outside the destination directory before writing anything, and
  caps decompressed size independently of the gzip header. Both signed mode
  and unsigned mode reject this endpoint outright when `trusted_keys` is
  configured — there's no coherent way to check a signature against uploaded
  source, only a built binary's digest, so offering it under a node that
  otherwise promises signed provenance would be a hole in that promise, not
  an extension of it.
- **Build & install**, one click: stage-from-source immediately followed by
  the same local-apply path the per-artifact "Apply here" button uses,
  confirm dialog included.

Neither upload path is offered — client-side, and the server independently
enforces the same split — once `trusted_keys` is configured; a node that
opted back into signing gets exactly the old signed-manifest-first flow.

**Also:** `Store.List()`'s re-verify pass, `Ingest`, and the new stage
handlers all route through the same `Store.Verify`, so the unsigned-vs-signed
decision can't drift between them. Fixed a real (if previously unreachable in
practice) panic: `handleUpgradeHome` sliced `m.Signer[:16]` unconditionally,
which indexes out of range on an empty `Signer` — every unsigned manifest.
Moved the Upgrade nav item from the Mesh group to Info, since it's a
per-node action now, not a mesh-wide one; updated its copy accordingly. The
Fleet/Rollout cards are left in place (useful if this policy ever changes
back) with an explicit notice that a rollout will stop at the first peer,
immediately, since every node now refuses it.

---

## v402 — 2026-07-14

Removed `upgrade.allow_remote`. **If a node is Managed, it is upgradable.**

The switch was introduced in v399 on the reasoning that being remotely
*manageable* (config changes) and remotely *upgradable* (code changes) are
separate decisions an operator is entitled to make separately. That reasoning
does not survive contact with what Managed mode already grants: a Manager peer on
a Managed node can rewrite its config, change its firewall, and restart it, and
where the remote shell is enabled it has a root PTY on it. A bool refusing that
same peer permission to replace the binary is a locked door beside an open one.
It stops nobody hostile.

And it was not free. A Managed node with `allow_remote: false` still appeared in
a rollout as a target — it is Managed, so `ManagedPeers()` returns it — and then
refused the apply when its wave came up. That is a scheduled failure: it stops the
rollout, splits the fleet, and does so for a "protection" that protects nothing.
The feature's only observable effect was to break rollouts on behalf of an
operator who thought they were hardening one.

What constrains an upgrade is not a bool, it is a key. An artifact must be signed
by something in `trusted_keys`, whose private half lives offline; that is a
boundary a compromised manager genuinely cannot cross, and it is the one doing all
the work. So the honest way to say "manageable, but never code-swapped from the
network" is to **trust no release key** — the default, failing closed, and immune
to manager compromise in a way a config bool never was. Such a node keeps being
upgraded however you upgrade it today.

- `config.Upgrade.AllowRemote` is gone; `UpgradeAllowRemote()` becomes
  `UpgradeAcceptsRemote()` = Managed && has trusted keys.
- The peer-facing `/api/upgrade/apply` now refuses only what authed() would
  already have refused, and says so in those terms: not in Managed mode → does not
  accept administration from peers, upgrades included.
- Configs carrying the old key still load (unknown fields are ignored); the field
  is simply no longer consulted. It shipped and was removed the same day, so no
  live install can be relying on it.

---

## v401 — 2026-07-14

Closed a hole in v399/v400's rollout that would have quietly left part of a mesh
behind, and made the two "who can drive / who can be driven" rules visible
instead of implicit.

**The bug.** A rollout's targets came from `Engine.ManagedPeers()`, which filters
out every node not advertising Managed mode. So a peer sitting right there on the
mesh, connected, visible in the peers table, but not Managed, was not a node the
rollout *skipped* — it was a node the rollout **could not see**. Six of nine nodes
would go green and the rollout would report complete success, with three left on
the old binary and nothing anywhere saying so.

This is precisely the outcome the `-skip-unreachable` rule already existed to
prevent for nodes that are merely offline ("seven of your ten are upgraded" is a
fleet in two versions, and the operator should have to say that out loud). There
was no principled reason an un-Managed node should slip past the same rule just
because its absence is a config setting rather than a dead link — same
consequence, same requirement to acknowledge it. So now:

- a rollout **refuses to start** if any peer on the mesh is un-Managed, naming
  them, and offering the three real ways forward (set `managed` on those nodes,
  upgrade them locally, or `-skip-unmanaged` to accept a split fleet);
- the fleet view **lists them anyway**, greyed, with `not managed` as their phase.
  A fleet table that quietly shows six rows for a nine-node mesh is not merely
  incomplete, it is confidently wrong in the one direction that matters;
- `NodeState` grew a `manageable` flag so the distinction survives the wire.

**The two rules, now stated on the surface rather than discovered by clicking.**
You roll out *from* a Manager-mode node (an outbound act, same boundary as every
other thing one node does to another) *to* Managed-mode nodes (which is what gives
a manager an admin surface to drive at all). On a non-manager the Upgrade tab
still does everything inbound — stage, dry run, apply here, roll back — and the
rollout form is greyed with the reason on its face instead of failing on click.
The rollout card also now shows its coverage before you press anything: how many
peers would be upgraded, how many would not, and which.

For a node you want manageable but not remotely *upgradable*, `managed` on plus
`upgrade.allow_remote: false` remains the answer: config changes yes, code changes
no. Such a node is still named as a target and refuses the apply itself — loud
rather than silent, which is the point.

New flag: `gravinet upgrade rollout -skip-unmanaged` (and the matching checkbox).

---

## v400 — 2026-07-14

A web-admin tab for the upgrade machinery added in v399, which until now was
CLI-only. **Mesh → Upgrade**.

The tab is laid out in the order the decisions are actually made, which is also
the order in which they can be made *safely*: what this node is running, what it
has staged, what the **fleet** is running, and only then the control that changes
any of it. The fleet table sits above the rollout form rather than beside it for
the same reason the package puts the canary first — you cannot sensibly roll a
binary out to ten nodes until you can see all ten, and on a mesh that view is
otherwise ten SSH sessions.

- **This node** — running version, guard phase, live peer count, the binary path
  that would be replaced. A **Roll back** button appears whenever a backup binary
  exists, *including after an upgrade committed cleanly*: the automatic guard only
  catches what it can see (a crash loop, a node that never rejoins), and the
  regression a health check has no opinion about is precisely the one a human
  finds an hour later.
- **Staged artifacts** — a table, plus an upload for a build and its signed
  manifest. The upload is streamed and the manifest is read *first*, so the
  binary is verified against a signature as it lands rather than after it is
  already sitting somewhere it could be executed from; the parts arrive in that
  order and the handler refuses them in any other. Per row: **Dry run** (verify,
  execute, and config-test the binary here, changing nothing) and **Apply here**.
- **Fleet** — every managed peer's version and phase. A peer reading REVERTED here
  took an upgrade, failed to come back healthy, and backed itself out on its own
  initiative.
- **Rollout** — artifact, canary size, batch size, dry-run/skip-unreachable/
  include-self, and a live wave-by-wave progress table.

Two things about it are deliberate and worth stating, because both are the kind
of thing that looks like an oversight until it isn't:

**The rollout runs in the daemon, not the browser.** The form kicks it off and
then polls; closing the tab doesn't stop it. It has to work this way — the last
thing a rollout does is upgrade the manager itself, which takes down the very
HTTP server that would have been driving it.

**The rollout form is never proxied to the peer selected in the header.**
`/api/upgrade/{rollout,fleet,stage}` are local-only, enforced in handleProxy and
not merely in the client. The per-node endpoints (state, local-apply, rollback)
stay proxyable on purpose — pointing the header at a peer to read its upgrade
state is exactly what that picker is for — but orchestration doesn't: asking peer
B to orchestrate a rollout of *B's* staged artifact across *B's* view of the mesh,
with a canary the operator didn't choose, while the browser sat on node A
believing it was driving, is the same class of bug as the Managed/Manager toggles
silently applying to the wrong node (see LOCAL_API's comment, and v398's).

Everything the tab does goes through the same daemon entry point the CLI reaches
over the control socket. There is no second implementation of a rollout to drift
out of step with the tested one: a rollout started from this form has the same
canary, the same "did it come back on the mesh" success test, the same abort
rule, and the same self-last ordering as one started from a terminal.

New endpoints: `/api/upgrade` (node summary + staged list), `/api/upgrade/fleet`,
`/api/upgrade/rollout` (POST starts, GET polls), `/api/upgrade/local-apply`,
`/api/upgrade/stage` (multipart).

---

## v399 — 2026-07-14

Added a way to distribute and apply a new [gravinet] binary across every node of
a mesh, over the mesh itself. New package `internal/upgrade`, new `gravinet
upgrade` command, new `upgrade` config section, three new peer-facing web-admin
endpoints. Full operator docs in `docs/UPGRADES.md`.

The design problem here isn't copying a file to ten machines — it's that
[gravinet] *is* the network you'd otherwise use to repair a machine you just
broke. A bad binary on ten peers takes down the overlay you'd need to push the
fix, and a node reachable only over a mesh it can no longer join is a node you
visit in a car. Everything below follows from that.

**A third trust boundary.** The mesh PSK gets you onto the overlay; Manager mode
lets you drive another node's admin API; neither is sufficient to *replace the
binary a node executes*. That takes an Ed25519 signature from a key in the
node's `upgrade.trusted_keys`, whose private half lives offline and never
touches a mesh node. So compromising a manager — or a node's entire config —
still doesn't get you code execution on the fleet. With no trusted keys
configured a node refuses every upgrade; it fails closed, so an unconfigured
node can't be talked into running the first binary a peer offers it.

**Distribution is pull-based and peer-to-peer.** The manager sends each node the
signed *manifest* (a few hundred bytes) plus a list of peers that hold the
artifact; the node pulls the binary itself, from the nearest holder, over the
overlay. Every node that completes becomes a source for the ones behind it, so
ten nodes don't drag ten copies through the manager's uplink, and a rollout
doesn't collapse if the manager goes away halfway through. Nothing about a
source is trusted — the signature and digest are checked against the bytes that
actually landed, regardless of who served them — so the worst a hostile peer can
do is waste bandwidth. The blob endpoint is served to any peer on the overlay
rather than only to Managers, deliberately: requiring Manager mode would collapse
the fanout back into a star, and what's being served is a signed release binary
the fetcher verifies independently anyway.

**Nothing is swapped in until it has been run.** A digest proves you have *the*
binary; it says nothing about whether that binary works *here*. Before either
rename, on the target's own filesystem, the candidate is executed (`gravinet
version`) and made to load this node's actual config (`gravinet selftest`, a new
subcommand added for exactly this). That catches the failures that actually
brick fleets and that a digest cannot see, because the digest of the *wrong*
binary is perfectly valid: the arm64 build sent to the amd64 boxes, the download
truncated by a proxy, the dynamically-linked binary whose libpam isn't there,
and — nastiest, because it passes every other gate — the new version that
tightened config validation and will crash-loop on a node whose only management
path is the mesh it's now failing to join. A silent `pam=yes` → `pam=no`
downgrade is also refused: it starts cleanly and then can't log anyone in, which
is the kind of failure a human discovers at 3am. The swap itself is two renames
on one filesystem, so there's no window where a half-written binary sits at the
installed path.

**Nodes rescue themselves.** The manager can't help a node that has fallen off
the mesh, so it isn't asked to. Each node writes a guard record before the swap:
a boot counter (incremented before anything that can fail, so a crash-looping
binary still gets counted) and a snapshot of how many peers it had. Three failed
starts, or a confirm window that closes with zero peers when it had four,
restores `gravinet.prev` and restarts into it — with no manager, no mesh, and no
operator involved. `gravinet upgrade rollback` covers the other case, the
regression no health check has an opinion about; the backup is kept even after a
successful commit for precisely that reason.

**Rollouts are canary-first and stop dead.** One node, then batches; a node
counts as upgraded only when it comes back *on the mesh, on the new version,
reporting healthy* — not when it acknowledges the command. The first node that
doesn't come back stops the rollout, and the nodes behind it are never touched.
The manager upgrades itself last, once every peer is known good. `-dry-run`
preflights the whole fleet without swapping anything; refusing to start when
nodes are offline is the default, because "seven of your ten nodes are upgraded"
is a fleet in two versions and the operator should have to say that out loud.

New CLI: `upgrade genkey|sign|stage|list|status|apply|rollback|fleet|rollout`,
plus the `selftest` subcommand the preflight depends on. New config:
`upgrade.{trusted_keys,store_dir,allow_remote,serve_to_peers,confirm_seconds,keep_artifacts}`.
New endpoints: `/api/upgrade/{state,blob,apply,rollback}`.

Known gap: there's no web-admin UI tab for this yet — the API surface exists and
the peer-facing endpoints are what a rollout drives, but the operator-facing
driver is the CLI. Also, bootstrapping is unavoidable: a node on v398 has no
`/api/upgrade/apply` to drive, so the first hop onto v399 has to happen by
whatever means you use today. From v399 on, upgrades ride the mesh.

---

## v398 — 2026-07-13

Finished the firewall parity work from v392 by bringing its five features into
the web admin — until now they were reachable only through the config file and
the control-plane API.

Two new sub-tabs under Firewall:

- **Objects** — the per-network address-object catalog. Add/edit/remove named
  objects of kind host, subnet, range (`a-b`), fqdn (re-resolved live), or group
  (a bundle of other objects). Double-click a cell to edit; the whole catalog
  saves live via `/api/firewall op:objects` with no restart.
- **Services** — the per-network service catalog. Ports are written compactly as
  `udp/53, tcp/53, tcp/8000-8100` and parsed into the leg list; a bare proto
  (e.g. `icmp`) matches any port.

The Rules table gained the columns the catalog makes meaningful:

- **services** — a rule can now name catalog services (comma-separated), with a
  `<datalist>` autocomplete of the network's service names.
- **src/dst** now autocomplete against the object catalog too, so you can type an
  object name instead of a raw CIDR (either still works).
- **log** — a per-rule checkbox; matches emit a rate-limited log line (v392).
- **hits** — the live packet counter (bytes in the tooltip), with a *reset
  counters* button per network. Counts come from the live rules feed
  (`FirewallRules`), which already carried them.

Plumbing: `internal/webadmin/webadmin.go` adds three firewall ops (`objects`,
`services`, `reset-counters`) dispatching to five new `Backend` methods
(`FirewallObjectsList`/`SetFirewallObjects`, `FirewallServicesList`/
`SetFirewallServices`, `FirewallResetCounters`) — all already implemented on the
engine since v392. `internal/webadmin/ui.go` adds the two editors, the rule-row
columns, the datalists, and the counter-reset control. Tests cover the new ops
(objects/services applied to the backend, counters reset, all live/no-restart);
the embedded JS passes `node --check`; existing firewall/webadmin/config suites
are unchanged and green.

That closes the follow-up flagged in v392: all five firewall features are now
usable end to end — config file, control-plane API, and the web console.

---

---

## v397 — 2026-07-13

Two RHEL-family fixes: the Linux installer now opens gravinet's ports in
firewalld, and enables systemd-resolved — without which DNS forwarding cannot
work on RHEL/Rocky/Alma/CentOS at all.

**1 — firewalld holepunch (`install-linux.sh`).** The installer never touched the
host firewall, but firewalld runs by default on RHEL, Rocky, Alma, CentOS and
Fedora and silently drops the inbound packets gravinet needs. It now opens, in the
default zone:

- the underlay port on **udp and tcp** (default 65432) — udp is the primary
  transport, tcp is the fallback the TLS transport uses where udp is blocked;
- the web admin port on **tcp** (default 8443) — not for browsers (it binds
  127.0.0.1 by default) but for *overlay* traffic: a Manager peer proxies /api
  calls, and the speedtest streams, to this node's web admin across the mesh
  interface, which lands in the default zone like any other inbound packet.

Ports are read from the config, not assumed, so a host that moved the underlay off
65432 gets *its* port opened. Idempotent (a second run adds nothing and skips the
reload), reloads firewalld (`--permanent` alone writes the config and changes
nothing live), no-ops cleanly when firewalld is absent or stopped, and
`--uninstall` closes exactly what it opened. `--no-firewall` skips the step and
prints the ports to open by hand.

The step keys off *state* ("is firewalld running?") rather than sniffing
/etc/os-release, which is both more accurate and automatically right on the hosts
that don't fit the pattern — a Debian box running firewalld gets its ports opened
too.

**2 — systemd-resolved (`install-linux.sh`, `internal/resolver`).** On
RHEL/Rocky/Alma/CentOS every DNS sync failed with:

    mesh: dns sync (net ..., iface mesh0): resolver: set dns on mesh0:
    resolvectl dns mesh0 ...: exit status 1:
    Failed to set DNS configuration: The name is not activatable

gravinet's Linux DNS forwarding is implemented *only* via systemd-resolved's
per-link routing domains (`resolvectl`), and those distros don't enable it by
default — NetworkManager writes /etc/resolv.conf itself. `resolvectl` nevertheless
ships in the base systemd package, so the existing `exec.LookPath` check passed on
every systemd host and the failure surfaced later, from D-Bus, as a message that
names neither systemd-resolved nor any remedy. ("The name" is the D-Bus bus name
`org.freedesktop.resolve1`; "not activatable" means nothing is installed to answer
it.)

- **The installer now enables it**, mirroring what install-freebsd.sh already does
  for local-unbound: installs the package if missing (RHEL 9+/Fedora ship it split
  out of systemd), enables and starts it, points /etc/resolv.conf at its stub, and
  drops a NetworkManager `dns=systemd-resolved` conf.d file — without that last
  step NM clobbers the symlink on the next connection change and DNS forwarding
  breaks again later with nothing obviously to blame. Skipped entirely when
  resolved is already active (Debian/Ubuntu/Fedora). `--no-systemd-resolved` opts
  out, and prints what to run if the feature is wanted later.
- **The daemon now explains the failure instead of relaying a D-Bus string.**
  `explainResolved` wraps exactly this failure with the cause, why it's these
  distros, the four commands that fix it, and the opt-out (turn DNS forwarding off
  — nothing else in gravinet depends on systemd-resolved). A genuine per-link error
  (unknown interface, invalid domain) is passed through untouched rather than
  buried under advice about a service that's working fine.
- **`Clear` no longer errors when resolved is down.** If it isn't running it cannot
  be holding per-link state to revert, so teardown (network disable, reload,
  shutdown) stopped reporting a failure to undo something that was never applied.
- **The warning is logged once, not every tick.** The generic DNS-sync error path
  deliberately doesn't record `lastDNSSig` — it must retry, so an operator's later
  fix takes effect without a daemon restart — which meant it re-logged the
  identical warning every maintenance tick, forever, for a condition only a human
  can clear. It now shouts once, in full, then stays quiet (debug level) until the
  error changes or clears; the retry continues silently either way. New
  `netState.lastDNSErr` memo, cleared on success so a recurrence after a recovery
  is news again.

**Tests.** `resolver_linux_diag_test.go` (the real RHEL error string is recognised;
genuine per-link errors are not; the explanation names cause, distros, cure and
opt-out, and still wraps the original), `dnssync_logdedup_test.go` (a persistent
failure warns exactly once across 20 ticks; a changed error warns again; recovery
re-arms it; the memo is per-network). The installer's firewalld/resolved helpers
are driven against stubbed `firewall-cmd`/`systemctl`/`dnf` — 20 assertions
covering port derivation from config, idempotence, reload-only-when-changed,
firewalld absent/stopped, uninstall symmetry, and each resolved state. shellcheck
clean.

**Docs.** getting-started.md gains the firewalld note in the seed-node section and
a systemd-resolved requirement box in the DNS-forwarding section.

## v396 — 2026-07-13

Web admin: the speedtest's source/target pickers now use the same dropdown as the
header's node picker, with the filter at the top of the list. These were the last
two native `<select>`s with a filter input parked beside them — the pattern v395
removed from the header.

Rather than copy the header's listbox a second time, it's been extracted into
**`buildListPicker(cfg)`**, now shared by all three pickers. `buildPeerPicker` is
a thin wrapper over it. The component gained what the speedtest needs:

- **Disabled options.** The two speedtest pickers exclude each other (a node can't
  be both endpoints of a test against itself). The `<select>` version did this by
  flipping `option.disabled` on the other box; the same idea is now an item's
  `disabled` flag, which the listbox renders grayed, refuses to pick, and *steps
  over during keyboard navigation* — the cursor never lands on something that
  can't be chosen.
- **A real selection model.** `setItems` / `getValue` / `setValue` / `count`,
  with the selection kept independent of the filter and a placeholder label when
  a pool is empty (e.g. no peer is in Manager mode). A selection that vanishes
  from the list falls back to the placeholder rather than silently pointing at a
  node that's gone.
- **Per-picker filter state.** `state.clusterFilter` is gone from the global app
  state — three pickers can't share one filter string. Each owns its own, cleared
  when its list closes.

Preserved from the `<select>` version: the client pool is Manager-mode peers only
(a merely-Managed peer is guaranteed a 401 as the initiator — see the function's
comment), the target pool is every manageable peer, the target still defaults to
the first peer rather than this node, Run is still disabled when no peer can
initiate, and the collision fallback (a pick colliding with the other box shifts
that box to the first node it can still hold) is still there — though with the
colliding option now correctly grayed, it's a safety net rather than a path a
click can reach.

Also fixed: a backtick in a JS comment I'd written inside `indexHTML` — which is
a Go **raw** string literal, so the backtick terminated it mid-file. Caught at
compile time, and now covered by a test naming the trap explicitly, since a stray
backtick surfaces as a syntax error hundreds of lines away from the cause.

**Tests.** `internal/webadmin/peerpicker_test.go` gains: the speedtest builds no
native `<select>`, both its pickers are `buildListPicker`s, exclusion is expressed
as disabled options, disabled options are inert in all three code paths (pick,
handler binding, keyboard stepping), and `indexHTML` holds no stray backticks. The
components were also driven in a headless DOM (jsdom) against the real extracted
UI code — 22 assertions across the speedtest pair (pool membership, per-list
filter-row threshold, defaults, mutual exclusion, gray-out following each pick,
keyboard skipping, filter-then-pick) and 28 for the header picker.

## v395 — 2026-07-13

Web admin: the header's node picker filter now lives at the top of the dropdown
list instead of beside it. Open the picker, type, the options below narrow.

The filter used to be a separate `<input>` sitting next to the picker in the
header — two controls that read as unrelated, with the filter occupying header
width whenever the mesh was large enough to show it, whether or not you were
using it. It couldn't have gone anywhere else: the picker was a native
`<select>`, whose option list is an OS-drawn popup that no markup can be placed
inside. So the picker is now a hand-rolled listbox (`buildPeerPicker`), which is
the only way the filter can sit at the top of the list it filters. It reuses the
`.ss-*` styling the global search box already established rather than inventing a
third dropdown look.

- **Structure.** A button showing the node currently being configured, plus a
  dropdown whose first row is a sticky filter input, options underneath. The
  filter row is hidden below `DROPDOWN_FILTER_MIN` (10) options, exactly as
  before — for a handful of peers a filter is pure clutter.
- **Behavior preserved.** Filtering only narrows what's visible/pickable; it
  never changes `state.target`. Typing a filter that excludes the currently
  targeted node doesn't silently switch the GUI back to local — the button keeps
  naming the real target throughout, and the option reappears when the filter
  stops excluding it. Matching is still case-insensitive against the display name
  *or* the raw node id, so it works for a peer that hasn't announced a hostname.
  The Manager-mode gating and the synchronous `syncClusterModeRows()` call on
  switch (see its comment — it closes a window where a stale toggle could flip
  *this* node's mode) both carry over intact, now in `setPeerTarget`.
- **Filter clears on close**, so the next open starts from the full list instead
  of a stale query you've since forgotten you typed. A background `/api/cluster`
  poll landing mid-search re-applies the in-progress filter to the fresh options
  rather than resetting the list under you.
- **Keyboard + a11y.** Enter/Space/ArrowDown opens, arrows move, Enter picks,
  Escape closes and returns focus to the button; clicking outside closes.
  `role="listbox"`/`role="option"`/`aria-expanded`, and the current node is marked
  in the list.
- Two bugs found and fixed while testing this in a real DOM: the button label
  didn't update until the next `/api/cluster` round trip came back (so the header
  briefly kept naming the node you'd just switched *away* from), and the caret was
  written as a `\u` escape inside `indexHTML` — a Go **raw** string, which does no
  escape processing, so it would have rendered as literal text.
- **Tests** (`internal/webadmin/peerpicker_test.go`): the picker is not a native
  `<select>`; the old beside-the-select `.peer-filter` input is gone; the filter
  row is appended to the list *before* the options container (that ordering is the
  feature); and `indexHTML` contains no raw-string unicode escapes.

Not converted: the speedtest source/target pickers, still native `<select>`s with
a filter beside them (their exclusive-selection logic makes them a bigger job).
That's the obvious follow-up if this shape is right.

## v394 — 2026-07-13

Fixed the *other half* of v393: the stale `/run/gravinet.sock` frozen inside
existing `config.json` files, which is why macOS was still broken after v393 and
why FreeBSD had a `/run` directory at all.

v393 made the CLI read `control_socket` from the config instead of assuming the
platform default — correct, and it made the CLI faithfully dial whatever the file
said. On macOS the file said `/run/gravinet.sock`, so that's what it dialed, and
the symptom survived with a new path in it.

**Where that value came from.** `Default()` wrote the then-current
`DefaultControlSocket` into the scaffolded config (`install-*.sh` runs
`gravinet run -init`). Every box installed *before* the `/run` -> `/var/run`
correction therefore has the old Linux-only path frozen in its config file, where
it outranks the corrected code default forever. Fixing the default never reached
an existing install; only a fresh scaffold saw it. The same stale value then
produced two different symptoms, which is what disguised it as two bugs:

- **macOS** — `/` is a read-only APFS system volume, so `control.Serve`'s
  best-effort `MkdirAll` couldn't create `/run`, `net.Listen` failed, and the
  daemon logged a single warning and carried on serving the mesh. The socket was
  never created, so the first visible sign was the CLI, much later, reporting
  "no such file or directory".
- **FreeBSD** — `/` is writable, so that same `MkdirAll` *succeeded*: the daemon
  **manufactured a top-level `/run` directory** the OS doesn't ship and bound the
  socket inside it. It worked, which is precisely the problem — the bad path
  looked like the right one, and the stale config entrenched itself.

Changes:

- **`NormalizeControlSocket()`** (`internal/config/socket_normalize.go`) is now
  the single resolver for both ends. Empty -> platform default. The legacy
  `/run/gravinet.sock` on any platform whose default *isn't* that (i.e. anything
  but Linux) -> platform default, with a logged note. Everything else — a custom
  path, a `host:port` TCP endpoint, a deliberate `/run/other-name.sock`, or `/run`
  on Linux where it's genuinely correct — is honoured verbatim. The daemon
  (`cmdRun`), the CLI (`defaultControlSocket`), and `reloadDaemon` all resolve
  through it, so binder and dialer cannot disagree, whatever the file holds.
- **Scaffolded configs no longer name a socket at all.** `Default()` leaves
  `ControlSocket` empty and the field is `omitempty`, so "follow the platform
  default" is a live decision made at runtime rather than a value frozen into
  JSON — this class of bug can't recur on the next correction, and a config
  copied between platforms stays right. Explicitly setting it still pins it.
- **`control.Serve` won't invent a top-level directory.** It still creates the
  socket's own leaf dir (e.g. `/var/run/gravinet/`), but only under a parent that
  already exists, so it can no longer fabricate `/run` at `/`. Both platforms now
  fail the same honest way instead of one of them quietly "working".
- **A failed control-socket bind is an error, not a warning.** The daemon now
  says at error level that the socket couldn't be created, that every control
  command will fail until it's fixed, that the mesh itself is unaffected, and
  which config key to change. The old lone `Warnf` is how this stayed invisible:
  the daemon started, the mesh came up, and the only symptom surfaced somewhere
  else entirely.
- **Tests** (`socket_normalize_test.go`, `serve_mkdir_test.go`): legacy value
  migrated off-Linux and preserved on Linux; deliberate paths and TCP endpoints
  untouched; resolution deterministic across both ends; scaffolded config
  serializes no `control_socket`; `Serve` creates a leaf dir under an existing
  parent but refuses to fabricate one under a missing parent.

**On upgrade** the stale key is ignored, not rewritten — nothing edits the file
behind your back. To clear the warning, delete the `control_socket` line from
`config.json` (or set it to the path you actually want).

## v393 — 2026-07-13

Fixed the CLI dialing a different control socket than the daemon binds, so any
config that set `control_socket` broke every control command (`status`, `ban`,
`managed`, `network`, `fw`, `key`, ...) with a misleading error.

The daemon binds `cfg.ControlSocket` from the config file and only falls back to
the platform default when that field is empty (`cmdRun`). The CLI subcommands
defaulted `-sock` straight to `config.DefaultControlSocket` and **never opened
the config at all**. So the moment `control_socket` was set to anything other
than the platform default, the two ends silently disagreed: the daemon listened
on the configured path while every CLI command dialed the platform default and
died with

    control: dial unix /var/run/gravinet.sock: connect: no such file or directory

The error names a path nothing is listening on and gives no hint that a
*different* path is in the config — so it reads exactly like "the daemon isn't
running", and (having done so at least once before, see v182 and the comment in
`internal/config/socket_bsd.go`) it invites "fixing" the platform default
instead, which would re-break the platform for everyone whose config *doesn't*
override it.

- **CLI now resolves the socket the same way the daemon does** — new
  `defaultControlSocket()` (`cmd/gravinet/cli_sock.go`): config's
  `control_socket` first, platform default second. Both ends agree by
  construction, on every platform. `$GRAVINET_CONFIG` overrides the config path
  for the control commands, which (unlike `run` and the config-editing commands)
  have no `-config` flag of their own. A missing/unreadable/invalid config is not
  an error here — those commands talk to a running daemon rather than validating
  config, and the platform default is the right guess anyway. `-sock PATH` still
  wins over both.
- **`reloadDaemon()`** (`cli_config.go`) uses the same resolver for its
  empty-string fallback, so a `config set` that edits a daemon on a custom socket
  now reloads it live instead of reporting "daemon not reachable — changes apply
  when the service starts".
- **The dial error now says what actually went wrong**: the socket is created by
  the daemon at startup, so a missing one means either the daemon isn't running
  (with the per-platform service command to check) *or* it's listening elsewhere
  (naming the config that decides it, and `-sock`). The bare syscall error only
  ever named the path.
- **Regression tests** (`cmd/gravinet/clisock_test.go`): config value preferred;
  empty value falls back to the platform default; missing config doesn't fail;
  the hint names both causes. Plus a guard pinning the BSD default to
  `/var/run/gravinet.sock` — `/run` is a Linux (systemd/FHS) convention that does
  not exist on a stock FreeBSD, macOS or OpenBSD, and `control.Serve` fails at
  daemon startup if it regresses there.

Unchanged: the platform defaults themselves (`/run` on Linux, `/var/run` on
BSD/macOS, a named pipe on Windows, `/tmp` elsewhere).

## v392 — 2026-07-13

Firewall parity pass, porting the five most useful ideas from parapet's policy
engine into gravinet's overlay firewall. gravinet's firewall was already ordered,
stateful, and live-editable; this adds the manageability and observability layer
it was missing. The internal rule model was generalised from single
prefix/proto/port fields to address-sets + service-legs, carrying the authored
form verbatim for faithful round-trip, so every feature below shares one compile
path (`compileRule`) and one recompile point (`firewall.store`).

**1 — Named address objects + groups.** A rule's `src`/`dst` may now be the name
of a reusable object instead of a raw CIDR: `host` (one or more literals),
`subnet` (CIDRs), `range` (`a-b`, decomposed to exact covering prefixes), or
`group` (a recursive, cycle-safe bundle of other objects). Define membership
once, reference it everywhere; edit the object and every referencing rule
recompiles. An empty-but-constrained object (e.g. an unresolved FQDN) matches
*nothing*, never silently widens to "any" — tracked via explicit any-flags.

**2 — Service catalog.** A named service (e.g. `DNS` = udp/53 + tcp/53) is
referenced by a rule's `services` list and unioned with any inline proto/ports.
This finally lets one rule express a multi-leg service, which the old single-leg
model couldn't.

**3 — Per-rule hit counters.** Every rule carries packet/byte tallies, surfaced
in the rule listing and resettable (`FirewallResetCounters`). The tally lives on
a pointer so it survives reordering and catalog recompiles instead of resetting.
Notably simpler than parapet's equivalent, which needs elaborate baseline
bookkeeping to survive nftables table rebuilds — gravinet's ruleset is an
in-memory snapshot, so the counter just persists.

**4 — Per-rule logging.** A rule with `log` set emits a one-line record of each
match (rule id, direction, 5-tuple), rate-limited per rule (default one line / 2s,
with a suppressed-count on the next line) so a busy flow can't flood the log.
Previously a dropped packet was silent; now "why is this blocked?" has an answer.

**5 — FQDN objects.** An `fqdn` object holds domain names, re-resolved every 60s
by a maintenance-tick task (and immediately on config reload); when the resolved
set changes the rulebase recompiles live. A total DNS failure keeps the last
known set rather than blanking the policy. The resolver is an injectable
interface so it's testable without real DNS.

Files: `internal/mesh/firewall.go` (rule model, catalog compile, counters,
logging, object/service/counter control-plane API), `internal/mesh/firewall_fqdn.go`
(resolver), `internal/mesh/engine.go` + `control.go` + `reload.go` (build/reload
wiring, periodic resolve), `internal/config/config.go` (config types +
`validateFirewallCatalog`), `cmd/gravinet/main.go` (config↔spec conversion and
persistence of the catalogs). Tests: object/host/subnet/range/group (incl. a
self-referential group and exact range decomposition), multi-leg services,
counters (accumulate, survive recompile/reorder, reset), rate-limited logging,
and FQDN resolution (empty-before-resolve matches nothing, then denies the
resolved address); plus config round-trip and catalog validation. All existing
firewall tests pass unchanged except literal-constructed rules in three tests,
updated to the spec-based builder.

**Scope note — engine and config, not the web UI.** All five features are fully
functional through the config file, the live reload path, and the control-plane
API (objects/services/counters have `Engine` methods ready for the CLI or web
admin to call). Wiring a drag-and-drop object/service editor into the embedded
web console (`internal/webadmin/ui.go`) is intentionally left as a follow-up —
it's a large front-end change with no bearing on the firewall's behaviour, which
is complete and tested here.

---

---

## v391 — 2026-07-13

The roaming issue (see v390, v388) has resisted incremental fixes for a while,
so this adds the sledgehammer that was asked for: **when the daemon detects its
own underlay source address changed — a Wi-Fi/cellular roam — it restarts
itself**, forcing a from-scratch re-establishment of every peer, socket, and
route instead of trying to heal in place. Blunt, deliberately so.

It reuses the clean-restart machinery already built for sleep/resume rather than
inventing a second one. `checkUnderlayChange` already does the in-process
mitigation (drop every path MTU to the floor, re-assert each network's OS state);
it now also, after that, requests the same restart the suspend/resume hook does.

- `internal/mesh/engine.go`: `SetUnderlayChangeHook` / `notifyUnderlayChange`,
  parallel to `SetSuspendResumeHook` / `notifySuspendResume`. One-shot per engine
  (`sync.Once`), and gated by `underlayRestartGrace` (45s): a change seen within
  the first 45s of a process's life is logged and skipped *without* consuming the
  one-shot. Combined with checkUnderlayChange's existing first-observation rebase
  (a fresh boot never counts its first reading as a change), this is what stops a
  flapping link from turning into a restart loop — under continuous flapping the
  service restarts at most about once per grace window, not once per flap. New
  `Engine.startedAt` anchors the grace check.
- `internal/mesh/pmtu.go`: `checkUnderlayChange` calls `notifyUnderlayChange()`
  after the in-process recovery it already did.
- `cmd/gravinet/main.go`: installs the hook (feeding the existing
  `restartRequested` path) when enabled. The restart handler now falls back to
  the platform **service manager** (`service.Restart()` — Restart-Service /
  systemctl / rcctl) when the in-place re-exec `selfRestart` isn't available,
  which is what finally makes this work on a Windows service; the re-exec still
  handles interactive Unix. This also, as a side effect, gives the sleep/resume
  restart a working path on Windows, which `selfRestart` never had.
- `internal/config/config.go`: `restart_on_underlay_change` (`*bool`,
  nil/true = on), with `RestartOnUnderlayChangeEnabled()`, mirroring
  `pmtu_discovery`. **Default on** — that's what was requested — set it to
  `false` to disable. Applied at engine construction, so toggling it live needs a
  restart to take effect.
- Tests: fires-once, nil-hook no-op, and the grace guard (muted inside the
  window, then fires once past it).

Two honest caveats. **It's default-on for everyone, not just the roaming node** —
if that's too aggressive as a global default, flip the nil/true to nil/false in
`RestartOnUnderlayChangeEnabled` and it becomes opt-in. And a restart is a
genuine connectivity blip: every session drops and re-handshakes, so on a link
that changes address often this trades steady-but-broken for brief-but-repeated
outages. That's the intended bargain here, but it is a bargain. Interactive
Windows runs (no service manager) still can't self-restart and will log a
manual-restart hint — unchanged, and noted in the config comment.

---

---

## v390 — 2026-07-13

Prompted by a roam capture: node moves off Wi-Fi `192.168.193.26` onto
`10.83.82.1` and the mesh goes dark, showing (among other things) a steady

```
mesh: local underlay address changed 192.168.193.26 -> 10.83.82.1; re-running path MTU discovery for all peers
mesh: path to "95e8beedf767fe66" rejected a 5092-byte packet ...; underlay MTU dropped to 1280 and re-discovering
send: write udp4 0.0.0.0:65432->74.208.225.216:443: sendto: message too long
send: write udp4 0.0.0.0:65432->192.168.193.9:65432: sendto: message too long
```

**What this release actually fixes — and what it does not.** Read the capture
carefully and most of it is *working as designed*, not the outage:

- The `rejected N-byte packet` / `message too long` staircase (5092 → 3162 →
  2197 …) is PMTU discovery's binary search descending from the ceiling toward
  the new link's real MTU after the v388/v383 EMSGSIZE-into-the-search path fired.
  `eff` sits safely at the floor (1280) the whole time; the search settles within
  a dozen ticks and then goes quiet. It is transient noise, not the partition.

The one genuine defect fixable from this capture alone is a gap v388 left. v388
taught the seed and handshake paths to consult `canSourceFamily` — don't emit a
send into a family this host can't route (no IPv6 on a tether ⇒ guaranteed
ENETUNREACH), every tick, forever. The **PMTU probe path was the one sender it
never covered.** `pmtuTick` issued a probe to `ps.ep()` unconditionally, so a
peer whose only endpoint is in a now-unroutable family gets a guaranteed-failing
probe once a second for the life of the session.

- `internal/mesh/pmtu.go`: `pmtuTick` now gates `sendProbe` on a new
  `Engine.probeReachable(ps)` — for a **direct** peer, `canSourceFamily(ep.Addr())`;
  a **relayed** peer is sent through its relay, whose reachability the relay session
  already governs, so it isn't gated. Fail-open, identical in spirit to
  `canSourceFamily`: no enumeration yet, or no valid endpoint, ⇒ probe proceeds.
- Tests: existing PMTU/roam/reachability suites pass unchanged.

**Not fixed here, because this one-sided log can't prove it.** The capture shows
two things this change does *not* address, and they are the likelier cause of
"never comes back":

1. **A peer pinned to a dead endpoint.** `192.168.193.9` is on the *old* Wi-Fi
   subnet; after the roam it's reachable only out the new default route, where a
   floor-sized datagram is silently dropped and an oversized one returns EMSGSIZE
   (same family, so `canSourceFamily` correctly does *not* gate it). `touch()`
   only follows *inbound* packets, and none arrive from a dead subnet, so the
   endpoint never gets re-pointed. Recovery for such a peer depends entirely on a
   handshake re-establishing it via a seed/relay.
2. **A flapping session.** `5f87d03fdff7b708` (gn-ionos2) comes up
   (`outbound tunnel up ... via 66.179.240.44:7`, then `:21`) and is pruned
   moments later — five stale sessions to the same node per prune pass, endpoints
   alternating between two ports. That is establish-then-die churn, not a PMTU
   problem, and pinning its root cause needs the *peer* side's log for the same
   window plus that peer's configured endpoints — exactly the same limitation
   v388 called out for its sibling case.

If you can grab gn-ionos2's log across 11:22:30–11:23:45 and confirm whether the
dead peers have any public/relay reachability from `10.83.82.1`, that's what it
takes to close the actual partition.

---

---

## v389 — 2026-07-13

Clearing the log from the web admin fails on Windows:

```
truncate C:\ProgramData\gravinet\gravinet.log: Access is denied.
```

The `Op` on that `*PathError` — `truncate` — points straight at the culprit:
`RotatingFile.Truncate` called `r.f.Truncate(0)` on the *active* handle. That
handle is opened `O_CREATE|O_WRONLY|O_APPEND` (so live logging keeps appending),
and there's the trap: Windows maps `O_APPEND` to `FILE_APPEND_DATA` and, in
doing so, drops `GENERIC_WRITE`/`FILE_WRITE_DATA` from the handle entirely. The
Windows implementation of `File.Truncate` is `SetEndOfFile`, which *requires*
`FILE_WRITE_DATA` — so on an append handle it returns `ERROR_ACCESS_DENIED`. On
Unix `ftruncate` doesn't care about append mode, which is why this only ever
showed up on Windows and passed every test on the build host.

The fix is to stop truncating through the append handle. Close it, empty the file
with a short-lived `O_TRUNC` (real write-access) handle, then reopen in append
mode for continued logging — the same "close before you touch the file so Windows
is happy" pattern rotation already uses.

- `internal/logx/rotate.go`: `RotatingFile.Truncate` now closes `r.f`, reopens the
  path with `O_CREATE|O_WRONLY|O_TRUNC` to zero it, closes that, and reopens the
  normal append handle via `open()` (which resets the tracked size from the now-empty
  file). If emptying fails it still tries to restore an append handle so logging
  survives, then returns the original error.
- Portable by construction: `O_TRUNC` opens with write access on every platform,
  so the same code path fixes Windows without special-casing `GOOS`.
- The existing `TestRotatingFileTruncate` (empties in place, resets the counter,
  writes-after-truncate start from zero) and the webadmin `handleLogsClear` tests
  continue to pass unchanged — behaviour is identical everywhere it already worked.

---

---

## v388 — 2026-07-13

From a post-roam debug capture: the node spends its entire recovery window
dialing addresses it cannot possibly reach.

```
send: write udp6 [::]:65432->[fdf5:168:5:0:7e58:...]:65432: sendto: network is unreachable
tcp fallback dial [fdf5:168:5:0:a5e3:...]:65432: connect: network is unreachable
tcp fallback dial [fdf5:168:55::33]:13: connect: network is unreachable
```

The new link has **no IPv6**. Every peer's IPv6 endpoint and every IPv6 host
candidate (v374 made there be a lot of these) is therefore a guaranteed
`ENETUNREACH` — and gravinet retried every one of them on every cycle, forever,
over both transports. Dozens of futile syscalls per tick, drowning the log and
eating the dial budget that the addresses which *can* work are competing for,
precisely when a roam has left the node with the least room to waste.

`ENETUNREACH` is a synchronous, definitive verdict from the kernel — the same
class as the `EMSGSIZE` handled in v383. It is not a transient to be retried; it
is a statement that this path does not exist from here.

Rather than react per-packet, ask the question once per maintenance tick, which
is where the answer actually changes. `refreshLocalCandidates` already enumerates
every address this host holds, so it already knows which families are live.

- `internal/mesh/engine.go`, `localcand.go`: `Engine.haveV4` / `haveV6`, refreshed
  alongside `ownAddrs`, and `canSourceFamily(addr)` — does this host hold any
  *routable* source address of that family (the same loopback/link-local filter
  `usableLocalCandidate` applies; neither can source a packet to a peer's global
  or ULA address).
- `internal/mesh/handshake_engine.go`: `initSeedTick` and `ensureFallback` skip a
  seed whose family cannot be sourced. Re-evaluated every tick, never latched, so
  a roam that *gains* IPv6 picks those addresses straight back up within one tick.
- Fails **open**: if addresses haven't been enumerated yet, everything is
  considered reachable. Refusing to dial on no evidence would wedge the node into
  dialing nothing at all — far worse than the wasted syscalls this prevents.
- Tests: family gating with a v4-only host, re-enablement the moment v6 appears,
  fail-open before enumeration, an invalid address sourceable in no family, and
  the guard actually being applied on the hot path (`initSeedTick` plans no
  handshake and issues no fallback dial for an unsourceable seed).

**Not fixed here, and not yet explained.** The same capture shows twelve TLS
connections to `77.68.127.174` established and *none* of them carrying a
handshake:

```
WARN mesh: tcp fallback to 77.68.127.174:443 connected but no mesh session formed within 10s
```

That is the one path on this link that should work, and it is the real blocker —
the IPv6 noise above is waste, not the cause. `Dual.Send` correctly prefers a live
TLS connection over UDP, so the handshake is going out over the right transport;
something on the far side is not answering it. Diagnosing that needs the *remote*
node's log for the same window, not more inference from this end.

---

---

## v387 — 2026-07-13

Report: on Linux, after a peer sleeps and wakes, the `mesh0` interface is gone —
and stays gone. There *is* a guard for a vanishing overlay interface
(`internal/mesh/dataplane.go`: `reconcileDataplane` asks the kernel every
maintenance tick whether the interface still exists, and `recoverDataplane`
rebuilds it). It was never reached, because the interface was never lost at
runtime — it was lost at **startup**, and the guard only supervises a network
that got created in the first place.

The sequence:

1. Host sleeps and wakes. `maintLoop` sees the clock jump and fires the
   suspend/resume hook, which asks for a clean process restart — the underlay
   socket, TUN device and OS routes can't all be reliably rebuilt in place (see
   `SetSuspendResumeHook`).
2. `shutdown()` closes the TUN fd. A Linux TUN is non-persistent, so `mesh0` is
   destroyed along with it.
3. `selfRestart()` immediately `syscall.Exec`s a new image of the process.
4. The new process calls `tun.New("mesh0")` — but the kernel's teardown of the
   *old* `mesh0` is asynchronous. `unregister_netdevice` is deferred and can be
   held up by references the stack hasn't dropped yet, so `TUNSETIFF` still sees
   the name in use and fails.

The new process loses a race against its own predecessor. And the consequence was
total, not partial, because `buildNetSpecs` logged the error and `continue`d:
**no NetSpec → no netState → no maintLoop → no data-plane supervisor.** The daemon
came up reporting success with zero networks, `mesh0` simply absent, and nothing
anywhere retried it. Gone until a manual restart — exactly as reported.

- `cmd/gravinet/main.go`: `newTunRetrying` — creates the interface with backoff
  (100ms doubling to 2s, 15s budget), used both at startup and inside
  `spec.NewDevice`, the engine's rebuild factory. The rebuild path needs it for
  the same reason: a rebuild is triggered *precisely* when the OS has just
  destroyed the interface, so the kernel may still be unwinding the old netdev
  when the name is asked for back. A first-try success costs nothing, which is the
  overwhelmingly common case.
- `cmd/gravinet/main.go`: dropping a network now says what that actually means —
  that nothing will supervise or retry it and the network is absent until a
  restart — rather than emitting one error line and carrying on as though the node
  were healthy. That silence is a large part of why this looked like the guard had
  regressed.
- `cmd/gravinet/tunretry_test.go`: retries past a transient busy (the predecessor
  still unwinding) and succeeds; gives up on a permanent failure (no
  CAP_NET_ADMIN) inside its budget with the cause intact, rather than hanging
  startup; and a first-try success takes one attempt and no sleeps.

Worth being precise about the earlier claim that a guard "was there and is gone":
it wasn't removed. `dataplane.go` still guards a *running* interface exactly as it
always did. What never existed was a guard for failing to **create** the interface
— a different hole, in a different file, reachable only through the restart the
resume path itself triggers.

---

---

## v386 — 2026-07-13

The v383 EMSGSIZE fix is confirmed working in the field — the debug log shows it
firing and binary-searching the MTU down (`rejected a 5092-byte packet ... dropped
to 1280`, then `3162`, converging). But the same log exposed something else,
loudly:

```
INFO mesh: pruned dead session to "5f87d03fdff7b708" on net eb1c2a7e984f072e
   × 25, at 06:21:04
   × 25, at 06:21:09
   × 25, at 06:21:14
```

Twenty-five identical prune lines, every five seconds (`maintInterval`), forever,
all for one peer. A session that is genuinely pruned cannot be pruned again — so
they were being *recreated* at ~5/second.

`5f87d03fdff7b708` is gn-ionos2, at `66.179.240.44`, which has **twelve
configured TCP seed ports** — plus its UDP endpoint, plus host candidates. Every
one of those is a separate entry in `ns.seeds`, all owned by the same node. When
that peer is relay-connected (as it is during a roam), every one of them lands in
`seedOwnerNeedsUpgrade` independently, on every tick, with no
`directUpgradeInterval` holding it back — because the unthrottled upgrade paths
(explicit seeds, v370; host candidates, v381) are keyed by **seed**, and a peer
owns many. A dozen concurrent upgrade handshakes per second, to one peer. Each
that lands installs a fresh session and displaces the previous one.

**A wrong fix, recorded because the tests earned it.** The obvious move is to
retire the displaced session in `install()` instead of leaving it for `pruneDead`.
That is wrong, and twenty end-to-end tests said so immediately: `localIdx` is our
*receive* index — the number the peer writes into packets addressed to us — and
both directions handshake (`inbound tunnel up` and `outbound tunnel up` both
appear in the log). A displaced session is still a live receive path. Deleting it
tears down traffic in flight. Reverted.

The real fix is upstream: don't launch a dozen upgrades in the first place.

- `internal/mesh/engine.go`: `netState.upgradeNodeAt` (keyed by **node id**, not
  seed) and `upgradeNodeInterval` (= `handshakeRetry`).
- `internal/mesh/handshake_engine.go`: `seedOwnerNeedsUpgrade` now gates the
  unthrottled paths per peer. One upgrade in flight per peer is all that is ever
  useful — they all reach the same node, and the first to land makes the rest
  redundant. Serializing costs nothing: a seed that loses the race is retried on a
  later tick, and since `planHandshake` pushes a failing seed into `seedBackoff`,
  the turn rotates naturally through a peer's addresses rather than sticking on
  the first.
- Tests: `TestUpgradeSerializedPerNodeNotPerSeed` (four seeds on one relayed peer
  → exactly one upgrade per tick, and another seed takes the next turn once the
  gate elapses) and `TestUpgradeGateIsPerPeerNotGlobal` (serializing must not let
  one relayed peer starve another — the gate is per node, not global).
  `TestSeedOwnerNeedsUpgradeSkipsThrottleForExplicitSeed` updated: its point was
  always that an explicit seed escapes `directUpgradeInterval`'s five minutes, and
  it still does — it is now paced by a two-second per-peer gate instead, which is
  orders of magnitude below the throttle a gossip-learned endpoint faces.

---

---

## v385 — 2026-07-13

Bug in v382's own log-level control: changing it from debug back to info didn't
stick. Leave the settings page, come back, and it's on debug again.

`reloadFn` gated the apply on `newCfg.LogLevel != cfg.LogLevel` — and `cfg` is
the config the daemon **booted** with, never reassigned. So:

- Boot at `info` → `cfg.LogLevel` is `"info"` for the life of the process.
- Select **debug** → `"debug" != "info"` → applied. Looks like it works.
- Select **info** → `"info" != "info"` is **false** → `SetLevel` never runs.

The config file saved `info` perfectly correctly. The *running logger* stayed on
debug. And `/api/config` reports the live level — which is right, it should
report what's actually in effect — so the settings page dutifully snapped back to
debug. The level could be changed away from its boot value exactly once, and
never back.

Every other live setting in `reloadFn` compares against a tracking variable it
actually updates (`prevPort`, `prevTCP`, `prevExtraTCP`). This one reached for
the startup snapshot, which never moves.

- `internal/logx/logx.go`: added `CurrentLevel()`. The running logger is the only
  source of truth for what's in effect; anything deciding whether a configured
  level still needs applying must ask it, not a snapshot.
- `cmd/gravinet/main.go`: `reloadFn` now compares
  `logx.ParseLevel(newCfg.LogLevel)` against `logx.CurrentLevel()`. Sets first,
  then logs — so raising verbosity announces itself under the new level, while
  lowering to warn/error is deliberately quiet.
- `internal/logx/level_test.go`: `TestLevelCanBeSetBackToItsBootValue` walks every
  boot level, moves to every other level and back again. The return leg is the one
  that used to fail. Verified load-bearing by restoring the snapshot comparison and
  watching it fail on the first round trip.

Worth naming the shape, because it is not specific to logging: **gating a live
update on a comparison against startup state.** It works exactly once per value
and then silently stops, and it presents as "the setting won't save" — which
sends you looking at persistence, where nothing is wrong at all.

---

---

## v384 — 2026-07-13

Closes the loose end flagged in v377 and left open since: **192.168.122.1**.

mcfed was advertising it as a host candidate. It's the libvirt default bridge —
and gn-cush1, which runs libvirt too, holds the *identical* address on its own
virbr0. cush1 duly logged `learned host candidate 192.168.122.1:65432 for peer
0916a3a70b1d5f4c` and dialed it, reaching itself. I called this cosmetic in v377.
It isn't: the candidate is guaranteed wrong for any peer sharing the convention,
it consumes one of that peer's `maxLocalEndpoints` slots, and it generates a dial
every cycle that can only ever fail. `10.83.82.1` in mcfed's advertisement is the
same class of thing.

The root cause is that enumeration used `net.InterfaceAddrs()`, which flattens
away *which interface* each address belongs to — and the interface is precisely
what separates a real uplink from a virtual bridge the host itself owns.

- `internal/mesh/localcand.go`: enumerate per-interface via `net.Interfaces()`,
  skipping down/loopback interfaces and host-local virtual networks
  (`virtualBridgeIface`: virbr, docker, br-<hex>, veth, vboxnet, vmnet, lxcbr,
  lxdbr, podman, cni, flannel, cali, weave, kube).

  What is deliberately **not** on that list matters as much as what is. Bare
  `br0`/`bridge0` is very often the *real* LAN bridge on a hypervisor host — the
  actual uplink, holding the address a peer genuinely should dial — so excluding
  it would break the same-LAN discovery this whole mechanism exists for. Docker's
  per-network bridges are `br-<12 hex>`, which the `br-` prefix catches without
  touching `br0`. VPN/tunnel interfaces (`wg*`, `tun*`, `tailscale*`) are also
  left in on purpose: unlike a host-local bridge, a peer really may be reachable
  across another VPN, and that is a legitimate path.
- `internal/mesh/engine.go`, `localcand.go`: `Engine.ownAddrs` — every address
  this host holds on any interface, *including* the bridges and down interfaces we
  refuse to advertise. `addLocalCandidates` now rejects any peer candidate naming
  an address we hold ourselves (`isOwnAddr`).

  This is the receive-side half, and it is precise where the name filter is
  heuristic. We cannot see a peer's interface names, so we cannot tell whether the
  192.168.122.1 it sent is an uplink or its libvirt bridge — but we don't need to.
  If we hold that address too, dialing it reaches *this* daemon, so it is worthless
  as a path to that peer regardless. Zero false positives (an address we own is
  never a route to somebody else), and it keeps working against peers still running
  older builds that advertise their bridges.
- Removed `netState.isHostCand`, dead since v381 dropped the fallback guard that
  used it.
- Tests: `TestVirtualBridgeIfaceExcludesVMPlumbingButNotRealBridges` (the `br0`
  half is the one that matters), `TestOwnAddrCandidateRejected` (a peer's
  192.168.122.1 is dropped while a genuine 192.168.55.3 is still seeded), and
  `TestRefreshPopulatesOwnAddrsIncludingBridges` (the own-address set is a superset
  of what we advertise — recording only the advertised set would leave exactly the
  ambiguous addresses unguarded, which is the entire point of it).

---

---

## v383 — 2026-07-13

Debug logging (v382) paid for itself on the first roam. The cause of the
underlay-switch blackout, which no amount of Info-level output could ever have
shown:

```
DEBUG mesh: send: write udp4 0.0.0.0:65432->192.168.193.9:65432: sendto: message too long
DEBUG mesh: send: write udp4 0.0.0.0:65432->74.208.225.216:443: sendto: message too long
```

**EMSGSIZE, once per second, forever.** mcfed's home LAN is jumbo — the peers
table had been showing `udp 9000 B` and `udp 8955 B` all along — so PMTU
discovery had settled every peer near 9000. Roaming to cellular (~1400 MTU)
meant every packet still sized for the old link was refused *by the kernel,
before it ever left the host*.

And `send()` threw the verdict away: `e.log.Debugf("mesh: send: %v", err)`, drop
the packet, re-send the same oversized packet a second later, indefinitely. `eff`
was never lowered, so nothing got out at all — not a handshake, not a keepalive,
not a probe. The node went completely dark and recovered only when the jumbo link
returned. Meanwhile the PMTU search sat waiting on probe *timeouts* to infer the
exact fact the kernel had already stated outright.

EMSGSIZE is strictly better information than a timeout, and it arrives free. A
timeout is inference — the ack didn't come back, so the size *probably* didn't
fit — and it costs `pmtuMaxTries × pmtuProbeTimeout` to reach that guess. EMSGSIZE
is the kernel saying, synchronously and definitively, that the size does not fit.

- `internal/mesh/pmtu.go`: new `pmtuState.tooBig(size, now)`. If the refused size
  is the in-flight candidate, fail it at once rather than after its retries
  expire. If it's a *non-probe* packet (keepalive, handshake, real traffic), then
  `eff` itself is too large — and no probe is in flight to time out, so the search
  could never learn it and the peer stays blackholed indefinitely. Drop straight
  to the floor (the configured known-good `underlay_mtu`) and re-search upward. If
  even the floor is refused, do nothing: `underlay_mtu` is simply wrong for that
  path and thrashing won't help.
- `internal/mesh/engine.go`: `send()` detects EMSGSIZE (`isMsgTooLong`, unwrapping
  the `*net.OpError` → `*os.SyscallError` → errno chain the transport actually
  returns) and routes it to `noteTooLong`, which finds the peer owning that
  endpoint and clamps it. Logs at **Info**, not Debug — a link whose MTU just
  shrank under you is worth saying out loud.
- Tests (`internal/mesh/pmtu_toobig_test.go`): in-flight candidate failed
  immediately; non-probe rejection drops `eff` to the floor and re-enters search
  (the blackout case a timeout can never catch); EMSGSIZE at the floor is inert;
  and `isMsgTooLong` unwraps the real wrapped error while *not* firing on
  `ENETUNREACH` — a routing failure is not an MTU verdict, and clamping on one
  would be its own bug.

Worth noting what this says about the earlier versions in this thread: the roam
never had anything to do with seeds, relays, or host candidates. Those were real
bugs and worth fixing, but the node was dark because it was shouting 9000-byte
packets at a 1400-byte link and refusing to hear the kernel tell it so.

---

---

## v382 — 2026-07-13

"Where do I set the log level to debug in the web admin?" Nowhere — and worse
than nowhere. `config.LogLevel` existed, but `logx.SetLevel` was called exactly
once, at startup (`main.go:289`), and `reloadFn` never touched it again. The
only way to raise the level was to edit `config.json` and **restart the daemon**.

That is close to useless, because the restart destroys the thing you are trying
to observe. It resets every session, backoff timer, learned endpoint and PMTU
estimate — so any fault that only reproduces on a live network event (a roam, a
peer flapping, a fallback that won't establish) is wiped out by the act of
preparing to watch it.

The level matters more here than in most daemons, because nearly every
*rejection* path in the mesh logs at Debug: a replayed handshake, a clock-skew
mismatch, a handshake claiming our own node id, a locally-disabled peer, a
failed TLS dial. At the default Info level, a node that is receiving handshakes
and **refusing every one of them** is indistinguishable, in the log, from a node
receiving nothing at all. Those are opposite bugs with opposite fixes, and
telling them apart began with "edit this JSON file and restart" — which is
exactly the wall this thread hit while chasing a roam that only fails on the
live transition.

- `cmd/gravinet/main.go`: `reloadFn` applies `log_level` live when it changes.
  It's a process-global with no dependent state, so this is safe and immediate —
  no restart, no sessions dropped.
- `internal/logx/logx.go`: added `Logger.GetLevel` and package-level
  `LevelName()` (so named to avoid colliding with the existing `Level` type),
  returning the lowercase spelling `ParseLevel` accepts and `config.LogLevel`
  stores.
- `internal/webadmin/loglevel.go` (new): `GET/POST /api/loglevel`, validated
  against `error|warn|info|debug`. Deliberately **not** in `LOCAL_API`, so it
  proxies like any other setting — a Manager can raise a *peer's* level, which is
  the case that actually needs it: the node with the problem is rarely the one
  you're sitting at.
- `internal/webadmin/webadmin.go`: `Backend.LogLevel()`, implemented by
  `mesh.Engine`; `/api/config` now reports `log_level` so the UI shows the live
  value rather than guessing at it.
- `internal/webadmin/ui.go`: a Log level selector in Settings, beside Remote
  shell, stating plainly what debug buys and warning that it's chatty.
- Tests (`internal/logx/level_test.go`): `TestLevelNameRoundTrips` — the UI reads
  the level back through `LevelName` and writes it through `ParseLevel`, so if
  the spellings drift the dropdown silently shows the wrong current value while
  still applying changes correctly, a UI bug that looks exactly like the setting
  not taking. And `TestDebugSuppressedAtInfo` — debug really is dropped at info,
  and raising the level takes effect with no restart.

---

---

## v381 — 2026-07-13

Root cause, finally, and it was mine. **gn-cush1 had UDP disabled.** Re-enabling
it made everything work — which is the proof, because with UDP off cush1 was
reachable over TCP/TLS and only over TCP/TLS, and gravinet was never dialing it
there.

v375 got the advertising half right: a node with UDP off (`PrimaryPort` 0 — the
'-' setting) advertises its LAN address at its TCP/TLS port, because that is
where it can actually be reached. cush1 was doing exactly that, and cush2 was
learning it — the logs confirmed the candidate arriving six times over.

Then v379, tightening the dial-storm cost, added this:

```go
if e.primaryPort.Load() != 0 && ns.isHostCand(seed) { return }
```

"Skip the TCP dial for a host candidate if UDP works." But `e.primaryPort` is
**our** port. The question that matters is whether the *peer* can be reached over
UDP, not whether *we* can speak it. cush2 has UDP enabled, so for every host
candidate it would probe over UDP, hear silence — correctly, there is no UDP
listener on a UDP-disabled node — and then decline to try TCP, on the grounds
that its own UDP was working fine. The single path that could ever have worked
was suppressed by the code meant to be conserving effort. Two machines on the
same switch, relaying to each other across the internet.

The cost problem v379 was solving is real; the fix was simply the wrong shape.
**Rate-limit, never suppress.**

- `internal/mesh/handshake_engine.go`: the guard is gone. `ensureFallback` now
  paces repeat dials per-seed via `fallbackDialCooldown` (30s) and
  `netState.fallbackAttempt`. The first dial to any seed is immediate; only
  retries wait. That bounds dozens of unreachable seeds to a trickle instead of
  a per-tick storm, without ever refusing a path that might be the only one a
  peer has.
- `internal/mesh/handshake_engine.go`: `seedOwnerNeedsUpgrade` no longer applies
  `directUpgradeInterval`'s 5-minute throttle to a host candidate. That throttle
  is calibrated for speculative WAN retries against a peer a relay is already
  covering for; a same-link LAN probe is the cheapest and highest-value dial in
  the system. It had also made the mechanism work only by accident of
  bookkeeping — `explicitSeedNode` is keyed by node ID and populated only when
  gossip attributes a node's *configured seed address* to it, so whether two
  co-located machines found each other promptly depended on whether an address in
  someone's config matched what gossip reported. Two nodes on the same switch
  should not need to be configured seeds of each other to find each other.
- Tests: `TestHostCandidateDialedOverTCPWhenPeerHasUDPDisabled` builds an engine
  with UDP **enabled** — that is the point — and asserts a UDP-disabled peer's
  host candidate still gets its TCP/TLS dial. Verified load-bearing by restoring
  the v379 guard and watching it fail. `TestFallbackDialIsPacedNotSuppressed`
  covers the cooldown (first attempt immediate, retries paced, per-seed not
  global). `TestHostCandidateUpgradeIsNotThrottled` covers the throttle
  exemption, while `TestSeedOwnerNeedsUpgradeThrottles` still passes — an
  ordinary gossip-learned endpoint is throttled exactly as before.

Worth stating plainly, since it cost several versions: v379's guard was written
to bound a cost, and it silently deleted a capability. "Skip the expensive thing
when we don't need it" is only safe if *we* is the right subject. It wasn't.

---

---

## v380 — 2026-07-13

v379 restored management (the resource exhaustion was real and is fixed). But
it introduced a bug of its own, and it's one that neutered host candidates for
precisely the case they exist to serve.

`hostCandGrace` was 90 seconds. `directUpgradeInterval` is **5 minutes**.

The single most valuable host candidate is the one belonging to a peer we
currently reach only through a relay — that is the entire point of the feature.
But an upgrade attempt toward a relay-only peer is throttled to one per
`directUpgradeInterval` unless that peer is an explicit seed. So a candidate for
a relayed, non-explicit peer got **exactly one dial attempt**, was then swept as
"never connected" at 90s, and was permanently recorded in `hostCandDead` — which
also bars gossip from ever re-adding it. One shot at its only job, then written
off forever. I set both constants myself, a version apart, and never checked
them against each other.

- `internal/mesh/localcand.go`: `hostCandGrace` is now 20 minutes — comfortably
  more than three throttled upgrade attempts, still far below `deadSeedGrace`'s
  hour. The cost that made a short grace seem necessary was overwhelmingly the
  speculative TCP/TLS dial storm, and v379 removed that separately (candidates
  get no speculative TCP dial while UDP works locally); the remaining per-tick
  cost of a dud candidate is a UDP handshake packet, which is cheap.
- `internal/mesh/localcand_test.go`: `TestHostCandGraceOutlastsUpgradeThrottle`
  asserts the relationship directly — grace must exceed the throttle, leave room
  for at least three attempts, and stay under `deadSeedGrace`. A constant tuned
  in isolation cannot silently break the mechanism again.

Note for whoever reads this next: a peer that IS an explicit seed skips the
throttle entirely (v370) and retries at `seedRetryBackoff`, so gn-cush1/cush2 —
being configured seeds — were never subject to the one-shot behaviour. This fix
matters for every peer that isn't.

---

---

## v379 — 2026-07-13

v378 removed the syscall from under `ns.mu`, but the node was still
unmanageable and — the new detail that gave it away — it *started* healthy and
degraded: only cush1 relayed at first, then gn-ionos1/2/3 dropping to relayed a
minute or two later. Peers going from direct to relayed means healthy sessions
were being **torn down**, not failing to form. That's resource exhaustion, and
v378 only fixed one symptom of it.

Host candidates were being treated as full seeds. Each one got `deadSeedGrace`
— **an hour** — of UDP handshake cycles every `seedRetryBackoff`, *plus* a
TCP/TLS dial (socket, TLS handshake, goroutine) on every init tick it spent
cooling down. In a mesh of ~9 peers each advertising several LAN addresses, a
node ends up with dozens of candidates, and by construction most are unreachable
from it — ionos's private addresses mean nothing to cush2, cush2's `192.168.55.x`
means nothing to ionos. Before v377 this was invisible because candidates were
being destroyed as fast as they were learned. Once they persisted, every node
began grinding continuously through ~70 dud addresses. That starved the web
admin (unmanageable) and delayed keepalives past `peerTimeout` (20s), so working
direct sessions died and `tryRelays` — correctly — replaced them with relayed
ones. Hence the rot from direct to relayed over the first few minutes.

A host candidate is not a seed. It's a *speculative same-link address*: if it
works it works instantly, and it has none of the reasons a real seed earns
patience (a peer still booting, a NAT yet to open, an operator-configured anchor
that must never be abandoned).

- `internal/mesh/localcand.go`: `hostCandGrace` (90s) replaces `deadSeedGrace`
  for candidates in `sweepDeadSeeds`.
- `internal/mesh/engine.go`: `netState.hostCandDead` remembers a candidate that
  had its chance and never connected. Without it the sweep and gossip chase each
  other forever — `learnPeers` re-delivers every peer's full candidate list on
  every interval, so a dud would be re-added the instant it was swept, in
  perpetuity. Cleared by `clearDeadHostCands` whenever this node's *own*
  candidate set changes (new lease, interface up/down, Wi-Fi → cellular):
  reachability is a property of both ends, so our own attachment moving
  invalidates every prior "unreachable from here" verdict.
- `internal/mesh/handshake_engine.go`: `ensureFallback` no longer issues a
  speculative TCP/TLS dial for a host candidate **while UDP works locally**.
  That dial was the single most expensive thing in the init loop, and it was
  firing for every cooling-down candidate on every tick. The peer's real,
  observed endpoint is a separate seed and still gets the full fallback
  treatment, so no peer loses its TCP path — only the speculative TCP dial to a
  LAN address whose UDP probe already failed, on a host where UDP demonstrably
  works, is dropped. When UDP is *off*, TCP is the only path there is (v375), so
  the guard lifts and candidates are dialed over TLS exactly as they must be.
- Tests: `TestHostCandidateSweptOnShortGraceAndNotResurrected` (short grace,
  dead-marking, gossip cannot resurrect, and a network change *does* grant
  another chance), `TestEnsureFallbackSkipsHostCandidatesWhileUDPWorks` (no dial
  storm; observed endpoints unaffected), and
  `TestEnsureFallbackDialsHostCandidateWhenUDPDisabled` (v375's guarantee still
  holds). Full suite plus `-race` across the candidate/seed/relay/fallback
  tests.

The general lesson, and the one I keep relearning in this thread: a mechanism
that adds N×M entries to a per-tick loop must have its cost bounded at the point
it's introduced, not inherited from whatever the loop already did to a handful
of entries. Host candidates were correct in v374–377 and still nearly took the
node down.

---

---

## v378 — 2026-07-13

**Regression fix — v377 made gn-cush2 unmanageable.** It still answered pings;
the web admin was simply unreachable. My bug, and the mechanism is worth
recording because the trigger was v377's *fix* rather than the code it fixed.

`localEndpoints` enumerated interfaces inline — `net.InterfaceAddrs()`, a
netlink syscall, plus `e.mu` via `isOverlayAddr`. It is called from
`buildHSInit`, which `planHandshake` invokes **while holding `ns.mu`**, for
every handshake packet the node builds. So every handshake build did a syscall
under the network-wide write lock. That has been true since v374 and was
survivable for one accidental reason: host candidates were being destroyed as
fast as they were learned (the very bug v377 fixed), so `ns.seeds` stayed tiny.

Once candidates persisted, `ns.seeds` grew to hold every peer's LAN addresses —
dozens of entries, most of them not reachable from this node and therefore
cycling handshakes forever. `initLoop` then issued a syscall *per seed per
tick*, with `ns.mu` held throughout. The data path survived (it takes `ns.mu`
only briefly, and ICMP is sparse enough to slip between writers), which is
exactly why it still pinged. Anything that needed the lock for longer — the web
admin's peer listing above all — was starved out completely.

- `internal/mesh/localcand.go`: `localEndpoints` is now a pure atomic read of a
  published set (`Engine.localCands`) — no syscall, no lock — so it is safe to
  call under `ns.mu`. The enumeration moved to `refreshLocalCandidates`, which
  runs off the handshake path: primed in `NewEngine` (after nets are stored, so
  `isOverlayAddr` can see the overlay subnets), refreshed once per maintenance
  tick in `maintLoop`, and re-run immediately by `SetPrimaryPort` /
  `SetFallbackPort` so a live port change doesn't wait for a tick.
- `internal/mesh/localcand_test.go`: `TestLocalEndpointsSafeUnderNetLock` calls
  `localEndpoints` from a goroutine that holds `ns.mu`, exactly as
  `planHandshake` does, and fails if it blocks. If anyone ever reintroduces a
  lock or syscall there, this deadlocks in CI instead of silently starving a
  production node's web admin. `TestRefreshLocalCandidatesPublishes` covers
  priming at construction and immediate republication on a port change.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, full
  repo suite, plus a `-race` run across the candidate/handshake/relay tests
  given this is a concurrency change.

The lesson generalizes past this one call site: `buildHSInit` runs under
`ns.mu`, so *nothing* it touches may block, syscall, or take another lock. Worth
remembering before adding the next field to a handshake payload.

---

---

## v377 — 2026-07-13

Production logs from v376 finally made this visible instead of theoretical, and
they caught it immediately.

cush1's side was fine — it advertises `[192.168.55.3:65432 192.168.203.1:65432
[fd00:203::1]:65432 [fdf5:168:55::3]:65432]`, and `192.168.55.3` is exactly the
LAN address cush2 needs. The advertising half of v374 works.

The tell was on cush2: it logged **"learned host candidate X for peer Y" twice
for the same address, three seconds apart** (05:27:47 and 05:27:50, peer
gn-openbsd). `addSeed` dedups, so an address can only be learned as *new* again
if it was **deleted in between**. Something was destroying host candidates
seconds after they arrived.

`install()`'s stale-seed prune. Its premise is "this seed is a stale guess at
where the peer is, superseded by where it actually is." That premise is exactly
false for a host candidate, which is *by definition* an address other than the
observed endpoint — that is the entire point of it. Owned by that node, not an
explicit seed, not equal to `ps.endpoint`: it matched the prune condition
perfectly, every single time. So every re-handshake deleted the peer's LAN
addresses moments after they were learned, gossip re-delivered them, they were
added again, and the next handshake deleted them again. Nothing could dial an
address that never survived a full tick.

v376 gated the prune on `ps.endpoint.IsValid()`, which fixed this for *relayed*
peers — but a **direct** peer has a perfectly valid endpoint and went right on
eating its own candidates. That's why the duplicate-learn logs are for
gn-openbsd (direct), and it's the same mechanism that would keep any
relay→direct upgrade from ever landing.

- `internal/mesh/engine.go`: `netState.hostCand` records which seeds are host
  candidates. Entries are never removed, so a candidate that gets swept as dead
  and is later re-gossiped doesn't re-announce itself as though it were news.
- `internal/mesh/handshake_engine.go`: `install()`'s prune now skips
  `ns.hostCand[s]`, the same exemption explicit seeds got in v371. A genuinely
  stale observed endpoint is still pruned exactly as before.
- `internal/mesh/localcand.go`: `addLocalCandidates` marks candidates in
  `hostCand` before seeding, and logs only the first time an address is *ever*
  seen as a candidate — so seed churn can't manufacture repeat announcements.
  It also drops overlay addresses before marking or logging them.
- `internal/mesh/localcand.go`: `localEndpoints` no longer advertises our own
  overlay (TUN) addresses. Receivers discard them anyway (`addSeed` refuses to
  dial into an overlay subnet), so they were pure waste — and worse, they eat
  slots under `maxLocalEndpoints`. cush1 was spending **two of its four**
  candidate slots telling peers about addresses none of them can ever use.
- `internal/mesh/handshake_flap_test.go`: `TestInstallNeverPrunesHostCandidates`,
  verified load-bearing by reverting the exemption and watching the LAN
  candidate vanish (`seeds=[192.168.5.108:65432]`) — the same disappearance the
  production logs were showing.

Not yet explained, and worth watching after this deploys: mcfed advertises
`192.168.122.1` (the libvirt default bridge). That address is identical on every
KVM host, so a peer dialing it may be talking to *its own* bridge rather than
mcfed's. The handshake would simply fail to authenticate rather than
mis-associate, so it's not dangerous — but it burns a candidate slot and
generates pointless dials. A "candidate addresses that are ambiguous across
hosts" filter (libvirt/docker/vbox default bridges) is worth considering
separately.

---

---

## v376 — 2026-07-13

With every node confirmed on v375, gn-cush1 was still relayed from gn-cush2.
Host candidates should have fixed that, so something was destroying them.
Something was.

**A relayed install deleted every dial candidate the peer had.** `onRelay`
dispatches the inner packet with a zero `netip.AddrPort` — the source actually
observed was the *relay*, not the peer — so a relayed session's `ps.endpoint`
is invalid. `install()`'s stale-seed pruning then asks, for each seed it can
attribute to that node, "is this something other than the peer's current
endpoint?" With no current endpoint, that is trivially true for **every** seed.
So every relayed (re)handshake wiped the peer's entire candidate set: its
gossip-learned endpoints, and — since v374 — its host candidates, the LAN
addresses whose sole purpose is upgrading a relayed peer to direct. The
upgrade path was left with nothing to dial, which guaranteed the peer stayed
relayed, which kept the session relayed, which kept wiping the candidates. The
new regression test shows it starkly: before the fix, `seeds=[]` immediately
after the install.

This predates v374 — it has been quietly sabotaging relay→direct upgrades for
every relayed peer all along. Host candidates just made it load-bearing, and
made it visible: v374/375 were correct, and were being erased a moment after
they worked.

- `internal/mesh/handshake_engine.go`: the prune loop is now gated on
  `ps.endpoint.IsValid()`. A relayed session supersedes nothing, so there is
  nothing for it to prune. `everConnected` is likewise no longer marked for the
  zero endpoint — a relayed session proves no underlay address reachable, and
  recording it would have made `sweepDeadSeeds`' "has this ever worked" check
  meaningless.
- `internal/mesh/handshake_flap_test.go`:
  `TestInstallRelayedSessionDoesNotPruneSeeds`. Verified load-bearing by
  reverting the fix and watching it fail with `seeds=[]`.

**Diagnostics, because this thread has been fought almost entirely blind.**
There was no way to see whether a host candidate was advertised, gossiped,
received, or dialed — the same silence that made `allow_relay` (v373)
undiagnosable. Two Info-level logs now bracket the whole path:

- `internal/mesh/localcand.go`: on the advertising side, "advertising N host
  candidate(s) to peers: [...]" — change-gated (it's called on every handshake
  build), so it fires at boot and whenever interfaces or ports change. Silence
  means this node offered nothing.
- `internal/mesh/localcand.go` + `engine.go`: on the receiving side, "learned
  host candidate X for peer Y — will try it for a direct path", only for a
  genuinely new candidate (`addSeed` now reports whether it actually added one,
  rather than returning nothing, so gossip re-delivering the same list every
  interval doesn't re-log).

Between them: if a peer is stuck relayed and the "learned host candidate" line
never appears for it, the candidate never arrived — not advertised, not
gossiped, or filtered — which is an entirely different problem from it arriving
and the dial failing. Those two were previously indistinguishable, which is
most of why this took eight versions.

---

---

## v375 — 2026-07-13

"What if I disabled UDP ports? Should it also work over TCP as well?" It
should, and as shipped in v374 it would not have. Two separate gaps, both
closed here — v374's host candidates were quietly UDP-only.

**1. A UDP-disabled node advertised no host candidates at all.**
`localEndpoints` paired each interface address with the primary UDP port and
bailed outright when that was 0 (the '-' setting). But the addresses are
still perfectly dialable over the TCP/TLS fallback — and a node with UDP off
has *more* need of a LAN path to its same-NAT neighbours, not less, since a
relay is its only alternative. Suppressing them was exactly backwards. It now
advertises at the fallback port when UDP is off, and returns nothing only when
UDP and TCP/TLS are both disabled — genuinely nothing dialable to offer.

**2. A host candidate could never resolve the peer's TCP port.**
`tcpPortForEndpoint` looked for a live session on the same IP, then for a
`nodeInfo` whose `endpoint` matched exactly. A peer's LAN address is by
definition *not* its observed endpoint, so that exact-match can never hit for
a candidate — every LAN candidate silently fell through to **our own**
fallback port, which is right only by coincidence, whenever both nodes happen
to use the same one. It now consults the candidate's owning node first
(`ns.seedOwner` → `ns.nodes[owner].tcpPort`), reading the port that node
actually advertised. Node-keyed, so it carries none of the same-IP collision
risk of matching on address, and it slots in *after* the live-session check so
no existing precedence changes (every pre-existing `TestEnsureFallback*`
port-resolution test passes untouched).

With both, the TCP path now works end to end: the LAN candidate is seeded like
any other address, `initSeedTick`'s UDP attempt fails fast (immediately, with
`errNoUDP`, when UDP is off locally), the seed cools down, and `ensureFallback`
dials that same LAN address over TLS at the peer's advertised port. The
relay-upgrade path reaches `ensureFallback` immediately rather than waiting for
that cycle, via the fix in v369 — which is what makes an existing *relayed*
session between two same-NAT nodes upgrade straight to a direct TCP one.

Worth noting explicitly: the futile UDP handshake cycle that precedes the TCP
dial is *load-bearing*, not just harmless. `tryRelays` gates relaying on
`ns.seedBackoff` having an entry for the target's endpoint ("direct has
demonstrably failed"), and `planHandshake` sets that entry on its own clock
regardless of whether the send actually left the host. Short-circuiting the
UDP attempt when UDP is disabled — an obvious-looking optimization — would
therefore mean `seedBackoff` never gets populated, `hasBackoff` never returns
true, and **relaying would stop working entirely on a UDP-disabled node**.
Left alone deliberately; noting it here so it isn't "cleaned up" later.

- `internal/mesh/localcand.go`: fallback-port advertisement when UDP is off.
- `internal/mesh/handshake_engine.go`: `tcpPortForEndpoint` owner lookup.
- `internal/mesh/localcand_test.go`:
  `TestLocalEndpointsFallBackToTCPPortWhenUDPDisabled` (UDP on → UDP port; UDP
  off + TCP on → fallback port; both off → nothing) replaces v374's
  now-incorrect "empty when UDP disabled" test, and
  `TestTCPPortForHostCandidateUsesOwnersAdvertisedPort` pins the resolution with
  the peer on 8443 and us on 443, so a regression to "our own port" fails loudly
  instead of passing by coincidence.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, full
  repo suite including a clean verbose run of all of `internal/mesh`.

---

---

## v374 — 2026-07-13

"gn-cush1 and gn-cush2 are direct neighbours in the same subnet with no
firewall between them. There should be absolutely no relaying going on
between those two hosts." Correct, and the reason turned out to be an
architectural gap rather than a policy one — which is also why the earlier
framing ("configured seeds should never relay") was aiming at the right
target with the wrong weapon. Banning relay for seeds would have left those
two nodes simply DEAD instead of relayed. The actual problem is that they
could never find each other in the first place.

A node's underlay endpoint was only ever *observed*, never *declared*. A peer
records the source address a packet arrived from; a third party gossips what
it observed. Nothing a node knew about its own addresses ever reached the
mesh — `net.InterfaceAddrs()` appears exactly once in the codebase, in
`reflexive.go`, and only to classify NAT type. Across the internet that's
fine: the observed (server-reflexive) address is the one everyone else must
dial anyway. For two nodes behind the *same* NAT it fails completely. Every
outside observer sees both at the one shared public address, so the only
candidate either ever learns for the other is that public address; dialing it
from inside requires NAT hairpin, which plenty of gateways don't implement.
The handshake fails, the seed cools down, and `tryRelays` — entirely
correctly, by its own rule — concludes direct has demonstrably failed and
relays them through a node on the far side of the internet. Two machines on
the same switch, and the LAN address that would have connected them instantly
was known to nobody but themselves, because nothing ever advertised it.

This adds host candidates (ICE's term), self-declared rather than observed.

- `internal/mesh/localcand.go` (new): `Engine.localEndpoints()` enumerates this
  node's interface addresses and pairs them with its primary UDP port.
  Loopback, link-local, multicast and unspecified are filtered out (a peer
  seeding our `127.0.0.1` would dial its *own* loopback; link-local needs a
  zone this encoding doesn't carry). Private ranges are emphatically kept —
  they're the whole point. Capped at `maxLocalEndpoints` (8) so a multi-homed
  box with a pile of bridge/VPN interfaces can't make every peer spray
  handshakes at a dozen dead ends, and sorted deterministically so an
  unchanged interface set doesn't churn `peerListSig` and re-flood gossip
  every tick. Empty when UDP is off (port 0), since a candidate nobody can
  dial over UDP is worthless. `addLocalCandidates` registers a peer's
  candidates as ordinary seeds via `AddSeedFor` — re-filtered on receipt,
  because a peer's claims get no more trust than our own.
- `internal/mesh/handshake.go`: `hsPayload.LocalEndpoints`, encoded as one
  more optional trailing field (`appendEndpointList`/`readEndpointList`),
  nested exactly like the extra-port lists so an older peer's shorter payload
  leaves it nil instead of erroring.
- `internal/mesh/control.go`: `peerEntry.localEndpoints`, carried by
  `buildPeerList` and encoded as a new trailing block (`peerListLocalBlock`,
  0x04) after the extra-port blocks — the decode loop already stops at an
  unrecognized marker, so older decoders are unaffected. **This is the leg
  that actually fixes the reported case**: two nodes behind one NAT never
  observe each other at all, so neither can learn the other's LAN address
  first-hand — it can only arrive through a mutual peer's gossip. `learnPeers`
  seeds them, and deliberately does so even when already connected, unlike the
  observed-endpoint seeding right above it: a same-NAT peer is precisely the
  case where we're most likely to be sitting on a working *relayed* session,
  and skipping the candidate because "connected" is true would skip the one
  thing that could upgrade it. `install()` registers a direct neighbour's
  candidates for the same reason. `peerListSig` now includes them, so a change
  in candidates re-floods.
- `internal/mesh/engine.go`, `cmd/gravinet/main.go`: the engine never knew its
  own UDP port (a peer's is normally learned by observation — the very
  mechanism this works around). Added `Options.PrimaryPort` and
  `SetPrimaryPort`, wired at construction and on both live port-change paths
  (including UDP being turned off, which stops the advertisement).
- Tests (`internal/mesh/localcand_test.go`): candidate filtering (loopback and
  link-local rejected, private ranges kept); no candidates when UDP is
  disabled; `hsPayload` round trip *and* an older peer's payload decoding
  cleanly to none; peer-list round trip carrying a LAN candidate alongside the
  shared public endpoint, plus a candidate-free list not growing a block;
  a gossiped candidate becoming an owner-attributed seed, with a peer's
  loopback claim ignored; and our own candidates never seeded back to us.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, full
  repo suite (`go test -short ./...`), including a clean verbose run of the
  whole `internal/mesh` package given this touches two wire formats.

Expected effect on the reported mesh: cush1 and cush2 advertise their
`192.168.55.x` addresses, a mutual peer gossips them across, each seeds the
other's LAN address, and `initSeedTick` dials it on the next tick —
upgrading the relayed session to direct in place (`install()` carries the
established time forward, so it isn't even a reconnect). The same mechanism
should collapse relaying between any other co-located pair in that mesh.

Known limitation, not addressed here: this is host-candidate discovery only,
not full ICE. There is still no UDP hole-punching for two nodes behind
*different* symmetric NATs with no LAN path between them — for those, relay
remains the correct and only answer, which is what `reflexive.go`'s
`symmetric` classification has always been telling you.

---

---

## v373 — 2026-07-13

After applying v372 and switching networks again, `meshping` on mcfed came
back with gn-ionos1/2/3 **ALIVE** and everything else (gn-cush1, gn-cush2,
gn-freebsd, gn-openbsd, gn-win11, macmini) still DEAD. That split is the
whole diagnosis: every peer mcfed can reach *directly* recovered — which is
what v371/372's explicit-seed pinning was for, and it worked — and every
peer that can only be reached *through a relay* did not. So the remaining
failure is relay establishment, not seeding.

Which exposes a genuine hole. `bestRelay` picks a relay from among connected
peers that gossip says know the target — but it has no idea whether a
candidate will actually *carry* the traffic. `allow_relay` (config, per
network; `ns.spec.AllowRelay`) is enforced only on the intermediary, in
`onRelay`, as a bare silent `return`. It was never advertised anywhere. So a
node whose only reachable peers have `allow_relay` disabled will: pick one
as its relay, send it a relayed handshake, have it silently dropped, time
the pending out after `relayPendingTTL`, pick the same peer again, and
repeat forever — every peer behind that relay permanently unreachable, with
**not one log line on either end** saying why. From the outside it is
indistinguishable from "no peer knows the target" or "the relayed handshake
is failing." That silence is precisely what makes the state mcfed is in so
hard to diagnose, and it's at its worst in exactly the situation that
produced it: a node that moves onto a network where only its public seeds
answer, and those seeds happen not to relay.

- `internal/mesh/handshake.go`: `hsPayload` gains `AllowRelay`, advertised in
  the handshake so a peer knows before committing to us as its relay. Encoded
  as *two* flag bits, not one — `flagRelayKnown` (1<<4) and `flagAllowRelay`
  (1<<5). A node predating the field sets neither, so "no bits" decodes as
  *unknown* rather than as a refusal; a single bit would have made every
  upgraded node instantly stop relaying through every not-yet-upgraded one
  for the whole duration of a rolling upgrade. Unknown keeps the old
  optimistic behavior (assume willing, try it). Both bits are free in the
  peer-list encoding too (which uses only 0–3), and adding flag bits changes
  no field lengths, so older decoders are unaffected.
- `internal/mesh/relay.go`: new `peerSession.willRelay()` — true unless the
  peer explicitly advertised a refusal. `bestRelay` skips refusers (preferring
  a willing peer even when a refuser scores better on RTT) and now also
  returns how many otherwise-suitable candidates it skipped for that reason,
  which is what lets `tryRelays` distinguish "nobody knows this target" from
  "everyone who knows it has allow_relay off."
- Two new warnings, both throttled to one per 5 minutes per target (and per
  src→dst pair) so a persistent misconfiguration doesn't spam every tick:
  `logRelayRefused` on the node that can't get through ("no direct path, and
  all N connected peer(s) that know it have allow_relay disabled"), and
  `logRelayDeclined` on the refusing intermediary itself ("declining to relay
  X → Y: allow_relay is disabled on this node"). Either one alone would have
  turned this into a five-second diagnosis.
- Tests (`internal/mesh/relay_allow_test.go`): `AllowRelay` round-trips in
  both states and `RelayKnown` is always set by a node new enough to encode
  it; `willRelay` treats a pre-advertisement peer as willing (the
  rolling-upgrade guard) and only honors an explicit no; `bestRelay` skips
  refusers, prefers a willing peer over a better-scoring refuser, returns nil
  when all refuse, and does *not* count a peer that simply doesn't know the
  target as a "refuser" (which would produce a misleading warning). The
  existing end-to-end `TestRelay` and all `TestBestRelay*`/`TestRelayBetter*`
  scoring tests pass unchanged.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, full
  repo suite (`go test -short ./...`). Note: one `internal/mesh` run failed
  once and then passed on three consecutive re-runs, including two full
  verbose ones, with no `--- FAIL` in any of them; the failing test wasn't
  captured. The new tests here are deterministic (no timers, no network), so
  they aren't the source, but the flake is recorded here rather than papered
  over.

This does not *prove* `allow_relay` is what's wrong on that mesh — that's a
config question only the operator's ionos nodes can answer, and it's now a
one-line log check. What it does fix is that gravinet could get into this
state at all without saying a word, and that it would keep choosing a relay
it had already been told, in the handshake, would never work.

---

---

## v372 — 2026-07-13

Closes the gap v371 called out as a known limitation and deliberately left
for its own change: `sweepDeadSeeds` could still permanently evict an
operator-configured seed.

v371 stopped `install()` from *pruning* explicit seeds, which fixed the roam
case (a seed that worked before the network changed is marked
`everConnected` for the life of the process, so it was already safe from
this sweep). But `sweepDeadSeeds` guards on `everConnected`, which only ever
gets set by a *successful* session — so a configured seed that has never
once connected in this process was still fair game after `deadSeedGrace`.
That's the right call for a gossip-learned address (it was only ever a
guess, and gossip will simply re-learn it if it's still valid) and the wrong
one for a configured address, which nothing else can ever re-add.

The failure mode is a cold start on a network where the configured seeds
happen to be unreachable — booting on cellular, coming up before the VPN or
Wi-Fi is ready, an upstream outage at boot, a laptop opened somewhere new.
All the seeds get evicted after `deadSeedGrace`, and then they are never
retried again: the node sits permanently dead with an empty seed list even
once the network comes back, and the only way out is a config reload or a
restart. That's precisely backwards — an explicit seed is the operator's
standing instruction for how to get back onto the mesh, so it has to stay
dialable exactly when nothing else is.

- `internal/mesh/handshake_engine.go`: `sweepDeadSeeds`'s keep condition
  gains `|| ns.explicitSeed[s]`. Retries for a pinned seed are already paced
  by `ns.seedBackoff`, so this costs one handshake attempt per
  `seedRetryBackoff`, not a busy loop. A gossip-learned seed that never
  connected is still evicted exactly as before.
- `internal/mesh/handshake_flap_test.go`: new
  `TestSweepDeadSeedsNeverEvictsExplicitSeed` — an explicit seed and a
  gossip-learned one, both never-connected and both well past
  `deadSeedGrace`; the configured one must survive, the learned one must
  still go. The existing `TestSweepDeadSeedsRemovesNeverConnected` passes
  unchanged, confirming the eviction path itself is intact.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, and
  the full repo test suite (`go test -short ./...`) — all clean.

Together with v370 (explicit seeds get retry priority over gossip-learned
peers) and v371 (`install()` never prunes them), `explicitSeed` now means
what an operator would reasonably assume a configured seed means: it is
never dropped, never throttled out of retrying, and always dialable — no
matter what the mesh has learned, forgotten, or failed to reach since.

---

---

## v371 — 2026-07-13

Report: switching a node from home Wi-Fi to 5G no longer renegotiates —
`meshping` on mcfed showed every peer, v4 and v6, DEAD, and it stayed that
way. This used to work. Found a concrete mechanism that produces exactly
that, and it is a real bug, not a topology problem.

`ns.seeds` is the only list `initLoop` ever dials, and it holds two very
different kinds of entry with nothing distinguishing them: the operator's
configured bootstrap seeds (`NetSpec.Seeds`) and endpoints learned
dynamically from gossip (`AddSeedFor`). `install()`'s stale-seed pruning —
added to stop the seed list growing without bound as a NAT-rotating peer
accumulates a new gossiped endpoint on every port change — drops every seed
it can attribute to the connecting node (`seedOwner`) other than that node's
current endpoint. It could not tell the two kinds apart, so a *configured*
seed was dropped like any other the moment gossip attributed it to a node
and that node was later seen at a different endpoint — a NAT rebind or roam
on that peer's end, which over a long uptime happens to essentially every
peer eventually. Nothing puts it back short of a config reload's seed merge
or a restart.

That drift is completely invisible while the local node stays put: the
learned endpoints it's left with all work. It becomes fatal the moment the
node changes its own underlay. On the new network every learned endpoint is
unreachable, `pruneDead` clears the sessions after `peerTimeout` (20s), and
`initLoop` is left re-dialing a list of addresses that can no longer answer
— with the one set of addresses the operator specifically chose as stable,
reachable anchors long since deleted from it. The node has no way back and
sits dead until restarted, which is precisely "it used to auto-negotiate the
move and now it doesn't."

The unbounded-growth problem the pruning exists to solve is purely a
gossip-learned one — `NetSpec.Seeds` is a small, fixed, operator-authored
set that never grows on its own — so exempting explicit seeds costs nothing
the pruning was actually there to prevent.

- `internal/mesh/handshake_engine.go`: `install()`'s prune loop now skips any
  seed in `ns.explicitSeed` (the operator-configured set introduced in v370
  — which is what made this fix a one-line condition rather than a new
  concept). A gossip-learned stale entry for the same node, on the same IP,
  is still pruned exactly as before; a configured seed is pinned, and keeps
  its `seedOwner` attribution rather than having it deleted.
- `internal/mesh/handshake_flap_test.go`: new
  `TestInstallNeverPrunesExplicitSeeds`, alongside the existing
  `TestInstallPrunesOwnedStaleSeeds` (which still passes unchanged — the
  growth case is still pruned) and
  `TestInstallDoesNotPruneUnattributedOrOtherNodesSeeds`. It pins a
  configured seed and a gossip-learned seed for the same node on the same
  IP, connects that node at a third endpoint, and asserts the configured one
  survives and the learned one doesn't.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, and
  the full repo test suite (`go test -short ./...`) — all clean.

Known limitation, not fixed here: `sweepDeadSeeds` can still evict an
explicit seed that has *never once* connected in `deadSeedGrace` (it's
guarded by `everConnected`, not by `explicitSeed`). That's harmless for the
roam case above — a seed that worked before the move is permanently marked
`everConnected` for the life of the process — but it does mean a daemon
that *starts* on a network where its configured seeds are unreachable will
eventually stop retrying them until a config reload or restart. Worth
revisiting separately; it's a different failure mode from this one and
wanted its own change rather than being folded in here.

---

---

## v370 — 2026-07-13

Follow-up to v369's mesh-peers thread: gn-cush1, gn-cush2, gn-ionos1,
gn-ionos2, and gn-ionos3 are all explicitly defined as seeds — pushback was
"one could argue they should never relay." Checked: they currently get zero
special treatment for it, which is a real gap, though "never relay" turned
out to be the wrong fix — "relay is a last resort, not permanently
throttled out of retrying direct, for a node the operator specifically
configured" is what actually holds up.

`ns.seeds` never distinguished *why* an address is in it — an entry from
`NetSpec.Seeds` (the operator's own config) and one added later via
`AddSeedFor` (a gossiped peer-list entry) are the identical `netip.AddrPort`
in the identical slice, with nothing recording which is which. So once a
seed's owner is relay-connected, `seedOwnerNeedsUpgrade` throttled *every*
upgrade attempt to once per `directUpgradeInterval` (5 minutes) — deliberate
and correct for a peer only ever learned about through gossip (its own doc
comment: "no urgency at all," since relay already has it covered), but the
same throttle was being applied, with no way to opt out, to a node the
operator explicitly told gravinet to reach directly.

- `internal/mesh/engine.go`: `netState` gains `explicitSeed`
  (`map[netip.AddrPort]bool`, addresses that came from `NetSpec.Seeds`) and
  `explicitSeedNode` (`map[string]bool`, promoted from it the moment an
  address's owning node ID becomes known — via `seedOwner`, from either
  gossip or a direct connection). Node-keyed, not just address-keyed, so
  the priority survives the node roaming onto a different address later —
  same reasoning `seedOwner` itself already uses for why it's node-precise.
  `addSeed` takes a new `explicit bool` and promotes in whichever order the
  two facts arrive — address marked explicit before the owner is known (the
  common case, since `NetSpec.Seeds` is present from network construction,
  before any connection), or the owner already known before a later config
  reload (re)affirms the address as explicit — either way ends up promoted,
  covered by `TestAddExplicitSeedPromotesOwnerNode`. New
  `Engine.AddExplicitSeed` (`AddSeed` + the explicit flag); `AddSeed` and
  `AddSeedFor` are unchanged in behavior, just now pass `explicit=false`
  through the shared internal `addSeed`.
- `internal/mesh/reload.go`: the config-seed merge loop (`for _, s := range
  spec.Seeds`) now calls `AddExplicitSeed` instead of `AddSeed`, so seeds
  reaffirmed on every reload keep their priority rather than silently
  losing it the moment `ReloadRuntime` re-touches them.
- `internal/mesh/handshake_engine.go`: `seedOwnerNeedsUpgrade` skips
  `directUpgradeInterval` entirely when the owner is in `explicitSeedNode`
  — but isn't unthrottled to "every tick forever" either. It's paced by
  `ns.seedBackoff` instead, the same cooldown `planHandshake` already sets
  on this exact seed whenever a handshake attempt cycle exhausts — so an
  explicit seed gets retried at roughly the same cadence a not-yet-connected
  peer would (`handshakeRetry` per key, `seedRetryBackoff` between cycles),
  not the 5-minute gossip-peer throttle, and not a one-attempt-per-second
  hammer either. Covered by
  `TestSeedOwnerNeedsUpgradeSkipsThrottleForExplicitSeed`, the explicit-seed
  counterpart to the existing `TestSeedOwnerNeedsUpgradeThrottles`.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, and
  the full repo test suite (`go test -short ./...`) — all clean.

Known limitation, not fixed here: this is still an *opportunistic* upgrade,
same as before — nothing tears down a working relay session to force a
retry, and a seed whose direct/fallback path is genuinely, currently
unreachable will still show relayed for as long as that's true, just
retried far more persistently than a gossip-only peer would be. If
gn-cush1/ionos1/2/3 are still relayed after this, that's now much stronger
evidence of an actual reachability problem on the wire rather than of the
retry cadence being too gentle to have noticed a fix yet.

---

---

## v369 — 2026-07-12

Follow-up question while looking at a Mesh Peers screenshot with several
peers stuck relayed through nodes that were themselves directly reachable:
"what if I turned UDP off with '-' in the settings port box?" Traced
through and found a real gap — reproduced and fixed.

Once a peer is relay-connected, `initLoop`'s only remaining retry path
toward it is `seedOwnerNeedsUpgrade`'s throttled "try a fresh, independent
handshake" attempt (immediately after relay connects, then once per
`directUpgradeInterval`, 5 minutes). That attempt went straight to
`planHandshake`/`e.send` — the plain UDP path — and never called
`ensureFallback`, unlike the ordinary "not yet connected at all" branch
just below it, which already tries UDP and the TCP/TLS fallback in
parallel while a seed is cooling down. The backoff branch that normally
calls `ensureFallback` is unreachable once `connectedToSeedOwner` is true:
every UDP-endpoint variant gossip re-registers for that owner (`AddSeedFor`,
`internal/mesh/control.go`) still routes into the same relay-owner branch,
never the backoff one. With UDP genuinely unreachable — including turned
off entirely via `Config.PrimaryPort == 0` (v366's `-` port setting) —
`Dual.Send` returns `errNoUDP` immediately (`internal/transport/dual.go`),
so every throttled upgrade attempt failed instantly, forever, and nothing
ever tried the TCP/TLS path that might have actually worked. A relayed
peer could stay relayed permanently even with fully working TCP/TLS
fallback connectivity to it.

- `internal/mesh/handshake_engine.go`: factored `initLoop`'s per-seed
  decision out into `initSeedTick(ns, seed, backoff, now)` — same logic,
  now directly unit-testable without waiting on the real 1s ticker. The
  relay-owner branch now also calls `e.ensureFallback(ns, seed)` alongside
  the existing UDP attempt, exactly mirroring what the not-yet-connected
  branch already does. `ensureFallback` already no-ops once a fallback
  connection exists or is already being dialed (`ns.dialing`,
  `internal/mesh/handshake_engine.go`), so this costs nothing extra on a
  throttled retry when UDP is working fine — it only matters when UDP
  isn't.
- `internal/mesh/directupgrade_test.go`: new
  `TestUpgradeAttemptAlsoTriesFallback`, the fast isolated counterpart to
  the existing `TestRelayedConnectionUpgradesToDirect` (whose in-memory
  switchboard harness has no `fallbackDialer` at all, so it couldn't have
  caught this). Sets up a seed whose owner is relay-connected, calls
  `initSeedTick` directly, and confirms the fallback gets dialed — using
  the same `fakeFallback`/polling pattern `fallback_test.go`'s
  `TestEnsureFallbackDialsAndSeeds` already established.
- Verified: `go build` for `linux` and `windows/amd64`, `go vet ./...`, and
  the full repo test suite (`go test -short ./...`, `-short` skips a couple
  of pre-existing, deliberately multi-minute real-time reproductions in
  `internal/mesh` unrelated to this change) — all clean.

Known limitation, not fixed here: this is still throttled to one attempt
per `directUpgradeInterval` (5 minutes) per relayed peer, same as before —
a peer that only just became relay-connected still gets one immediate free
attempt, but after that a genuinely-fixed direct/fallback path can take up
to 5 minutes to be noticed. That throttle is deliberate (see
`directUpgradeInterval`'s own doc comment on why it's gentler than a truly
failing seed's retry cadence) and orthogonal to the bug fixed here, which
was that the retry — whenever it did fire — could never have succeeded via
TCP/TLS at all.

---

---

## v368 — 2026-07-12

Follow-up to v367. That fix stopped the redundant kernel-NAT reprogram from
running as a side effect of edits that were never NAT edits at all — but a
web-admin edit that *does* change NAT rules on a Windows node was still a
genuine ~30s wait on the response, because `applyKernelNAT` (`cmd/gravinet/main.go`)
called `nfMgr.Apply` inline, and it runs from `reloadFn`, which
`mutateConfig` calls synchronously, which the HTTP handler for the edit
waits on before it can respond. Asked directly whether that could just run
asynchronously instead — it can, and the shape to copy was already sitting
right there: `reloadFn`'s own network-teardown loop, a few dozen lines
below this same block, already runs `RemoveNetwork` in its own goroutine
rather than making the caller wait on it, for the identical reason (its own
comment: "the person clicking 'disable' should have to sit through" a
teardown that's "a real, user-visible amount of time ... on Windows").

- `cmd/gravinet/main.go`: split the old `applyKernelNAT` in two.
  `natApplyNow(rules)` is the actual create-manager-if-needed +
  skip-if-unchanged + `Apply` logic (the skip-if-unchanged check from v367
  moved here, now compared against `lastNATRules` under a new `natMu`
  mutex rather than left unguarded). It's called directly, synchronously,
  exactly once at startup — nothing is being served yet at that point, so
  there's nothing to unblock — and is otherwise only ever called from a
  single long-lived background goroutine reading off `natApplyCh`, a
  capacity-1 channel. The new `applyKernelNAT` that `reloadFn` calls just
  computes the ruleset and sends it to that channel, draining and replacing
  whatever's already pending if the worker is still busy with a previous
  apply — so a burst of rapid edits converges on the *last* desired
  ruleset instead of working through a backlog of ones already superseded
  (each of which would otherwise cost its own 30s on Windows for nothing).
  Sends only ever come from `reloadFn`, itself only ever running one at a
  time under `mutateConfig`'s `cfgMu`, so they're already strictly ordered;
  only the worker's own consumption can lag behind, and `natApplyNow`'s
  unchanged-check makes it idempotent even if a stale duplicate ever gets
  through regardless.
- `natMu` also now guards the shutdown path's read of `nfMgr` before
  `nfMgr.Clear()`, since the worker goroutine can touch `nfMgr` concurrently
  with shutdown in a way the old single-goroutine version never had to
  account for. The lock is only ever held around the read/write of `nfMgr`/
  `lastNATRules` themselves, never across the slow `Apply`/`Clear()` calls,
  so it can't turn into the same kind of stall this whole change exists to
  remove.
- On the UI side nothing changes: `toggleTagState`'s optimistic tag-flip
  (`internal/webadmin/ui.go`) already assumed a NAT-touching edit might take
  a while to actually land on a Windows host, so that part of the contract
  was already correct — this just makes it true end to end instead of true
  everywhere except the one path that actually is NAT.
- No new test: as with v367, this is private state inside `main`'s
  long-lived setup, not something the existing black-box `cmd/gravinet`
  tests reach. Verified by building for both `linux` and `windows/amd64`
  (`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`), `go vet`, a
  `-race` build of `cmd/gravinet` (needs `libpam0g-dev` for cgo on this
  host), and `go test -race -count=1 ./cmd/...`, all clean.

Known limitation, not fixed here: the startup apply is still synchronous, so
a Windows host with existing NAT rules still takes its one-time ~30s at
daemon start before the process finishes its startup sequence — unchanged
from every version before this one, and a one-time cold-start cost rather
than something any GUI interaction waits on.

---

---

## v367 — 2026-07-12

Fixed a report that every element in the web admin — not just NAT or
firewall controls, literally anything with a double-click toggle or a
save — was "super slow to react" on Windows nodes specifically, while the
same UI stayed snappy on Linux/macOS/*BSD nodes.

The web admin already knew some host operations are much slower on Windows
than elsewhere — that's the whole reason `toggleTagState` (`internal/webadmin/ui.go`)
flips a tag instantly and fires its API call in the background instead of
waiting on it, per that function's own doc comment about WinNAT's
PowerShell-driven backend taking on the order of 30s where Linux's netlink
calls are near-instant. That comment turned out to be describing a real cost
that was being paid far more often than it needed to be: `reloadFn`
(`cmd/gravinet/main.go`), which runs at the end of *every* config-mutating
web-admin request via `mutateConfig`, ended with an unconditional
`applyKernelNAT(newCfg)` — re-deriving the kernel NAT ruleset from config and
calling `nfMgr.Apply(rules)` regardless of whether anything NAT-related had
actually changed. Renaming a network, toggling a firewall exemption, editing
a DNS entry — none of them touch NAT, but all of them ended their request by
reprogramming it anyway. `Apply` itself has no short-circuit for "nothing
changed" (confirmed for both backends: `netfilter_linux.go`'s nft/iptables
path and `netfilter_windows.go`'s `runPS`, which always shells out); the
caller was relying on file-level diffing that didn't exist. On Linux that
redundant call is a cheap no-op reprogram via netlink; on Windows it's a full
PowerShell round-trip through WinNAT every time — and since `applyKernelNAT`
runs synchronously inside the same call chain the HTTP handler is waiting on
before it can respond (`reloadFn` → `mutateConfig` → `s.editResult`), that
~30s landed on the response to every single edit, not just NAT ones.

- `cmd/gravinet/main.go`: `applyKernelNAT`'s closure now keeps `lastNATRules`
  (the `[]netfilter.Rule` most recently handed to `nfMgr.Apply`) alongside
  the existing `nfMgr` variable, and skips the `Apply` call when
  `slices.Equal(rules, lastNATRules)` — `netfilter.Rule` has no slice/map
  fields, so it's directly comparable and `slices.Equal` is a real deep
  comparison, not a length check. A manager that doesn't exist yet, or a
  ruleset that actually changed (including changing to empty, which still
  needs to reach the backend to clear whatever was applied before), still
  applies exactly as before; a failed `Apply` deliberately leaves
  `lastNATRules` untouched so the next reload retries rather than believing
  a change landed that didn't.
- No test added for this specific closure — it's private state inside `main`'s
  long-lived setup, not something `cmd/gravinet`'s existing black-box tests
  reach. Verified instead by building for both `linux` and `windows/amd64`
  (`CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`) and running the
  full `cmd/gravinet` suite (`go test -count=1 ./cmd/...`), both clean.

Known limitation, not fixed here: this only removes the *redundant* 30s hit.
An edit that's actually a NAT change on a Windows node is still genuinely a
~30s PowerShell round-trip, same as it always was — that cost is real
WinNAT latency, not a bug, and toggleTagState's optimistic-UI pattern is
still what hides it from that toggle itself. What this fixes is every *other*
toggle no longer paying it too.

---

---

## v366 — 2026-07-12

Added the ability to turn off UDP or the TCP/TLS fallback entirely, each
independently, by typing `-` into that port field in the web admin's
Underlay settings (previously the field just reverted an empty value — "at
least one port is required" was hard-enforced with no way out). Requested
directly: an operator wants to run UDP-only or TCP-only, and there was no
way to actually stop the daemon from listening on the other transport at
all, not even a config-file-only escape hatch.

The TCP/TLS fallback side turned out to already be fully wired end to end —
`Config.DisableTCPFallback` and `TCPFallbackEnabled()` were fully plumbed
through daemon startup and the live-reload path in `cmd/gravinet/main.go`
(`if cfg.TCPFallbackEnabled() { ... }` gates opening the TLS listener at
startup; the reload loop already has a complete `wantTCP == 0` branch that
tears the live listener down) — it just had zero exposure through the web
admin. Wiring it up was a small, safe change.

The UDP side needed more: `Config.PrimaryPort` had no "off" representation
at all (`Validate` flatly rejected anything `<= 0`), and — more importantly —
`transport.Dual.Send`, the sender both underlays share, unconditionally
called `d.UDP.Send(...)` with no nil check, unlike its already-nil-safe `d.TLS`
handling. Setting `Dual.UDP` to nil the same way `Dual.TLS` already can be
would have nil-pointer-panicked the daemon on literally the first send
attempt to any peer. Traced every other direct use of the raw
`*transport.Transport` (outside its own package, just three call sites in
`main.go` — the `Dual` construction itself, a shutdown `.Close()`/`.Stats()`
pair, and a port-change grace-close goroutine) before touching anything, to
make sure nothing else would panic the same way.

- `transport.Dual.Send` (`internal/transport/dual.go`) now nil-checks `UDP`
  the same way it already nil-checks `TLS`, returning a new `errNoUDP`
  sentinel instead of dereferencing. A send that fails this way behaves
  exactly like a peer that's merely unreachable over UDP right now — the
  existing seed-backoff-then-`ensureFallback` mechanism in
  `handshake_engine.go` (built for a peer behind a UDP-blocking firewall)
  picks it up and dials the TCP/TLS fallback without any new logic, typically
  within a couple of seconds (`handshakeRetry`, 2s per key attempt) of a cold
  seed dial. `Dual.Close` was already nil-safe for both fields; left as is.
- `Config.PrimaryPort == 0` is now the sentinel for "UDP off" (it has
  `json:"primary_port"` with no `omitempty`, so it's always written
  explicitly — no ambiguity with an old config that simply never set it).
  `Validate` accepts 0 only when the TCP fallback is enabled; both off at
  once is refused with a specific error, since the node would then have no
  way to be reached at all.
- `cmd/gravinet/main.go`: the initial `transport.Open` call is now gated on
  `cfg.PrimaryPort > 0`, leaving `tr` nil otherwise — the same pattern
  already used for `tlsTr`. The live-reload block gained a
  `newCfg.PrimaryPort == 0` branch, structured identically to the existing
  TCP-fallback on/off branch just below it (grace-close the old socket,
  `reattach()` with a nil `Dual.UDP`, log it). The shutdown path's
  `getTr().Close()` / `getTr().Stats()` gained the same nil guard `getTLS()`
  already had.
- `handlePort` and `handleTCPPort` (`internal/webadmin/edit.go`) accept
  `{disabled:true}` as an alternative to a port list — each refuses to
  disable its own transport while the other is already off (a specific,
  actionable error; `Config.Validate` is the backstop). Disabling UDP clears
  `ExtraListenPorts` too; disabling the TCP fallback leaves
  `TCPFallbackPort`/`ExtraTCPListenPorts` alone so they're remembered if
  re-enabled later.
- The config JSON response gained `tcp_fallback_disabled` (`webadmin.go`) —
  needed because `TCPFallbackPortValue()` always resolves to a real port
  (defaulting to 65432), so the UI can't tell "off" from "using the default"
  by the port number alone the way it can for UDP (`primary_port` is 0 only
  when actually off).
- `buildPortListRow` (`internal/webadmin/ui.go`) renders `-` when disabled
  and sends `{disabled:true}` when the field is saved as exactly `-`, for
  both the UDP and TCP port rows; also merged a stale duplicated doc comment
  above it left over from an earlier edit (same kind of leftover as the one
  cleaned up in `seedEdit` last version) into one accurate one.
- Tests: `TestValidatePrimaryPortZeroRequiresTCPFallback`
  (`internal/config`) covers the Validate invariant directly.
  `TestDualSendNilUDP`/`TestDualCloseNilUDP` (`internal/transport`) are the
  regression guard for the nil-panic fix specifically. `internal/webadmin`'s
  `TestHandlePortDisableInteraction` exercises the full disable/re-enable/
  mutual-refusal cycle through the actual HTTP handlers.

Known limitation, not fixed here: the first connection attempt to any seed
made shortly after UDP is disabled still spends one `handshakeRetry` cycle
(default 2s per configured key) attempting UDP before backing off into the
TCP fallback, since nothing proactively skips straight to TCP when UDP is
known to be off — it just relies on the existing reachability-detection
path reacting to a `Send` failure that now happens immediately instead of
timing out. A small, one-time cold-start delay per peer, not a connectivity
gap.

---

---

## v365 — 2026-07-12

Fixed a real data-loss bug in the web admin's Seeds editor, reported as two
separate-looking symptoms that turned out to share one root cause: editing a
seed's address/port and hitting Enter made the row appear to vanish, and
flipping a seed's transport (udp \u2194 tcp) silently cleared its notes.

Both `seedEdit` (double-click the address cell) and `seedSetProto`
(double-click the transport cell) implemented "change this seed" the same
way: `POST /api/seed {op:'add'}` with the new address, then `{op:'remove'}`
the old one, on the theory (stated in the old comment) that this dials the
new address live before dropping the old. `SeedAdd` always starts a brand
new `Seed{Address: addr}` \u2014 empty `Notes`, appended at the end of the
network's seed slice \u2014 so every single one of these edits wiped that
seed's notes and moved its row to the bottom of the table. With more than a
handful of seeds configured, a re-addressed row landing off-screen at the
bottom reads exactly like "it disappeared."

Turned out neither the notes-loss nor the reordering was actually necessary
to get live effect: `ReloadRuntime`'s seed handling (`internal/mesh/reload.go`)
is additive-only \u2014 it ranges over `spec.Seeds` and dials whatever isn't
already dialed, and a removed address is explicitly left dialed until restart
either way, per its own comment. So an in-place rename dials the new address
on the next reload exactly as add-then-remove did, with neither side effect.

- New `Config.SeedUpdateAddr(netName, oldAddr, newAddr)` in
  `internal/config/ops.go`: finds the seed by its current address and mutates
  `.Address` in place \u2014 same slice index, `.Notes` untouched. Rejects an
  unknown old address, a collision with a different existing seed (would
  otherwise silently merge two entries), and an invalid new address (same
  validation `SeedAdd` already used).
- New `update-addr` op on `POST /api/seed` (`internal/webadmin/edit.go`),
  taking `addr` (old) and `newAddr`.
- `seedEdit` and `seedSetProto` in `internal/webadmin/ui.go` now call
  `update-addr` once instead of `add` then `remove` \u2014 also cleaned up a
  stray duplicated comment block above `seedEdit` left over from an earlier
  edit that was never removed.
- Tests: `TestSeedUpdateAddr` (`internal/config/seed_test.go`) covers notes
  and position preservation across an address rename and a transport-flip
  rename, plus the three error cases. `TestSeedAddRemoveLive` in
  `internal/webadmin/seed_handler_test.go` gained an `update-addr` case
  exercising the same thing through the actual HTTP handler.

Version bumped to 365.

---

---

## v364 — 2026-07-12

Added `meshping` — a small diagnostic script that walks the gravinet-managed
block(s) of the OS hosts file (`# BEGIN gravinet <tag>` / `# END gravinet
<tag>`, written by `internal/hosts`) and pings every entry, printing an
IP/hostname/ALIVE-or-DEAD table. It already existed as a POSIX `/bin/sh`
script (Linux, Darwin, FreeBSD, OpenBSD, plus a generic `uname` fallback,
each with its own ping-timeout-flag quirks already handled); this change
wires it into installation and adds a Windows equivalent.

- `install/meshping` now ships and installs the same way `pkgman` does:
  `install-linux.sh` and `install-macos.sh` drop it at
  `$PREFIX/bin/meshping`, `install-freebsd.sh` likewise, `install-
  openbsd.sh` at `$PREFIX/sbin/meshping` (OpenBSD keeps daemons/tools in
  sbin, same as its `pkgman`). All four installers' `--uninstall` paths,
  and their standalone `uninstall-*.sh` counterparts, remove it again;
  macOS also clears its quarantine xattr on install, same as it already
  does for `pkgman` and the main binary.
- New `install/meshping.bat`: a from-scratch Windows batch port, not a
  wrapper around a `.ps1` (unlike `install-windows.bat`/`uninstall-
  windows.bat`, which are thin elevation shims — meshping needs no admin
  rights, so it's a single self-contained file, mirroring the single-file
  POSIX script). Reads `%SystemRoot%\System32\drivers\etc\hosts`, tracks
  block membership with a *prefix* match on `# BEGIN gravinet` / `# END
  gravinet` rather than an exact-line match, so it still recognizes each
  network's individually-tagged block the same way the POSIX version's
  `sed -n '/# BEGIN gravinet/,/# END gravinet/p'` does by substring
  (missing this would have made it silently see zero hosts on any config
  with more than the default tag). Pings with `ping -4`/`-6 -n 1 -w 2000`
  per entry and renders the same `%-40s %-15s %s` column layout via
  manual pad-and-truncate, since batch has no `printf`. `install-
  windows.ps1` copies it beside `gravinet.exe` into `$InstallDir`; both it
  and `uninstall-windows.ps1` remove it again on `-Uninstall`/uninstall.

Version bumped to 364; no daemon behavior changed.

---

---

## v363 — 2026-07-11

Started from a screenshot question ("why relayed if directly accessible")
and ended up fixing a real, previously-undiscovered architectural gap: once
gravinet falls back to a relay for a peer, for *any* reason — even a single
dropped packet during the original handshake — it never tries the direct
path again, for as long as that relayed session keeps working. Not a
missed retry timing, a complete absence of the check. Traced every gate
that was supposed to allow reconnection:

```go
func (e *Engine) connectedToNode(ns *netState, nodeID string) bool {
    _, ok := ns.byNode[nodeID]
    return ok   // true for *any* session — relayed or direct, no distinction
}
```

`initLoop`'s seed loop and `tryRelays` both use exactly this shape of
check. Compounding it: `learnPeers` (gossip processing) stopped
registering a peer's endpoint as a fresh seed candidate as soon as *any*
session existed (`if !connected`), so a relay-only-connected peer's real
address could stop being offered as a dial target at all, not just stop
being dialed.

The fix, both pieces:

- `learnPeers`: the seed-registration gate is now `!directlyConnected`
  (connected via relay still counts as "keep offering fresh endpoint
  candidates"), not `!connected`. The `managed`/`manager`/`webPort` gossip-
  override gate right above it — deliberately not touched, that's a
  different, correct concern (don't let second-hand gossip override
  handshake-sourced truth) unrelated to seed candidacy.
- `initLoop`: new `seedOwnerNeedsUpgrade(ns, seed, now)` — true only when
  the seed's owner has a live session that's relay-only, and enough time
  (`directUpgradeInterval`, 5 minutes — a `var`, not `const`, so tests can
  shorten it) has passed since the last such attempt. On a true result,
  the loop falls through to the existing `planHandshake` path instead of
  skipping — a genuinely independent pending handshake, entirely separate
  from `ns.byNode`, so the working relay session is never touched unless
  and until the new attempt actually succeeds. Confirmed `install()`
  already handles that cleanly (verified by reading it, not assumed): it
  unconditionally overwrites `ns.byNode[nodeID]`, even carrying the
  original session's establishment time forward — nothing new needed there
  for the upgrade itself, only for triggering the attempt in the first
  place.

**Separately, fixed the actual UI bug from the screenshot**: a relayed
peer's `Endpoint` was showing the literal string `"invalid AddrPort"` —
confirmed exactly why: `peerSession.endpoint` is deliberately the zero
value for a relayed session (there's no direct underlay address to hold),
and the UI stringified it with no check for that. New `PeerInfo.RelayVia`
(the relay's hostname, or node id if no hostname is known) lets the UI
show "via `<relay>`" instead. This needed care, not just a find-replace:
`endpoint` is also used verbatim by the peer-info lookup feature's API
call and by seed-note address matching, both of which need the *real*
value (now sanitized to `''` for a relayed peer, not the placeholder
string, which is what actually makes the lookup gate and note matching
behave correctly) — so the display string lives in a new, separate
`endpointText` field, used only where a cell's visible content is
rendered, never passed to anything that treats it as a real address.

**Verified**: new `TestSeedOwnerNeedsUpgradeThrottles` (fast, no network)
covers every branch of the throttle logic directly — no owner, no session,
already-direct, first relay-only attempt, throttled retry, retry after the
interval elapses. New `TestRelayedConnectionUpgradesToDirect` is the real
end-to-end proof: two peers forced through a relay (a blocked direct path,
same harness as the existing `TestRelay`), confirmed relayed, then the
direct path opens — asserts the session upgrades to direct within the
(shortened, for the test) interval, *and* that a packet sent immediately
after still arrives, so this isn't just "the flag flipped," the connection
actually kept working through the transition. Existing seed-attribution/
dedup tests (`TestOutboundHandshakeAttributesSeedOwner`,
`TestConnectedToRecognizesResolvedFallback`,
`TestConnectedToDoesNotFalsePositiveAcrossPeersOnSameIP`, the
`TestInstall*`/`TestSweepDeadSeeds*` family, `TestRelay`,
`TestSelfHandshakeIsRejected`) all re-run clean — this didn't disturb any
of the existing gating logic they depend on. `node --check` on the
extracted UI script. Full -short suite green across all 16 packages, both
cross-compiles clean.

---

## v362 — 2026-07-11

An unresolved investigation, documented honestly rather than closed with a
fix that hasn't been found: a real report that a node (`gn-ionos1`) with a
seed pointing at another node that's since been shut down (`gn-cush1`)
stops responding to an *unrelated*, otherwise-healthy direct peer
(`mcfed`) — not a slow failover, a hang that doesn't clear on its own; only
restarting `gn-ionos1` fixes it.

Read every plausible mechanism in the retry path: `initLoop`'s per-seed
loop (releases `ns.mu` before any network I/O), `planHandshake`'s pending-
attempt reuse and `allocIndex` (correctly collision-checked against
`e.sessions`, no leak), `ensureFallback`/`dialFallbackCandidate` (real TCP/
TLS dial runs in its own goroutine, deduped at two levels via
`ns.dialing`/`TLSTransport.dialing`, bounded to a real `net.Dialer{Timeout:
8s}` connect plus a 10s TLS handshake deadline — not capable of hanging
indefinitely). None of it looks wrong on paper.

Built two real, multi-minute reproductions rather than stop at reading the
code:

- `TestDeadSeedRetryDoesNotDegradeOtherPeers`: a node with a permanently
  unreachable seed (closed local UDP port — no response at all, same wire
  signature as a genuinely powered-off machine), directly connected to an
  unrelated healthy peer, continuously verified for 3 real minutes while
  the dead seed is retried at its normal cadence throughout. 36/36 rounds
  delivered. No degradation.
- `TestDeadSeedWithTCPFallbackDoesNotDegrade`: the same shape, but with
  real TCP/TLS fallback enabled on both nodes (production always enables
  this — the one piece of the real path the first test didn't exercise),
  so the dead seed's backoff genuinely triggers `ensureFallback`'s real
  dial against a target with nothing listening on UDP *or* TCP. 30/30
  rounds over 90 real seconds. No degradation.

Neither reproduces the report. That's evidence the mechanisms read as
correct actually behave correctly under this shape of load — not evidence
there's no bug. The gap between "a synthetic loopback reproduction is
clean" and "a real deployment exhibits it repeatedly" is real, and staying
open about that gap is the point of writing this down here instead of
declaring it fixed.

**Separately, re-verified a related but distinct claim with more rigor
than it originally got**: v354 asserted a self-referential seed (a node
listing its own address as a seed) is handled safely, based on a single-
shot test that only proved one handshake attempt gets rejected once. That
test never checked *repeated* cycles or any effect on an unrelated
connection — exactly the gap that just went unaddressed in the
investigation above. New `TestSelfSeedDoesNotDegradeOtherPeers` runs the
same 3-minute reproduction shape for that case specifically: 36/36 rounds
delivered while a node continuously dialed, completed real ephemeral-key/
AEAD crypto against, and got rejected by itself the whole time, with an
unrelated peer connection verified throughout and confirmed to never
register a self-session. Clean. This one now has the evidence its earlier
claim didn't.

**Next step, since local reproduction is exhausted for the open one**:
capture a goroutine dump from the actual affected node next time this
happens, before restarting it — `kill -QUIT <pid>` dumps every goroutine's
stack to stderr on the way down, no new build needed, and slots into the
existing restart-when-it-happens workflow. Also offered, not yet built: a
proper non-destructive `/debug/pprof/goroutine` endpoint on the admin
server, so a snapshot can be pulled without ending the process.

**Verified**: the two new reproductions and the self-seed re-verification
all pass individually (3min, 90s, 3min respectively). One unrelated,
pre-existing test (`TestDynamicAddressing`) failed once during a full
-short suite run under this session's heavy sandbox load, and passed
cleanly 5/5 times in isolation immediately after — consistent with this
sandbox's already-documented CPU-contention-driven flakiness (see v357),
not a regression; confirmed it has no dependency on the peerTimeout/
keepaliveInterval defaults changed in v361. Full -short suite green
otherwise across all 16 packages, both cross-compiles clean.

---

## v361 — 2026-07-11

Follow-up to the route-failover investigation: keepalive cadence and
dead-session timeout were hardcoded package constants (20s and 75s) with
no config or UI exposure at all. Made both live, independently
configurable settings — new default keepalive is 10s (was 20s), new
default peer timeout is a flat 20s (was 75s; briefly considered "3x
keepalive," dropped in favor of a simple fixed default with an explicit
independent override, both exposed).

Mirrored the existing `RouteAdvInterval` pattern exactly, at every layer,
for consistency with the one config value that already worked this way:

- `mesh.Engine`: `keepaliveNs`/`peerTimeoutNs` (`atomic.Int64`, tuned
  live) alongside the existing `routeAdvNs`. New `keepaliveInterval()`/
  `SetKeepaliveInterval(d)` and `peerTimeoutDuration()`/
  `SetPeerTimeout(d)`, same shape as `routeAdvInterval()`/
  `SetRouteAdvInterval(d)`. The `keepaliveInterval`/`peerTimeout` package
  constants are gone — every call site (`maintLoop`'s keepalive trigger,
  `pruneDead`, `onResume`'s clock-skew aging) now reads the live value
  through the engine instance instead.
- `config.Config`: new `keepalive_interval`/`peer_timeout` fields (seconds,
  0 = default), with `KeepaliveDuration()`/`PeerTimeoutDuration()` resolver
  methods mirroring `RouteAdvDuration()`.
- `main.go`: wired into both initial `mesh.Options` construction and the
  same `reloadFn` closure `SetRouteAdvInterval` already goes through, so
  editing either setting applies live, no restart — same as route
  advertisement interval already does.
- `webadmin`: new `/api/keepalive` and `/api/peertimeout` endpoints,
  GET/POST, byte-for-byte the same shape as `/api/routeadv`. New
  "Liveness" settings card (two rows, same debounced-input-on-blur pattern
  as the existing route-advertisement-interval row — there was no numeric
  settings field to model this on until that row existed either, so this
  reuses it directly rather than inventing a second pattern).

**The one piece of actual policy, not just plumbing**: `SetPeerTimeout`
clamps an explicit value below the *current* keepalive interval up to it —
timing a session out before a single keepalive round trip can even
complete would just cause constant reconnection thrashing, not faster
failure detection. This reads the live keepalive value at the moment
`SetPeerTimeout` is called, not a value captured once at construction, so
setting keepalive first and a too-low peer timeout second clamps against
the already-updated keepalive correctly. `config.PeerTimeoutDuration()`
applies the identical clamp at the config-resolution layer too, so a
config.json hand-edited with an inconsistent pair still resolves sanely
before it ever reaches the engine.

**Verified**: new `config.TestKeepaliveDurationDefaultsAndFloors`,
`TestPeerTimeoutDurationDefaultsAndFloors`, and
`TestPeerTimeoutDurationClampsToKeepalive` cover the config-resolution
layer (defaults, floors, and the clamp holding regardless of which side —
keepalive or peer-timeout — is the one left at its default). New
`mesh.TestKeepaliveIntervalDefaultAndSet`, `TestPeerTimeoutDefaultAndSet`,
and `TestPeerTimeoutClampsToLiveKeepalive` cover the same at the live
engine layer. New `TestLoweredPeerTimeoutPrunesFaster` proves the setting
isn't just a getter that returns the right number in isolation: a session
idle for 10s survives `pruneDead` under the default 20s timeout, and gets
reaped once the timeout is lowered to 5s — the live value actually
changes real pruning behavior. `TestRouteFailoverBetweenTwoOrigins` (last
session's reproduction) re-run under the new 20s default: passes in ~25s
real time, down from ~80s under the old 75s default — direct confirmation
the new default is live end-to-end, not just plumbed through and ignored.
Full non-race suite green across all 16 packages, both cross-compiles
clean.

---

## v360 — 2026-07-11

Investigated a bug report, not a fix: two peers redistributing the same
CIDR at different metrics, one turned off, expected the survivor's
advertisement to take over — "it never recovered," then a follow-up that
it was actually worse ("unable to ping any other peer"), then a further
detail (the turned-off peer still showed as online/connected in Monitor →
Mesh peers).

Read every function in the reconciliation path first (`dropNodeRoutes`,
`syncRoute`, `bestRedistMetric`, `sweepStaleRoutes`, `pruneDead`,
`applyBan`, `localDisconnect`) specifically checking `ns.mu` lock
discipline around every `syncRoute` call site, since a stuck lock there
would explain all three symptoms at once (nothing in the same
`maintLoop` tick — including keepalives — would ever run again). Every
call site correctly releases `ns.mu` before calling `syncRoute`; found
nothing wrong by reading.

No existing test covered this specific shape, though — `TestSweepStale-
RoutesWithdrawsAndKeeps` and `TestPrunedNodeRoutesRemoved` both only ever
exercise a *single* origin per prefix, never two origins advertising the
*same* prefix at different metrics. New `TestRouteFailoverBetweenTwoOrigins`
closes that gap: two real engines (A metric 10, B metric 20) redistributing
one CIDR to a third node C, which is also connected to an entirely
unrelated fourth node D; A is stopped; asserts the route fails over to B,
A eventually disappears from C's peer list, and C's connection to D is
undisturbed throughout.

First attempt at the test itself reproduced exactly the reported
symptoms — but it turned out to be a bug in the *test*, not the engine:
it called `pruneDead(ns, time.Now().Add(2*time.Minute))` inside a tight
polling loop (copying a pattern from `TestPrunedNodeRoutesRemoved`, which
only has one other peer, so the same pattern is harmless there). On every
poll iteration this pushed the injected clock further forward, which made
*every* session — B and D included, not just A — look more than
`peerTimeout` stale on every single check, repeatedly pruning and
reconnecting all of them in a fast loop. Rewritten to let the real,
already-running `maintLoop` discover A's silence on its own, in real wall
time, with no manual clock manipulation — and with that fix, **the test
passes**: the route fails over at ~20-25s (`routeTTL`, independent of
session pruning — it doesn't wait on `peerTimeout` at all), A disappears
from `ListPeers` at ~79s (matching the 75s `peerTimeout`), and C's
connection to D is never affected.

So the core mechanism, as tested, is correct. Two things worth taking
from this rather than a code fix:

- **Timing.** Route failover here takes on the order of 20-25 seconds,
  not instant — and "the peer still shows connected" is accurate,
  expected behavior for up to 75 seconds after it goes silent (the
  95%-of-the-time-correct assumption that a network blip shouldn't tear
  down a session is exactly why that timeout isn't shorter). If the
  report was checked within that window, everything described is
  consistent with working-as-designed behavior, not a bug.
- **The still-open question**: "unable to ping any other peer" didn't
  reproduce in this minimal setup, where C's connection to D is direct.
  The most likely remaining explanation, not yet investigated, is
  whether the peer that was turned off was *also* serving as a relay for
  other connections (see the `bestRelay`/relay-scoring work from earlier
  this session) — a relayed session going down is a slower, individually-
  timed-out-and-rebuilt recovery path per affected peer, not the single
  direct reconnection this test exercises, and could plausibly look and
  feel like much more sweeping breakage if several other peers all
  happened to be relayed through the same node.

**Verified**: new test passes consistently in real wall-clock time (no
manual clock injection). Full non-race suite green across all 16
packages, darwin/arm64 and windows/amd64 cross-compile clean.

---

## v359 — 2026-07-11

Follow-up to two questions answered in the same conversation: what happens
on a wire-version mismatch (silent drop, `dispatch`), and whether peer
clock skew matters (yes — ±120s tolerance on handshake timestamps, also a
silent drop, `onHSInit`'s `freshTimestamp` check). Both are now diagnosable
instead of manifesting only as "this peer never connects" with nothing in
the logs pointing at why.

The two cases needed different treatment, not one shared log line, because
they sit at very different trust levels:

- **Version mismatch** (`dispatch`): this check runs on *every* inbound
  UDP datagram before any authentication happens at all — `from` isn't
  verified, so this fires just as readily on port-scanner noise and random
  internet garbage as on a real peer running an incompatible build.
  Logging this at Warn or Info would make it a trivial, unauthenticated
  log-flood vector against any node with an internet-facing UDP port — one
  spoofed packet, no rate limit needed to trigger it, repeated forever for
  free. Logged at **Debug only**, unconditionally: Debug is opt-in, so a
  default-configured node can't be flooded through it, but it's there to
  find when an operator is actively debugging why a *specific known* peer
  won't connect. Matches the existing precedent elsewhere in this codebase
  of logging other pre-auth malformed input at Debug (e.g. transport's own
  read-error path).
- **Clock skew** (`onHSInit`): by the time `freshTimestamp` runs, the
  packet has already passed PSK authentication — whoever sent it knows a
  real key for this network, which anonymous internet noise structurally
  can't. That's what justifies **Warn, unconditionally**, no new rate
  limiting: retries from a genuinely clock-skewed peer are already bounded
  by the existing handshake-retry/seed-backoff cadence (`handshakeRetry` =
  2s, `seedRetryBackoff` = 15s) — this doesn't add a flood path, it adds
  visibility into a bounded one that already existed silently. Reports
  which peer, which network, which direction (their clock ahead of or
  behind ours), the actual magnitude, and the tolerance — "rejected" alone
  wasn't the point; telling an operator which side to go check NTP on was.
  Deliberately *not* routed through the same `ns.throttle.Fail` mechanism
  used for auth-failure bans elsewhere in this function: that would make a
  badly-drifted-but-legitimate peer's address actually get banned for a
  while, a real behavioral change beyond what was asked for (diagnostics),
  not just a log line.

**Verified**: two new tests. `TestVersionMismatchIsLogged` sends a
version-mismatched packet through the real `OnPacket` entry point and
confirms the Debug line fires and names the source address — and that a
merely-too-short packet (a different failure, `ErrShort`) does *not*
trigger this specific message. `TestClockSkewHandshakeIsLogged` constructs
a genuinely PSK-sealed HS_INIT (passes real authentication) with a
timestamp 10 minutes off and confirms the Warn line names the peer, the
skew direction, and the tolerance — and that the handshake is still
rejected (`PeerCount` stays 0): the diagnostic is additive, not a behavior
change. Existing `TestGarbageHandshakesSafe` (throws malformed/garbage
handshakes at the real entry point, asserts nothing panics and no peer
forms) re-run alongside both new tests and `TestSelfHandshakeIsRejected`
under `-race`, ×5: clean. Full non-race suite green across all 16
packages, darwin/arm64 and windows/amd64 cross-compile clean.

---

## v358 — 2026-07-11

Follow-up to a question, not a report: asked whether an IPv6-only peer
would work correctly. Answer, after actually tracing it through transport
binding, wire encoding, reflexive/NAT discovery, overlay DAD, and relay
forwarding, was yes — v4/v6 are independently gated throughout, not a v4
core with v6 bolted on. One real gap turned up while checking, though: the
Mesh → Peers table (and Monitor → Mesh peers, which shares the same
row-building code) only ever displayed `Overlay4`, so a v6-only peer — no
overlay4 assigned at all, a fully supported configuration — showed a blank
overlay column despite being fully connected.

Fixed the display in `peerRowsForNet`: falls back to `Overlay6` when
`Overlay4` is empty, for both the self row and peer rows.

That surfaced a second, non-cosmetic bug along the way: `canEditOv` (the
double-click-to-edit affordance on that column) is gated on `p.overlay`
being non-empty — so before this, a v6-only peer's overlay address wasn't
just blank, it was *uneditable*, the control never appeared at all. Once
the display fallback made `p.overlay` non-empty again, the edit control
came back too — but `peerOverlayEdit`'s save handler unconditionally
submitted the typed value as `address4`, regardless of what it actually
was. For a v6-only peer this meant: editing their address would try to
save a v6 literal into the field the peer's own `NetworkSetAddress`
validates as "must be an IPv4 CIDR" (rejected outright), and clearing via
"none" would silently do nothing to their real (v6) address, since
`address6` was never sent and `NetworkSetAddress` only touches a family
when its field is non-empty — a clear action that reported success but
changed nothing.

Fixed by choosing the target field from the address actually being
submitted: a typed value's own notation is unambiguous (contains `:` =>
`address6`), and "none" (which carries no family of its own) targets
whichever family the *pre-edit* value (`cur`) was — so clearing a v6-only
peer's address now correctly clears their v6 address, not a v4 address
that was never set.

Checked whether the same bug existed in the separate managed-peers cluster
selector (used for switching which node this admin session operates on) —
it didn't; `handleCluster` already had the `Overlay4`-then-`Overlay6`
fallback server-side. The bug was isolated to `peerRowsForNet` and the one
handler downstream of it.

**Verified**: `node --check` on the extracted `<script>` block; full Go
build/vet/test suite green across all 16 packages (no Go-side behavior
changed — this is JS-only); darwin/arm64 and windows/amd64 cross-compile
clean.

---

## v357 — 2026-07-11

Asked directly for the before/after performance difference from v356's
transport round-robin + tunLoop worker-pool changes — not an architectural
argument this time, an actual number. Getting one honestly took a real
detour: the first measurement showed the "improvement" was a regression on
this hardware, which led to a real fix, not just a caveat.

**The environment this was measured in has exactly 1 CPU core** (`nproc` =
1). That's the whole story's hinge: v356's worker pool only pays for itself
when a second core exists for the extra goroutine to actually run on. With
`runtime.NumCPU()-1` clamped to a minimum of 1, this box was always going to
spin up exactly one worker — meaning the channel handoff and `sync.Pool`
coordination v356 added could only ever be pure overhead here, never a win.

**New `BenchmarkOutboundThroughput`** (`internal/mesh`) drives real packets
through the actual TUN-read → firewall/NAT/route → encrypt → UDP-send
pipeline between two engines connected over real loopback sockets — not a
synthetic microbenchmark. Same file, run unmodified against the pre-v356
tree and the current one, on the same hardware:

```
before (v353, single-threaded tunLoop):    ~6,490 ns/op   ~188 MB/s
v356 as shipped (pooled path, 1 worker):  ~11,150 ns/op   ~109 MB/s   (-42%)
```

v356 made the single-core case *worse*, not neutral — the coordination cost
of a worker pool that only ever has one worker is real, and it showed up
immediately once actually measured instead of reasoned about.

**The fix**: `tunLoop` now branches once, at call time, on
`tunWorkerCount()`. `<=1` routes to new `tunLoopSerial` — the exact shape
gravinet used before v356: one buffer, no channel, no pool, no extra
goroutine, `processOutbound` called inline. `>1` routes to
`tunLoopPooled` (v356's design, renamed, otherwise unchanged). Re-measured
with the fast path in place:

```
after (v357, tunLoopSerial fast path):     ~6,540 ns/op   ~187 MB/s
```

Back to within noise of the original. The win v356 was written for — real
parallelism across multiple cores — is architecturally still there for
`TunWorkers > 1` (unchanged from v356: firewall/NAT/route/encrypt/send
handed to a worker pool instead of running inline on the reader), but
*could not be measured in this environment* for the same reason the
regression showed up in the first place: there's no second core here for it
to show a benefit on. That number remains an honest gap — stated plainly
rather than assumed from the architecture a second time.

**Two real bugs found and fixed while getting these numbers, both in test
code, not production code:**

- New `TestTunLoopPooledDeliversAllPackets` (forces `TunWorkers: 4` so the
  pooled path gets exercised at all on a 1-core CI runner, where every
  other test's default `TunWorkers` computation yields 1) first appeared to
  show `tunLoopPooled` losing over 90% of packets — 17/200 arriving.
  Instrumenting both ends showed the real transport-level rx count on the
  receiving side was 204 (~200 data packets + keepalives): delivery was
  fine. The loss was entirely in the test's own `fakeDev.out` debug tap — a
  16-slot channel that drops non-blockingly by design so a test with nobody
  reading can't deadlock — losing its scheduling race against two full
  engines' worth of goroutines contending for one core. Fixed by asserting
  on the transport's real, monotonic rx counter instead of that lossy debug
  channel, which is the only signal here that can't produce a false
  negative from scheduling pressure.
- New `crypto.TestConcurrentSealNoCollision` (32 goroutines × 200 calls,
  same session, asserting every `Seal` counter is claimed exactly once and
  decrypts correctly) was itself intermittently flaky — reproducible
  specifically when run alongside the mesh package's tests, i.e. under
  exactly the kind of CPU pressure this whole session has been measuring.
  Root cause: it opened each result in the order it arrived from a channel
  fed by 32 concurrent goroutines — essentially the arbitrary interleaving
  of when each one happened to finish — rather than in ascending counter
  order. The 64-wide replay window correctly rejected a counter once
  something more than 64 higher had already been opened
  (`crypto: replayed or stale packet`), which is the *window working as
  designed*, not a flaw in `Seal`'s concurrency safety. Fixed by collecting
  all results first and opening them in ascending counter order — matching
  how a real receiver actually consumes a stream, and how the window is
  meant to be exercised.

**Also resolved**: v356's changelog reported one unreproduced `-race`
failure on `internal/mesh` early in that session, with the diagnostic detail
lost to a `tail -100` pipe. In hindsight this is very likely the same class
of issue as the two above — test-harness timing/ordering assumptions
breaking under heavy scheduling pressure on this 1-core box, not a
production race — though that specific instance was never directly
confirmed, and is noted here rather than silently assumed closed.

**Verified**: full non-race Go build/vet/test suite green across all 16
packages. `-race` run across `internal/mesh` + `internal/crypto` +
`internal/transport` together (the combination that reproduced both test
bugs above): clean. `TestTunLoopPooledDeliversAllPackets` and
`TestConcurrentSealNoCollision` specifically re-run repeatedly (5-15×) both
standalone and combined post-fix: all clean. darwin/arm64 and windows/amd64
cross-compile clean.

---

## v356 — 2026-07-11

Follow-up to the relay-scoring discussion: asked how gravinet's performance
compares to WireGuard/IPsec, then asked what could be improved. Answer to
the first was architectural, not a benchmark run here — reading the actual
transport code confirmed no `recvmmsg`/`sendmmsg` batching and a per-packet
`ReadFromUDPAddrPort`/`WriteToUDPAddrPort` syscall, putting gravinet in
wireguard-go's performance class rather than kernel WireGuard's, with AES-
256-GCM measured at ~2.95 GB/s single-core on this hardware (`internal/
crypto`'s `BenchmarkSealInPlace`) confirming crypto was never the ceiling.
That reading turned up two concrete, asymmetric bottlenecks, and this
session fixed both.

**1. `internal/transport.Send` always used `conns4[0]`/`conns6[0]`.**
Inbound already got real per-core parallelism — one REUSEPORT socket per
worker (`startWorkers`) — but every outbound write, from every goroutine,
funneled through a single socket regardless of how many were bound. Fixed
with a plain atomic round-robin counter (`txRR4`/`txRR6`) over the same
socket set reads already use. Every socket in a REUSEPORT group is bound to
the identical address:port, so which one performs the write changes
nothing on the wire — this only spreads outbound syscalls across sockets
instead of piling every write onto one. `len==1` (reuseport disabled, or a
single worker) makes it a no-op, so non-Linux platforms and single-worker
setups are unaffected.

**2. `tunLoop` was fully single-threaded — the bigger one.** One goroutine
per network did `dev.Read()` then, inline, on the same goroutine: firewall,
NAT, classify, route lookup, encrypt, send — before looping back to read
the next packet. No pipelining, no parallelism. Everything this node
*originates* (as opposed to relays or receives) was capped to whatever one
core could push through that whole sequence, no matter the core count —
while inbound already had a real worker pool.

**The fix**: `tunLoop` now does only the read on its single goroutine (a
single-queue fd — concurrent reads on it aren't assumed safe, and
`IFF_MULTI_QUEUE` isn't wired up) and hands each packet to a channel-backed
pool of `TunWorkers` goroutines for everything after — extracted verbatim
into a new `processOutbound`. New `Options.TunWorkers` (mesh) defaults to
`runtime.NumCPU()-1`/min 1, same convention as `config.WorkerThreads`, and
`main.go` now passes it the *same* computed `workers` value already given
to the UDP transport, so both pools scale together under one existing knob
rather than picking their own counts independently.

This is the riskier of the two changes — it's new concurrent access to
state (`ns.fw`, `ns.nat`, routing tables, `peerSession.sess`) that used to
see only one goroutine on the outbound side — so it got real scrutiny
before shipping, not just "it compiles":

- Audited every function `processOutbound` calls: `firewall.allow` (atomic
  `enabled`/`snap`), `natTable.translateOut` (`t.mu.Lock()`),
  `netState.routeTo`/`redistRoute`/`flood` (`ns.mu.RLock()`),
  `tokenBucket.allow` (`b.mu.Lock()`), `shaper.enqueue` (`s.mu.Lock()`) —
  all already built for concurrent callers, because the *inbound* path
  (`deliverInner`, already running on `Workers` goroutines via
  REUSEPORT) already hits the same state concurrently today. This wasn't
  a new category of risk, just a new caller of an already-concurrent-safe
  surface.
- The one property that specifically needed proving rather than assuming:
  two workers can now call `Seal` on the *same* peer session at once
  (two packets to one destination, picked up by different workers).
  `Cipher.Seal`'s counter is `atomic.AddUint64` and Go's AEAD
  implementations carry no shared mutable state, so this is safe — but
  "should be fine" isn't the same as verified. New
  `crypto.TestConcurrentSealNoCollision` (32 goroutines × 200 calls each,
  same session) asserts every counter is claimed exactly once and forms a
  dense run, and every ciphertext decrypts back to its own plaintext.
  New `mesh.TestProcessOutboundConcurrentSameDest` drives the actual
  mesh-layer function the same way (32×100, same destination peer) and
  confirms no send is lost. Both run in ~0.02-0.03s and were run 20× each
  under `-race`: 40/40 clean.
- Explicitly not a regression, but worth naming since it's a real
  behavior change: packets to the same destination are no longer
  guaranteed to be *sent* in the order they were *read*, since two workers
  can finish out of order. Not a new failure mode for anything running
  over this tunnel — UDP never promised ordering, the 64-packet replay
  window comfortably absorbs goroutine-scheduling-induced reordering, and
  TCP-over-gravinet already carries its own sequence numbers for exactly
  this reason.
- Buffer lifetime: the reader pools TUN-sized buffers (`sync.Pool`,
  resized on `cap` mismatch rather than trusting a fixed size, so an
  MTU change after `recoverDataplane` rebuilds the interface is handled
  without extra bookkeeping); ownership of each buffer transfers to
  whichever worker receives the job and is never touched by the reader
  again until that worker returns it to the pool. Shutdown ordering:
  `tunLoop`'s own `ns.wg.Done()` defer is registered *before* the
  `close(jobs); workers.Wait()` defer, so — deferred calls unwinding
  LIFO — every worker has fully exited before `ns.wg.Done()` fires, same
  external contract teardown already relied on.

**Also run, honestly reported**: the full `internal/mesh` suite under
`-race` (not just the two new targeted tests) was run 16 times across this
session. 15 were clean. One failed, and the run that produced it was piped
through `tail -100` without being saved to disk, so the actual `--- FAIL`
line and any race stack trace were cut off by the time that was noticed —
it couldn't be recovered, and 13 subsequent full/targeted re-runs (plus the
40 fast targeted runs above) did not reproduce it. Manual audit of every
newly-concurrent call found no issue. This is being stated plainly rather
than rounded up to "all green": the balance of evidence (one
unreproduced, undiagnosed failure against 15 clean full runs and 40/40 on
tests built specifically to stress the new concurrency) supports shipping,
but it's not the same as a clean bill of health with the actual failure
explained. Worth another look if anything resembling a data race or
dropped/duplicated packet shows up in practice.

Full non-race Go build/vet/test suite green across all 16 packages,
darwin/arm64 and windows/amd64 cross-compile clean.

---

## v355 — 2026-07-11

Follow-up to a question, not a bug report: asked how relay selection is
chosen when multiple peers are being relayed, and whether there's any
optimization behind it. There wasn't — `tryRelays` picked the first
connected peer that had ever gossiped knowledge of the target, out of a Go
map with randomized iteration order, with no regard for latency or chain
depth. Asked to add real scoring, specifically RTT-based preference and a
preference for a direct (non-relayed) candidate to keep chains short — with
the assumption that Monitor → Latency's RTT numbers were already
available to draw on.

**They weren't** — worth being precise about since it shapes the whole
design. Monitor → Latency (`handleLocalLatency`, `internal/webadmin/
sysinfo.go`) is a synchronous, on-demand HTTP handler: it fires only when a
human loads that tab, shells out to the OS's native `ping` against every
peer's overlay address, and returns the result directly in that one HTTP
response. Nothing stores it, nothing refreshes it in the background, and
`internal/mesh` — where `tryRelays` lives — has no access to `internal/
webadmin` at all, nor any business shelling out to OS binaries from the
packet-processing path. That data was never reachable from relay selection
and couldn't have been reused as-is.

What was already there, and is the right foundation: `ctrlPing`/`ctrlPong`,
the NAT keepalive round trip `sendKeepalive` fires at every connected peer
every `keepaliveInterval` (20s). The pong handler did nothing with the
timing before this — liveness was already recorded elsewhere by `touch()`,
so `case ctrlPong:` was a no-op comment. That's a real, continuous,
already-encrypted-and-authenticated round trip to every connected peer,
including one reached via a relay (ctrlPing/ctrlPong travel inside the
per-peer session like any other control message, so RTT to an
already-relayed peer is the true you-through-the-relay-to-them figure, not
just to the relay itself) — it only needed its timestamps kept.

**The change**:

- `peerSession` gained `pingSentNanos`/`rttNanos` (both `atomic.Int64`).
  `sendKeepalive` stamps the former right before sending `ctrlPing`;
  `onControl`'s `ctrlPong` case now computes the delta and stores it,
  skipping a pong with no matching ping (sent==0) or a non-positive delta
  rather than recording garbage.
- `PeerInfo` gained `RTTMs` (json `rtt_ms`, omitempty), populated in
  `ListPeers` — surfaces this passively-collected figure through the same
  API the admin UI already reads, distinct from Info → Latency's on-demand
  number.
- `relay.go`: `tryRelays`' inline first-match loop is now `bestRelay` +
  `relayBetter`. Selection is two-tier: a candidate reached directly
  (`ps.getRelay() == nil`) always beats one that's itself already relayed,
  regardless of RTT — stacking a second hop is worse than a slightly slower
  single one. Within the same tier, lowest measured RTT wins; a candidate
  with no sample yet (freshly connected, hasn't completed a keepalive round
  trip) never wins against a measured one, so an untested peer doesn't get
  picked over a known-good one just because it happened to gossip first.

**What this does and doesn't optimize**, stated plainly in relay.go's
package doc now too: the RTT is this node's own round trip *to the
candidate*, not the candidate's path *to the target* — a relay only ever
forwards opaque ciphertext, so it has no way to attribute or report a round
trip to someone else's traffic, and nothing today has a candidate gossip
its own measured RTTs onward. This picks the fastest, shortest-chain relay
reachable from here; it doesn't yet know which candidate has the fastest
path onward to the target specifically. Noted as a natural v2 (candidates
gossiping their own RTT table alongside the existing peer-list entries) if
that distinction ends up mattering in practice.

Also unchanged, and worth flagging since it surprised in the original
question: relay selection still only runs when a target is first
unreachable — once a relayed session is up, `tryRelays` skips it
(`connectedToNode` is true) and never re-evaluates for a better relay
while the current one keeps working, same as before this change.

**Verified**: seven new tests in `internal/mesh/relayscore_test.go` —
`relayBetter`/`bestRelay` table-style coverage (direct-beats-relayed
regardless of RTT, lowest-RTT-within-tier, unmeasured-never-wins including
the two-unmeasured tie case, ignoring the target itself and non-reporters,
nil-candidate-list) plus `TestKeepaliveRTTCapture`, which drives the actual
`sendKeepalive` → `onControl(ctrlPong)` path with a manufactured delay and
confirms a stray pong with no matching ping doesn't record a bogus sample.
Existing `TestRelay` (single candidate) and `TestMeshFormation` still pass
unchanged under the new scoring. Full Go build/vet/test suite green across
all 16 packages, darwin/arm64 and windows/amd64 cross-compile clean.

---

## v354 — 2026-07-11

Reported with a screenshot of Mesh → Peers on gn-cush2, viewing its own
"cush1" network card: gn-cush2 listed twice — once as v306's inert "this
node" row, once as an ordinary "enabled" peer with a real endpoint
(`192.168.55.33:443`). That second row wasn't a rendering glitch; the node
genuinely had a live, authenticated session with itself sitting in
`ns.byNode`.

**Root cause**: gn-cush2's status banner reads "behind NAT — symmetric
mapping (a relay may be needed)." A symmetric-NAT node dialing a seed
address it learned from gossip/a relay can hairpin — the outbound packet
loops back to its own listener instead of reaching the peer it thought it
was dialing. Nothing in the handshake path checked whether a completed
session's claimed node id matched the local node's own: the crypto genuinely
authenticates (it's really this node's own PSK and ephemeral keys), so
`onHSInit`/`onHSResp` sealed and installed it exactly like a legitimate
peer, and `install()` registered it in `byNode` (plus `routes4`/`routes6` —
worse than the UI symptom, since that's a route pointing our own overlay
traffic back into our own tunnel). The frontend's `peerRowsForNet` then
had no reason to suspect `n.peers` would ever contain an entry matching
`n.self`, so it rendered both.

**The fix**, three layers deep to match how every other self-interaction
in this codebase is guarded (`control.go`'s gossip learner, `relay.go`'s
relay/candidate paths, `ban.go`'s disconnect guard, etc. — each site checks
`== e.nodeID` rather than relying on one central chokepoint):

- `onHSInit` (responder) and `onHSResp` (initiator) now both drop a
  handshake whose claimed `NodeID` equals our own before doing anything
  further with it — the actual root-cause fix, stopping the loopback
  session from ever completing.
- `install()` itself got a backstop guard refusing to register a session
  for our own node id, in case any future caller reaches it some other way
  — this is the one place `byNode`/`routes4`/`routes6` entries actually get
  created, so it doesn't rely on every caller remembering the check.
- `peerRowsForNet` (`internal/webadmin/ui.go`) now also skips any `n.peers`
  entry matching `n.self`'s id before pushing it onto the rows — belt and
  suspenders for a node already running when this ships, which may be
  holding a stale self-session in memory until it restarts onto the fix.

**Verified**: new `TestSelfHandshakeIsRejected` (`internal/mesh`) points a
node's seed list at its own listening address — the wire-level shape of a
hairpin — over the same real-transport harness `TestMeshFormation` uses,
and asserts `PeerCount`/`ListPeers` stay empty. Confirmed the test actually
catches the bug: with the three guards temporarily stripped, it fails with
`PeerCount(netID) = 1` and the log shows `"inbound tunnel up with
\"hairpin-node\"... outbound tunnel up with \"hairpin-node\""` — the node
connecting to itself, logged like any other peer. Restored the fix and it
passes. Full Go build/vet/test suite green across all 16 packages
(`internal/mesh` 108s, `internal/webadmin` 27s), darwin/arm64 and
windows/amd64 cross-compile clean, `node --check` on the extracted
`<script>` block.

---

## v353 — 2026-07-11

v352 regressed the exact feature it was trying to fix: "now switching
between peers is broken," reported directly and immediately after
installing it. Owned it rather than re-explaining the theory — traced the
actual mechanism before changing anything again.

v352 had `load()`, `startPolling()`, and `refreshCluster()` each do
`const seq = ++state.targetSeq;` — every one of them bumping the *same*
shared counter independently. But `sel.onchange` fires `refresh()` (which
calls `load()`) and `refreshCluster()` **together**, neither awaiting the
other, both for the *same* switch. Sequence, concretely: `load()` bumps
`targetSeq` to, say, 6 and captures `seq=6`, then awaits its fetch;
`refreshCluster()` runs next (still synchronous up to its own first
await), bumps `targetSeq` to 7, captures `seq=7`. The moment `load()`'s
fetch resolves, its own check (`seq(6) !== targetSeq(7)`) fails —
permanently, since the counter only increases — so its result gets
discarded as "stale" **every single time**, on every switch, not just the
genuinely racy ones. `state.status`/`state.nat`/`state.cfg` stopped being
updated by `load()` at all; only `startPolling`'s next 4-second tick could
still occasionally get a result in, and it was racing the exact same
self-inflicted problem.

Reproduced this in isolation first, the same way v351/v352's original
fixes were verified, rather than trusting the diagnosis on reasoning
alone: modeled both the broken and fixed counter schemes with fake timed
fetches standing in for `/api/status` and `/api/cluster`, fired together
the way `sel.onchange` actually does. The broken model reproduces exactly
what was reported — `load()`'s result never lands, `refreshCluster()`'s
does. The fixed model gets both.

**The actual fix**: only one thing should ever bump `targetSeq` — the
selection itself changing, not every function that happens to read it.
New `setTarget(v)` is now the *only* place `state.target` is assigned
(replacing all four prior direct assignments — `sel.onchange`, `load()`'s
401 fallback, and `refreshCluster()`'s two reset branches), bumping
`targetSeq` alongside it exactly once per real switch.
`load()`/`startPolling()`/`refreshCluster()` now each capture
`state.targetSeq` **read-only** — no `++` — before firing their request,
and compare again after. Multiple fetches launched together for the same
switch now share one generation number and don't invalidate each other;
only a genuine further switch while they're in flight — which does call
`setTarget`, correctly bumping the generation — makes an in-flight fetch
stale. Re-ran the isolated reproduction a third way to confirm this: a
real switch mid-flight still correctly discards the older of the two
`load()` calls, so v351's original protection isn't lost, just no longer
applied at the wrong granularity.

Full Go build/vet/test suite; `node --check` on the extracted `<script>`
block for syntax; the three-scenario isolated reproduction (broken,
fixed-normal-switch, fixed-genuine-race) run and inspected directly rather
than assumed correct from the diff.

---

## v352 — 2026-07-11

Follow-up to v351: reported directly that after installing it, the NAT
banner still didn't change when switching peers in the management
dropdown. Didn't take v351's fix on faith and guess again — went and
proved what was and wasn't true first.

**Proved the backend proxy itself is correct**, rather than continuing to
reason about it from reading alone: new `TestProxyRoutesToCorrectPeer`
(`internal/webadmin/proxy_roundtrip_test.go`) is a real two-server round
trip — an actual second `httptest.NewTLSServer` standing in for a managed
peer (`handleProxy` always dials `https://`, so this had to be a TLS test
server, not a plain one, for the dial to succeed at all), an actual
managed-peer registration pointing at its real address, and an actual
proxied `GET /api/proxy?node=...&path=/api/status` through
`handleProxy`'s real HTTP client. Confirmed the response is genuinely the
target's own `nat_class`, `public`, and `self.hostname` — not the caller's,
not a mix of the two. `stubBackend.NATStatusStrings()` was hardcoded to one
fixed value for every test server before this (harmless until a test
needed two servers with two different identities, which none had before);
made it configurable so this test — and any future one — can actually
tell two simulated nodes apart.

**Found the actual remaining gap**: v351 added a staleness guard
(`state.statusSeq`, bumped per fetch, checked before committing) to
`load()` and `startPolling()`, but `refreshCluster()` — which fires
concurrently with `load()` on every dropdown switch (`sel.onchange` calls
`refresh(); refreshCluster();`, neither awaited before the other starts) —
had no such guard at all, despite being able to silently reset
`state.target` back to local:

```js
if (state.target && (!state.manager || !state.cluster.some(p => p.node_id === state.target && p.manageable))) {
  state.target = null;
  ...
}
```

A slow `/api/cluster` response — fetched for the *previous* target,
resolving after the user has already moved on to a new one — could
undo the switch moments after it appeared to take, independent of
whatever `load()` itself had correctly fetched. Structurally the same
class of bug v351 fixed for the *data*, just one layer further up, for
the *target selection itself* — v351 made sure `state.status`/`state.nat`
couldn't be clobbered by a late response, but never checked whether
`state.target` was still what the fetch was actually for by the time it
resolved.

Fixed by extending the same guard to `refreshCluster()`, sharing v351's
counter — renamed `state.statusSeq` → `state.targetSeq` since it now
protects every target-dependent refresh, not just the status one: bumped
once per `load()` call, `startPolling()` tick, *and* `refreshCluster()`
call, checked again before any of the three commits anything. Whichever
was issued last is the only one allowed to write, regardless of arrival
order — the exact invariant the name and v351's original design already
intended, just not yet applied everywhere it needed to be.

Full Go build/vet/test suite, including the new round-trip test;
`node --check` on the extracted `<script>` block for syntax. Genuinely
uncertain whether this fully closes what was reported — asked for
specific follow-up detail (does the dropdown itself visually revert? does
the peers table's data change but not just the banner? any console
errors?) rather than declaring it fixed outright a second time on
reasoning alone.

---

## v351 — 2026-07-11

Asked directly, with a screenshot: shouldn't "This node: behind NAT —
symmetric mapping" change when a different peer is selected in the
management dropdown? The same screenshot also showed something not asked
about but worth noticing: `gn-cush2` listed twice in the peers table — once
correctly as **this node** (self, when the dropdown targets it directly)
and once as a completely ordinary **enabled** peer at a different address.

Traced both to the same cause rather than treating them as two separate
reports. `handleStatus` (webadmin.go) builds `self` from `SelfPeer` and
`peers` from `ListPeers` — on any single node, structurally, those can never
overlap (a node doesn't create a peer session for itself), confirmed by
reading `Engine.ListPeers`, which only ever walks `ns.byNode`, the map of
*other* nodes' sessions. And `renderSection()` does a full `c.innerHTML = ''`
before every rebuild, so nothing can linger in the DOM from a previous
render either. Both of those ruled out — a single node's response can't
produce this, and stale DOM can't either — which leaves only one place left
to look: `state.status` itself getting overwritten by two different
responses that were never meant to land in that order.

Found it: switching the management dropdown triggers `load()`'s own
`/api/status` call, now proxied to the new target — the code's own comment
already acknowledges this can take "a couple of seconds" on a slow or
high-latency peer, exactly the kind of peer a symmetric-NAT relay path
produces. `startPolling`'s independent 4-second timer fires on its own
schedule regardless, and neither it nor `load()` had any way to tell a
late-arriving response was for a target the user had already switched away
from — both just unconditionally overwrote `state.status`/`state.nat` with
whatever came back, whenever it happened to arrive. A poll tick sent to the
old (local) target just before a switch, slow to resolve, landing *after*
the new target's own faster response, silently puts the wrong node's data
back — the self row from the correct proxied response, sitting next to a
peer row left over from the stale one, and a NAT banner that looks stuck
because it keeps getting reset moments after it briefly updates.

Reproduced the exact race in isolation before touching the real code —
a fake `/api/status` standing in for a slow local poll (500ms) racing a
faster proxied fetch for a just-selected target (50ms, started 100ms
later) — and confirmed the old code's `state.nat` ends up showing the
*previous* target's data even though the new one resolved and was applied
first, then got clobbered.

Fixed with a shared monotonic counter, `state.statusSeq`: bumped once per
`load()` call and per `startPolling` tick, checked again after every
`await` before committing anything to `state`. Whichever request was
issued *last* is the only one ever allowed to write — regardless of which
one's response happens to arrive first — so a superseded response is
silently discarded instead of winning a race it shouldn't have been in.
Re-ran the same isolated reproduction against the fixed logic: the stale
response is now discarded and the freshly-selected target's data is what
sticks.

No Go changes — `internal/webadmin/ui.go` only. `node --check` on the
extracted `<script>` block for syntax, plus the standalone race
reproduction above (both before and after, showing the actual failure and
the actual fix rather than just reasoning about it). Full Go build/vet/test
suite run anyway, since it costs nothing to confirm nothing else moved.

---

## v350 — 2026-07-11

Requested directly: the Mesh → Seeds and Mesh → Peers description
paragraphs had gotten wordy — shorten them.

They had, honestly — five sessions in a row (v341 through v349) each added
a sentence or two to one or both without anyone stepping back to look at
the whole paragraph: multi-port syntax, the udp/tcp distinction, seed-note
inheritance, and so on, each individually justified at the time, additively
turning two originally-tight hints into ~150 and ~180-word paragraphs.

Trimmed both by more than half — 85 and 71 words — by cutting the *why*
behind each gesture (already covered in getting-started.md and this
changelog for anyone who wants it) and keeping only the *what* and *how*: what
the page shows, what each double-click/tick/button does, and the couple of
facts that actually change behavior (udp vs tcp transport, add-is-live vs
remove-is-next-restart, seed notes auto-filling a peer's own note on first
connect). Nothing described was removed from the product, only from these
two paragraphs specifically — the longer explanations stayed put in
getting-started.md's own Seeds and Peers sections and the relevant
changelog entries.

No Go changes — `internal/webadmin/ui.go` only. `node --check` on the
extracted `<script>` block for syntax; full Go build/vet/test suite run
anyway since it costs nothing to confirm nothing else moved.

---

## v349 — 2026-07-11

Follow-up to v348, with another screenshot — this time Mesh → Peers itself,
not Seeds. Same diagnosis taken one step further, and the operator's own
proposed fix: "these things try different ports, their notes disappear —
maybe we should tie notes to a guid and not a name:port."

Checked the new screenshot's actual data before agreeing with anything.
Two failures, not one:

- `gn-cush1` now connects over a LAN-discovered direct path
  (`192.168.55.3`) instead of its public seed (`98.191.177.78`) — a
  completely different *host*, not just a different port. No amount of
  address-matching cleverness fixes this one; there's nothing shared
  between those two addresses except that they're the same physical box.
- `gn-cush2` is sitting on `98.191.177.78:443` — the exact port configured
  on `gn-cush1`'s seed. v348's fix trusts an exact `host:port` match
  outright (reasoned, at the time, that pinning down one specific seed row
  made it unambiguous by construction) — but that's not actually true once
  two *different* physical boxes can independently land on the same public
  NAT'd port at different times. Confirmed by replaying both endpoints
  from the screenshot through the current matching code before writing
  anything: `gn-cush1`'s LAN path → correctly blank; `gn-cush2`'s address
  → confidently returns `gn-cush1`'s note.

Agreed with the diagnosis: address-based matching, exact or fuzzy, can
never be fully reliable here, because an address isn't a stable identity
for these boxes and a node id already is — gravinet already has a notes
mechanism keyed by exactly that (`Config.PeerNotes`, the same field the
Peers/Bans notes columns already read and write). Asked one clarifying
question before building — automatic inheritance vs. a manual one-click
adopt action — since that's a real fork with different implications
(silent write vs. explicit operator action) that a mid-size backend change
wasn't worth guessing on. Answered: automatic.

**Built as one-time inheritance, not a live/ongoing address-matching
system.** New file `cmd/gravinet/seednotes.go`:

- `seedNotesForEndpoint` — a Go port of the web UI's `seedNotesForAddr`
  (v348's ambiguity-safe host/port matching), used here server-side since
  this needs to *persist*, not just render a tooltip. Verified against
  Go's actual `net.SplitHostPort` behavior first rather than assuming it
  matches the hand-rolled JS version's semantics (it does — including for
  a v346 multi-port seed's comma-joined "port" field, which
  `SplitHostPort` splits on position without validating content, same as
  the JS version's own reasoning already established).
- `applySeedNoteInheritance` — the actual decision logic (which peer gets
  which note, and whether anything changed), deliberately split out from
  the engine/disk plumbing around it so it could be tested directly
  against constructed `mesh.PeerInfo` values — `PeerInfo`'s fields are all
  exported, so this needed no live engine, no real handshake, nothing
  engine-internal at all.
- `inheritSeedNotes` — the glue: loads config under `config.WithLock` (the
  same locking `mutateConfig` uses, since this now writes to the same file
  webadmin edits can touch), pulls each network's live peers via the
  existing `Engine.ListPeers`, applies the above, and — only on an actual
  change — validates, saves, and calls the same `reloadFn` every other
  config mutation in `main.go` already uses, so the live engine's
  in-memory `PeerNotes` (what actually feeds the peers table and every
  hover-tooltip) picks it up immediately rather than waiting on some
  unrelated future reload.

Never overwrites an existing peer note — an operator's own edit always
wins — and never touches the seed's own note either, so it stays as the
description for whoever hasn't connected yet. Runs on a new 20-second
ticker in `run()`, sharing the key-expiry sweep's existing `tickStop`
channel rather than inventing a new shutdown path for it (closing
`tickStop` in `shutdown()` already stops every ticker registered against
it, not just the one it was originally added for).

Once a note is inherited, the *existing* hover-tooltip logic (v345,
patched v348) benefits automatically with no changes needed on that side:
it already checks the peer's own note as a fallback after the live
seed-address match, so a peer whose address later drifts away from every
seed entirely — `gn-cush1`'s exact situation — still shows its note from
here on, because by then it has one of its own.

New tests: `TestSeedNotesForEndpoint` (the same matching scenarios v348's
JS was checked against, replayed in Go, plus the two exact failures from
this session's screenshot) and `TestApplySeedNoteInheritance` (the
three-peer scenario from the Peers-table screenshot directly — LAN-local
no-match, exact-port cross-match, no-seed-at-all — plus never-overwrites
and empty-node-id-is-skipped-not-keyed-on-"" cases). Full Go test suite,
`go vet`, and build clean.

Docs and in-app hint text updated to describe the new behavior:
`getting-started.md`'s Peers section, and the Mesh → Peers hint text
itself.

---

## v348 — 2026-07-11

Reported directly, with a screenshot: the seed/peer notes tooltip on a
peer's name (v345) "doesn't always work — works for some peers, but not
others." The screenshot was Mesh → Seeds for one network, showing exactly
the shape of the bug: two seeds, same host (`98.191.177.78`), different
ports (`443`, `65432`), different notes (`cox gn-cush1 nat phoenix` /
`cox gn-cush2 nat phoenix`) — two boxes behind one residential NAT
connection, disambiguated only by which port their router forwards to
which box.

Reproduced `seedNotesForAddr`'s exact logic against those two seeds before
touching anything (Node, not a browser — this is client-side JS with no
test harness of its own, so verified the same way the multi-port seed work
two versions ago was sanity-checked). Both peers resolve correctly when
each is reached exactly on its own seeded port. But feed it a *third*
address — same host, a port that matches neither seed exactly, which is
exactly what a NAT rebind after an idle timeout produces on a residential
connection — and the old fallback returned `gn-cush1`'s note for what
should've been `gn-cush2`. Not blank: confidently wrong, silently, which
reads as "worked for some peers" (whichever one's live port still happens
to match its own seed) and "didn't for others" (whichever one's NAT
mapping has since drifted) — exactly the report.

The root cause: the host-only fallback (added in v345, for when a seed is
entered without a port, or a peer answers on an extra port) picked
whichever same-host seed came first in the list the moment no port
matched exactly, with no way to tell two *different* boxes sharing one
host apart. Two seeds, two different notes, same host, is a case a seed
list can express precisely — the ambiguity was purely in how the fallback
collapsed it back down to one guess.

Fixed by making the fallback ambiguity-aware instead of first-match: it
now collects the *set* of distinct notes among every same-host seed, and
only uses a host-only match when that set has exactly one member — i.e.,
every seed sharing that host agrees on the same note (the same box
entered more than once, with and without a port, say). Two seeds on one
host with two different notes now correctly produces no tooltip rather
than a wrong one. An exact `host:port` match is untouched and still wins
immediately regardless — it pins down one specific seed row by
construction, so it was never actually ambiguous.

Manually verified against the screenshot's own two seeds plus three probe
addresses: each box on its own exact port still resolves correctly; the
same host on a third, unseeded port (the roam case) now returns blank
instead of the wrong box's note; and a control case — one box entered as
two seed rows (bare host + host:port) sharing one note — confirmed the fix
doesn't regress the *legitimately* unambiguous host-only case by treating
"same host" alone as automatically suspect.

No Go changes — `internal/webadmin/ui.go` only, same as v345's original
feature. `node --check` on the extracted `<script>` block for syntax; full
Go build/vet/test suite run anyway since it costs nothing to confirm
nothing else moved.

---

## v347 — 2026-07-11

Reported directly: the Settings → Underlay UDP port field wouldn't accept
`65432,23,70,79,513,21,7,11,13,15,17,19,443` — typed it in, hit tab/enter,
it reverted to just `65432`.

Didn't assume the multi-port field itself was broken — checked first.
Reproduced the exact input two ways: simulated `buildPortListRow`'s
client-side parse/validate logic directly (accepted, 13 ports, nothing
rejected) and sent it through the real `/api/port` HTTP handler in a test
harness (`ok:true`, `PrimaryPort: 65432`, `ExtraListenPorts:
[23 70 79 513 21 7 11 13 15 17 19 443]`, persisted to disk correctly).
Diffed `buildPortListRow` itself against the original v344 upload too, byte
for byte — untouched by anything this session did. So the code, as written,
handles this input correctly on both sides of the wire.

That pointed at a different question: what would make correct server-side
code produce this exact symptom in a real browser? `65432` is specifically
the *leading numeric prefix* of the typed string — precisely what
JavaScript's `parseInt` returns on a comma-containing string when called
without first splitting on commas. That's exactly what the *old*, pre-v341
single-number port input did before `buildPortListRow` existed. Which
raised the real question: could a browser be running that old JS against a
server that's since been upgraded?

Checked, and yes — found a real, independent gap. `authed()` (the wrapper
around every `/api/*` call) already sets `Cache-Control: no-store`, with a
comment explicitly reasoning about this exact staleness risk: *"a browser
could in principle keep showing [something] from before [a change on the
server]."* `/` is registered directly (`mux.HandleFunc("/", s.handleIndex)`)
— it never passes through `authed()`, and `handleIndex` set no
`Cache-Control` header of its own. So the one response in the entire server
that actually delivers the client-side JavaScript — including
`buildPortListRow` — was the one response with no protection against
exactly the staleness class `authed()` was already written to guard
against. A cached copy of `/` from before a version's JS changes keeps
running indefinitely: every `/api/*` call it makes still reaches the
current, upgraded backend and "succeeds" by the old JS's own logic, so
nothing about the requests themselves looks wrong — the mismatch is
invisible from the server's side entirely, which is exactly consistent
with the backend testing correct while a real browser doesn't.

Fixed by giving `handleIndex` the same `Cache-Control: no-store` `authed()`
already uses, for the same reason.

**For anyone hitting this now**: a hard refresh (Ctrl/Cmd+Shift+R) or a
private/incognito window should confirm it immediately, and will keep
working correctly even without today's fix, since either forces a real
refetch of `/`. Today's fix is about not needing to know to do that after
every future upgrade.

New test: `TestIndexNoStore` (`internal/webadmin/index_test.go` — no test
file for `handleIndex` existed before this at all) confirms `/` responds
with `Cache-Control: no-store`. Left `handleXtermJS`/`handleXtermCSS`
alone — their long `max-age` is a deliberate, already-documented choice for
those two vendored, content-addressed-in-spirit assets specifically, a
different tradeoff than the app shell and not what was reported here.

Full Go test suite, `go vet`, and build clean (Go 1.22 + libpam0g-dev, this
sandbox).

---

## v346 — 2026-07-11

Requested directly, following up on a specific attempted syntax: can a seed
be told to try multiple ports against one host, the way local listen ports
already can — something like
`21.221.254.143:65432,23,70,79,513,21,7,11,13,15,17,19,443`? Checked before
answering rather than assuming: that exact string was rejected outright.
`net.SplitHostPort` splits on the *last* colon, so it happily parsed host =
the IP and port = the whole comma-joined tail, but `validateSeedAddr` then
ran `strconv.Atoi` on that tail, which of course isn't a bare number — so
the seed never made it into the config at all. And even hand-editing the
JSON to bypass validation wouldn't have helped: `resolveSeeds` hits the same
wall at resolve time, logs an error, and drops the seed — zero ports tried,
not all of them.

Built real support for it, one syntax addition — `host:port,port,...` — used
in three places that each needed their own change:

- **`internal/config/ops.go`**: `validateSeedAddr` now splits the port
  component on `,` and validates every one is 1–65535, rejecting the whole
  address if any single port in the list is bad (same "whole entry rejects
  together" rule the local multi-port fields already use). A bare single
  port with no comma is just a one-element list, so existing single-port
  seeds validate identically to before.
- **`cmd/gravinet/main.go`**: `resolveSeeds` (UDP) and `resolveTCPSeeds`
  (TCP/TLS) both now expand a multi-port seed into one dial candidate per
  port, all against the same host, each going through the existing
  dedup/overlay-guard logic (`add()` in both functions — factored out as a
  closure in `resolveTCPSeeds`, which didn't have one before, so the
  per-candidate logic wasn't duplicated three ways). For a single-port seed
  the loop runs once and produces byte-identical output and error messages
  to before; `resolveSeeds` specifically keeps track of the *last*
  resolution error and only logs it if every candidate in the list failed,
  so a 12-port seed with one typo doesn't spam eleven-plus log lines, and a
  plain single-port seed's error message is unchanged.

This is a genuinely different shape than the local **UDP port**/**TCP
port** fields' comma list (v341–v342): those distinguish primary
(outbound-sourced, advertised) from extra (inbound-only), because this
node's *own* listen ports have that asymmetry. A seed is something *we*
dial — there's no "primary" candidate among several ports on a remote host,
just N equally-valid attempts — so no primary/extra split was needed here,
and the whole feature is one flat list.

Nothing changed in `internal/mesh` — seeds already flow into the engine as
a plain `[]netip.AddrPort` per network (`spec.Seeds`), built by
`resolveSeeds`/`resolveTCPSeeds` before the mesh engine ever sees them,
same as always; from the engine's side, ten ports on one host and ten
seeds on ten different hosts look identical.

Docs and UI updated to match: the Settings-page pattern from v341/v342 —
mention the syntax right where it's used. `getting-started.md`'s Seeds
section, the Mesh → Seeds hint text, the add-seed input placeholder, and
`gravinet seed`'s doc comment (CLI already worked for free — `cmdSeed`
calls the same `Config.SeedAdd` → `validateSeedAddr` path as the web UI,
so no CLI code change was needed, just the comment).

New tests: `TestSeedPartsAndValidate`/`TestSeedAddValidation` gained
multi-port and malformed-list cases (valid list, one bad port anywhere,
trailing comma); new `TestSeedAddMultiPort` confirms the list is stored
verbatim (expansion happens later, at resolve time, not at add time);
`TestResolveSeedsMultiPort` covers the UDP expansion, including within-list
dedup, and re-confirms the single-port path is unchanged.
`resolveTCPSeeds` had no dedicated tests at all before this — added
`TestResolveTCPSeedsBasics` (no-port default, explicit port, dedup) and
`TestResolveTCPSeedsRejectsOverlayAddress` alongside the new
`TestResolveTCPSeedsMultiPort`, so the function has real coverage going
forward rather than just the new behavior in isolation.

Full Go test suite, `go vet`, and build clean (verified in this sandbox —
Go 1.22 + libpam0g-dev installed via `apt-get` for the cgo/PAM build, same
as v344/v345 should have been checked but weren't, since no toolchain was
available then). `internal/mesh` (104s) and `internal/webadmin` (27s, the
package most of this touches) both pass. Manually re-verified the exact
address from the request end to end: parses, host/port-list split
correctly, and expands into the 13 expected dial candidates.

---

## v345 — 2026-07-11

Requested directly: hovering a peer's name should show that node's local
notes — peer notes if there are any, seed notes if there are any, seed
notes winning when both exist — in Monitor → mesh peers, Monitor →
latency, Mesh → bans, and anywhere else it makes sense.

Investigated the existing notes machinery before writing anything: peer
notes (`Config.PeerNotes`, keyed by node id, exposed per-row on
`/api/status`'s `peers`/`disabled_peers` entries as `Notes`) and seed notes
(`config.Seed.Notes`, exposed via `/api/config`'s `seeds` list) were
already two separate, independently-editable concepts — one attached to a
node id, the other to an address — with no existing link between them.
Building that link is inherently address-based, since a seed has no node
id until it's actually connected: matched a peer's observed underlay
endpoint against configured seed addresses, exact `host:port` first,
falling back to a host-only match (a seed is frequently entered without a
port, or the peer answered on an extra port rather than the one in the
seed list — see v341/v343's extra-port work), skipping any seed with no
notes so an unrelated match can't mask a real one further down the list.

New shared helpers in `internal/webadmin/ui.go` (all client-side, no
backend or wire changes — everything needed was already being fetched,
just not cross-referenced):

- `splitHostPort` — tolerant host:port split, bracketed-IPv6 aware.
- `seedNotesForAddr(netId, endpoint)` / `peerNotesFor(netId, nodeId)` /
  `nodeNotesTitle(netId, nodeId, endpoint)` — the seed/peer lookup and
  precedence (seed wins) described above.
- `notesTitleForNetName(netName, nodeId)` — the same thing for Monitor →
  latency specifically, since `/api/latency` only carries a network
  *name*, not an id; resolves name → id via `state.cfg`, then endpoint via
  `state.status`, same as everywhere else.

`nodeCell` — already the shared renderer for Mesh → bans' target/origin
and Monitor → mesh peers' target, and (checked) *not* previously used by
Monitor → latency, which builds its row markup separately — gained two
new optional params, `netId`/`endpoint`, wrapping its output in a `title`
tooltip when there's anything to show. Existing callers that don't pass
them are unaffected. Monitor → latency got the same tooltip logic inlined
by hand, since it has no `nodeCell` call to extend.

Wired into all three requested locations plus one more:

- **Monitor → mesh peers** and **Mesh → bans** (target and origin) — via
  `nodeCell`'s new params.
- **Monitor → latency** — via `notesTitleForNetName`.
- **Mesh → peers** — not explicitly asked for, but it's `nodeCell`'s third
  caller and already has its own peer-notes column; the new tooltip on the
  *name* now additionally surfaces a seed note there too, which that table
  had no way to show before (its notes column is peer-notes-only,
  editable, and per-node — there was nowhere for a seed's note to appear).

Deliberately left alone: the header's "manage as" node-switcher dropdown.
It's a `<select>`, tooltips on `<option>` elements are inconsistently
supported across browsers, and it's a target-picker rather than a
peer-identity reference in a table — didn't fit the pattern the other four
share.

One real limitation, stated rather than hidden: Mesh → bans has no
endpoint to match against (a ban target may never have been a live peer,
and the ban payload carries no underlay address), so its tooltip is
peer-notes-only there — seed notes can't be cross-referenced. Everywhere
else, both are checked.

No Go changes beyond the version bump — this is `internal/webadmin/ui.go`
only (client-side JS embedded in a Go string literal), so `go
build`/`go vet`/the Go test suite don't exercise any of this logic and
weren't the right check here. This sandbox has no Go toolchain available
to confirm the Go side still compiles unchanged (it should — no `.go`
logic changed except the version constant, and the ui.go edit is a
same-shape string literal with no stray backticks, confirmed by grep), so
that's asserted rather than verified, and worth a real `go build ./...`
before this ships. What *was* checked here: `node --check` on the
extracted `<script>` block for syntax, and every new field access
(`p.Endpoint`/`p.endpoint`, `s.notes`/`s.Notes`, `x.NodeID`/`x.node_id`,
etc.) cross-checked against the exact casing already used by the
surrounding code that reads the same API responses.

---

## v344 — 2026-07-11

Asked directly: what happens if a configured TCP or UDP listen port is
already held by something else at startup — does gravinet skip it and carry
on with whatever else is configured? Went and checked rather than assuming,
port by port, since the honest answer turned out to be "mostly yes, with one
real gap."

**Already correct, verified rather than touched:** UDP's `extra_listen_ports`
(`transport.Open`'s `ExtraPorts` loop) and TCP/TLS's `extra_tcp_listen_ports`
(`OpenTLS`'s, added in v341) were both already best-effort — an extra port
that loses the bind race is logged and skipped, everything else configured
still comes up. UDP's primary port already had somewhere to go on failure
too: `PrimaryPort` plus the six hardcoded `FallbackUDPPorts` are tried in
order, so one busy port there just falls through to the next candidate. The
control socket and web admin listener bind failures were already non-fatal
warnings at the daemon level (there's nothing to "move on to" for either —
each is exactly one configured address, not a list).

**The actual gap: `OpenTLS`'s *primary* fallback port wasn't best-effort at
all.** Unlike its own `ExtraPorts` and unlike UDP's primary, a busy TCP
fallback port (default 65432) made the whole call return an error *before
ever attempting the configured extra TCP ports* — so a node with, say,
`tcp_fallback_port: 65432` and `extra_tcp_listen_ports: [80, 8443]` would
lose all three the moment something else (a stale gravinet process from an
unclean restart is the realistic case) already held 65432, even though 80
and 8443 were sitting free. `main.go` then logged "continuing UDP-only" and
meant it literally — including for the extra ports an operator had
specifically configured to survive exactly this kind of situation.

Fixed by giving the primary port the identical treatment its own extra ports
already get: attempt the bind, and on failure log
`tls fallback port %d not bound (%v) — skipping, trying any configured extra
ports` and fall through to the `ExtraPorts` loop instead of returning.
`OpenTLS` now only errors out if *nothing* configured could bind — neither
the primary nor a single extra port — which is the one case where there's
genuinely nothing left to fall through to and the existing "continuing
UDP-only" handling in `main.go` remains the right call.

Two follow-on details, both to keep the rest of the system honest about
which case it's in:

- Added `TLSTransport.HasPrimary()` (`t.ln != nil`) so a caller can tell
  "running, but only on extra ports" apart from "running normally" — used in
  `main.go` at both startup and live config reload to log a specific warning
  (`tcp/%d fallback port already in use — skipped; still listening on
  configured extra tcp port(s)`) rather than staying silent about a
  degraded-but-functional state.
- Deliberately left `Port()` returning the *configured* primary port
  regardless of whether it actually bound, rather than 0 or some other
  sentinel. Checked how it's actually used first: nothing reads it as "is
  this really listening" (that's what the new `HasPrimary` is for) — its one
  production call site (`main.go`'s `reqTCPPort`) only ever compares it
  against another configured value to detect a *settings change* across a
  reload, and gravinet already advertises the extra-ports list to peers by
  configured value independent of actual bind success (same as
  `ExtraTCPPorts` elsewhere) — so changing what `Port()` reports here would
  have been inventing a new convention for one caller instead of matching
  the one already established everywhere else this comes up.

New tests in `internal/transport/tcptls_extraport_test.go`:
`TestTLSPrimaryPortBusySkipsToExtra` (busy primary, free extra — extra still
binds and carries a real ping/pong frame) and `TestTLSAllPortsBusyFails`
(primary *and* extra both busy — confirms `OpenTLS` still correctly errors
out rather than silently returning a transport with nothing listening).
`TestTLSExtraPortBadPortSkipped`, already covering the extra-port side of
this contract, needed no changes.

Full Go test suite, `go vet`, and build clean, `-race` included on the
touched packages.

---

## v343 — 2026-07-11

Extra listen ports are now advertised to peers, not just listened on —
closing the gap identified two versions ago when asked directly why the
handshake only had room for one TCP port: "seems like a limitation. maybe
it should be this." Investigated before writing anything, since a wire
protocol change is a different order of risk than a config field: found
the format is a genuinely extensible trailing-field design already
extended repeatedly (subnets, name, managed/manager, web port, TCP port
were all added this exact way, each tolerant of an older peer simply
omitting it), so adding two more fields was the established move, not a
new mechanism. Also found — and corrected — an overstatement from that
same conversation: the mesh does *not* assume a mesh-wide shared TCP
port; `ensureFallback`'s existing port resolution already dials each
peer's individually advertised port. The real gap was narrower: one port
each, not a list, and no gossip-level advertisement of extra UDP ports at
all.

**Wire format**, extended in three separate places, since this data
travels three different ways:

- **Handshake payload** (`handshake.go`) — two new optional trailing
  fields, `ExtraTCPPorts`/`ExtraUDPPorts`, each count-prefixed
  (`appendPortList`/`readPortList`), nested after `TCPPort` the same way
  every prior optional field nests after the one before it — only
  attempted if the previous field was present, so an older peer's shorter
  payload just leaves these nil.
- **Gossip peer-list entries** (`control.go`) — a *separate* wire format
  from the handshake, used when peer info propagates transitively (A
  tells B about C). The existing single TCP-port trailing block
  (`peerListTCPBlock`) was a one-shot check, not a loop — never needed to
  be more, since it was the only optional block. Converted to a proper
  loop over marker bytes (`peerListExtraTCPBlock`/`peerListExtraUDPBlock`
  join it) so more blocks can follow; an unrecognized marker stops
  parsing there rather than guessing at a length it can't know, and an
  older decoder is unaffected either way since it never reads past its
  own one-shot check to begin with.
- **Live cluster-state push** (`ban.go`, `announceClusterState`/
  `onClusterNotify`) — a *third* mechanism, easy to miss: already-connected
  peers get Managed/Manager/WebPort/TCPPort pushed live on a change,
  without waiting for a reconnect. Missing this would have meant a live
  edit to Settings → Underlay's extra ports only reaching peers who
  happen to reconnect afterward, not the ones already meshed — silently
  half-working in exactly the way that's hardest to notice.

Found one real bug in my own draft while writing the round-trip tests:
`readPortList` returned "keep whatever was read, report success anyway"
on a truncated list, matching the tolerant spirit of the rest of this
decoder — but for the *gossip* format specifically (many entries, each
with its own list, read back-to-back), silently accepting a truncated
list would desync every entry after it, misreading unrelated bytes as
the next entry's data. A genuine older peer never emits a count it
doesn't follow through on — only malformed or adversarial input does —
so truncation now correctly reports failure and every caller stops
rather than continuing on bytes that no longer mean anything.

**Engine wiring** (`engine.go`, `handshake_engine.go`): `peerSession`/
`nodeInfo` gained `extraTCPPorts`/`extraUDPPorts` fields, propagated
everywhere `tcpPort` already was — fresh session install, gossip
learning (gated the same "direct handshake stays authoritative over
second-hand gossip" way `tcpPort` already is), and the live push above.
This node's *own* advertised extra ports live in two new
`atomic.Pointer[[]uint16]` engine fields (`fallbackPort`'s shape, just
for lists instead of a scalar), set via new `SetExtraTCPPorts`/
`SetExtraUDPPorts` — which also call `announceClusterStateAll()`
immediately, the same live-push `SetManaged`/`SetManager` already do,
so a config change doesn't wait for anyone to reconnect to notice it.

**Actually using the advertised ports** turned out to split into two
genuinely different shapes, not one:

- **TCP**: `ensureFallback` resolved to exactly one candidate port before;
  refactored into `ensureFallback` (resolves the candidate set: the
  existing priority-ordered primary, plus every advertised extra) calling
  a new `dialFallbackCandidate` (the *original* per-port claim/dial body,
  extracted unchanged) once per candidate. Deliberately parallel, not
  sequential-with-a-timeout: the whole point of an extra port is not
  knowing in advance which one gets through a restrictive firewall, which
  is exactly the case where the primary is most likely to be the one
  that fails — paying its full timeout before trying alternates would
  work against the feature's own purpose. Multiple candidates succeeding
  is harmless, not something this cancels for: each resolves to its own
  distinct address independently claimed and dialed, so they can't
  collide the way redialing the *same* address concurrently would.
- **UDP**: no equivalent per-candidate dial loop was needed. A node's
  *primary* UDP endpoint is already learned automatically (a peer just
  observes which port a handshake packet arrived from) via the existing
  seed pool and `initLoop`'s retry/backoff/dedup — so extra UDP ports
  just became additional seed candidates fed into that same pool
  (`learnPeers` in `control.go`, one `AddSeedFor` per advertised extra
  port, deduped against the primary) rather than a parallel mechanism
  built to match TCP's. Reusing an existing, already-correct pool beat
  building a second one.

Config → engine plumbing (`main.go`): `mesh.Options` gained
`ExtraTCPPorts`/`ExtraUDPPorts` (new `toUint16Ports` helper converts
config's `[]int` to the wire/engine `[]uint16`), wired at startup and
into both reload branches — reusing v341's existing `reqExtraUDP`/
`reqExtraTCP` change-detection (added then for the transport-socket
rebind) as the same trigger for `engine.SetExtraTCPPorts`/
`SetExtraUDPPorts`, so one config change updates both the sockets
actually listening and what gets advertised about them, not just one.

Tests, three new files rather than one, matching the three genuinely
different things being tested: `extraports_wire_test.go` (encode/decode
round-trips for both wire formats, plus the backward/forward-compat
truncation cases the fix above was found while writing),
`fallback_extraports_test.go` (the TCP parallel-dial loop against a fake
transport — confirms all candidates get dialed, confirms a duplicate
between primary and extra doesn't double-dial), `learnpeers_extraports_test.go`
(the UDP seed-injection path against a real two-node live mesh, not a
fake — confirms gossiped extra ports actually become seeds, confirms the
primary isn't re-seeded when it happens to duplicate an extra). Also
fixed two now-incorrect truncation offsets in the *pre-existing*
`TestHSPayloadCarriesTCPPort` test, broken by adding fields after the
one it was hardcoded to assume was last.

One thing deliberately not built: a dedicated full end-to-end live test
(two real engines, one reachable only via an extra port). Judged
redundant rather than skipped for time — `dialFallbackCandidate` is the
original `ensureFallback` body verbatim, already exercised end-to-end by
the pre-existing `TestEnsureFallbackDialsAndSeeds`; the new tests confirm
the surrounding loop calls it correctly, not that the body itself works,
which was never in question.

Full Go test suite, `go vet`, and build clean across the whole project.
The one pre-existing flaky test hit while running the full suite under
`-race` (`TestKeyDisableReconnects`, unrelated key-rotation logic —
passes in isolation in 2s, only intermittently times out under full-suite
resource contention with `-race`'s overhead) — confirmed unrelated by
running it standalone before moving on, not just assumed.

---

## v342 — 2026-07-11

Reworked v341's multi-port UI, per direct correction: no separate "Extra
UDP ports"/"Extra TCP ports" fields — the existing **UDP port** and **TCP
port** fields themselves now just take a comma-separated list, first entry
primary, rest extra. One field per protocol, not two.

The underlying data model didn't need to change — `PrimaryPort`/
`ExtraListenPorts` and `TCPFallbackPort`/`ExtraTCPListenPorts` stay two
separate config fields each, since the transport layer genuinely
distinguishes them (the primary is used for outbound dialing and seed
resolution; extras are inbound-only) — only how they're *edited* changed.

Backend: `handlePort`/`handleTCPPort` now take `{ports: []int}` instead of
`{port: int}` — `ports[0]` becomes the primary/fallback port,
`ports[1:]` becomes the extra list, at least one port required. The two
separate endpoints v341 added (`/api/extraudpports`/`/api/extratcpports`)
and their handlers are gone entirely — this is an internal API with no
other caller (confirmed again, same check as v341: no CLI command touches
either endpoint), so changing the request shape outright was safe, nothing
to keep backward-compatible.

Web UI: `buildPortListRow` (new in v341 for the now-removed separate rows)
kept, simplified to always require at least one port, and pointed at the
existing `udpport-row`/`tcpport-row` ids instead — so search navigation
and everything else already keyed to those ids kept working unchanged.
The old single-number `prInput`/`tpInput` blocks and their inline
save handlers are gone, replaced by two `buildPortListRow` calls that
combine `state.primaryPort`+`state.extraUDPPorts` (and the TCP
equivalents) into one display list on load, and split a saved list back
into the same two state fields on save.

Tests: `port_handler_test.go`/`tcpport_handler_test.go` rewritten for the
list-based request shape — single-port list (unchanged behavior), a
multi-port list split correctly into primary+extras, an invalid port
anywhere in the list rejecting the whole thing, and an empty list
rejected outright (a primary is mandatory, unlike the old separate
extra-ports endpoints which correctly allowed empty). The two test files
v341 added for the now-deleted separate endpoints are deleted with them.

Docs: `getting-started.md`'s mention of "Extra UDP ports"/"Extra TCP
ports" as named fields corrected to describe the actual UI (comma-separated
lists in the existing UDP port/TCP port fields). README's mention didn't
name specific fields, so it was already accurate and needed no change —
checked rather than assumed.

Full Go test suite, `go vet`, and build clean, `-race` included.

---

## v341 — 2026-07-11

Multi-port underlay listening, requested directly: type a comma-separated
list of UDP and TCP ports into Settings → Underlay and this node listens
on — and responds on — all of them, not just the one primary port each.

Turned out to be two different sizes of work, discovered by investigating
before building anything: the UDP side (`extra_listen_ports`) already
existed, completely, in `internal/transport` — bound concurrently with the
primary port, reply-routed back out whichever socket a peer actually
arrived on, best-effort (an unbindable port is logged and skipped, not
fatal), wired into both startup and live reload in `main.go`. It had
*zero* exposure anywhere outside the raw config JSON — no CLI, no web UI,
not even a mention — so nobody could have known it was there without
reading the source. The TCP/TLS fallback had no equivalent at all:
`TLSOptions` took exactly one `Port int`, full stop.

**Built the TCP side from scratch, in the same shape as the UDP one.**
Turned out simpler in one respect: TCP is connection-oriented, so "reply
on the port a peer arrived on" is automatic — each accepted connection is
already its own `net.Conn`, registered by remote address regardless of
which listener produced it, so `Send` already writes back down the right
connection with no UDP-style reply-routing bookkeeping needed.
`TLSTransport` gained `extraLns []net.Listener` alongside the existing
primary one; `acceptLoop` was parameterized to serve any listener instead
of hardcoding the primary; `OpenTLS` binds each configured extra port the
same best-effort way UDP does — skip and log, never fail the whole call —
and `Close` tears all of them down. New config field
`extra_tcp_listen_ports`, validated the same way `extra_listen_ports`
already was.

**Fixed a real gap in the already-shipped UDP feature while wiring the new
TCP one, not something this session introduced:** the live-reload path
only ever reopened the underlay transport when the *primary* port
changed — changing `extra_listen_ports` alone, with the primary
untouched, went completely unnoticed until the next unrelated port change
or a full restart. Added tracking for the currently-applied extra-port
lists (`reqExtraUDP`/`reqExtraTCP`, `slices.Equal` against the incoming
config) so a change to *just* the extra list now triggers a live reopen
on its own, for both UDP and TCP — the TCP side additionally gated on the
fallback listener actually being enabled, since there's nothing to attach
extra ports to while it's off.

Web UI: two new Settings → Underlay rows, **Extra UDP ports** and **Extra
TCP ports** — comma-separated text inputs, applied on blur, reverting the
whole field on any single invalid entry rather than trying to partially
accept a list with one bad port in it (simpler and more predictable than
partial acceptance, matching how the existing single-port inputs already
handle an invalid value). New `buildPortListRow` helper so the two don't
duplicate the parse/validate/save logic. Two new endpoints,
`/api/extraudpports` and `/api/extratcpports`, mirroring `/api/port`'s
existing shape exactly. Both new settings indexed for global search, and
both values added to the status response so the fields populate correctly
on page load — checked for a second status-serving endpoint the way an
earlier version's NAT-class fix needed to, and confirmed there's only the
one.

No CLI command added for either — checked first, and there isn't one for
the *existing* `primary_port`/`tcp_fallback_port` settings either; those
have only ever been config-file or web-UI. Adding CLI-only support for
the new extra-ports settings while the closely-related primary ones still
lack one would have been the more inconsistent choice, not the less.

New tests, not just reused ones: `TestTLSExtraPortAcceptAndReply` and
`TestTLSExtraPortBadPortSkipped` (`internal/transport`, modeled directly
on the UDP side's existing `TestExtraPortListenAndReply`, reusing the
package's own `openTLSLoopback`/`Dial` pattern rather than hand-rolling a
raw TLS client), plus `TestHandleExtraUDPPortsChangesConfigAndReloads`/
`TestHandleExtraTCPPortsChangesConfigAndReloads` (`internal/webadmin`,
modeled on the existing `handlePort`/`handleTCPPort` tests) covering the
new HTTP handlers end to end — valid list saved and reload triggered,
empty list clears it, one invalid port rejects the whole list with the
config left unchanged. One thing intentionally *not* independently
covered by a new automated test: the specific "extra-ports-only change
triggers reload" branch in `main.go`'s `reloadFn`, since nothing in this
codebase unit-tests that closure directly — it's a giant closure tightly
coupled to full daemon startup, and no existing `cmd/gravinet` test
touches it either. Verified by hand-tracing all four cases (both
unchanged, primary changed, extra changed, fallback off) instead, same
confidence level the rest of `reloadFn` already carries.

Full Go test suite, `go vet`, and build clean, `-race` included. README's
Features section and getting-started.md's section 1 (already about port
reachability) both updated with a short, honest mention — this is new
capability, not a naming/doc cleanup, so it earns real documentation
rather than a passing reference.

---

## v340 — 2026-07-11

Removed the `LICENSE` and `third_party/wintun/` hyperlinks from
`README.md`'s License section, per direct instruction — same treatment as
the getting-started.md link in v339, plain inline-code filename references
instead of markdown links. Reflowed the wintun paragraph's line wrapping
afterward, since it was hand-wrapped around the old, longer link syntax
and read a little oddly with the shorter plain text in the same spots.

Pure content change, no code touched.

---

## v339 — 2026-07-11

Three small, direct README/getting-started requests in one pass.

**Moved Features to the top of `README.md`, before Build.** Pure
reordering — cut the section from between Test and Status, pasted it
right after the intro, checked the seams on both sides (Test → Status
still reads with exactly one blank line between them; Features → Build
likewise) since the section text itself is identical either way and
"did the move corrupt the surrounding blank-line spacing" is the only way
this particular edit could actually go wrong. `grep -n "^## "` confirms a
single Features header now, in the new position, nothing left behind at
the old one.

**Removed the redundant top-level heading from `getting-started.md`.**
Same bug class as v334 (Readme/License/About/Speedtest/Logs all once
repeated their own page title inside their card) — a `# Getting started
with [gravinet]` heading rendered directly under the page's own `<h2
class="sec">Getting Started</h2>`, saying the same thing twice. Removed;
the guide now opens straight into its intro paragraph. Re-verified through
the real `mdRender()`, not just eyeballed: h1 count in the rendered output
went from 1 to 0, h2 (20) and hr (20) counts unchanged, confirming nothing
else shifted.

**Removed the getting-started.md hyperlink from `README.md`'s Quick
start**, per direct instruction that it's unnecessary — the in-app pointer
(**Info → Getting Started**) it sat next to already says where to go, and
(per v338's own note) that link would 404 if actually clicked from inside
the running admin anyway, since nothing serves a raw `/getting-started.md`
route on purpose. One less link that only worked in one of its two
contexts.

Pure content changes, no code touched. Full Go build and test suite clean
regardless, since the last few versions' worth of header-editing mistakes
in this file made "verify structure, don't just trust the diff" worth
doing as a habit even for changes with zero Go involved: `grep -n "^## v"`
checked for gaps or duplicates across the full version sequence before
calling this done.

---

## v338 — 2026-07-11

Removed `getting-started.html` entirely, per direct instruction: v337 kept
both it and `getting-started.md` around for genuinely different contexts
(standalone polished page vs. natively-styled in-app section), but that
was still two copies of the same guide to keep in sync forever, for a
distinction not worth the ongoing cost. One file now — `getting-started.md`
— read fresh from disk on every request via `/api/getting-started`
(`serveDocFile`, the same no-caching-in-app-memory shape README/LICENSE
already use), same as it already was for the in-app page specifically; the
only actual change is that the *second* file/route this version deletes
never gets touched at all now.

Deleted the file, then removed everything that existed only to serve it:
`handleGettingStartedFile` and the `/getting-started.html` route
(`internal/webadmin/webadmin.go`, `edit.go`) — the raw, non-JSON-wrapped
endpoint that only ever existed for that file. Collapsed the config/server
plumbing that briefly had two names for two files back down to one:
`GettingStartedMDFile`/`GettingStartedMDPath`/`gettingStartedMDPath`/
`SetGettingStartedMDPath` are gone; `GettingStartedFile`/`GettingStartedPath`/
`gettingStartedPath`/`SetGettingStartedPath` (the original names, present
since v336) now simply mean the one remaining file — `getting_started_path`
in the config JSON keeps meaning "where's the getting-started doc," same
key, just one less thing for it to disambiguate. `TestHandleGettingStartedFile`
(the raw-serve test) is gone with the code it tested; `TestHandleGettingStarted`
updated to call the renamed setter.

Every installer, every uninstaller, and `scripts/build-release.sh` had
`getting-started.html` added in v336/v337 for exactly this file — all of
it removed, `getting-started.md` is all any of them reference now. Grepped
the whole repo for every symbol this touched (`GettingStartedMDPath`,
`GettingStartedMDFile`, `SetGettingStartedMDPath`, `gettingStartedMDPath`,
`handleGettingStartedFile`, `getting_started_md_path`, and the literal
string `getting-started.html`) to confirm nothing was left dangling,
rather than trust memory of which files touched it across three versions.

One real, honest trade-off from this, not hidden: the README's own
pointer used to be a working link (`getting-started.html`, served
in-app), and a bare link to `getting-started.md` would 404 the same way
that one originally did if clicked from inside the running web admin —
nothing serves a plain `/getting-started.md` route, deliberately, since
the only in-app consumer is the JS's own live JSON fetch, not a public
raw-file endpoint. Rather than add one back just to keep a clickable link
alive, `README.md`'s Quick Start now leads with the actual in-app
destination (**Info → Getting Started**) and mentions the raw file
secondarily — which still works correctly in its real primary context
(reading the repo, e.g. on GitHub, where `.md` files render natively) and
simply isn't a live link inside the app anymore.

Full Go build, `go vet`, and test suite clean. Every modified shell
script re-checked with `bash -n`; the PowerShell edits reviewed by hand
again (no `pwsh` available here), same as v336/v337.

---

## v337 — 2026-07-11

Reworked the Info → Getting Started page (v336) from an iframe to injected,
natively-styled content, per direct feedback: the iframe correctly kept
getting-started.html's own look intact, but that was exactly the problem —
its own light-themed stylesheet, unrelated to the app's dark/light theme,
made it look like a foreign page bolted onto the sidebar rather than part
of the app.

The real fix couldn't be "inject the HTML instead of iframing it" as
literally asked, though — `getting-started.html` is a standalone document
built around its own `<style>` block; stripping that and injecting the
bare markup would either drag broken/unstyled tag soup into the page or
require hand-porting its CSS into the app's own theme, fragile either way.
`secReadme` already solves this exact problem correctly for README: fetch
markdown *source*, run it through `mdRender()` — the app's own hand-rolled
renderer, which builds every element already styled with the app's own
`var(--bg)`/`var(--fg)`/`var(--line)`/etc. CSS variables — and inject the
result. Native styling is what that function is *for*.

So: authored `getting-started.md`, a markdown-source rewrite of the guide
(same 18 sections, same content, reformatted for the renderer's actual
capabilities), and pointed the Info → Getting Started page at it instead.
The original `getting-started.html` is untouched and still exists — it's
still what the rendered README's own getting-started.html link opens (a
polished standalone page that works even without the web admin running,
e.g. before you've installed anything), a genuinely different context from
"already inside the app, wanting it to look like the rest of the app."
That's two files to keep in sync from now on, a real ongoing cost worth
naming rather than hiding — direct consequence of native styling requiring
a source that isn't tied to the other file's separate stylesheet.

`mdRender` (`internal/webadmin/ui.go`) needed three additions the
existing README content never exercised, since the guide's original HTML
used ordered lists, horizontal rules, and italic emphasis throughout —
none previously supported by this hand-rolled renderer:

- Horizontal rules (`---` on its own line) — the guide's own major-section
  dividers, 20 of them.
- Ordered lists (`1.`/`2.`/...) — the previous list handling was
  unordered-only; generalized the existing list-state tracking to a
  null/'ul'/'ol' tri-state so switching list types mid-document correctly
  closes one and opens the other, rather than assuming only one kind can
  ever appear.
- Italic (`_text_`, not `*text*` — deliberately a different marker than
  `**bold**` so there's no ambiguity to resolve by asterisk-counting).

Also had to touch the paragraph-continuation lookahead, which previously
only knew to stop before a heading, list item, or code fence — a `---` or
`1. ` line immediately after a paragraph with no blank line between them
would otherwise have been swallowed into the paragraph text instead of
starting its own block.

Backend: renamed the split cleanly rather than overload one path —
`GettingStartedPath`/`SetGettingStartedPath` (added in v336) keep
resolving `getting-started.html` for the raw-serving endpoint the README
link uses; new `GettingStartedMDPath`/`SetGettingStartedMDPath` resolve
`getting-started.md` for the JSON endpoint (`/api/getting-started`, same
`{text, path, available}` shape as README/LICENSE) the section actually
renders. `TestHandleGettingStarted` updated to match — it now writes and
serves a `.md` fixture instead of `.html`; `TestHandleGettingStartedFile`
(the raw-serve path) is untouched, since that behavior didn't change.

Every installer/uninstaller and `scripts/build-release.sh` that got
`getting-started.html` added in v336 needed `getting-started.md` added
too, for the same reason: this only works on a real install if the file
is where `GettingStartedMDPath` looks for it. Grepped for every
`getting-started.html` reference left over from v336 to make sure none
were missed, rather than trust memory of which files were touched.

Verified the actual content, not just the renderer in isolation: ran the
real `getting-started.md` all the way through `mdRender()` and checked
the output — tag counts matched what the source should produce exactly
(1 h1, 20 h2, 20 hr, 10 ul, 2 ol, 2 pre, 0 stray links — internal
cross-reference links and the external ones were both dropped
deliberately, since `mdRender` doesn't generate heading ids for the
former to target and the latter weren't essential), and every block-level
tag opened and closed in equal counts. One apparent leak (a literal `#`
appearing in the output) turned out to be a false positive in my own
check — it's the `# then open https://localhost:8443 locally` shell
comment, correctly preserved literally inside a fenced code block, not a
broken heading.

Full Go test suite and build clean.

---

## v336 — 2026-07-11

Fixed the getting-started.html link added to `README.md` last version
404ing when clicked from the web admin's rendered Readme page — reported
live. Root cause: nothing on the web admin server served that path at all,
regardless of whether the file existed on disk. Fixed by actually serving
it, and added a dedicated Info → Getting Started page for it too, between
Readme and License as suggested.

Backend (`internal/webadmin/webadmin.go`, `edit.go`,
`internal/config/config.go`): added `GettingStartedPath`, mirroring
`ReadmePath`/`LicensePath` exactly — explicit `getting_started_path`
override, else search the same install-standard locations. Two new
routes rather than one, because this file needed different handling than
every other doc view here: `/api/getting-started` returns the usual
`{text, path, available}` JSON (so the new section can show the same
"not installed" message Readme/License already do), but `/getting-started.html`
serves the *raw* bytes via `http.ServeFile` — this is the URL both the
rendered README's relative link and the new section's iframe actually hit,
and it needs a real `text/html` response, not a JSON envelope.

Why an iframe and not injected markup: unlike README (markdown) or LICENSE
(plain text), `getting-started.html` is a full standalone page with its
own `<style>` block and page chrome, built to be opened directly in a
browser. Stripping that down to inject as text would have meant losing
its styling entirely; an iframe pointed at the raw endpoint keeps it
intact in its own document, the same reasoning already applied to the
Geo-IP map embed elsewhere in this file.

`internal/webadmin/ui.go`: new `secGettingStarted` (modeled on
`secReadme`, no redundant header per v334 — the page's own `<h2>` already
says it), a `getting-started` entry in `NAV_GROUPS`' info group between
`readme` and `license`, a `label()` case ("Getting Started", not the
default hyphen-preserving capitalization), and a dispatch-table entry.
Global search picks the new section up automatically — it's driven by
`NAV_GROUPS`, no separate index code needed.

This only actually works on a real install if the file is where
`GettingStartedPath` looks for it, so the four Unix installers (Linux,
macOS, FreeBSD, OpenBSD) and their uninstallers, `install-windows.ps1`
and `uninstall-windows.ps1`, and `scripts/build-release.sh`'s release
bundling all got `getting-started.html` added alongside every existing
README/LICENSE copy-and-cleanup line — checked with a repo-wide grep for
the pairing afterward to confirm none were missed, not just the ones
remembered.

Added `TestHandleGettingStarted`/`TestHandleGettingStartedFile`
(`internal/webadmin/readme_test.go`), modeled directly on the existing
Readme/License handler tests. Caught and fixed two mistakes in my own
edit to that file before they shipped — a botched insertion that dropped
`TestHandleLicense`'s function signature entirely, then a second pass
that fixed the signature but dropped its `lp` variable declaration in the
same spot — both caught by actually running `go vet`/`go test` against
the change rather than trusting the diff looked right, which is exactly
why that step exists. All four tests pass now. Every modified shell
script syntax-checked with `bash -n`; the two PowerShell files couldn't
be syntax-checked (no `pwsh` in this environment) so were reviewed by
hand instead — both edits are single-line, low-risk string insertions
into an existing, unchanged list literal.

Full Go test suite, `go vet`, and build clean.

---

## v335 — 2026-07-11

Linked `getting-started.html` from `README.md`'s Quick start section —
previously mentioned nowhere in the README at all (checked by grep, zero
hits), despite being a full 18-section walkthrough already sitting in the
repo.

Considered and rejected pasting its contents into the README directly, per
how the request first came in: `getting-started.html` is a full standalone
page with its own `<style>` block and page chrome, meant to be opened in a
browser, not embedded as a Markdown fragment — doing that literally would
have meant hand-converting ~550 lines of styled HTML and roughly
quadrupling the README's length. Checked the actual overlap first, too:
README's "Quick start" is a 3-command, CLI-only stub; the HTML guide is a
much larger, mostly web-UI-focused 18-step onboarding doc (install through
monitoring) that was never really duplicating it, just going further in a
different direction nobody linked to.

Fix: left "Quick start" as the minimal CLI path (relabeled as such), added
a pointer to `getting-started.html` right above it for the fuller
walkthrough. Pure content change, no code touched.

---

## v334 — 2026-07-11

Removed a repeated-title pattern in the web admin, flagged with screenshots
of four pages (Readme, About, Speedtest, License) each showing the page's
own `<h2>` title immediately followed by a card `<h3>` repeating the exact
same word again, uppercased by `.card h3`'s styling — visually "Readme" /
"README" back to back with nothing between them.

Checked every single-card section in `internal/webadmin/ui.go` against its
own page label, not just the four flagged, since the same pattern could've
been anywhere. Found one more with the identical bug: Logs (`secLogs`).
Fixed all five (`secReadme`, `secLicense`, `infoAbout`, `infoSpeedtest`,
`secLogs`) by removing the duplicate card-level `<h3>` — the page's own
`<h2 class="sec">` already says it once. Two of the five (Speedtest, Logs)
had a hint paragraph right below the h3 with a `margin:-4px` compensating
for sitting under it; adjusted to `margin:0` now that there's no h3 above
it to pull up against, so removing the header didn't leave the following
text sitting slightly too close to the card's top edge.

Deliberately left five superficially-similar cases alone, checked
individually rather than assumed: Capture ("Packet capture" vs. the page's
"Capture"), Route Table ("Local routing table" vs. "Route Table"), Hosts
File ("Local hosts file" vs. "Hosts File"), DNS State ("Conditional DNS
forwarding — live state" vs. "DNS State"), and Latency ("Latency to mesh
peers" vs. "Latency"). Each of these card headers says something the page
title alone doesn't — "local," "conditional," "to mesh peers" — so removing
them would have traded a real clarification for tidiness, not fixed a
repeat. Also confirmed Settings' own "NAT" card heading (under the
"Settings" page title) is a legitimate sub-grouping label, not this bug —
same shape as Settings' Appearance/Cluster/Routing/Underlay/Privacy cards,
just a different card on the same page, not a repeat of anything.

Full Go test suite and build clean; entirely client-side JS, no Go-side
changes.

---

## v333 — 2026-07-11

Added a creator byline to `README.md` — "*Created by micush.*" right under
the title, the natural spot for it and the most visible one. Pure content
change, no code touched.

---

## v332 — 2026-07-11

Brought `README.md` up to date — it hadn't tracked several sessions' worth
of real feature work, and had drifted into actively describing a smaller
product than what's actually there.

Specifically stale:

- The web admin sidebar description still listed a flat "Networks, Peers,
  Bans, Routes, Firewall, NAT, QoS, Bandwidth" — missing Keys, Seeds, DNS,
  Hosts, the entire Monitor group (metrics, mesh peer detail, capture,
  speedtest, latency, route table, hosts file, DNS state, logs), the
  entire Info group, and Settings. Rewritten to match the actual grouped
  nav structure (Mesh / Traffic / Naming / Monitor / Info, plus Settings
  and Sign out pinned at the bottom).
- No mention anywhere of the global search feature (v325–v331) — a
  significant piece of UI with no trace in the docs. Added to the Web
  admin section.
- `seed`, `host`, `fw exempt`, `managed`, and `manager` are all real,
  working CLI subcommands with zero README examples — found by checking
  `cmd/gravinet/main.go`'s actual top-level switch against what the
  "Managing the config" examples covered, not by memory. Added examples
  for all five; caught and fixed one mistake before it shipped
  (`fw exempt add` needs `-name` as a flag, not a positional argument —
  checked `cmdFWExempt`'s real flag parsing rather than trust the first
  draft). Also noted, honestly, that DNS forwarding has *no* CLI yet —
  web UI or hand-edited JSON only.
- The Firewalling feature bullet didn't mention the global allow list at
  all (CLI `fw exempt`, or the Allow List tab as of v330) despite it
  existing well before this session. Also added a one-line note on what
  "firewall direction" actually covers — in/out relative to the tunnel,
  with forwarded traffic sharing the same two rule directions rather than
  being its own category — straight from the answer given earlier this
  session when asked the same question directly.
- IP forwarding (`internal/ipfwd`, enabled by default on startup, opt-out
  via config) had no mention anywhere in the README at all. Added as its
  own Features bullet.
- The CLI/daemon Features bullet's command list had the same undercount
  as the prose examples — missing `seed`, `key`, `host`, `managed`,
  `manager`. Fixed to match.

Nothing here is new capability — every feature described was already
real and working; the README just hadn't caught up to it. No version
string is embedded in the README itself, so this is a pure content
update, verified line-by-line against the actual CLI switch statement and
UI structure rather than assumed from memory.

---

## v331 — 2026-07-11

Fixed Settings' own content missing from global search — reported live.
Settings has had exactly one index entry since v326 (the section name
itself, "Settings"); the nine actual settings inside it — Dark mode,
Managed mode, Manager mode, Remote shell, Route advertisement interval,
UDP port, TCP port, NAT state timeout, and Geo-IP lookups — were never
indexed at all, the same shape of gap as the exempt list before v329, just
never caught because nobody had asked about Settings specifically until
now.

Six of the nine settings rows (`internal/webadmin/ui.go`, `secSettings`)
had no `id` attribute to navigate to at all — only Managed/Manager/Remote
shell had one, added earlier for `syncClusterModeRows`' own
`getElementById` lookups, not for search. Added stable ids to the
remaining six (dark mode, route advertisement, UDP port, TCP port, NAT
state timeout, Geo-IP) so every setting can be scrolled to and flashed
like any other search result.

Indexed each with its label and description as searchable text — for
Managed/Manager specifically, whose descriptions are dynamic
(`syncClusterModeRows` swaps them depending on whether a remote peer is
selected), the *local*-node description is what's indexed, since that's
the text most people searching for these would actually be picturing.
`navigateToSearchResult` gained a `setting` match kind, resolved by plain
`getElementById` rather than the per-network card-scoping every other
section's rows need — Settings has no network concept, so there's nothing
to scope against.

Caught before shipping, not after, the same class of gap v330 already
hit once with "allowlist": "geoip" (one word, how the term is normally
written) matched nothing against a label that reads "Geo-IP lookups" —
fixed by adding the one-word spelling to that entry's searchable text.
Checked a few more candidates in the same pattern ("udpport", "tcpport",
"natstate") and left them alone — unlike "geoip" or "allowlist", nobody
searches for a setting called "NAT state timeout" by mashing three words
into one; "udp port" (with the space) already works fine.

Verified with the same kind of standalone Node harness as the last several
entries — all nine settings resolve by name, several also resolve by a
phrase pulled from their description alone (e.g. "ipapi" finds Geo-IP
lookups). Full Go test suite and build clean; still entirely client-side
JS.

---

## v330 — 2026-07-11

Split Firewall (`internal/webadmin/ui.go`) into two sub-tabs — Rules
(the existing per-network rule tables, unchanged) and Allow List — and
moved the global firewall allow list into the new Allow List tab, out of
Settings where it lived until now.

New `buildTabBar` helper renders the tabs, reusing the `.seg`/`.seg-btn`
segmented-control styling already used for the Metrics duration selector —
the same "switch what's showing within a section" role, so this isn't a
new visual pattern, just a second use of an existing one. `state.firewallTab`
(defaulting to `'rules'`) tracks which is active; `secFirewall` renders the
bar first, then branches. Allow List is handled *before* the "no networks
configured" early-out, since — unlike Rules — it's node-global and has
something to show regardless of how many networks exist.

`secAlwaysAllowed` itself is otherwise unchanged (same fields, same live
apply-on-edit behavior) — just relocated, with its heading shortened from
"Firewall Allow List" to "Allow List" (redundant now that it's already
under a Firewall tab) and its doc comment and hint text updated to stop
saying "Settings."

Search index updated for the move: exempt-list entries now point at
`section:'firewall'` instead of `'settings'`. Firewall gaining sub-tabs
also meant a plain section-name hit ("firewall") no longer fully says
where to land, since it doesn't know which tab you want — so two new
explicit entries, "Rules" and "Allow List", each carry their own `tab` in
their match descriptor, and existing firewall-rule entries (`kind:'fw'`)
now carry `tab:'rules'` too. `navigateToSearchResult` applies `match.tab`
to `state.firewallTab` before rendering, generalizable to
`state.<section>Tab` if another section grows tabs later. Caught one gap
before shipping, not after: "allowlist" (one word) matched nothing even
though "allow list" (two words, the actual tab label) did — substring
matching doesn't bridge a missing space — fixed by adding both spellings
to that entry's searchable text.

Verified with the same kind of standalone Node harness as the last several
entries: "firewall" still finds the section (tab left as whatever it was);
"rules" and "allow list"/"allowlist" both resolve to Firewall with the
correct tab; an allow-list entry ("BGP") and a firewall rule's port
("8443") both resolve with the right tab attached. Full Go test suite and
build clean — no Go-side changes, this is entirely the client-side JS/CSS
inside `ui.go`'s embedded admin page, and no Go test references the moved
text.

---

## v329 — 2026-07-11

Asked directly again ("anything else that needs to be searched?"), so this
one is a proactive audit rather than a reported symptom: enumerated every
`<th>` column header across every table in the admin UI and cross-checked
each against `buildSearchIndex`, instead of waiting for the next thing to
turn up missing one report at a time. Found five more real gaps:

- **Firewall rule ports** (`dport_min`/`dport_max`) were never in the
  label or hay at all, despite the exact `portLabel` helper already being
  used for QoS rules two lines below. Now included the same way.
- **NAT rule interface** (`r.interface`) was missing — now folded into the
  label the same parenthetical way the rendered table cell already shows
  it (`masquerade (eth0)`).
- **Peer overlay and endpoint addresses** weren't searchable — only
  hostname/id/notes were. Both added to a peer's hay, so "who's at
  100.64.0.7" or "who's connecting from 203.0.113.9" now resolves.
- **Seed transport** (tcp vs udp) was invisible to search because
  `stripScheme` — correctly, for display — strips the `tcp://`/`udp://`
  prefix before it ever reaches the label. The raw address (scheme
  included) is now also in the hay, so the scheme is still matchable even
  though it's not shown.
- **The global firewall allow list** (Settings → Firewall Allow List) is
  architecturally different from everything else indexed so far: it isn't
  part of `state.cfg` or `state.status`, it's fetched separately
  (`/api/exempt`) only when Settings is actually opened. Now cached onto
  `state.exempt` when that fetch happens, and indexed from there — which
  means, honestly, it's only searchable once Settings has been visited at
  least once this session, and (unlike everything else) a match just lands
  on Settings rather than pinpointing the row: `exemptReload`'s own async
  fetch would otherwise race a freshly-rendered Settings page's table not
  existing yet, for what's normally a three- or four-entry list. Noted
  rather than silently accepted as good enough.

Checked and confirmed *not* gaps, so they're not touched: Monitor's
live-only views (mesh peers, route table, latency, logs) stay deliberately
unindexed — same reasoning as always, it's live/computed data, not stored
text to jump to. Route metrics and QoS class numbers were considered and
left out on purpose: low-value, high-noise substring matches (a search for
"10" matching every rule with a metric or class containing "10" isn't a
useful result). Bandwidth/Shaping has no per-entity list to index beyond
the network itself (just two rate values per network), already reachable
via the network's own name and the section name.

Verified with the same kind of standalone Node harness as v327/v328 —
this time one populated instance of each newly-added field (a firewall
port, a NAT interface, a peer's overlay and endpoint, a seed's raw
scheme, and two allow-list entries) — all resolve correctly, including a
combined "tcp" search correctly surfacing all four unrelated entities that
legitimately contain it. Full Go test suite and build clean; still
entirely client-side JS.

---

## v328 — 2026-07-11

Fixed a network's own `notes` field (Networks \u2192 double-click notes to
edit \u2014 a free-form operator note, e.g. purpose or owner) missing from
global search. Asked directly ("all notes fields are searched?") rather
than reported as a symptom, so this one was found by audit instead of a
repro: grepped every `.notes`/`.Notes` reference in `ui.go` to enumerate
every entity that actually has a notes concept \u2014 Networks, Keys,
Seeds, Firewall rules, Peers, and Bans, confirmed exhaustive (nothing else
in the app has an equivalent free-text field; routes, hosts, DNS forwards,
NAT rules, and QoS rules have none) \u2014 and checked each against
`buildSearchIndex`. Five of six were already covered; only `cf.notes`, the
network's own field, had been left out of its index entry's `extraHay`
when that entry was first written.

One-line fix: `cf.notes` added to the network entry's search text
alongside its id and subnets. Verified with the same kind of standalone
Node harness as v327, this time with one populated notes field per entity
type (network, key, seed, firewall rule, peer, disabled peer, ban) and one
search term per field \u2014 all seven now resolve to the right result.
Full Go test suite and build clean; still entirely client-side JS.

---

## v327 — 2026-07-11

Fixed global search not finding individual peers or bans, and silently
mis-indexing disabled peers — reported live right after v326.

Root cause was more fundamental than a missing field: `buildSearchIndex`
only ever walked `state.cfg`, but peers and bans aren't config — they're
live per-network state, read from `state.status` by `secPeers`/`secBans`/
`peerRowsForNet` and never from `cf` at all. Connected peers and bans were
simply never indexed, full stop. Disabled peers *were* attempted (via
`cf.disabled_peers`), but with a wrong shape assumption — a disabled-peer
entry is an object (`NodeID`, `Hostname`, `Notes`), not a bare id string,
and passing that straight to `label: String(...)` silently produced
`"[object Object]"` as the search text. It shipped without either bug
being caught because the harness used to verify v325/v326 tested with an
empty `disabled_peers: []` and never exercised connected peers or bans at
all — a gap in the test, not just the code.

Fix, in `buildSearchIndex` (`internal/webadmin/ui.go`): peers and bans are
now indexed from `state.status`, one pass per network alongside the
existing `cf` loop. Peers reuse `peerRowsForNet(n)` directly rather than
re-deriving `NodeID`/`Hostname`/`Notes` field access a second time —
that re-derivation is exactly how the disabled-peers shape bug happened in
the first place, and `peerRowsForNet` is already the one place
`secPeers`/`infoMeshPeers` get this right. The self row is excluded (not a
useful search target). Bans didn't have a stable per-row attribute to
navigate to afterward — `secBans`'s `<tr>` carried no `data-target` the way
every other section's rows do — so one was added there for precise
navigation, matching `searchRowSelector`'s new `ban` case.

Verified with a standalone Node harness against the real (extracted, not
reimplemented) `buildSearchIndex`/`searchIndexQuery`, `peerRowsForNet`, and
`nameOf` — this time with actual `state.status` data: a connected peer, a
disabled peer (object-shaped, the exact case that broke before), and a
ban, each matched by name, hostname, notes, and (for the ban) target node
id. Also re-confirmed seeds, which were never actually broken (the report
grouped them with peers/bans, but seeds read from `state.cfg` — the path
that was already correct) — a seed's notes text matched as expected. Full
Go test suite and build clean; still entirely client-side JS.

---

## v326 — 2026-07-11

Fixed v325's global search returning nothing for terms like "peer",
"traffic", "seed", or "qos" — reported live immediately after v325 shipped.

Not a code bug so much as a scope gap: `buildSearchIndex` only ever indexed
config *data* (route CIDRs, host names, rule fields, and so on), never the
navigation labels themselves — section names ("Peers", "Seeds", "QoS") and
nav-group names ("traffic"). Those words generally don't appear inside
actual data (a QoS rule's indexed text is its protocol/port, not the word
"qos"), so searching for them found nothing even on a fully-populated
config, which is exactly what made this easy to miss before shipping: it's
invisible until someone tries typing a section name instead of a value,
which is one of the first things anyone would reasonably try in a global
search box.

Fix, in `buildSearchIndex` (`internal/webadmin/ui.go`): every `NAV_GROUPS`
section is now indexed once (label + tip text, so e.g. searching "traffic"
also surfaces Firewall and QoS, both of which mention it in their tip), every
group name is indexed once, and Settings is indexed too. `navigateToSearchResult`
handles the two new match kinds: a section hit switches straight to that
section (Settings specifically renders without a reload, matching the
existing Settings rail link's own behavior); a group hit expands that rail
group and lands on its first section, the same outcome `setActiveRailTab`
already produces for an ordinary click into a collapsed group. Neither has
a specific row to scroll to or flash — landing on the section itself,
already scrolled to top, is the whole result.

Verified with a standalone Node harness against the real (extracted, not
reimplemented) `buildSearchIndex`/`searchIndexQuery` — a mock `state.cfg`
plus all four previously-failing terms, confirming each now resolves (e.g.
"qos" \u2192 the QoS section; "traffic" \u2192 the traffic group, Firewall,
and QoS) while the existing data-search results (a route CIDR, a host
name, a firewall rule) are unchanged. Full Go test suite and build clean;
this remains entirely client-side JS/CSS, no Go-side changes.

---

## v325 — 2026-07-11

Added a global search box to the web admin header, to the left of the node
dropdown (`internal/webadmin/ui.go`). Searches every config entity with
meaningful text — network names/ids/subnets, keys (label + notes), seeds
(address + notes), routes and rejected routes, hosts and rejected hosts
(name + ip), DNS forwards and rejected domains (+ servers), firewall/NAT/QoS
rules, and locally-disabled peers — and shows matches as a dropdown;
clicking (or arrow-keying to one and hitting Enter) switches to the section
and network the result came from, scrolls to the specific row, and gives it
a brief highlight flash. Deliberately doesn't index live-only data (peer
connection state, metrics, logs) — that's not stored text to jump to, and
already has its own Monitor pages.

Reuses the `.search-select`/`.ss-*` component classes already sitting in
the stylesheet unused (`.ss-input`, `.ss-list`/`.ss-list.show`, `.ss-opt`/
`.ss-opt.sel`, `.ss-empty`) rather than inventing a second dropdown pattern
— arrow keys move the selection and Enter picks it, the interaction that
styling already implies. `state.cfg` (the full per-network config already
held client-side for rendering every section) is walked fresh into a flat
index on every keystroke rather than kept incrementally in sync — well
under a millisecond even at a few hundred entries, and there's no separate
cache to invalidate as config changes.

Placement: inserted into the header's existing `.cluster` flex container,
ahead of the node filter/dropdown, with its own `margin-right:14px` (on
top of `.cluster`'s existing `gap:8px`) so it reads as visually distinct
from the tightly-paired node filter and dropdown next to it rather than
looking like a third item in that same group.

Navigation reuses `setActiveRailTab` + `refresh()` — the same path an
ordinary rail-tab click takes — so the active tab and expanded rail group
stay consistent with a manual click getting you there. Finding the specific
row afterward needed one real special case: every section renders one
`.card` per network (each with a `.net-id` span identifying it) *except*
Networks, which is a single shared table with one row per network
(`secNetworks`) — the standard "find the card whose net-id matches" lookup
would silently break there, since it'd compare a target network id against
whichever network's row happens to render first in the one card that
exists, not the actual match. Networks results are resolved directly via
`tr.netrow[data-netid=...]` instead.

Full test suite, `go vet` clean. No Go-side changes; this is entirely
client-side JS/CSS inside `ui.go`'s embedded admin page.

---

## v324 — 2026-07-11

Fixed full-tunnel silently requiring a full process restart to recover
after switching Wi-Fi networks — reported live, traced end-to-end rather
than guessed at.

Root cause was in `demotePhysicalDefaultRoute` (`internal/mesh/
fulltunnel.go`), part of v315–v322's default-route demotion work. To make
gravinet's own full-tunnel default route win on every platform, it
deprioritizes the pre-existing physical default route once, guarded by
"skip if anything is already recorded for this prefix" — added specifically
so a resume-from-sleep replay (`reassertOSState` re-driving `syncRoute` for
every prefix) wouldn't re-demote an already-demoted route and clobber the
real original metric it needs to restore later. That guard conflated two
different situations: a true resume, where the physical route is the same
one, just possibly stripped and re-added — and a genuine network change
(Wi-Fi roam, new DHCP lease, cellular failover), where the physical default
route is a *different* route entirely. On a network change, the guard saw
a stale record and skipped forever, leaving a brand-new, fully undemoted
physical route to silently compete with the mesh's own default — and
nothing short of a restart ever cleared that record, since a plain Wi-Fi
roam doesn't touch the tun interface itself and so never passes through
`reconcileDataplane`'s own rebuild path.

Fix: `demotePhysicalDefaultRoute` now checks the *live* routing table
(`defaultGatewayFn`, the same read path `physicalGateway` already uses)
before deciding anything — it only skips when the physical route it finds
is already sitting at `demotedDefaultMetric`, which is true for a resume
replay and false for a changed network. A lookup failure also skips rather
than guessing, deferring to the next trigger instead of risking recording
an already-demoted metric as if it were the original. While in there,
also fixed the second half of the same bug: `physicalGateway`'s
per-family cache (used to route peer-bypass host routes around the
tunnel) is now invalidated right before a real re-demotion, instead of
only on full deactivate/reactivate — its own doc comment already named
"a roam to different Wi-Fi" as the known gap this leaves, unaddressed
until now.

Neither fix does anything without a trigger, though: a Wi-Fi roam that
doesn't disturb the tun interface itself never reaches `reconcileDataplane`
or `reassertOSState` on its own. `checkUnderlayChange` (`internal/mesh/
pmtu.go`) already detects exactly this — a changed local underlay source
address, the same signal that triggers path-MTU rediscovery, explicitly
documented as firing on "the user switched Wi-Fi networks or failed over to
cellular" — so it now also fans out `reassertOSState` to every network on
a detected change, not just `resetAllPMTU`. Safe to call unconditionally:
`reassertOSState` is a no-op for a network with nothing to fix, and the
live-check above means calling it more often no longer risks the state
corruption the old presence-only guard was written to prevent.

Two existing tests needed updating, not just new coverage: `withFakeDemotion`
(`route_demotion_test.go`) fed `defaultGatewayFn` a frozen metric that never
reflected a demotion having happened, which would have made every call look
like a permanent no-op now that `demotePhysicalDefaultRoute` actually reads
it — it's fixed to track the same mutable "current physical metric" state
`demoteDefaultRouteFn`'s fake already did, so the two behave like the one
routing table they're standing in for. New
`TestDemotePhysicalDefaultRouteRedemotesAfterNetworkChange` is the actual
regression test: demotes once, swaps in a fake representing a genuinely
different physical route (new address, new undemoted metric), confirms a
second demotion happens (not skipped), confirms the recorded "original
metric" updates to the new route's real value rather than keeping the stale
one, and confirms the `physicalGW` cache entry gets refreshed rather than
left pointing at a gateway that's gone. `TestDemotePhysicalDefaultRouteSkipsIfAlreadyRecorded`,
the test that encoded the original (incomplete) guard behavior, still
passes unchanged — a true resume replay still correctly does nothing.

Full test suite, `go vet`, and `-race` all clean.

---

## v323 — 2026-07-10

Made every live-apply state toggle in the web admin fire-and-forget instead
of waiting on the round trip before updating: firewall/NAT/QoS rules and
their per-network enable switches (also covers Bandwidth, which shares
`netCardHead`), routes and route-reject entries, hosts and host-reject
entries, DNS forwards and dns-reject entries, network enable/disable, peer
enable/disable, key enable/disable, key "distributed", and Managed/Manager
mode — 13 call sites in `internal/webadmin/ui.go`.

Prompted by a report that toggling looks instant on some platforms and
takes up to ~30s on others. That gap is real backend cost, not a client
sync/async artifact — `internal/netfilter/netfilter_windows.go` shells out
to PowerShell to drive WinNAT for kernel NAT, where Linux's `nft`/netlink
calls are near-instant, and network add/remove pays a similar Windows-only
cost tearing down routes/interfaces (already backgrounded server-side in
`reloadFn`, see its comment in `cmd/gravinet/main.go`). Making the client
async doesn't shrink that cost, it just stops the UI from waiting on it:
the tag/checkbox now flips in the DOM immediately and the API call goes out
in the background via the new `toggleTagState` helper (plain inline
patches for the handful of toggles, like network and peer, whose markup
doesn't fit that helper's shape); a background `refresh()` reconciles once
the request actually settles.

Deliberate trade for all 13: no more revert-on-failure or blocking
`alert()` — a failed toggle is logged to the console and left as the user
set it, and the old `dataset.busy` re-entrancy guard is gone, so rapid
repeated toggling before a slow request settles can race server-side in a
way the previous await-and-lock pattern didn't allow. Accepted explicitly
for this change rather than defaulting to it; see v264/v265 above for a
previous case where async timing on these exact toggles caused a real bug,
which is the standing reason this trade doesn't get made by default.

Remote shell and Geo-IP were left on their existing path (`edit(...,
true)`, which restarts the daemon via `quietRestart` and polls for it
coming back) rather than converted — restarting the whole process isn't
the class of platform-dependent live-apply latency this was chasing, and
firing that one without waiting on it risks the browser trying to talk to
a daemon that's mid-restart.

Verified by building all platform targets, syntax-checking the extracted
`<script>` block with `node --check`, and running the full Go test suite
(`internal/webadmin`'s handler tests all still pass unchanged, since none
of this touched the Go side of the API contract).

---

## v322 — 2026-07-10

Fixed macOS's `DemoteDefaultRoute`, which had exactly the two bugs v320
and v319 already fixed on FreeBSD and OpenBSD — flagged as a known,
likely-affected gap in v320's own changelog entry rather than fixed
sight-unseen at the time; a field report on real macOS hardware has now
confirmed it and this closes it. `netstat -rn` after enabling full-tunnel
showed the physical default route (`UGScg`, via `en1`) completely
unchanged, no second `default` line for `utun0` anywhere in the table —
the identical symptom, and root cause, as v319's original FreeBSD report.

Both bugs, both already fixed elsewhere, just not here yet:

1. `DemoteDefaultRoute` used an in-place `RTM_CHANGE` to the physical
   route's `rmx.hopcount`, which never frees the `0.0.0.0/0` (or `::/0`)
   destination/mask key — macOS's routing table, like FreeBSD's and
   OpenBSD's, holds exactly one route per destination, no Linux-style
   multi-priority coexistence. Fixed the same way as v320: `DemoteDefaultRoute`
   now actually removes the physical route via `RTM_DELETE`, stashing its
   gateway/interface/metric in a package-level map (`darwinDemoteState`)
   keyed by address family, and a later call for the same family — the
   eventual restore — re-adds it from that stashed state instead of
   re-querying a table that, by then, doesn't have it anymore.
2. `darwinSendRouteMsg` hardcoded `RTF_HOST` into every message
   unconditionally — correct for its original sole callers
   (`AddGatewayRoute`/`DelGatewayRoute`, always a `/32` or `/128` peer-bypass
   route), wrong for `DemoteDefaultRoute`'s new `/0` delete/restore calls,
   which are network routes. Fixed the same way as v320:
   `darwinIsHostPrefix` gates the flag now, set only when the prefix
   actually is a host route.

The `internal/mesh`-layer `physicalGateway` caching fix from v321 (bypass
routes silently failing for any peer/seed that connects after the physical
route is actually demoted) needed no macOS-specific work at all — it's
implemented once, platform-agnostically, in `fulltunnel.go`/`engine.go`,
and already covers every `routeDemotionNeeded` platform including this
one. Confirmed by running the same regression tests
(`TestPhysicalGatewayCachedAcrossDemotion`,
`TestPhysicalGatewayCacheClearedOnWithdrawal`) unchanged — both already
pass, since they exercise the mesh-layer cache through the same faked
`demoteDefaultRouteFn`/`defaultGatewayFn` seam every platform shares, not
anything darwin-specific.

Also fixed in passing, same as v320's OpenBSD cleanup: a pre-existing
`gofmt` violation in `darwinRtMetrics`'s field alignment, unrelated to the
routing bug but caught while already in the file.

Full test suite passes; full cross-compile matrix and `go vet` clean for
`darwin/{amd64,arm64}` specifically, in addition to the full matrix this
version already re-checks every time. No new test added against a real
macOS kernel, for the same reason already noted for FreeBSD/OpenBSD: none
available in this environment. The mesh-layer regression tests already
added in v321 are the real coverage here, since the actual bug pattern
(a flag or mechanism silently changing what a delete matches, or a cache
gap once a table stops answering) lives one layer below what those tests
exercise and can only really be confirmed against a live kernel — which is
exactly how both rounds of this bug reached the field before being caught.

---

## v321 — 2026-07-10

Fixed another regression from the same v319→v320 FreeBSD/OpenBSD demotion
work, caught from a second field report on the same test box: this time
the default route took correctly (v320's fix), but *none* of the peer or
seed bypass host routes did, breaking connectivity outright — full-tunnel
capturing all traffic, including the mesh's own encrypted underlay
packets, with no escape hatch for them.

Root cause: `physicalGateway` (`fulltunnel.go`), the lookup every
`acquireBypassRoute` call uses to find "the real gateway to route a bypass
host route through," re-resolves from the live OS routing table on every
single call — it always has, on every platform. That was harmless when
`demotePhysicalDefaultRoute` only reprioritized the physical route in
place (through v319) or, on Linux/Windows, still leaves it genuinely
present in the table today. But as of v320, FreeBSD and OpenBSD actually
*remove* the physical default route from the table (`RTM_DELETE`, freeing
the `0.0.0.0/0` key for gravinet's own route — see v319/v320's own
entries for why that's necessary there). Once that's happened, a live
lookup excluding this network's own tun finds nothing at all: the
physical route genuinely isn't there anymore. `demotePhysicalDefaultRoute`
itself runs once, early, and never calls `physicalGateway` again — so this
was invisible in that function. Every *later* `acquireBypassRoute` call
did call it, though, on every new or reconnecting peer session and every
seed dial from that point on, and every one of them failed, logged only
at `Debugf` (`"mesh: full-tunnel bypass route for %s on net %x: %v"`) —
easy to miss entirely unless watching debug-level logs at the right
moment, which is exactly how this got past v320's own testing.

Fix: `physicalGateway` now caches its result per address family in a new
`ns.physicalGW` map (guarded by the existing `osMu`), populated on first
successful resolution and reused for the remainder of one full-tunnel
activation instead of re-querying a table that, on these two platforms,
stops having an answer partway through. `demotePhysicalDefaultRoute` also
now explicitly warms this cache immediately before it removes the
physical route — not just relying on `resyncAllBypassRoutes`/
`syncSeedBypassRoutes` (which run first in `syncFullTunnelRoute` and
normally populate it as a side effect of their own work) to have already
done so, since a network with no live peer or seed at the exact instant
full-tunnel activates would otherwise leave the cache empty right up to
the point the physical route disappears. `restorePhysicalDefaultRoute`
clears the cached entry for that family on withdrawal, so a later
reactivation resolves fresh rather than reusing a gateway from however
long ago full-tunnel was last on — the deliberate trade-off this
introduces: a physical gateway that changes (roaming to different Wi-Fi, a
new DHCP lease) while full-tunnel stays *continuously* active won't be
picked up until the next deactivate/reactivate cycle, since there's no
longer a live table to notice the change against. Judged acceptable
against the alternative of bypass routes not working at all.

New regression coverage, `physicalgw_cache_test.go`:
`TestPhysicalGatewayCachedAcrossDemotion` fakes `defaultGatewayFn` to fail
once demotion has "happened" (exactly what a real FreeBSD/OpenBSD kernel
does) and confirms a bypass-route acquisition for a brand new address
*after* that point still succeeds — deliberately via a network with no
live peer or seed at activation time, so this exercises
`demotePhysicalDefaultRoute`'s own cache warm-up specifically, not just
the incidental populate-as-a-side-effect path. Verified this test actually
catches the regression: reverting `physicalGateway` to its pre-fix,
uncached form fails it exactly as described in the field report
(`"expected a bypass route for 198.51.100.7 ... got calls []"`), confirmed
by temporarily reverting and rerunning before restoring the fix.
`TestPhysicalGatewayCacheClearedOnWithdrawal` confirms the cache doesn't
outlive one activation: a second activation with a different fake gateway
address picks up the new one, not the first activation's cached value.

None of this was — or could have been — caught by the existing
`internal/mesh` test suite's mocked `defaultGatewayFn`
(`withFakeGateway`), which always succeeds unconditionally regardless of
how many times or in what order it's called; the bug was specifically in
what happens when a *second* call to the live table doesn't return what
the first one did, a distinction a fixed always-succeeds fake can't
represent. Both new tests fake `defaultGatewayFn` to actually change
behavior across calls instead.

Full test suite passes (`internal/mesh` included); full cross-compile
matrix and `go vet` clean for `freebsd`/`openbsd` on both `amd64` and
`arm64`. Noted in passing, unrelated to this fix and confirmed pre-existing
on the original v318 tree as well: `TestKeyDisableReconnects`
(`keyretire_test.go`) is flaky under `-race` (roughly 50% fail rate in
repeated local runs, timing-related, nothing to do with routing) — logged
here rather than silently worked around, since it isn't this version's bug
to fix.

---

## v320 — 2026-07-10

Fixed a real regression in v319's own fix, caught from a field report: a
live `netstat -rn` after injecting and accepting a full-tunnel default
route on FreeBSD showed the physical default route completely unchanged —

```
default            192.168.5.1        UGS          vtnet0
```

— and no second `default` line for `mesh0` anywhere in the table. v319's
fix hadn't taken effect at all; the physical default route was never
displaced, so the mesh's own literal `0.0.0.0/0` was never installed
either.

Root cause: `sendRouteMsg` (the shared FreeBSD routing-socket plumbing
behind every route add/delete this package does) hardcodes `RTF_HOST` into
*every* message it builds, unconditionally. That was correct for its only
caller before v319 — `AddGatewayRoute`/`DelGatewayRoute`, which install
peer-bypass routes and always pass a `/32` or `/128` prefix, genuinely a
host route. v319's `DemoteDefaultRoute` rewrite reused this same function
to delete (and later restore) the *physical* default route — `0.0.0.0/0`,
a network route, not a host route — and inherited `RTF_HOST` along with
everything else `sendRouteMsg` does. Setting `RTF_HOST` on that `RTM_DELETE`
made the FreeBSD kernel perform a host-route lookup instead of honoring
the network netmask actually supplied in the message, so it never matched
the real default route in the table. The kernel returned `ESRCH` for "no
such route" — which `sendRouteMsg`'s own delete-is-idempotent handling
(there specifically for the legitimate case of deleting an
already-withdrawn route) swallowed as success, exactly the same shape of
bug v318 fixed on Linux (a mismatched selector field making a delete
silently no-op) and, in hindsight, exactly what `RouteDemotionNeeded`'s own
doc comment should have made this version double-check rather than assume
away by analogy with the working `/32` case.

`DemoteDefaultRoute` believed the physical route was gone and proceeded to
call the tun device's own `AddRoute` (`route add -net 0.0.0.0/0 -interface
tunN`), which then collided with the still-present physical default and
failed with `EEXIST` — logged only as a `Warnf` by `syncFullTunnelRoute`'s
existing not-fatal handling, never surfaced anywhere an operator would see
it without watching the logs at the right moment. Net effect: full-tunnel
"activated" (`fullTunnel` flipped true, bypass routes installed) with
nothing actually different in the OS routing table — the worst version of
this failure, since the mesh believed it had won.

Fix: `sendRouteMsg` (and OpenBSD's `obsdSendRouteMsg`, which has the
identical hardcoded flag and would fail the identical way once actually
exercised against a live kernel) now sets `RTF_HOST` only when the prefix
being operated on is actually a full-length host route
(`isHostPrefix`/`obsdIsHostPrefix`: `p.Bits() == p.Addr().BitLen()`), not
unconditionally. `AddGatewayRoute`/`DelGatewayRoute`'s existing `/32`/`/128`
bypass-route callers are unaffected (still get `RTF_HOST`, correctly);
`DemoteDefaultRoute`'s `/0` delete and restore now don't, matching the
`UGS`-not-`UGHS` flags a real default route actually carries (visible in
the same `netstat -rn` output above).

No test added against a real kernel for the same reason v319 already
flagged: neither a FreeBSD nor an OpenBSD host is available in this
environment, so this class of bug — a flag or selector field silently
changing what a delete matches — has now surfaced twice (v318 on Linux,
this on FreeBSD) specifically *because* it only manifests against a real
routing table, not against the mocked `demoteDefaultRouteFn` the
`internal/mesh` test suite exercises. That suite's coverage of the
call-contract (when demotion is triggered, what it's excluded, what gets
recorded for restore) is real and stays green here — this was never a gap
that layer could have caught, since the bug was entirely inside what a
"successful" `RTM_DELETE` actually accomplished on the wire.

Full cross-compile matrix clean, `go vet`/`gofmt` clean, full `go test
./...` passes.

---

## v319 — 2026-07-10

Fixed: mesh default-route injection ("full-tunnel") never actually took
over the default route on FreeBSD or OpenBSD — the physical default route
was always the one left standing, on both platforms, regardless of what
`ip route`'s BSD-family equivalent (`netstat -rn`) showed for
`demotedDefaultMetric` — matching a field report of exactly that symptom.

Two separate bugs, one per platform, both in the same place:
`DemoteDefaultRoute` in `gateway_freebsd.go` / `gateway_openbsd.go`.

**OpenBSD didn't compile at all.** `obsdChangeRouteMetric` (the RTM_CHANGE
helper `DemoteDefaultRoute` called) referenced `obsdRtmChange` and
`obsdRtvHopcount` — neither ever defined anywhere in the file. `GOOS=openbsd
go build ./...` failed outright:

```
internal/tun/gateway_openbsd.go:34:2: "sync" imported and not used
internal/tun/gateway_openbsd.go:405:12: undefined: obsdRtmChange
internal/tun/gateway_openbsd.go:412:12: undefined: obsdRtvHopcount
```

So this wasn't "full-tunnel silently doesn't win" on OpenBSD, it was
"gravinet doesn't ship an OpenBSD binary at all" — `scripts/build-release.sh`
targets `openbsd/amd64` and `openbsd/arm64` directly, so this would have
failed the release build the moment anyone actually ran it rather than
trusting the last cross-compile.

**FreeBSD compiled fine, but the fix it compiled was never going to work.**
`DemoteDefaultRoute`'s actual strategy — an in-place `RTM_CHANGE` to the
physical default route's `rmx.hopcount`, leaving its destination/mask
untouched — never frees the `0.0.0.0/0` (or `::/0`) key in the routing
table. Unlike Linux's FIB, FreeBSD's (and OpenBSD's) holds exactly one
route per destination/mask; there's no multi-priority coexistence to fall
back into. So the very next step, `syncFullTunnelRoute`'s call to
`ns.dev().AddRoute` — which on these platforms shells out to
`route add -net 0.0.0.0/0 -interface tunN` — collided with the
physical route still sitting at that exact key and failed with `route:
writing to routing socket: File exists`, silently downgraded to a `Warnf`
by `syncFullTunnelRoute`'s existing not-fatal handling
(`internal/mesh/routes.go`). Nothing in the operator-visible routing table
ever changed.

Root cause: the doc comments on both platforms already half-diagnosed
this — OpenBSD's own `RouteDemotionNeeded` comment states plainly "The
physical route has to actually be removed first, not just deprioritized
in place," and directly says the in-place `RTM_CHANGE` approach "didn't
actually work." The comment was right; the code beneath it just never
caught up to match — an edit that updated the reasoning without updating
the mechanism, left in a state that doesn't compile as a result.

Fix, identical in shape on both platforms: `DemoteDefaultRoute` now
actually removes the physical default route via `RTM_DELETE`, freeing the
key for gravinet's own `AddRoute` to claim, and stashes the exact
gateway/interface/metric it removed in a small package-level map keyed by
address family (`demoteState` / `obsdDemoteState`) — there's no way to
recover that from the routing table once the route is gone, so it has to
be captured before the delete goes out. `routes.go` already calls this
same exported function a second time, later, to restore
(`restorePhysicalDefaultRoute` passes the metric the first call returned
back in as `newMetric`); that second call now recognizes the pending
stashed entry and re-adds the saved route via `RTM_ADD` instead of trying
to look up a physical default that, by design, isn't there anymore. No
change to the exported signature or to `internal/mesh`'s side of the
contract — every existing mesh-level test
(`route_demotion_test.go`, fully mocked at `demoteDefaultRouteFn`) passes
unchanged.

Also fixed in passing, in the same file: a pre-existing `gofmt` violation
in `obsdRtMetrics`'s field alignment (unrelated to the routing bug, but
`scripts/build-all.sh`'s `gofmt -l` gate would have failed the release
build on it regardless of the fix above).

**Darwin note, left deliberately unfixed here:** `gateway_darwin.go`'s
`DemoteDefaultRoute` still uses the identical in-place `RTM_CHANGE`
approach this version replaces on FreeBSD/OpenBSD, and macOS's routing
table has the same one-route-per-destination shape — so it's very likely
the same bug, just not the one reported or asked about this round. Flagging
it explicitly rather than silently leaving it, in the spirit of v318's own
"known gap" note: worth its own report and fix, not bundled into this one
sight-unseen.

Testing: full cross-compile matrix (`linux/{amd64,arm64}`,
`freebsd/amd64`, `openbsd/{amd64,arm64}`, `darwin/{amd64,arm64}`,
`windows/amd64`) clean, including OpenBSD for the first time this
diagnostic pass actually checked it. `go vet` clean for
`freebsd/{amd64,arm64}` and `openbsd/{amd64,arm64}`. `gofmt -l` clean.
Full `go test ./...` passes, `internal/mesh` included. As with v318's own
caveat for the non-Linux platforms: the real `RTM_ADD`/`RTM_DELETE`
syscalls in `gateway_freebsd.go`/`gateway_openbsd.go` remain structurally
verified and cross-compiled only, not run against a real FreeBSD or
OpenBSD kernel in this environment — neither is available here, same
caveat v316 first flagged.

---

## v318 — 2026-07-10

Fixed a real bug in v317's Linux default-route demotion, caught from a live
`ip route` an operator ran after activating full-tunnel:

```
default dev mesh0 scope link
default via 192.168.193.1 dev wlan0 proto dhcp src 192.168.193.26 metric 600
default via 192.168.193.1 dev wlan0 metric 1000
```

Three default routes where there should have been two. Routing still
worked — `mesh0`'s route has the lowest metric (unset/0), so it still won —
but `DemoteDefaultRoute`'s delete of the original physical default route
(the `proto dhcp` one, metric 600) silently did nothing: a new demoted-metric
copy got added (the `metric 1000` line, correctly) but the original was
never actually removed, left sitting alongside it.

Root cause, once traced through `route_linux.go`'s `sendRouteMsg`
(the shared rtnetlink plumbing behind every route add/delete this package
does, including v317's new demotion delete): it hardcoded `rtm_protocol`
(`RTPROT_BOOT`) into *every* request, add or delete. The Linux kernel's
route-delete matching (`fib_table_delete`, both v4 and v6) treats a nonzero
`rtm_protocol` as an exact-match selector — delete only a route installed
under that specific protocol — and this hardcoding made every delete
request effectively say "only delete this if it was installed by
`RTPROT_BOOT`." That was invisible for fifteen versions of Linux gateway
work (v311 onward) because every route gravinet itself deletes, it also
added — always with `RTPROT_BOOT` on both ends, so add/delete stayed
symmetric by construction, and the field never had to matter. v317's
`DemoteDefaultRoute` is the first caller that deletes a route gravinet
*didn't* install: the pre-existing physical default, added by a DHCP
client (`RTPROT_DHCP`, in the field report), NetworkManager, systemd-
networkd, or a static config, essentially never `RTPROT_BOOT`. The mismatch
made the kernel return `ESRCH` ("no route matches"), which
`sendRouteMsg`'s own delete-is-idempotent handling — there specifically for
the legitimate case of deleting an already-withdrawn route — swallowed as
success, masking the real failure underneath.

Fix: `sendRouteMsg` now only sets `rtm_protocol` on an add/replace; a
delete leaves it at `RTPROT_UNSPEC` (0), which the kernel's own matching
logic (`!cfg->fc_protocol || fi->fib_protocol == cfg->fc_protocol`) treats
as "match any protocol." Every other field the delete still specifies exactly
(destination, table, scope, type, gateway, interface, and — critically,
already correct before this fix — the metric/priority) is enough on its own
to identify the one specific route being removed; protocol was the one
field this codebase happened to over-specify. Applies to every caller of
`sendRouteMsg`, not just `DemoteDefaultRoute` — strictly a widening of what
a delete matches, so gravinet's own routes (still always `RTPROT_BOOT` on
both add and delete) are unaffected.

New real-syscall regression test, `TestDelGatewayRouteMatchesRegardlessOfProtocol`
(`gateway_linux_test.go`, root-gated like this file's existing
`TestAddDelGatewayRouteLinux`): installs a throwaway TEST-NET-3 host route
with an explicit foreign protocol (`RTPROT_STATIC`, standing in for "not
gravinet"), confirms `DelGatewayRoute` now actually removes it, and checks
`/proc/net/route` to prove it. Run for real in this environment (root/
`CAP_NET_ADMIN` available here) against a throwaway destination — never
against this machine's own real default route, which every full-tunnel
test in `internal/mesh` continues to avoid touching at all, per v317's
`withFakeGateway` fix. `/proc/net/route` diffed clean before/after the
full suite, same as every version since v317.

This closes the "known gap" v317 flagged honestly rather than papering
over — `DemoteDefaultRoute` on Linux now does have a real-kernel test, just
not the way v317's note anticipated (no network-namespace infrastructure
turned out to be necessary; a differently-configured throwaway route was
enough to isolate the actual bug). FreeBSD/OpenBSD/Darwin/Windows'
`DemoteDefaultRoute` implementations don't share this specific bug — none
of them filter a delete by anything resembling `rtm_protocol` — but this is
a reminder that "reuses an already-tested code path" (what v316/v317 both
leaned on for confidence) isn't the same guarantee as "tested for the
specific new thing being asked of it." The other four platforms' `RTM_CHANGE`/
`SetIpForwardEntry2` calls remain structurally verified and cross-compiled
only, not run against real hardware, per v316's original caveat.

Full test suite, `go vet`, and `-race` all clean. Cross-compiles clean on
every platform gravinet ships for.

---

## v317 — 2026-07-10

Extended v316's default-route demotion to Linux too, at the operator's
request, purely for consistency — Linux's kernel never actually needed it
(it already keeps two default routes at different metrics and prefers the
lower one, which is exactly what let `syncFullTunnelRoute` get away with
installing gravinet's own default route "alongside" the physical one,
undemoted, through v316), but as of this version every platform gravinet
has a real gateway backend for goes through the identical demote-then-install
sequence, Linux included, rather than Linux being the one platform that
skips a step the other four can't.

`gateway_linux.go`: `RouteDemotionNeeded` flips from `false` to `true`, and
`DemoteDefaultRoute` goes from a no-op stub to a real implementation.
Mechanically this is the one platform in the whole `DemoteDefaultRoute`
family that *isn't* a single in-place "change" operation: FreeBSD/OpenBSD/
Darwin have `RTM_CHANGE` and Windows has `SetIpForwardEntry2`, but on
Linux a route's kernel identity includes its metric (`RTA_PRIORITY`) as
part of the key — the same fact `route_linux.go`'s existing `DelRoute` doc
comment already spells out for gravinet's own routes — so
`RTM_NEWROUTE|NLM_F_REPLACE` at a different metric doesn't update the
existing entry, it adds a second one alongside it. `DemoteDefaultRoute`
therefore does add-the-demoted-copy, then delete-the-original, deliberately
in that order (not delete-then-add) so a failure or crash between the two
steps can never leave the host with zero physical default routes, even for
an instant — at every point either the original, the demoted copy, or both
are present, never neither. Reuses `dumpDefaultRoutes` (the same read path
`DefaultGateway` already uses and this package's own tests already
exercise against a real kernel table) for route selection, and
`sendRouteMsg` (the same write path `AddRoute`/`AddGatewayRoute` already
use and `TestAddDelGatewayRouteLinux` already exercises for real) for both
the add and the delete — no new syscall plumbing, just a new sequencing of
existing, already-tested pieces.

`internal/mesh`: no code changes needed — `demotePhysicalDefaultRoute`/
`restorePhysicalDefaultRoute` (fulltunnel.go) and `syncFullTunnelRoute`
(routes.go) were already written platform-generically against
`routeDemotionNeeded`/`demoteDefaultRouteFn`, so flipping Linux's constant
was sufficient to bring it in line. Doc comments across both files (plus
the `demotedGatewayMetric` field in `engine.go`) updated throughout to stop
describing Linux as the platform that skips this step, since it no longer
does.

**Test-safety note, not a functional change:** this surfaced one real risk
worth recording. `internal/mesh`'s existing test suite has several tests
that spin up real `Engine`s and exercise full-tunnel activation without
faking the platform gateway layer (`withFakeGateway`, `fulltunnel_test.go`)
— harmless through v316, since an unfaked `demotePhysicalDefaultRoute` was
a guaranteed no-op on Linux (`routeDemotionNeeded` false) regardless of
whatever real default route the test machine happened to have. With Linux
now demoting for real, `TestFullTunnelRouteEndToEnd`
(`fulltunnel_route_test.go`) would otherwise have reached out via genuine
rtnetlink calls and reprogrammed the *actual* physical default route of
whatever machine runs this test suite. Fixed by adding a `withFakeGateway`
call to that one test (every other full-tunnel test already had one) —
audited every test in the package for the same gap first (grepped for
`fullTunnel`/default-route literals across every `_test.go` file), found
this to be the only unfaked one, and confirmed after the fix that a full
run, `-race` included, leaves this environment's real routing table
byte-for-byte unchanged (`/proc/net/route` diffed before/after). `withFakeGateway`
itself now also fakes `demoteDefaultRouteFn`/`routeDemotionNeeded` by
default so this class of gap can't recur as new tests are added.

Seven new tests from v316 (`route_demotion_test.go`) already covered the
demote/restore/no-redemote/resume-guard/failure-tolerance logic through
fakes, platform-independently — nothing new needed there, and they all
still pass unchanged with Linux's real default now flipped to true.

**Known gap, stated plainly:** unlike `DefaultGateway` and
`AddGatewayRoute`/`DelGatewayRoute` (each independently exercised against
this environment's real kernel routing table by existing root-gated tests
in `gateway_linux_test.go`), `DemoteDefaultRoute` itself has no equivalent
real-kernel test here. One was deliberately not written for this version:
the only way to exercise it for real against an actual default route while
excluding this test suite's own — an isolated network namespace with a
throwaway default route of its own — is more test infrastructure than this
one-line-of-consistency change justified building right now, and testing
it against this environment's *actual* default route was ruled out outright
as an acceptable risk. Confidence here instead rests on `DemoteDefaultRoute`
being built entirely out of already-real-tested pieces
(`dumpDefaultRoutes`, `sendRouteMsg`) recombined in a new but simple
sequence, plus full test coverage of everything above it in
`internal/mesh`. Worth closing properly in a future version rather than
carried forward silently.

Full test suite, `go vet`, and `-race` all clean. Cross-compiles clean on
every platform gravinet ships for.

---

## v316 — 2026-07-10

Full-tunnel default routes now actually take effect on FreeBSD, OpenBSD,
macOS, and Windows, not just Linux. v315 got the peer-bypass host-route
backend working on all five platforms (`AddGatewayRoute`/`DelGatewayRoute`),
but that was only half the feature: the *literal default route itself*
(`syncFullTunnelRoute` in `internal/mesh/routes.go`) still just called the
ordinary `Device.AddRoute`, on the documented assumption that installing it
"alongside" the existing physical default route is enough, with the OS
preferring whichever has the lower metric — true on Linux, not true on the
other four. FreeBSD/OpenBSD/macOS's BSD routing tables hold one route per
destination/mask; a second `RTM_ADD` for `0.0.0.0/0` while one already
exists collides with it instead of coexisting at a lower priority. Windows
does support a per-route metric, but a manually low one on gravinet's own
adapter isn't reliably preferred over the physical adapter's own
automatically-computed interface metric. Practical effect before this fix:
accepting a peer's advertised default route on any of those four platforms
either failed to install at all or installed without actually taking over
outbound traffic — full-tunnel silently didn't route through the mesh.

The fix, the same shape on all four: before gravinet's own default route is
installed for the first time, deprioritize the pre-existing physical
default route first, then install as before. `internal/tun`'s
`gateway_{freebsd,darwin,openbsd,windows}.go` each gain a
`DemoteDefaultRoute(family, excludeIfIndex, newMetric) (int, error)` and a
`RouteDemotionNeeded = true` const (`gateway_linux.go` gets both too, for a
uniform symbol across every platform, but `RouteDemotionNeeded = false`
there and `DemoteDefaultRoute` is a no-op — Linux's existing coexistence
behavior needs nothing extra). Returns the route's previous metric so it
can be put back later, rather than left pinned at the demoted value forever.

**FreeBSD, macOS, OpenBSD:** `DemoteDefaultRoute` reuses the same
`sysctl(CTL_NET, PF_ROUTE, ...)` dump `DefaultGateway` already does to find
the physical route, then sends an `RTM_CHANGE` touching only its
`rmx_hopcount` field (`RTV_HOPCOUNT` in `Inits`) — chosen as this package's
stand-in "route metric" across all three BSD flavors because it's the one
`route(8)`/`rt_metrics` field every one of them actually exposes under that
name (the `-hopcount` modifier), even though none of their kernels enforce
it as a forwarding-decision input the way Linux's `RTA_PRIORITY` is. The
kernel doesn't need to honor hopcount for this to work: freeing up the
`0.0.0.0/0`/`::/0` destination-mask key for gravinet's own `AddRoute` to
succeed at all is what actually matters; recording an accurate before/after
value is just so restoring it later doesn't leave a nonsense number behind.
OpenBSD's `RTM_CHANGE` message still sets `Hdrlen` explicitly, same
requirement as its `RTM_ADD`/`RTM_DELETE` messages from v315.

**Windows:** a new `SetIpForwardEntry2` binding (`iphlpapi.dll`, same
zero-dependency `syscall.NewLazyDLL` convention as the rest of this file).
`DemoteDefaultRoute` finds the physical route via the existing
`GetIpForwardTable2` dump, mutates just its `Metric` field in place on the
row the kernel already gave back, and passes that same row straight to
`SetIpForwardEntry2` — no rebuilding a `MIB_IPFORWARD_ROW2` field-by-field
the way `buildForwardRow` does for a brand new route, so there's no chance
of dropping a field `CreateIpForwardEntry2` never needed but
`SetIpForwardEntry2`'s row-matching logic does.

**`internal/mesh`:** `fulltunnel.go` gains `demotePhysicalDefaultRoute` /
`restorePhysicalDefaultRoute`, wired into `routes.go`'s
`syncFullTunnelRoute` — demote only once, on the transition into
full-tunnel (not on every later metric update), restore once the mesh's
own default route is withdrawn again. `demotePhysicalDefaultRoute` skips
outright if a demotion is already recorded for that prefix, specifically so
`reassertOSState`'s resume-from-sleep path (which clears `ns.osMetric` and
re-drives `syncRoute` for every prefix, full-tunnel default included)
can't overwrite the real original metric with the already-demoted one.
Both functions are best-effort — a demotion or restore failure is logged,
not fatal, same as every other OS-table operation in this path already is.
New `demoteDefaultRouteFn`/`routeDemotionNeeded` package vars (mirroring
the existing `defaultGatewayFn`/`gatewaySupported` pattern) back seven new
tests in `internal/mesh/route_demotion_test.go` covering: the demote call
happens once on install with the right family/ifindex/metric and records
the prior value; a metric-only update never re-demotes; withdrawal restores
the original metric and clears the record; the resume-reassert guard holds
across two calls; a demotion failure still lets the mesh route install; and
`routeDemotionNeeded = false` (Linux) makes this whole path an inert no-op,
matching v315's existing test coverage exactly on that platform.

Full Linux test suite, `go vet`, and `-race` all still clean — nothing
about this changed Linux's own (already-working) full-tunnel behavior.
Cross-compiles clean on all four affected platforms
(`GOOS={freebsd,darwin,openbsd,windows} GOARCH={amd64,arm64}`), same honest
caveat as v315: structurally verified and cross-compiled, not run on real
hardware — there's still no FreeBSD, macOS, OpenBSD, or Windows machine
available in this environment to confirm `RTM_CHANGE`/`SetIpForwardEntry2`
actually land the way the man pages and MSDN docs say they should.

---

## v315 — 2026-07-10

Full-tunnel default routes now work on all five platforms gravinet ships
for — Windows, macOS, FreeBSD, and OpenBSD each got their own real
`DefaultGateway`/`AddGatewayRoute`/`DelGatewayRoute` (`internal/tun/
gateway_{windows,darwin,freebsd,openbsd}.go`), replacing the stub in
`gateway_unsupported.go` that's now purely a forward-compatibility
safety net for any platform gravinet doesn't build for yet. Same design
as Linux (v312-314) on every platform — a real gateway-routed host route
via each OS's own native route-table API, no platform-specific socket
tricks, no `/1` split, nothing WireGuard's own clients wouldn't have to
build too if they took this approach instead of policy routing.

**Windows** (`gateway_windows.go`): the IP Helper API's "2" route
functions — `GetIpForwardTable2`, `CreateIpForwardEntry2`,
`DeleteIpForwardEntry2` (`iphlpapi.dll`, via `syscall.NewLazyDLL`, no
`golang.org/x/sys/windows`). The `MIB_IPFORWARD_ROW2` struct layout isn't
derived from the C header by hand — it's checked directly against
WireGuard's own Windows client (`golang.zx2c4.com/wireguard/windows/
tunnel/winipcfg`), which solves this identical problem in production on a
large number of machines; matching a known-correct, battle-tested layout
beat re-deriving Win32 struct padding by reasoning alone. `Device.IfIndex()`
on Windows now resolves for real too, via `WintunGetAdapterLUID` (added to
`tun_windows.go`'s existing WinTun proc bindings) +
`ConvertInterfaceLuidToIndex`.

**FreeBSD, macOS, OpenBSD** (`gateway_freebsd.go`, `gateway_darwin.go`,
`gateway_openbsd.go`): the BSD routing-socket family — a
`sysctl(CTL_NET, PF_ROUTE, ...)` dump for reading the table (the same
mechanism `netstat -rn`/`route -n get` and `golang.org/x/net/route`'s
`FetchRIB` use; a routing-socket `RTM_GET` only returns one route, which
can't distinguish gravinet's own already-installed route from the genuine
physical one once it exists — same problem a single Linux `ip route get`
has, see `gateway_linux.go`'s doc comment), and `RTM_ADD`/`RTM_DELETE`
messages written to an `AF_ROUTE` socket for mutating it.

These three turned out to need three genuinely different struct layouts,
not one shared "BSD" implementation with minor tweaks — confirmed by
reading this Go toolchain's own bundled stdlib source
(`src/syscall/ztypes_{freebsd,darwin,openbsd}_{amd64,arm64}.go`,
`route_bsd.go`) directly rather than working from the C headers or
web-search snippets, since that source is generated straight from each
OS's real headers and is exactly what `golang.org/x/net/route` itself is
built on:

- FreeBSD's `rt_msghdr` has a 64-bit `Inits` and an `Fmask` field.
- Darwin's has a 32-bit `Inits` and a `Use` field in the same position —
  not the same struct under a different name.
- OpenBSD's has diverged furthest: it adds `Hdrlen`, `Tableid`,
  `Priority`, and `Mpls`, and drops `Use`/`Fmask` in favor of just
  `Fmask`. `Hdrlen` specifically isn't optional bookkeeping — OpenBSD's
  `route(4)` requires it set to the real header size on an outgoing
  message so the kernel knows where the header ends and the sockaddr
  array begins; FreeBSD/Darwin have no equivalent because they assume a
  fixed, well-known header size instead. Missing this would have been a
  quiet, hard-to-diagnose failure specifically on OpenBSD.
- Sockaddr alignment inside a routing-socket message also isn't uniform:
  FreeBSD and OpenBSD round to 8 bytes (native `sizeof(long)` on
  amd64/arm64), but **Darwin rounds to 4 even on a 64-bit kernel** — Go's
  own `route_bsd.go` states this outright ("Darwin kernels require 32-bit
  aligned access to routing facilities"). This is exactly the kind of
  divergence that "modern 64-bit BSD" intuition gets wrong, confirmed
  before it became a real bug rather than after.

`Device.IfIndex()` on all three resolves via Go's own stdlib
`net.InterfaceByName` (already non-cgo on these platforms, via the same
routing-socket family internally) rather than a hand-rolled
interface-list parser — one less struct layout to get right, for a part
of the problem the standard library already solves correctly.

**Honest limits of what's actually verified here:** every struct field
layout is checked line-for-line against Go's own toolchain source, and
every alignment/padding rule against `route_bsd.go`'s actual logic, not
guessed — that part is about as solid as it can be without a compiler on
the real OS. All four new platforms cross-compile clean
(`GOOS={windows,darwin,freebsd,openbsd} GOARCH={amd64,arm64}`) and
`go vet` is clean on all of them (Windows shows the same
already-accepted `unsafe.Pointer` class of vet false-positive already
present elsewhere in this codebase's Windows syscall code, nothing new).
None of this can actually run in this environment — there's no Windows,
macOS, FreeBSD, or OpenBSD machine available to execute a single line of
it, so real-hardware testing (does `CreateIpForwardEntry2` actually
install the route Windows Firewall doesn't quietly block; does the
OpenBSD kernel actually accept a message with `Hdrlen` set this way; does
`DefaultGateway` actually find the right gateway on a real multi-homed
Mac) is still genuinely open on all four, the same honest caveat carried
since v311's FreeBSD PTY fix for exactly the same reason: cross-compiled
and structurally verified is not the same claim as run and confirmed.

Full Linux test suite, `go vet`, and `-race` on the full-tunnel tests all
still clean — nothing about this touched the Linux implementation or the
mesh-layer logic above it.

---

## v314 — 2026-07-10

Reversed a design decision from v312, on a correction from the operator
testing it: full-tunnel now installs the accepted default route (0.0.0.0/0
or ::/0) *literally*, at its advertised metric, instead of splitting it into
two /1s.

The /1 split was built on the assumption that it was needed to keep the
mesh's own underlay traffic from looping into the tunnel — the actual
hazard `RouteRej`'s default has warned about since before this feature
existed. That assumption stopped being true the moment
`internal/mesh/fulltunnel.go`'s /32-per-peer bypass routes landed, earlier
in the same arc: a /32 wins longest-prefix-match against *any* less
specific route, split or literal, so the split bought nothing for
peer-connectivity safety once the bypass mechanism existed — it just wasn't
noticed at the time, because the split was written as part of the same
change that introduced the bypass routes, without stepping back to ask
whether the split was still pulling its own weight afterward.

The split's only remaining justification was a different, unrelated one:
not fighting with DHCP clients/NetworkManager/systemd-networkd, which is a
real thing wg-quick's own docs cite as their reason for it, but is an
operational trade-off for whoever runs the node to weigh, not a safety
requirement gravinet should impose. In the common case there's no fighting
to avoid in the first place: Linux keeps multiple default routes at
different metrics without conflict, and a lease renewal doing a
metric-scoped route replace never touches an entry at a different metric.
`syncFullTunnelRoute`'s doc comment (routes.go) now says this plainly
rather than asserting the split is required.

Mechanically: `syncFullTunnelRoute` drops `defaultRouteSplits` and the
per-family /1 pair entirely, installs/removes the literal prefix p via the
same `AddRoute`/`DelRoute` calls syncRoute already uses for every other
redistributed prefix, and is otherwise unchanged — the platform-support hard
guard (v313) and the ordering guarantee (bypass routes exist before the
route does, on install; route removed before bypass routes are released, on
withdrawal) both still apply exactly as before, just to one route instead of
two.

All references to the /1 split were checked for staleness, not just the
code that installed it — doc comments in fulltunnel.go and
gateway_linux.go described current behavior in terms of the split and
needed the same correction, or they'd have been actively misleading about
what the code does now.

Full test suite rewritten to assert the literal prefix is what's actually in
the OS table (including the end-to-end test, over the real wire protocol)
rather than the two /1s. `go vet`, the full 11-target build matrix, and
`-race` on the full-tunnel test suite are all clean.

---

## v313 — 2026-07-10

Found while double-checking v312's own platform boundary out loud, not from
a report: on any platform other than Linux, accepting a peer-advertised
full-tunnel default route was *worse* than doing nothing, not just
unsupported. The `/1` split (`syncFullTunnelRoute`, routes.go) is a plain
on-link route installed through the ordinary `AddRoute` — already
implemented on every platform, unrelated to any of the new gateway-route
code — so it would install just fine everywhere. Only the *protective* half,
the peer/seed bypass routes that keep that split from looping the mesh's own
traffic into itself, was gated behind the Linux-only backend
(`gateway_unsupported.go`'s stubs) — and that half failed silently, a
Debug-level log line, nothing louder. So on Windows/macOS/FreeBSD/OpenBSD, an
operator who removed the `RouteRej` default and had a peer advertise
`0.0.0.0/0` would get the dangerous half activated with the safety net
quietly absent — reproducing the exact routing-loop hazard this whole
feature exists to prevent, just harder to notice than the naive
literal-`/0` version would have been.

Fixed by making platform support a hard, checked prerequisite rather than an
implicit one: `tun.GatewaySupported` (newly exported from both
`gateway_linux.go`, `true`, and `gateway_unsupported.go`, `false`) is
checked by `syncFullTunnelRoute` before touching anything — if the platform
can't back the bypass routes, it doesn't get the split either, full stop,
with a `[WARN]`-level log explaining why rather than a quiet no-op.
Routed through a swappable package var (`gatewaySupported` in
`internal/mesh/fulltunnel.go`, alongside the existing
`defaultGatewayFn`/`addGatewayRouteFn`/`delGatewayRouteFn` test hooks) rather
than referencing the constant directly, specifically so both branches of the
guard are actually testable from a single platform instead of only ever
exercising the "supported" path in CI and taking the "unsupported" path on
faith.

`go vet`, the full 11-target build matrix, and `-race` on the full-tunnel
test suite are all clean. One new test
(`TestSyncFullTunnelRouteRefusesWithoutGatewaySupport`) confirms the split
never installs and `ns.fullTunnel` never flips true when the platform can't
back it.

---

## v312 — 2026-07-10

Full-tunnel default routes over the mesh (accepting a peer-advertised
0.0.0.0/0 or ::/0 so this node's *entire* internet path goes through the
overlay) now actually works on Linux, instead of just being rejected by
default with a warning about why. This landed in stages across one long
session:

**The hazard, and why a literal `/0` was never going to work.** Installing a
peer's advertised default route verbatim — `AddRoute(0.0.0.0/0, dev tun0)` —
does two bad things at once: it throws away the host's only other path to
the internet, and it loops the mesh's *own* underlay traffic (the UDP
packets carrying the encrypted tunnel itself) back into the tunnel that
traffic is what keeps up in the first place, breaking every peer connection,
not just the exit node's. `config.NewNetworkDefaults()` already shipped
`RouteRej: [0.0.0.0/0]` for exactly this reason — but that only *prevented*
the hazard, it didn't provide a working alternative. Fixed a real,
independent bug found along the way: that reject default only ever covered
`0.0.0.0/0`, never `::/0` — an IPv6-enabled network was silently accepting a
peer-advertised IPv6 default route, unguarded, hitting the identical loop.
Both families are in the default reject list now
(`internal/config/config.go`, both `NewNetworkDefaults` and the config-load
backfill path).

**The design** (this took a couple of false starts, kept for the record
since they're informative): first considered mirroring wg-quick's Linux
approach directly — fwmark + policy routing tables, plus `IP_BOUND_IF` /
`IP_UNICAST_IF` / `SO_SETFIB` / `SO_RTABLE` on macOS/Windows/FreeBSD/OpenBSD
respectively. Correct in principle, but four different socket primitives,
a `net.fibs` reboot requirement on FreeBSD, and an entirely new
default-route-change-watcher subsystem on every platform — a lot of new,
low-level surface for a problem gravinet is unusually well positioned to
solve more simply: it already knows, precisely and continuously, every
peer's real underlay endpoint (that's what the dataplane needs to be correct
about regardless). So instead: a `/32` (or `/128`) host route to each
peer's real endpoint, via the *original* physical gateway — the most
specific possible prefix, which wins longest-prefix-match against anything
less specific, including a literal `/0` or a `/1` split, on every platform,
with zero new platform-specific socket options. The `/0` itself is still
never installed literally — split into `0.0.0.0/1` + `128.0.0.0/1` (or
`::/1` + `8000::/1`), the same technique WireGuard's own clients use, so the
real default route underneath is never touched at all.

**`internal/tun/gateway_linux.go`, `route_linux.go`** — the foundation
everything else depends on. `DefaultGateway` reads the kernel's own route
table via an `RTM_GETROUTE` dump (the read counterpart to the existing
`RTM_NEWROUTE`/`RTM_DELROUTE` writes), filtering out whichever interface
gravinet's own tun device is on — so it keeps finding the *real* physical
default even after gravinet's own full-tunnel route already exists,
something a naive `ip route get`-style single lookup can't do once that's
true (it would just report gravinet's own route back to itself).
`AddGatewayRoute`/`DelGatewayRoute` add the "via a gateway, on a specific
physical interface" route shape alongside the existing on-link
`AddRoute`/`DelRoute`, sharing the same rtnetlink plumbing
(`sendRouteMsg`). `Device.ifIndex()` → exported `IfIndex()`, added to the
`mesh.Device` interface and every platform's tun backend (a real
implementation on Linux, a clear "not implemented on this platform yet"
stub everywhere else — same shape as `pty_unsupported.go`'s pattern for
remote shell before v308) so mesh-layer code can call it uniformly
regardless of platform. All three gateway/route primitives get the same
stub treatment on Windows/macOS/FreeBSD/OpenBSD — full-tunnel is Linux-only
for now; those platforms fail closed with a clear error rather than
silently doing nothing or crashing.

**`internal/mesh/fulltunnel.go`** — reference-counted bypass routes, not a
direct one-owner-per-route model. Turned out to be necessary, not
optional: a seed (dialed before any session exists — otherwise a full
tunnel would swallow the very handshake dial that would establish a
session, a chicken-and-egg problem) and the peer session it becomes both
need a route to the same address, sometimes at the same time, and neither's
cleanup should be able to delete the other's still-needed route.
`Engine.bypassRefs` is global (per-Engine, not per-network) for the same
reason: two different networks could in principle have a peer at the same
underlay address. `acquireBypassRoute`/`releaseBypassRoute` are the only
things in this package allowed to touch one of these routes; release
reuses the exact gateway/interface captured at install time rather than
re-resolving it, so a gateway change between acquire and release can't
produce a delete that targets a different route than the one actually
installed. `syncPeerBypassRoute` covers the peer-session side (wired into
NAT-roam, session install/re-handshake, `localDisconnect`, and
`pruneDead`); `syncSeedBypassRoutes` covers seeds and explicit TCP/TLS
seeds (deduplicated by address — a UDP seed and its resolved TCP fallback
share one `/32` automatically), wired into `initLoop` *before*
`primeTCPSeeds` specifically (a first-pass ordering bug: `primeTCPSeeds`
fires an async dial goroutine that could otherwise race the route
installation). Gated by `ns.fullTunnel`, which nothing sets yet at this
point in the session — inert by construction, so wiring the calls in
doesn't change behavior for a network that hasn't opted in.

**`internal/mesh/routes.go`** — the actual trigger.
`syncRoute` special-cases a prefix with `Bits() == 0` to
`syncFullTunnelRoute` instead of installing it literally: installs the `/1`
split (never the literal default), and — critically — flips
`ns.fullTunnel` true and backfills every already-live session and seed
*before* installing the splits, not after, so there's no window where the
splits are up but a pre-existing connection's own traffic has no escape
hatch yet. Withdrawal reverses the order: remove the splits first, then
turn `fullTunnel` off and release the now-orphaned bypass routes.

Tested at every layer: synthetic-message parsing, a real integration test
against the sandbox's actual kernel routing table (`DefaultGateway` and
`AddGatewayRoute`/`DelGatewayRoute` both verified against a genuine rtnetlink
dump, not just mocked), unit tests for the ref-counted acquire/release
behavior including the exact seed-to-peer handoff collision this design
exists to prevent, and a full end-to-end test through the real gossip/wire
protocol between two real engines over real UDP — proving B genuinely learns
A's advertised default route, installs the `/1` split (never a literal
`/0`), and cleans up correctly on withdrawal. `go vet` and the full
linux/windows/darwin/freebsd/openbsd/{amd64,arm64,arm} build matrix are
clean; `-race` is clean on every new test.

Non-Linux full-tunnel backends (the `gateway_unsupported.go` stubs) are
still open — Windows, macOS, FreeBSD, and OpenBSD each need their own
`DefaultGateway`/`AddGatewayRoute`/`DelGatewayRoute`, but the design now
calls for the *same* mechanism everywhere (host routes via `AddRoute`-style
primitives, no platform-specific socket option), so each is "port the
existing pattern to this platform's native route-table API," not a new
design per platform.

---

## v311 — 2026-07-09

Reported: remote shell refused on FreeBSD with `open /dev/ptmx: open
/dev/ptmx: no such file or directory` (the doubled "open /dev/ptmx:" is just
`fmt.Errorf`'s own wrapper prefix in front of the stdlib `*PathError`'s
identical-looking message — not a formatting bug, just a redundant-looking
but accurate one). Traced to `pty_freebsd.go` opening `/dev/ptmx` the same
way pty_linux.go does, which turns out to be wrong on FreeBSD specifically,
not just a missing-device deployment quirk.

FreeBSD's own `pts(4)` man page is explicit that `/dev/ptmx` is a *separate,
legacy* device: "this device should not be opened directly ... new devices
should only be allocated with posix_openpt(2)." It's provided by the
old-style `pty(4)` compatibility driver (for things like Linux binary
emulation), and — unlike `pts(4)` itself, the real Unix98 pty
implementation, which is always present on FreeBSD 8.0+ — that compat
driver isn't loaded by default on a stock install. So `open("/dev/ptmx")`
fails with ENOENT until something (`kldload pty`, or a port/package that
needs Linux emulation) happens to have loaded it, which most FreeBSD hosts
never do. v308 modeled the FreeBSD backend on the Linux one without
noticing this — Linux's own `/dev/ptmx` really is always present, so nothing
in testing against that assumption would have caught it.

Fixed by allocating through `posix_openpt(2)` directly (the actual, correct,
always-available FreeBSD entry point for a pts(4) master) instead of
`/dev/ptmx`. Go's stdlib `syscall` package doesn't wrap this call, but does
export the syscall number — `syscall.SYS_POSIX_OPENPT`, uniformly `504`
across every FreeBSD arch gravinet builds for (386/amd64/arm/arm64/riscv64)
— so it's issued with a plain `syscall.Syscall`, no golang.org/x/sys/unix
needed for one call, matching the project's existing zero-dependency
approach. Everything downstream of allocation (`TIOCGPTN`, the `/dev/pts/<n>`
slave, window sizing, spawning) is unchanged — it's the same pts(4) master
either way, just reached through its real syscall entry point instead of a
device node that isn't guaranteed to exist.

Only `pty_freebsd.go` changed. Darwin's `/dev/ptmx` isn't part of this same
legacy/native split — it's the one true allocation path there, no
posix_openpt(2)-vs-compat-device distinction exists on macOS — so
pty_darwin.go was never at risk from this and didn't need the same fix.

`go vet` and `CGO_ENABLED=0 go build ./...` are clean for both FreeBSD
targets (amd64, arm64) plus the full linux/windows/darwin/openbsd matrix,
and the existing test suite still passes. Still worth confirming on real
FreeBSD hardware before the next release build — this couldn't be run
there to verify the original ENOENT actually goes away, only reasoned about
against FreeBSD's own posix_openpt(2)/pts(4) man pages and confirmed to
build.

---

## v310 — 2026-07-09

Reported from real Windows hardware: the gravinet service itself would crash
shortly after a remote shell session ended — connect, use the shell
normally, disconnect, and the daemon (not just the shell) would go down,
dropping the node from every other peer's list until the service manager
brought it back. Not the same bug as v309: that one was about the
`AllowRemoteShell` toggle's restart not landing; this one only showed up
*after* a shell session had actually been used and closed.

The bug: `pumpShellSession` (shell.go) runs two goroutines against one
`*ptySession` — pty-output-to-browser and browser-input-to-pty — and either
one can call `sess.close()` on its own the instant its side of the
connection errors, which on an ordinary disconnect happens to both at
roughly the same moment if the shell had just produced any output (a prompt
redraw is enough). So `close()` gets called twice, concurrently, more often
than not.

That's harmless everywhere except Windows. `pty_unix_common.go`'s `close()`
just calls `(*os.File).Close()`, which Go itself reference-counts against a
concurrent double-close — a second call is a safe no-op. `pty_windows.go`'s
`close()` (added in v308) instead makes four raw, unguarded
`syscall.CloseHandle`/`ClosePseudoConsole` calls with no synchronization at
all. Closing the same Win32 handle value twice isn't a benign no-op the way
POSIX `close(2)` on an already-closed fd is: if anything else in the
process — a new peer connection, a file, another pipe — gets allocated a
handle in the gap between the two closes, it can be handed that exact
numeric value, and the second, stale `CloseHandle` then closes *that*
unrelated, currently-in-use handle instead. In a service constantly
opening and closing handles for mesh peers, that's a real race, not a
theoretical one, and it's a very plausible way to take the whole process
down or corrupt live peer connections out from under it — which is exactly
what got reported first, before it was traced back to the shell feature at
all.

Fixed by giving `ptySession` a `sync.Once` and wrapping `close()`'s body in
it, so the real teardown runs exactly once no matter how many goroutines
call it or how many times. Only `pty_windows.go` changed — the Unix
backends were never at risk from this, for the reason above.

`go vet` and a full `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`
are both clean. Worth real-machine testing on Windows before the next
release build, same as v308/v309 — this still couldn't be run on real
Windows hardware to confirm the crash reproduces and stops reproducing,
only reasoned about from the code and confirmed to build.

---

## v309 — 2026-07-09

Reported from real Windows 11 25H2 hardware: enabling Remote shell from the
web admin (Settings → Cluster) looked like it worked — the toggle showed on,
`/api/shell/setting` had reported `restart:true`, the UI's own restart flow
ran — but a shell session then opened with no output and hung. Turned out
the toggle's live value hadn't actually taken effect: the running process
still had the old `AllowRemoteShell=false` it started with, meaning the
service silently hadn't restarted itself the way the UI implied it had. A
manual service restart fixed it — which is exactly the symptom of the
automatic restart not actually landing, not of anything wrong with v308's
ConPTY code itself (traced a fair way into that first — pipe wiring,
`UpdateProcThreadAttribute`'s by-value `lpValue` convention, the
`STARTF_USESTDHANDLES` question — before the real cause turned out to be one
level up, in the restart mechanism, not the shell session).

The bug: `service.Restart()`'s Windows case shelled out to `powershell
-Command "Restart-Service gravinet"` and `.Run()` — *blocked and waited for
it* — from inside the web admin's own HTTP handler, which is running inside
the very gravinet.exe process Restart-Service needs to see stop before it
can start a new one. Unlike systemd/launchctl/rc.d (the Linux/macOS/BSD
cases right next to this one), which are purpose-built for a service asking
to restart itself, `Restart-Service` is a generic, synchronous cmdlet with
no special handling for that: it polls the SCM until *this* process reports
`SERVICE_STOPPED`, then starts a new one — so waiting on it synchronously
from inside that same process makes the outcome depend on timing (how long
this process's own shutdown takes, whether it releases its listeners and
TUN adapter before the SCM's stop-wait times out, whether the goroutine
even survives long enough to notice) that the process has no way to
guarantee about its own death.

Fixed by detaching the restart from this process entirely: `Start()`
(fire-and-forget) instead of `Run()`, so `service.Restart()` returns
immediately regardless of what happens to gravinet.exe next, and the
spawned script sleeps 2 seconds before it even calls `Restart-Service` —
giving the old process time to actually finish exiting and release its
resources before anything tries to stop-and-restart it. The real
confirmation that a restart landed was never this exit code anyway — it's
the web admin's own `quietPollBack` loop (ui.go), which polls `/api/ping`
until a *new* boot id shows up, i.e. actual proof a fresh process
answered — so losing Restart-Service's own success/failure signal in
exchange for removing the self-referential race is a clean trade, not a
regression: the failure path this used to catch (`couldn't restart
automatically — run (elevated): Restart-Service gravinet`) still exists for
the case where the script can't even be launched, it just no longer covers
a late failure inside the detached script itself.

Only the Windows case changed. Linux/macOS/FreeBSD/OpenBSD keep their
existing synchronous calls — systemd, launchctl, and rc.d are already the
right tool for "a service restarting itself" and don't share this failure
mode, so there was nothing to fix there.

---

## v308 — 2026-07-09

Remote shell (`AllowRemoteShell`) worked on linux/amd64 and linux/arm64 only
— everywhere else, `spawnPTY` just returned "not supported on this
platform/architecture yet" (pty_unsupported.go). Asked directly to make it
work everywhere gravinet actually ships (see `scripts/build-release.sh`'s
`TARGETS`): linux/arm, freebsd/amd64, freebsd/arm64, darwin/amd64,
darwin/arm64, openbsd/amd64, openbsd/arm64, windows/amd64, windows/arm64.
All nine now have a real PTY backend.

First, a refactor: pty_linux.go had never been split into "how Linux
specifically allocates a pty" versus "everything about running a shell once
you have one" (spawn the child, resize the window, wait for exit, ...) —
the latter is identical logic regardless of which OS-specific ioctls got
you a `ptySession{ptmx, cmd}` in the first place. Pulled that shared half
out into pty_unix_common.go so the three new Unix backends (below) don't
each carry their own copy of `defaultShell`, `resizePTY`, `close`, `wait`,
and the /etc/passwd-fallback shell lookup.

New pty_<os>.go files, one per allocation mechanism (all pure Go — no cgo,
no golang.org/x/sys, matching the zero-dependency approach the existing
Linux file already took):

- **linux/arm** — turned out to need nothing new: the generic ioctl numbers
  pty_linux.go already used are architecture-independent across every Linux
  port gravinet ships (amd64, arm64, arm all use the same
  `<asm-generic/ioctls.h>` numbering — it's only a handful of ports
  gravinet *doesn't* ship, like sparc and mips, that differ). Widened the
  build tag; no new file needed.
- **FreeBSD** (pty_freebsd.go) — /dev/ptmx + `TIOCGPTN`, same idea as
  Linux's own approach and even the same `/dev/pts/<n>` slave path, just a
  different numeric ioctl code (`0x4004740f`, confirmed against FreeBSD's
  own `pts(4)` man page and cross-checked against `golang.org/x/sys/unix`'s
  published constant for the same value — not imported, just used to verify
  the hand-derived number). No lock/unlock step: FreeBSD's `grantpt()`/
  `unlockpt()` are documented as pure validation with no ioctl behind them
  at all, since `posix_openpt(2)` already hands back a correctly-owned
  slave.
- **Darwin/macOS** (pty_darwin.go) — /dev/ptmx again, but three different
  ioctls (`TIOCPTYGNAME`/`GRANT`/`UNLK`) since Darwin's pty driver predates
  the Linux/FreeBSD unit-number convention; the slave's path comes back
  directly from `TIOCPTYGNAME` rather than being constructed. Ioctl values
  and call order cross-checked against `creack/pty`'s own `pty_darwin.go` —
  a widely used, actively maintained reference for exactly this — not
  reproduced from scratch.
- **OpenBSD** (pty_openbsd.go) — no `/dev/ptmx` at all; instead one
  `PTMGET` ioctl on `/dev/ptm` allocates a pty pair and hands back *both*
  fds already open (see `ptm(4)`), which is actually less code than every
  other backend here. `PTMGET`'s value (`0x40287401`) isn't published
  anywhere convenient, so it's computed by hand from OpenBSD's own
  `_IOR('t', 1, struct ptmget)` macro and cross-checked by running the same
  encoding against a *different*, independently-documented `ptmget`-shaped
  ioctl (NetBSD's `TIOCPTSNAME`) to confirm the formula lands on its known
  published value before trusting it for `PTMGET` itself. A compile-time
  assertion pins `struct ptmget`'s Go mirror to exactly 40 bytes, matching
  the size baked into that encoding, so a future field edit that silently
  desyncs the two fails the build instead of sending a malformed ioctl.
- **Windows** (pty_windows.go) — a different API family entirely: ConPTY
  (`CreatePseudoConsole`, Windows 10 1809+ / Server 2019+), which hands back
  two pipes instead of one master fd. The five ConPTY entry points aren't
  wrapped by the standard `syscall` package, so they're resolved from
  `kernel32.dll` via `syscall.NewLazyDLL` — but `LazyProc.Call` *panics* on
  a missing DLL export, so `spawnPTY` resolves all five up front
  (`findConPtyProcs`) and fails with a clear "needs Windows 10 1809+ /
  Server 2019+" error instead of crashing the daemon on an older host.
  Struct layouts (`STARTUPINFOEXW`, the `PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE`
  calling convention) and the close-ordering needed to dodge a documented
  `ClosePseudoConsole` deadlock hazard (plus a real lingering-`conhost.exe`
  bug, microsoft/terminal#4050, that means a pending output-pipe read isn't
  reliably unblocked by the child dying on its own) are cross-checked
  against `github.com/UserExistsError/conpty`, a small, widely used, actively
  maintained wrapper around Microsoft's own documented ConPTY sample.
  `close()` deliberately leaves the process/thread handles open for `wait()`
  (running concurrently) to close once it unblocks, rather than risk closing
  a handle out from under a concurrent `WaitForSingleObject` on it.

Split the Windows backend's pure logic (COORD packing, command-line
quoting, the UTF-16 environment-block encoding) into its own
build-tag-free file, windows_pty_helpers.go, specifically so it can be unit
tested on every platform rather than only on a real Windows host — added
tests there that actually exercise the encoding (byte layout of a
multi-entry env block, the empty- and single-entry edge cases, COORD's
bit-packing) rather than just confirming it compiles.

**On verification**: this session only has Linux hardware. Everything above
was cross-compiled for its real target (`GOOS=freebsd/darwin/openbsd/windows
GOARCH=...`) and passes `go vet`, and every hand-derived struct size has a
compile-time assertion pinning it — but none of the four new backends have
actually spawned a shell on their real OS. The Linux path is unchanged in
behavior and still fully covered by the existing integration tests
(`TestShellHijackAndLocalWSSession` and friends), which continue to pass.
Worth real-machine testing on FreeBSD, macOS, OpenBSD, and Windows before
leaning on this in production there.

---

## v307 — 2026-07-09

v306 added the current node as a row in its own Peers table but made that
row inert — checkbox disabled, nothing selectable — on the assumption that
"can't be banned" meant "can't be selected at all." Reported back
immediately: that also silently took out the 🛈 info lookup and the ■
shell button, both of which start from a ticked row, and there's no reason
either should be off-limits for your own node. Opening a shell on yourself
is in fact the *original*, simplest case the shell feature supports —
`handleShellWS` already special-cases `node == "" || node == SelfID()` to
spawn a local PTY directly, no overlay relay involved — so the self row
was blocking access to a path the backend had no problem with at all.

Re-enabled the self row's checkbox so it participates in selection like any
other row, and split "can't be banned" out as its own, narrower rule rather
than a blanket one: the Ban button now checks specifically for the self id
among what's ticked and refuses with an explanatory alert ("untick 'this
node' first") instead of the row being unselectable in the first place.
Shell and 🛈 both work on it now.

Two things had to be real, not stubbed, for that to actually be useful
rather than just clickable:

- **🛈 needs an endpoint to look up**, and a self row was previously given
  `endpoint: ''` — there was nothing for a lookup to resolve. It's now filled
  with this node's own observed public address (`state.nat.public`, the same
  value already shown in the "This node: ..." banner above the table), which
  is the honest self-equivalent of what a peer's endpoint column means (its
  observed NAT mapping) — Monitor → mesh peers' endpoint column shows it too,
  rather than the placeholder dash it had been overridden to.
- **Shell needs to actually dial the right thing.** The self row's node id
  is whichever node's peers you're currently looking at — your own, normally,
  but if a remote peer is selected up top (Manager mode, browsing another
  node's admin panel), that page's "self" row is *that peer's* identity, and
  opening a shell on it is a real cross-node relay, not a local session — it
  still needs Manager mode and the peer actually connected, same as any other
  row on that page. The Manager-mode/connectedness checks are now skipped
  only when the ticked row is self *and* no remote node is currently
  selected; otherwise they apply exactly as they already did for a normal
  peer row. Getting this distinction backwards would have let a self-labeled
  row silently skip a real permission check on someone else's node.

No backend changes — `SelfPeer`, `/api/status`'s `self` field, and the local
shell path were already correct from v306 (and shortly before); this was
entirely the frontend being more restrictive than either warranted.

---

## v306 — 2026-07-09

The Peers page listed every peer connected to a network except one: the
node you were actually looking at. Asked for directly — an operator staring
at Mesh → Peers has no way to see where their own node sits relative to
what it's connected to, and (more concretely) no way to run the 🛈
info/WHOIS lookup or open a shell *on* the node they're logged into from
that page, since both actions start from a row in that table and there was
never a row for self.

Added one: each network's peer list now includes this node's own hostname,
node id, and overlay address, labeled "this node" and sorted in among the
real peers the same way any other row is (alphabetically by hostname, id as
tiebreaker) rather than pinned to the top — "next to the other peers" was
the ask, not "above them". The row is deliberately inert: its checkbox is
disabled (so it can never enter a selection, which is what actually keeps
it out of reach of Ban and the rest of the row-button toolbar — no separate
guard needed there), its state cell reads "this node" instead of a
double-clickable enabled/disabled toggle, and its endpoint/notes cells read
"—" rather than data that doesn't apply to a connection you're not
actually making. Banning your own node was never going to be meaningful
mesh-wide (it's the node the ban would be issued *from*), so it's simply
absent from what can be ticked, rather than present-but-erroring.

New on the backend: `Engine.SelfPeer(networkID)` returns this node's own
identity in the same `PeerInfo` shape `ListPeers` already returns, scoped to
one network — same as how overlay addresses work generally, since a node
can have a different overlay address on each network it's joined. Endpoint,
relay, transport, and session-timing fields are left at their zero values;
none of them describe a connection to yourself, and the two frontend tables
that render peer rows (Mesh → Peers and the read-only Monitor → mesh peers
page) both already special-case a row with nothing meaningful in those
columns, so the self row rides that existing path rather than needing new
cases. `/api/status` now carries this per network as `self`, alongside the
`peers`/`disabled_peers`/etc. it already returned.

---

## v305 — 2026-07-09

Renamed the "Allow remote shell" setting to "Remote shell" (and the Peers
page's shell-button tooltip to match). Cosmetic only.

The terminal modal now closes itself automatically, shortly after a shell
session exits — asked for directly, but finding it surfaced a real
deadlock in the local (non-proxied) shell path that a naive fix would have
shipped right on top of: `pumpShellSession` checked the "has the shell
exited" channel non-blockingly, immediately before a *blocking* read of the
next browser message. Once that read was blocked waiting for the browser's
next message — which the browser has no reason to send once *it's* waiting
on the exit confirmation *from us* — the exit could never be noticed. Every
clean "type `exit`, session ends" case deadlocked, not just some of them;
it just wasn't visible before because nothing needed to react to the
session ending until now (there was no auto-close depending on it — the
status text update just never arrived to fire). Fixed by moving that read
loop into its own goroutine, matching the already-correct structure the
node-to-node proxy path (`pumpShellHijack`) used from the start — this
function was the one place that didn't.

Found because the end-to-end browser test written to confirm the requested
auto-close actually exercised the deadlocked path and hung — not from
reading the code. Two of the three existing Go tests covering this feature
had the same blind spot for a different reason: they declared success the
moment "exit" was *sent*, without ever confirming the exit *message* came
back, so they couldn't have caught this either. Fixed those too, so they
now wait for and assert on the actual exit code — which also caught a
second, smaller bug the same fix exposed: `shellControl.Code` had
`omitempty` on its JSON tag, so a *clean* exit (code 0 — the common case)
serialized with no `code` field at all, and the frontend read that back as
`undefined` rather than `0`. `omitempty` on a field whose most important
value is its zero value is exactly this trap; removed it.

---

## v304 — 2026-07-09

Two small UI adjustments to the shell feature's buttons. The Peers page's
text "Shell" button is now an icon-only button (▪), matching the existing
icon-button convention elsewhere in the same toolbar (the Networks page's
join-token button is ● — a filled circle — so the shell button is ■, a
filled square, deliberately the same visual weight and style rather than a
new icon language). Settings' "Open shell on this node" button — added,
then removed, then kept after some back-and-forth on what was actually
being asked for — stayed removed; nothing in this entry changes that.

Confirmed the glyph itself renders as an actual solid square (not a
rounded-corner or textured render some fonts substitute for geometric
Unicode shapes) by screenshotting the button in isolation at 4x device
scale and reading the pixels back: a clean, sharp-edged, uniformly filled
rectangle, next to the existing circle icon for comparison.

---

## v303 — 2026-07-09

Fixed a visible layout bug in the shell terminal modal introduced by v302's
switch to `xterm.js`: a block of the terminal's own dark background showing
as empty space to the right of the actual terminal content, because
`.modal-panel`'s `width:100%` stretched the modal to its `920px` max-width
regardless of how wide the terminal itself actually rendered (100 columns
at a 13px monospace font comes out narrower than that). `.term-panel` now
overrides `width` to `auto`, so the panel sizes to its content — the
terminal — instead of stretching to fill the available space up to the
cap.

Verified with the same real-browser pipeline introduced in v302: measured
actual pixel values across the panel/terminal boundary in a fresh
screenshot (not just re-reading the buffer text, which this bug wouldn't
have shown up in at all — it's purely a CSS sizing issue, invisible to
anything that only checks rendered characters). The terminal's background
now extends to within a few pixels of the panel's own border and corner
radius, with no leftover gap.

---

## v302 — 2026-07-09

Replaced the shell feature's hand-rolled terminal emulator with a vendored
copy of `@xterm/xterm` 6.0.0 (MIT licensed — see
`internal/webadmin/vendor/xterm/VENDORED.md`), the actual industry-standard
browser terminal implementation (used by VS Code, GitHub Codespaces, Google
Cloud Shell, and effectively every other web-based terminal). v301 fixed a
specific, real bug in the hand-rolled parser found by capturing and
replaying real `htop` output; this pass is the broader fix that bug was a
symptom of: a small hand-rolled subset of VT100/xterm will always be behind
on some sequence some real application depends on, discovered one
incorrect-or-missing escape at a time, one application at a time. Rather
than keep closing that gap reactively, the actual, exhaustively-tested
implementation of that surface now does the parsing.

Vendored as static browser assets (`vendor/xterm/xterm.js`,
`vendor/xterm/xterm.css`), embedded into the binary via `go:embed` — the
same mechanism already used for the bundled Windows driver in
`internal/tun` — and served unauthenticated at `/static/xterm.js` and
`/static/xterm.css` by two new handlers, at the same trust level as the
main index page's own HTML/JS/CSS. This is a Go-standard-library-only way
to bundle a static frontend asset, not a Go module dependency: `go.mod`
still has zero third-party dependencies.

The `Term` class, `shellKeyBytes`, and the manual character-grid rendering
in `ui.go` are gone, replaced by `xterm.js`'s own `Terminal` API:
`term.write()` takes the WebSocket's raw binary frames directly (including
doing its own UTF-8 decoding across message boundaries, which the code it
replaced did not do correctly), and `term.onData()` already encodes every
keystroke, paste, and special key correctly — replacing the hand-rolled key
encoding table along with the output-side parsing. This also picks up
alternate-screen-buffer and scroll-region support for free, which the
hand-rolled version explicitly didn't have (see v301 and the shell
feature's original v299 entry) — a full-screen app like `vim`, `less`, or
`tmux` should now render correctly, not just plain shell output.

Verified end-to-end against a real browser, not just the backend: launched
headless Chrome against a real running instance of the actual webadmin
server, drove a real login, opened a real shell over a real WebSocket into
a real PTY, ran real `htop` in it, and read the rendered terminal buffer
back out through `xterm.js`'s own buffer API (not just a screenshot) to
confirm the output — meters, process list, column alignment, the
`F1Help ... F10Quit` footer — matches what a real terminal shows,
byte-for-byte-driven, character-grid-verified. Screenshots taken and
inspected too, but the buffer-text check is the one that doesn't depend on
a human looking at a picture.

Terminal size is still fixed at 100x30 for this pass, same as before —
`xterm.js` supports live resizing and the backend's resize control message
already exists end to end, but wiring an actual resizable/auto-fitting
terminal UI is a reasonable follow-up, not part of replacing the emulator
itself.

---

## v301 — 2026-07-09

Fixed real corruption in the web admin's shell terminal (`Term`, in
`ui.go`) when running full-screen curses/ncurses apps — reported against
`htop`, whose output was showing up with stray "B" characters spliced
throughout the CPU/Mem/Swp meters, the task summary line, and column
headers. Found by capturing `htop`'s actual raw PTY byte stream (via a
throwaway test using the real `spawnPTY` code path, not a guess) and
replaying it through the actual `Term` class in Node outside the browser,
which reproduced the exact corruption from the report byte-for-byte on the
first try and made the root cause immediately visible in the raw stream
rather than guessed at from the rendered symptom.

The bug: `ESC ( B` — a three-byte sequence selecting the ASCII character set
into G0 (VT100's answer to "make sure a terminal that might be in
line-drawing mode is back to normal text," and one of the most common
sequences any real terminal application emits, `htop` included) — was
being treated as a two-byte escape. The parser consumed `ESC` and `(`,
returned to normal, and then printed the sequence's third byte (`B`, or
whichever charset it selected) as a literal character instead of consuming
it as part of the escape. Every single `ESC ( B` in the stream leaked
exactly one stray letter into the output — which is exactly the pattern in
the report. Fixed by adding a state that correctly consumes all three bytes
of `ESC ( / ) / * / + <byte>` (and `ESC # <byte>`) before returning to
normal.

Same investigation surfaced two more real gaps once the main corruption was
out of the way and the rest of the screen could actually be read clearly:
`ECH` (`CSI Pn X`, erase N characters in place) and `VPA` (`CSI Pn d`, move
to row N, column unchanged) were silently ignored, and `REP` (`CSI Pn b`,
repeat the preceding character N more times) wasn't implemented at all.
`htop` leans on `REP` heavily — both for meter-bar fills (print one `|`,
then repeat it instead of sending each character) and for column padding
in the process list header — so without it, bars were under-filled and
header columns lost their padding and drifted out of alignment with the
data rows below. All three are now implemented; `Term.putChar` tracks the
last printed character so `REP` has something to repeat.

Verified against the real captured byte stream after each fix (not just
"looks right" — the same Node replay showed the exact corruption disappear
character-for-character), including a second, longer capture spanning
several of `htop`'s own incremental redraw cycles (not just one full
repaint) to check the fix holds up under partial-screen updates too, which
is `htop`'s actual normal operating mode.

Still true from the shell feature's own introduction (v299): no scroll
regions or alternate screen buffer, so an app that depends on those will
still look wrong in ways beyond today's fix — this pass was specifically
about the corruption that made even supported apps like `htop` render
incorrectly, not about extending coverage to apps that need the
unsupported features.

---

## v300 — 2026-07-09

The three settings that need a service restart to take effect — a network's
subnet or this node's overlay address, Geo-IP lookups, and Allow remote
shell — now restart the service automatically once saved, instead of saving
the change and leaving a "Structural changes saved — restart the service to
apply them" banner with a manual "Restart now" button for the operator to
come back to later. No backend change was needed: every one of these edits
already reported `restart: true` in its response, and the frontend already
had a quiet, no-full-page-takeover auto-restart-and-reconnect path
(`quietRestart`/`quietPollBack`, backing the `edit(path, payload,
autoRestart)` helper's third argument) — it just wasn't wired up for these
three call sites, which all passed `false` (or omitted the argument) and
took the defer-to-a-banner path instead. This was a one-line flip at each of
the three sites, not new plumbing.

Both confirm() dialogs on the subnet/address path (which already warn about
the restart before the edit is even sent) now say the node restarts
immediately, rather than "takes effect on next restart" — and the subnet
dialog still separately calls out that the same change has to be made on
every other node in the network, which auto-restarting this one node does
nothing to change. That's the one real tradeoff worth naming: today, making
several structural edits back-to-back means several restarts instead of
one batched at the end. Given each edit already requires clicking through
an explicit confirm() that discloses the restart, this reads as a minor
efficiency cost rather than a new risk, and it's what removes the actual
friction being asked about — but if that trade stops feeling worth it,
reverting any one of these three call sites to `false` (or dropping the
argument entirely) puts the manual banner straight back for just that
setting.

The deferred-restart banner/button infrastructure itself
(`restartBanner`/`doRestart`/`state.restartPending`) is untouched and still
there, just unreachable from any current call site — left in place for any
future setting that wants to batch rather than restart immediately.

---

## v299 — 2026-07-09

Added a real OS shell/PTY session through the web admin — for this node, or
(via the existing Manager/Managed cluster) for a Manager peer opening one on
a Managed peer. Gated by a new `WebAdmin.AllowRemoteShell` flag, off by
default and deliberately separate from `Managed`: the rest of Managed mode's
API surface (firewall rules, peers, keys, routes, ...) is this app's own
surface, which is a different risk than a full shell running as this
daemon's own user — normally root. Unlike `Managed`/`Manager`, this flag is
never remotely toggleable, not even by an already-authorized Manager peer —
`handleShellSetting` uses a new, stricter `sessionOnly` wrapper instead of
the usual `authed()`, and `handleProxy` also hard-blocks the path explicitly
as a second layer. Every session is transcript-logged in full (input and
output, tagged and timestamped) to a `shell-sessions/` directory next to the
config file; a one-line summary (who, target, duration, exit code) also
goes to the main log. Because it's a genuine, independent record of input,
it necessarily captures anything typed into that shell, including a password
at a prompt the terminal itself declined to echo back — a deliberate choice,
not an oversight, and worth treating the transcript directory as at least as
sensitive as the config file it sits next to.

Two hops, two transports. Browser to the node you're logged into: a real
WebSocket — gravinet has zero third-party Go dependencies (see `go.mod`,
still true after this) and this is the one feature that genuinely needs a
full-duplex, long-lived connection, so `ws.go` is a from-scratch, minimal
RFC 6455 server (handshake, text/binary/continuation/ping/pong/close frames,
correct masking direction) rather than a pulled-in library. When the target
is a *different* Managed peer, that node relays to the peer over a second,
inner hop: a raw hijacked TCP/TLS stream (both ends are gravinet's own code,
so paying for a second WebSocket handshake bought nothing) using a small
length-prefixed frame codec (`shellframe.go`). `handleShellHijack` is the
inner hop's endpoint and the only place that actually spawns a PTY — even a
"local" session goes through it, just via an in-process call instead of a
network round trip, so there is exactly one code path that ever starts a
shell. It rides the exact same trust model as the existing management
proxy: SSRF-guarded peer resolution (refactored out of `handleProxy` into
`resolveManagedTarget` so both share it), the overlay-source-plus-
Manager-advertisement check `authed()` already enforced for every other
proxied call, and TLS with the same overlay-internal self-signed trust
boundary `proxyClient` already uses.

PTY allocation (`pty_linux.go`) is real, syscall-level, dependency-free code
for linux/amd64 and linux/arm64: opening `/dev/ptmx`, the `TIOCSPTLCK`/
`TIOCGPTN` ioctls (not exposed by the stdlib `syscall` package, so given
directly), and `os/exec`'s own `Setsid`/`Setctty` support for adopting the
slave as the controlling terminal — no cgo, no `golang.org/x/sys`. Every
other OS/architecture (macOS, FreeBSD, OpenBSD, Windows, and any Linux
architecture outside those two) gets `pty_unsupported.go`: a clear "not
supported yet" error rather than a half-working or silently-wrong guess at
another platform's pty mechanism.

The web UI gained a hand-rolled terminal emulator (`Term`, in `ui.go`) —
a fixed character grid, cursor, and enough ANSI/VT100 (SGR colors/bold,
cursor movement, absolute positioning, erase line/display) for ordinary
interactive shell use to look right. It does not implement an alternate
screen buffer or scroll regions, so a full-screen app that depends on those
(vim, htop, tmux) will look wrong; this is an honest terminal for shell use,
not an xterm clone. A "Shell" button on the Peers page opens one on a
selected, connected peer (requires Manager mode locally); a button in
Settings opens one on this node.

Found and fixed two genuine data races along the way (caught by
`go test -race`, not by inspection): two goroutines could each write to the
same connection unsynchronized during session teardown in both pump
functions (now serialized behind a mutex), and `shellTranscript`'s log/close
methods checked their file handle for nil *before* taking their own lock,
racing against each other at exactly the moment a session ended. Also
cross-compiled the whole repo for every previously-supported OS/arch after
adding the Linux-only PTY file — an earlier version of `pty_unsupported.go`
declared a different struct shape than `pty_linux.go`'s, which built fine
natively but would have failed on every other platform's own build.

---

## v298 — 2026-07-09

Added a `Notes` field — a free-form, operator-authored, purely local string,
never gossiped to peers — to networks, seeds, key slots, and individual
firewall rules, and renamed bans' `Reason` field to `Notes` for consistency
(same rename applied to the local control-socket protocol, the CLI's
`-reason` flag → `-notes`, and the webadmin route `/api/ban/reason` →
`/api/ban/notes`). Firewall rules already had a `Comment` field serving the
exact same purpose, so that got renamed to `Notes` rather than adding a
redundant second free-text field — the CLI's `fw add -comment` is now
`fw add -notes`.

Seeds needed a real type change to carry notes: `Network.Seeds` was `[]string`,
now it's `SeedList` (`[]Seed{Address, Notes}`). `SeedList.UnmarshalJSON`
accepts either the new object-array form or the bare string array every older
config used, so nothing already on disk needs migrating — the next save just
upgrades it to the object form. `SeedParts`/`resolveSeeds` and everything else
that only ever cared about the address itself now goes through
`Seeds.Addrs()`.

Peers don't have a persisted record at all in this codebase (they're
re-learned from the mesh each session), so peer notes are a new small
side-table: `Network.PeerNotes map[string]string`, keyed by node id,
local-only like `DisabledPeers` — new `PeerSetNotes`, a `peerNotes` map on
`netState` alongside the existing `disabledPeers`, and a `Notes` field on both
`PeerInfo` (connected peers) and `DisabledPeerInfo` (disabled peers) so a note
on an offline-but-disabled peer still shows up.

Every new field reaches the web UI: networks, keys, seeds, peers, and
firewall rules each got a notes column with double-click-to-edit (peers and
firewall rules didn't have any per-row editing UI for this kind of metadata
before — the firewall rule editor in particular had never exposed `Comment`
at all, CLI-only until now). The CLI got matching support: `network notes`,
`key generate|set|notes -notes`, `seed add|notes -notes`; peer notes stayed
webadmin-only, matching the existing peer-enable/disable split (the CLI has
never exposed peer management either).

QoS and NAT rules deliberately did *not* get a notes field — terse
match/action tuples (protocol+port+class; direction+source+dest+translate)
that don't tend to need a human explanation the way a firewall rule or a
named entity does. Worth adding on request, but not by default.

This is a packaging-only-adjacent change in scope (no wire-protocol bytes
changed — bans' gossip encoding, key distribution, and the join-token format
are all untouched; only Go field names, JSON keys on the *local*
control-socket/webadmin protocol, and CLI flags moved), but it touches config,
mesh, control, webadmin, and the CLI, so it's a real version bump rather than
an install-tree change like v297.

---

## v297 — 2026-07-09

Bundled `pkgman`, a small POSIX-shell wrapper that gives a single
`-u`/`-i`/`-r`/`-s` interface over whichever package manager is actually on
the box (`apt`, `dnf`, `pacman`, Homebrew, `pkg`, `pkg_add`), directly in the
`install/` tree and wired it into all four Unix installers
(`install-linux.sh`, `install-macos.sh`, `install-freebsd.sh`,
`install-openbsd.sh`). It has nothing to do with the mesh/networking code —
it's a general-purpose convenience utility, not a gravinet feature — but it
now installs and uninstalls alongside the `gravinet` binary itself: same
install step, same directory (`$PREFIX/bin` on Linux/macOS/FreeBSD,
`$PREFIX/sbin` on OpenBSD, matching wherever `gravinet` lands on that
platform), and removed by both the in-installer `--uninstall` path and the
standalone `uninstall-*.sh` scripts.

Windows was deliberately left out: `pkgman`'s own distro detection has no
Windows branch, so shipping a POSIX shell script there would just be dead
weight with nothing to invoke it.

This is a packaging-only change — no source files outside `main.go`'s
version string were touched, and the mesh/webadmin/transport code is
identical to v296.

---

## v296 — 2026-07-09

Geo-IP lookups (v295) now default to on rather than off, joining the info
panel's forward/reverse DNS and WHOIS lookups, which already ran
unconditionally on the same admin-triggered click — still disableable under
Settings → Privacy for an operator who'd rather this node never contact
ipapi.co. This only affects *new* configs; an existing config saved under
v295 keeps behaving as off (it never wrote the field to disk at all, since
v295's zero value was false) until the setting is explicitly touched.

Flipping the default surfaced a real bug in how it was represented: `Load()`
seeds a fresh `Config` from `Default()` and unmarshals the file's JSON on
top of it, so a plain `bool` that defaults to true can never actually
persist an explicit `false` — with `omitempty` (what v295 shipped),
`Marshal` drops a `false` value from the file entirely, and the very next
`Load()` silently resurrects the `Default()`-seeded `true` no matter how
many times it was explicitly turned back off in between. Caught by a
round-trip test (`TestGeoIPLookupExplicitFalsePersists`) written specifically
to exercise this, not by inspection — the bug wasn't visible from reading
the field's own code in isolation, only from actually saving, reloading, and
checking the value survived.

`WebAdmin.GeoIPLookup` is now a `*bool`, matching `IPForwarding`'s existing
pattern for exactly this scenario (a setting that defaults to true but must
also be explicitly overridable back to false): nil means "unset, use the
default" and is what `omitempty` correctly keeps out of the file indefinitely
across any number of unrelated saves, while an explicit `&true`/`&false`
always round-trips exactly as written. `Default()` deliberately leaves it
nil rather than setting a literal `true` — the accessor (`WebAdmin.GeoIPEnabled()`)
is what resolves nil to "enabled," not a value baked into `Default()` itself,
which is also what lets the default change again in the future without that
change silently failing to reach any config that was ever saved by a
version in between. Every read of the raw field elsewhere in the codebase
was moved onto the accessor.

---

## v295 — 2026-07-09

Added an optional geo-IP location + map to the peer/seed info (🛈) panel,
alongside the forward/reverse DNS and WHOIS sections already there. Looked
up from the target's public endpoint address via ipapi.co (HTTPS, no API
key), showing city/region/country, the network operator, and an embedded
OpenStreetMap view with a marker (plus a "view larger map" link) when
coordinates come back — with an explicit "usually accurate to city level at
best, sometimes far less" disclaimer, since IP geolocation is nowhere near
GPS-precise and showing an unqualified pin on a map risks implying
otherwise.

Off by default, unlike the DNS/WHOIS lookups next to it: those use the
internet's own decentralized, RFC-standard protocols (any DNS resolver, any
of the five regional WHOIS registries), while this sends the address to one
specific commercial third party over HTTPS — a different enough privacy
trade-off that it needed an explicit opt-in rather than just joining the
lookups that already ran unconditionally. New toggle under Settings →
Privacy, off by default, `geoip_lookup` in config (`WebAdmin.GeoIPLookup`).
Like `AuthMode`/the local admin user list, this is a webadmin.Server-scoped
setting captured once at startup rather than something the live-reload path
touches, so — same as those — it takes a restart to actually apply; the
toggle shows the standard pending-restart banner instead of pretending it
took effect immediately.

Backend: `lookupGeoIP` (new `internal/webadmin/geoip.go`) with its base URL
overridable for tests; `lookupSeedInfo` now runs it as a third goroutine
alongside the existing reverse-DNS/WHOIS ones when enabled, all best-effort
and independent (one failing doesn't block the others). Unit-tested against
a local `httptest.Server` rather than the real ipapi.co, including the
specific quirk that mattered most to get right: ipapi.co reports its own
failures — rate limits, reserved/private IPs — as HTTP 200 with
`{"error":true,"reason":"..."}` in the body, not a non-2xx status, so a bare
status-code check would have silently treated a failed lookup as a
successful empty one.

Frontend rendering was verified with the same real-jsdom-DOM approach as the
dropdown filtering in v294 — not just a syntax check — covering all four
states (disabled, enabled-and-errored, enabled-and-succeeded-with-a-map,
enabled-and-succeeded-without-coordinates) plus an explicit XSS-escaping
check on the rendered location text.

---

## v294 — 2026-07-09

Fixed the same "list reshuffles on its own" bug as v292, this time in
`Engine.ManagedPeers` — the source of the header's node picker and the
Speedtest source/target pickers. It built its result from a Go map
(`best := map[string]ManagedPeer{}`) and returned it via `for _, m := range
best`, which Go deliberately randomizes on every call. The header dropdown
already re-sorted its own copy client-side, which papered over this there,
but the Speedtest pickers had no sort at all — at any real peer count they'd
visibly reorder their entire option list on every 6s poll. `ManagedPeers`
now sorts by hostname (case-insensitive, falling back to node id, node id as
tiebreaker) before returning — the same fix, same rationale, as `ListPeers`
in v292 — so every consumer gets a stable order at the source, and the
Speedtest pickers are now explicitly sorted client-side too rather than
relying on that alone.

Also added filtering to both: the header node picker and the Speedtest
source/target pickers are plain native `<select>` elements, which have no
built-in search — just OS type-ahead (jump to the next option starting with
a typed letter, resets after a pause) — so a large mesh meant scrolling one
long unfiltered list. Each picker now grows a small text box above/beside it
once it has 10+ options (`DROPDOWN_FILTER_MIN`) that narrows the option list
by a case-insensitive substring match against the peer's name or raw node id
as you type; below that count nothing changes, so a typical small mesh's UI
is unaffected. Filtering only narrows what's visible/selectable — it never
touches the actual selection in JS state, so typing a filter that
temporarily hides the currently targeted peer doesn't silently switch the
GUI back to local; the real selection is restored the moment the filter no
longer excludes it. The Speedtest pickers' mutual-exclusivity behavior
(the same host can't be both endpoints) is preserved across a re-filter.

Verified with a real jsdom-driven DOM test (not just a JS syntax check) —
built a 500-peer and a 30-peer fixture, drove the actual browser-facing
functions (`renderPeerSelectOptions`, and `infoSpeedtest`'s `fill`/filter
closures end-to-end through its real async fetch path), and asserted on the
resulting `<select>`/`<input>` elements. That test caught a real bug during
development: the header picker's render function included "This node"
unconditionally regardless of the filter text, unlike the Speedtest
pickers' equivalent logic — fixed before landing, not after.

---

## v293 — 2026-07-09

Added system uptime to Monitor → metrics, underneath the per-network
bandwidth graphs. It's a single live value refreshed every poll rather than
a history graph like CPU/memory/disk/bandwidth above it — seconds-since-boot
only ever counts up at the same rate the clock does, so a chart of it would
just be a straight diagonal line, nothing worth plotting.

Backend readers (`readUptime`, alongside the existing `readCPUTotals`/
`readMemUsedPct`/`readDiskUsedPct`/`readNetDev` per-platform readers):
Linux reads `/proc/uptime` directly; Windows calls `GetTickCount64` (a
direct documented Win32 return value, no text to parse); macOS and FreeBSD
shell out to `sysctl -n kern.boottime` and parse its `{ sec = N, usec = N }`
struct-print form. OpenBSD also shells out to `kern.boottime`, but since
it isn't verified here whether OpenBSD's sysctl(8) uses that same
struct-print convention for this OID or instead prints a bare epoch
integer, it tries the struct form first, then a bare integer, and reports
unavailable rather than guess and risk silently showing a wrong number —
the same caution every other platform reader in this file already applies
to anything it can't parse with confidence. `uptime_seconds` is omitted
from the `/api/metrics` response entirely (rather than sent as 0) on a
platform whose reader couldn't get a value, so the UI can tell "just
booted" apart from "unavailable here" and hide the card instead of
showing 0s.

---

## v292 — 2026-07-09

Fixed the peers list reordering on its own — in the admin UI's Mesh → Peers
and Monitor → mesh peers pages, and in `gravinet list` — instead of staying
alphabetical by peer. The root cause was `Engine.ListPeers`: it built its
result by ranging over `ns.byNode`, a Go map, and Go deliberately randomizes
map iteration order on every single range — so the list came back in a
different order on every call even when the peer set itself hadn't changed
at all. `gravinet list` printed that raw order straight through. The admin
UI's shared row-builder (`peerRowsForNet`, behind both peers pages) did
re-sort its copy, which masked the backend randomness there, but by node id
rather than hostname — and since a node id is an opaque hex string with no
relation to the hostname actually shown (`nodeCell` puts the hostname
first), the resulting order had no visible alphabetical pattern, so any
shift in the peer set still looked like the list reshuffling at random.

`ListPeers` now sorts its own result by hostname (case-insensitively),
falling back to node id for a peer with no known hostname yet and using
node id as the final tiebreaker so the order stays fully deterministic even
if two peers ever share a hostname — fixing `gravinet list` and every other
consumer directly, not just the admin UI. `peerRowsForNet`'s client-side
sort now uses the same hostname-first key (via the existing `netNameCmp`
comparator already used for network-name sorting elsewhere in the UI, for
case- and accent-insensitive, naturally-numbered ordering) rather than node
id, so it actually matches what's on screen instead of just being *a*
stable order.

---

## v291 — 2026-07-09

Fixed the Unix installers (`install-linux.sh`, `install-freebsd.sh`,
`install-macos.sh`, `install-openbsd.sh`) leaking a `/tmp/tmp*` directory on
every single run. `build_from_source()` stages the freshly-built binary
under a `mktemp -d` directory so it can be installed from a known path
regardless of how it was produced — but nothing ever removed that directory
afterwards, on any of the four platforms. Since it's a fresh, uniquely-named
directory every time (never reused), repeated installs/upgrades/reinstalls —
exactly what a fleet's config-management or CI does — steadily filled up
`/tmp`, which is often tmpfs (RAM-backed) and has no automatic reclamation
of its own.

Each script now tracks that directory in `BUILD_TMP` and registers its
removal as a single `trap ... EXIT` right at the top of the script, before
anything could plausibly create it. That means cleanup fires on every exit
path — normal completion, or an `exit 1` on some later, unrelated failure
(e.g. the service-install step failing after the build already succeeded) —
not just a `rm -rf` tacked on after the one place the directory gets used,
which would've missed exactly those early-exit cases.

`install-openbsd.sh`'s two other `mktemp` uses (staging `pf.conf` and
`unbound.conf` edits before validating them) were already cleaned up on
every code path and needed no change; `install-linux.sh`'s `can_link_pam()`
probe directory likewise already removes itself on both its return paths.
The Windows installer doesn't use `/tmp` at all, so it's unaffected.

---

## v290 — 2026-07-09

Changed the stylized display name from `gravi[net]` to `[gravinet]`
everywhere it appears as a user-facing string: the CLI's own `--help` banner,
the web admin UI (page title, login card, top bar), the Windows/systemd
service descriptions, `README.md`, `getting-started.html`,
`docs/ARCHITECTURE.md`, and the Wintun attribution notice in
`third_party/wintun/`. The plain, unstyled `gravinet` identifier — module
path, binary name, CLI command, service name — is untouched; this only
affects the bracketed marketing form. The changelog's own "Origin" entry
(v0-era project rename from `meshvpn`) is left as `gravi[net]`, since that's
a historical record of what the name actually was at the time, not a
current reference to correct.

---

## v289 — 2026-07-09

QoS classes now mark their matching traffic with a DSCP codepoint on egress,
not just reorder it in the local shaper queue. Previously, `DSCP` on a
`QoSRule` was match-only (classify traffic *by* its existing mark) with
nothing on the output side — a class's priority never left the node, so a
qos'd flow looked identical to best-effort traffic to any router beyond
[gravinet]'s own hop, and a decrypted packet's DSCP field at the far peer
was whatever the sending OS originally set (typically 0), not a reflection
of the priority it was actually given.

Every class now marks its traffic with a standard Diffserv codepoint by
default, based on its position: EF (46) for the highest class, CS0 (0, the
ordinary "no particular treatment" default) for whichever class unmatched
traffic actually lands in (the network's configured default class, not just
the numeric middle — marking default traffic as anything other than the
standard "default" codepoint would be misleading), CS1 (8, the conventional
"scavenger"/below-default class) for the lowest, and the Assured-Forwarding
low-drop-precedence codepoints (AF41/AF31/AF21/AF11) for whatever falls in
between highest and default. A network's marks can be overridden per class
(`gravinet qos mark CLASS DSCP` / `qos unmark CLASS`, same shape as
`qos add`/`qos list`) to match an existing organizational Diffserv policy
instead of the default ladder.

The mark happens in `shaper.enqueue`, right after classification and before
the packet enters its priority queue — the same point that already reads
the inbound DSCP for rule matching, now also writes the outbound one. For
IPv4 it rewrites the ToS byte's DSCP bits (preserving the 2 ECN bits) and
recomputes just the header checksum, which is the only checksum that covers
those bits — the pseudo-header behind TCP/UDP checksums is unaffected by
DSCP/ECN, on either protocol. IPv6 has no header checksum, so no fixup
needed there. Re-marking a packet that already carries the target value is
a true no-op (checked byte-for-byte, not just skipped as an optimization) —
worth calling out because it's what keeps this cheap on the hot path
instead of walking every packet's header on every send regardless of
whether anything actually changed.

Same caveat as classification itself: this only fires when an up-throttle
is configured (QoS enabling one automatically if none is set), since that's
what creates the shaper queue packets pass through in the first place.

---

## v288 — 2026-07-09

Fixed the Windows installer/uninstaller hanging on stop instead of finishing,
which had been forcing people to kill the process by hand. The cause was
`Stop-Service -Force`: unlike `sc.exe stop`, it blocks until the SCM reports
the service Stopped and has no timeout parameter of its own, so a gravinet.exe
that didn't promptly acknowledge the stop control (rather than crashing or
erroring, which would already have surfaced as a message) just hung the
cmdlet, and with it the whole install-windows.ps1 / uninstall-windows.ps1 run,
forever.

Both scripts' service-stop logic now issues `sc.exe stop` (which only
requests the stop and returns immediately) and polls `Get-Service` itself for
up to 10s. If the service still hasn't reached Stopped by then, it kills the
gravinet.exe process directly instead of continuing to wait, so the script
can proceed to overwrite the binary / delete the service either way. The
existing 5-second wait loops in both scripts only ran *after* the blocking
`Stop-Service` call had already returned (or hung), so they never actually
protected against this; the fix is in the stop call itself, not the wait
that follows it.

---

## v287 — 2026-07-08

Latency's trend sparkline now covers 180s (18 readings) instead of 120s (12).
Getting there took more than just bumping `LATENCY_WINDOW_MS`: the chart was
a fixed 104px wide, so packing 50% more bars into the same space would have
shrunk each one from ~4.6px to ~3px \u2014 thin enough to hurt both legibility
and the per-bar hover target. `latencySparkline` now derives the chart's
width from a fixed per-bar allotment (`PX_PER_SLOT`, ~8.7px) times however
many readings it's given, so bar thickness stays constant (156px wide for 18
bars, same ~4.6px bars as the old 104px/12-bar chart) instead of the chart
staying a fixed size and the bars shrinking. That also means the chart will
auto-scale correctly if the window length changes again later, rather than
needing its width hand-tuned each time.

Rendered the actual generated 18-bar SVG (colors substituted for their CSS
values, same as the v285 check) through `rsvg-convert` before shipping to
confirm the bars stayed comfortably legible and the down-bar still reads
clearly against 18 neighbors instead of 12.

---

## v286 — 2026-07-08

Monitor → Latency's peer rows no longer reorder themselves on every 10s
refresh. They were sorted reachable-first-then-by-RTT, so a table would
visibly reshuffle every poll as RTT values fluctuated by fractions of a
millisecond or a peer's status flipped, making it hard to scan since a
peer's row wasn't in a stable place from one refresh to the next. Peers are
now sorted alphabetically by the same name shown in the table (hostname, or
the node ID if it has none), case-insensitively, and nothing else — a
peer's position is now fixed regardless of its current RTT or reachability.
Reachability is already visible without needing to group by it: the rtt
cell, the trend chart's red bars, and the row flash on a state change (v283)
all already show it.

---

## v285 — 2026-07-08

Latency's trend sparkline is now a small inline SVG bar chart instead of text
block characters (\u2581\u2582...\u2588), and the permanent "up 3m 30s" / "down 30s"
line underneath it is gone. Text block glyphs only have 8 discrete heights
tied to font-size, so "make it taller" wasn't really achievable with them —
switching to SVG gives real pixel-level height control (32px now, versus a
single line of monospace text before) and, more importantly, a proper fixed
canvas to place a down reading as a full-height, solidly-colored red bar
rather than mixing a small ✕ glyph in among differently-scaled value bars.

That resolves the actual question this was built to answer: yes, downtime
within the visible window is now visually unambiguous without reading
anything — a full-height red bar next to shorter blue ones doesn't need a
caption. What the permanent streak text carried that the chart alone can't
show is whether the current state extends *before* the visible 120s window
(a peer up for 10 minutes and one up for 90 seconds look identical once
they've both filled the window with "up" bars) — that's a real, deliberate
trade-off, not an oversight, and it's not fully gone: the exact streak
("up for Xm" / "down for Xs") is still there as a hover tooltip on the
chart, along with each bar's exact reading via a native SVG `<title>`. It's
just no longer permanently on screen for every row.

Colors follow the same convention the rest of the UI already uses: blue
(`var(--acc)`) for a normal reading, scaled to that peer's own min/max in the
window like before, and red (`var(--danger)`) exclusively for a miss — never
used for "high but still reachable," so red keeps meaning exactly one thing.
Rendered the actual generated SVG output (colors substituted for their CSS
values) through `rsvg-convert` to confirm the visual before shipping it,
rather than trusting the coordinate math alone.

---

## v284 — 2026-07-08

Latency's trend sparkline now covers 120s instead of 60s. The history length
was a hardcoded reading count (6); it's now derived from an explicit time
budget — `LATENCY_HIST_LEN = LATENCY_WINDOW_MS / LATENCY_POLL_MS` — so the
window is defined in seconds (`LATENCY_WINDOW_MS`) rather than in "readings,"
and it stays correct on its own if the poll interval (`LATENCY_POLL_MS`) ever
changes rather than silently drifting out of sync with what the in-tab hint
text claims. The poll interval itself is unchanged (still every 10s); only
how much of that history the sparkline keeps and displays doubled, from 6
readings to 12.

---

## v283 — 2026-07-08

Monitor → Latency now shows a per-peer trend, not just the latest reading: a
new **trend** column with a small sparkline of the last 6 readings, plus how
long the peer has held its current up/down state ("up 2m", "down 30s"). A row
flashes green or red for a couple of seconds the moment a peer's state
actually flips, so a drop-and-recover between two glances doesn't just read
as "unreachable" then quietly "12ms" again with no indication anything
happened.

This builds directly on v282's auto-refresh: now that the tab polls every
10s on its own, there was already a natural cadence to build a short history
from, purely client-side (no server or `/api/latency` payload changes —
snapshot pings). History is kept in a small per-(network, node_id) map,
module-level like `metricsMinutes`, so it survives navigating away and back
to the tab rather than restarting blank every visit; it's capped at 6
readings per peer so it can't grow unbounded over a long session.

The sparkline scales each peer to its own observed min/max in the window
rather than a fixed millisecond scale — a peer whose RTT sits in a tight
1-2ms band would otherwise render as a flat wall of full-height bars next to
one that's genuinely spiking, since both would round to "near max" against
an absolute scale. A missed probe renders as its own glyph (\u2715) rather
than a zero-height bar, since "no reply" and "0ms" are different things and
collapsing them loses exactly the information this feature exists to
surface. Verified the history/streak/flash-trigger logic and the sparkline's
min-max scaling directly in Node against synthetic multi-poll sequences
(first-reading suppression, drop/recover transitions, window capping) before
wiring it into the DOM-dependent render path.

---

## v282 — 2026-07-08

Monitor → Latency now refreshes itself every 10 seconds instead of requiring
a manual click — the Refresh button is gone, matching how the other
Monitor tabs (Metrics, live mesh-peers) already behave. Reuses the exact
start/stop lifecycle `infoMetrics` established: a module-level timer is
cleared and re-armed on entering the tab, and the load function checks
`state.section` on every tick so it self-cancels the moment the tab is left,
rather than leaking a timer that keeps pinging every peer in the background.

One small UX improvement beyond a literal "just add setInterval": the old
manual Refresh handler wiped the table to a "pinging…" placeholder on every
click, which was fine for a deliberate click but would have meant the table
blanking out for a few seconds — probing every peer isn't instant — every 10
seconds once automatic. The refresh cycle now only shows that placeholder on
the very first load; subsequent ticks leave the existing table visible while
the new probe round runs in the background and swap in fresh rows once it
completes, so the page doesn't flicker to empty on a timer the person didn't
ask for.

---

## v281 — 2026-07-08

Fixed the Linux installer's "built WITHOUT PAM support" warning firing even on
a binary that genuinely has PAM compiled in, whenever `/tmp` (or `$TMPDIR`) is
mounted `noexec` — a real, confirmed configuration on some hardened distros.
Reproduced end to end: `build_from_source` correctly builds with `cgo=1` (its
own `can_link_pam` probe only *compiles and links* a test program, never
executes it, so it's unaffected by `noexec`), but the installer's final
PAM_BUILT determination — `binary_has_pam "$SRC"` — ran against that binary
while it was still sitting in its `mktemp -d` staging directory, i.e. under
`/tmp`, *before* being installed to `$BIN`. `binary_has_pam`'s self-report
check (`"$1" version`) requires actually executing the binary, which fails
outright with "Permission denied" on a `noexec` mount; its `ldd` fallback
fails the exact same way and for the exact same reason — for a normal
(non-setuid) ELF, `ldd` determines shared-library dependencies by *executing*
the target with `LD_TRACE_LOADED_OBJECTS=1` set, which `noexec` blocks just as
hard, printing "not a dynamic executable" instead. Both failures are silent
(stderr discarded), so the installer had no way to tell "this binary lacks
PAM" apart from "this binary can't be exec'd from where it's sitting right
now" — and printed the scary warning either way.

`ldd`'s fallback is replaced with `readelf -d` (falling back to `objdump -p`
if `readelf` is unavailable): both read the ELF dynamic section straight off
disk, so they need no execute permission on the file at all, and still catch
the `libpam.so` `NEEDED` entry even on the release binary here, which is built
with `-ldflags "-s -w"` — `-s` strips the symbol table, not the dynamic
section, so the dependency is still visible. This is also just catching Linux
up to what the sibling installers already did safely: `install-freebsd.sh`
already used `objdump -p` and `install-macos.sh` already used `otool -L`,
neither of which executes the target either — `install-linux.sh`'s `ldd` was
the one exec-dependent check. Verified with a real `noexec`-mounted tmpfs and
an actual `CGO_ENABLED=1 -ldflags "-s -w"` gravinet build: before this fix,
`binary_has_pam` reported false (no PAM) on that binary every time; after,
`readelf -d` finds `libpam.so.0` and reports true, matching what the same
binary reports once copied somewhere executable.

---

## v280 — 2026-07-08

Added disk space utilization to the Monitor → Metrics tab, sitting directly
under the Memory graph — CPU, then Memory, then Disk, matching how those three
host-level series are grouped everywhere else (About, install warnings, etc.),
with the per-overlay-interface throughput cards following as before.

The new `readDiskUsedPct()` reader reports used space on `/` (root filesystem)
on Linux/macOS/FreeBSD/OpenBSD, or `C:\` on Windows — the same
per-platform-file structure `readCPUTotals`/`readMemUsedPct` already use, with
the same "unavailable rather than wrong" contract (`ok=false` if the read
fails, and the Disk series is just empty rather than showing a bogus number).
On Linux, macOS, FreeBSD, and OpenBSD this goes through `syscall.Statfs`
directly rather than shelling out to `df`: Go's standard library already ships
a typed, verified `Statfs_t` layout for all four, unlike the raw sysctl values
the CPU/memory readers on macOS/FreeBSD/OpenBSD have to parse by hand — so for
disk specifically, the syscall is both simpler *and* no less trustworthy than
a subprocess would be. Windows uses `GetDiskFreeSpaceExW`, the direct
analogue. The used-percentage convention matches `readMemUsedPct`: space
available to the caller (not reserved-for-root blocks) counts as available,
so the number tracks what `df`/Explorer would show rather than raw free
blocks. The collector, `/api/metrics` payload, and rolling 60-minute
retention/sampling are otherwise unchanged; `disk` is just a third sampled
series alongside `cpu` and `mem`, plus a `disk_path` field so the UI can title
the card "Disk (/)" or "Disk (C:)" correctly without guessing the OS
client-side.

---

## v279 — 2026-07-08

Fixed the "built WITHOUT PAM support" installer warning firing on Linux boxes
that could in fact build PAM. This is a sibling of the v276 fix but a distinct
bug: v276 corrected how the installer detects PAM in an *already-built* binary
(it now asks `gravinet version` for `pam=yes|no` — that machinery is intact and
verified). What was still wrong was the decision, *before* building, of whether
to build with cgo/PAM at all. The Linux installer gated that on `have_pam_hdr`,
a check that only looked for `security/pam_appl.h` at two hardcoded paths
(`/usr/include`, `/usr/local/include`). On any box where the header is present
but the compiler finds it via a different search path (a nonstandard prefix, a
distro's multiarch include dir), that test false-negatived — so the installer
built cgo=0 and then, correctly per its own logic, warned that PAM was missing.
The warning was honest about the binary; the binary was needlessly PAM-less.
(macOS and FreeBSD were never affected — they gate only on the compiler
existing, since their base systems ship the PAM header.)

Replaced the hardcoded-path check with `can_link_pam`: it actually compiles and
links a tiny program against `<security/pam_appl.h>` and `-lpam` using the same
compiler cgo will use — the only test that answers the real question. This
removes the false-negative (header in a nonstandard place) and the matching
false-positive (header present but libpam not linkable). When it still can't
build PAM, the probe now surfaces the compiler's own error (`PAM_PROBE_LOG`,
e.g. "fatal error: security/pam_appl.h: No such file" or "cannot find -lpam")
instead of a mystery, so a genuine no-PAM fallback is diagnosable. The
build-from-source gate and the prebuilt-fallback upgrade path both use it.

## v278 — 2026-07-08

Manager-side fail-fast when the local overlay is down — the follow-up flagged
at the end of v277, and the fix for the *confusing* half of the mcfed
incident. When you select a peer to manage, the web admin proxies the request
to that peer over the mesh (`handleProxy` dials the peer's overlay address).
If this node's own overlay interface is missing or down, the OS silently
routes that dial out the underlay instead, and the far end rejects it with the
baffling "connection arrived from &lt;underlay ip&gt;, which isn't inside any of
this node's overlay subnets" — a remote auth error that gives no hint the real
problem is a dead tun on the *manager*.

`handleProxy` now checks the local overlay data plane before dialing, via a new
`Backend.OverlayPathHealthy(dst)` / `Engine.OverlayPathHealthy`. It finds the
network whose subnet contains the target and confirms that interface is
present, up, and carrying our overlay address (the same kernel-truth check
`reconcileDataplane` uses, factored into `netState.dataplaneHealthy`). If it
isn't, the proxy returns `503` with a clear local message — "cannot manage
peers over the mesh: overlay interface mesh0 is not present on this node" —
right on the manager's own UI, instead of leaking the connection and bouncing
a cryptic remote 401 back. Note this is distinct from the existing
`OverlayReachable`, which only validates that the *target* is a structurally
valid overlay address (SSRF guard); the new check is about *this* node's
ability to actually carry the traffic. New test covers the down-overlay refusal
end to end; the SSRF, traversal, and body-limit proxy guards are unchanged and
still pass.

Not yet addressed from the incident's follow-up list: making the degraded
state visible in status/`IfaceInfo` (still reports healthy whenever `spec.Dev`
is non-nil), and the same fail-fast on the speedtest path (`speedtest.go` dials
peers over the overlay too and currently only gates on `OverlayReachable`).

## v277 — 2026-07-08

Self-healing overlay data plane: a node whose tun interface disappears at
runtime now rebuilds it on its own instead of silently running with a dead
data plane until a manual restart. This came out of diagnosing a live
incident where the manager node `mcfed` had no mesh interface at all — its
daemon was up and holding encrypted sessions over the underlay (so it
appeared healthy and populated the peer list), but every packet aimed at an
overlay address fell through to the default route and leaked out `wlan0`.
The visible symptom was a managed peer (`cush1`) refusing a management hop
with "the connection arrived from 192.168.193.26, which isn't inside any of
this node's overlay subnets" — `cush1` correctly rejecting an underlay-sourced
connection, because the manager's overlay-destined traffic never entered a
tunnel that didn't exist.

Root cause was structural: the only routine that ever rebuilt interface state
(`reassertOSState`) fired solely on suspend/resume detection, and
`maybeAssignAddress` short-circuits once `ns.self4` is set — it checks the
engine's memory that it once assigned an address, never whether the address,
interface, or route still exist in the kernel. So any non-resume loss (driver
reset, `ip link del`, VM/Wi-Fi churn) was never noticed, and every failing
`AddIPv4`/`AddRoute` on that path logged only at Debug, so it ran degraded in
silence for hours.

Two triggers now cover it (new `internal/mesh/dataplane.go`). `tunLoop`'s
blocking `Read` returns the moment the interface is destroyed; that's the
fastest signal, and it now drives an in-place rebuild with capped backoff
rather than returning and permanently ending outbound delivery. Keeping the
rebuild inside the one goroutine that owns the read means no extra goroutine
for `ns.wg` to track and no race with teardown's `Wait`. As a belt, a new
`reconcileDataplane` runs each maintenance tick and checks the interface
against the kernel via `net.InterfaceByName`: a stripped address/route on a
surviving interface is re-asserted in place, and a wholly missing interface is
handed back to `tunLoop` (by closing the live device so its `Read` unblocks).
Both are gated on a new injected `NetSpec.NewDevice` factory, so anything
without it (tests, embedders) keeps the exact prior behavior.

The live device is now held in an `atomic.Pointer` and read via `ns.dev()`, so
a rebuild can swap it lock-free without the hot write path or `tunLoop` ever
seeing a torn interface value; `spec.Dev` becomes only the seed and is never
mutated. Teardown (`RemoveNetwork`, `Stop`) sets a `dpClosing` flag under the
same mutex a rebuild uses to swap devices, closing the race where a rebuild
could install a fresh interface after teardown had already closed the old one
(which would hang `wg.Wait` forever). Four new tests cover the swap-and-reassert,
the teardown-abort guard, the missing-interface belt, and the no-factory no-op;
the loop-spinning and device-swap tests pass under `-race`.

Scoped deliberately to detection-and-recovery. Making the degraded state loud
and visible (status/`IfaceInfo` still reports healthy whenever `spec.Dev` is
non-nil) and failing fast on the manager side when the local overlay is down
are follow-ups, not in this version.

## v276 — 2026-07-07

Fixed the Linux (and macOS/FreeBSD) installers printing the "PAM is NOT
compiled in" warning — and defaulting the web admin's local-auth-mode
guidance — even on a binary that was built with cgo and genuinely has PAM
working. Root cause was in the installer script, not the Go build: each
`install-<os>.sh` builds via `CGO_ENABLED=$cgo` and initially sets
`PAM_BUILT=$cgo` correctly inside `build_from_source`, but a later "record
what the final binary can actually do" line unconditionally *overwrote* that
correct value by re-deriving it from inspecting the binary with
`ldd`/`otool -L`/`objdump -p` and grepping for `libpam` — a heuristic that
can be (and evidently was) fooled, e.g. by a toolchain that links libpam
statically, where the functionality is fully compiled in and working but
never shows up as a dynamic dependency. Fixed at the root: `gravinet
version` now self-reports `pam=yes`/`pam=no` (only the compiled binary
itself reliably knows what it was built with — see the new `PAMCompiledIn`
const, `true` only in `auth_pam.go`), and all three installers'
`binary_has_pam` now asks the binary this way first, falling back to the
old ldd/otool/objdump heuristic only for a binary built before this
self-report existed (so an old prebuilt binary is no worse off than
before, just not improved).

## v275 — 2026-07-07

Fixed the Peers table showing the OpenBSD node as a full FQDN
(`gn-openbsd.cush.local`) while every other peer showed a short name
(`gn-cush1`, `macmini`, etc). `os.Hostname()` conventionally returns a short
name on Linux, macOS, Windows, and FreeBSD, but OpenBSD's `/etc/myname` is
very commonly configured as a full FQDN, and `os.Hostname()` there just
echoes it back verbatim — that raw string was being gossiped mesh-wide as
this node's hostname with no normalization. Beyond the display
inconsistency, it would have broken bare-hostname resolution the same way
too (`ping gn-openbsd` wouldn't have resolved the way `ping gn-cush1`
does). Extracted the auto-detect path into a small `shortHostname` helper
that keeps only the first label, with a unit test covering the OpenBSD case
directly plus the already-short, single/multi-dot, and empty-string edges.
An explicit `hostname` set in config is left untouched — only the
`os.Hostname()` fallback is normalized, on the assumption that a value
someone typed into config was intentional.

## v274 — 2026-07-07

Documentation only, no code changes. Re-ran the changelog reconstruction with
past-conversation search enabled (the first pass had produced this file
using only what a single session's own context contained). Recovered and
added versions v202, v203, v207–209, v212's actual mechanism, v241–245
(replacing the vaguer "v244–245ish" entry with five precise ones), and
v259–262. v273 was searched for specifically and could not be recovered —
see "About this document" above.

## v273 — 2026-07-07

**Undocumented.** The version string jumps from 272 to 273 in
`cmd/gravinet/main.go` with no other change in the tree that a diff against
v272 reveals, and no past conversation surfaced by search describes what
this bump was for. Left as an acknowledged gap rather than a guess — if you
remember what this was, it's worth adding by hand.

## v272 — 2026-07-07

Renamed the `packaging/` directory to `install/`. Fixed every real reference
to the old path rather than just the directory itself: the functional `cp`
and `-d` checks in `scripts/build-release.sh` that bundle installers into a
release build, and the runtime hint string in `resolver_openbsd.go` that
tells an operator which script to run to enable unbound — both would have
silently pointed at a path that no longer existed. Verified by actually
running `build-release.sh` end to end, not just checking for stale text.

## v271 — 2026-07-07

Renamed the top-level directory inside the distributed source archive from
`gravinet-dns-forwarding` to `gravinet`. The Go module name was already
`gravinet` (module identity was never tied to the folder name), so this was
a packaging-only change.

## v270 — 2026-07-07

Removed a redundant closing note from the getting-started guide's HTML
rendering.

## v269 — 2026-07-07

Added `docs/getting-started.html` — later moved to the project root at the
user's request, alongside moving `SECURITY-FIXES.md` into `docs/` to make
room. The guide itself was built as a GUI-first walkthrough (most users run
the web admin, not the CLI), covering: the one-reachable-host requirement
for a mesh's seed node, install, opening the web admin (including a
tunnel-vs-temporarily-expose tradeoff for headless boxes, with Windows'
built-in OpenSSH client called out explicitly), creating a network,
joining via a generated token, enabling DNS forwarding, and dedicated
sections for hosts, routes, clustering (Managed/Manager mode), key
management and rotation, and each of firewall/NAT/QoS/shaping/seeds/peers/
bans/monitoring individually. Delivered as both Markdown (matches how
`README.md`/`ARCHITECTURE.md` already ship) and a self-contained styled
HTML file (opens natively in any browser on any OS, no tooling required).

## v268 — 2026-07-07

`gravinet network add/delete/enable/disable/join/join-token` no longer
restart the whole service. The engine's live-reload path (`reloadFn` in
`main.go`) already brings a new or newly-enabled network up without a
restart — building its TUN device and calling `AddNetwork` — and tears a
removed/disabled one down the same way; the web admin already used this
correctly. The CLI didn't: it always did a full OS-level service restart for
these operations, needlessly dropping every *other* network's sessions along
with the one actually being changed. Fixed to match the web admin's
existing live/restart split exactly — only `network subnet` (re-addressing a
live interface) still restarts, since that genuinely isn't something a hot
reload does. Verified end to end with a real running daemon: added a second
network via the CLI while the first stayed connected, confirmed via the
daemon's own log and an unchanged process ID that it came up live.

## v267 — 2026-07-07

Real socket-level tuning (`SO_REUSEADDR`, `IPV6_V6ONLY`, and don't-fragment/
path-MTU-discovery socket options) for Windows, FreeBSD, and OpenBSD — all
three previously fell through to an explicit no-op stub
(`internal/transport/control_other.go`) while Linux and macOS had real
implementations. The missing don't-fragment guarantee mattered beyond a
minor optimization: without it, path-MTU discovery can settle on a size that
only worked because the kernel silently fragmented an oversized probe,
which then black-holes on any real path that doesn't tolerate fragments.
FreeBSD got real `IP_DONTFRAG`/`IPV6_DONTFRAG`; OpenBSD got `IPV6_DONTFRAG`
only (its IPv4 stack has no such option at all — confirmed against `ip(4)`'s
documented option list — so the existing macOS defensive PMTU-ceiling
mechanism was extended to cover OpenBSD's IPv4 gap too, for a different
underlying reason than macOS's); Windows got both, hand-defined from
Microsoft's own `ws2ipdef.h` since Go's syscall package doesn't export them
for that platform. Also fixed a real bug in `scripts/build-release.sh`: it
only ever granted cgo/PAM eligibility to `darwin` or `linux`, so a FreeBSD
release binary never got PAM support even when built natively on a FreeBSD
host with PAM headers present — despite `auth_pam.go` explicitly supporting
FreeBSD. Fixed to treat FreeBSD the same as Linux (native build + PAM
headers actually found).

## v266 — 2026-07-07

Fixed the web admin showing a dead-end login screen when a proxied call to a
managed peer failed, even though the local session was perfectly fine. A
401 from `/api/proxy` (the peer rejecting the management hop — session
expired *there*, Manager-mode gossip hadn't reached it yet, or it's no
longer Managed) was being treated identically to the local node's own
session expiring, which just looped back to the same unhelpful screen since
re-logging in can't fix a peer-side rejection. Now falls back to the local
node and surfaces the peer's actual reason instead.

## v265 — 2026-07-07

Found and fixed the real root cause of the Managed/Manager toggle race (see
v264): `syncClusterModeRows()` — the function that disables the toggles and
syncs their displayed values — was being called from `secSettings()` before
the Cluster card was actually attached to the live DOM, making
`document.getElementById` return null and the call silently do nothing,
every time. This meant the toggles were never reliably disabled when
viewing a remote peer, independent of any timing race — v264's fix closed
one specific window but didn't address this. Fixed by attaching the card to
the document before syncing rather than after. Verified with a real DOM
(jsdom) rather than by inspection alone.

## v264 — 2026-07-07

First attempt at fixing the Managed/Manager toggle race: made
`syncClusterModeRows()` run synchronously the instant a peer is selected in
the dropdown, instead of waiting for an async refresh to complete. This
closed the specific window where switching to a remote peer while already
on the Settings page left the *previous* render's toggle live and
clickable. It turned out not to be the actual bug behind the report that
prompted it (see v265) — switching peers on a *different* tab and then
navigating to Settings was still broken, because of a separate,
independent issue.

## v263 — 2026-07-07

Full cross-platform kernel NAT parity. Previously, `internal/netfilter`
only had a real backend on Linux (`nft`/`iptables`); every other platform
got an explicit no-op stub, even though the config layer had no platform
gating at all — you could configure NAT rules on any OS with no error,
they just silently never applied anywhere but Linux. Added real backends
for macOS, FreeBSD, and OpenBSD (all via `pf`, using platform-appropriate
anchor strategies — macOS's default `com.apple/*` wildcard hook needs no
`pf.conf` edit at all, while FreeBSD/OpenBSD need an explicit,
idempotently-managed anchor hook since neither has that convention) and
Windows (via WinNAT/`New-NetNat`, which is honestly narrower than the
others — no fixed-address SNAT, no address-only DNAT — so those specific
cases are reported back as unsupported rather than silently dropped).
Verified by actually cross-compiling all five targets and running the new
pure rule-generator unit tests, not just by reading the code.

## v262 — 2026-07-07

Fixed the web admin's Capture tab silently ignoring the header peer dropdown
and always showing the manager's own interfaces. `/api/capture/interfaces`,
`/start`, `/stop`, `/clear`, and `/packets` had all been swept into the
frontend's `LOCAL_API` list (the "never proxy this" list) at the same time
as the unrelated Managed/Manager local-only exception, under the same
reasoning — but capture isn't session state, it's a peer's interface. Removed
all five so they proxy through `/api/proxy?node=...` like every other tab.
The `.pcap` download button needed its own separate fix since it's a raw
`window.location` navigation, not a `fetch` call. That in turn surfaced a
second real bug: `handleProxy`'s generic response copy caps every proxied
response at 8 MiB, but the capture buffer holds up to 32 MiB — a
well-populated `.pcap` export would have silently truncated the moment it
started proxying. Gave `/api/capture/pcap` a cap sized to the buffer's real
worst case instead.

## v261 — 2026-07-07

Fixed OpenBSD's brand-new Metrics tab (v259/v260): memory worked but CPU and
network both came back empty. Root causes were two separate parsing bugs,
not one — OpenBSD's `sysctl kern.cp_time` joins its array output with
commas (`2391,0,1987,60,117,976656`), unlike FreeBSD's space-separated
equivalent, so splitting on whitespace alone left the whole value as one
unparseable token; and `netstat -ibn`'s link-layer header row prints as
`<Link>` on OpenBSD rather than `<Link#1>`, plus point-to-point interfaces
(like gravinet's own tun devices) can have no such row at all. Both fixes
landed in the build-tag-free `metrics_openbsd_parse.go` so they're
unit-testable without an OpenBSD box, including a regression test built
from a real `kern.cp_time` sample pulled from a live OpenBSD bug report.

## v260 — 2026-07-07

Added the OpenBSD packet-capture backend (`/dev/bpf`), closing out the other
half of the platform-parity audit from v259. Deliberately verified against
real OpenBSD header definitions rather than assumed-compatible-with-FreeBSD
guesswork, since a wrong `bpf_hdr` byte offset corrupts captured packets
silently instead of failing loudly.

## v259 — 2026-07-07

Added the OpenBSD Metrics tab backend (CPU/memory/interface throughput),
the other stub a platform-parity audit had turned up. Shells out to stable
documented tools rather than hand-rolling kernel struct parsing: `sysctl
kern.cp_time` for CPU, `top -b -d1` for memory (OpenBSD's memory stats come
back as one opaque `uvmexp` struct with no individually-named sysctl leaves
the way FreeBSD exposes `vm.stats.vm.v_*_count`, so top's own parsing of it
was the only clean path — field mapping traced against OpenBSD's own
`usr.bin/top/machine.c` source to get "used" right, excluding reclaimable
buffer cache), and `netstat -ibn` for interfaces. The actual parsing logic
lives in a separate, build-tag-free file mirroring the existing
`metrics_darwin_parse.go` pattern, so it's unit-testable without a real
OpenBSD host. (See v261 for two parsing bugs found shortly after this
shipped.)

---

## Earlier history (best-effort, reconstructed from search)

Everything below predates this conversation and is assembled from whatever
detail past conversations happened to surface. Version numbers are cited
only where a past conversation explicitly stated one.

**v254** — Fixed the web admin's Latency tab always reporting "no reply" for
an OpenBSD peer even though it was perfectly reachable. `pingRTT` fell
through to the Linux/default case and ran `ping -c 2 -W 1 addr`, but
OpenBSD's `ping(8)` doesn't accept `-W` the way Linux's does — given the
correct OpenBSD flags after checking the man page.

**v245** — OpenBSD's installer made `unbound` the default local resolver
instead of opt-in (matching FreeBSD's `local-unbound`, which already
defaulted on) — `--no-unbound` is now the opt-out flag.

**v244** — Added OpenBSD conditional DNS forwarding (`resolver_openbsd.go`),
driving unbound forward zones through `unbound-control` — the same
mechanism the FreeBSD backend already used, since OpenBSD ships unbound in
base with identical control commands. Closed the one remaining feature gap
from the v241 port.

**v243** — Two real bugs in `install-openbsd.sh` found after the v241 port
shipped: a `` `gravinet genpass` `` backtick inside the install banner's
`cat <<EOF` heredoc was executing at install time and silently generating a
local web-admin user nobody asked for (removed the whole clause); and the
banner's firewall note just *told* the operator to open the underlay port
themselves instead of doing it — the installer now appends an idempotent,
`pfctl -nf`-validated `pass in` rule to `/etc/pf.conf` and reloads pf,
rather than only talking about it.

**v242** — Version-only bump at the user's request, no code change — used
to force an already-installed OpenBSD host past the installer's "already up
to date" skip while stale files were cleaned up by hand on that host.

**v241** — OpenBSD port lands (from a v240 baseline): `tun_openbsd.go` for
the datapath, `ipfwd_bsd.go` (a shared FreeBSD/OpenBSD file, replacing the
FreeBSD-only `ipfwd_freebsd.go`) for IP forwarding, rc.d/`rcctl` service
integration, `bsd_auth`/`login_passwd(8)` web-admin login (no PAM/cgo
needed), and `install-openbsd.sh`/`uninstall-openbsd.sh`. Explicitly
untested on real OpenBSD hardware at the time (no OpenBSD CI host
available) — every subsequent OpenBSD entry in this changelog is a fix
found once it actually ran on one.

**v232** — Fixed a PowerShell parse error (`Unexpected token '}'`) in
`install-windows.ps1` caused by a brace mismatch.

**v218** — Added a restart-on-crash hook for the service on platforms where
it's meaningful; documented that Windows doesn't get the same treatment
since the SCM integration always reports a clean stop regardless of cause,
so a configured "restart on failure" wouldn't fire and spawning a detached
replacement would desync the SCM's view of the service from reality —
falls back to the existing best-effort in-process behavior there instead of
shipping something subtly wrong.

**v212–214** — Removed the standalone "Search domains" list added in v209
in favor of deriving search domains automatically from each network's own
enabled DNS-advertise entries (so an advertised domain doubles as a search
suffix, with no separate list to keep in sync) — simpler, but it introduced
the next bug: fixed bare-hostname DNS resolution (`ping media1` failing
while `ping media1.cush.local` worked) for a domain this node only *learned*
from a peer via `dns_sync` rather than advertising itself, since search
domains were being derived only from locally-advertised entries; every
forwarded-for domain, advertised or learned, now doubles as a search suffix.
Also in this range: fixed distributed key-expiry not propagating to peers;
fixed a banned/unbanned host needing a manual restart to rejoin the mesh
after being unbanned.

**v209** — Added per-network DNS search domains as its own feature: Linux
via `resolvectl domain` (merged correctly with any existing routing
domains), Windows via the interface's connection-specific DNS suffix (only
the first configured domain applies — a genuine Windows limitation, surfaced
as a clear error for the rest), and an explicit "not supported here"
rejection on macOS/FreeBSD instead of silently doing nothing. Superseded by
the redesign in v212 above.

**v207–208** — A macOS host with a `/32` mistakenly pinned in its
`address4` config had that netmask applied faithfully — and silently — to
the live interface, which quietly dropped its `/24` route. Fixed on both
ends: `NetworkSetAddress` now rejects a mismatched prefix length outright
(also catches an overly-broad `/16` and an IPv6 `/128`), and the
address-(re)assignment paths (DAD, and post-sleep/resume state reassertion)
now explicitly call `AddRoute` afterward, so even a bad address slipping in
some other way self-heals on the next reapplication rather than staying
silently wrong.

**v203** — The control socket (`gravinet status`, `ban`, `managed`, etc.)
had defaulted to a Linux-only `/run/gravinet.sock` path; macOS and FreeBSD
now get `/var/run/gravinet.sock`, and Windows gets a `%ProgramData%`-based
path.

**v202** — `RemoveNetwork` now clears a network's `/etc/hosts` block and DNS
forwards immediately when the network is disabled live, instead of leaving
stale entries in place until the whole daemon next restarts.

**~v191–192** — Added a "distributed" tickbox next to a newly generated
join key that pushes it live to every connected peer over the mesh itself
(no copy/paste); added a key-label column to the Peers table showing which
key is currently authenticating each session.

**v188–189** — Removed the NSIS-based graphical Windows installer in favor
of a minimal `install.bat` that sets an execution-policy bypass for just
that process and hands off to `install-windows.ps1`; fixed the Bans table
so a ban only the *origin* node can lift shows a genuinely disabled,
grayed-out checkbox (with a tooltip naming who can lift it) on every other
node, instead of an interactive-looking control that silently couldn't do
anything; added the "reset" button on the Networks page (tick a network,
flush and reconnect all its peer/seed connections) with matching `=`/token
button styling to match the existing `+`/`−` toolbar.

**v182** — Fixed FreeBSD's default config path being wrong, which broke
`network join <token>` on FreeBSD specifically.

**v176–177** — Fixed FreeBSD network enable/disable: `SIOCIFDESTROY`'s
return value was being ignored, so a failed interface teardown (including a
plausible race right after closing the controlling file descriptor) failed
silently and left a stale interface behind, which then blocked a
subsequent enable from renaming onto that same name.

**v174–175** — Metrics and packet-capture FreeBSD backends already existed
and were fully implemented, gated by real capability checks rather than a
hardcoded platform string — the only actual bug was stale "Linux hosts
only" wording shown in the frontend even when the backend was genuinely
available.

**v145–146** — Added DNS conditional forwarding as a feature (gossiped
per-domain forwarding rules — this is what the project's original
distribution name, `gravinet-dns-forwarding`, refers to); reorganized the
web admin's navigation into grouped sections (Mesh, Network/Traffic,
Naming, Monitor, Info) instead of a flatter tab layout.

**Pre-integer versioning** — Wintun (the Windows TUN driver) was switched
from a loose sibling DLL to an embedded resource (`go:embed`, per
architecture), written out to a per-user cache directory on first use and
loaded from there — removing the loose-file/DLL-search-order fragility of
shipping it alongside the binary.

**Origin** — The project began under the internal working name `meshvpn`
and was renamed to `gravinet` / `gravi[net]` early in its development,
including the module path, binary name, and every user-facing string.
