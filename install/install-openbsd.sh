#!/bin/sh
# install-openbsd.sh — build (if needed) and install gravinet as an rc.d service.
#
#   doas ./install-openbsd.sh                 # build from source + install + start
#   doas ./install-openbsd.sh --uninstall     # remove
#
# With no prebuilt binary present, this installs a Go toolchain if the host
# lacks one (pkg_add go, falling back to a checksum-verified go.dev tarball),
# builds the binary from the bundled source, then installs it.
#
# Web-admin auth on OpenBSD: unlike Linux/macOS/FreeBSD, OpenBSD has no PAM —
# it uses BSD Authentication. This binary is built CGO_ENABLED=0 and the web
# admin authenticates system accounts by shelling out to login_passwd(8) (the
# same helper login(1)/su(1) use) — no PAM, no cgo, no C toolchain. The daemon
# runs as root so login_passwd can verify passwords against the master
# database. (A local auth_mode via 'gravinet genpass' also exists if you'd
# rather not use system accounts, but it isn't set up or needed here.)
#
# Options:
#   --bin PATH     use this prebuilt binary instead of building
#   --prefix DIR   install prefix (default: /usr/local)
#   --config PATH  config file (default: /etc/gravinet/config.json)
#   --no-start     install but do not start the daemon now
#   --no-unbound   don't set up unbound(8) as the system resolver. By default
#                  (unless this flag is passed) this installer sets up
#                  unbound(8) as the system resolver so gravinet's per-network
#                  conditional DNS forwarding works (enables remote-control,
#                  points /etc/resolv.conf at 127.0.0.1, and disables
#                  resolvd(8) so it can't overwrite that). Unlike FreeBSD's
#                  local-unbound, OpenBSD's default resolver stack
#                  (unwind/resolvd) has to be displaced for this, which is why
#                  the installer does it for you — pass --no-unbound to skip
#                  it if no network here uses DNS forwarding.
set -eu

PREFIX=/usr/local
CONFIG=/etc/gravinet/config.json
SRC=""
START=1
ACTION=install
RCSCRIPT=/etc/rc.d/gravinet
REPO="$(cd "$(dirname "$0")/.." && pwd)"
GO_MIN_MINOR=22   # go.mod requires go 1.22
UNDERLAY_PORT=65432   # default gravinet underlay port (tcp+udp); the pf rule below opens it
SETUP_UNBOUND=1       # default on: makes unbound the system resolver; --no-unbound opts out (see help)

# build_from_source() stages the freshly-built binary under a mktemp -d
# directory (BUILD_TMP, set there) so it can be installed from a known path
# regardless of how it was produced. That directory used to just get left
# behind under /tmp on every single run — a fresh one every install/upgrade,
# never reused, never removed — which is exactly what fills up a tmpfs-backed
# /tmp over time on a box that gets reinstalled/upgraded repeatedly. Registering
# the cleanup here, once, as an EXIT trap means it fires on every exit path
# (success, `exit 1` on a later failure, etc.), not just the one line right
# after the mktemp call — so a later unrelated failure can't leak it either.
BUILD_TMP=""
cleanup_build_tmp() { [ -n "$BUILD_TMP" ] && rm -rf "$BUILD_TMP"; }
trap cleanup_build_tmp EXIT

while [ $# -gt 0 ]; do
  case "$1" in
    --uninstall) ACTION=uninstall ;;
    --bin) SRC="$2"; EXPLICIT_BIN=1; shift ;;
    --prefix) PREFIX="$2"; shift ;;
    --config) CONFIG="$2"; shift ;;
    --no-start) START=0 ;;
    --start) START=1 ;;
    --unbound) SETUP_UNBOUND=1 ;;
    --no-unbound) SETUP_UNBOUND=0 ;;
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "error: run as root (doas/su)" >&2; exit 1; }

# OpenBSD keeps daemons in sbin.
BIN="$PREFIX/sbin/gravinet"
PKGMAN="$PREFIX/sbin/pkgman"
MESHPING="$PREFIX/sbin/meshping"

# bin_version runs "<bin> version" and extracts the version field from its
# "gravinet NNN (commit) os/arch" output (cmd/gravinet/main.go's version
# subcommand).
bin_version() {
  [ -x "$1" ] || return 0
  "$1" version 2>/dev/null | awk '{print $2}'
}

# source_version reads the version baked into the source so you can see what
# this tree will install before it builds anything.
source_version() {
  sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
    "$REPO/cmd/gravinet/main.go" 2>/dev/null | head -1
}
SRC_VER="$(source_version)"

if [ -n "$SRC" ]; then
  NEW_VER="$(bin_version "$SRC")"
else
  NEW_VER="$SRC_VER"
fi
CUR_VER="$(bin_version "$BIN")"

if [ "$ACTION" != uninstall ]; then
  echo "==> currently installed version: ${CUR_VER:-none}"
  echo "==> version to install: ${NEW_VER:-unknown}"
  if [ -n "$CUR_VER" ] && [ -n "$NEW_VER" ] && [ "$CUR_VER" = "$NEW_VER" ]; then
    echo "already up to date (version $CUR_VER) — skipping install"
    exit 0
  fi
fi

if [ "$ACTION" = uninstall ]; then
  echo "==> stopping daemon"
  rcctl stop gravinet 2>/dev/null || true
  rcctl disable gravinet 2>/dev/null || true
  rm -f "$RCSCRIPT" "$BIN" "$PKGMAN" "$MESHPING"
  rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"
  rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true
  echo "==> removed $BIN, $PKGMAN, $MESHPING, $RCSCRIPT, and the docs (config at $CONFIG left in place)"
  exit 0
fi

# --- Go toolchain bootstrap + build-from-source --------------------------------

go_minor() {
  command -v go >/dev/null 2>&1 || { echo -1; return; }
  v="$(go version 2>/dev/null | awk '{print $3}')"; v="${v#go}"
  m="${v#*.}"; m="${m%%.*}"
  case "$m" in '' | *[!0-9]*) echo -1 ;; *) echo "$m" ;; esac
}

go_arch() {
  case "$(uname -m)" in
    arm64|aarch64) echo arm64 ;;
    amd64|x86_64)  echo amd64 ;;
    i386)          echo 386 ;;
    *) echo "" ;;
  esac
}

# go.dev fallback fetch. OpenBSD's base ftp(1) speaks HTTPS, so no curl needed.
official_install_go() {
  command -v ftp >/dev/null || command -v curl >/dev/null || { echo "    need ftp(1) or curl to download Go" >&2; return 1; }
  ga="$(go_arch)"; [ -n "$ga" ] || { echo "    unsupported arch for Go" >&2; return 1; }
  if command -v curl >/dev/null; then get="curl -fsSL"; else get="ftp -o -"; fi
  ver="$($get 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1)"
  [ -n "$ver" ] || { echo "    could not resolve latest Go version" >&2; return 1; }
  tgz="${ver}.openbsd-${ga}.tar.gz"
  echo "==> downloading https://go.dev/dl/${tgz}"
  $get "https://go.dev/dl/${tgz}" > "/tmp/${tgz}" || return 1
  if command -v sha256 >/dev/null; then
    want="$($get 'https://go.dev/dl/?mode=json&include=all' 2>/dev/null \
      | grep -A6 "\"${tgz}\"" | grep -m1 '"sha256"' | sed -E 's/.*"sha256": *"([0-9a-f]+)".*/\1/')"
    if [ -n "$want" ]; then
      got="$(sha256 -q "/tmp/${tgz}")"
      [ "$got" = "$want" ] || { echo "    Go checksum mismatch" >&2; rm -f "/tmp/${tgz}"; return 1; }
      echo "    Go ${ver} checksum verified"
    fi
  fi
  rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/${tgz}" && rm -f "/tmp/${tgz}"
  export PATH="/usr/local/go/bin:$PATH"
}

ensure_go() {
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null && return 0
  [ -x /usr/local/go/bin/go ] && export PATH="/usr/local/go/bin:$PATH"
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null && return 0
  # Prefer the OpenBSD package (kept current in -stable/-release); fall back to
  # a pinned go.dev tarball if it's missing or too old.
  if command -v pkg_add >/dev/null && pkg_add -I go >/dev/null 2>&1 && [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null; then
    return 0
  fi
  official_install_go || return 1
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null
}

build_from_source() {
  [ -f "$REPO/go.mod" ] || { echo "error: no prebuilt binary and no source tree at $REPO" >&2; exit 1; }
  ensure_go || { echo "error: could not obtain a Go toolchain (>=1.${GO_MIN_MINOR})" >&2; exit 1; }
  # CGO off: OpenBSD has no PAM. The web admin authenticates system accounts
  # via login_passwd(8) (see webadmin/auth_bsdauth.go), so system login works
  # without cgo or a C toolchain — nothing to link here.
  echo "==> building gravinet from source with $(go version | awk '{print $3}') (cgo=0, bsd_auth web-login)"
  BUILD_TMP="$(mktemp -d)"
  out="$BUILD_TMP/gravinet"
  # GOCACHE/GOPATH default to under $HOME (~/.cache/go-build, ~/go/pkg/mod)
  # when unset, and go build never cleans either one up itself — it's a
  # persistent cache, by design, for a machine that keeps rebuilding the same
  # module. On a one-shot install/upgrade that isn't going to happen again
  # anytime soon, that's not a cache, it's litter: every run leaves another
  # copy behind, and on a default OpenBSD partition layout (small root, no
  # separate large /home) that quietly eats the filesystem it's on. Redirect
  # both under BUILD_TMP instead, which the EXIT trap above already removes
  # on every exit path — the build gets it own scratch space and nothing
  # outlives this run. GOTMPDIR gets the same treatment: go build's own
  # per-run scratch dir (go-buildNNNNNN, separate from GOCACHE) otherwise
  # defaults straight to /tmp and is exactly the kind of leftover this whole
  # function exists to avoid.
  mkdir -p "$BUILD_TMP/.gotmp"
  ( cd "$REPO" && CGO_ENABLED=0 GOTOOLCHAIN=auto \
      GOCACHE="$BUILD_TMP/.gocache" GOPATH="$BUILD_TMP/.gopath" GOTMPDIR="$BUILD_TMP/.gotmp" \
      go build -buildvcs=false -trimpath -ldflags "-s -w" -o "$out" ./cmd/gravinet ) \
    || { echo "error: build failed" >&2; exit 1; }
  SRC="$out"
  echo "    built $SRC"
}

EXPLICIT_BIN=${EXPLICIT_BIN:-0}

if [ "$EXPLICIT_BIN" = 1 ] && [ -n "$SRC" ]; then
  [ -f "$SRC" ] || { echo "error: --bin $SRC not found" >&2; exit 1; }
elif [ -f "$REPO/go.mod" ]; then
  build_from_source
else
  case "$(uname -m)" in
    amd64|x86_64)  a=amd64 ;;
    arm64|aarch64) a=arm64 ;;
    *) a="" ;;
  esac
  here="$(cd "$(dirname "$0")" && pwd)"
  for cand in "$here/gravinet-openbsd-$a" "$here/gravinet"; do
    [ -f "$cand" ] && { SRC="$cand"; break; }
  done
  [ -n "$SRC" ] && [ -f "$SRC" ] || { echo "error: no source tree at $REPO and no prebuilt binary found" >&2; exit 1; }
fi

# Upgrade-in-place: stop a running daemon before replacing its binary. A
# running OpenBSD binary can be unlinked/replaced while executing (the old
# inode survives until the process exits); this is about not leaving the old
# build running under a replaced rc.d script.
WAS_RUNNING=0
if rcctl check gravinet >/dev/null 2>&1; then
  WAS_RUNNING=1
  echo "==> daemon is running; stopping before upgrade"
  rcctl stop gravinet 2>/dev/null || true
fi

echo "==> installing $SRC -> $BIN"
install -d -m 0755 "$PREFIX/sbin"
install -m 0755 "$SRC" "$BIN"

# The binary now lives at $BIN, so the build scratch (BUILD_TMP holds the Go
# build+module caches — hundreds of MB) has served its purpose. Remove it now
# rather than holding it through the rest of the run (service, pf, unbound
# setup); the EXIT trap still covers every path that exits before reaching
# here. Guarded + cleared so the trap's later pass is a harmless no-op.
if [ -n "$BUILD_TMP" ]; then
  rm -rf "$BUILD_TMP"
  BUILD_TMP=""
fi

SCRIPTDIR="$(dirname "$0")"
echo "==> installing pkgman -> $PKGMAN"
if [ -f "$SCRIPTDIR/pkgman" ]; then
  install -m 0755 "$SCRIPTDIR/pkgman" "$PKGMAN"
else
  echo "    note: pkgman not found in the package; skipping"
fi

echo "==> installing meshping -> $MESHPING"
if [ -f "$SCRIPTDIR/meshping" ]; then
  install -m 0755 "$SCRIPTDIR/meshping" "$MESHPING"
else
  echo "    note: meshping not found in the package; skipping"
fi

echo "==> installing docs (README, LICENSE)"
DOCDIR="$PREFIX/share/doc/gravinet"
install -d -m 0755 "$DOCDIR"
for doc in README.md LICENSE getting-started.md; do
  src=""
  for cand in "$REPO/$doc" "$SCRIPTDIR/$doc" "$SCRIPTDIR/../$doc"; do
    [ -f "$cand" ] && { src="$cand"; break; }
  done
  if [ -n "$src" ]; then
    install -m 0644 "$src" "$DOCDIR/$doc"
    echo "    installed $DOCDIR/$doc"
  else
    echo "    note: $doc not found in the package; its web admin page will be empty"
  fi
done

echo "==> config $CONFIG"
mkdir -p "$(dirname "$CONFIG")"
if [ ! -f "$CONFIG" ]; then
  "$BIN" run -config "$CONFIG" -init >/dev/null
  echo "    scaffolded a default config (no networks yet)"
else
  netOut="$("$BIN" network list -config "$CONFIG" 2>/dev/null || true)"
  if [ -z "$netOut" ] || [ "$netOut" = "(no networks)" ]; then
    echo "    keeping existing config"
  else
    netCount="$(printf '%s\n' "$netOut" | grep -c .)"
    echo "    keeping existing config ($netCount network(s) already defined)"
  fi
fi

echo "==> writing rc.d script $RCSCRIPT"
"$BIN" service install -config "$CONFIG" >/dev/null

if [ "$START" = 1 ]; then
  rcctl enable gravinet
  rcctl start gravinet
  [ "$WAS_RUNNING" = 1 ] && echo "==> restarted gravinet" || echo "==> started gravinet"
fi

# --- firewall (pf): open the underlay port inbound ----------------------------
# pf is enabled by default on OpenBSD, so add an explicit pass rule for the
# gravinet underlay (tcp+udp) rather than relying on the stock ruleset staying
# permissive. Appended (not quick) at the end of pf.conf so pf's last-match
# wins over a default-deny "block in" catch-all; validated before it's loaded
# so a bad edit can't lock the box out. Idempotent via the marker line.
echo "==> firewall (pf): opening inbound ${UNDERLAY_PORT}/tcp+udp"
PF_CONF=/etc/pf.conf
PF_MARKER="# gravinet: overlay underlay (added by install-openbsd.sh)"
if command -v pfctl >/dev/null 2>&1; then
  if [ -f "$PF_CONF" ] && grep -qF "$PF_MARKER" "$PF_CONF"; then
    echo "    rule already present in $PF_CONF"
  else
    tmp="$(mktemp)"
    pferr="$(mktemp)"
    [ -f "$PF_CONF" ] && cat "$PF_CONF" > "$tmp"
    {
      echo ""
      echo "$PF_MARKER"
      echo "pass in proto { tcp udp } to port ${UNDERLAY_PORT}"
    } >> "$tmp"
    # pferr (like tmp above) is a plain mktemp file, not BUILD_TMP's
    # directory — nothing else on this run's exit path removes it, so it's
    # cleared explicitly on every branch below instead of left at a fixed
    # /tmp path forever.
    if pfctl -nf "$tmp" 2>"$pferr"; then
      cat "$tmp" > "$PF_CONF"; rm -f "$tmp"
      if pfctl -f "$PF_CONF" 2>"$pferr"; then
        echo "    added pass rule and reloaded pf"
      else
        echo "    warning: wrote the rule to $PF_CONF but 'pfctl -f' failed:" >&2
        sed 's/^/      /' "$pferr" >&2
      fi
    else
      echo "    warning: proposed rule failed 'pfctl -nf' validation; left $PF_CONF untouched:" >&2
      sed 's/^/      /' "$pferr" >&2
      rm -f "$tmp"
    fi
    rm -f "$pferr"
  fi
else
  echo "    pfctl not found (unexpected on OpenBSD); open ${UNDERLAY_PORT}/tcp+udp inbound yourself"
fi

# --- DNS: make unbound the system resolver so conditional forwarding works ----
# gravinet's per-network DNS forwarding registers unbound forward zones at
# runtime (via unbound-control), which only takes effect if the host actually
# resolves through unbound. OpenBSD ships unbound in base but doesn't route the
# system through it by default (resolvd(8) points /etc/resolv.conf at
# DHCP/unwind). This step (on by default; skip with --no-unbound) wires that
# up. Everything it changes is marked and validated so it's reversible and
# can't wedge resolution silently.
RESOLV_CONF=/etc/resolv.conf
RESOLV_MARKER="# gravinet: system resolver via unbound (added by install-openbsd.sh)"
UNBOUND_CONF=/var/unbound/etc/unbound.conf
UNBOUND_RC_MARKER="# gravinet: remote-control for dynamic forward zones"
if [ "$SETUP_UNBOUND" = 1 ]; then
  echo "==> DNS: configuring unbound as the system resolver (default; pass --no-unbound to skip)"
  if ! command -v unbound-control >/dev/null 2>&1; then
    echo "    warning: unbound-control not found (unexpected on OpenBSD); skipping DNS setup" >&2
  else
    # 1) Enable unbound's control socket (idempotent via marker). A unix-socket
    #    control-interface needs no unbound-control-setup keys, unlike a TCP one.
    if [ -f "$UNBOUND_CONF" ] && grep -qF "$UNBOUND_RC_MARKER" "$UNBOUND_CONF"; then
      echo "    remote-control already configured in $UNBOUND_CONF"
    else
      utmp="$(mktemp)"
      unberr="$(mktemp)"
      [ -f "$UNBOUND_CONF" ] && cat "$UNBOUND_CONF" > "$utmp"
      {
        echo ""
        echo "$UNBOUND_RC_MARKER"
        echo "remote-control:"
        echo "    control-enable: yes"
        echo "    control-interface: /var/run/unbound.sock"
      } >> "$utmp"
      # unberr (like utmp above) is a plain mktemp file, not BUILD_TMP's
      # directory — nothing else on this run's exit path removes it, so it's
      # cleared explicitly below instead of left at a fixed /tmp path forever.
      if unbound-checkconf "$utmp" >"$unberr" 2>&1; then
        cat "$utmp" > "$UNBOUND_CONF"; rm -f "$utmp"
        echo "    enabled remote-control in $UNBOUND_CONF"
      else
        echo "    warning: adding remote-control failed unbound-checkconf; left $UNBOUND_CONF untouched:" >&2
        sed 's/^/      /' "$unberr" >&2
        rm -f "$utmp"
      fi
      rm -f "$unberr"
    fi

    # 2) Point resolv.conf at unbound and stop resolvd from clobbering it.
    #    Back up the current resolv.conf once (first run only) for reversal.
    if [ -f "$RESOLV_CONF" ] && ! grep -qF "$RESOLV_MARKER" "$RESOLV_CONF"; then
      cp "$RESOLV_CONF" "${RESOLV_CONF}.gravinet-backup" 2>/dev/null || true
      echo "    backed up $RESOLV_CONF -> ${RESOLV_CONF}.gravinet-backup"
    fi
    rcctl disable resolvd 2>/dev/null || true
    rcctl stop resolvd 2>/dev/null || true
    { echo "$RESOLV_MARKER"; echo "nameserver 127.0.0.1"; } > "$RESOLV_CONF"
    echo "    /etc/resolv.conf -> 127.0.0.1; resolvd disabled (so it won't overwrite it)"

    # 3) Enable + (re)start unbound and verify the control socket answers.
    rcctl enable unbound 2>/dev/null || true
    if rcctl restart unbound >/dev/null 2>&1; then
      if unbound-control status >/dev/null 2>&1; then
        echo "    unbound running; control socket responding — conditional forwarding is ready"
      else
        echo "    warning: unbound restarted but its control socket isn't responding yet;" >&2
        echo "    check: unbound-control status  (and /var/log/messages)" >&2
      fi
    else
      echo "    warning: could not start unbound; run 'rcctl restart unbound' and check the config" >&2
    fi
  fi
else
  echo "==> DNS: skipping unbound setup (--no-unbound); per-network DNS forwarding will stay inert"
fi

cat <<EOF


gravinet installed and running (rc.d: gravinet, via rcctl).
EOF

if [ -x "$BIN" ]; then
  INST_VER="$("$BIN" version 2>/dev/null | awk '{print $2}')"
  if [ -n "$INST_VER" ]; then
    if [ -n "$SRC_VER" ] && [ "$INST_VER" != "$SRC_VER" ]; then
      echo "Installed version: $INST_VER  (NOTE: source tree is $SRC_VER — installed binary does not match this source)"
    else
      echo "Installed version: $INST_VER"
    fi
  fi
fi

cat <<EOF

Web admin:  https://127.0.0.1:8443   (self-signed TLS — accept the warning)
            Log in with a system account — its password is checked via
            login_passwd(8) (BSD auth). The daemon runs as root so it can.
            Restrict who may log in with web_admin.allow_users.
            Remote box? tunnel it:  ssh -L 8443:127.0.0.1:8443 <user>@<host>

Firewall (pf): inbound ${UNDERLAY_PORT}/tcp+udp was opened in /etc/pf.conf
(marked '# gravinet'); see the 'firewall (pf)' step above for the actual result.
Changed the underlay port from the default? Add a matching rule and run
'pfctl -f /etc/pf.conf'. (A custom ruleset using 'block ... quick' ahead of
this rule may need it placed earlier.)

Conditional DNS forwarding: gravinet drives unbound(8) forward zones on OpenBSD
(same mechanism as FreeBSD's local-unbound). By default this installer makes
unbound the system resolver so that takes effect; the 'DNS' step above reports
what was done. Passed --no-unbound? Then the overlay, routing, firewall, QoS
and bandwidth limiting all still work — only per-network DNS forwarding stays
inert until you point this host's resolver at unbound (rerun without
--no-unbound, or see internal/resolver's enableHint). To revert later: restore
/etc/resolv.conf.gravinet-backup and 'rcctl enable resolvd && rcctl start resolvd'.

To join a mesh:
  1. Generate keys:    $BIN genkey -n 3
  2. Edit the config:  $CONFIG   (set keys, subnet/seeds, enable the network)
  3. Apply changes:    rcctl restart gravinet
  4. Status/logs:      rcctl check gravinet   (log path is in the config)
EOF
