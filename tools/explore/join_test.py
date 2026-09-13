"""join_test.py — 用真实 metrics.db + 真实账单导出，实测 JOIN 可行性与匹配率。

回答三个问题：
  Q1. 本地日志的 id 与账单的 RequestID 是否同一命名空间（前缀/长度/格式）？
  Q2. 时间戳能否对齐（时区、精度）？
  Q3. 按时间窗 + 模型配对，匹配率是多少？剩余的是什么？
"""

import datetime
import io
import sqlite3
import sys
import zipfile
import xml.etree.ElementTree as ET
from collections import Counter, defaultdict

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"


def load_billing():
    rows = read_sheet(BILLING)
    h = rows[0]
    recs = []
    for r in rows[1:]:
        r = r + [""] * (len(h) - len(r))
        d = dict(zip(h, r))
        if not any(x.strip() for x in r):
            continue
        d["credits"] = float(d["积分消耗"] or 0)
        d["ts"] = datetime.datetime.strptime(d["时间"], "%Y-%m-%d %H:%M:%S")
        recs.append(d)
    return recs


def load_local():
    c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    cols = ["id", "created_at", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens", "credits_consumed", "credit_source"]
    recs = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        recs.append(d)
    c.close()
    return recs


def main():
    billing = load_billing()
    local = load_local()
    print(f"账单行数={len(billing)}  本地日志行数={len(local)}")

    # ---------- Q1: ID 命名空间 ----------
    print("\n=== Q1 ID 命名空间 ===")
    bpre = Counter(x["RequestID"].split("-")[0] for x in billing)
    lpre = Counter(x["id"].split("-")[0].split("_")[0] for x in local)
    print(f"  账单前缀: {dict(bpre)}")
    print(f"  本地前缀: {dict(lpre)}")
    blens = Counter(len(x["RequestID"]) for x in billing)
    print(f"  账单 ID 长度: {dict(blens)}")
    print(f"  账单 ID 样例: {billing[0]['RequestID']}")
    # 本地非本地生成（非 req_）的 ID 样例
    up = [x["id"] for x in local if not x["id"].startswith("req_")]
    print(f"  本地上游 ID 数: {len(up)}  样例: {up[:3]}")
    if up:
        print(f"  上游 ID 长度: {dict(Counter(len(i) for i in up))}")
    # 直接交集
    bset = {x["RequestID"] for x in billing}
    lset = {x["id"] for x in local}
    print(f"  ID 直接交集: {len(bset & lset)}")
    # 去掉前缀后比较（前缀不同但主体相同？）
    bbody = {x["RequestID"].split("-", 1)[1] for x in billing}
    lbody = {x["id"].split("-", 1)[1] for x in local if "-" in x["id"]}
    print(f"  去前缀后交集: {len(bbody & lbody)}")
    if bbody & lbody:
        print(f"    样例: {list(bbody & lbody)[:3]}")

    # ---------- Q2: 时间对齐 ----------
    print("\n=== Q2 时间对齐 ===")
    bt = sorted(x["ts"] for x in billing)
    lt = sorted(x["ts"] for x in local)
    print(f"  账单: {bt[0]} -> {bt[-1]}")
    print(f"  本地: {lt[0]} -> {lt[-1]}")
    print(f"  账单秒位取值: {sorted(set(t.second for t in bt))}")
    print(f"  本地秒位取值（前10）: {sorted(set(t.second for t in lt))[:10]}")
    print(f"  账单时间粒度: 分钟（秒位恒为0）" if set(t.second for t in bt) == {0} else "  账单有秒精度")

    # ---------- Q3: 时间窗 + 模型配对 ----------
    print("\n=== Q3 时间窗配对（账单窗口内）===")
    b0, b1 = bt[0], bt[-1]
    win = [x for x in local if b0 - datetime.timedelta(minutes=5) <= x["ts"] <= b1 + datetime.timedelta(minutes=5)]
    print(f"  本地在账单窗口内的请求: {len(win)}  (账单 {len(billing)})")
    print(f"  本地模型分布: {dict(Counter(x['model'] for x in win))}")
    print(f"  账单模型分布: {dict(Counter(x['模型'] for x in billing))}")

    # 账单按分钟分桶
    bmin = defaultdict(list)
    for x in billing:
        bmin[x["ts"]].append(x)
    print(f"  账单不同分钟数: {len(bmin)}")

    lmin = defaultdict(list)
    for x in win:
        lmin[x["ts"].replace(second=0, microsecond=0)].append(x)
    print(f"  本地不同分钟数: {len(lmin)}")

    # 检查同一分钟内数量是否一致
    print("\n  分钟级对齐检查（账单分钟 : 账单数 vs 本地数）:")
    for m in sorted(bmin):
        print(f"    {m}  billing={len(bmin[m]):3d}  local={len(lmin.get(m, [])):3d}")

    # 本地 vs 账单 总量
    print(f"\n  本地窗口内 token 合计: in={sum(x['input_tokens'] for x in win)} "
          f"out={sum(x['output_tokens'] for x in win)} "
          f"cw={sum(x['cache_write_tokens'] for x in win)} "
          f"cr={sum(x['cache_read_tokens'] for x in win)}")
    print(f"  账单窗口内积分合计: {sum(x['credits'] for x in billing):.2f}")

    # 关键：本地有多少行是 0 token（说明 stats 没抓全）
    z = sum(1 for x in win if x["input_tokens"] == 0 and x["output_tokens"] == 0)
    print(f"  本地窗口内 0/0 token 行数: {z} / {len(win)}  <-- 这些不可用于拟合")
    print(f"  本地 credit_source: {dict(Counter(x['credit_source'] for x in win))}")


if __name__ == "__main__":
    main()
