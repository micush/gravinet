#!/usr/bin/env bash
# uninstall-linux.sh — remove the gravinet systemd service and binary.
#
#   sudo ./uninstall-linux.sh            # remove binary, unit, PAM file (keep config)
#   sudo ./uninstall-linux.sh --purge    # also remove /etc/gravinet
set -euo pipefail

PREFIX=/usr/local
CONFIG_DIR=/etc/gravinet
SERVICE=gravinet
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

echo "==> stopping and disabling $SERVICE"
systemctl disable --now "$SERVICE" 2>/dev/null || true
rm -f /etc/systemd/system/${SERVICE}.service
systemctl daemon-reload 2>/dev/null || true

echo "==> removing binaries and PAM file"
rm -f "$PREFIX/bin/gravinet" "$PREFIX/bin/pkgman" "$PREFIX/bin/meshping" /etc/pam.d/gravinet
rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"; rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true

if [ "$PURGE" = 1 ]; then
  rm -rf "$CONFIG_DIR"
  echo "==> purged $CONFIG_DIR"
else
  echo "    left config in $CONFIG_DIR (use --purge to remove)"
fi
echo "gravinet uninstalled."
