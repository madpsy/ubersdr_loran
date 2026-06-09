#!/usr/bin/env bash
# build.sh — build ubersdr_loran
#
# Usage:
#   ./build.sh              # build binary in this directory
#   ./build.sh install      # build and install to /usr/local/bin (requires sudo)
#   ./build.sh clean        # remove built binary

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

BIN="ubersdr_loran"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

build() {
    echo "Building $BIN..."
    go build -o "$BIN" .
    echo "Done: $SCRIPT_DIR/$BIN"
}

install() {
    build
    echo "Installing to $INSTALL_DIR..."
    sudo cp "$BIN" "$INSTALL_DIR/"
    echo "Installed $INSTALL_DIR/$BIN"
}

clean() {
    echo "Removing built binary..."
    rm -f "$BIN"
    echo "Done"
}

case "${1:-build}" in
    build)   build   ;;
    install) install ;;
    clean)   clean   ;;
    *)
        echo "Usage: $0 [build|install|clean]" >&2
        exit 1
        ;;
esac
