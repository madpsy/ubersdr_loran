#!/usr/bin/env bash
# stop.sh — stop the ubersdr_loran service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/loran"

cd "${INSTALL_DIR}"
echo "Stopping ubersdr_loran..."
docker compose down
echo "Done."
