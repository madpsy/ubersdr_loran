#!/bin/sh
# entrypoint.sh — translate environment variables into ubersdr_loran flags
#
# Environment variables:
#   UBERSDR_URL   UberSDR base URL (required, e.g. http://ubersdr:8080)
#   PASS          Bypass password (optional)
#   WEB_PORT      Port for the scope web UI (default: 6088)
#   UPDATE_HZ     Scope update rate in Hz (default: 10, use 1 for KiwiSDR-compatible)
#   WEB_STATIC    Path to static web files (default: /usr/local/share/ubersdr_loran/static)

set -e

WEB_STATIC="${WEB_STATIC:-/usr/local/share/ubersdr_loran/static}"

args=""
[ -n "$UBERSDR_URL" ] && args="$args -url $UBERSDR_URL"
[ -n "$PASS"        ] && args="$args -pass $PASS"
[ -n "$WEB_PORT"    ] && args="$args -web-port $WEB_PORT"
[ -n "$UPDATE_HZ"   ] && args="$args -update-hz $UPDATE_HZ"
args="$args -web-static $WEB_STATIC"

# shellcheck disable=SC2086
exec /usr/local/bin/ubersdr_loran $args "$@"
