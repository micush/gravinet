#!/usr/bin/env bash
# install-linux.sh — build (if needed) and install gravinet as a systemd service.
#
#   sudo ./install-linux.sh                 # build from source + install + start
#   sudo ./install-linux.sh --uninstall     # remove
#
# With no prebuilt binary present, this installs a Go toolchain if the host
# lacks one, builds the binary from the bundled source, then installs it.
#
# Options:
#   --bin PATH     use this prebuilt binary instead of building
#   --prefix DIR   install prefix (default: /usr/local)
#   --config PATH  config file (default: /etc/gravinet/config.json)
#   --user NAME    run-as user (default: root; the unit grants CAP_NET_ADMIN)
#   --no-start     install/enable but do not start the service now
#   --no-firewall  don't open gravinet's ports in firewalld. By default, on a host
#                  running firewalld (the default on RHEL/Rocky/Alma/CentOS/Fedora),
#                  this opens the underlay port (udp+tcp, default 65432) and the web
#                  admin port (tcp, default 8443) in the default zone. Without this
#                  firewalld silently drops inbound underlay packets, so peers can
#                  only ever reach this node via a relay (if at all), and managed-peer
#                  proxying/speedtest to its web admin over the overlay is refused.
#                  Ports are read from the config, not assumed.
#   --no-systemd-resolved
#                  don't enable systemd-resolved. By default, if it isn't already the
#                  active resolver, this installs (RHEL 9+/Fedora ship it as its own
#                  package), enables and starts it, points /etc/resolv.conf at its
#                  stub, and tells NetworkManager to hand DNS to it. gravinet's
#                  per-network DNS forwarding is implemented on Linux *only* via
#                  systemd-resolved's per-link routing domains (resolvectl), and
#                  RHEL/Rocky/Alma/CentOS do not enable it by default — NetworkManager
#                  writes /etc/resolv.conf itself. Without it, every DNS sync fails
#                  with "Failed to set DNS configuration: The name is not activatable"
#                  (that's D-Bus saying org.freedesktop.resolve1 has nothing to
#                  activate). Pass this if you don't use DNS forwarding and don't want
#                  this host's DNS stack changed.
set -euo pipefail

PREFIX=/usr/local
CONFIG=/etc/gravinet/config.json
SRC=""
RUNUSER=""
START=1
ACTION=install
FIREWALL=1
ENABLE_RESOLVED=1
UNDERLAY_PORT_DEFAULT=65432 # config.DefaultUDPPort
WEB_PORT_DEFAULT=8443       # config.Default()'s web_admin.listen
REPO="$(cd "$(dirname "$0")/.." && pwd)" # repo root (parent of install/)
GO_MIN_MINOR=21                          # go.mod is 1.22; Go >=1.21 auto-fetches it

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
    --user) RUNUSER="$2"; shift ;;
    --no-start) START=0 ;;
    --start) START=1 ;;
    --no-firewall) FIREWALL=0 ;;
    --firewall) FIREWALL=1 ;;                     # now the default; kept as a harmless no-op for anyone already scripting it
    --no-systemd-resolved) ENABLE_RESOLVED=0 ;;
    --enable-systemd-resolved) ENABLE_RESOLVED=1 ;; # ditto
    -h|--help) sed -n '2,40p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "error: run as root (sudo)" >&2; exit 1; }
command -v systemctl >/dev/null || { echo "error: systemd (systemctl) required" >&2; exit 1; }

BIN="$PREFIX/bin/gravinet"
PKGMAN="$PREFIX/bin/pkgman"
MESHPING="$PREFIX/bin/meshping"
SERVICE=gravinet

# bin_version runs "<bin> version" and extracts just the version field from
# its "gravinet NNN (commit) os/arch" output (see cmd/gravinet/main.go's
# version subcommand) — the same parse the post-install confirmation below
# already used, just also applied to whatever's installed *before* this run
# touches anything.
bin_version() {
  [ -x "$1" ] || return 0
  "$1" version 2>/dev/null | awk '{print $2}'
}

# source_version extracts the version string baked into the source
# (cmd/gravinet/main.go: `version = "NNN"`), so you can see exactly what this
# tree will install before it builds. Empty if the line can't be found.
source_version() {
  sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
    "$REPO/cmd/gravinet/main.go" 2>/dev/null | head -1
}
SRC_VER="$(source_version)"

# --- distro / firewalld / systemd-resolved helpers ----------------------------

# Neither step below sniffs /etc/os-release for the distro, though both exist
# because of RHEL/Rocky/Alma/CentOS/Fedora specifically: firewalld runs by default
# there and is usually absent on Debian/Ubuntu, and systemd-resolved is close to
# the inverse — enabled by default on Ubuntu, and on Debian up through 12
# (bookworm) it shipped bundled inside the base systemd package, needing at most
# `systemctl enable --now`. Debian 13 (trixie) split it into its own
# `systemd-resolved` package that isn't pulled in by default, so on a fresh
# trixie host the unit genuinely isn't there yet and pkg_install's job (below)
# is the difference between this working and the "not activatable" warning
# further down firing on a distro pair this whole comment used to treat as a
# non-issue. Asking the *state* ("is firewalld running?", "is resolved
# active?") rather than guessing the distro is still what's right here — it's
# automatically correct on hosts that don't fit the pattern, and it's what
# caught this: a RHEL box that already runs resolved is left alone either way.

# pkg_install installs one or more packages by whatever name they're called
# in *this* host's package manager. Right now it only has one caller
# (ensure_resolved, for the systemd-resolved package), and "systemd-resolved"
# happens to be the correct package name on every distro that ships it
# separately at all — Debian/Ubuntu (verified against Debian trixie's actual
# archive) and Fedora/RHEL alike. Arch and any distro where resolved ships
# bundled inside the base systemd package never reach this at all: the
# systemctl-cat check in ensure_resolved already succeeds before pkg_install
# would be called, so the pacman branch below only matters if that
# assumption is ever wrong for a particular install.
pkg_install() {
  if command -v apt-get >/dev/null; then
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" >/dev/null 2>&1
  elif command -v dnf >/dev/null; then dnf install -y "$@" >/dev/null 2>&1
  elif command -v yum >/dev/null; then yum install -y "$@" >/dev/null 2>&1
  elif command -v zypper >/dev/null; then zypper --non-interactive install "$@" >/dev/null 2>&1
  elif command -v pacman >/dev/null; then pacman -Sy --noconfirm "$@" >/dev/null 2>&1
  else return 1
  fi
}

# cfg_underlay_port / cfg_web_port read the ports actually in effect out of the
# config rather than assuming the defaults — someone who moved the underlay off
# 65432 needs *their* port opened, not the one the docs mention. Falls back to the
# built-in defaults when the config has no explicit value (the scaffolded config
# omits fields that are at their default) or can't be read.
cfg_underlay_port() {
  local p=""
  [ -f "$CONFIG" ] && p="$(sed -n 's/.*"primary_port"[[:space:]]*:[[:space:]]*\([0-9]\{1,\}\).*/\1/p' "$CONFIG" | head -1)"
  echo "${p:-$UNDERLAY_PORT_DEFAULT}"
}
cfg_web_port() {
  local p=""
  [ -f "$CONFIG" ] && p="$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"[^"]*:\([0-9]\{1,\}\)".*/\1/p' "$CONFIG" | head -1)"
  echo "${p:-$WEB_PORT_DEFAULT}"
}

firewalld_running() {
  command -v firewall-cmd >/dev/null || return 1
  systemctl is-active --quiet firewalld 2>/dev/null || return 1
  return 0
}

# firewalld_ports prints the ports gravinet needs open, one per line, in
# firewall-cmd's port/proto form:
#   - the underlay port on BOTH udp and tcp: udp is the primary transport, and tcp
#     is the fallback the TLS transport uses where udp is blocked or unusable.
#   - the web admin port on tcp: not for browsers (it binds 127.0.0.1 by default)
#     but for *overlay* traffic — a Manager peer proxies /api calls, and the
#     speedtest streams, to this node's web admin across the mesh interface, which
#     lands in firewalld's default zone like any other inbound packet and is
#     dropped without this.
firewalld_ports() {
  printf '%s/udp\n%s/tcp\n%s/tcp\n' "$(cfg_underlay_port)" "$(cfg_underlay_port)" "$(cfg_web_port)"
}

open_firewalld() {
  if ! firewalld_running; then
    if command -v firewall-cmd >/dev/null; then
      echo "    firewalld is installed but not running; nothing to open"
    else
      echo "    firewalld not installed; nothing to open (if you run another firewall, open the ports below yourself)"
    fi
    return 0
  fi
  local zone changed=0 p
  zone="$(firewall-cmd --get-default-zone 2>/dev/null || echo public)"
  while read -r p; do
    [ -n "$p" ] || continue
    if firewall-cmd --permanent --zone="$zone" --query-port="$p" >/dev/null 2>&1; then
      echo "    $p already open in zone $zone"
      continue
    fi
    if firewall-cmd --permanent --zone="$zone" --add-port="$p" >/dev/null 2>&1; then
      echo "    opened $p in zone $zone"
      changed=1
    else
      echo "    warning: could not open $p in zone $zone" >&2
    fi
  done <<EOF
$(firewalld_ports)
EOF
  # --permanent only writes the config; without a reload the running firewall is
  # unchanged and this whole step would appear to have worked while still dropping
  # every packet until the next reboot.
  if [ "$changed" = 1 ]; then
    firewall-cmd --reload >/dev/null 2>&1 && echo "    reloaded firewalld" \
      || echo "    warning: wrote the rules but could not reload firewalld; run 'firewall-cmd --reload'" >&2
  fi
}

close_firewalld() {
  firewalld_running || return 0
  local zone p removed=0
  zone="$(firewall-cmd --get-default-zone 2>/dev/null || echo public)"
  while read -r p; do
    [ -n "$p" ] || continue
    if firewall-cmd --permanent --zone="$zone" --query-port="$p" >/dev/null 2>&1; then
      firewall-cmd --permanent --zone="$zone" --remove-port="$p" >/dev/null 2>&1 && { echo "    closed $p in zone $zone"; removed=1; }
    fi
  done <<EOF
$(firewalld_ports)
EOF
  [ "$removed" = 1 ] && firewall-cmd --reload >/dev/null 2>&1 || true
}

resolved_active() { systemctl is-active --quiet systemd-resolved 2>/dev/null; }

# stub_resolv_conf points /etc/resolv.conf at systemd-resolved's stub. Without
# this, resolved runs but nothing on the host actually asks it anything, so a
# routing domain registered on the mesh link is never consulted and DNS forwarding
# looks broken in a completely different way (queries just go to the old servers).
stub_resolv_conf() {
  local stub=/run/systemd/resolve/stub-resolv.conf
  [ -e "$stub" ] || return 0
  if [ "$(readlink -f /etc/resolv.conf 2>/dev/null || true)" = "$(readlink -f "$stub")" ]; then
    echo "    /etc/resolv.conf already points at systemd-resolved's stub"
    return 0
  fi
  if [ -e /etc/resolv.conf ] && [ ! -L /etc/resolv.conf ]; then
    cp -a /etc/resolv.conf "/etc/resolv.conf.gravinet-backup" 2>/dev/null || true
    echo "    backed up the previous /etc/resolv.conf to /etc/resolv.conf.gravinet-backup"
  fi
  ln -sf "$stub" /etc/resolv.conf && echo "    pointed /etc/resolv.conf at $stub"
}

# nm_hand_dns_to_resolved stops NetworkManager overwriting /etc/resolv.conf with
# its own server list. On RHEL/Rocky/Alma/CentOS NM owns resolv.conf by default,
# so without this drop-in the symlink above gets clobbered on the next connection
# change and DNS forwarding breaks again later, with nothing obviously to blame.
nm_hand_dns_to_resolved() {
  systemctl is-active --quiet NetworkManager 2>/dev/null || return 0
  local d=/etc/NetworkManager/conf.d f=/etc/NetworkManager/conf.d/10-gravinet-resolved.conf
  [ -d "$d" ] || mkdir -p "$d"
  if [ -f "$f" ]; then
    echo "    NetworkManager already handing DNS to systemd-resolved (gravinet drop-in present)"
    return 0
  fi
  cat > "$f" <<'NMEOF'
# Installed by gravinet: hand DNS to systemd-resolved, which gravinet's
# per-network DNS forwarding registers routing domains with (resolvectl).
# Without this, NetworkManager writes /etc/resolv.conf itself and the mesh's
# DNS servers are never consulted. Remove this file to revert.
[main]
dns=systemd-resolved
NMEOF
  echo "    wrote $f (dns=systemd-resolved)"
  systemctl reload NetworkManager >/dev/null 2>&1 || systemctl restart NetworkManager >/dev/null 2>&1 || \
    echo "    warning: could not reload NetworkManager; do it yourself for the DNS change to stick" >&2
}

ensure_resolved() {
  if resolved_active; then
    echo "    systemd-resolved is already running"
    stub_resolv_conf
    nm_hand_dns_to_resolved
    return 0
  fi
  # RHEL 9+ and Fedora ship systemd-resolved as its own package, split out of
  # systemd; on older RHEL/CentOS the unit is already there, just not enabled.
  if ! systemctl cat systemd-resolved.service >/dev/null 2>&1; then
    echo "    systemd-resolved is not installed; installing it"
    pkg_install systemd-resolved || true
  fi
  if ! systemctl cat systemd-resolved.service >/dev/null 2>&1; then
    echo "    warning: systemd-resolved is unavailable on this host; gravinet's per-network DNS" >&2
    echo "    forwarding (if any network uses it) will fail with \"The name is not activatable\"" >&2
    return 0
  fi
  if systemctl enable --now systemd-resolved >/dev/null 2>&1; then
    echo "    enabled and started systemd-resolved"
    stub_resolv_conf
    nm_hand_dns_to_resolved
  else
    echo "    warning: could not enable systemd-resolved; DNS forwarding will not work" >&2
  fi
}


# NEW_VER is what this run would actually install: an explicit --bin's own
# version if one was given (no need to build anything to find that out), or
# else the source tree's version, which is exactly what building it now would
# produce — so this check runs before the (possibly slow) Go bootstrap and
# build, not after it.
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
  echo "==> stopping and disabling $SERVICE"
  systemctl disable --now "$SERVICE" 2>/dev/null || true
  rm -f /etc/systemd/system/${SERVICE}.service
  systemctl daemon-reload 2>/dev/null || true
  rm -f "$BIN" "$PKGMAN" "$MESHPING" /etc/pam.d/gravinet
  rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"; rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true
  # Close the ports this installer opened. Done while the config is still around,
  # since that's where the port numbers come from.
  echo "==> firewalld"
  close_firewalld
  # Hand DNS back to NetworkManager. systemd-resolved itself is left enabled: it's
  # a general-purpose resolver that other things may now depend on, and disabling
  # it here could take the host's DNS down on the way out.
  if [ -f /etc/NetworkManager/conf.d/10-gravinet-resolved.conf ]; then
    rm -f /etc/NetworkManager/conf.d/10-gravinet-resolved.conf
    systemctl reload NetworkManager >/dev/null 2>&1 || true
    echo "==> removed the NetworkManager dns=systemd-resolved drop-in (systemd-resolved left enabled)"
  fi
  echo "==> removed $BIN, $PKGMAN, $MESHPING, the service unit, the PAM file, and the docs (config at $CONFIG left in place)"
  exit 0
fi

# --- Go toolchain bootstrap + build-from-source --------------------------------

go_minor() { # echo Go minor version (integer), or -1 if go is missing/unparseable
  command -v go >/dev/null 2>&1 || { echo -1; return; }
  local v; v="$(go version 2>/dev/null | awk '{print $3}')"; v="${v#go}"
  local m="${v#*.}"; m="${m%%.*}"
  case "$m" in '' | *[!0-9]*) echo -1 ;; *) echo "$m" ;; esac
}

go_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    armv6l | armv7l | arm) echo armv6l ;;
    i386 | i686) echo 386 ;;
    *) echo "" ;;
  esac
}

pkg_install_go() {
  echo "==> installing Go via the system package manager"
  if command -v apt-get >/dev/null; then
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y golang-go
  elif command -v dnf >/dev/null; then dnf install -y golang
  elif command -v yum >/dev/null; then yum install -y golang
  elif command -v pacman >/dev/null; then pacman -Sy --noconfirm go
  elif command -v zypper >/dev/null; then zypper --non-interactive install go
  elif command -v apk >/dev/null; then apk add --no-cache go
  else return 1; fi
}

official_install_go() {
  command -v curl >/dev/null || { echo "    need curl to download Go" >&2; return 1; }
  command -v tar >/dev/null || { echo "    need tar to unpack Go" >&2; return 1; }
  local ga; ga="$(go_arch)"; [ -n "$ga" ] || { echo "    unsupported arch for Go" >&2; return 1; }
  local ver; ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1)"
  [ -n "$ver" ] || { echo "    could not resolve latest Go version" >&2; return 1; }
  local tgz="${ver}.linux-${ga}.tar.gz"
  echo "==> downloading https://go.dev/dl/${tgz}"
  curl -fsSL "https://go.dev/dl/${tgz}" -o "/tmp/${tgz}" || return 1
  if command -v sha256sum >/dev/null; then
    local want; want="$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' 2>/dev/null \
      | grep -A6 "\"${tgz}\"" | grep -m1 '"sha256"' | sed -E 's/.*"sha256": *"([0-9a-f]+)".*/\1/')"
    if [ -n "$want" ]; then
      local got; got="$(sha256sum "/tmp/${tgz}" | awk '{print $1}')"
      [ "$got" = "$want" ] || { echo "    Go checksum mismatch" >&2; rm -f "/tmp/${tgz}"; return 1; }
      echo "    Go ${ver} checksum verified"
    fi
  fi
  rm -rf /usr/local/go && tar -C /usr/local -xzf "/tmp/${tgz}" && rm -f "/tmp/${tgz}"
  export PATH="/usr/local/go/bin:$PATH"
}

ensure_go() {
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null && return 0
  pkg_install_go || true
  hash -r 2>/dev/null || true
  [ -x /usr/local/go/bin/go ] && export PATH="/usr/local/go/bin:$PATH"
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null && return 0
  official_install_go || return 1
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null
}

have_cc() { command -v gcc >/dev/null || command -v cc >/dev/null || command -v clang >/dev/null; }

# can_link_pam is the authoritative "can this box build the cgo PAM auth?"
# check: it actually compiles AND links a tiny program against
# <security/pam_appl.h> / -lpam with the same compiler cgo will use. It replaces
# a former test that merely looked for the header at two hardcoded paths
# (/usr/include, /usr/local/include) — fragile in both directions. That test
# false-negatived whenever the header lived somewhere the compiler still
# searches but the script didn't hardcode (a nonstandard prefix, a distro's
# multiarch include dir), forcing a needless no-PAM build and printing the
# "built WITHOUT PAM" warning on a box that would in fact have built PAM fine;
# and it false-positived when the header was present but libpam wasn't
# linkable. Asking the compiler directly is the only check that matches what
# cgo does. On failure the compiler's own output is kept in PAM_PROBE_LOG so
# the reason for falling back to a no-PAM build is visible, not a mystery.
PAM_PROBE_LOG=""
can_link_pam() {
  local cc; cc="$(command -v gcc || command -v cc || command -v clang)" || return 1
  local t; t="$(mktemp -d)" || return 1
  cat > "$t/probe.c" <<'CEOF'
#include <security/pam_appl.h>
int main(void) { pam_handle_t *h = 0; (void)pam_start("gravinet", 0, 0, &h); return 0; }
CEOF
  if "$cc" "$t/probe.c" -lpam -o "$t/probe" >"$t/log" 2>&1; then
    PAM_PROBE_LOG=""; rm -rf "$t"; return 0
  fi
  PAM_PROBE_LOG="$(cat "$t/log" 2>/dev/null)"
  rm -rf "$t"
  return 1
}

install_build_deps() { # C toolchain + PAM headers, for the cgo build that enables PAM
  echo "==> installing C toolchain + PAM headers (for web-admin PAM auth)"
  if command -v apt-get >/dev/null; then
    apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y gcc libpam0g-dev
  elif command -v dnf >/dev/null; then dnf install -y gcc pam-devel
  elif command -v yum >/dev/null; then yum install -y gcc pam-devel
  elif command -v pacman >/dev/null; then pacman -Sy --noconfirm gcc pam
  elif command -v zypper >/dev/null; then zypper --non-interactive install gcc pam-devel
  elif command -v apk >/dev/null; then apk add --no-cache build-base linux-pam-dev
  else return 1; fi
}

write_pam_service() {
  local f=/etc/pam.d/gravinet
  if [ -f /etc/pam.d/common-auth ]; then
    printf '# gravinet web admin\nauth     include common-auth\naccount  include common-account\n' > "$f"
  elif [ -f /etc/pam.d/system-auth ]; then
    printf '# gravinet web admin\nauth     include system-auth\naccount  include system-auth\n' > "$f"
  else
    printf '# gravinet web admin\nauth     required pam_unix.so\naccount  required pam_unix.so\n' > "$f"
  fi
  echo "    wrote PAM service /etc/pam.d/gravinet"
}

build_from_source() {
  [ -f "$REPO/go.mod" ] || { echo "error: no prebuilt binary and no source tree at $REPO" >&2; exit 1; }
  ensure_go || { echo "error: could not obtain a Go toolchain (>=1.${GO_MIN_MINOR})" >&2; exit 1; }
  # Web-admin PAM auth needs a cgo build (libpam). Try to get a C toolchain +
  # headers; if unavailable, fall back to a static build (local auth only).
  local cgo=0
  if can_link_pam; then
    cgo=1
  elif install_build_deps && can_link_pam; then
    cgo=1
  else
    echo "    warning: cannot compile+link the PAM auth module here; building without PAM"
    echo "             (web admin will need a local user via 'gravinet genpass')"
    [ -n "$PAM_PROBE_LOG" ] && printf '%s\n' "$PAM_PROBE_LOG" | sed 's/^/             pam probe: /'
  fi
  echo "==> building gravinet from source with $(go version | awk '{print $3}') (cgo=$cgo)"
  BUILD_TMP="$(mktemp -d)"
  local out="$BUILD_TMP/gravinet"
  # GOCACHE/GOPATH default to under $HOME (here, root's) when unset, and go
  # build never cleans either one up itself — it's a persistent cache, by
  # design, for a machine that keeps rebuilding the same module. On a
  # one-shot install/upgrade that isn't going to happen again anytime soon,
  # that's not a cache, it's litter left behind on every run — the same class
  # of bug reported (and fixed) on the OpenBSD installer, present here too.
  # Redirect both under BUILD_TMP instead, which the EXIT trap above already
  # removes on every exit path.
  ( cd "$REPO" && CGO_ENABLED=$cgo GOTOOLCHAIN=auto \
      GOCACHE="$BUILD_TMP/.gocache" GOPATH="$BUILD_TMP/.gopath" \
      go build -buildvcs=false -trimpath -ldflags "-s -w" -o "$out" ./cmd/gravinet ) \
    || { echo "error: build failed" >&2; exit 1; }
  SRC="$out"
  PAM_BUILT=$cgo
  echo "    built $SRC"
}

PAM_BUILT=0
EXPLICIT_BIN=${EXPLICIT_BIN:-0}

# Resolve the binary. The default is to BUILD FROM SOURCE with cgo (CGO_ENABLED=1)
# so the web admin's PAM login works out of the box. A prebuilt release binary is
# cross-compiled without cgo (no PAM), so it's only a fallback when we can't
# build. An explicit --bin is always respected as-is.
binary_has_pam() {
  # Ask the binary itself first — it's the only thing that reliably knows
  # what it was built with. This is what the "PAM works despite the warning"
  # bug was: the ldd fallback below can be fooled (e.g. by a toolchain that
  # links libpam statically, so it never shows up as a dynamic dependency
  # even though the code is fully compiled in and functional) — and this
  # function used to be the *only* check, silently overwriting a PAM_BUILT
  # value that build_from_source had already gotten right moments earlier.
  # A binary built before this self-report existed just won't match either
  # "pam=yes" or "pam=no" below, so it falls through to the old heuristic —
  # no worse off than before for an old binary, and correct for a new one.
  #
  # This self-report requires actually *executing* the binary, though, and
  # that's not guaranteed to work from wherever "$1" currently sits:
  # build_from_source() stages the freshly-built binary under `mktemp -d`,
  # i.e. under /tmp (or $TMPDIR) by default, and on hosts that mount /tmp
  # noexec (common under hardened configs, e.g. systemd-hardened distros or
  # a dedicated noexec /tmp), exec-ing anything there fails with a plain
  # "Permission denied" — silently, since stderr is discarded below — even
  # though the binary is completely fine and will run once installed to
  # $BIN. The genuinely bad news is that `ldd` doesn't help: for a normal
  # (non-setuid) ELF, ldd works by executing the target with
  # LD_TRACE_LOADED_OBJECTS=1 set, which a noexec mount blocks exactly as
  # hard, so the "fallback" used to fail the identical way — printing "not a
  # dynamic executable" — making it indistinguishable from PAM genuinely
  # being absent. Net effect: a binary correctly built with cgo+PAM (go
  # build itself never needed to *execute* anything, so that part always
  # succeeded) got reported as PAM-less, and the installer printed the "built
  # WITHOUT PAM" warning every single time /tmp was noexec, regardless of
  # whether PAM was actually compiled in.
  #
  # readelf/objdump fix that: they read the ELF dynamic section straight off
  # disk, so they need no execute permission on the file at all. That's a
  # direct, static replacement for what ldd was doing (both ultimately just
  # report DT_NEEDED entries) — including on the release binary here, which
  # is built with `-ldflags "-s -w"`: -s strips the symbol table, but not
  # the dynamic section, so the libpam.so dependency is still visible.
  local out
  out="$("$1" version 2>/dev/null)" || out=""
  case "$out" in
    *" pam=yes"*) return 0 ;;
    *" pam=no"*)  return 1 ;;
  esac
  if command -v readelf >/dev/null 2>&1; then
    readelf -d "$1" 2>/dev/null | grep -qi 'libpam' && return 0
  elif command -v objdump >/dev/null 2>&1; then
    objdump -p "$1" 2>/dev/null | grep -qi 'libpam' && return 0
  fi
  ldd "$1" 2>/dev/null | grep -qi 'libpam'
}

if [ "$EXPLICIT_BIN" = 1 ] && [ -n "$SRC" ]; then
  [ -f "$SRC" ] || { echo "error: --bin $SRC not found" >&2; exit 1; }
  binary_has_pam "$SRC" || echo "    note: --bin lacks PAM; web admin will need a local user (gravinet genpass)"
elif [ -f "$REPO/go.mod" ]; then
  build_from_source            # default: cgo build with PAM
else
  case "$(uname -m)" in
    x86_64 | amd64) a=amd64 ;;
    aarch64 | arm64) a=arm64 ;;
    armv7l | armv6l | arm) a=arm ;;
    *) a="" ;;
  esac
  here="$(cd "$(dirname "$0")" && pwd)"
  for cand in "$here/gravinet-linux-$a" "$here/gravinet"; do
    [ -f "$cand" ] && { SRC="$cand"; break; }
  done
  [ -n "$SRC" ] && [ -f "$SRC" ] || { echo "error: no source tree at $REPO and no prebuilt binary found" >&2; exit 1; }
  # A prebuilt is cross-compiled (no PAM). If we can build, prefer a PAM binary.
  if ! binary_has_pam "$SRC" && { can_link_pam || { install_build_deps && can_link_pam; }; }; then
    echo "==> prebuilt binary lacks PAM; building a PAM-enabled one from source"
    build_from_source
  fi
fi

# Record what the final binary can actually do. This re-derives PAM_BUILT
# from $SRC rather than trusting build_from_source's local $cgo unconditionally,
# since $SRC isn't always what build_from_source produced (the --bin and
# found-a-prebuilt paths above set it directly). That's only safe because
# binary_has_pam now asks the binary itself first — it used to be ldd-only,
# which could (and did) silently overwrite a correct PAM_BUILT=1 with a wrong
# 0 for a binary that actually had PAM compiled in.
if binary_has_pam "$SRC"; then PAM_BUILT=1; else PAM_BUILT=0; fi

# Upgrade-in-place: stop a running instance before replacing its binary.
WAS_ACTIVE=0
if systemctl is-active --quiet "$SERVICE" 2>/dev/null; then
  WAS_ACTIVE=1
  echo "==> $SERVICE is running; stopping before upgrade"
  systemctl stop "$SERVICE" 2>/dev/null || true
fi

echo "==> installing $SRC -> $BIN"
install -D -m 0755 "$SRC" "$BIN"

# Build scratch (BUILD_TMP: Go build+module caches) is done once the binary is
# in place — remove it now instead of holding it through the rest of the run;
# the EXIT trap still covers earlier exit paths. Guarded + cleared so the
# trap's later pass no-ops.
if [ -n "$BUILD_TMP" ]; then
  rm -rf "$BUILD_TMP"
  BUILD_TMP=""
fi

SCRIPTDIR="$(dirname "$0")"
echo "==> installing pkgman -> $PKGMAN"
if [ -f "$SCRIPTDIR/pkgman" ]; then
  install -D -m 0755 "$SCRIPTDIR/pkgman" "$PKGMAN"
else
  echo "    note: pkgman not found in the package; skipping"
fi

echo "==> installing meshping -> $MESHPING"
if [ -f "$SCRIPTDIR/meshping" ]; then
  install -D -m 0755 "$SCRIPTDIR/meshping" "$MESHPING"
else
  echo "    note: meshping not found in the package; skipping"
fi

echo "==> installing docs (README, LICENSE, getting-started.md)"
DOCDIR="$PREFIX/share/doc/gravinet"
for doc in README.md LICENSE getting-started.md; do
  src=""
  for cand in "$REPO/$doc" "$SCRIPTDIR/$doc" "$SCRIPTDIR/../$doc"; do
    [ -f "$cand" ] && { src="$cand"; break; }
  done
  if [ -n "$src" ]; then
    install -D -m 0644 "$src" "$DOCDIR/$doc"
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
  # Say what's actually in it, not just that it exists - "keeping existing
  # config" alone reads the same whether that's expected (an upgrade) or a
  # surprise (e.g. a prior --purge silently failed to clear it, so networks
  # from before it "should" be a fresh install reappear with no obvious
  # explanation why). See the equivalent fix in install-windows.ps1.
  netOut="$("$BIN" network list -config "$CONFIG" 2>/dev/null || true)"
  if [ -z "$netOut" ] || [ "$netOut" = "(no networks)" ]; then
    echo "    keeping existing config"
  else
    netCount="$(printf '%s\n' "$netOut" | grep -c .)"
    echo "    keeping existing config ($netCount network(s) already defined)"
  fi
fi

echo "==> web admin PAM service"
write_pam_service

echo "==> firewalld"
if [ "$FIREWALL" = 1 ]; then
  open_firewalld
else
  echo "    --no-firewall passed; leaving firewalld alone. gravinet needs inbound"
  echo "    $(cfg_underlay_port)/udp + $(cfg_underlay_port)/tcp (underlay) and $(cfg_web_port)/tcp (web admin, over the"
  echo "    overlay: managed-peer proxying and speedtest) — open them yourself if a"
  echo "    firewall is in the way."
fi

echo "==> DNS forwarding (systemd-resolved)"
if [ "$ENABLE_RESOLVED" = 1 ]; then
  ensure_resolved
else
  cat <<'NOTE'
    note: skipped enabling systemd-resolved (--no-systemd-resolved was passed). DNS
          here is left as-is. gravinet's optional per-network DNS forwarding is
          implemented on Linux only via systemd-resolved's per-link routing domains
          (resolvectl), so without it a network using that feature will log:
              resolver: set dns on <iface>: ... The name is not activatable
          Enable it yourself if you end up wanting the feature:
              systemctl enable --now systemd-resolved
              ln -sf /run/systemd/resolve/stub-resolv.conf /etc/resolv.conf
          Skip this if you don't use gravinet's DNS-forwarding feature.
NOTE
fi

echo "==> writing systemd unit"
"$BIN" service install -config "$CONFIG" ${RUNUSER:+-user "$RUNUSER"} >/dev/null
if systemctl daemon-reload 2>/dev/null; then
  if systemctl enable "$SERVICE" >/dev/null 2>&1; then
    echo "    enabled $SERVICE (start on boot)"
  else
    echo "    warning: could not enable $SERVICE"
  fi
  if [ "$START" = 1 ]; then
    if systemctl restart "$SERVICE" 2>/dev/null; then
      [ "$WAS_ACTIVE" = 1 ] && echo "==> restarted $SERVICE" || echo "==> started $SERVICE"
    else
      echo "    warning: could not start $SERVICE; check 'journalctl -u $SERVICE'"
    fi
  fi
else
  echo "    warning: systemd not active here; unit written to /etc/systemd/system/${SERVICE}.service"
  echo "    once systemd is available: systemctl enable --now $SERVICE"
fi

if [ "$PAM_BUILT" = 1 ]; then
  web_auth="log in with a system account (PAM)"
else
  web_auth="PAM is NOT compiled in — see the warning below"
fi

cat <<EOF

gravinet installed and running ($SERVICE).
EOF

# Confirm what actually landed: the installed binary's own reported version,
# compared against the source version printed at the top.
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
            $web_auth
            Remote box? tunnel it:  ssh -L 8443:127.0.0.1:8443 <user>@<host>

To join a mesh:
  1. Generate keys:    $BIN genkey -n 3
  2. Edit the config:  $CONFIG   (set keys, subnet/seeds, enable the network)
  3. Apply changes:    systemctl restart $SERVICE
  4. Check status:     systemctl status $SERVICE  /  journalctl -u $SERVICE -f
EOF

if [ "$PAM_BUILT" != 1 ]; then
  cat >&2 <<EOF

  ********************************************************************************
  WARNING: this gravinet binary was built WITHOUT PAM support, so the web admin
  cannot authenticate system accounts and you will NOT be able to log in.
  This happens when a prebuilt binary is used or a C toolchain / libpam headers
  were unavailable at build time. To fix, either:
    - install gcc and the PAM dev headers, then re-run this installer from the
      source tree (it will build a PAM-enabled binary), or
    - create a local web user instead:
        $BIN genpass -user admin -pass 'yourpassword'
      add the printed object to web_admin.users, set web_admin.auth_mode "local",
      and restart: systemctl restart $SERVICE
  ********************************************************************************
EOF
fi

