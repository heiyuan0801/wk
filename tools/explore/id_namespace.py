"""id_namespace.py — 判定账单 RequestID 与本地日志 ID 是否可 JOIN。

已观测：账单 crb-<32hex>，本地上游 cmb-<32hex> / 本地兜底 req_<nano>_<n>，直接交集为 0。
本脚本进一步检查：
  N1. 窗口内本地行的 ID 前缀构成（cmb 是否恰好覆盖账单行数）
  N2. 两种 ID 的 UUID 结构（版本位、时间片段）是否可比
  N3. 若 ID 不可用，退回 (模型, 分钟) 桶配对的可用性
"""

import datetime
import sqlite3
import sys
from collections import Counter, defaultdict

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"


def uuid_parts(uid):
    """拆解 <prefix>-<8>-<4>-<4>-<4>-<12> 形式。"""
    try:
        p, rest = uid.split("-", 1)
        seg = rest.split("-")
        if len(seg) == 5:
            return p, seg[0], seg[1], seg[2]
    except ValueError:
        pass
    return uid, "", "", ""


def main():
    rows = read_sheet(BILLING)
    h = rows[0]
    b = []
    for r in rows[1:]:
        r = r + [""] * (len(h) - len(r))
        if not any(x.strip() for x in r):
            continue
        d = dict(zip(h, r))
        d["credits"] = float(d["积分消耗"] or 0)
        d["ts"] = datetime.datetime.strptime(d["时间"], "%Y-%m-%d %H:%M:%S")
        b.append(d)

    c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    cols = ["id", "created_at", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens", "error_code"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()

    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)]
    nz = [x for x in win if not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]

    print("=== N1 窗口内本地 ID 前缀 ===")
    print(f"  账单行数        : {len(b)}")
    print(f"  窗口内本地总行数: {len(win)}")
    print(f"  非空行数        : {len(nz)}")
    print(f"  非空前缀构成    : {dict(Counter(x['id'].split('-')[0].split('_')[0] for x in nz))}")
    print(f"  空行前缀构成    : {dict(Counter(x['id'].split('-')[0].split('_')[0] for x in win if x not in nz))}")
    print(f"  → cmb- 行数 {sum(1 for x in nz if x['id'].startswith('cmb-'))} vs 账单 {len(b)}")

    print("\n=== N2 ID 结构对比 ===")
    bp = Counter(uuid_parts(x["RequestID"])[0] for x in b)
    lp = Counter(uuid_parts(x["id"])[0] for x in nz)
    print(f"  账单前缀分布: {dict(bp)}")
    print(f"  本地前缀分布: {dict(lp)}")
    # UUID 版本位（第3段首字符）
    bver = Counter(uuid_parts(x["RequestID"])[2][:1] for x in b)
    lver = Counter(uuid_parts(x["id"])[2][:1] for x in nz if x["id"].startswith("cmb-"))
    print(f"  账单版本位: {dict(bver)}   本地(cmb)版本位: {dict(lver)}")
    print(f"  样例账单: {b[0]['RequestID']}")
    cmb = [x['id'] for x in nz if x['id'].startswith('cmb-')]
    if cmb:
        print(f"  样例本地: {cmb[0]}")
    # UUID 时间片段是否落在合理区间
    print("\n  UUID 时间片段（第2段前8位，v1 time_low）对比:")
    print(f"    账单: {[uuid_parts(x['RequestID'])[1][:8] for x in b[:4]]}")
    print(f"    本地: {[uuid_parts(x)[1][:8] for x in cmb[:4]]}")

    print("\n=== N3 桶配对可用性（ID 不可用时的退路）===")
    bmin = defaultdict(lambda: {"n": 0, "c": 0.0})
    for x in b:
        bmin[x["ts"]]["n"] += 1
        bmin[x["ts"]]["c"] += x["credits"]
    lmin = defaultdict(lambda: {"n": 0})
    for x in nz:
        lmin[x["ts"].replace(second=0, microsecond=0)]["n"] += 1
    tot_b = sum(v["n"] for v in bmin.values())
    tot_l = sum(v["n"] for v in lmin.values())
    print(f"  账单总请求 {tot_b}  本地非空总请求 {tot_l}")
    print(f"  分钟桶数: 账单 {len(bmin)}  本地 {len(lmin)}  交集 {len(set(bmin) & set(lmin))}")
    exact = sum(1 for k in set(bmin) | set(lmin)
                if bmin.get(k, {"n": -1})["n"] == lmin.get(k, {"n": -2})["n"])
    print(f"  计数完全一致的桶: {exact} / {len(set(bmin) | set(lmin))}")
    print(f"  仅账单有的桶: {sorted(set(bmin) - set(lmin))}")
    print(f"  仅本地有的桶: {sorted(set(lmin) - set(bmin))}")


if __name__ == "__main__":
    main()
