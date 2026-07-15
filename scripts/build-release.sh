#!/usr/bin/env bash
# Build the full release matrix as static, stripped, version-stamped binaries
# and emit SHA-256 checksums. Pure-Go, CGO disabled, reproducible-ish.
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo 0.1.0-dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
OUT="${OUT:-dist}"

LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}"

# WINTUN_DIR, if set, points at a directory holding the real signed Wintun DLLs
# laid out per architecture, e.g. the `bin/` folder from the wintun.net zip:
#   $WINTUN_DIR/amd64/wintun.dll, $WINTUN_DIR/arm64/wintun.dll
# When present, the matching DLL is staged into the embed slot so the Windows
# build is a single self-contained .exe. When absent, Windows binaries embed the
# placeholder and load a side-by-side wintun.dll at runtime instead.
WINTUN_DIR="${WINTUN_DIR:-}"

embed_path() { echo "internal/tun/wintun/$1/wintun.dll"; }

stage_wintun() { # $1 = GOARCH ; echoes "1" if a real DLL was staged
  local arch="$1" slot src
  slot="$(embed_path "$arch")"
  src="${WINTUN_DIR}/${arch}/wintun.dll"
  if [ -n "$WINTUN_DIR" ] && [ -f "$src" ]; then
    cp "$slot" "${slot}.placeholder.bak"
    cp "$src" "$slot"
    echo 1
  fi
}

unstage_wintun() { # $1 = GOARCH
  local slot
  slot="$(embed_path "$1")"
  if [ -f "${slot}.placeholder.bak" ]; then
    mv "${slot}.placeholder.bak" "$slot"
  fi
}

# GOOS/GOARCH pairs to ship.
TARGETS=(
  linux/amd64 linux/arm64 linux/arm
  windows/amd64 windows/arm64
  darwin/amd64 darwin/arm64
  freebsd/amd64
  openbsd/amd64 openbsd/arm64
)

rm -rf "$OUT"
mkdir -p "$OUT"

# The native linux/darwin/freebsd target is built with cgo so its binary has
# PAM web-auth (CGO_ENABLED=1 is the default for the platform you run on;
# darwin always has PAM headers, linux and freebsd only when actually
# installed). Cross targets can't link libpam, so they're static
# (CGO_ENABLED=0, local web-auth only) — openbsd and windows are unaffected
# either way, since their system auth (bsd_auth(3), LogonUser) needs no cgo.
HOSTOS="$(go env GOHOSTOS)"; HOSTARCH="$(go env GOHOSTARCH)"
rel_have_cc() { command -v cc >/dev/null || command -v gcc >/dev/null || command -v clang >/dev/null; }
rel_have_pam() { [ -f /usr/include/security/pam_appl.h ] || [ -f /usr/local/include/security/pam_appl.h ]; }

echo "gravinet ${VERSION} (${COMMIT})"
[ -z "$WINTUN_DIR" ] && echo "  note: WINTUN_DIR unset — Windows builds use side-by-side wintun.dll"
for t in "${TARGETS[@]}"; do
  os="${t%/*}"; arch="${t#*/}"
  ext=""; [ "$os" = "windows" ] && ext=".exe"
  bin="${OUT}/gravinet-${os}-${arch}${ext}"

  staged=""
  if [ "$os" = "windows" ]; then
    staged="$(stage_wintun "$arch")"
  fi

  cgo=0
  if [ "$os" = "$HOSTOS" ] && [ "$arch" = "$HOSTARCH" ] && rel_have_cc; then
    if [ "$os" = "darwin" ] || { { [ "$os" = "linux" ] || [ "$os" = "freebsd" ]; } && rel_have_pam; }; then
      cgo=1
    fi
  fi

  CGO_ENABLED=$cgo GOOS="$os" GOARCH="$arch" \
    go build -buildvcs=false -trimpath -ldflags "$LDFLAGS" -o "$bin" ./cmd/gravinet

  tag=""
  [ "$cgo" = 1 ] && tag="(PAM)"
  if [ "$os" = "windows" ]; then
    unstage_wintun "$arch"
    if [ -n "$staged" ]; then tag="(wintun embedded)"; else tag="(side-by-side wintun.dll)"; fi
  fi
  printf "  built %-28s %s %s\n" "$(basename "$bin")" "$(du -h "$bin" | cut -f1)" "$tag"
done

# Bundle installers and reference daemon definitions alongside the binaries.
if [ -d install ]; then
  cp install/install-linux.sh install/install-macos.sh install/install-freebsd.sh install/install-openbsd.sh install/install-windows.ps1 install/install-windows.bat "$OUT/" 2>/dev/null || true
  cp install/uninstall-linux.sh install/uninstall-macos.sh install/uninstall-freebsd.sh install/uninstall-openbsd.sh install/uninstall-windows.ps1 install/uninstall-windows.bat "$OUT/" 2>/dev/null || true
  cp install/gravinet.service install/com.gravinet.daemon.plist install/gravinet.rc install/windows-service.txt "$OUT/" 2>/dev/null || true
  echo "  bundled installers + uninstallers + daemon definitions"
fi
cp LICENSE README.md getting-started.md "$OUT/" 2>/dev/null || true
# Ship the Wintun prebuilt-binaries license alongside the Windows binaries.
if [ -f third_party/wintun/prebuilt-binaries-license.txt ]; then
  cp third_party/wintun/prebuilt-binaries-license.txt "$OUT/wintun-prebuilt-binaries-license.txt"
fi

# Checksums.
( cd "$OUT" && sha256sum gravinet-* > SHA256SUMS )
echo "checksums written to ${OUT}/SHA256SUMS"
