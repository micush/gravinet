#!/bin/sh
# install-freebsd.sh — build (if needed) and install gravinet as an rc.d service.
#
#   sudo ./install-freebsd.sh                 # build from source + install + start
#   sudo ./install-freebsd.sh --uninstall     # remove
#
# With no prebuilt binary present, this installs a Go toolchain if the host
# lacks one, builds the binary from the bundled source, then installs it.
# Unlike macOS, FreeBSD's base system already ships a C compiler (clang) and
# PAM headers, so — unlike install-macos.sh — there's no Xcode-Command-Line-
# -Tools-equivalent bootstrap step here: a stock FreeBSD install can already
# build the PAM-enabled binary.
#
# Options:
#   --bin PATH           use this prebuilt binary instead of building
#   --prefix DIR         install prefix (default: /usr/local)
#   --config PATH        config file (default: /usr/local/etc/gravinet/config.json)
#   --no-start           install but do not start the daemon now
#   --no-local-unbound   don't touch FreeBSD's base-system local-unbound resolver.
#                        By default, if local-unbound isn't already configured on
#                        this host, this installer enables it (sysrc
#                        local_unbound_enable=YES && service local_unbound start)
#                        and drops a conf.d override disabling its DNSSEC
#                        validator (module-config: iterator) — needed because
#                        most internal/corporate resolvers don't return signed
#                        responses, which otherwise makes local-unbound SERVFAIL
#                        every query instead of relaying the (correct) answer.
#                        gravinet's optional per-network DNS forwarding needs
#                        local-unbound to route mesh-domain queries, the same way
#                        Linux relies on systemd-resolved and macOS on
#                        /etc/resolver, both already present with no setup step
#                        of their own and neither validating DNSSEC locally
#                        either. Pass this flag if you don't use that feature and
#                        don't want this host's system-wide DNS resolution
#                        changed (/etc/resolv.conf ends up pointing at
#                        127.0.0.1 once local-unbound takes over).
set -eu

PREFIX=/usr/local
CONFIG=/usr/local/etc/gravinet/config.json
SRC=""
START=1
ACTION=install
ENABLE_LOCAL_UNBOUND=1
RCSCRIPT=/usr/local/etc/rc.d/gravinet
REPO="$(cd "$(dirname "$0")/.." && pwd)"
GO_MIN_MINOR=21

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
    --no-local-unbound) ENABLE_LOCAL_UNBOUND=0 ;;
    --enable-local-unbound) ENABLE_LOCAL_UNBOUND=1 ;; # now the default; kept as a harmless no-op for anyone already scripting it
    -h|--help) sed -n '2,36p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "error: run as root (sudo)" >&2; exit 1; }

BIN="$PREFIX/bin/gravinet"
PKGMAN="$PREFIX/bin/pkgman"
MESHPING="$PREFIX/bin/meshping"

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
# tree will install before it builds. Empty if the line can't be found (e.g. a
# future refactor) — printing just skips rather than failing the install.
source_version() {
  sed -n 's/^[[:space:]]*version[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
    "$REPO/cmd/gravinet/main.go" 2>/dev/null | head -1
}
SRC_VER="$(source_version)"

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
  echo "==> stopping daemon"
  if [ -x "$RCSCRIPT" ]; then service gravinet onestop 2>/dev/null || true; fi
  rm -f "$RCSCRIPT" "$BIN" "$PKGMAN" "$MESHPING" /etc/pam.d/gravinet
  sysrc -x gravinet_enable >/dev/null 2>&1 || true
  rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"; rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true
  echo "==> removed $BIN, $PKGMAN, $MESHPING, $RCSCRIPT, the PAM file, and the docs (config at $CONFIG left in place)"
  exit 0
fi

# --- Go toolchain bootstrap + build-from-source --------------------------------

go_minor() {
  command -v go >/dev/null 2>&1 || { echo -1; return; }
  local v; v="$(go version 2>/dev/null | awk '{print $3}')"; v="${v#go}"
  local m="${v#*.}"; m="${m%%.*}"
  case "$m" in '' | *[!0-9]*) echo -1 ;; *) echo "$m" ;; esac
}

go_arch() {
  case "$(uname -m)" in
    arm64|aarch64) echo arm64 ;;
    amd64|x86_64) echo amd64 ;;
    *) echo "" ;;
  esac
}

# FreeBSD installs run as root (sudo), same reasoning as install-macos.sh: fetch
# Go straight from go.dev rather than via pkg, so the exact version is pinned
# and checksum-verified rather than whatever's currently in the ports tree.
official_install_go() {
  command -v fetch >/dev/null || command -v curl >/dev/null || { echo "    need fetch(1) or curl to download Go" >&2; return 1; }
  local ga; ga="$(go_arch)"; [ -n "$ga" ] || { echo "    unsupported arch for Go" >&2; return 1; }
  local get; if command -v curl >/dev/null; then get="curl -fsSL"; else get="fetch -qo -"; fi
  local ver; ver="$($get 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1)"
  [ -n "$ver" ] || { echo "    could not resolve latest Go version" >&2; return 1; }
  local tgz="${ver}.freebsd-${ga}.tar.gz"
  echo "==> downloading https://go.dev/dl/${tgz}"
  $get "https://go.dev/dl/${tgz}" > "/tmp/${tgz}" || return 1
  if command -v sha256 >/dev/null || command -v shasum >/dev/null; then
    local want; want="$($get 'https://go.dev/dl/?mode=json&include=all' 2>/dev/null \
      | grep -A6 "\"${tgz}\"" | grep -m1 '"sha256"' | sed -E 's/.*"sha256": *"([0-9a-f]+)".*/\1/')"
    if [ -n "$want" ]; then
      local got; if command -v sha256 >/dev/null; then got="$(sha256 -q "/tmp/${tgz}")"; else got="$(shasum -a 256 "/tmp/${tgz}" | awk '{print $1}')"; fi
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
  if command -v pkg >/dev/null && pkg install -y go >/dev/null 2>&1 && [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null; then
    return 0
  fi
  official_install_go || return 1
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null
}

have_cc() { command -v clang >/dev/null || command -v cc >/dev/null || command -v gcc >/dev/null; }

# Unlike macOS, FreeBSD's base system already ships clang and the PAM headers
# (security/pam_appl.h) — no separate SDK/Command-Line-Tools download exists
# to trigger here. This is just a friendly nudge for the unusual case (a
# stripped-down jail or minimal install) where the base toolchain is missing.
install_build_deps() {
  echo "    no C compiler found; this is unusual for FreeBSD (clang ships in the" >&2
  echo "    base system by default) — installing llvm from packages" >&2
  command -v pkg >/dev/null && pkg install -y llvm >/dev/null 2>&1
  have_cc
}

# FreeBSD's base system PAM (OpenPAM — the same implementation macOS uses,
# which is why gravinet's cgo PAM code covers both with one build tag) also
# ships pam_unix.so for traditional local-account authentication, same module
# name as Linux's.
write_pam_service() {
  cat > /etc/pam.d/gravinet <<'EOF'
# gravinet web admin
auth       required       pam_unix.so
account    required       pam_unix.so
EOF
  echo "    wrote PAM service /etc/pam.d/gravinet"
}

build_from_source() {
  [ -f "$REPO/go.mod" ] || { echo "error: no prebuilt binary and no source tree at $REPO" >&2; exit 1; }
  ensure_go || { echo "error: could not obtain a Go toolchain (>=1.${GO_MIN_MINOR})" >&2; exit 1; }
  local cgo=0
  if have_cc; then
    cgo=1
  elif install_build_deps; then
    cgo=1
  else
    echo "    warning: no C compiler available; building without PAM (web admin will need 'gravinet genpass')"
  fi
  echo "==> building gravinet from source with $(go version | awk '{print $3}') (cgo=$cgo)"
  BUILD_TMP="$(mktemp -d)"
  local out="$BUILD_TMP/gravinet"
  # GOCACHE/GOPATH default to under $HOME (here, root's) when unset, and go
  # build never cleans either one up itself — it's a persistent cache, by
  # design, for a machine that keeps rebuilding the same module. On a
  # one-shot install/upgrade that isn't going to happen again anytime soon,
  # that's not a cache, it's litter left behind on every run. Redirect both
  # under BUILD_TMP instead, which the EXIT trap above already removes on
  # every exit path.
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

binary_has_pam() {
  # See install-linux.sh's copy of this function for the full story: the
  # objdump fallback below is a heuristic that can be fooled (e.g. static
  # linking), and used to be the only check — silently overwriting a
  # correct PAM_BUILT value from build_from_source. Ask the binary itself
  # first; only fall back for a binary built before this self-report existed.
  local out
  out="$("$1" version 2>/dev/null)" || out=""
  case "$out" in
    *" pam=yes"*) return 0 ;;
    *" pam=no"*)  return 1 ;;
  esac
  command -v objdump >/dev/null && objdump -p "$1" 2>/dev/null | grep -qi 'libpam'
}

if [ "$EXPLICIT_BIN" = 1 ] && [ -n "$SRC" ]; then
  [ -f "$SRC" ] || { echo "error: --bin $SRC not found" >&2; exit 1; }
  binary_has_pam "$SRC" || echo "    note: --bin lacks PAM; web admin will need a local user (gravinet genpass)"
elif [ -f "$REPO/go.mod" ]; then
  build_from_source
else
  case "$(uname -m)" in
    amd64|x86_64) a=amd64 ;;
    arm64|aarch64) a=arm64 ;;
    *) a="" ;;
  esac
  here="$(cd "$(dirname "$0")" && pwd)"
  for cand in "$here/gravinet-freebsd-$a" "$here/gravinet"; do
    [ -f "$cand" ] && { SRC="$cand"; break; }
  done
  [ -n "$SRC" ] && [ -f "$SRC" ] || { echo "error: no source tree at $REPO and no prebuilt binary found" >&2; exit 1; }
  if ! binary_has_pam "$SRC" && [ -f "$REPO/go.mod" ] && { have_cc || install_build_deps; }; then
    echo "==> prebuilt binary lacks PAM; building a PAM-enabled one from source"
    build_from_source
  fi
fi
# Record what the final binary can actually do — safe now that
# binary_has_pam checks the binary's own self-report first (see above).
if binary_has_pam "$SRC"; then PAM_BUILT=1; else PAM_BUILT=0; fi

# Upgrade-in-place: stop a running daemon before replacing its binary. Unlike
# Windows, a running FreeBSD binary can be unlinked/replaced while still
# executing (the old inode stays alive until the process exits) — this step
# is about not leaving the old build running under a replaced rc.d script,
# not about a file-locking requirement the way it is on Windows.
WAS_RUNNING=0
if [ -x "$RCSCRIPT" ] && service gravinet onestatus >/dev/null 2>&1; then
  WAS_RUNNING=1
  echo "==> daemon is running; stopping before upgrade"
  service gravinet onestop 2>/dev/null || true
fi

echo "==> installing $SRC -> $BIN"
install -d -m 0755 "$PREFIX/bin"
install -m 0755 "$SRC" "$BIN"

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
  # Say what's actually in it, not just that it exists - "keeping existing
  # config" alone reads the same whether that's expected (an upgrade) or a
  # surprise (e.g. a prior --uninstall's config removal silently failed, so
  # networks from before it "should" be a fresh install reappear with no
  # obvious explanation why). See the equivalent fix in install-windows.ps1.
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

LOCAL_UNBOUND_CONF=/var/unbound/unbound.conf
DNSSEC_OVERRIDE=/var/unbound/conf.d/gravinet-no-dnssec.conf
local_unbound_configured() { [ -f "$LOCAL_UNBOUND_CONF" ]; }
dnssec_override_present() { [ -f "$DNSSEC_OVERRIDE" ]; }

# write_dnssec_override drops gravinet's DNSSEC-validator-disable drop-in and
# restarts local-unbound to apply it. Called both on first-ever setup and on
# every later run where local-unbound was already configured by something
# else (or by an earlier run of this installer) but the override is missing —
# e.g. if it was deleted by hand, or local-unbound was set up before this
# installer knew to write it. Treating "is local-unbound configured" and "is
# our override in place" as separate, independently-checked conditions is the
# point: the first only needs doing once, but the second needs to hold every
# time this script runs, or local-unbound quietly reverts to validating and
# every query SERVFAILs again with no visible sign why.
write_dnssec_override() {
  # DNSSEC validation is on by default, but only works if the upstream
  # forwarder actually returns signed responses. Most internal/corporate
  # resolvers don't — and a validator that can never "prime" its root trust
  # anchor discards every answer as unverifiable and returns SERVFAIL, even
  # though the forwarder answered correctly. Neither of gravinet's other two
  # platforms validate DNSSEC locally either (systemd-resolved on Linux and
  # /etc/resolver on macOS just relay), so dropping the validator here keeps
  # local-unbound a plain forwarding cache — parity with those, not a
  # workaround specific to this host.
  mkdir -p /var/unbound/conf.d
  cat > "$DNSSEC_OVERRIDE" <<'EOF'
server:
    module-config: "iterator"
EOF
  if service local_unbound restart; then
    echo "    disabled DNSSEC validation (module-config: iterator) so forwarding to a"
    echo "    non-validating upstream resolver doesn't SERVFAIL every query"
  else
    echo "    warning: wrote conf.d/gravinet-no-dnssec.conf but could not restart" >&2
    echo "    local-unbound to apply it; run 'service local_unbound restart' yourself" >&2
  fi
}

echo "==> DNS forwarding (local-unbound)"
if local_unbound_configured; then
  echo "    local-unbound is already configured on this host"
  if [ "$ENABLE_LOCAL_UNBOUND" = 1 ]; then
    if dnssec_override_present; then
      echo "    DNSSEC-validator override already in place"
    else
      echo "    DNSSEC-validator override missing (removed by hand, or set up before"
      echo "    this installer added it) — restoring it"
      write_dnssec_override
    fi
  else
    echo "    --no-local-unbound passed; leaving its DNSSEC settings as-is"
  fi
elif [ "$ENABLE_LOCAL_UNBOUND" = 1 ]; then
  echo "    enabling local-unbound (sysrc local_unbound_enable=YES && service local_unbound start)"
  if sysrc local_unbound_enable=YES >/dev/null && service local_unbound start; then
    echo "    local-unbound enabled — /etc/resolv.conf now points at 127.0.0.1, with your"
    echo "    previous nameservers kept on as its own forwarders"
    write_dnssec_override
  else
    echo "    warning: could not enable local-unbound; gravinet's DNS-forwarding feature" >&2
    echo "    (if any network here uses it) won't work until you do this yourself" >&2
  fi
else
  cat <<'NOTE'
    note: skipped enabling local-unbound (--no-local-unbound was passed). DNS here
          is left as-is (e.g. resolvconf pointing straight at upstream servers).
          gravinet's optional per-network DNS forwarding needs local-unbound to
          route mesh-domain queries — enable it yourself if you end up wanting
          that feature:
              sysrc local_unbound_enable=YES && service local_unbound start
          Skip this if you don't use gravinet's DNS-forwarding feature.
NOTE
fi

echo "==> writing rc.d script $RCSCRIPT"
"$BIN" service install -config "$CONFIG" >/dev/null

if [ "$START" = 1 ]; then
  sysrc gravinet_enable=YES >/dev/null
  service gravinet start
  [ "$WAS_RUNNING" = 1 ] && echo "==> restarted gravinet" || echo "==> started gravinet"
fi

if [ "$PAM_BUILT" = 1 ]; then
  web_auth="log in with a system account (PAM)"
else
  web_auth="PAM is NOT compiled in — see the warning below"
fi

cat <<EOF

gravinet installed and running (rc.d: gravinet).
EOF

# Confirm what actually landed: the installed binary's own reported version.
# Compared against the source version printed at the top, this catches a stale
# --bin, a cached build, or a mismatched tree at a glance.
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

Note: FreeBSD has no firewall enabled by default, so — unlike the Windows and
macOS installers — there's no "allow this app through the firewall" step
here. If you do run pf/ipfw/ipfilter, open gravinet's underlay port yourself.

To join a mesh:
  1. Generate keys:    $BIN genkey -n 3
  2. Edit the config:  $CONFIG   (set keys, subnet/seeds, enable the network)
  3. Apply changes:    service gravinet restart
  4. Status/logs:      service gravinet onestatus   (log path is in the config)
EOF

if [ "$PAM_BUILT" != 1 ]; then
  cat >&2 <<EOF

  ********************************************************************************
  WARNING: this gravinet binary was built WITHOUT PAM support, so the web admin
  cannot authenticate system accounts and you will NOT be able to log in. Install
  a C compiler (pkg install llvm, or clang should already be in the base system)
  and re-run this installer from the source tree to get a PAM-enabled binary, or
  create a local web user instead:
      $BIN genpass -user admin -pass 'yourpassword'
  add it to web_admin.users, set web_admin.auth_mode "local", and reload.
  ********************************************************************************
EOF
fi
