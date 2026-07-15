#!/bin/sh
# uninstall-openbsd.sh — remove the gravinet rc.d service and binary.
#
#   doas ./uninstall-openbsd.sh            # remove binary, rc.d script (keep config)
#   doas ./uninstall-openbsd.sh --purge    # also remove /etc/gravinet
set -eu

PREFIX=/usr/local
CONFIG_DIR=/etc/gravinet
RCSCRIPT=/etc/rc.d/gravinet
PURGE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --prefix) PREFIX="$2"; shift ;;
    --purge) PURGE=1 ;;
    -h|--help) sed -n '2,5p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

[ "$(id -u)" = 0 ] || { echo "error: run as root (doas/su)" >&2; exit 1; }

BIN="$PREFIX/sbin/gravinet"
PKGMAN="$PREFIX/sbin/pkgman"
MESHPING="$PREFIX/sbin/meshping"

echo "==> stopping and disabling daemon"
rcctl stop gravinet 2>/dev/null || true
rcctl disable gravinet 2>/dev/null || true

echo "==> removing binaries and rc.d script"
rm -f "$BIN" "$PKGMAN" "$MESHPING" "$RCSCRIPT"
rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"
rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true

echo "==> removing pf rule (if present)"
PF_CONF=/etc/pf.conf
PF_MARKER="# gravinet: overlay underlay (added by install-openbsd.sh)"
if [ -f "$PF_CONF" ] && grep -qF "$PF_MARKER" "$PF_CONF"; then
  tmp="$(mktemp)"
  # Drop the marker line and the single pass rule that follows it.
  awk -v m="$PF_MARKER" '$0==m{skip=2} skip>0{skip--;next} {print}' "$PF_CONF" > "$tmp"
  cat "$tmp" > "$PF_CONF"; rm -f "$tmp"
  command -v pfctl >/dev/null 2>&1 && pfctl -f "$PF_CONF" 2>/dev/null || true
  echo "    removed gravinet pf rule and reloaded pf"
else
  echo "    no gravinet pf rule found"
fi

if [ "$PURGE" = 1 ]; then
  rm -rf "$CONFIG_DIR"
  echo "==> purged $CONFIG_DIR"
else
  echo "    left config in $CONFIG_DIR (use --purge to remove)"
fi
echo "gravinet uninstalled."
