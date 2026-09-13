#!/bin/sh
set -eu

REPO_DIR="${WB2A_REPO_DIR:-/opt/workbuddy2api}"
BRANCH="${WB2A_UPDATE_BRANCH:-main}"
cd "$REPO_DIR"

# The compose watcher can run under a different service user than the user
# that owns the checkout. Scope Git's safe-directory exception to this exact
# configured repository instead of mutating global Git configuration.
git -c "safe.directory=$REPO_DIR" fetch --no-tags origin "$BRANCH"
git -c "safe.directory=$REPO_DIR" merge --ff-only "origin/$BRANCH"
VERSION="$(git -c "safe.directory=$REPO_DIR" rev-parse --short HEAD)"
WB2API_VERSION="$VERSION" WB2A_VERSION="$VERSION" docker compose up -d --build wb2api
