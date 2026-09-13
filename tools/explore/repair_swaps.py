"""repair_swaps.py — 用"拟合残差 > 0.005"检测并修复 JOIN 配对错误（错配/交换）。

动机（validate_join_and_fit.py 的 LP 残差暴露）：
  最近邻贪心配对在密集时段会**交换**相邻两条请求的积分：
    Δt=+0.53s y=0.00 pred=0.17 un=11361    ← 应该拿 0.17
    Δt=+0.55s y=0.17 pred=0.00 un=110      ← 应该拿 0.00
  两条的残差正好互为相反数（±0.168），是典型的交换特征。

算法（迭代）：
  1. 初次配对（UUIDv1 内嵌时刻 + 模型 + 容差）
  2. LP 拟合（min Σ 越界量，硬约束 |y - Xp| <= 0.005）
  3. 找出残差超限的项，在同(模型)且时间邻近的候选集合内做**局部最优重配对**
     （用当前 p̂ 计算代价，匈牙利/贪心最小化总残差）
  4. 重复 2-3 直到无可改进或达上限
"""

import datetime
import itertools
import random
import sqlite3
import sys

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
            "cache_read_tokens", "cache_write_tokens", "error_code"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()
    return b, l


def lp_fit(X, y, cap=CAP):
    n, k = X.shape
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


def main():
    b, l = load()
    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)
           and not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]
    print(f"账单={len(b)} 本地有效={len(win)}")

    # --- 1. 初始配对：每个账单项取同模型最近邻（贪心 1:1）---
    def assign(shift=0.0, tol=3.0):
        cands = []
        for i, x in enumerate(b):
            if not x["dec"]:
                continue
            xt = x["dec"] + datetime.timedelta(seconds=shift)
            for j, y in enumerate(win):
                if y["model"] != x["模型"]:
                    continue
                dt = (xt - y["ts"]).total_seconds()
                if abs(dt) <= tol:
                    cands.append((abs(dt), i, j))
        cands.sort()
        ub, ul, pair = set(), set(), {}
        for a, i, j in cands:
            if i in ub or j in ul:
                continue
            ub.add(i); ul.add(j); pair[i] = j
        return pair

    pair = assign()
    print(f"初始配对: {len(pair)} / {len(b)}")

    # --- 2-3. 迭代：拟合 → 局部重配对 ---
    X = np.array([feats(win[pair[i]]) for i in sorted(pair)])
    yv = np.array([b[i]["credits"] for i in sorted(pair)])
    idx = sorted(pair)
    p, viol = lp_fit(X, yv)
    print(f"\n初始拟合: p_in={p[0]:.6f} p_cr={p[1]:.6f} p_out={p[2]:.6f} 总越界={viol:.4f}")

    for it in range(1, 6):
        resid = yv - X @ p
        bad = np.where(np.abs(resid) > CAP + 1e-9)[0]
        print(f"\n--- 迭代 {it}: 越限项 {len(bad)} ---")
        if len(bad) == 0:
            print("  所有残差均在 ±0.005 内，收敛")
            break

        # 收集涉及到的账单项与它们的候选本地项（同模型、时间邻近）
        changed = 0
        for k in bad:
            i = idx[k]
            bx = b[i]
            # 候选：同模型，|内嵌时刻 - 本地时刻| <= 5s
            cand = [j for j, y in enumerate(win)
                    if y["model"] == bx["模型"]
                    and abs((bx["dec"] - y["ts"]).total_seconds()) <= 5.0]
            if not cand:
                continue
            # 当前占用者
            cur = pair.get(i)
            # 选使 |y - X_j p| 最小的候选
            best_j, best_err = cur, abs(resid[k])
            for j in cand:
                e = abs(bx["credits"] - float(feats(win[j]) @ p))
                if e < best_err - 1e-12:
                    best_j, best_err = j, e
            if best_j != cur:
                # 若目标已被别人占用，则交换
                holder = [ii for ii, jj in pair.items() if jj == best_j]
                pair[i] = best_j
                if holder:
                    pair[holder[0]] = cur
                    print(f"    交换: 账单 {bx['RequestID'][-8:]} {bx['ts'].strftime('%H:%M')} "
                          f"y={bx['credits']:.2f} ↔ {holder[0]} "
                          f"(Δt {(bx['dec']-win[best_j]['ts']).total_seconds():+.2f}s→ "
                          f"{abs(bx['credits']-float(feats(win[best_j])@p)):.4f})")
                else:
                    print(f"    重指: 账单 {bx['RequestID'][-8:]} y={bx['credits']:.2f} "
                          f"err {abs(resid[k]):.4f} → {best_err:.4f}")
                changed += 1
        # 重新拟合
        idx = sorted(pair)
        X = np.array([feats(win[pair[i]]) for i in idx])
        yv = np.array([b[i]["credits"] for i in idx])
        p2, viol2 = lp_fit(X, yv)
        print(f"  重拟合: p={np.round(p2,6)} 总越界={viol2:.4f} (之前 {viol:.4f}) 改动={changed}")
        if viol2 >= viol - 1e-9:
            print("  无改进，停止")
            p, viol = p2, viol2
            break
        p, viol = p2, viol2

    print(f"\n=== 最终结果（{len(idx)} 对）===")
    print(f"  p_in(未缓存输入) = {p[0]:.6f} 积分/1K")
    print(f"  p_cr(缓存读取)   = {p[1]:.6f} 积分/1K")
    print(f"  p_out(输出)      = {p[2]:.6f} 积分/1K")
    print(f"  比值 p_out/p_in = {p[2]/p[0]:.2f}   p_cr/p_in = {p[1]/p[0]:.4f}")
    resid = yv - X @ p
    print(f"  残差: MAE={np.abs(resid).mean():.5f} max={np.abs(resid).max():.5f} sum={resid.sum():+.5f}")
    over = np.where(np.abs(resid) > CAP + 1e-9)[0]
    print(f"  仍越限: {len(over)} / {len(idx)}")
    for k in over:
        i = idx[k]
        print(f"    y={yv[k]:6.2f} pred={X[k]@p:6.2f} res={resid[k]:+7.4f} | "
              f"un={X[k,0]*1000:7.0f} cr={X[k,1]*1000:8.0f} out={X[k,2]*1000:6.0f}")

    # 对照：打乱后拟合
    print("\n=== 对照：同模型内打乱积分 ===")
    rnd = random.Random(0)
    by_model = {}
    for k, i in enumerate(idx):
        by_model.setdefault(b[i]["模型"], []).append(k)
    vs = []
    for s in range(20):
        r = random.Random(s)
        yy = yv.copy()
        for m, ks in by_model.items():
            vals = [yv[k] for k in ks]
            r.shuffle(vals)
            for k, v in zip(ks, vals):
                yy[k] = v
        _, v2 = lp_fit(X, yy)
        if v2 is not None:
            vs.append(v2)
    print(f"  打乱后总越界 均值={np.mean(vs):.4f}  真实={viol:.4f}  "
          f"→ 真实{'显著更优' if viol < np.mean(vs)*0.5 else '优势不明显'}")

    # 区间估计
    print("\n=== 参数区间（在总越界 <= 最优+0.01 的可行域内）===")
    n, k3 = X.shape
    c = np.concatenate([np.zeros(k3), np.ones(n)])
    A1 = np.hstack([-X, -np.eye(n)]); b1 = CAP - yv
    A2 = np.hstack([X, -np.eye(n)]); b2 = CAP + yv
    Au = np.vstack([A1, A2]); bu = np.concatenate([b1, b2])
    # 附加：总越界 <= viol + 0.01
    A3 = np.concatenate([np.zeros(k3), np.ones(n)]).reshape(1, -1)
    Au = np.vstack([Au, A3]); bu = np.concatenate([bu, [viol + 0.01]])
    bd = [(0, None)] * k3 + [(0, None)] * n
    names = ["p_in", "p_cr", "p_out"]
    for j in range(3):
        lo = hi = None
        for sense in (1, -1):
            cc = np.zeros(k3 + n); cc[j] = sense
            rr = linprog(cc, A_ub=Au, b_ub=bu, bounds=bd, method="highs")
            if rr.status == 0:
                if sense == 1:
                    lo = rr.x[j]
                else:
                    hi = rr.x[j]
        if lo is not None and hi is not None:
            print(f"  {names[j]:6s} ∈ [{lo:.6f}, {hi:.6f}]   宽={hi-lo:.6f}  "
                  f"相对={100*(hi-lo)/max(p[j],1e-9):.1f}%")


if __name__ == "__main__":
    main()
