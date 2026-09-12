"""analyze_join.py — 用真实账单+真实日志验证数据口径，并实测费率拟合。

关键待验证事实：
  F1. 本地 input_tokens 是否 = cache_read + cache_write（即 cache_write 列其实是"未命中输入"）
  F2. 本地 0/0 token 行数 是否恰好等于 billing 缺失的行数
  F3. 账单 ID（crb-）与上游 ID（cmb-）是否同一命名空间
  F4. 分钟级聚合能否支撑一次真实拟合
"""

import datetime
import sqlite3
import sys
from collections import Counter, defaultdict

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"


def load_billing():
    rows = read_sheet(BILLING)
    h = rows[0]
    out = []
    for r in rows[1:]:
        r = r + [""] * (len(h) - len(r))
        if not any(x.strip() for x in r):
            continue
        d = dict(zip(h, r))
        d["credits"] = float(d["积分消耗"] or 0)
        d["ts"] = datetime.datetime.strptime(d["时间"], "%Y-%m-%d %H:%M:%S")
        out.append(d)
    return out


def load_local():
    c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    cols = ["id", "created_at", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens", "credits_consumed", "credit_source",
            "passthrough", "error_code"]
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

    # ---------- F1: token 口径 ----------
    print("=== F1 本地 token 口径 ===")
    ok = bad = 0
    for x in local:
        if x["input_tokens"] == 0:
            continue
        if x["input_tokens"] == x["cache_read_tokens"] + x["cache_write_tokens"]:
            ok += 1
        else:
            bad += 1
    print(f"  input == cache_read + cache_write 的行: {ok}")
    print(f"  不等的行: {bad}")
    nz = [x for x in local if x["input_tokens"] > 0]
    if nz:
        print(f"  样例: " + "; ".join(
            f"in={x['input_tokens']} cr={x['cache_read_tokens']} cw={x['cache_write_tokens']}"
            for x in nz[:4]))
    print(f"  cache_read > 0 的行数: {sum(1 for x in local if x['cache_read_tokens'] > 0)} / {len(local)}")

    # ---------- F2: 计数对齐 ----------
    print("\n=== F2 计数对齐（账单窗口内）===")
    b0 = min(x["ts"] for x in billing)
    b1 = max(x["ts"] for x in billing)
    win = [x for x in local if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)]
    zero = [x for x in win if x["input_tokens"] == 0 and x["output_tokens"] == 0]
    nz_win = [x for x in win if not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]
    print(f"  账单行数            : {len(billing)}")
    print(f"  本地窗口内总行数     : {len(win)}")
    print(f"  其中 0/0 token      : {len(zero)}")
    print(f"  本地非空行数         : {len(nz_win)}")
    print(f"  → 非空行数 == 账单行数 ? {len(nz_win) == len(billing)}")
    print(f"  0/0 行的 status: {dict(Counter(x['status'] for x in zero))}")
    print(f"  0/0 行的 error:  {dict(Counter(x['error_code'] for x in zero))}")

    # ---------- F3: ID 命名空间 ----------
    print("\n=== F3 ID 命名空间 ===")
    bids = sorted(x["RequestID"] for x in billing)
    lids = sorted(x["id"] for x in local if not x["id"].startswith("req_"))
    print(f"  账单 ID  前缀: crb , 样例 {bids[0]}")
    print(f"  上游 ID  前缀: cmb , 样例 {lids[0] if lids else '-'}")
    # UUID v1 时间片段对比
    print(f"  账单 ID 中间8位取值(前6): {[i.split('-')[1][8:16] for i in bids[:6]]}")
    print(f"  上游 ID 中间8位取值(前6): {[i.split('-')[1][8:16] for i in lids[:6]]}")
    common = set(bids) & set(lids)
    print(f"  直接交集: {len(common)}")

    # ---------- F4: 分钟级聚合表 ----------
    print("\n=== F4 分钟级聚合（可用于拟合的方程）===")
    bmin = defaultdict(lambda: {"n": 0, "credits": 0.0})
    for x in billing:
        k = x["ts"]
        bmin[k]["n"] += 1
        bmin[k]["credits"] += x["credits"]

    lmin = defaultdict(lambda: defaultdict(float))
    for x in nz_win:
        k = x["ts"].replace(second=0, microsecond=0)
        m = x["model"]
        lmin[k][m + ":in"] += x["input_tokens"] - x["cache_read_tokens"]
        lmin[k][m + ":cr"] += x["cache_read_tokens"]
        lmin[k][m + ":out"] += x["output_tokens"]
        lmin[k][m + ":n"] += 1

    print(f"  {'分钟':20s} {'模型':22s} {'n':>3s} {'uncached':>9s} {'cache_rd':>10s} {'out':>7s} {'credits':>8s}")
    rows = []
    for k in sorted(bmin):
        for m in sorted(set(x["模型"] for x in billing)):
            if lmin[k].get(m + ":n", 0) == 0:
                continue
            un = lmin[k][m + ":in"]
            cr = lmin[k][m + ":cr"]
            ou = lmin[k][m + ":out"]
            print(f"  {str(k):20s} {m:22s} {int(lmin[k][m+':n']):3d} {int(un):9d} {int(cr):10d} {int(ou):7d} "
                  f"{bmin[k]['credits']:8.2f}")
            rows.append((k, m, un, cr, ou, bmin[k]["credits"], int(lmin[k][m + ":n"])))

    tot_un = sum(r[2] for r in rows)
    tot_cr = sum(r[3] for r in rows)
    tot_ou = sum(r[4] for r in rows)
    tot_c = sum(r[5] for r in rows)
    print(f"\n  合计: uncached={tot_un} cache_read={tot_cr} out={tot_ou} credits={tot_c:.2f}")
    if tot_un:
        print(f"  粗暴上界 p_in ≈ {tot_c/(tot_un/1000):.6f} 积分/1K（把全部积分都算到未缓存输入上）")
    if tot_cr:
        print(f"  粗暴下界（全算缓存读取）≈ {tot_c/(tot_cr/1000):.6f} 积分/1K")

    # 粗粒度拟合预览：只按模型分组
    print("\n=== 按模型分组的粗略估计（仅未缓存输入列，忽略 cache_read/out）===")
    for m in sorted(set(r[1] for r in rows)):
        sub = [r for r in rows if r[1] == m]
        u = sum(r[2] for r in sub)
        c = sum(r[5] for r in sub)
        if u:
            print(f"  {m:24s} uncached={u:9.0f} credits={c:6.2f}  → {c/(u/1000):.6f} 积分/1K")


if __name__ == "__main__":
    main()
