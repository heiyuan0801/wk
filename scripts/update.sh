#!/bin/sh
set -eu

REPO_DIR="${WB2A_REPO_DIR:-/opt/workbuddy2api}"
BRANCH="${WB2A_UPDATE_BRANCH:-main}"
cd "$REPO_DIR"

git fetch --no-tags origin "$BRANCH"
git merge --ff-only "origin/$BRANCH"
VERSION="$(git rev-parse --short HEAD)"
WB2API_VERSION="$VERSION" WB2A_VERSION="$VERSION" docker compose up -d --build wb2api
