"""inspect_metrics.py — 查看 metrics.db 中可用作账单 JOIN 键的字段。"""
import datetime
import sqlite3
import sys

PATHS = [
    r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\export-tmp\metrics.db",
    r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db",
]

for p in PATHS:
    print("=" * 72)
    print(p)
    try:
        c = sqlite3.connect("file:" + p.replace("\\", "/") + "?mode=ro", uri=True)
    except Exception as e:
        print("  打不开:", e)
        continue
    tables = [r[0] for r in c.execute("select name from sqlite_master where type='table'")]
    print("  表:", tables)
    if "request_logs" not in tables:
        continue
    n = c.execute("select count(*) from request_logs").fetchone()[0]
    print("  request_logs 行数:", n)
    print("  列:", [d[0] for d in c.execute("select * from request_logs limit 1").description])

    lo, hi = c.execute("select min(created_at), max(created_at) from request_logs").fetchone()
    if lo:
        print(f"  时间范围: {datetime.datetime.fromtimestamp(lo)} -> {datetime.datetime.fromtimestamp(hi)}")

    print("  模型分布:")
    for m, k in c.execute("select model, count(*) from request_logs group by model order by 2 desc"):
        print(f"    {m:28s} {k}")

    print("  credit_source 分布:")
    for m, k in c.execute("select credit_source, count(*) from request_logs group by credit_source"):
        print(f"    {m!r:16s} {k}")

    print("  ID 样本（与账单 RequestID 对比）:")
    for r in c.execute("select id, model, credits_consumed, credit_source, input_tokens, cache_read_tokens, cache_write_tokens, output_tokens from request_logs limit 6"):
        print(f"    id={r[0]}")
        print(f"      model={r[1]} credits={r[2]} src={r[3]!r} in={r[4]} cr={r[5]} cw={r[6]} out={r[7]}")

    print("  ID 前缀分布:")
    for m, k in c.execute("select substr(id,1,4), count(*) from request_logs group by 1 order by 2 desc limit 10"):
        print(f"    {m!r:8s} {k}")

    print("  有 token 且成功且有积分的可拟合样本:")
    try:
        q = """select count(*) from request_logs
               where status=200 and input_tokens>0 and credit_source!='' and credit_source!='unknown'"""
        print("   ", c.execute(q).fetchone()[0])
    except Exception as e:
        print("    ERR", e)
    c.close()
