"""probe_rates.py — 用真实数据测试费率假设，找出哪个线性模型能"装下"观测积分。

背景（已由 analyze_join.py 确认）：
  - 账单只给 (RequestID, 积分, 模型, 时间)，时间粒度为分钟；本地日志 token 精确。
  - 本地 input_tokens == cache_read + cache_write 恒成立（口径：input 为总输入）。
  - 账单 ID 前缀 crb-，上游日志 ID 前缀 cmb-，**无交集**，不能用 ID 直接 JOIN。
  - 桶级（分钟×模型）计数在多数桶内吻合，但 4/16 桶 MISMATCH。

本脚本测试的假设：
  H1. p_out 为负 → 说明"输出"不是独立计费项，或未缓存输入列有误。
  H2. output 的 reasoning token 也计费（out 应按 completion 原始计数）。
  H3. 真正的干净关系：credits ≈ p_in*(un) + p_cr*(cr) + p_out*(out)，用非负最小二乘。
  H4. 只保留 "干净" 桶（不含异常大 output 的桶）后重估。
"""

import datetime
import itertools
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
    cols = ["created_at", "model", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens", "requested_output_tokens"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()
    return b, l


def nnls(A, bvec, iters=20000, lr=None):
    """投影梯度法求解非负最小二乘（无外部依赖）。"""
    n, p = len(A), len(A[0])
    x = [0.0] * p
    # 步长：1 / Lipschitz 常数（用 A^T A 的最大行和估计）
    AtA = [[sum(A[i][a] * A[i][b] for i in range(n)) for b in range(p)] for a in range(p)]
    L = max(sum(abs(v) for v in row) for row in AtA)
    step = lr or 1.0 / max(L, 1e-12)
    Aty = [sum(A[i][a] * bvec[i] for i in range(n)) for a in range(p)]
    for _ in range(iters):
        grad = [sum(AtA[a][b] * x[b] for b in range(p)) - Aty[a] for a in range(p)]
        for a in range(p):
            x[a] = max(0.0, x[a] - step * grad[a])
    return x


def report(tag, rows):
    """rows: list of (un, cr, out, credits, model, label)"""
    if len(rows) < 3:
        print(f"\n[{tag}] 样本不足 ({len(rows)})")
        return
    for use_out in (True, False):
        A = [[r[0] / 1000.0, r[1] / 1000.0] + ([r[2] / 1000.0] if use_out else []) for r in rows]
        y = [r[3] for r in rows]
        x = nnls(A, y)
        pred = [sum(A[i][a] * x[a] for a in range(len(x))) for i in range(len(A))]
        res = [y[i] - pred[i] for i in range(len(y))]
        mae = sum(abs(r) for r in res) / len(res)
        names = ["p_in", "p_cr"] + (["p_out"] if use_out else [])
        print(f"\n[{tag}] n={len(rows)} {'三参数' if use_out else '两参数'} NNLS:")
        print("    " + "  ".join(f"{n}={v:.6f}" for n, v in zip(names, x)))
        print(f"    MAE={mae:.4f}  maxres={max(abs(r) for r in res):.4f}  sumres={sum(res):+.4f}")
        for i in range(len(rows)):
            print(f"      y={y[i]:6.2f} pred={pred[i]:6.2f} res={res[i]:+7.3f} | "
                  f"un={rows[i][0]:7.0f} cr={rows[i][1]:8.0f} out={rows[i][2]:6.0f} | {rows[i][5]}")


def main():
    b, l = load()
    b0 = min(x["ts"] for x in b)
    b1 = max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)]
    nz = [x for x in win if not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]
    print(f"账单={len(b)} 本地窗口非空={len(nz)}")

    # 桶级
    bb = defaultdict(lambda: {"n": 0, "credits": 0.0})
    for x in b:
        k = (x["ts"], x["模型"])
        bb[k]["n"] += 1
        bb[k]["credits"] += x["credits"]
    ll = defaultdict(lambda: {"n": 0, "un": 0, "cr": 0, "out": 0})
    for x in nz:
        k = (x["ts"].replace(second=0, microsecond=0), x["model"])
        ll[k]["n"] += 1
        ll[k]["un"] += x["input_tokens"] - x["cache_read_tokens"]
        ll[k]["cr"] += x["cache_read_tokens"]
        ll[k]["out"] += x["output_tokens"]

    all_rows, ok_rows = [], []
    for k in sorted(set(bb) | set(ll)):
        if k not in bb or k not in ll:
            continue
        B, L = bb[k], ll[k]
        rec = (L["un"], L["cr"], L["out"], B["credits"], k[1], f"{k[0]} n={B['n']}")
        all_rows.append(rec)
        if B["n"] == L["n"]:
            ok_rows.append(rec)

    print(f"\n全部桶={len(all_rows)} 计数吻合桶={len(ok_rows)}")

    report("全部桶", all_rows)
    report("计数吻合桶", ok_rows)

    # 只取 single-request 桶（n==1，最干净：无配对歧义）
    single = [r for r in ok_rows if "n=1" in r[5]]
    report("单请求桶 n=1", single)

    # 按模型分开
    for m in sorted(set(r[4] for r in ok_rows)):
        report(f"模型 {m}", [r for r in ok_rows if r[4] == m])

    # H4: 剔除 output 异常桶（out 占比 > 30% 的桶）
    clean = [r for r in ok_rows if r[2] <= 0.3 * (r[0] + r[1] + r[2])]
    report("剔除高 output 桶", clean)

    print("\n=== 单参数下界/上界（稳健包络）===")
    # 对每个桶，p_in 的下界（假设其它为 0）和上界（假设其它列不贡献）
    for r in single:
        un, cr, out, c = r[0], r[1], r[2], r[3]
        if un > 0:
            print(f"  {r[5]:28s} y={c:5.2f} un={un:6d} cr={cr:7d} out={out:5d} "
                  f"| 若全算 p_in → {c/(un/1000):.4f}")


if __name__ == "__main__":
    main()
