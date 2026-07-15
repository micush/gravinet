#!/usr/bin/env bash
# Build an entire gravinet release in one step: every platform binary
# (build-release.sh), stamped with VERSION, with dist/SHA256SUMS. Run this
# whenever gravinet changes and you're ready to cut a release; everything you
# hand out lives in dist/ afterward.
#
# Usage:
#   ./scripts/release.sh                # version = git describe, or 0.1.0-dev
#   VERSION=1.2.3 ./scripts/release.sh  # explicit version
set -euo pipefail

cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo 0.1.0-dev)}"
COMMIT="${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo none)}"
export VERSION COMMIT

echo "=== gravinet release ${VERSION} (${COMMIT}) ==="
echo

./scripts/build-release.sh
echo
echo "=== dist/ ready to share ==="
ls -la dist/
echo
echo "checksums (dist/SHA256SUMS):"
cat dist/SHA256SUMS
