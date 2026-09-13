#!/bin/sh
set -eu

REPO_DIR="${WB2A_REPO_DIR:-/opt/workbuddy2api}"
REQUEST_FILE="$REPO_DIR/data/update-request.json"
LOCK_FILE="$REPO_DIR/data/update.lock"
LOG_FILE="$REPO_DIR/data/update.log"

while :; do
  if [ -f "$REQUEST_FILE" ] && mkdir "$LOCK_FILE" 2>/dev/null; then
    mv "$REQUEST_FILE" "$LOCK_FILE/request.json" 2>/dev/null || true
    # Keep diagnostics after the lock directory is removed. This makes a
    # transient git/docker failure actionable from the admin host.
    if "$REPO_DIR/scripts/update.sh" >> "$LOG_FILE" 2>&1; then
      printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ) update completed" > "$REPO_DIR/data/update-result.json"
    else
      status=$?
      printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ) update failed (exit $status)" >> "$LOG_FILE"
      printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ) update failed (exit $status)" > "$REPO_DIR/data/update-result.json"
    fi
    rm -rf "$LOCK_FILE"
  fi
  sleep 5
done
