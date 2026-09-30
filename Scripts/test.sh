#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="${BUILD_DIR:-$ROOT/.build}"
mkdir -p "$BUILD_DIR"
BUILD_DIR="$(cd "$BUILD_DIR" && pwd)"
export CARGO_TARGET_DIR="$BUILD_DIR/rust"
( cd "$ROOT/converter"; go test ./internal/kernel ./internal/bps ./cmd/converter; CGO_ENABLED=0 go build -trimpath -o "$BUILD_DIR/converter-native" ./cmd/converter )
export BPS_ENGINE_BINARY="$BUILD_DIR/converter-native"
cargo +1.97.0 fmt --manifest-path "$ROOT/plugin/Cargo.toml" -- --check
cargo +1.97.0 clippy --manifest-path "$ROOT/plugin/Cargo.toml" --locked -- -D warnings
cargo +1.97.0 test --manifest-path "$ROOT/plugin/Cargo.toml" --locked
