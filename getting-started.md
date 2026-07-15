[gravinet] is a full-mesh, encrypted overlay VPN. Almost everything in this guide is done from the **web admin UI** — that's how most people will run this day to day. The one thing you can't do from a browser is the initial install, so that part still uses a shell command.

---

## 1. Before you start: one reachable host

[gravinet] nodes find each other by gossip, but gossip has to start somewhere. **At least one node in your mesh needs to be reachable from the internet** on:

- **UDP port 65432** (the primary transport — this is the one that matters)
- **TCP port 65432** (a fallback transport, used when UDP is blocked)

Both default to **65432** (changeable later, but there's no reason to unless something else is already using it).

This doesn't mean every machine needs a public IP — only one does. Think of it as your mesh's **seed node**:

- Give it a public IP, or forward UDP+TCP 65432 to it on your router/firewall.
- Every other node just needs outbound internet access. They connect _to_ the seed, and after that the mesh introduces them to everyone else — you get a full mesh from one seed, not a hub-and-spoke.

On a host running **firewalld** (the default on RHEL, Rocky, Alma, CentOS and Fedora), the OS firewall also has to let those packets in — `install-linux.sh` opens the underlay port (UDP + TCP) and the web admin port (TCP) in the default zone for you, reading the port numbers from your config rather than assuming the defaults. Pass `--no-firewall` to skip that and do it yourself.

If a node's UDP 65432 is blocked outright, [gravinet] automatically tries a handful of well-known fallback ports (443, 4500, 3478, 1194, 500, 53). But don't rely on that for your seed node specifically — open 65432 on it deliberately.

That's the client side trying alternates; the seed itself can also simply listen on more than one port at once — the **UDP port** and **TCP port** fields under Settings → Underlay each take a comma-separated list, not just one number (e.g. `65432, 443, 80`), so a node can accept connections on all of them simultaneously rather than picking just one. The first port in each list is the primary (used for outbound and advertised to peers); any more are extra ports. Extra ports are advertised too, not just listened on — peers actually try them: an extra TCP port gets dialed in parallel with the primary whenever a peer's UDP looks blocked, and an extra UDP port joins the same pool of candidate addresses ordinary seeds use, so both kinds of extra ports genuinely help a peer reach this node through a restrictive firewall, not just a manually-configured one.

---

## 2. Install

Each release bundle has a platform installer that places the binary, scaffolds a config, and registers + starts the service — all in one step.

```sh
sudo ./install-linux.sh        # Linux (systemd)
sudo ./install-macos.sh        # macOS (launchd)
sudo ./install-freebsd.sh      # FreeBSD (rc.d)
doas ./install-openbsd.sh      # OpenBSD (rc.d via rcctl)
install-windows.bat            # Windows — double-click; it prompts for admin rights itself
```

That's the whole install. The service is now running with an empty config, and the rest of this guide happens in your browser.

---

## 3. Open the web admin

Every node runs a local HTTPS admin UI by default, at **`https://127.0.0.1:8443`**. It's TLS with a self-signed cert — accept the one-time browser warning (the cert persists across restarts, so you won't be re-prompted) — and log in with a **real system account**: PAM on Linux/macOS/FreeBSD, `login_passwd`/BSD-auth on OpenBSD, or a Windows account on Windows. There's no separate admin password to invent.

On a headless box, the safest option is to tunnel in rather than exposing the admin UI to the network:

```sh
ssh -L 8443:127.0.0.1:8443 user@your-server
# then open https://localhost:8443 locally
```

This works the same way on Windows as everywhere else — PowerShell (and `cmd`) has shipped a real `ssh` client since Windows 10 1809 / Windows 11, no separate install needed; run that exact command from a PowerShell prompt.

If SSH genuinely isn't an option (an older Windows build, a locked-down machine, or you'd just rather not touch a terminal at all), you can instead **temporarily point the admin UI at the network** for initial setup, then close it back down once you're done:

1. On the server, edit the config file and change one line under `"web_admin"` — `"listen": "0.0.0.0:8443"` (it starts as `"127.0.0.1:8443"`, loopback-only). The default path is `/etc/gravinet/config.json` on Linux/macOS/OpenBSD, or `/usr/local/etc/gravinet/config.json` on FreeBSD.
2. Restart the service so the new bind takes effect (`sudo systemctl restart gravinet` on Linux; the equivalent service restart on other platforms).
3. Make sure port 8443 can actually reach the box — this config change only controls what the _process_ listens on, not any OS firewall or cloud security group in front of it. You'll likely need to open 8443 there too, the same way you already opened 65432 in section 1.
4. Browse to `https://SERVER-IP:8443` from your Windows machine, accept the self-signed cert warning, and log in normally — the account and login flow are exactly the same as the loopback case.
5. **When you're done, put it back.** Change `"listen"` back to `"127.0.0.1:8443"` and restart the service again. The admin UI logs in with your real system account either way, but there's no reason to leave it reachable from the network any longer than it takes to finish setup — an SSH tunnel (or, if you want this open long-term rather than just for initial setup, a real TLS cert via `tls_cert`/`tls_key` instead of the self-signed one) is the better answer for ongoing access.

Once you're in, the left sidebar is organized into four working groups — **Mesh** (Networks, Keys, Seeds, Peers, Bans), **Traffic** (Routes, Firewall, NAT, QoS, Shaping), **Naming** (DNS, Hosts), and **Monitor** (live metrics, peer health, packet capture, speed test, and more) — plus a Settings page and a light/dark toggle up top. Almost every table in every section follows the same small set of gestures, so once you've done one, you've basically learned all of them:

- **+** adds a row
- tick one or more rows, then **−** removes them
- **double-click** a value to edit it in place
- **double-click** an enabled/disabled tag to flip it

Everything below assumes you're in this UI unless it says otherwise.

---

## 4. Create a network

Go to **Mesh → Networks**. There's nothing there yet — a fresh install has no default network on purpose, so the config only ever contains what you actually created.

Click **+**, type a name (e.g. `corp`), and click **create**. That's it — this mints a random network ID, its first join key, and a non-overlapping address range automatically. If you'd rather pick your own address range, the same row has optional **subnet4**/**subnet6** fields — fill in what you want (e.g. `10.50.0.0/16`) and leave the other blank for a single-family network, or fill both for dual-stack.

This applies immediately — no restart, and no interruption to any other network you already have running. (An earlier version of this daemon restarted the service for every network change; that's no longer true for add/delete/enable/disable/join — only changing a network's address range still needs one, since re-addressing a live interface genuinely isn't something a hot reload can do.)

You can belong to more than one network — each is a fully independent overlay with its own key, address range, and per-network settings (routes, firewall, NAT, QoS, bandwidth, DNS, hosts). Just click **+** again for another one; every per-network page then shows one card per network, so there's never any ambiguity about which one you're editing.

---

## 5. Join a network with a token

This is the fastest way to bring a second node in.

**On a node already in the network** (your seed node, right after step 4): go to **Mesh → Networks**, tick that network's row, and click **●** (generate token). A box opens with a token that's already being generated; once it appears, click **Copy**. It's valid for one hour and already bundles this node's keys, subnets, and every seed/peer address it knows — you don't need to type anything else in.

**On the new node**: go to **Mesh → Networks**, click **=** (join), paste the token into the box, and click **join**. Done — no network ID, no key, no seed address to track down by hand. The network's name and address range are filled in automatically once it connects.

That join box also has a manual fallback right below the token field — network ID, key, and seed peer as three separate inputs — for cases where you'd rather not generate a token (scripting, or handing out credentials some other way).

---

## 6. Enable DNS forwarding

Go to **Naming → DNS**. DNS _syncing_ is already on by default for every network — a node automatically applies any forwarding domain its peers advertise. What you actually do here is **advertise one**, i.e. tell the mesh "queries under this domain should go to that resolver":

1. Under the network's **Advertise** table, click **+**.
2. Enter a domain suffix (e.g. `corp.internal`, no leading dot) and one or more upstream DNS server IPs.
3. Save.

Every peer with DNS sync on (the default) picks this up automatically and registers it with its own OS resolver as a **routing domain** — only fully-qualified queries under that suffix are affected, so it can't interfere with ordinary internet DNS. Each advertised domain also becomes a search suffix by default (so an unqualified query like `grafana` is also tried as `grafana.corp.internal`) on Linux and Windows.

Don't want to accept a domain a peer advertises? Use the **Reject** table right below — same `+`/tick-and-`−` pattern — rather than turning sync off entirely.

> **Linux (RHEL, Rocky, Alma, CentOS): this feature needs systemd-resolved.**
> DNS forwarding is implemented on Linux via systemd-resolved's per-link routing
> domains (`resolvectl`), and those distros don't enable it by default —
> NetworkManager writes `/etc/resolv.conf` itself. Without it, every sync fails
> with `Failed to set DNS configuration: The name is not activatable` (that's
> D-Bus reporting that nothing is installed to answer `org.freedesktop.resolve1`).
> `install-linux.sh` now enables it for you; if you installed by hand, or passed
> `--no-systemd-resolved`:
>
> ```
> dnf install -y systemd-resolved          # RHEL 9+/Fedora ship it as its own package
> systemctl enable --now systemd-resolved
> ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
> printf '[main]\ndns=systemd-resolved\n' > /etc/NetworkManager/conf.d/10-gravinet-resolved.conf
> systemctl reload NetworkManager
> ```
>
> Debian, Ubuntu and Fedora already run it, so there's nothing to do there. Nothing
> else in gravinet depends on systemd-resolved — if you don't use DNS forwarding,
> you can leave your resolver stack alone.

---

## 7. Manage hosts

Go to **Naming → Hosts**. This advertises name → IP records into the mesh, the same idea as a shared `/etc/hosts` — every peer with host sync on (also on by default) writes them into its own hosts file automatically.

Each network gets its own card with two tables:

- **Advertise** — click **+**, enter a name and an IP (an overlay address, or anything reachable over a route you're advertising), save. Double-click the name or IP later to change it; double-click the state tag to enable/disable without deleting it.
- **Reject** — a local filter: names listed here are never written into _this_ node's hosts file even if a peer advertises them. Same `+`/tick/double-click pattern.

---

## 8. Manage routes

Go to **Traffic → Routes**. Same per-network, two-table layout as Hosts:

- **Advertise** — click **+**, enter a CIDR to redistribute into the mesh (e.g. `10.1.1.0/24`, or `0.0.0.0/0` to advertise a default route and send everyone's internet-bound traffic through this node). Double-click the metric to change route preference (lower wins) or the cidr to edit it; double-click the state tag to stop advertising it without deleting it.
- **Reject** — refuse a route a peer advertises. Tick **inclusive** to also reject every more-specific network inside that CIDR, not just an exact match.

Routing uses longest-prefix-match, same as any router — a more specific route a peer advertises wins over a broader one you're also carrying.

---

## 9. Clustering: manage a whole fleet from one login

Every node you've met so far manages _only itself_ — you log into each one's web admin separately. Clustering removes that: turn it on and you can browse and configure other nodes' Networks, Routes, Firewall, NAT, QoS, DNS, and Hosts pages from inside **one** node's browser session, switching between them with the peer dropdown in the top bar. There's no separate account, password, or certificate to set up for this — it rides the same encrypted mesh session peers already use to talk to each other.

It's two independent switches, both off by default, both on the **Settings** page under **Cluster**:

- **Managed mode** — lets _other_ nodes manage _this_ one. Turn this on for every node you want to be able to reach remotely. (CLI: `gravinet managed on|off|status`.)
- **Manager mode** — lets _this_ node manage _other_ Managed nodes. Turn this on for the one (or few) node(s) you'll actually administer from. (CLI: `gravinet manager on|off|status`.)

These are genuinely independent, not two ends of one switch:

- **Both off** — an ordinary node, log in locally only: a pure admin console that can drive the fleet, but can't itself be managed without a normal login.
- **Managed on, Manager off** — can be managed, but can't manage anyone else.
- **Manager on, Managed on** — full two-way (the old single-flag behavior).

A typical setup: flip **Manager** on for your own laptop or a bastion box, and **Managed** on for every server you want to reach from it. Then, from your laptop's web admin, the peer dropdown lists every Managed node the mesh currently knows about — pick one and every page you visit (Networks, Routes, Firewall, NAT, QoS, DNS, Hosts...) shows and edits _that_ node's configuration instead of your own. Switch back to "This node" to return to your own.

A few things worth knowing about how it's secured, since "remote admin access with no separate login" is worth understanding before relying on it:

- A Managed node only accepts a management connection that genuinely arrives over the overlay (a structural overlay address — not something a peer can fake by lying in gossip) **and** resolves, via the mesh's own live peer registry, to a node currently advertising Manager mode. Reaching the overlay at all already required the network's join key; Manager mode is a second, separate gate on top of that.
- **Managed/Manager mode are never remotely configurable, on purpose.** No matter which peer is selected in the dropdown, the Cluster toggles on the Settings page always read and write _this_ node's own status — flipping them can never accidentally change a peer's setting instead of the one you meant to.
- The dropdown only ever lists a peer as reachable if it's advertised Managed _and_ been heard from within the last 90 seconds — a node that's gone quiet just drops off the list rather than showing as a dead entry.
- Flip Manager mode on and try to use it immediately against a peer you're already connected to — it works right away, not after some delay, because the flag change is pushed live to every connected peer the moment you toggle it, not just picked up at the next reconnect.

---

## 10. Key management

Every network is authenticated by a shared key — anyone who has it (and can reach a seed) can join. A network has **8 key slots**, not just one, which is what makes rotating a key possible without a network-wide outage: old and new keys both authenticate joiners at the same time, for as long as you leave them both enabled.

Go to **Mesh → Keys**. Each network gets its own table, one row per slot:

- **Generate** a new key into an empty slot (tick it, then Generate).
- Tick **distributed** on a newly generated key to push it straight to every peer currently connected on that network, over the mesh itself — no copy/paste, no separate channel, and it lands in the same slot number on their end too. This is the easiest way to rotate: generate, tick distributed, wait until you're confident it's reached everyone (the Peers page shows who's connected), then disable the old slot.
- Prefer to hand a key out manually instead (e.g. to a brand-new node that isn't meshed yet, so there's nothing to push it to yet)? Tick a filled slot and use **Reveal**/**Copy** — or generate a join token instead (see section 5), which bundles a key with everything else a new node needs in one paste.
- **Disable** a slot to stop it authenticating new joins without deleting it — useful while you're still confirming a rotation reached everyone. **Delete** it once you're sure nothing still depends on it.
- Double-click **expires** to set a date/time after which a key stops authenticating on its own; anything still using it re-handshakes on a remaining key rather than dropping.

The CLI has the same operations — `gravinet key list|generate|show|set|enable|disable|delete|distribute -net NAME -slot N` — useful for scripting a rotation across many networks at once. One guardrail applies everywhere, GUI or CLI: you can't disable or delete the _last_ enabled key on a network, since that would lock everyone out, yourself included.

---

## 11. Firewall

Go to **Traffic → Firewall**. Disabled means all traffic passes; enabled means rules are evaluated top to bottom, first match wins, and anything that matches nothing is allowed — it's stateful, so replies to a flow you allowed come back automatically without a separate rule. A lone inbound-deny rule is enough to block everything unsolicited, since the reply traffic for connections _you_ started still gets through.

Click **+** to add a rule, drag rows to reorder them (order matters — first match wins), double-click a field to edit it or the state tag to toggle a rule without deleting it, and tick rows plus **−** to remove. The toolbar also has cut/copy/paste for moving rules between networks.

A separate, node-global **Allow List** — management, BGP, OSPF, and RIP by default — sits outside every network's rulebase entirely, so a broad deny can't lock out management or routing protocols. It's its own tab next to Rules.

---

## 12. NAT

Go to **Traffic → NAT**. This rewrites IPv4 addresses (IPv4-only) as traffic crosses the tunnel. **Source** and **dest** pick which packets a rule matches (blank = any); **translate** is either _masquerade_ (rewrite to whatever address the chosen physical interface currently has — many overlay addresses map to one outside address) or a literal IPv4 address to translate to instead.

Click **+** to add a rule, double-click any field to edit it or the state tag to toggle it, tick rows and **−** to remove.

---

## 13. QoS

Go to **Traffic → QoS**. Traffic is classified by protocol, port, or DSCP into 5 priority classes (0 = highest, 4 = lowest/bulk traffic); anything that doesn't match a rule uses class 3 (normal). Scheduling is strict-priority — under contention, higher classes fully drain before a lower one gets anything, so use the top classes sparingly for things that actually need it (voice, RDP, control traffic), not everything at once.

Click **+** to add a rule, double-click to edit it or toggle its state, tick rows and **−** to remove.

---

## 14. Shaping (bandwidth)

Go to **Traffic → Shaping**. A per-network rate cap, set independently for each direction: egress is shaped (queued and paced to the rate), ingress is policed (anything over the rate is dropped rather than queued). Double-click a rate to set it — type a number and pick a unit, or clear the number for unlimited. Double-click the tag above the rate to turn the cap on or off without losing the configured number, so you can lift it temporarily and reapply the same rate later.

---

## 15. Seeds

Go to **Mesh → Seeds**. These are the host or host:port addresses this node dials to _find_ each network — distinct from the live Peers list below, since a seed stays configured whether or not anything is currently connected to it. A join token already embeds the seeds it knew about at the time it was generated; this page is for adding more by hand, or seeing what's already there.

A seed's address can carry more than one port, comma-separated (e.g. `203.0.113.5:65432,443,53`) — each is tried as its own dial candidate against that host, on top of whatever a bare host with no port already gets (the primary port plus gravinet's own built-in fallback set). This is for a seed known to answer on a handful of specific, non-default ports — likely to make it through a restrictive firewall — rather than the built-in set.

Each seed has a transport: **udp** seeds bootstrap over UDP, with automatic TCP/TLS fallback if UDP turns out to be blocked; **tcp** seeds are dialed straight over the TCP/TLS fallback, which is useful for cold-starting a node onto the mesh when UDP is blocked end to end and there's no point trying it first. Double-click an address to edit it, or the transport cell to flip udp/tcp. Click **+** to add a seed (applies live), tick rows and **−** to remove one (takes effect on the next restart).

---

## 16. Peers

Go to **Mesh → Peers** to see who's actually connected right now, grouped by network, and operate on them. Double-click a peer to disable it — it disconnects and stays down until you turn it back on; disabled peers stay listed so re-enabling is a click away, not a re-add. Tick rows and **Ban** to block a peer mesh-wide rather than just locally.

Double-click **notes** to attach a free-form local note to a peer's node id — never sent to the peer, and permanent regardless of what its address does afterward (a NAT rebind, a LAN-discovered direct path, a stretch spent relayed, none of it matters once the note is on the id). If a peer connects on an address matching one of this network's seeds and doesn't have its own note yet, it automatically inherits that seed's note the first time this happens — a one-time copy, checked every 20 seconds, that never overwrites a note you've set yourself and never touches the seed's own note either. This is also why hovering a peer's name elsewhere (Monitor → Mesh Peers, Monitor → Latency, Bans) can show a note even before you've typed anything for that peer directly.

This page is about _acting_ on peers. For connection health, transport detail, and which key is authenticating each session, see the read-only **Monitor → Mesh Peers** page instead.

---

## 17. Bans

Go to **Mesh → Bans**. Bans propagate across the mesh from whichever node created them, and are grouped here by network, showing the banned target, the node that issued the ban (its origin), and the reason. Tick rows to lift a ban. If you issued a ban yourself, double-click its reason to edit it — the change re-floods to every node, the same way the ban itself did.

---

## 18. Monitoring

The **Monitor** group is read-only — Metrics, Mesh Peers, Capture, Speedtest, Latency, Route Table, Hosts File, DNS State, and Logs. This is where you go to see what's actually happening on a node (live throughput, per-peer connection detail, a packet capture, what's really registered with the OS resolver right now) as opposed to configuring what should be happening, which is everything above.

---

## For scripting: the CLI

Everything above also has a CLI equivalent (`gravinet network`, `route`, `host`, `nat`, `qos`, `bandwidth`, `key`, `seed`, `fw`, ...) — useful for automation, or a headless box you'd rather not tunnel into. Run `gravinet -h` for the full list. The one exception is DNS forwarding, which right now is GUI-only.

---

## Where to go next

- The web admin's own hint text under each table — it's specific to what that page does and kept current with the UI.
- `README.md` (ships next to the binary) — build/install details and the full CLI reference, for anything you'd rather script.
- `docs/ARCHITECTURE.md` — the full design, and an honest list of what's platform-bound vs. universal.
