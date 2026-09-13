"""pair_and_fit.py — 逐请求配对 + 真实拟合尝试。

配对键（账单无 token、ID 前缀不同，故不能用 ID）：
  (分钟, 模型) 桶内按积分升序 / token 升序无法直接对应，因此采用：
    - 桶内 n 相等 → 视为一一对应，做"同桶内积分配对"
  这只能给出**桶级**方程，对拟合而言等价于把桶内请求求和。

先做诊断，暴露 token 口径问题。
"""

import datetime
import sqlite3
import sys
from collections import Counter, defaultdict

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
    cols = ["id", "created_at", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens", "error_code", "mode", "route"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()
    return b, l


def main():
    b, l = load()
    b0 = min(x["ts"] for x in b)
    b1 = max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)]
    nz = [x for x in win if not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]

    print("=== 逐请求 token/积分明细（账单窗口内，非空行）===")
    print(f"{'时间':20s} {'模型':22s} {'mode':7s} {'in':>7s} {'cr':>8s} {'cw':>7s} {'out':>6s} {'total':>7s}")
    for x in sorted(nz, key=lambda v: v["ts"]):
        print(f"{str(x['ts']):20s} {x['model']:22s} {x['mode']:7s} {x['input_tokens']:7d} "
              f"{x['cache_read_tokens']:8d} {x['cache_write_tokens']:7d} {x['output_tokens']:6d} "
              f"{x['input_tokens']+x['output_tokens']:7d}")

    print("\n=== 异常检查：out 特别大的行 ===")
    for x in sorted(nz, key=lambda v: -v["output_tokens"])[:6]:
        print(f"  {x['ts']} {x['model']} out={x['output_tokens']} in={x['input_tokens']} "
              f"cr={x['cache_read_tokens']} route={x['route']}")

    print("\n=== 分钟×模型 桶级方程 ===")
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

    keys = sorted(set(bb) | set(ll))
    print(f"{'时间':20s} {'模型':22s} {'bn':>3s} {'ln':>3s} {'un':>8s} {'cr':>9s} {'out':>7s} {'credits':>8s} {'match':>6s}")
    rows = []
    for k in keys:
        B, L = bb.get(k), ll.get(k)
        bn = B["n"] if B else 0
        ln = L["n"] if L else 0
        ok = "OK" if bn == ln else "MISMATCH"
        print(f"{str(k[0]):20s} {k[1]:22s} {bn:3d} {ln:3d} "
              f"{L['un'] if L else 0:8d} {L['cr'] if L else 0:9d} {L['out'] if L else 0:7d} "
              f"{B['credits'] if B else 0:8.2f} {ok:>6s}")
        if bn == ln and bn > 0:
            rows.append((k[1], L["un"], L["cr"], L["out"], B["credits"], bn))

    print(f"\n可用于拟合的桶数: {len(rows)}")

    # 只用一个模型做 3 参数最小二乘（un, cr, out）→ credits，非负 + 无正则
    for model in sorted(set(r[0] for r in rows)):
        sub = [r for r in rows if r[0] == model]
        if len(sub) < 4:
            print(f"\n[{model}] 桶数不足 ({len(sub)})，跳过拟合")
            continue
        print(f"\n[{model}] 桶数={len(sub)} 尝试 3 参数拟合")
        fit(sub)


def fit(sub):
    """用高斯消元做非负最小二乘（NNLS 简化：先普通 LS 看系数符号）。"""
    # X: n x 3 (un/1000, cr/1000, out/1000), y: credits
    X = [[r[1] / 1000.0, r[2] / 1000.0, r[3] / 1000.0] for r in sub]
    y = [r[4] for r in sub]
    n, p = len(X), 3

    # 正规方程
    XtX = [[sum(X[i][a] * X[i][b] for i in range(n)) for b in range(p)] for a in range(p)]
    Xty = [sum(X[i][a] * y[i] for i in range(n)) for a in range(p)]
    print("  XtX:")
    for r_ in XtX:
        print("   ", [f"{v:.4g}" for v in r_])
    print("  Xty:", [f"{v:.4g}" for v in Xty])

    # 条件数（粗略：用 XtX 对角比值 + 相关矩阵）
    corr = [[XtX[a][b] / (XtX[a][a] ** 0.5 * XtX[b][b] ** 0.5) if XtX[a][a] > 0 and XtX[b][b] > 0 else 0
             for b in range(p)] for a in range(p)]
    print("  相关矩阵:")
    for r_ in corr:
        print("   ", [f"{v:+.3f}" for v in r_])

    sol = gauss(XtX, Xty)
    if sol is None:
        print("  奇异，无法求解")
        return
    names = ["p_in(未缓存)", "p_cr(缓存读)", "p_out(输出)"]
    print("  OLS 解:")
    for nm, v in zip(names, sol):
        print(f"    {nm:14s} = {v:+.6f} 积分/1K")
    pred = [sum(X[i][a] * sol[a] for a in range(p)) for i in range(n)]
    res = [y[i] - pred[i] for i in range(n)]
    print(f"  残差: max={max(abs(r) for r in res):.4f} sum={sum(res):+.4f}")
    for i in range(n):
        print(f"    y={y[i]:6.2f} pred={pred[i]:6.2f} res={res[i]:+7.3f}  "
              f"un={X[i][0]*1000:8.0f} cr={X[i][1]*1000:9.0f} out={X[i][2]*1000:7.0f}")


def gauss(A, bvec):
    n = len(A)
    M = [A[i][:] + [bvec[i]] for i in range(n)]
    for c in range(n):
        piv = max(range(c, n), key=lambda r_: abs(M[r_][c]))
        if abs(M[piv][c]) < 1e-12:
            return None
        M[c], M[piv] = M[piv], M[c]
        pv = M[c][c]
        M[c] = [v / pv for v in M[c]]
        for r_ in range(n):
            if r_ != c and M[r_][c] != 0:
                f = M[r_][c]
                M[r_] = [a - f * bb for a, bb in zip(M[r_], M[c])]
    return [M[i][n] for i in range(n)]


if __name__ == "__main__":
    main()
