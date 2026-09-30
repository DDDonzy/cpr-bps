#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
ARCH="${1:-arm64}"
case "$ARCH" in arm64|amd64) ;; *) echo 'Usage: build.sh arm64|amd64' >&2; exit 2;; esac
BUILD_DIR="${BUILD_DIR:-$ROOT/.build}"
VERSION="$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$ROOT/plugin/Cargo.toml" | head -1 | tr -d '\r')"
mkdir -p "$BUILD_DIR" "$ROOT/packages"
(cd "$HERE"; CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags='-s -w' -o "$BUILD_DIR/bps-log-gateway-$ARCH" .)
PACKAGE="$ROOT/packages/bps-log-gateway-$VERSION-linux-$ARCH.gz"
gzip -n -c "$BUILD_DIR/bps-log-gateway-$ARCH" > "$PACKAGE"
(cd "$ROOT/packages"; sha256sum "$(basename "$PACKAGE")" > "$(basename "$PACKAGE").sha256")
echo "$PACKAGE"
