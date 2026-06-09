#!/usr/bin/env bash
# update.sh — pull the latest image and restart the service

set -euo pipefail

INSTALL_DIR="${HOME}/ubersdr/loran"

cd "${INSTALL_DIR}"
echo "Pulling latest ubersdr_loran image..."
docker compose pull
echo "Restarting service..."
docker compose up -d --remove-orphans --force-recreate
echo "Done."
echo "  View logs : docker compose logs -f"
