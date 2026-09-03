#!/usr/bin/env bash
# docker.sh — build the ubersdr_loran Docker image
#
# Usage:
#   ./docker.sh [build|push|arm64|run]
#
#   build  — build the image for linux/amd64 locally (default)
#   arm64  — build the image for linux/arm64 locally
#   push   — build both linux/amd64 AND linux/arm64 via buildx and push a
#             multi-arch manifest to the registry, then commit & push git
#   run    — run the image locally (set UBERSDR_URL env var)
#
# Environment variables (build):
#   IMAGE      Docker image name/tag   (default: madpsy/ubersdr_loran:latest)
#   PLATFORM   Docker --platform flag  (default: linux/amd64)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_loran:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"

BUILDER_NAME="ubersdr_loran_builder"

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

ensure_builder() {
    if ! docker buildx inspect "$BUILDER_NAME" &>/dev/null; then
        echo "Creating buildx builder '$BUILDER_NAME'..."
        docker buildx create --name "$BUILDER_NAME" --driver docker-container --bootstrap
    fi
    docker buildx use "$BUILDER_NAME"
}

stage_context() {
    TMPCTX="$(mktemp -d)"
    trap 'rm -rf "$TMPCTX"' EXIT
    echo "Staging build context in $TMPCTX..."
    rsync -a --exclude='/ubersdr_loran' \
              --exclude='.git' \
              "$SCRIPT_DIR/" "$TMPCTX/"
}

build() {
    check_deps
    stage_context

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    ensure_builder
    docker buildx build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        --load \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

push() {
    check_deps
    stage_context
    ensure_builder

    echo "Building and pushing multi-arch image $IMAGE (linux/amd64,linux/arm64)..."
    docker buildx build \
        --platform linux/amd64,linux/arm64 \
        --tag "$IMAGE" \
        --push \
        "$TMPCTX"

    echo "Pushed multi-arch manifest: $IMAGE"

    # Push whatever is already committed — but never commit on the user's
    # behalf. This previously ran "git add -A" and committed everything with
    # a generic "Release" message, which silently swallowed real commit
    # messages and would sweep any unrelated work in progress (or a stray
    # credentials file) into a public push with no chance to review it.
    if [[ -n "$(git status --porcelain)" ]]; then
        echo
        echo "WARNING: uncommitted changes — the image was built from them," >&2
        echo "         but they are NOT being committed or pushed:" >&2
        git status --short >&2
        echo >&2
        echo "         Commit them yourself, then run: git push" >&2
        exit 1
    fi

    echo "Pushing git repository..."
    git push
}

run_image() {
    args=()
    [[ -n "${UBERSDR_URL:-}" ]] && args+=(-e "UBERSDR_URL=$UBERSDR_URL")
    [[ -n "${PASS:-}"        ]] && args+=(-e "PASS=$PASS")
    [[ -n "${WEB_PORT:-}"    ]] && args+=(-e "WEB_PORT=$WEB_PORT")

    docker run --rm -it \
        --platform "$PLATFORM" \
        -p "${WEB_PORT:-6088}:${WEB_PORT:-6088}" \
        "${args[@]}" \
        "$IMAGE" \
        "${@}"
}

case "${1:-build}" in
    build) build ;;
    arm64) PLATFORM=linux/arm64 build ;;
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|arm64|push|run]" >&2
        exit 1
        ;;
esac
