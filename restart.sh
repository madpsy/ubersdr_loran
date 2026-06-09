#!/usr/bin/env bash
# restart.sh — restart the ubersdr_loran service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/loran"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_loran..."
docker compose down
echo "Starting ubersdr_loran..."
docker compose up -d --remove-orphans
echo "Done."
echo "  Scope UI  : http://localhost:8095/"
echo "  View logs : docker compose logs -f"
