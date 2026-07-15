#!/bin/sh
# uninstall-freebsd.sh — remove the gravinet rc.d service and binary.
#
#   sudo ./uninstall-freebsd.sh            # remove binary, rc.d script, PAM file (keep config)
#   sudo ./uninstall-freebsd.sh --purge    # also remove /usr/local/etc/gravinet
set -eu

PREFIX=/usr/local
CONFIG_DIR=/usr/local/etc/gravinet
RCSCRIPT=/usr/local/etc/rc.d/gravinet
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

[ "$(id -u)" = 0 ] || { echo "error: run as root (sudo)" >&2; exit 1; }

BIN="$PREFIX/bin/gravinet"
PKGMAN="$PREFIX/bin/pkgman"
MESHPING="$PREFIX/bin/meshping"

echo "==> stopping daemon"
if [ -x "$RCSCRIPT" ]; then service gravinet onestop 2>/dev/null || true; fi

echo "==> removing binaries, rc.d script, and PAM file"
rm -f "$BIN" "$PKGMAN" "$MESHPING" "$RCSCRIPT" /etc/pam.d/gravinet
sysrc -x gravinet_enable >/dev/null 2>&1 || true
rm -f "$PREFIX/share/doc/gravinet/README.md" "$PREFIX/share/doc/gravinet/LICENSE" "$PREFIX/share/doc/gravinet/getting-started.md"; rmdir "$PREFIX/share/doc/gravinet" 2>/dev/null || true

if [ "$PURGE" = 1 ]; then
  rm -rf "$CONFIG_DIR"
  echo "==> purged $CONFIG_DIR"
else
  echo "    left config in $CONFIG_DIR (use --purge to remove)"
fi
echo "gravinet uninstalled."
