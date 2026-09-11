#!/usr/bin/env bash
# login.sh — WorkBuddy OAuth 登录（中国区/海外版）→ 落盘 auth 文件
#
# 用法:
#   ./login.sh              # 中国区
#   ./login.sh global       # 海外版
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录
#   3. 回到这里按 y → poll 拿 token+uid+nickname → 签到 → 落盘 auths/workbuddy-<uid>.json
#   4. 重启 workbuddy2api 容器加载新账号
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"
CONFIG_FILE="${WB2A_CONFIG_FILE:-./config.json}"

case "${1:-cn}" in
    cn|china) REGION="cn" ;;
    global|overseas|international|intl) REGION="global" ;;
    *)
        echo "用法: $0 [cn|global]" >&2
        exit 2
        ;;
esac

mkdir -p "$AUTH_DIR"

# login 工具：不存在或源码更新时自动编译
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" || "./cmd/login/main.go" -nt "$LOGIN_BIN" ]]; then
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录"
echo "  区域: $([[ "$REGION" == "global" ]] && echo "海外版" || echo "中国区")"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url "$REGION")

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll "$REGION") || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ "$REGION" == "global" ]]; then
    BILLING_BASE="https://www.workbuddy.ai"
    [[ -n "$DOMAIN" ]] || DOMAIN="www.workbuddy.ai"
else
    BILLING_BASE="https://www.codebuddy.cn"
fi

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── 签到（按账号区域选择 billing host，幂等不阻塞）─────────────────────
WB2A_LOGIN_TOKEN="$TOKEN" WB2A_LOGIN_USER_ID="$USER_ID" WB2A_LOGIN_ENT_ID="$ENT_ID" WB2A_LOGIN_DOMAIN="$DOMAIN" WB2A_LOGIN_BILLING_BASE="$BILLING_BASE" python3 - <<'PYEOF'
import json, os, urllib.request, urllib.error

token = os.environ["WB2A_LOGIN_TOKEN"]
user_id = os.environ["WB2A_LOGIN_USER_ID"]
enterprise_id = os.environ.get("WB2A_LOGIN_ENT_ID", "")
domain = os.environ.get("WB2A_LOGIN_DOMAIN", "")
billing_base = os.environ["WB2A_LOGIN_BILLING_BASE"].rstrip("/")

req = urllib.request.Request(
    billing_base + "/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer " + token,
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-User-Id": user_id,
        **({"X-Enterprise-Id": enterprise_id, "X-Tenant-Id": enterprise_id} if enterprise_id else {}),
        **({"X-Domain": domain} if domain else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=$USER_ID），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=$USER_ID），新增 auth 文件"
    ACTION="新增"
fi
WB2A_LOGIN_AUTH_FILE="$AUTH_FILE" WB2A_LOGIN_USER_ID="$USER_ID" WB2A_LOGIN_ENT_ID="$ENT_ID" WB2A_LOGIN_NICKNAME="$NICKNAME" WB2A_LOGIN_TOKEN="$TOKEN" WB2A_LOGIN_REFRESH="$REFRESH" WB2A_LOGIN_EXPIRES_AT="$EXPIRES_AT" WB2A_LOGIN_DOMAIN="$DOMAIN" python3 - <<'PYEOF'
import json, os

auth = {
    "account": {
        "uid": os.environ["WB2A_LOGIN_USER_ID"],
        "enterpriseId": os.environ.get("WB2A_LOGIN_ENT_ID", ""),
        "nickname": os.environ.get("WB2A_LOGIN_NICKNAME", "")
    },
    "auth": {
        "accessToken": os.environ["WB2A_LOGIN_TOKEN"],
        "refreshToken": os.environ.get("WB2A_LOGIN_REFRESH", ""),
        "expiresAt": int(os.environ["WB2A_LOGIN_EXPIRES_AT"]),
        "domain": os.environ.get("WB2A_LOGIN_DOMAIN", "")
    }
}
with open(os.environ["WB2A_LOGIN_AUTH_FILE"], "w") as f:
    json.dump(auth, f, indent=1)
PYEOF
chmod 600 "$AUTH_FILE"
echo "已保存（$ACTION）: $AUTH_FILE"

# Dockerfile 以 UID/GID 10001 运行服务。脚本常由 root 执行时，修正新文件
# 的所有权，否则容器会因 600 权限无法读取海外或中国区凭证。
if [[ "$(id -u)" == "0" ]]; then
    APP_UID="10001"
    APP_GID="10001"
    if docker ps --format '{{.Names}}' 2>/dev/null | grep -q "^${CONTAINER}$"; then
        CONTAINER_UID=$(docker exec "$CONTAINER" id -u 2>/dev/null || true)
        CONTAINER_GID=$(docker exec "$CONTAINER" id -g 2>/dev/null || true)
        [[ "$CONTAINER_UID" =~ ^[0-9]+$ ]] && APP_UID="$CONTAINER_UID"
        [[ "$CONTAINER_GID" =~ ^[0-9]+$ ]] && APP_GID="$CONTAINER_GID"
    fi
    chown "$APP_UID:$APP_GID" "$AUTH_FILE" 2>/dev/null || true
fi

# 添加另一地区账号后，账号池必须改为混合模式，否则下一次启动会按
# 原来的单区域配置把新账号过滤掉。保留配置文件原有权限和所有权。
if [[ -f "$CONFIG_FILE" ]]; then
    if ! WB2A_LOGIN_CONFIG_FILE="$CONFIG_FILE" WB2A_LOGIN_REGION="$REGION" python3 - <<'PYEOF'
import json, os, stat, tempfile

path = os.environ["WB2A_LOGIN_CONFIG_FILE"]
login_region = os.environ["WB2A_LOGIN_REGION"]
with open(path, encoding="utf-8") as f:
    doc = json.load(f)
configured = str(doc.get("region", "cn")).strip().lower()
if configured not in ("all", login_region):
    doc["region"] = "all"
    directory = os.path.dirname(os.path.abspath(path)) or "."
    mode = stat.S_IMODE(os.stat(path).st_mode)
    fd, tmp_path = tempfile.mkstemp(prefix=".wb2api-config-", suffix=".tmp", dir=directory)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(doc, f, indent=2, ensure_ascii=False)
            f.write("\n")
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp_path, mode)
        try:
            owner = os.stat(path)
            os.chown(tmp_path, owner.st_uid, owner.st_gid)
        except (AttributeError, PermissionError, OSError):
            pass
        os.replace(tmp_path, path)
        print("账号池配置已切换为混合模式")
    finally:
        try:
            os.unlink(tmp_path)
        except FileNotFoundError:
            pass
PYEOF
    then
        echo "警告：无法保存混合区域配置；请手动将 $CONFIG_FILE 的 region 设置为 all"
    fi
fi

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-tistzach}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
