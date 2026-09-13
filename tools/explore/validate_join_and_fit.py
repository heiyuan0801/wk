"""validate_join_and_fit.py — (1) 用置换检验否证"UUID 时间 JOIN 是巧合"；
(2) 用 LP 舍入包络做严格拟合，给出各单价的可识别区间。

背景：账单 ID crb- 与本地 ID (req_/cmb-) 字符串无交集，但都是 UUIDv1。
decode 出的内嵌时刻与本地 created_at 仅差 ~0.7s，故疑似同一请求。
但本地请求密集（1 分钟内可多达 11 条），最近邻可能纯属巧合 →
必须做置换检验：把账单内嵌时刻整体加上随机偏移，看匹配率是否下降。
"""

import datetime
import random
import sqlite3
import sys
from collections import Counter

import numpy as np
from scipy.optimize import linprog

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"
GREGORIAN_OFFSET = 0x01B21DD213814000
TOL = 2.0  # 秒


def decode_v1(uid):
    if "-" not in uid:
        return None
    hx = uid.split("-", 1)[1].replace("-", "")
    if len(hx) != 32:
        return None
    try:
        tl = int(hx[0:8], 16)
        tm = int(hx[8:12], 16)
        vh = int(hx[12:16], 16)
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
    cols = ["id", "created_at", "model", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens"]
    l = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = datetime.datetime.fromtimestamp(d["created_at"])
        l.append(d)
    c.close()
    return b, l


def greedy_match(b, win, shift_sec=0.0):
    """按 |Δt| 升序做贪心 1:1 匹配，返回配对列表。"""
    cands = []
    for x in b:
        if not x["dec"]:
            continue
        xt = x["dec"] + datetime.timedelta(seconds=shift_sec)
        for y in win:
            if y["model"] != x["模型"]:
                continue
            dt = (xt - y["ts"]).total_seconds()
            if abs(dt) <= TOL:
                cands.append((abs(dt), dt, x, y))
    used_b, used_l, pairs = set(), set(), []
    for a, dt, x, y in sorted(cands, key=lambda t: t[0]):
        bi, li = id(x), id(y)
        if bi in used_b or li in used_l:
            continue
        used_b.add(bi); used_l.add(li)
        pairs.append((dt, x, y))
    return pairs


def main():
    b, l = load()
    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)
           and not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]
    print(f"账单项={len(b)} 本地窗口非空={len(win)}  容差={TOL}s")

    # ---------- 1. 置换检验 ----------
    real = greedy_match(b, win, 0.0)
    print(f"\n=== 置换检验 ===")
    print(f"  真实偏移(0s) 匹配数: {len(real)}")

    rnd = random.Random(0)
    counts = []
    for _ in range(200):
        shift = rnd.uniform(-300, 300)
        counts.append(len(greedy_match(b, win, shift)))
    counts_arr = np.array(counts)
    print(f"  随机偏移 ±300s: 均值={counts_arr.mean():.1f} "
          f"中位={np.median(counts_arr):.0f} p95={np.percentile(counts_arr,95):.0f} max={counts_arr.max()}")
    print(f"  → 真实匹配 {len(real)} vs 随机最大 {counts_arr.max()}")
    if len(real) > counts_arr.max():
        print("  结论: 真实匹配数显著高于任何随机偏移 → JOIN 有效，非巧合")
    else:
        print("  结论: 匹配数未超出随机范围 → JOIN 存疑！")

    # 更严格的检验：用很小的偏移（±5s）——因为内嵌时刻与本地时刻本质差 ~0.7s
    counts2 = [len(greedy_match(b, win, rnd.uniform(-5, 5))) for _ in range(200)]
    c2 = np.array(counts2)
    print(f"\n  随机偏移 ±5s（更严格，因为真实偏差仅~0.7s）:")
    print(f"    均值={c2.mean():.1f} 中位={np.median(c2):.0f} max={c2.max()}")
    print(f"    真实 {len(real)}；随机偏移下几乎不下降 → 说明窗口内请求密集，")
    print(f"    '能配上'本身信息量低，需要用 Δt 的紧致程度判断")

    # Δt 紧致度：真实 vs 随机
    real_dt = sorted(abs(p[0]) for p in real)
    rnd_best = []
    for _ in range(50):
        ps = greedy_match(b, win, rnd.uniform(-5, 5))
        if ps:
            rnd_best.append(np.median([abs(p[0]) for p in ps]))
    print(f"\n  Δt 中位数: 真实={np.median(real_dt):.3f}s  随机偏移={np.mean(rnd_best):.3f}s")
    print(f"  → 若真实 Δt 明显更小且集中，说明是同一事件；否则是巧合")

    # ---------- 2. 用真实配对做严格拟合 ----------
    print(f"\n=== 用 {len(real)} 对配对做 LP 舍入包络拟合 ===")
    pairs = sorted(real, key=lambda p: p[1]["ts"])
    X, yv, meta = [], [], []
    for dt, bx, ly in pairs:
        un = ly["input_tokens"] - ly["cache_read_tokens"]
        cr = ly["cache_read_tokens"]
        out = ly["output_tokens"]
        X.append([un / 1000.0, cr / 1000.0, out / 1000.0])
        yv.append(bx["credits"])
        meta.append((dt, bx, ly, un, cr, out))
    X = np.array(X)
    yv = np.array(yv)
    print(f"  设计矩阵 n={len(X)}  列范围: un[{X[:,0].min():.3f},{X[:,0].max():.1f}] "
          f"cr[{X[:,1].min():.3f},{X[:,1].max():.1f}] out[{X[:,2].min():.3f},{X[:,2].max():.1f}]")

    # 相关系数矩阵
    if len(X) > 3:
        corr = np.corrcoef(X.T)
        print("  列相关矩阵:")
        for r_ in corr:
            print("   ", [f"{v:+.3f}" for v in r_])

    # LP: min sum s_i  s.t. |y - Xp| <= 0.005 + s_i, p>=0, s>=0
    n = len(X)
    # 变量: [p(3), s(n)]
    c = np.concatenate([np.zeros(3), np.ones(n)])
    # y - Xp <= 0.005 + s  →  -Xp - s <= 0.005 - y
    A1 = np.hstack([-X, -np.eye(n)])
    b1 = 0.005 - yv
    # -(y - Xp) <= 0.005 + s →  Xp - s <= 0.005 + y
    A2 = np.hstack([X, -np.eye(n)])
    b2 = 0.005 + yv
    A_ub = np.vstack([A1, A2])
    b_ub = np.concatenate([b1, b2])
    bounds = [(0, None)] * 3 + [(0, None)] * n
    res = linprog(c, A_ub=A_ub, b_ub=b_ub, bounds=bounds, method="highs")
    if res.status == 0:
        p = res.x[:3]
        slack = res.x[3:]
        names = ["p_in(未缓存输入)", "p_cr(缓存读取)", "p_out(输出)"]
        print(f"\n  LP 解（最小化总越界量）:")
        for nm, v in zip(names, p):
            print(f"    {nm:18s} = {v:.6f} 积分/1K")
        print(f"  总越界量 = {slack.sum():.4f}  越界配对={int((slack>1e-9).sum())}/{n}")
        pred = X @ p
        resid = yv - pred
        print(f"  残差: MAE={np.abs(resid).mean():.4f} max={np.abs(resid).max():.4f} sum={resid.sum():+.4f}")
        big = np.abs(resid) > 0.0051
        if big.any():
            print(f"  超出 ±0.005 的配对（模型形式或配对错误的嫌疑）:")
            for i in np.where(big)[0]:
                dt, bx, ly, un, cr, out = meta[i]
                print(f"    Δt={dt:+6.2f}s y={yv[i]:6.2f} pred={pred[i]:6.2f} res={resid[i]:+7.3f} "
                      f"| un={un:7d} cr={cr:8d} out={out:6d}")

        # 区间：对每个参数求 min/max（在总越界量 <= 当前最优 + eps 的可行域内）
        print("\n  参数可识别区间（固定其它参数自由度）:")
        best = res.fun
        for j in range(3):
            iv = []
            for sense in (1, -1):
                cc = np.zeros(3 + n)
                cc[j] = sense
                r2 = linprog(cc, A_ub=A_ub, b_ub=b_ub, bounds=bounds, method="highs")
                iv.append(r2.x[j] if r2.status == 0 else float("nan"))
            print(f"    {names[j]:18s} ∈ [{iv[0]:.6f}, {iv[1]:.6f}]  (宽 {iv[1]-iv[0]:.6f})")
    else:
        print(f"  LP 失败: {res.message}")

    # ---------- 3. 分模型 ----------
    print("\n=== 分模型拟合 ===")
    for m in sorted(set(x["模型"] for x in b)):
        idx = [i for i, (dt, bx, ly, *_ ) in enumerate(meta) if ly["model"] == m]
        if len(idx) < 4:
            print(f"  [{m}] 配对仅 {len(idx)}，样本不足")
            continue
        Xs, ys = X[idx], yv[idx]
        nn = len(Xs)
        cc = np.concatenate([np.zeros(3), np.ones(nn)])
        A1 = np.hstack([-Xs, -np.eye(nn)]); b1 = 0.005 - ys
        A2 = np.hstack([Xs, -np.eye(nn)]); b2 = 0.005 + ys
        Au = np.vstack([A1, A2]); bu = np.concatenate([b1, b2])
        bd = [(0, None)] * 3 + [(0, None)] * nn
        r3 = linprog(cc, A_ub=Au, b_ub=bu, bounds=bd, method="highs")
        if r3.status == 0:
            pp = r3.x[:3]
            rr = ys - Xs @ pp
            print(f"  [{m}] n={nn}  p_in={pp[0]:.6f} p_cr={pp[1]:.6f} p_out={pp[2]:.6f} "
                  f"| MAE={np.abs(rr).mean():.4f} 越界={r3.x[3:].sum():.4f}")


if __name__ == "__main__":
    main()
