#!/bin/bash
# sync-auths.sh — 从 CPA 容器拷出 WorkBuddy 中国区/海外版 auth 到 ./auths
set -e
cd /root/workbuddy2api
mkdir -p auths /tmp/wb-sync
rm -rf /tmp/wb-sync
mkdir -p /tmp/wb-sync
docker cp cpa-manager-plus-cli-proxy-api-1:/root/.cli-proxy-api/. /tmp/wb-sync/ 2>/dev/null
kept=0; kept_cn=0; kept_global=0
for f in /tmp/wb-sync/workbuddy*.json; do
  [ -e "$f" ] || continue
  # 两个区域的凭证都保留；服务会依据 domain 自动选择上游 host。
  dom=$(grep -o '"domain"[[:space:]]*:[[:space:]]*"[^"]*"' "$f" | head -1 | sed 's/.*: *"//;s/"$//')
  case "$dom" in
    *workbuddy.ai*|*codebuddy.ai*)
      cp "$f" auths/ && kept=$((kept+1)) && kept_global=$((kept_global+1));;
    *)
      cp "$f" auths/ && kept=$((kept+1)) && kept_cn=$((kept_cn+1));;
  esac
done
chmod 600 auths/*.json 2>/dev/null || true
chown 10001:10001 auths/*.json 2>/dev/null || true
echo "kept=$kept kept_cn=$kept_cn kept_global=$kept_global total_in_auths=$(ls auths/ | wc -l)"
