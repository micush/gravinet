# gravinet — Feature Overview

**gravinet is a self-hosted mesh VPN.** You run it on each machine, point them
at each other, and they form a private, fully encrypted network that every
node can reach directly — no cloud service, no account, no subscription, and
no central control plane that can see your traffic or take the whole network
down with it.

---

## Set up in minutes

- **Five platforms, no dependencies.** Linux, Windows, macOS, FreeBSD, and
  OpenBSD all run the same program with no runtime dependencies to chase down.
- **Installers that just work.** Each installer drops the binary in place,
  registers it as a proper background service (systemd, launchd, or a Windows
  service), and starts it. Re-running the installer upgrades in place. On
  Windows you can just double-click — it elevates itself.
- **Builds itself if it has to.** Installing from source with no prebuilt
  binary? The installer fetches a Go toolchain, compiles, and installs it for
  you — no manual build steps.

## The network forms itself

- **Give one node a friend, get a whole mesh.** Point a node at a single seed
  address and the rest is automatic: every node learns about every other and
  connects directly — a true full mesh, not hub-and-spoke.
- **Self-addressing.** Each node picks its own address from the network's subnet
  and checks that nobody else has it before claiming it. No IP spreadsheet to
  maintain.
- **Self-healing.** Dead peers are pruned automatically, unreachable seeds back
  off and retry, and connections follow a peer as its real-world IP changes.

## Connects through the hard networks

Getting a direct line between machines that "can't" reach each other is the whole
point of a mesh.

- **NAT traversal and roaming** so nodes behind home routers and mobile
  connections still link up and stay linked as addresses change.
- **Automatic router configuration.** gravinet can ask your router (via UPnP) to
  open the port it needs — the "just works behind NAT" convenience.
- **Falls back intelligently.** If UDP is blocked it tries TCP/TLS, and it can
  listen on several ports at once so a peer stuck behind a strict firewall can
  reach you on whichever port gets through. It dials all of a peer's known
  addresses in parallel rather than giving up on the first failure.
- **Relay as a last resort.** If two nodes genuinely can't connect directly,
  traffic hops through a willing third node — still end-to-end encrypted, so the
  relay only ever sees ciphertext.

## Private by default

- **Everything is encrypted end-to-end** with modern cryptography
  (AES-256-GCM, X25519 key exchange), authenticated with a pre-shared key so
  only your nodes can join.
- **Replay and tamper protection** on every packet.
- **Brute-force defense.** Repeated bad join attempts get the source banned
  automatically, and the admin login locks out after a few failed tries.

## You decide what traffic is allowed

- **A built-in firewall.** Per-network, ordered rules matched on direction,
  protocol, address, and port. Stateful by default — replies to connections you
  started come back automatically. Edit rules live; nothing needs a restart.
- **Management can't lock itself out.** A node-wide allow list keeps the admin UI
  and routing protocols reachable even behind a broad "deny everything" rule.
- **Quality of Service.** Prioritize important traffic so it goes first when the
  link is busy — classify by protocol, port, or DSCP.
- **Bandwidth caps.** Limit the up and down rate per network so one link can't
  starve the rest.
- **Broadcast and multicast** work across the overlay (with storm control), so
  things that expect a local network — discovery protocols, clustering — behave
  as if the machines were side by side.

## Real networking, not just a tunnel

- **Route whole subnets across the mesh** with longest-prefix matching, turning
  any node into a gateway to the network behind it.
- **NAT and port-forwarding on the overlay** (masquerade plus destination NAT),
  so you can reach non-gravinet hosts through a node.
- **Per-domain DNS forwarding** — send specific domains to DNS servers inside the
  mesh.
- **Names, not just numbers.** Hostnames sync into each machine's hosts file
  automatically.
- **BGP and BFD** for anyone running dynamic routing (through FRR), including a
  live view of BGP peer sessions.

## A real admin console, built in

Open an HTTPS page served by the node itself — no extra software to install.

- **Log in with your system account** (PAM on Linux/macOS/FreeBSD, native auth on
  Windows and OpenBSD), or a local password when you'd rather.
- **See the mesh at a glance:** live per-peer connection health, transport, and
  session detail; round-trip latency to every peer; and a map of where peers are.
- **Live metrics:** CPU, memory, disk, and per-interface throughput graphs.
- **Diagnose on the spot:** run a speed test between two nodes, capture packets on
  an overlay interface and download the pcap, and inspect the live routing table,
  DNS state, and daemon logs.
- **Remote shell** (off by default): a real terminal on the node, right in the
  browser, for when you need it.
- **Light and dark themes.**
- **Everything the UI does, a JSON API does too** — automate anything.
  See `docs/API.md` for the full reference.

## Manage a whole fleet from one place

- **One console for many nodes.** Put nodes in managed mode and drive them from a
  single Manager node — switch which node you're looking at from the header.
- **Push upgrades across the fleet, safely.** A Manager can hand a node new
  gravinet *source* (never a binary), which that node compiles itself, self-tests
  against its own config, and rolls back automatically if the new build can't
  rejoin the mesh. It's opt-in per node and can never be switched on remotely.
- **Roll the whole fleet in one action** — upgrade every peer first and finish
  with the node you're on last, and only if every other node came back healthy.

## Upgrades you can trust

- **Build-from-source upgrades** with a preflight self-test and a
  confirm-or-rollback guard: if the new binary can't get its peers back, it
  reverts itself. A bad upgrade doesn't strand a node.
- **Your configuration always survives.** Every change — from the command line or
  the web UI — is written to the config file and reloaded live, so nothing you
  set is lost on restart. The CLI and the UI edit the same source of truth.

## Built to be trusted

- **Pure Go, standard-library core.** Straightforward to read, audit, and
  reason about, with nothing exotic linked in.
- **Heavily tested,** including race detection and fuzzing of every component
  that parses data off the wire, plus live end-to-end tests of relaying, the
  firewall, NAT, bandwidth shaping, and the admin login.
- **Reproducible release builds** for every platform, published with SHA-256
  checksums.
- **Self-hosted end to end** — no account, no subscription, and no cloud control
  plane your network depends on.

---

*gravinet is a full-mesh, encrypted overlay VPN with a distributed control
plane, relay fallback, and a built-in admin console. Created by micush.*
