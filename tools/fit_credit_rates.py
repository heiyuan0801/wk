"""fit_credit_rates.py — WorkBuddy 积分费率拟合（端到端，唯一权威入口）。

输入：
  1. 官网导出的请求级账单 xlsx（RequestID, 积分消耗, 模型, 客户端, 时间）
  2. 反代服务的 metrics.db（request_logs 表，token 精确）

输出：
  - stdout 诊断报告
  - rate_model.json（各模型单价 + 区间 + 诊断）

方法要点（详见 docs/credit-rate-inference.md）：
  1. 账单无 token，本地日志有 token，ID 前缀不同源 → 用 **UUIDv1 内嵌时间戳 + 模型** 配对。
  2. 配对有效性的判据**不是匹配率**（密集时段随机也能配上），而是
     **信息量检验**：打乱积分后重新拟合，拟合质量是否显著变差。
  3. 用 LP "舍入包络"拟合：|y - X·p| <= 0.005 是硬约束，最小化越界总量。
  4. 用 LP 残差**迭代修复交换配对**（相邻请求积分互换是最近邻配对的典型错误）。
  5. 区分"可识别"与"不可识别"：列完全共线时必须报告区间而非点估计。
  6. 留一交叉验证防守"用残差挑配对"带来的过拟合。

用法：
  python tools/fit_credit_rates.py --billing <xlsx> --db <metrics.db> [--model 模型名] [--json out.json]
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import sqlite3
import sys
import zipfile
import xml.etree.ElementTree as ET
from collections import defaultdict

import numpy as np
from scipy.optimize import linprog

# ---------- 常量 ----------
CAP = 0.005                  # 四舍五入到 0.01 的半宽
GREGORIAN_OFFSET = 0x01B21DD213814000  # UUIDv1 纪元偏移（100ns）
NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
TZ_OFFSET_HOURS = 8          # 账单时间为本地时间（UTC+8）


# ================= xlsx 读取（无 pandas 依赖） =================
def _col_index(ref: str) -> int:
    letters = "".join(ch for ch in ref if ch.isalpha())
    n = 0
    for ch in letters:
        n = n * 26 + (ord(ch) - ord("A") + 1)
    return n - 1


def read_sheet(path: str) -> list[list[str]]:
    with zipfile.ZipFile(path) as z:
        names = z.namelist()
        shared: list[str] = []
        if "xl/sharedStrings.xml" in names:
            root = ET.fromstring(z.read("xl/sharedStrings.xml"))
            for si in root.findall(f"{NS}si"):
                shared.append("".join(t.text or "" for t in si.iter(f"{NS}t")))
        sheet = next(n for n in names
                     if n.startswith("xl/worksheets/sheet") and n.endswith(".xml"))
        root = ET.fromstring(z.read(sheet))
    rows = []
    for row in root.iter(f"{NS}row"):
        cells = {}
        for c in row.findall(f"{NS}c"):
            idx = _col_index(c.get("r"))
            t = c.get("t")
            v = c.find(f"{NS}v")
            if v is None or v.text is None:
                val = ""
            elif t == "s":
                val = shared[int(v.text)]
            else:
                val = v.text
            cells[idx] = val
        if cells:
            rows.append([cells.get(i, "") for i in range(max(cells) + 1)])
    return rows


# ================= UUIDv1 解码 =================
def decode_uuid_v1(uid: str) -> dt.datetime | None:
    """crb-/cmb-<32hex> → 内嵌时刻（UTC naive）。非 v1 或解析失败返回 None。"""
    if "-" not in uid:
        return None
    hx = uid.split("-", 1)[1].replace("-", "")
    if len(hx) != 32:
        return None
    try:
        time_low = int(hx[0:8], 16)
        time_mid = int(hx[8:12], 16)
        ver_hi = int(hx[12:16], 16)
    except ValueError:
        return None
    if (ver_hi >> 12) & 0xF != 1:
        return None
    ts = ((ver_hi & 0x0FFF) << 48) | (time_mid << 32) | time_low
    if ts < GREGORIAN_OFFSET:
        return None
    return dt.datetime(1970, 1, 1) + dt.timedelta(seconds=(ts - GREGORIAN_OFFSET) / 1e7)


# ================= 数据加载 =================
def load_billing(path: str) -> list[dict]:
    rows = read_sheet(path)
    header = rows[0]
    out = []
    for r in rows[1:]:
        r = r + [""] * (len(header) - len(r))
        if not any(x.strip() for x in r):
            continue
        d = dict(zip(header, r))
        d["credits"] = float(d.get("积分消耗") or 0)
        d["model"] = d.get("模型", "")
        d["ts"] = dt.datetime.strptime(d["时间"], "%Y-%m-%d %H:%M:%S")
        dec = decode_uuid_v1(d.get("RequestID", ""))
        d["decoded"] = dec + dt.timedelta(hours=TZ_OFFSET_HOURS) if dec else None
        out.append(d)
    return out


def load_local(db_path: str) -> tuple[list[dict], dict]:
    c = sqlite3.connect("file:" + db_path.replace("\\", "/") + "?mode=ro", uri=True)
    cols = ["id", "created_at", "route", "model", "status", "input_tokens", "output_tokens",
            "cache_read_tokens", "cache_write_tokens", "credits_consumed", "credit_source",
            "error_code"]
    recs = []
    for row in c.execute(f"select {','.join(cols)} from request_logs"):
        d = dict(zip(cols, row))
        d["ts"] = dt.datetime.fromtimestamp(d["created_at"])
        recs.append(d)
    agg = {}
    try:
        raw = c.execute("select data from metrics where id=1").fetchone()
        if raw:
            agg = json.loads(raw[0])
    except sqlite3.Error:
        pass
    c.close()
    return recs, agg


# ================= 特征与拟合 =================
def features(rec: dict) -> np.ndarray:
    """[未缓存输入, 缓存读取, 输出] 单位: 1K token。

    注意：实测 input_tokens == cache_read_tokens + cache_write_tokens 恒成立，
    因此未缓存输入 = input - cache_read == cache_write_tokens。
    """
    return np.array([(rec["input_tokens"] - rec["cache_read_tokens"]) / 1000.0,
                     rec["cache_read_tokens"] / 1000.0,
                     rec["output_tokens"] / 1000.0])


def lp_fit(X: np.ndarray, y: np.ndarray, cap: float = CAP):
    """min Σ s_i  s.t. |y - X p| <= cap + s_i, p>=0, s>=0。

    返回 (p, 总越界量) 或 (None, None)。
    """
    n, k = X.shape
    if n < k:
        return None, None
    c = np.concatenate([np.zeros(k), np.ones(n)])
    A1 = np.hstack([-X, -np.eye(n)])
    b1 = cap - y
    A2 = np.hstack([X, -np.eye(n)])
    b2 = cap + y
    res = linprog(c, A_ub=np.vstack([A1, A2]), b_ub=np.concatenate([b1, b2]),
                  bounds=[(0, None)] * k + [(0, None)] * n, method="highs")
    if res.status != 0:
        return None, None
    return res.x[:k], float(res.x[k:].sum())


def param_intervals(X: np.ndarray, y: np.ndarray, slack_allow: float, cap: float = CAP):
    """在"总越界量 <= 最优 + slack_allow"的可行域内，求每个参数的 min/max。"""
    n, k = X.shape
    p0, v0 = lp_fit(X, y, cap)
    if p0 is None:
        return None, None
    A1 = np.hstack([-X, -np.eye(n)]); b1 = cap - y
    A2 = np.hstack([X, -np.eye(n)]); b2 = cap + y
    row = np.concatenate([np.zeros(k), np.ones(n)]).reshape(1, -1)
    A_ub = np.vstack([A1, A2, row])
    b_ub = np.concatenate([b1, b2, [v0 + slack_allow]])
    bounds = [(0, None)] * k + [(0, None)] * n
    iv = []
    for j in range(k):
        lo = hi = float("nan")
        for sense, assign in ((1, "lo"), (-1, "hi")):
            cc = np.zeros(k + n); cc[j] = sense
            r = linprog(cc, A_ub=A_ub, b_ub=b_ub, bounds=bounds, method="highs")
            if r.status == 0:
                if assign == "lo":
                    lo = float(r.x[j])
                else:
                    hi = float(r.x[j])
        iv.append((lo, hi))
    return p0, iv


# ================= 配对 =================
def initial_pair(billing: list[dict], local: list[dict], tol: float) -> dict[int, int]:
    """贪心最近邻 1:1 配对（同模型，|Δt| <= tol）。返回 {账单下标: 本地下标}。"""
    cands = []
    for i, x in enumerate(billing):
        if not x["decoded"]:
            continue
        for j, y in enumerate(local):
            if y["model"] != x["model"]:
                continue
            delta = (x["decoded"] - y["ts"]).total_seconds()
            if abs(delta) <= tol:
                cands.append((abs(delta), i, j))
    cands.sort()
    used_b, used_l, pair = set(), set(), {}
    for _, i, j in cands:
        if i in used_b or j in used_l:
            continue
        used_b.add(i); used_l.add(j); pair[i] = j
    return pair


def repair_swaps(billing, local, pair: dict[int, int], tol: float, iters: int = 8):
    """用 LP 残差迭代修复"相邻请求积分互换"的配对错误。"""
    idx = sorted(pair)
    X = np.array([features(local[pair[i]]) for i in idx])
    y = np.array([billing[i]["credits"] for i in idx])
    p, viol = lp_fit(X, y)
    if p is None:
        return pair, None, None, 0
    swaps = 0
    for _ in range(iters):
        resid = y - X @ p
        bad = np.where(np.abs(resid) > CAP + 1e-9)[0]
        if len(bad) == 0:
            break
        changed = 0
        for k in bad:
            i = idx[k]
            x = billing[i]
            cand = [j for j, yrec in enumerate(local)
                    if yrec["model"] == x["model"]
                    and abs((x["decoded"] - yrec["ts"]).total_seconds()) <= tol + 2.0]
            cur = pair.get(i)
            best_j, best_err = cur, abs(resid[k])
            for j in cand:
                e = abs(x["credits"] - float(features(local[j]) @ p))
                if e < best_err - 1e-12:
                    best_j, best_err = j, e
            if best_j != cur:
                holder = [ii for ii, jj in pair.items() if jj == best_j]
                pair[i] = best_j
                if holder:
                    pair[holder[0]] = cur
                    swaps += 1
                changed += 1
        idx = sorted(pair)
        X = np.array([features(local[pair[i]]) for i in idx])
        y = np.array([billing[i]["credits"] for i in idx])
        p2, viol2 = lp_fit(X, y)
        if p2 is None or viol2 >= viol - 1e-12:
            break
        p, viol = p2, viol2
    return pair, p, viol, swaps


def information_test(X, y, models, seed_n: int = 30) -> tuple[float, float]:
    """信息量检验：真实 vs 同模型内打乱积分后的总越界量。"""
    _, real = lp_fit(X, y)
    if real is None:
        return float("nan"), float("nan")
    by_model: dict[str, list[int]] = defaultdict(list)
    for k, m in enumerate(models):
        by_model[m].append(k)
    sims = []
    for s in range(seed_n):
        rng = np.random.default_rng(s)   # 固定种子，结果可复现
        yy = y.copy()
        for _, ks in by_model.items():
            vals = yy[ks].copy()
            rng.shuffle(vals)
            yy[ks] = vals
        _, v = lp_fit(X, yy)
        if v is not None:
            sims.append(v)
    return real, (float(np.mean(sims)) if sims else float("nan"))


# ================= 主流程 =================
def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--billing", required=True, help="官网导出的 xlsx")
    ap.add_argument("--db", required=True, help="metrics.db 路径")
    ap.add_argument("--tol", type=float, default=3.0, help="配对时间容差（秒）")
    ap.add_argument("--json", default="rate_model.json", help="输出 JSON 路径")
    ap.add_argument("--slack", type=float, default=0.01,
                    help="区间估计允许的额外越界量")
    args = ap.parse_args()

    billing = load_billing(args.billing)
    local_all, agg = load_local(args.db)
    if not billing:
        sys.exit("账单为空")
    b0 = min(x["ts"] for x in billing)
    b1 = max(x["ts"] for x in billing)

    win_all = [x for x in local_all if b0 <= x["ts"] <= b1 + dt.timedelta(minutes=1)]
    zero = [x for x in win_all if x["input_tokens"] == 0 and x["output_tokens"] == 0]
    local = [x for x in win_all if x not in zero]

    print("=" * 72)
    print("数据概览")
    print("=" * 72)
    print(f"  账单: {len(billing)} 行, {len(set(x['model'] for x in billing))} 个模型, "
          f"{b0} -> {b1}")
    print(f"  本地: 窗口内 {len(win_all)} 行, 其中 0-token {len(zero)} 行(已排除), 有效 {len(local)} 行")
    print(f"  有效行数 == 账单行数 ? {len(local) == len(billing)}   "
          f"★ 这是最强的管道验证")
    if zero:
        print(f"  被排除行的 status/error: "
              f"{sorted(set((x['status'], x['error_code']) for x in zero))}")

    # token 口径验证
    nz = [x for x in local_all if x["input_tokens"] > 0]
    ident = sum(1 for x in nz
                if x["input_tokens"] == x["cache_read_tokens"] + x["cache_write_tokens"])
    print(f"  input == cache_read + cache_write : {ident}/{len(nz)} 行")
    if ident == len(nz) and nz:
        print("  → 上游为 OpenAI 语义；本地 cache_write 列 == 未缓存输入")

    print(f"  账单积分是否全为 0.01 倍数: "
          f"{all(abs(x['credits']*100 - round(x['credits']*100)) < 1e-9 for x in billing)}")
    print(f"  账单 0.00 行数: {sum(1 for x in billing if x['credits'] == 0)}/{len(billing)}")

    # ID 命名空间
    bpre = sorted(set(x["RequestID"].split("-")[0] for x in billing))
    lpre = sorted(set(x["id"].split("-")[0].split("_")[0] for x in local))
    bset = {x["RequestID"] for x in billing}
    lset = {x["id"] for x in local}
    print(f"  账单 ID 前缀 {bpre} ; 本地 ID 前缀 {lpre} ; 字符串交集 {len(bset & lset)}")
    print(f"  账单 UUIDv1 可解码: {sum(1 for x in billing if x['decoded'])}/{len(billing)}"
          f"  ← 配对只依赖这一侧的内嵌时刻")
    dec_local = sum(1 for x in local if decode_uuid_v1(x["id"]))
    print(f"  本地 UUIDv1 可解码: {dec_local}/{len(local)}"
          f"  ← 本地 created_at 本身已精确到秒，无需解码（0 表示本批次用的是兜底 req_ ID）")

    # ---- 配对 ----
    pair = initial_pair(billing, local, args.tol)
    print("\n" + "=" * 72)
    print("配对")
    print("=" * 72)
    print(f"  初始配对: {len(pair)}/{len(billing)}")
    pair, p_all, viol_all, swaps = repair_swaps(billing, local, dict(pair), args.tol)
    print(f"  交换修复: 触发 {swaps} 次 → 总越界 {viol_all:.4f}")

    idx = sorted(pair)
    X = np.array([features(local[pair[i]]) for i in idx])
    y = np.array([billing[i]["credits"] for i in idx])
    models = [billing[i]["model"] for i in idx]

    real, shuffled = information_test(X, y, models)
    print(f"\n  ★ 信息量检验（配对是否携带真实信息）")
    print(f"    真实配对总越界     = {real:.4f}")
    print(f"    同模型打乱积分后   = {shuffled:.4f}")
    if shuffled > 0 and real < shuffled * 0.5:
        print(f"    → 真实显著更优（{shuffled/max(real,1e-9):.1f}x），配对有效")
    else:
        print(f"    → 优势不明显，配对存疑，结果不可信")

    # ---- 分模型拟合 ----
    report = {
        "generated_at": dt.datetime.now().isoformat(timespec="seconds"),
        "method": "uuid-time join + swap repair + LP rounding-envelope fit",
        "inputs": {"billing": args.billing, "db": args.db},
        "pairing": {"matched": len(pair), "billing_rows": len(billing),
                    "swaps_repaired": swaps, "total_violation": viol_all,
                    "info_test_real": real, "info_test_shuffled": shuffled},
        "models": {},
    }

    print("\n" + "=" * 72)
    print("分模型拟合")
    print("=" * 72)
    for model in sorted(set(x["model"] for x in billing)):
        sel = [k for k, m in enumerate(models) if m == model]
        if len(sel) < 4:
            print(f"\n[{model}] 配对仅 {len(sel)}，样本不足以识别 3 参数")
            report["models"][model] = {"n_pairs": len(sel), "status": "insufficient_samples"}
            continue
        Xm, ym = X[sel], y[sel]
        print(f"\n[{model}]  n={len(sel)}")
        print(f"  列范围: 未缓存[{Xm[:,0].min():.3f},{Xm[:,0].max():.1f}]K  "
              f"缓存读[{Xm[:,1].min():.3f},{Xm[:,1].max():.1f}]K  "
              f"输出[{Xm[:,2].min():.3f},{Xm[:,2].max():.1f}]K")

        # 共线性
        stds = Xm.std(axis=0)
        degenerate = [i for i, s in enumerate(stds) if s < 1e-9]
        if len(Xm) > 3 and not degenerate:
            cr = np.corrcoef(Xm.T)
            print(f"  相关: un~cr={cr[0,1]:+.3f} un~out={cr[0,2]:+.3f} cr~out={cr[1,2]:+.3f}")
        if degenerate:
            print(f"  ⚠ 零方差列: {degenerate} → 对应单价不可识别")

        entry: dict = {"n_pairs": len(sel), "status": "ok"}
        p3, v3 = lp_fit(Xm, ym)
        if p3 is not None:
            resid = ym - Xm @ p3
            names = ["input_per_1k", "cache_read_per_1k", "output_per_1k"]
            print(f"  3 参数: " + "  ".join(f"{n}={v:.6f}" for n, v in zip(names, p3)))
            print(f"          越界={v3:.4f} MAE={np.abs(resid).mean():.5f} "
                  f"max={np.abs(resid).max():.5f}")
            entry["rates"] = {n: float(v) for n, v in zip(names, p3)}
            entry["residual"] = {"mae": float(np.abs(resid).mean()),
                                 "max": float(np.abs(resid).max()),
                                 "total_violation": v3}
            p0, iv = param_intervals(Xm, ym, args.slack)
            if iv:
                entry["intervals"] = {}
                print(f"  参数区间（允许额外越界 {args.slack}）:")
                for j, (lo, hi) in enumerate(iv):
                    rel = 100 * (hi - lo) / max(p3[j], 1e-12)
                    flag = "  ← 宽（不可识别）" if rel > 100 else ""
                    print(f"    {names[j]:18s} ∈ [{lo:.6f}, {hi:.6f}] 相对宽={rel:.1f}%{flag}")
                    entry["intervals"][names[j]] = {"lo": lo, "hi": hi, "rel_width_pct": rel}

            # 倍率（便于跨模型比较）
            if p3[0] > 0:
                print(f"  倍率: out/in={p3[2]/p3[0]:.2f}  cache_read/in={p3[1]/p3[0]:.4f}")
                entry["ratios"] = {"output_over_input": float(p3[2] / p3[0]),
                                   "cache_read_over_input": float(p3[1] / p3[0])}

        # 简化模型对比
        print("  模型选择:")
        for tag, cols in (("un+cr+out", [0, 1, 2]), ("un+out", [0, 2]), ("un 仅输入", [0])):
            if any(c in degenerate for c in cols):
                continue
            pp, vv = lp_fit(Xm[:, cols], ym)
            if pp is None:
                continue
            print(f"    [{tag:10s}] " + " ".join(f"{v:.6f}" for v in pp) + f"   越界={vv:.4f}")

        # LOO 交叉验证（防守"用残差挑配对"的过拟合）
        errs = []
        for hold in range(len(sel)):
            m = [t for t in range(len(sel)) if t != hold]
            pp, _ = lp_fit(Xm[m], ym[m])
            if pp is None:
                continue
            errs.append(abs(ym[hold] - float(Xm[hold] @ pp)))
        if errs:
            e = np.array(errs)
            print(f"  LOO: 中位={np.median(e):.5f} p90={np.percentile(e,90):.5f} "
                  f"max={e.max():.5f} 超±0.005={100*(e>CAP+1e-9).mean():.1f}%")
            entry["loo"] = {"median": float(np.median(e)),
                            "p90": float(np.percentile(e, 90)),
                            "frac_over_cap_pct": float(100 * (e > CAP + 1e-9).mean())}
        report["models"][model] = entry

    # ---- p_cw 假设 ----
    print("\n" + "=" * 72)
    print("关于 p_cw（缓存写入单价）—— 数据能说什么，不能说什么")
    print("=" * 72)
    print("  实测恒有 input == cache_read + cache_write，即本地 cache_write 列")
    print("  记录的是**未命中缓存的输入**，不是 Anthropic 语义的 cache_creation。")
    print("  账单侧也没有 cache_write 项。")
    print("  => 本数据无法把 p_cw 与 p_in 分离（二者对观测完全等价）。")
    print("  => 采用「缓存写入 = 输入同价」是先验假设，不是拟合结论。")
    print("     若该假设成立，计费式即 p_in·input_total + p_cr·cache_read + p_out·out。")

    with open(args.json, "w", encoding="utf-8") as f:
        json.dump(report, f, ensure_ascii=False, indent=2)
    print(f"\n已写出 {args.json}")


if __name__ == "__main__":
    main()
