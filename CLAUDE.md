# Instructions for Claude working on this project

gravinet is a Go daemon that joins machines into one private, encrypted mesh
(AES-256-GCM, X25519 key exchange, per-network keys), with seeds for discovery,
relaying through a third node when two can't reach each other, and a firewall,
NAT, QoS, shaping, routes, DNS forwarding and BGP (through FRR) built in. It is
managed three ways that all change the same config: an HTTPS web admin
(`internal/webadmin`), a terminal screen (`internal/tui`) and the CLI
(`cmd/gravinet`). It runs on Linux, Windows, macOS, FreeBSD and OpenBSD. This
file is a standing instruction from the project owner so it doesn't need
repeating each conversation. If you (Claude) are asked to change this project,
follow the process below without being told to.

## Release process — do this for every request that changes the project

1. **Do the requested work** (code, docs, config, whatever was asked).
   **Layout.** One Go module, `module gravinet` (stdlib-only; `go.mod` has no
   requires). `cmd/gravinet/` is the binary and the CLI; `internal/` holds the
   engine (`mesh`, `protocol`, `crypto`, `transport`, `tun`), the host side
   (`hostnet`, `netfilter`, `tcshape`, `ipfwd`, `resolver`, `hosts`, `ddns`,
   `dhcrelay`, `upnp`), `service` (OS integration: users, groups, launchd/
   systemd/rc.d/SCM), `upgrade`, `config`, `tui` and `webadmin`. The whole web
   page is the Go raw string `indexHTML` in `internal/webadmin/ui.go`.
   `install/` has one installer and uninstaller per OS plus the service files,
   `pkgman` and `meshping`; `get.sh` at the top is the `curl | sudo bash`
   installer (newest release, else the highest `v<number>` tag; keep it working
   with the tag rules here). `scripts/` builds releases; `docs/` has
   `ARCHITECTURE.md`, `API.md`, `UPGRADES.md`, `SECURITY-FIXES.md` and
   `changelog.md`; `snaps/` holds the README screenshots; `README.md`,
   `getting-started.md` and `features.md` are user docs (the installers copy
   README, LICENSE, getting-started.md and API.md next to the binary, and the
   web admin serves them).

2. **Build and test before committing, not after.** From the repo root run
   `gofmt -l .` (must print nothing for files you touched; `internal/tui` has a
   few long-standing unformatted files — don't reformat them as a side effect),
   `go build ./...`, `go vet ./...` and `go test ./...` for the packages you
   touched plus `cmd/gravinet` and `internal/webadmin`. `internal/mesh` is slow
   (over ten minutes on a weak box): give it `-timeout 60m` when you run it, and
   if you didn't, say so. Use `-race` on code with goroutines or locks. Don't
   skip straight to committing on a change that could plausibly have broken
   something.
   - **This is the owner's Mac, not a sandbox.** Never run an installer, `sudo`,
     `launchctl`, `dscl`/`dseditgroup`, or anything that changes users, groups,
     PAM, the firewall or the running daemon without asking first. Throwaway
     daemons, configs and responders go in the scratchpad, on loopback.
   - **Known macOS failures.** These already fail on a clean macOS tree and are
     not yours: in `internal/service`, `TestUnixGroupMembers*` and
     `TestIsGroupMemberOrdinaryCases` (they assume a Linux `/etc/group`); in
     `internal/webadmin`, the VLAN tests in `sysvlans_test.go`,
     `TestSampleRunsReadersConcurrently` and
     `TestFetchTshootOneSelfUsesBuildTshootText`. Compare against a clean tree
     (`git stash`) before calling a failure pre-existing, and list it in the
     changelog either way.
   - **Touched the wire protocol (`internal/protocol`, `internal/mesh`
     handshakes)?** Meshes upgrade one node at a time, so old and new versions
     must keep talking during a rollout. A change that breaks that needs a
     compatibility plan and a changelog entry saying so. Keep the fuzz tests in
     `internal/protocol/fuzz_test.go` passing and extend them for new decoders.
   - **Touched the web admin (`internal/webadmin`, especially `ui.go`)?** It
     runs as root and can reconfigure the host, so verify properly, not just by
     compiling:
     1. The `internal/webadmin` tests parse the page script. Keep them passing,
        and never weaken an auth, session, CSRF or lockout test to make it pass.
     2. `indexHTML` is a Go raw string: no backticks, so no template literals
        (`TestPageScriptUsesNoTemplateLiterals`). In single-quoted strings,
        escape apostrophes as `\'` like the strings around them — an unescaped
        one blanks the whole page (`TestPageScriptHasNoUnterminatedStringLiteral`).
     3. No native browser dialogs: use `confirmModal` and `noticeModal`, and
        always `await` a `confirmModal` (`dialogsuppression_test.go`). Browsers
        silently suppress `alert`/`confirm` after a few, turning buttons into
        no-ops.
     4. Give a new page or section its `HELP` entry, with a topic. Entries must
        name real sections, columns and tabs (`helpmode_test.go`). Help mode is
        off by default and CSS-driven.
     5. For real UI logic, write a throwaway jsdom or headless-Chromium check in
        the scratchpad (not the repo) against a scratch daemon on loopback with
        `auth_mode` `local` and a `gravinet genpass` user. Look at it in light
        and dark when layout or theme changed.
   - **Touched login (`auth_*.go`, `internal/service/groups.go`,
     `sysusers.go`)?** Sign-in is root, or a member of the `gravinet` OS group,
     checked live on every attempt, before PAM runs. System auth is PAM on
     Linux, macOS and FreeBSD (cgo, `auth_pam.go`), `login_passwd(8)` on OpenBSD
     (`auth_bsdauth.go`, no cgo) and `LogonUser` on Windows (`auth_windows.go`).
     A build without cgo (`auth_nopam.go`) falls back to local users. The
     installers write `/etc/pam.d/gravinet` and add the installing user to the
     group with `gravinet system users grant`. A fresh install must still let
     that user log in: a group nobody can join without logging in first has
     happened (v1025). Real grants and PAM checks change the host — ask first.
   - **Touched an installer or uninstaller (`install/*.sh`, `*.ps1`, `*.bat`,
     `get.sh`)?** They run as root on other people's machines: `bash -n`, and
     `shellcheck -x -S warning` if it's installed (say so if it isn't). Keep
     them non-fatal around optional extras, idempotent on re-run, and never let
     an uninstaller delete the config without `--purge`. Remember that an
     installer skips the install when the installed version equals the source
     version — test an upgrade with a bumped version. On Windows PowerShell 5.1
     with `$ErrorActionPreference = "Stop"`, redirecting a native command's
     stderr throws; run such commands through `cmd /c`. No Windows host here:
     say "not run" for `.ps1`/`.bat` changes.
   - **Added or changed a page, a CLI command or a setting?** The GUI, TUI and
     CLI must stay in step: `cmd/gravinet/navparity_test.go` maps every web nav
     group to a CLI group. Add the CLI leaf in the same change as the page, and
     the other way round. Update the help entry, `docs/API.md` for a new API
     route, and the README where it describes the feature.
   - **Touched the upgrade path (`internal/upgrade`, `webadmin/upgrade*.go`,
     `cmd/gravinet/upgrade.go`)?** A node builds the new source itself, tests
     it, and rolls back if it can't rejoin. Never replace the running binary
     without keeping the rollback copy and the confirm-or-rollback guard. A
     manager can push to a whole fleet, so a bug here breaks every node at once.
   - **Touched README images?** Every `![…](snaps/…)` must exist
     (`TestMdRenderSkipsImageLines`).

3. **Cross-compile every release**, not just once — a later change can
   silently break a platform nothing here runs. For each target in
   `scripts/build-release.sh` (`linux/amd64`, `linux/arm64`, `linux/arm`,
   `windows/amd64`, `windows/arm64`, `darwin/amd64`, `darwin/arm64`,
   `freebsd/amd64`, `openbsd/amd64`, `openbsd/arm64`) run
   `CGO_ENABLED=0 GOOS=<os> GOARCH=<arch> go build -o /dev/null ./cmd/gravinet`.
   If a target is added, add it to that script and this list in the same change.

   **cgo caveat (PAM):** cross-compiles run with cgo off, so the Linux, macOS
   and FreeBSD ones build the no-PAM fallback (local users only). They prove
   the code compiles, not that PAM works. Only the native build is checked with
   PAM (`gravinet version` prints `pam=yes`); say so in the changelog.

4. **Bump the version.** It's a plain integer, no `v` prefix and no semver:
   `version = "N"` in `cmd/gravinet/main.go`. The installers and `get.sh` read
   it from there. Increment it by exactly 1.

5. **Add a new entry to the top of `docs/changelog.md`**, right below the
   `# Changelog` header and its `---`, in the style of the existing entries:
   `## vN — YYYY-MM-DD` (the new version and the date of the change), a bold
   one-line summary, then prose and bullets saying what changed and why, and a
   closing `Verified: … Not verified: …` paragraph naming exactly what was run.
   End the entry with `---`. Don't rewrite or renumber older entries.

6. **Commit and tag.** Stage only the files you changed and commit with the
   message `see changelog for release info` (plus the attribution line Claude
   Code supplies), then make a lightweight tag `vN` on that commit. One release
   is one commit: the work, the version bump and the changelog together. Don't
   push the branch or the tag unless asked. A source archive, when asked for,
   is `gravinet-N.tar.gz` with `gravinet/` at the top: source only, no `dist/`,
   no binaries, no scratch files (check `tar tzf` first).

Do steps 4–6 after every other file change (including this file and the
README), so the version and changelog describe the final tree.

## Notes

- **NO BANNERS. Ever.** Never add a banner, notice bar or status-message line
  to the GUI: not for progress, not for success, not for info or warnings.
  State goes in the control itself (tooltip, button label, a few words of small
  text beside it, a row's colour). A failure the person must act on is plain
  red text, with no box, beside the control that failed — or a `noticeModal`
  when there is no control to put it beside. When touching a page, remove any
  banner you find in the code you are changing.
- **No update or status text between cards.** Anything of that kind goes
  inside a card, in a control's tooltip or state, or in the help entry.
- **GUI text: keep it short.** No explanatory paragraphs, hints or notes on
  pages, forms or tables. Explanations go in the help entries. A label, a
  placeholder and at most a few words of hint are the limit. Don't add text the
  owner didn't ask for.
- The process applies to substantive changes (features, fixes, docs,
  refactors). Pure Q&A that changes nothing on disk needs no version bump.
- A pure packaging fix (archive name, permissions, layout) that changes nothing
  in the code, tests or behaviour doesn't need a bump: note it under the
  current version's changelog entry and keep the archive name matching.
- A request big enough to split into several pieces in one sitting still gets
  one bump, one entry and one commit.
- Keep changelog entries specific about what was built or fixed and why, and
  honest about what wasn't verified. Real multi-host meshes, NAT traversal and
  relaying on a real wire, the Windows and BSD TUN drivers, and fleet upgrades
  can't be exercised here; say "not verified" rather than implying they were.
- Dependencies: stdlib only, on purpose. The one non-Go runtime piece is the
  signed Wintun driver for Windows, embedded at release time
  (`internal/tun/wintun`, staged by `scripts/build-all.sh`). Don't add a module
  for something small; if one is truly warranted, say so in the changelog.
- `distribute-gravinet` is the owner's private deployment script. It is not
  part of this tree: never add it here or to an archive, and don't add a
  `.gitignore` for it.
- Web admin invariants (don't regress without saying so in the changelog):
  login = root or `gravinet` group membership, then the system auth succeeding;
  three failed logins in a minute lock the address out for 15 minutes; it
  listens on `127.0.0.1:8443` by default with a self-signed certificate, and
  exposing it on the network is the operator's explicit choice — keep the
  README's "firewall it and use a real certificate" advice.
- If you are a future Claude, in any session type, asked to work on this
  project and this file's process hasn't been mentioned, read this file before
  starting anyway.
