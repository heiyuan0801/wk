"""calibrate_live.py — 发送阶段在线标定费率（推荐方案的可运行实现）。

与阶段一（离线配对账单）的对比：
    阶段一：账单 xlsx(60行/批) + UUIDv1 配对 + 交换修复 → p_in=0.014236 p_cr=0.000285 p_out=0.057009
    本脚本：发送时直接读 usage.credit，**无需配对**，样本量只受你愿意花的积分限制。

设计要点（全部来自前序实测）：
  1. credit 与账单同为 0.01 精度 → 用 LP 舍入包络拟合（|y - Xp| <= 0.005 硬约束）。
  2. 舍入误差是**确定性**的 → 靠 token 规模**梯度**取信息，不做重复请求。
  3. 共线性靠**主动设计** cache 状态来消除（冷启动 vs 复用）。
  4. credit=0 的样本仍是有效约束（0 <= Xp <= 0.005），不要丢弃。

用法：
  python tools/explore/calibrate_live.py --model deepseek-v4.1-flash --budget 1.0
"""

import argparse
import json
import time
import urllib.request
from dataclasses import dataclass, field

import numpy as np
from scipy.optimize import linprog

BASE = "http://127.0.0.1:7863"
KEY = "123"
CAP = 0.005


def call(model, messages, max_tokens, effort=None):
    payload = {"model": model, "messages": messages, "stream": False,
               "max_tokens": max_tokens}
    if effort:
        payload["reasoning_effort"] = effort
    req = urllib.request.Request(
        BASE + "/v1/chat/completions", data=json.dumps(payload).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="POST")
    with urllib.request.urlopen(req, timeout=300) as r:
        resp = json.loads(r.read().decode())
    u = resp.get("usage", {}) or {}
    return {
        "prompt": u.get("prompt_tokens", 0),
        "hit": u.get("prompt_cache_hit_tokens", 0),
        "miss": u.get("prompt_cache_miss_tokens", 0),
        "cw": u.get("prompt_cache_write_tokens", 0),
        "out": u.get("completion_tokens", 0),
        "think": u.get("completion_thinking_tokens", 0),
        "credit": float(u.get("credit", 0) or 0),
    }


@dataclass
class Sample:
    label: str
    miss: int
    hit: int
    out: int
    credit: float
    think: int = 0


def lp_fit(X, y, cap=CAP):
    n, k = X.shape
    c = np.concatenate([np.zeros(k), np.ones(n)])
    A1 = np.hstack([-X, -np.eye(n)]); b1 = cap - y
    A2 = np.hstack([X, -np.eye(n)]); b2 = cap + y
    r = linprog(c, A_ub=np.vstack([A1, A2]), b_ub=np.concatenate([b1, b2]),
                bounds=[(0, None)] * k + [(0, None)] * n, method="highs")
    if r.status != 0:
        return None, None
    return r.x[:k], float(r.x[k:].sum())


def intervals(X, y, slack=0.01, cap=CAP):
    n, k = X.shape
    p0, v0 = lp_fit(X, y, cap)
    if p0 is None:
        return None, None
    A1 = np.hstack([-X, -np.eye(n)]); b1 = cap - y
    A2 = np.hstack([X, -np.eye(n)]); b2 = cap + y
    row = np.concatenate([np.zeros(k), np.ones(n)]).reshape(1, -1)
    A_ub = np.vstack([A1, A2, row])
    b_ub = np.concatenate([b1, b2, [v0 + slack]])
    bd = [(0, None)] * k + [(0, None)] * n
    out = []
    for j in range(k):
        lo = hi = float("nan")
        for sense, which in ((1, "lo"), (-1, "hi")):
            cc = np.zeros(k + n); cc[j] = sense
            r = linprog(cc, A_ub=A_ub, b_ub=b_ub, bounds=bd, method="highs")
            if r.status == 0:
                if which == "lo":
                    lo = float(r.x[j])
                else:
                    hi = float(r.x[j])
        out.append((lo, hi))
    return p0, out


def build_probe_plan(model, budget):
    """探针计划：token 规模按几何级数铺开 + 制造 cache 状态变化。

    - 长输入定 p_in（单条 32K 输入的识别区间仅 0.01/32 = 0.0003）
    - 冷/热配对定 p_cr
    - 输出长度梯度定 p_out
    重复同规模无益（舍入误差确定性），故每档只发一次。
    """
    SYS = {"role": "system", "content": "You are a terse assistant. Answer briefly."}
    def filler(n_chars):
        return ("lorem ipsum dolor sit amet consectetur adipiscing elit " * (n_chars // 56 + 1))[:n_chars]

    plan = []
    # 输入梯度（几何级数），冷缓存 —— 定 p_in
    for chars, tag in [(4000, "2K"), (16000, "8K"), (64000, "32K"), (120000, "60K")]:
        plan.append((f"in-{tag}-cold", [SYS, {"role": "user", "content": filler(chars) + "\nReply OK."}], 5))
    # 复用最长那条（热缓存）—— 定 p_cr
    plan.append(("in-60K-hot", [SYS, {"role": "user",
                 "content": filler(120000) + "\nReply OK."}], 5))
    plan.append(("in-60K-hot2", [SYS, {"role": "user",
                 "content": filler(120000) + "\nReply OK."}], 5))
    # 输出梯度（固定短输入）—— 定 p_out
    plan.append(("out-short", [SYS, {"role": "user", "content": "Reply with just: OK"}], 5))
    plan.append(("out-mid", [SYS, {"role": "user",
                 "content": "Count from 1 to 120, space separated, no other text."}], 400))
    plan.append(("out-long", [SYS, {"role": "user",
                 "content": "Count from 1 to 600, space separated, no other text."}], 2000))
    return plan


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="deepseek-v4.1-flash")
    ap.add_argument("--budget", type=float, default=1.0, help="积分预算上限")
    ap.add_argument("--json", default="")
    args = ap.parse_args()

    print(f"=== 在线标定 {args.model}（预算 {args.budget} 积分）===")
    samples = []
    spent = 0.0
    plan = build_probe_plan(args.model, args.budget)

    print(f"\n{'探针':16s} {'miss':>7s} {'hit':>7s} {'out':>6s} {'think':>6s} {'credit':>8s} {'累计':>8s}")
    print("-" * 70)
    for label, msgs, mt in plan:
        if spent >= args.budget:
            print(f"{label:16s} 跳过（预算用尽）")
            continue
        try:
            r = call(args.model, msgs, mt)
        except Exception as e:
            print(f"{label:16s} 失败: {str(e)[:40]}")
            continue
        spent += r["credit"]
        samples.append(Sample(label, r["miss"], r["hit"], r["out"], r["credit"], r["think"]))
        print(f"{label:16s} {r['miss']:7d} {r['hit']:7d} {r['out']:6d} {r['think']:6d} "
              f"{r['credit']:8.2f} {spent:8.2f}")
        time.sleep(0.3)

    print("-" * 70)
    print(f"共 {len(samples)} 个样本，花费 {spent:.2f} 积分")

    # ---- 拟合 ----
    print("\n" + "=" * 70)
    print("拟合（LP 舍入包络，|credit - Xp| <= 0.005）")
    print("=" * 70)
    X = np.array([[s.miss / 1000.0, s.hit / 1000.0, s.out / 1000.0] for s in samples])
    y = np.array([s.credit for s in samples])
    names = ["p_in(未缓存)", "p_cr(缓存读)", "p_out(输出)"]

    if len(samples) < 4:
        print("样本不足，无法拟合")
        return

    # 共线性
    stds = X.std(axis=0)
    print("列统计:")
    for i, nm in enumerate(names):
        print(f"  {nm:14s} 范围[{X[:,i].min():8.3f},{X[:,i].max():8.3f}]  std={stds[i]:7.3f}"
              f"{'  ⚠ 零方差' if stds[i] < 1e-9 else ''}")
    if all(s > 1e-9 for s in stds):
        cr = np.corrcoef(X.T)
        print(f"相关: miss~hit={cr[0,1]:+.3f} miss~out={cr[0,2]:+.3f} hit~out={cr[1,2]:+.3f}")

    p, viol = lp_fit(X, y)
    print(f"\n3 参数: " + "  ".join(f"{n}={v:.6f}" for n, v in zip(names, p)))
    print(f"总越界={viol:.5f}")
    resid = y - X @ p
    print(f"残差: MAE={np.abs(resid).mean():.5f} max={np.abs(resid).max():.5f}")
    print("\n逐样本:")
    for s in samples:
        i = samples.index(s)
        print(f"  {s.label:16s} y={s.credit:5.2f} pred={(X[i]@p):6.3f} res={resid[i]:+7.4f}")

    p0, iv = intervals(X, y)
    if iv:
        print("\n参数区间（允许额外越界 0.01）:")
        for nm, (lo, hi), pt in zip(names, iv, p):
            rel = 100 * (hi - lo) / max(pt, 1e-12)
            print(f"  {nm:14s} ∈ [{lo:.6f}, {hi:.6f}]  相对宽={rel:6.1f}%"
                  f"{'  ← 不可识别' if rel > 100 else ''}")

    print("\n与阶段一（离线账单反推）对比:")
    ref = {"p_in(未缓存)": 0.014236, "p_cr(缓存读)": 0.000285, "p_out(输出)": 0.057009}
    for nm, v in zip(names, p):
        r = ref[nm]
        d = 100 * (v - r) / r if r else float("nan")
        print(f"  {nm:14s} 在线={v:.6f}  离线={r:.6f}  偏差={d:+7.1f}%")

    if args.json:
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump({"model": args.model, "n": len(samples), "spent_credits": spent,
                       "rates": dict(zip(names, [float(v) for v in p])),
                       "total_violation": viol,
                       "residual": {"mae": float(np.abs(resid).mean()),
                                    "max": float(np.abs(resid).max())},
                       "intervals": {nm: {"lo": l, "hi": h} for nm, (l, h) in zip(names, iv)},
                       "samples": [vars(s) for s in samples]},
                      f, ensure_ascii=False, indent=2)
        print(f"\n已写出 {args.json}")


if __name__ == "__main__":
    main()
