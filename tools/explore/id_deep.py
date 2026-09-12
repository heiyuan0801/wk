"""id_deep.py — 弄清本地日志里 cmb- / req_ 两类 ID 的来源与时间分布，
判定账单 crb- ID 能否与任何本地记录对上。
"""

import datetime
import sqlite3
from collections import Counter, defaultdict

DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"
c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)

print("=== 按 ID 前缀分组的时间范围与模型 ===")
q = """
select case
         when id like 'cmb-%' then 'cmb-'
         when id like 'req_%' then 'req_'
         else 'other'
       end as kind,
       count(*), min(created_at), max(created_at)
from request_logs group by kind
"""
for kind, n, lo, hi in c.execute(q):
    print(f"  {kind:6s} n={n:5d}  {datetime.datetime.fromtimestamp(lo)} -> {datetime.datetime.fromtimestamp(hi)}")

print("\n=== cmb- 行的详情（前 15）===")
for r in c.execute("""select id, created_at, model, status, input_tokens, output_tokens,
                      cache_read_tokens, credit_source, route, error_code
                      from request_logs where id like 'cmb-%' order by created_at limit 15"""):
    ts = datetime.datetime.fromtimestamp(r[1])
    print(f"  {ts} {r[0]} model={r[2]} status={r[3]} in={r[4]} out={r[5]} cr={r[6]} src={r[7]} route={r[8]}")

print("\n=== 账单窗口 22:43-23:49 (2026-09-10) 内的行，按 route/mode 分组 ===")
b0 = datetime.datetime(2026, 9, 10, 22, 43).timestamp()
b1 = datetime.datetime(2026, 9, 10, 23, 50).timestamp()
for r in c.execute("""select route, mode, count(*), sum(case when id like 'cmb-%' then 1 else 0 end)
                      from request_logs where created_at between ? and ? group by route, mode""", (b0, b1)):
    print(f"  route={r[0]:24s} mode={r[1]:7s} n={r[2]:3d} 其中 cmb-={r[3]}")

print("\n=== 窗口内所有行的 id（确认前缀）===")
kinds = Counter()
for (i,) in c.execute("select id from request_logs where created_at between ? and ?", (b0, b1)):
    kinds[i.split("-")[0].split("_")[0]] += 1
print(f"  {dict(kinds)}")

print("\n=== 全局：cmb- ID 的完整样例与长度 ===")
for (i,) in c.execute("select id from request_logs where id like 'cmb-%' limit 3"):
    print(f"  {i} len={len(i)}")
print("=== 全局：req_ ID 的完整样例 ===")
for (i,) in c.execute("select id from request_logs where id like 'req_%' limit 3"):
    print(f"  {i} len={len(i)}")

print("\n=== 检查是否存在 crb- 前缀的任何记录 ===")
n = c.execute("select count(*) from request_logs where id like 'crb-%'").fetchone()[0]
print(f"  crb- 行数: {n}")

print("\n=== metrics 聚合表 ===")
for r in c.execute("select data from metrics"):
    print(" ", r[0][:400])
c.close()
