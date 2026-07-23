#!/usr/bin/env bash
# install-macos.sh — build (if needed) and install gravinet as a launchd daemon.
#
#   sudo ./install-macos.sh                 # build from source + install + load
#   sudo ./install-macos.sh --uninstall     # remove
#
# With no prebuilt binary present, this installs a Go toolchain if the host
# lacks one, builds the binary from the bundled source, then installs it.
#
# Options:
#   --bin PATH     use this prebuilt binary instead of building
#   --prefix DIR   install prefix (default: /usr/local)
#   --config PATH  config file (default: /etc/gravinet/config.json)
#   --no-load      install but do not load (start) the daemon now
set -euo pipefail

PREFIX=/usr/local
CONFIG=/etc/gravinet/config.json
SRC=""
LOAD=1
ACTION=install
PLIST=/Library/LaunchDaemons/com.gravinet.daemon.plist
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
    --no-load) LOAD=0 ;;
    --load) LOAD=1 ;;
    -h|--help) sed -n '2,18p' "$0"; exit 0 ;;
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
# tree will install before it builds. Empty if the line can't be found.
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
  echo "==> unloading daemon"
  launchctl bootout system "$PLIST" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null || true
  FW=/usr/libexec/ApplicationFirewall/socketfilterfw
  [ -x "$FW" ] && "$FW" --remove "$BIN" >/dev/null 2>&1 || true
  rm -f "$PLIST" "$BIN" "$PKGMAN" "$MESHPING" /etc/pam.d/gravinet
  rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md" "$PREFIX/share/doc/gravinet/API.md"; rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true
  echo "==> removed $BIN, $PKGMAN, $MESHPING, $PLIST, the PAM file, and the docs (config at $CONFIG left in place)"
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
    arm64) echo arm64 ;;
    x86_64 | amd64) echo amd64 ;;
    *) echo "" ;;
  esac
}

# macOS installs run as root (sudo), where Homebrew refuses to operate, so Go is
# fetched straight from go.dev rather than via brew.
official_install_go() {
  command -v curl >/dev/null || { echo "    need curl to download Go" >&2; return 1; }
  local ga; ga="$(go_arch)"; [ -n "$ga" ] || { echo "    unsupported arch for Go" >&2; return 1; }
  local ver; ver="$(curl -fsSL 'https://go.dev/VERSION?m=text' 2>/dev/null | head -1)"
  [ -n "$ver" ] || { echo "    could not resolve latest Go version" >&2; return 1; }
  local tgz="${ver}.darwin-${ga}.tar.gz"
  echo "==> downloading https://go.dev/dl/${tgz}"
  curl -fsSL "https://go.dev/dl/${tgz}" -o "/tmp/${tgz}" || return 1
  if command -v shasum >/dev/null; then
    local want; want="$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' 2>/dev/null \
      | grep -A6 "\"${tgz}\"" | grep -m1 '"sha256"' | sed -E 's/.*"sha256": *"([0-9a-f]+)".*/\1/')"
    if [ -n "$want" ]; then
      local got; got="$(shasum -a 256 "/tmp/${tgz}" | awk '{print $1}')"
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
  official_install_go || return 1
  [ "$(go_minor)" -ge "$GO_MIN_MINOR" ] 2>/dev/null
}

# have_cc checks that a C compiler not only exists on PATH but actually runs.
# On macOS, /usr/bin/clang, /usr/bin/cc, and /usr/bin/gcc are always present as
# thin wrappers that shell out to `xcrun` to locate the real toolchain — they
# resolve via `command -v` even when the Command Line Tools registration is
# broken or missing (common after an OS upgrade, a partial CLT removal, or a
# stale /Library/Developer/CommandLineTools). In that state, invoking the
# wrapper fails with "xcrun: error: invalid active developer path ...", which
# only surfaces once cgo actually shells out to it during `go build` — too late
# to fall back cleanly. Actually running --version catches that up front.
have_cc() {
  local cc
  for cc in clang cc gcc; do
    if command -v "$cc" >/dev/null 2>&1 && "$cc" --version >/dev/null 2>&1; then
      return 0
    fi
  done
  return 1
}

# macOS ships the C compiler (clang) and the PAM headers (security/pam_appl.h)
# in the Xcode Command Line Tools / SDK. Install them headlessly when absent so
# the cgo build that enables web-admin PAM auth can proceed. Returns success
# only if a compiler is available afterwards.
install_build_deps() {
  # "invalid active developer path" (rather than "no compiler at all") means
  # Xcode's selected developer directory points somewhere without a working
  # toolchain — stale after an OS upgrade, or left behind by a partial Command
  # Line Tools removal — even though the CLT package itself may still be
  # present. Resetting to the default path fixes exactly that case for free,
  # before falling back to a real (re)install below.
  if ! have_cc; then
    xcode-select --reset 2>/dev/null || true
    have_cc && return 0
  fi
  echo "==> installing Xcode Command Line Tools (clang + SDK PAM headers)"
  # The trigger file makes softwareupdate list the on-demand CLT package.
  local trigger=/var/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress
  touch "$trigger"
  local prod
  prod="$(softwareupdate -l 2>/dev/null | grep -E '\* .*Command Line' | tail -n 1 | sed -E 's/^[^C]*//')"
  if [ -n "$prod" ]; then
    echo "    installing: $prod"
    softwareupdate -i "$prod" --verbose 2>/dev/null || true
  fi
  rm -f "$trigger"
  if ! have_cc; then
    # Headless path didn't land it (e.g. no matching label); trigger the GUI
    # installer, which runs asynchronously.
    echo "    falling back to the graphical installer (xcode-select --install)"
    xcode-select --install 2>/dev/null || true
    echo "    if an install dialog appeared, finish it and re-run this script"
  fi
  have_cc
}

# macOS ships PAM headers in the SDK; pam_opendirectory authenticates local and
# directory accounts.
write_pam_service() {
  cat > /etc/pam.d/gravinet <<'EOF'
# gravinet web admin
auth       required       pam_opendirectory.so
account    required       pam_permit.so
EOF
  echo "    wrote PAM service /etc/pam.d/gravinet"
}

build_from_source() {
  [ -f "$REPO/go.mod" ] || { echo "error: no prebuilt binary and no source tree at $REPO" >&2; exit 1; }
  ensure_go || { echo "error: could not obtain a Go toolchain (>=1.${GO_MIN_MINOR})" >&2; exit 1; }
  # Web-admin PAM auth needs a cgo build; macOS has clang + PAM headers in the
  # SDK (Command Line Tools). Install them if missing.
  local cgo=0
  if have_cc; then
    cgo=1
  elif install_build_deps && have_cc; then
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
  # every exit path. Same treatment for GOTMPDIR: go build's own per-run
  # scratch dir (go-buildNNNNNN, separate from GOCACHE) otherwise defaults
  # straight to /tmp and outlives an interrupted or OOM-killed build.
  mkdir -p "$BUILD_TMP/.gotmp"
  ( cd "$REPO" && CGO_ENABLED=$cgo GOTOOLCHAIN=auto \
      GOCACHE="$BUILD_TMP/.gocache" GOPATH="$BUILD_TMP/.gopath" GOTMPDIR="$BUILD_TMP/.gotmp" \
      go build -buildvcs=false -trimpath -ldflags "-s -w" -o "$out" ./cmd/gravinet ) \
    || { echo "error: build failed" >&2; exit 1; }
  SRC="$out"
  PAM_BUILT=$cgo
  echo "    built $SRC"
}

PAM_BUILT=0
EXPLICIT_BIN=${EXPLICIT_BIN:-0}

# Default: build from source with cgo (CGO_ENABLED=1) so PAM login works. A
# prebuilt release binary is cross-compiled without PAM, so it's only a fallback.
binary_has_pam() {
  # See install-linux.sh's copy of this function for the full story: the
  # otool fallback below is a heuristic that can be fooled (e.g. static
  # linking), and used to be the only check — silently overwriting a
  # correct PAM_BUILT value from build_from_source. Ask the binary itself
  # first; only fall back for a binary built before this self-report existed.
  local out
  out="$("$1" version 2>/dev/null)" || out=""
  case "$out" in
    *" pam=yes"*) return 0 ;;
    *" pam=no"*)  return 1 ;;
  esac
  otool -L "$1" 2>/dev/null | grep -qi 'libpam'
}

if [ "$EXPLICIT_BIN" = 1 ] && [ -n "$SRC" ]; then
  [ -f "$SRC" ] || { echo "error: --bin $SRC not found" >&2; exit 1; }
  binary_has_pam "$SRC" || echo "    note: --bin lacks PAM; web admin will need a local user (gravinet genpass)"
elif [ -f "$REPO/go.mod" ]; then
  build_from_source
else
  case "$(uname -m)" in
    arm64) a=arm64 ;;
    x86_64) a=amd64 ;;
    *) a="" ;;
  esac
  here="$(cd "$(dirname "$0")" && pwd)"
  for cand in "$here/gravinet-darwin-$a" "$here/gravinet"; do
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

# Upgrade-in-place: unload a running daemon before replacing its binary.
WAS_LOADED=0
if launchctl print system/com.gravinet.daemon >/dev/null 2>&1; then
  WAS_LOADED=1
  echo "==> daemon is loaded; unloading before upgrade"
  launchctl bootout system "$PLIST" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null || true
fi

echo "==> installing $SRC -> $BIN"
install -d -m 0755 "$PREFIX/bin"
install -m 0755 "$SRC" "$BIN"
# Clear the quarantine flag so Gatekeeper doesn't block a locally-built binary.
xattr -dr com.apple.quarantine "$BIN" 2>/dev/null || true

# Build scratch (BUILD_TMP: Go build+module caches) is done once the binary is
# in place — remove it now instead of holding it through the rest of the run;
# the EXIT trap still covers earlier exit paths. Guarded + cleared so the
# trap's later pass no-ops.
if [ -n "$BUILD_TMP" ]; then
  rm -rf "$BUILD_TMP"
  BUILD_TMP=""
fi

SCRIPTDIR_PKGMAN="$(dirname "$0")"
echo "==> installing pkgman -> $PKGMAN"
if [ -f "$SCRIPTDIR_PKGMAN/pkgman" ]; then
  install -m 0755 "$SCRIPTDIR_PKGMAN/pkgman" "$PKGMAN"
  xattr -dr com.apple.quarantine "$PKGMAN" 2>/dev/null || true
else
  echo "    note: pkgman not found in the package; skipping"
fi

echo "==> installing meshping -> $MESHPING"
if [ -f "$SCRIPTDIR_PKGMAN/meshping" ]; then
  install -m 0755 "$SCRIPTDIR_PKGMAN/meshping" "$MESHPING"
  xattr -dr com.apple.quarantine "$MESHPING" 2>/dev/null || true
else
  echo "    note: meshping not found in the package; skipping"
fi

# Allow inbound connections through the Application Firewall, if it's on. Off
# by default on most Macs, so this is lower-stakes than the equivalent gap
# was on Windows (where Defender Firewall is on by default and silently drops
# everything a service never got an explicit rule for) - but the same failure
# mode exists here for anyone who has it on: a signed app is normally
# auto-allowed when the firewall's "automatically allow signed software"
# option is set, but a locally-built binary may not qualify, and there's no
# desktop session for launchd to show an allow prompt on regardless. Scoped
# to this binary specifically (not opening ports globally).
FW=/usr/libexec/ApplicationFirewall/socketfilterfw
if [ -x "$FW" ]; then
  "$FW" --add "$BIN" >/dev/null 2>&1 || true
  "$FW" --unblockapp "$BIN" >/dev/null 2>&1 || true
fi

echo "==> installing docs (README, LICENSE, getting-started.md, API.md)"
DOCDIR="$PREFIX/share/doc/gravinet"
SCRIPTDIR="$(dirname "$0")"
install -d -m 0755 "$DOCDIR"
for doc in README.md LICENSE getting-started.md API.md; do
  src=""
  # API.md lives under docs/ in the repo, unlike the other three at the repo
  # root; checking both locations for every name is harmless and keeps this
  # one loop instead of a special case.
  for cand in "$REPO/$doc" "$REPO/docs/$doc" "$SCRIPTDIR/$doc" "$SCRIPTDIR/../$doc" "$SCRIPTDIR/docs/$doc" "$SCRIPTDIR/../docs/$doc"; do
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

echo "==> writing launchd daemon $PLIST"
"$BIN" service install -config "$CONFIG" >/dev/null

echo "==> web admin PAM service"
write_pam_service

if [ "$LOAD" = 1 ]; then
  # RunAtLoad=true in the plist means bootstrapping starts the daemon.
  launchctl bootstrap system "$PLIST" 2>/dev/null || launchctl load -w "$PLIST"
  [ "$WAS_LOADED" = 1 ] && echo "==> reloaded and started com.gravinet.daemon" || echo "==> loaded and started com.gravinet.daemon"
fi

if [ "$PAM_BUILT" = 1 ]; then
  web_auth="log in with a system account (PAM)"
else
  web_auth="PAM is NOT compiled in — see the warning below"
fi

cat <<EOF

gravinet installed and running (com.gravinet.daemon).
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

Note: for distribution you should codesign + notarize the binary; a locally
built one runs once quarantine is cleared (done above). If the Application
Firewall is on, $BIN is now allowlisted for inbound connections too.

To join a mesh:
  1. Generate keys:    $BIN genkey -n 3
  2. Edit the config:  $CONFIG   (set keys, subnet/seeds, enable the network)
  3. Apply changes:    sudo launchctl kickstart -k system/com.gravinet.daemon
  4. Logs:             /var/log/gravinet.out.log  and  .err.log
EOF

if [ "$PAM_BUILT" != 1 ]; then
  cat >&2 <<EOF

  ********************************************************************************
  WARNING: this gravinet binary was built WITHOUT PAM support, so the web admin
  cannot authenticate system accounts and you will NOT be able to log in. The
  Xcode Command Line Tools (clang + SDK PAM headers) could not be installed
  automatically. Finish their install (xcode-select --install) and re-run this
  installer from the source tree to get a PAM-enabled binary, or create a local
  web user instead:
      $BIN genpass -user admin -pass 'yourpassword'
  add it to web_admin.users, set web_admin.auth_mode "local", and reload.
  ********************************************************************************
EOF
fi

