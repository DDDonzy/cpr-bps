#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ARCH="${1:-amd64}"
case "$ARCH" in
  amd64|x86_64) GOARCH=amd64; TARGET=x86_64-unknown-linux-musl; LABEL=x86_64-unknown-linux-gnu ;;
  arm64|aarch64) GOARCH=arm64; TARGET=aarch64-unknown-linux-musl; LABEL=aarch64-unknown-linux-gnu ;;
  *) echo 'Usage: bash Scripts/build.sh amd64|arm64' >&2; exit 2 ;;
esac
BUILD_DIR="${BUILD_DIR:-$ROOT/.build}"
mkdir -p "$BUILD_DIR" "$ROOT/packages"
BUILD_DIR="$(cd "$BUILD_DIR" && pwd)"
export CARGO_TARGET_DIR="$BUILD_DIR/rust"
( cd "$ROOT/converter"; CGO_ENABLED=0 GOOS=linux GOARCH="$GOARCH" go build -trimpath -o "$BUILD_DIR/converter-$GOARCH" ./cmd/converter )
export BPS_ENGINE_BINARY="$BUILD_DIR/converter-$GOARCH"
rustup target add "$TARGET" --toolchain 1.97.0
HOST="$(rustc +1.97.0 -vV | sed -n 's/^host: //p')"
LINKER="$(rustc +1.97.0 --print sysroot)/lib/rustlib/$HOST/bin/rust-lld"
case "$TARGET" in
 x86_64-unknown-linux-musl) export CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_LINKER="$LINKER"; export CARGO_TARGET_X86_64_UNKNOWN_LINUX_MUSL_RUSTFLAGS='-C linker-flavor=ld.lld' ;;
 aarch64-unknown-linux-musl) export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_LINKER="$LINKER"; export CARGO_TARGET_AARCH64_UNKNOWN_LINUX_MUSL_RUSTFLAGS='-C linker-flavor=ld.lld' ;;
esac
cargo +1.97.0 build --manifest-path "$ROOT/plugin/Cargo.toml" --release --locked --target "$TARGET"
if [ -z "${CPR_PLUGIN_CLI:-}" ]; then
 cargo +1.97.0 build --manifest-path "$ROOT/vendor/codex-proxy-rs-3.18.2/backend/Cargo.toml" --locked --release -p codex-proxy-plugin-cli
 CPR_PLUGIN_CLI="$CARGO_TARGET_DIR/release/cpr-plugin"
fi
"$CPR_PLUGIN_CLI" package --manifest "$ROOT/plugin/plugin.json" --binary "$CARGO_TARGET_DIR/$TARGET/release/cpr-bps-plugin" --target "$LABEL" --output-dir "$ROOT/packages"
