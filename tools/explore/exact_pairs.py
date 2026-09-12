"""exact_pairs.py — 用"分钟内只有 1 条请求"的分钟做精确逐请求配对。

这是 L3 JOIN 的最佳情况：该分钟内账单恰好 1 行、本地恰好 1 行，
时间（到分钟）+ 模型足以唯一确定配对，**无需 ID**。

用这些精确配对检验：
  E1. 逐请求的积分与各 token 维度的关系（是否有最小计费、是否向上取整）。
  E2. 用 n=1 配对做非负最小二乘，估计各维度单价。
  E3. 关键假设 H: 缓存写入(未命中输入)是否与输入同价。
"""

import datetime
import sqlite3
import sys
from collections import defaultdict

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"


def load():
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
    cols = ["created_at", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()
    return b, l


def main():
    b, l = load()
    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)
           and not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]

    bmin = defaultdict(list)
    for x in b:
        bmin[x["ts"]].append(x)
    lmin = defaultdict(list)
    for x in win:
        lmin[x["ts"].replace(second=0, microsecond=0)].append(x)

    pairs = []
    for k, bl in bmin.items():
        ll = lmin.get(k, [])
        if len(bl) == 1 and len(ll) == 1 and bl[0]["模型"] == ll[0]["model"]:
            pairs.append((bl[0], ll[0]))

    print(f"精确配对（分钟唯一 + 模型一致）: {len(pairs)} 对\n")
    print(f"{'时间':>6s} {'模型':20s} {'un':>7s} {'cr':>8s} {'out':>6s} {'y':>6s} "
          f"{'y/un*1k':>9s} {'y/(un+cr)':>10s}")
    for B, L in sorted(pairs, key=lambda p: p[1]["ts"]):
        un = L["input_tokens"] - L["cache_read_tokens"]
        cr = L["cache_read_tokens"]
        out = L["output_tokens"]
        y = B["credits"]
        r1 = y / (un / 1000) if un else float("nan")
        r2 = y / ((un + cr) / 1000) if (un + cr) else float("nan")
        print(f"{B['ts'].strftime('%H:%M'):>6s} {L['model']:20s} {un:7d} {cr:8d} {out:6d} {y:6.2f} "
              f"{r1:9.4f} {r2:10.6f}")

    # ---------- E1: 最小计费 / 取整 ----------
    print("\n=== E1 最小计费与取整 ===")
    tiny = [(B, L) for B, L in pairs if (L["input_tokens"] + L["output_tokens"]) < 300]
    print("  小请求（总 token < 300）:")
    for B, L in tiny:
        tot = L["input_tokens"] + L["output_tokens"]
        print(f"    {B['ts'].strftime('%H:%M')} tokens={tot:4d} credits={B['credits']:.2f}")
    # 零积分请求的 token 上界
    zero = [(B, L) for B, L in pairs if B["credits"] == 0]
    nonzero = [(B, L) for B, L in pairs if B["credits"] > 0]
    if zero:
        print(f"\n  零积分请求 {len(zero)} 条，其 token 规模:")
        for B, L in zero:
            print(f"    {B['ts'].strftime('%H:%M')} un={L['input_tokens']-L['cache_read_tokens']:6d} "
                  f"cr={L['cache_read_tokens']:7d} out={L['output_tokens']:5d}")
    if nonzero:
        mn = min(B["credits"] for B, _ in nonzero)
        print(f"\n  非零积分最小值: {mn}  → 若为向上取整，则真实值 ∈ (0, {mn}]")
        print(f"  → 最小计费步长证据: 账单取值集合 "
              f"{sorted(set(B['credits'] for B, _ in pairs))}")

    # ---------- E2/E3: 拟合 ----------
    print("\n=== E2/E3 拟合（用全部精确配对）===")
    # 模型 A: p_in*(un+cw) + p_cr*cr + p_out*out   （cw == un，同价假设内建）
    # 模型 B: p_in*un + p_cr*cr + p_out*out （把 un 当输入，cw 不单独计）
    for tag, cols_fn in (
        ("A: un+cr+out", lambda L: (L["input_tokens"] - L["cache_read_tokens"], L["cache_read_tokens"], L["output_tokens"])),
        ("B: un+cr (无输出)", lambda L: (L["input_tokens"] - L["cache_read_tokens"], L["cache_read_tokens"], 0)),
        ("C: 全输入+输出", lambda L: (L["input_tokens"], 0, L["output_tokens"])),
    ):
        rows = [(cols_fn(L), B["credits"]) for B, L in pairs]
        p = fit_nnls(rows)
        if p:
            names = ["p_in", "p_cr", "p_out"]
            print(f"  [{tag}] " + "  ".join(f"{n}={v:.6f}" for n, v in zip(names, p)))
            pred = [sum(r[0][a] / 1000.0 * p[a] for a in range(3)) for r in rows]
            res = [rows[i][1] - pred[i] for i in range(len(rows))]
            print(f"       MAE={sum(abs(r) for r in res)/len(res):.4f} max={max(abs(r) for r in res):.4f}")

    # ---------- 用大请求单独估计 p_cr（小请求舍入噪声占比过大）----------
    print("\n=== 只用 token>5000 的配对（降低舍入相对误差）===")
    big = [(B, L) for B, L in pairs if (L["input_tokens"] + L["output_tokens"]) > 5000]
    print(f"  条数: {len(big)}")
    for B, L in big:
        un = L["input_tokens"] - L["cache_read_tokens"]
        cr = L["cache_read_tokens"]
        out = L["output_tokens"]
        print(f"    {B['ts'].strftime('%H:%M')} un={un:7d} cr={cr:8d} out={out:6d} y={B['credits']:5.2f}")


def fit_nnls(rows):
    n = len(rows)
    if n < 3:
        return None
    A = [[r[0][0] / 1000.0, r[0][1] / 1000.0, r[0][2] / 1000.0] for r in rows]
    y = [r[1] for r in rows]
    p = 3
    AtA = [[sum(A[i][a] * A[i][b] for i in range(n)) for b in range(p)] for a in range(p)]
    Aty = [sum(A[i][a] * y[i] for i in range(n)) for a in range(p)]
    Lc = max(sum(abs(v) for v in row) for row in AtA)
    step = 1.0 / max(Lc, 1e-12)
    x = [0.0] * p
    for _ in range(60000):
        g = [sum(AtA[a][b] * x[b] for b in range(p)) - Aty[a] for a in range(p)]
        for a in range(p):
            x[a] = max(0.0, x[a] - step * g[a])
    return x


if __name__ == "__main__":
    main()
