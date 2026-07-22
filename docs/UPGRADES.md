# Upgrading a gravinet node

`gravinet upgrade` replaces this node's own binary, in place, with a health
check that reverts it automatically if the new one can't rejoin the mesh or
crash-loops. As of v403, this is **per-node and local-only, no key
required** — a real change from how this document used to describe it, and
worth being upfront about since the two models don't blend:

- There is no remote trigger for an upgrade at all, from anywhere, under any
  configuration. Not from a Manager peer, not through the peer picker in the
  header, not even from a node that's otherwise fully entitled to administer
  this one's config and firewall. There is no setting that turns this back
  on: every upgrade endpoint checks, first, before anything else, that the
  request is coming from a real session logged into *this* machine. The web
  admin's Upgrade page reflects this visibly, not just underneath: select a
  peer in the header and the upload controls grey out with an explanation,
  rather than silently applying to whichever node you're actually logged
  into while looking like it's targeting the selected one.
- No release key is required to use it. A node with nothing configured under
  `upgrade.trusted_keys` accepts a structurally sound, unsigned artifact —
  safely, specifically *because* the point above means the only thing that
  can ever hand it one is a session already logged into that exact node.
  Signing is still available if you want it (see below), but it's an opt-in
  layer on top, not a prerequisite for the feature to work at all.

The design is still shaped by the same fact that made the old model careful:
**gravinet is the network you would otherwise use to fix a node you just
broke.** That's why the self-protection machinery below — preflight, the
confirm window, automatic revert, keeping a rollback copy — is unchanged.
What changed is *who* can ever reach the button in the first place.

---

## Trust: none required by default, one optional layer on top

| | Default (no `trusted_keys`) | With `trusted_keys` configured |
|---|---|---|
| Who can trigger an upgrade | A session logged into this exact node. Nobody else, ever. | Same. |
| What's required of the artifact | Structurally valid (right fields, a real digest) | A valid Ed25519 signature from a key in `trusted_keys` |
| Where the release key lives | N/A — there isn't one | Offline. Never on a node. |

The right-hand column still exists because signing verifies something the
local-only guarantee doesn't: that a specific artifact is one you actually
built and meant to ship, not just that whoever's uploading it is sitting at
this node's own console. If that distinction matters to you — multiple
people with local admin access, wanting a paper trail on what's actually
been installed — turn it on:

```sh
gravinet upgrade genkey          # on your laptop / build host, NOT on a node
```

Put the **public** half in the node's config, keep the private half off any
mesh node entirely:

```json
"upgrade": {
  "trusted_keys": ["103e7b3b…"],
  "confirm_seconds": 90
}
```

With that set, the node stops accepting the plain binary-only upload and the
build-from-source upload (there's no coherent way to check a signature
against either), and goes back to requiring a signed manifest alongside the
binary — exactly the old flow:

```sh
go build -o gravinet ./cmd/gravinet
gravinet upgrade sign -bin ./gravinet -key "$GRAVINET_RELEASE_KEY" -notes "v403"
# -> gravinet.json (the signed manifest)
```

`sign` probes the binary (`gravinet version`) rather than trusting flags, so
a manifest can't claim amd64 over an arm64 build. Cross-built artifacts need
explicit `-os`/`-arch`.

---

## Getting a new binary onto a node

**Default (no `trusted_keys`): one file, one button, source only.**
Web admin → Info → Upgrade → Upload: pick the project's source archive
(`.tgz`/`.tar.gz` — same format, whichever your copy happens to be named)
and click **Upgrade**. It's built here — `go build`, cgo/PAM first, falling
back to a static build if that fails, the same as the platform installers —
then identified (`gravinet version`, the same probe `upgrade sign` uses on
a build host), applied, and this node restarts into it. All of that behind
one confirmation, one click. A pre-built binary isn't accepted in this mode
at all; if you have one and want to skip the build step, that's what
signed mode (below) is for.

The GUI is intentionally upload-and-apply only — it doesn't show what's
sitting in the local artifact store, what phase a previous upgrade is in,
or a way to roll one back; there's no separate "stage now, apply later"
step to look at, and no status display that needs to stay in sync with
what's actually happening. If a click is interrupted partway (built or
uploaded, but the apply step didn't finish), the file is still in the
store; finish it from the CLI: `gravinet upgrade list` to see it,
`gravinet upgrade apply -id ID` to apply it. `gravinet upgrade rollback`
is the same story — always available, CLI-only. The daemon still refuses a
second real apply while one is already mid-trial (see below) regardless of
what the page shows; that guard is server-side and isn't something the UI
needs to display to be true.

Building from source needs a Go toolchain on `PATH` on that node (a C
toolchain + PAM headers too, for PAM web-admin auth in the result) — this
node does not fetch or install either on its own, since doing that from an
HTTP handler is a materially bigger thing than "compile the source I gave
you." A missing toolchain fails with a clear message instead.

There's no CLI equivalent of this today: `gravinet upgrade stage` always
requires a manifest (`-manifest PATH`, or `<bin>.json` next to the binary)
and fails without one, signed or not — it doesn't have the GUI's build-and-
auto-identify path. Building from source and applying it with no key is
currently a web-admin-only capability.

**With `trusted_keys` configured: binary + signed manifest, same as before.**
The single-button flow above disappears — there's no coherent way to check a
signature against uploaded source, so build-from-source isn't offered at
all, and the binary upload goes back to requiring its manifest alongside it:

```sh
go build -o gravinet ./cmd/gravinet
gravinet upgrade sign -bin ./gravinet -key "$GRAVINET_RELEASE_KEY" -notes "v405"
# -> gravinet.json (the signed manifest)
```

`sign` probes the binary (`gravinet version`) rather than trusting flags, so
a manifest can't claim amd64 over an arm64 build. Cross-built artifacts need
explicit `-os`/`-arch`. Upload binary + `.json` manifest together (both
fields are back in the form once `trusted_keys` is configured), or
`gravinet upgrade stage -bin ... -manifest ...` from the CLI.

---

## Why this is per-node now, not fleet-wide

This used to be a mesh-distributed rollout: stage once on a Manager, and it
would fan out to every Managed peer in waves, each one pulling the binary
from whichever peer already had it. That's gone. The reasoning, briefly: a
Manager peer authorized to administer a Managed node's config and firewall
was, by the same authorization, able to drive that node's upgrades too —
stage an artifact, apply it, roll it back — with only the release-key
signature check standing between "authorized peer" and "arbitrary code
execution as root." For a node choosing to trust no release keys at all (now
the default), that check doesn't exist, so there'd be nothing left standing
between a Manager peer and unsigned root code execution on every node it
manages. Closing that meant closing remote upgrade triggering entirely, not
narrowing it — hence local-only, full stop, regardless of Managed/Manager
state or whether keys are configured.

The practical cost, worth having in view before you're mid-upgrade on node
six of nine: there's no "stage once, push to the fleet" anymore. Each node's
upgrade happens through a session logged into that node, with whatever file
you're uploading reachable from that specific browser.

## There is no fleet view or rollout anymore

Earlier versions had Fleet and Rollout cards on the Upgrade page, and
`gravinet upgrade fleet` / `rollout` on the CLI: stage once on a Manager,
watch a canary wave, then a rollout that fanned the binary out peer-to-peer.
All of it — the UI, the CLI subcommands, the peer-to-peer artifact-serving
endpoint, the manifest+sources apply variant a Manager used to push with —
has been removed outright, not just made unreachable. Once upgrades became
local-only (the previous section), none of that machinery could still do
anything: a rollout that stops at the first target, forever, isn't a feature
running in a degraded mode, it's dead code with a UI on top of it. Deleted
rather than left in place, on the reasoning that a control sitting on the
page that cannot work is worse than no control at all — it's something to
explain, not something a smaller deployment benefits from carrying.

If per-node is ever not enough — a fleet genuinely too large to upgrade one
browser tab at a time — that's a reason to reach for external tooling built
for exactly that (config management, an orchestrator, a script driving the
CLI over SSH), not a reason to bring this back.

---

## What a node does before it swaps anything

Unchanged from before, and still worth being precise about, since these are
what actually stand between a bad binary and a bricked node — the local-only
change is about *who* can start this, not what it checks once started:

- **arch/os match** against the running node;
- **the candidate is executed** (`gravinet version`) — catches wrong
  architecture, a truncated file, and a missing `libpam`, none of which a
  digest alone can see;
- **its self-reported version is recorded** against what's actually running;
- **PAM parity** — replacing a `pam=yes` binary with a `pam=no` one starts
  cleanly and then can't log anyone in. Refused unless `-allow-pam-downgrade`;
- **`gravinet selftest -config …`** — the candidate loads *this node's
  actual* config. A version that tightened validation or renamed a field can
  be genuine, right architecture, and still crash-loop on a node whose only
  remaining management path was the mesh it's no longer joining — this is
  what catches that before the swap, not after.

Only then: two renames on one filesystem — `gravinet` → `gravinet.prev`,
the candidate → `gravinet`. No window where a half-written binary occupies
the installed path.

## If the new binary is bad

The node rescues itself; there is no manager to do it for you, by design,
same as before:

- **Crash loop.** Every start increments a boot counter, written before
  anything that can fail. After 3 failed starts the node restores
  `gravinet.prev` and restarts into it.
- **Starts, but the mesh doesn't come back.** The node records how many peers
  it had before the swap. If it had peers and has none when the confirm
  window closes, that's a failed upgrade regardless of how clean the process
  looks, and it reverts.
- **A regression no health check can see.** `gravinet upgrade rollback` — the
  backup is kept even after a clean commit, because the 3am regression is the
  one nobody's automated check caught.

The one failure this can't catch is a binary that dies before Go's runtime
reaches `main()` — a corrupt file, a missing shared library. That's exactly
what "the candidate is executed" in preflight refuses to let through the swap
in the first place.

## Inspecting

```sh
gravinet upgrade status     # this node: version, phase, boots, peers
gravinet upgrade list       # what's staged here
```

## Bootstrapping

A node running a version older than v403 doesn't have any of the above yet —
its Upgrade page still describes and requires the old signed/mesh-distributed
model, and a node with no `trusted_keys` configured on an old version simply
refuses every upgrade rather than falling back to local-only-unsigned (that
fallback is itself part of what v403 added). The first hop onto v403 has to
happen by whatever means you'd use for any upgrade on an older version — the
platform installers, rebuilding from source directly, or the old signed flow
if you already had keys set up. From v403 on, the page and this document
match what the code actually does.
