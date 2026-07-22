#!/usr/bin/env bash
# build-all.sh — one command to produce shippable gravinet binaries for every
# target, with all runtime dependencies bundled.
#
# It fetches and checksum-verifies the signed Wintun driver (the only non-Go
# runtime dependency, needed for the Windows TUN), runs gofmt/vet/tests, then
# builds the full static cross-compile matrix with that driver embedded into the
# Windows binaries — so each artifact is a single self-contained file.
#
# Environment overrides:
#   VERSION, COMMIT     stamp into the binary (default: git describe / -dev)
#   OUT                 output dir (default: dist)
#   WINTUN_DIR          use a local Wintun bin/ dir instead of downloading
#   WINTUN_VERSION      Wintun release to fetch (default 0.14.1)
#   WINTUN_SHA256       expected zip checksum (pinned by default)
#   WINTUN_CACHE        download cache dir (default ~/.cache/gravinet-build)
#   REQUIRE_WINTUN=1    fail (don't fall back) if the driver can't be obtained
#   SKIP_TESTS=1        skip gofmt/vet/test (faster iteration)
set -euo pipefail
cd "$(dirname "$0")/.."

WINTUN_VERSION="${WINTUN_VERSION:-0.14.1}"
WINTUN_SHA256="${WINTUN_SHA256:-07c256185d6ee3652e09fa55c0b673e2624b565e02c4b9091c79ca7d2f24ef51}"
WINTUN_URL="https://www.wintun.net/builds/wintun-${WINTUN_VERSION}.zip"
CACHE="${WINTUN_CACHE:-${HOME}/.cache/gravinet-build}"
REQUIRE_WINTUN="${REQUIRE_WINTUN:-0}"
SKIP_TESTS="${SKIP_TESTS:-0}"

# Progress goes to stderr so command substitution captures only real output.
say()  { printf '\033[1m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

command -v go >/dev/null || die "go toolchain not found"
DL=""; command -v curl >/dev/null && DL=curl; [ -z "$DL" ] && command -v wget >/dev/null && DL=wget
SHACMD=""; command -v sha256sum >/dev/null && SHACMD="sha256sum"
[ -z "$SHACMD" ] && command -v shasum >/dev/null && SHACMD="shasum -a 256"

say "go $(go version | awk '{print $3}')"

# fetch_wintun downloads, verifies and extracts the driver, echoing the bin/ dir.
fetch_wintun() {
  command -v unzip >/dev/null || { warn "unzip missing"; return 1; }
  [ -n "$DL" ] || { warn "neither curl nor wget available"; return 1; }
  [ -n "$SHACMD" ] || { warn "no sha256 tool"; return 1; }
  mkdir -p "$CACHE"
  local zip="$CACHE/wintun-${WINTUN_VERSION}.zip"
  local root="$CACHE/wintun-${WINTUN_VERSION}"
  if [ ! -f "$zip" ]; then
    say "downloading $WINTUN_URL"
    if [ "$DL" = curl ]; then curl -fsSL "$WINTUN_URL" -o "$zip.tmp" || { warn "download failed"; return 1; }
    else wget -qO "$zip.tmp" "$WINTUN_URL" || { warn "download failed"; return 1; }; fi
    mv "$zip.tmp" "$zip"
  fi
  local got; got="$($SHACMD "$zip" | awk '{print $1}')"
  [ "$got" = "$WINTUN_SHA256" ] || { warn "Wintun checksum mismatch (got $got)"; rm -f "$zip"; return 1; }
  say "Wintun ${WINTUN_VERSION} verified (sha256 ok)"
  [ -d "$root/wintun/bin" ] || unzip -oq "$zip" -d "$root" || { warn "unzip failed"; return 1; }
  echo "$root/wintun/bin"
}

WINTUN_DIR="${WINTUN_DIR:-}"
if [ -z "$WINTUN_DIR" ]; then
  if dir="$(fetch_wintun)"; then
    WINTUN_DIR="$dir"
    say "Wintun ready at $WINTUN_DIR"
  elif [ "$REQUIRE_WINTUN" = 1 ]; then
    die "could not obtain Wintun (set WINTUN_DIR or fix network); REQUIRE_WINTUN=1"
  else
    warn "no Wintun driver — Windows binaries will load a side-by-side wintun.dll instead of embedding it"
  fi
fi

# Keep the bundled prebuilt-binaries license tied to the verified driver.
if [ -n "$WINTUN_DIR" ] && [ -f "$WINTUN_DIR/../prebuilt-binaries-license.txt" ]; then
  mkdir -p third_party/wintun
  cp "$WINTUN_DIR/../prebuilt-binaries-license.txt" third_party/wintun/prebuilt-binaries-license.txt 2>/dev/null || true
fi

if [ "$SKIP_TESTS" != 1 ]; then
  say "gofmt check"
  bad="$(gofmt -l . || true)"
  [ -z "$bad" ] || { printf '%s\n' "$bad" >&2; die "gofmt: files above need formatting"; }
  say "go vet"
  CGO_ENABLED=0 go vet ./... || die "go vet failed"
  say "go test"
  CGO_ENABLED=0 go test ./... >/dev/null || die "tests failed"
  say "tests passed"
fi

say "building release matrix"
WINTUN_DIR="$WINTUN_DIR" VERSION="${VERSION:-}" COMMIT="${COMMIT:-}" OUT="${OUT:-dist}" \
  ./scripts/build-release.sh

say "done — artifacts in ${OUT:-dist}/"
