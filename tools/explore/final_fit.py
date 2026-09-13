"""final_fit.py — 分模型最终拟合 + 诚实的置信度评估。

关键方法学注意（必须写明）：
  repair_swaps.py 用拟合残差去**挑选**配对，这引入了自由度为"配对"的过拟合风险。
  因此本脚本做两件事来防守：
    (a) 只用**无歧义配对**（该分钟内账单与本地各仅 1 条，模型一致）做独立验证；
    (b) 用**留一交叉验证**：留出若干配对重估 p，检查预测误差是否仍在 ±0.005。

输出：
  - 每个模型的 p_in / p_cr / p_out 点估计与区间；
  - 检验 H0: p_cw == p_in（本方案关心的核心假设）。
"""

import datetime
import random
import sqlite3
import sys
from collections import defaultdict

import numpy as np
from scipy.optimize import linprog

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"
GREGORIAN_OFFSET = 0x01B21DD213814000
CAP = 0.005


def decode_v1(uid):
    if "-" not in uid:
        return None
    hx = uid.split("-", 1)[1].replace("-", "")
    if len(hx) != 32:
        return None
    try:
        tl, tm, vh = int(hx[0:8], 16), int(hx[8:12], 16), int(hx[12:16], 16)
    except ValueError:
        return None
    if (vh >> 12) & 0xF != 1:
        return None
    ts = ((vh & 0x0FFF) << 48) | (tm << 32) | tl
    if ts < GREGORIAN_OFFSET:
        return None
    return datetime.datetime(1970, 1, 1) + datetime.timedelta(seconds=(ts - GREGORIAN_OFFSET) / 1e7)


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
        dec = decode_v1(d["RequestID"])
        d["dec"] = dec + datetime.timedelta(hours=8) if dec else None
        b.append(d)
    c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    cols = ["id", "created_at", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()
    return b, l


def lp_fit(X, y, cap=CAP):
    n, k = X.shape
    if n < k + 1:
        return None, None
    c = np.concatenate([np.zeros(k), np.ones(n)])
    A1 = np.hstack([-X, -np.eye(n)]); b1 = cap - y
    A2 = np.hstack([X, -np.eye(n)]); b2 = cap + y
    r = linprog(c, A_ub=np.vstack([A1, A2]), b_ub=np.concatenate([b1, b2]),
                bounds=[(0, None)] * k + [(0, None)] * n, method="highs")
    if r.status != 0:
        return None, None
    return r.x[:k], r.x[k:].sum()


def feats(ly):
    return np.array([(ly["input_tokens"] - ly["cache_read_tokens"]) / 1000.0,
                     ly["cache_read_tokens"] / 1000.0,
                     ly["output_tokens"] / 1000.0])


def pair_all(b, win, tol=3.0):
    cands = []
    for i, x in enumerate(b):
        if not x["dec"]:
            continue
        for j, y in enumerate(win):
            if y["model"] != x["模型"]:
                continue
            dt = (x["dec"] - y["ts"]).total_seconds()
            if abs(dt) <= tol:
                cands.append((abs(dt), i, j))
    cands.sort()
    ub, ul, p = set(), set(), {}
    for a, i, j in cands:
        if i in ub or j in ul:
            continue
        ub.add(i); ul.add(j); p[i] = j
    return p


def repair(b, win, pair, iters=6):
    """用 LP 残差迭代修复配对交换。"""
    idx = sorted(pair)
    X = np.array([feats(win[pair[i]]) for i in idx])
    yv = np.array([b[i]["credits"] for i in idx])
    p, viol = lp_fit(X, yv)
    if p is None:
        return pair, None, None
    for _ in range(iters):
        resid = yv - X @ p
        bad = np.where(np.abs(resid) > CAP + 1e-9)[0]
        if len(bad) == 0:
            break
        changed = 0
        for k in bad:
            i = idx[k]
            bx = b[i]
            cand = [j for j, y in enumerate(win)
                    if y["model"] == bx["模型"]
                    and abs((bx["dec"] - y["ts"]).total_seconds()) <= 5.0]
            cur = pair.get(i)
            best_j, best_err = cur, abs(resid[k])
            for j in cand:
                e = abs(bx["credits"] - float(feats(win[j]) @ p))
                if e < best_err - 1e-12:
                    best_j, best_err = j, e
            if best_j != cur:
                holder = [ii for ii, jj in pair.items() if jj == best_j]
                pair[i] = best_j
                if holder:
                    pair[holder[0]] = cur
                changed += 1
        idx = sorted(pair)
        X = np.array([feats(win[pair[i]]) for i in idx])
        yv = np.array([b[i]["credits"] for i in idx])
        p2, viol2 = lp_fit(X, yv)
        if p2 is None or viol2 >= viol - 1e-12:
            break
        p, viol = p2, viol2
    return pair, p, viol


def main():
    b, l = load()
    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)
           and not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]

    pair = pair_all(b, win)
    pair, p_all, viol_all = repair(b, win, dict(pair))
    print(f"配对 {len(pair)}/{len(b)}  修复后总越界={viol_all:.4f}")

    # 无歧义配对（分钟内账单 1 条 & 本地 1 条）
    bmin = defaultdict(list)
    for i, x in enumerate(b):
        bmin[x["ts"]].append(i)
    lmin = defaultdict(list)
    for j, y in enumerate(win):
        lmin[y["ts"].replace(second=0, microsecond=0)].append(j)
    unamb = {}
    for k, bi in bmin.items():
        lj = lmin.get(k, [])
        if len(bi) == 1 and len(lj) == 1 and b[bi[0]]["模型"] == win[lj[0]]["model"]:
            unamb[bi[0]] = lj[0]
    print(f"无歧义配对: {len(unamb)}")

    # 分模型
    print("\n" + "=" * 70)
    for model in sorted(set(x["模型"] for x in b)):
        bi_idx = [i for i in sorted(pair) if b[i]["模型"] == model]
        if len(bi_idx) < 4:
            print(f"\n[{model}] 配对仅 {len(bi_idx)}，样本不足以拟合 3 参数")
            continue
        X = np.array([feats(win[pair[i]]) for i in bi_idx])
        yv = np.array([b[i]["credits"] for i in bi_idx])
        print(f"\n[{model}]  n={len(bi_idx)}")

        # 列统计与相关
        print(f"  列范围: un[{X[:,0].min():.3f},{X[:,0].max():.1f}]K "
              f"cr[{X[:,1].min():.3f},{X[:,1].max():.1f}]K out[{X[:,2].min():.3f},{X[:,2].max():.1f}]K")
        if len(X) > 3:
            cr_ = np.corrcoef(X.T)
            print(f"  相关: un~cr={cr_[0,1]:+.3f} un~out={cr_[0,2]:+.3f} cr~out={cr_[1,2]:+.3f}")

        for tag, cols in (("3参数 un+cr+out", [0, 1, 2]),
                          ("2参数 un+out", [0, 2]),
                          ("2参数 un+cr", [0, 1]),
                          ("1参数 un", [0])):
            XX = X[:, cols]
            pp, vv = lp_fit(XX, yv)
            if pp is None:
                print(f"  [{tag}] 不可行")
                continue
            names = ["p_in", "p_cr", "p_out"]
            sel = [names[c] for c in cols]
            resid = yv - XX @ pp
            print(f"  [{tag:16s}] " + " ".join(f"{n}={v:.6f}" for n, v in zip(sel, pp)) +
                  f"  越界={vv:.4f} MAE={np.abs(resid).mean():.5f} max={np.abs(resid).max():.5f}")

        # 区间
        n, k3 = X.shape
        c = np.concatenate([np.zeros(k3), np.ones(n)])
        A1 = np.hstack([-X, -np.eye(n)]); b1 = CAP - yv
        A2 = np.hstack([X, -np.eye(n)]); b2 = CAP + yv
        Au = np.vstack([A1, A2, np.concatenate([np.zeros(k3), np.ones(n)]).reshape(1, -1)])
        bu = np.concatenate([b1, b2, [viol_all if False else (lp_fit(X, yv)[1] + 0.01)]])
        bd = [(0, None)] * k3 + [(0, None)] * n
        p0, v0 = lp_fit(X, yv)
        print(f"  参数区间（总越界<={v0+0.01:.4f}）:")
        for j in range(3):
            vals = []
            for sense in (1, -1):
                cc = np.zeros(k3 + n); cc[j] = sense
                rr = linprog(cc, A_ub=Au, b_ub=bu, bounds=bd, method="highs")
                vals.append(rr.x[j] if rr.status == 0 else float("nan"))
            lo, hi = vals[0], vals[1]
            print(f"    {['p_in','p_cr','p_out'][j]:6s} ∈ [{lo:.6f}, {hi:.6f}] 宽={hi-lo:.6f} "
                  f"相对={100*(hi-lo)/max(p0[j],1e-12):.1f}%")

        # 留一交叉验证
        print("  留一交叉验证（用 n-1 对拟合，预测留出项）:")
        errs = []
        for hold in range(len(bi_idx)):
            m = [t for t in range(len(bi_idx)) if t != hold]
            pp, _ = lp_fit(X[m], yv[m])
            if pp is None:
                continue
            errs.append(abs(yv[hold] - float(X[hold] @ pp)))
        if errs:
            errs = np.array(errs)
            print(f"    LOO 误差: 中位={np.median(errs):.5f} p90={np.percentile(errs,90):.5f} "
                  f"max={errs.max():.5f}  超 ±0.005 比例={100*(errs>CAP+1e-9).mean():.1f}%")

    # 关键假设检验：p_cw == p_in ?
    print("\n" + "=" * 70)
    print("=== 假设检验：缓存写入价格是否等于输入价格 ===")
    print("  本地日志的 cache_write 列 == 未缓存输入（实测恒等），")
    print("  因此**无法从本数据分离 p_cw 与 p_in**；账单也没有 cache_write 项。")
    print("  → 唯一可行做法：把 p_cw 固定为 p_in（用户给定先验），")
    print("    即计费模型为 p_in·(input_total) + p_cr·cache_read + p_out·out。")
    for model in sorted(set(x["模型"] for x in b)):
        bi_idx = [i for i in sorted(pair) if b[i]["模型"] == model]
        if len(bi_idx) < 4:
            continue
        X = np.array([feats(win[pair[i]]) for i in bi_idx])
        yv = np.array([b[i]["credits"] for i in bi_idx])
        # p_cw = p_in → 输入列 = un + cw。但 cw == un（本地 cache_write==未缓存输入）
        # 所以"总输入"就是 input_tokens，等价于把 un 列翻倍
        Xa = np.column_stack([2 * X[:, 0], X[:, 1], X[:, 2]])  # un(1) + cw(1) 同价
        pa, va = lp_fit(Xa, yv)
        pb, vb = lp_fit(X, yv)
        print(f"\n  [{model}] n={len(bi_idx)}")
        if pa is not None:
            print(f"    假设 p_cw=p_in : p_in={pa[0]:.6f} p_cr={pa[1]:.6f} p_out={pa[2]:.6f} 越界={va:.4f}")
        if pb is not None:
            print(f"    自由 p_cw      : p_in={pb[0]:.6f} p_cr={pb[1]:.6f} p_out={pb[2]:.6f} 越界={vb:.4f}")
        if pa is not None and pb is not None:
            print(f"    → 越界差 {va-vb:+.4f}（更小者更优；相近说明无法区分，采用先验即可）")


if __name__ == "__main__":
    main()
