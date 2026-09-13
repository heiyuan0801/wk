"""model_compare.py — 判定 (a) JOIN 是否真的有效；(b) 哪个计费模型被数据支持。

已确认事实：
  F1. 账单列 = RequestID, 积分消耗, 模型, 客户端, 时间(分钟)
  F2. 账单 ID 前缀 crb-，本地日志 ID 前缀 req_(兜底) 或 cmb-(上游)，字符串无交集
  F3. 本地 input_tokens == cache_read_tokens + cache_write_tokens 恒成立
      → cache_write 其实是"未命中缓存的输入"，不是真正的 cache write
  F4. 窗口内本地 72 行，其中 12 行是 503 no_healthy_account（0 token）
      72 - 12 = 60 == 账单行数（精确吻合！）

待判定：
  Q1. 时间最近邻 JOIN 是否可信？（置换检验：打乱账单内嵌时刻或积分）
  Q2. 计费模型是 3 参数、2 参数还是 1 参数？
"""

import datetime
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


def lp_fit(X, y, cap=0.005):
    """min Σ s_i  s.t. |y - Xp| <= cap + s_i, p>=0, s>=0. 返回 (p, 总越界, 最大残差)"""
    n, k = X.shape
    c = np.concatenate([np.zeros(k), np.ones(n)])
    A1 = np.hstack([-X, -np.eye(n)]); b1 = cap - y
    A2 = np.hstack([X, -np.eye(n)]); b2 = cap + y
    A_ub = np.vstack([A1, A2]); b_ub = np.concatenate([b1, b2])
    bounds = [(0, None)] * k + [(0, None)] * n
    r = linprog(c, A_ub=A_ub, b_ub=b_ub, bounds=bounds, method="highs")
    if r.status != 0:
        return None, None, None
    p = r.x[:k]
    resid = y - X @ p
    return p, r.x[k:].sum(), np.abs(resid).max()


def main():
    b, l = load()
    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win_all = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)]
    bad = [x for x in win_all if x["input_tokens"] == 0 and x["output_tokens"] == 0]
    win = [x for x in win_all if x not in bad]
    print(f"=== F4 数据管道验证 ===")
    print(f"  本地窗口内总行数={len(win_all)}  0-token(排除)={len(bad)}  有效={len(win)}  账单={len(b)}")
    print(f"  → 有效本地行数 == 账单行数 ? {len(win)==len(b)}  ★这是最强的管道验证")
    print(f"  被排除的 0-token 行: status={set(x['status'] for x in bad)} error={set(x['error_code'] for x in bad)}")

    # ---------- Q1: 置换检验 ----------
    print(f"\n=== Q1 JOIN 可信度（置换检验）===")
    # 真实配对：账单内嵌时刻 ↔ 本地 created_at
    def match(shift=0.0, tol=2.0):
        cands = []
        for x in b:
            if not x["dec"]:
                continue
            xt = x["dec"] + datetime.timedelta(seconds=shift)
            for y in win:
                if y["model"] != x["模型"]:
                    continue
                dt = (xt - y["ts"]).total_seconds()
                if abs(dt) <= tol:
                    cands.append((abs(dt), dt, id(x), id(y)))
        cands.sort(key=lambda t: t[0])
        ub, ul, pr = set(), set(), []
        for a, dt, bi, li in cands:
            if bi in ub or li in ul:
                continue
            ub.add(bi); ul.add(li); pr.append((dt, bi, li))
        return pr

    real = match(0.0)
    print(f"  真实(0s 偏移) 配对: {len(real)}")
    # 偏移分布：内嵌时刻 - 本地时刻
    byid = {id(x): x for x in b}
    byidl = {id(y): y for y in win}
    dts = []
    for dt, bi, li in real:
        dts.append((byid[bi]["dec"] - byidl[li]["ts"]).total_seconds())
    dts_a = np.array(sorted(dts))
    print(f"  Δt(账单内嵌 - 本地) : min={dts_a.min():+.2f} 中位={np.median(dts_a):+.2f} max={dts_a.max():+.2f}")
    print(f"  全部落在 [{dts_a.min():.2f},{dts_a.max():.2f}]，跨度 {dts_a.max()-dts_a.min():.2f}s")

    rnd = random.Random(1)
    sims = []
    for _ in range(120):
        pr = match(rnd.uniform(-120, 120))
        if pr:
            dd = [(byid[bi]["dec"] + datetime.timedelta(seconds=0) - byidl[li]["ts"]).total_seconds()
                  for _, bi, li in pr]
            sims.append((len(pr), np.median(np.abs(dd))))
    sims_n = np.array([s[0] for s in sims])
    print(f"\n  随机偏移 ±120s: 配对数 均值={sims_n.mean():.1f} max={sims_n.max()} "
          f"| 真实={len(real)}")
    print(f"  → 判别力: {'真实显著更高（JOIN 有效）' if len(real) > sims_n.max() else '未显著超出随机（JOIN 存疑）'}")

    # 关键检验：把账单"积分"在配对内部打乱，看拟合是否变差
    print(f"\n  ★ 信息量检验：打乱配对的积分值后重新拟合")
    meta = []
    for dt, bi, li in real:
        x, y = byid[bi], byidl[li]
        un = y["input_tokens"] - y["cache_read_tokens"]
        meta.append((un, y["cache_read_tokens"], y["output_tokens"], x["credits"], x["模型"]))
    X = np.array([[m[0] / 1000, m[1] / 1000, m[2] / 1000] for m in meta])
    yv = np.array([m[3] for m in meta])
    p, viol, mx = lp_fit(X, yv)
    print(f"    真实配对: p={np.round(p,6)} 总越界={viol:.4f} 最大残差={mx:.4f}")
    for tag, perm in (("同模型内打乱", "model"), ("全局打乱", "global")):
        vs = []
        for s in range(30):
            r2 = random.Random(s)
            yy = yv.copy()
            if perm == "global":
                r2.shuffle(yy)
            else:
                idx_by_model = {}
                for i, m in enumerate(meta):
                    idx_by_model.setdefault(m[4], []).append(i)
                for m, idxs in idx_by_model.items():
                    vals = [yv[i] for i in idxs]
                    r2.shuffle(vals)
                    for i, v in zip(idxs, vals):
                        yy[i] = v
            pp, vv, mm = lp_fit(X, yy)
            if vv is not None:
                vs.append(vv)
        print(f"    {tag}: 总越界 均值={np.mean(vs):.4f} (真实 {viol:.4f}) "
              f"→ {'真实更优，JOIN 携带信息' if viol < np.mean(vs)*0.9 else '无显著差异'}")

    # ---------- Q2: 模型比较 ----------
    print(f"\n=== Q2 计费模型比较（{len(X)} 对配对）===")
    cols = {
        "M1: p_in·uncached 仅输入":      [X[:, 0:1]],
        "M2: p_in·uncached + p_cr":      [X[:, 0:1], X[:, 1:2]],
        "M3: p_in + p_cr + p_out":       [X[:, 0:1], X[:, 1:2], X[:, 2:3]],
        "M4: p_in·(全部输入) + p_out":    [ (X[:, 0] + X[:, 1]).reshape(-1, 1), X[:, 2:3] ],
        "M5: p_in·全部输入（含 out）":     [ (X[:, 0] + X[:, 1] + X[:, 2]).reshape(-1, 1) ],
        "M6: p_in·uncached + p_out":     [X[:, 0:1], X[:, 2:3]],
    }
    print(f"  {'模型':30s} {'参数':28s} {'总越界':>8s} {'最大残差':>9s}")
    for name, col_list in cols.items():
        XX = np.hstack(col_list)
        pp, vv, mm = lp_fit(XX, yv)
        if pp is None:
            print(f"  {name:30s} LP 不可行")
            continue
        ps = " ".join(f"{v:.5f}" for v in pp)
        print(f"  {name:30s} {ps:28s} {vv:8.4f} {mm:9.4f}")

    # ---------- 桶级聚合（免疫配对误差）----------
    print(f"\n=== 桶级聚合拟合（分钟×模型，仅计数吻合的桶）===")
    bmin = {}
    for x in b:
        k = (x["ts"], x["模型"])
        bmin.setdefault(k, {"n": 0, "c": 0.0})
        bmin[k]["n"] += 1
        bmin[k]["c"] += x["credits"]
    lmin = {}
    for y in win:
        k = (y["ts"].replace(second=0, microsecond=0), y["model"])
        lmin.setdefault(k, {"n": 0, "un": 0.0, "cr": 0.0, "out": 0.0})
        d = lmin[k]
        d["n"] += 1
        d["un"] += y["input_tokens"] - y["cache_read_tokens"]
        d["cr"] += y["cache_read_tokens"]
        d["out"] += y["output_tokens"]
    keys = sorted(set(bmin) & set(lmin))
    okk = [k for k in keys if bmin[k]["n"] == lmin[k]["n"]]
    print(f"  交集桶={len(keys)} 计数吻合桶={len(okk)}")
    print(f"  {'时间':18s} {'模型':22s} {'n':>3s} {'un':>7s} {'cr':>8s} {'out':>6s} {'y':>6s} {'y/un*1k':>8s}")
    for k in okk:
        B, L = bmin[k], lmin[k]
        r = B["c"] / (L["un"] / 1000) if L["un"] else float("nan")
        print(f"  {str(k[0])[5:16]:18s} {k[1]:22s} {B['n']:3d} {int(L['un']):7d} {int(L['cr']):8d} "
              f"{int(L['out']):6d} {B['c']:6.2f} {r:8.4f}")

    Xb = np.array([[lmin[k]["un"] / 1000, lmin[k]["cr"] / 1000, lmin[k]["out"] / 1000] for k in okk])
    yb = np.array([bmin[k]["c"] for k in okk])
    print(f"\n  桶级列相关矩阵（vs 配对级的 {np.corrcoef(X.T)[0,1]:+.3f}）:")
    cb = np.corrcoef(Xb.T)
    for r_ in cb:
        print("   ", [f"{v:+.3f}" for v in r_])
    for name, cl in (("M3: un+cr+out", [Xb[:, 0:1], Xb[:, 1:2], Xb[:, 2:3]]),
                     ("M2: un+cr", [Xb[:, 0:1], Xb[:, 1:2]]),
                     ("M1: un", [Xb[:, 0:1]])):
        XX = np.hstack(cl)
        # 桶级容差 = 0.005 * n_in_bucket
        nper = np.array([bmin[k]["n"] for k in okk])
        pp, vv, mm = lp_fit(XX, yb, cap=0.005)
        print(f"  [{name}] p={np.round(pp,6) if pp is not None else None} "
              f"总越界={vv:.4f} 最大残差={mm:.4f}  (桶内请求数 {list(nper)})")


if __name__ == "__main__":
    main()
