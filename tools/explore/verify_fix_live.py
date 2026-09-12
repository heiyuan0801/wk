"""verify_fix_live.py — 端到端验证 credit 修复：
发一个真实请求，然后确认 metrics.db 里该行 credit_source='upstream' 且 credits>0。

前提：运行中的网关已用**修复后的二进制**重建/重启（go build）。
"""

import json
import sqlite3
import time
import urllib.request

BASE = "http://127.0.0.1:7863"
KEY = "123"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"


def send(text, max_tokens=5):
    payload = {"model": "deepseek-v4.1-flash",
               "messages": [{"role": "system", "content": "You are terse."},
                            {"role": "user", "content": text}],
               "stream": False, "max_tokens": max_tokens}
    req = urllib.request.Request(BASE + "/v1/chat/completions",
                                 data=json.dumps(payload).encode(),
                                 headers={"Authorization": f"Bearer {KEY}",
                                          "Content-Type": "application/json"},
                                 method="POST")
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.loads(r.read().decode())


def db_rows(limit=5):
    c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    rows = list(c.execute(
        "select id, created_at, model, input_tokens, output_tokens, "
        "cache_read_tokens, credits_consumed, credit_source "
        "from request_logs order by created_at desc limit ?", (limit,)))
    c.close()
    return rows


print("=== 修复前基线（DB 现状）===")
c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
print("  credit_source 分布:",
      dict(c.execute("select credit_source, count(*) from request_logs group by 1")))
print("  credits_consumed>0 行数:",
      c.execute("select count(*) from request_logs where credits_consumed>0").fetchone()[0])
c.close()

print("\n=== 发送一个会产生费用的请求（长输入）===")
before = db_rows(1)[0][0] if db_rows(1) else None
long_text = "The quick brown fox jumps over the lazy dog. " * 400
resp = send(long_text)
u = resp.get("usage", {})
print(f"  上游返回 usage.credit = {u.get('credit')}")
print(f"  prompt={u.get('prompt_tokens')} miss={u.get('prompt_cache_miss_tokens')} "
      f"hit={u.get('prompt_cache_hit_tokens')} comp={u.get('completion_tokens')}")

time.sleep(1.5)
print("\n=== 修复后：DB 最新一行 ===")
rows = db_rows(3)
hdr = f"{'id':30s} {'in':>6s} {'out':>5s} {'cr':>7s} {'credits':>8s} {'source':>10s}"
print(hdr)
for r in rows:
    rid, ca, model, inn, out, cr, credits, src = r
    print(f"{str(rid)[:30]:30s} {inn:6d} {out:5d} {cr:7d} {credits:8.2f} {src:>10s}")

newest = rows[0]
print()
if newest[7] == "upstream":
    print("  ✅ credit_source='upstream' —— 修复生效，上游真实积分被记录")
    print(f"     credits_consumed = {newest[6]}")
elif newest[7] == "unknown":
    print("  ⚠ 仍是 'unknown' —— 运行中的网关很可能还是**旧二进制**。")
    print("    需要用修复后的代码重建并重启服务后重测。")
else:
    print(f"  状态: source={newest[7]!r} credits={newest[6]}")

print("\n=== 全库统计 ===")
c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
print("  credit_source 分布:",
      dict(c.execute("select credit_source, count(*) from request_logs group by 1")))
print("  credits_consumed>0 行数:",
      c.execute("select count(*) from request_logs where credits_consumed>0").fetchone()[0])
c.close()
