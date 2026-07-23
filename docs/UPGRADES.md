# Upgrading a gravinet node

`gravinet upgrade` replaces this node's own binary with one **built here, from
source you supply**, behind a health check that reverts it automatically if the
new binary can't rejoin the mesh or crash-loops.

Source is not one option among several. It is the only one:

- **gravinet publishes no prebuilt binary, for any platform.** Every fresh
  checkout is source and nothing else, so a binary upload had no supply to draw
  on even when it existed.
- **A binary is only ever valid for the platform that built it.** A mesh
  routinely spans Linux, FreeBSD, OpenBSD, macOS and Windows at once. One
  source archive serves all of them; one binary serves a fraction of them and
  bricks the rest if you get the targeting wrong.

So there is nothing to sign, nothing to cross-compile, nothing to stage, and no
artifact shelf to inspect. You hand a node an archive; it compiles it, checks
the result against its own config, swaps it in, and watches it.

---

## Upgrading one node

**Web admin → System → Upgrade → Upload.** Pick the source archive (`.tgz`,
`.tar.gz` or `.zip` — the format is detected from the file's content, not its
name, so GitHub's "Download ZIP" works as-is) and click **Upgrade**. One
confirmation, one click, no staging step in between.

**CLI:**

```sh
gravinet upgrade apply -src ./gravinet-src.tgz
gravinet upgrade apply -src ./gravinet-src.tgz -dry-run   # build + preflight, no swap
gravinet upgrade status
gravinet upgrade rollback
```

The build runs inside the daemon in both cases, so the terminal and the browser
drive one implementation rather than two that can drift.

### What the node needs

A Go toolchain. The platform installers (`install/install-*.sh`,
`install-windows.ps1`) put one there via `ensure_go`, along with a C toolchain
and PAM headers via `install_build_deps`, so a node installed normally is
already equipped. A node **does not** fetch or install a toolchain on its own:
doing that from an HTTP handler is a materially bigger capability than
"compile the source I gave you". If `go` is missing the upgrade fails with a
clear message instead.

`go` is found on `PATH` first, then at `/usr/local/go/bin` (where the
installers unpack the go.dev tarball) and `/usr/local/bin` (where FreeBSD's
`pkg` and OpenBSD's `pkg_add` put it). That fallback matters because the build
runs inside the daemon, which a service manager starts with its own minimal
environment — not the shell PATH you had when you installed Go.

---

## Upgrading a fleet

One upload, one archive, every platform. Each peer compiles its own native
binary from the same bytes.

**Once per peer, on that peer:** turn on **Accept Manager-pushed upgrades**
(`upgrade.accept_manager_upgrades`). It is off by default, and it is
deliberately local-only — the switch that authorizes remote upgrades can never
itself be flipped by a remote peer.

**Then, from the Manager:** System → Upgrade → **Push to managed peers**. Pick
the archive, tick the peers, push. Results come back per peer.

A pushed archive is built and applied on a peer only if **all** of these hold,
none of which the Manager controls:

| | |
|---|---|
| The peer opted in | `accept_manager_upgrades`, off by default |
| The caller is a **directly-connected** Manager | a live handshake, not a gossip-labeled one — `IsManagerNeighborAddr` |
| The bytes match | SHA-256 over the archive, declared before it and checked as it lands |
| It compiles there | with that peer's own toolchain |
| It survives preflight | the same checks a local upgrade runs, below |
| It stays healthy | the same confirm-or-rollback guard, below |

Note what a Manager never gets, even with the opt-in on: it does not supply
executable bytes. It supplies source, which the peer chooses to compile.

### Practical notes

- **The Manager does not upgrade itself as part of a push.** Do the peers
  first and the Manager last — it holds a connection open per peer for the
  whole build-and-swap, so upgrading itself mid-push kills every in-flight
  upgrade.
- **Four peers build at a time.** Unbounded fan-out means every node in the
  fleet running a Go build simultaneously, which on small boxes is a
  self-inflicted outage.
- **Expect the mesh view to churn.** Every peer drops its session while
  restarting and reappears during its confirm window. Nodes vanishing from the
  peer list mid-rollout is the process working.
- **There is no CLI push.** Like the other fleet actions, pushing is driven
  from the web admin of the node you're on.
- If per-node is ever not enough, that's a reason to reach for tooling built
  for fleets (config management, an orchestrator, a script driving the CLI over
  SSH), not a reason to grow this.

---

## What a node does before it swaps anything

The build is only half of it. Compiling successfully proves the source is
valid; it does not prove the result will run *here*.

- **The candidate is executed** (`gravinet version`) — catches a truncated
  build, a missing `libpam`, and anything else that produces a file that cannot
  start. This is the single most valuable check, because the failures that
  actually brick nodes are invisible to a digest: the digest of a broken binary
  is perfectly valid.
- **PAM parity** — replacing a `pam=yes` binary with a `pam=no` one starts
  cleanly and then can't log anyone in. Refused unless `-allow-pam-downgrade`.
  This is a live risk here, because a missing C toolchain makes the build fall
  back to `CGO_ENABLED=0` and produce exactly that binary.
- **Downgrade check** — refused unless `-allow-downgrade`.
- **`gravinet selftest -config …`** — the candidate loads *this node's actual*
  config. A version that tightened validation or renamed a field can compile
  perfectly and still crash-loop on a node whose only management path was the
  mesh it's no longer joining. This is what catches that before the swap.

Only then: two renames on one filesystem — `gravinet` → `gravinet.prev`, the
candidate → `gravinet`. There is no window in which a half-written binary
occupies the installed path.

## If the new binary is bad

The node rescues itself; there is no manager to do it for you, by design.

- **Crash loop.** Every start increments a boot counter, written before
  anything that can fail. After 3 failed starts the node restores
  `gravinet.prev` and restarts into it.
- **Starts, but the mesh doesn't come back.** The node records how many peers
  it had before the swap. If it had peers and has none when the confirm window
  closes (`confirm_seconds`, default 90), that's a failed upgrade regardless of
  how clean the process looks, and it reverts.
- **A regression no health check can see.** `gravinet upgrade rollback` — the
  backup is kept even after a clean commit, because the 3am regression is the
  one nobody's automated check caught.

The one failure this can't catch is a binary that dies before Go's runtime
reaches `main()`. That's exactly what executing the candidate during preflight
refuses to let through the swap in the first place.

---

## Configuration

```json
"upgrade": {
  "confirm_seconds": 90,
  "accept_manager_upgrades": false,
  "state_dir": "/etc/gravinet/upgrades"
}
```

`state_dir` holds the guard's `state.json` — the record that lets a node back
out a bad binary after a restart — and is where uploads are spooled and builds
run. Created 0700. It was called `store_dir` back when it also held staged
binaries; that spelling is still read, so a node that set it keeps its
directory (and therefore its in-flight upgrade state) across the rename.

There is no `trusted_keys` and no `keep_artifacts`. Signing covered a built
artifact's digest, which is meaningless when no built artifact is ever
distributed; retention counted staged binaries, of which there are now none.

## A note on what source-only costs

Each node's binary is its own build, so "every node is running byte-identical
code" is no longer a property you can check. `-trimpath` is set, which helps,
but reproducible builds would be the thing that actually recovers it if that
ever matters for audit.

What you get in exchange is that one archive upgrades a mesh of five different
operating systems, with no signing ceremony, no cross-compilation, and no
possibility of sending arm64 to an amd64 box.
