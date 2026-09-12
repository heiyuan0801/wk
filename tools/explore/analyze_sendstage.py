"""analyze_sendstage.py — 回答"发送 LLM 请求阶段要如何做"所需的三个事实：

S1. 上游 chat 响应里到底有没有积分/费用字段？（决定能否在发送阶段直接拿到费用）
S2. 账单是按**账号**还是全局？（决定探针能否在账号池轮换下工作）
S3. 现有网关日志里哪些字段可用于**确定性配对**？
"""

import json
import sqlite3
from collections import Counter, defaultdict

MERGED = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\request-logs-merged.json"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"

d = json.load(open(MERGED, encoding="utf-8"))
print("merged sources:", d.get("sources"))
print("merged counts:", d.get("counts"))
print("merged credits:", d.get("credits"))
print("merged match_rule:", d.get("match_rule"))
data = d["data"]

print("\n" + "=" * 70)
print("S2. 账号维度")
acc = Counter(r.get("account_uid") or "(empty)" for r in data)
print(f"  不同 account_uid 数: {len(acc)}")
for a, n in acc.most_common(10):
    print(f"    {a}  n={n}")

print("\n  按 match_status × account 交叉:")
cross = defaultdict(Counter)
for r in data:
    cross[r.get("match_status")][r.get("account_uid") or "(empty)"] += 1
for ms, c in cross.items():
    print(f"    {ms}: {dict(c)}")

print("\n" + "=" * 70)
print("S1. 上游是否返回积分")
cs = Counter(r.get("credit_source") for r in data)
print(f"  merged 里 credit_source: {dict(cs)}")
nz = [r for r in data if (r.get("credits_consumed") or 0) > 0]
print(f"  credits_consumed > 0 的行: {len(nz)} / {len(data)}")

# 原始网关日志（未合并）里的 credit_source
gw = json.load(open(r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\request-logs.json", encoding="utf-8"))
print(f"\n  原始网关导出 request-logs.json: n={len(gw['data'])}")
cs2 = Counter(r.get("credit_source") for r in gw["data"])
print(f"  原始 credit_source 分布: {dict(cs2)}")
cred2 = Counter((r.get("credits_consumed") or 0) for r in gw["data"])
print(f"  原始 credits_consumed 取值: {dict(list(cred2.items())[:8])}")

# 直接读 DB（含 WAL）
print("\n  直接读 metrics.db（含 WAL）:")
c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
n = c.execute("select count(*) from request_logs").fetchone()[0]
print(f"    行数={n}")
for row in c.execute("select credit_source, count(*), count(distinct account_uid) from request_logs group by 1"):
    print(f"    credit_source={row[0]!r} n={row[1]} distinct_accounts={row[2]}")
print("    credits_consumed 非零行数:",
      c.execute("select count(*) from request_logs where credits_consumed>0").fetchone()[0])
print("    账号分布:")
for row in c.execute("select account_uid, count(*) from request_logs group by 1 order by 2 desc limit 5"):
    print(f"      {row[0]} n={row[1]}")
print("    error_code 分布:")
for row in c.execute("select error_code, count(*) from request_logs group by 1 order by 2 desc limit 6"):
    print(f"      {row[0]!r} n={row[1]}")
c.close()

print("\n" + "=" * 70)
print("S3. 可用于确定性配对的字段")
sample = data[0]
for k in ["created_at", "created_at_iso", "upstream_uuid_time", "upstream_sheet_time",
          "gateway_request_id", "upstream_request_id", "match_delta_sec", "latency_ms",
          "ttfb_ms", "mode", "route", "status"]:
    print(f"    {k:22s} = {sample.get(k)}")

print("\n  match_delta_sec 分布（已配对行）:")
ds = sorted(r["match_delta_sec"] for r in data if r.get("match_delta_sec") is not None)
if ds:
    import statistics
    print(f"    n={len(ds)} min={ds[0]:.3f} 中位={statistics.median(ds):.3f} max={ds[-1]:.3f}")

print("\n  gateway_only 行的特征（为什么没配上）:")
go = [r for r in data if r.get("match_status") == "gateway_only"]
print(f"    数量={len(go)}  status={dict(Counter(r.get('status') for r in go))}")
print(f"    error_code={dict(Counter(r.get('error_code') for r in go))}")
print(f"    模型={dict(Counter(r.get('model') for r in go))}")
print(f"    token 全为0者={sum(1 for r in go if not r.get('input_tokens') and not r.get('output_tokens'))}")
