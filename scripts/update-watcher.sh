#!/bin/sh
set -eu

REPO_DIR="${WB2A_REPO_DIR:-/opt/workbuddy2api}"
REQUEST_FILE="$REPO_DIR/data/update-request.json"
LOCK_FILE="$REPO_DIR/data/update.lock"
LOG_FILE="$REPO_DIR/data/update.log"

while :; do
  if [ -f "$REQUEST_FILE" ] && mkdir "$LOCK_FILE" 2>/dev/null; then
    mv "$REQUEST_FILE" "$LOCK_FILE/request.json" 2>/dev/null || true
    started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    printf '{"state":"running","started_at":"%s","message":"正在拉取代码、构建并重启容器"}\n' "$started_at" > "$REPO_DIR/data/update-result.json"
    # Keep diagnostics after the lock directory is removed. This makes a
    # transient git/docker failure actionable from the admin host.
    if "$REPO_DIR/scripts/update.sh" >> "$LOG_FILE" 2>&1; then
      version="$(git -C "$REPO_DIR" -c safe.directory="$REPO_DIR" rev-parse --short HEAD 2>/dev/null || true)"
      printf '{"state":"succeeded","finished_at":"%s","version":"%s","message":"更新完成，容器已重启"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$version" > "$REPO_DIR/data/update-result.json"
    else
      status=$?
      printf '%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ) update failed (exit $status)" >> "$LOG_FILE"
      printf '{"state":"failed","finished_at":"%s","exit_code":%s,"message":"更新失败，请查看日志尾部"}\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$status" > "$REPO_DIR/data/update-result.json"
    fi
    rm -rf "$LOCK_FILE"
  fi
  sleep 5
done
