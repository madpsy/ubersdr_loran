#!/usr/bin/env bash
# start.sh — start the ubersdr_loran service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/loran"

cd "${INSTALL_DIR}"
echo "Starting ubersdr_loran..."
docker compose up -d --remove-orphans
echo "Done."
echo "  Scope UI  : http://localhost:6088/"
echo "  View logs : docker compose logs -f"
