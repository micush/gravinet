#!/usr/bin/env bash
# uninstall-macos.sh — remove the gravinet launchd daemon and binary.
#
#   sudo ./uninstall-macos.sh            # remove binary, plist, PAM file (keep config)
#   sudo ./uninstall-macos.sh --purge    # also remove /etc/gravinet
set -euo pipefail

PREFIX=/usr/local
CONFIG_DIR=/etc/gravinet
PLIST=/Library/LaunchDaemons/com.gravinet.daemon.plist
PURGE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="$2"; shift ;;
    --purge) PURGE=1 ;;
    -h|--help) sed -n '2,6p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "error: run as root (sudo)" >&2; exit 1; }

echo "==> unloading daemon"
launchctl bootout system "$PLIST" 2>/dev/null || launchctl unload "$PLIST" 2>/dev/null || true

BIN="$PREFIX/bin/gravinet"
PKGMAN="$PREFIX/bin/pkgman"
MESHPING="$PREFIX/bin/meshping"
FW=/usr/libexec/ApplicationFirewall/socketfilterfw
[ -x "$FW" ] && "$FW" --remove "$BIN" >/dev/null 2>&1 || true

echo "==> removing binaries, plist, and PAM file"
rm -f "$BIN" "$PKGMAN" "$MESHPING" "$PLIST" /etc/pam.d/gravinet
rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"; rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true

if [ "$PURGE" = 1 ]; then
  rm -rf "$CONFIG_DIR"
  echo "==> purged $CONFIG_DIR"
else
  echo "    left config in $CONFIG_DIR (use --purge to remove)"
fi
echo "gravinet uninstalled."
