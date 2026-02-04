#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd "$(dirname "$0")"/.. && pwd)
cd "$ROOT_DIR"

echo "[build_vendor] Tidying modules..."
go mod tidy

echo "[build_vendor] Vendoring dependencies..."
go mod vendor

BIN_DIR="$ROOT_DIR/bin"
mkdir -p "$BIN_DIR"

echo "[build_vendor] Building executable with vendor..."
go build -mod=vendor -o "$BIN_DIR/goftpfs" ./cmd/goftpfs

echo "[build_vendor] Build complete. Binary: $BIN_DIR/goftpfs"
