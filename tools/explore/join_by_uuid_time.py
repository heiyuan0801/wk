"""join_by_uuid_time.py — 用 UUIDv1 内嵌时间戳做精确 JOIN。

已确认（uuid_decode.py）：
  - 账单 crb-<32hex> 与本地上游 cmb-<32hex> 都是 **UUIDv1**（版本位=1）；
  - 两者内嵌时间戳都≈请求实际时刻（cmb- 相对本地 created_at 偏差 0~3s）；
  - 账单时间列只到分钟（delta 到内嵌时刻差 0~58s，即被截断/取整）。

因此即便字符串 ID 无交集，也可用 **(模型, 内嵌时刻)** 做 JOIN。
本脚本对账单窗口内的 60 条本地记录与 60 条账单项做最优 1:1 匹配，评估匹配紧密度。
"""

import datetime
import sqlite3
import sys
from collections import Counter, defaultdict

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"
GREGORIAN_OFFSET = 0x01B21DD213814000


def decode_v1(uid):
    if "-" not in uid:
        return None
    hexpart = uid.split("-", 1)[1].replace("-", "")
    if len(hexpart) != 32:
        return None
    try:
        time_low = int(hexpart[0:8], 16)
        time_mid = int(hexpart[8:12], 16)
        ver_hi = int(hexpart[12:16], 16)
        version = (ver_hi >> 12) & 0xF
        time_hi = ver_hi & 0x0FFF
    except ValueError:
        return None
    if version != 1:
        return None
    ts = (time_hi << 48) | (time_mid << 32) | time_low
    if ts < GREGORIAN_OFFSET:
        return None
    secs = (ts - GREGORIAN_OFFSET) / 1e7
    return datetime.datetime(1970, 1, 1) + datetime.timedelta(seconds=secs)


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
        d["dec"] = decode_v1(d["RequestID"])  # 已是"本地时间"（因为我们要减8小时？）
        # decode_v1 返回的是 UTC 语义的 naive datetime；
        # 本地(UTC+8) = 解码值 + 8h
        d["dec_local"] = d["dec"] + datetime.timedelta(hours=8) if d["dec"] else None
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

    b0, b1 = min(x["ts"] for x in b), max(x["ts"] for x in b)
    win = [x for x in l if b0 <= x["ts"] <= b1 + datetime.timedelta(minutes=1)
           and not (x["input_tokens"] == 0 and x["output_tokens"] == 0)]
    print(f"账单项={len(b)}  本地窗口非空={len(win)}")

    # 账单内嵌时刻 vs 账单分钟 的偏移分布
    print("\n=== 账单内嵌时刻 - 账单分钟 ===")
    offs = [(x["dec_local"] - x["ts"]).total_seconds() for x in b if x["dec_local"]]
    offs.sort()
    print(f"  min={offs[0]:.1f}s p25={offs[len(offs)//4]:.1f}s "
          f"中位={offs[len(offs)//2]:.1f}s p75={offs[3*len(offs)//4]:.1f}s max={offs[-1]:.1f}s")
    print("  → 若全部落在 [0,60) 则说明账单时间 = 内嵌时刻向下取整到分钟")
    inrange = sum(1 for o in offs if 0 <= o < 60)
    print(f"  落在 [0,60) 的比例: {inrange}/{len(offs)}")

    # 用内嵌时刻与本地 created_at 做匹配
    print("\n=== 最近邻分析：每个账单项找同模型、|Δt| 最小的本地项 ===")
    nn = []
    for x in b:
        if not x["dec_local"]:
            continue
        best = None
        for y in win:
            if y["model"] != x["模型"]:
                continue
            dt = (x["dec_local"] - y["ts"]).total_seconds()
            if best is None or abs(dt) < abs(best[0]):
                best = (dt, y)
        if best:
            nn.append((best[0], x, best[1]))
    ds = sorted(d[0] for d in nn)
    print(f"  n={len(ds)}  Δt: min={ds[0]:+.2f}s p25={ds[len(ds)//4]:+.2f}s "
          f"中位={ds[len(ds)//2]:+.2f}s p75={ds[3*len(ds)//4]:+.2f}s max={ds[-1]:+.2f}s")
    print(f"  |Δt|<=1s: {sum(1 for d in ds if abs(d)<=1)}   "
          f"<=2s: {sum(1 for d in ds if abs(d)<=2)}   "
          f"<=5s: {sum(1 for d in ds if abs(d)<=5)}   "
          f"<=10s: {sum(1 for d in ds if abs(d)<=10)}")
    print("\n  最近邻明细（Δt 排序前 15）:")
    for dt, x, y in nn[:15]:
        print(f"    Δt={dt:+7.2f}s 账单 {x['RequestID'][-8:]} {x['ts'].strftime('%H:%M')} y={x['credits']:5.2f} "
              f"| 本地 {y['id'][-10:]} {y['ts'].strftime('%H:%M:%S')} "
              f"in={y['input_tokens']:6d} cr={y['cache_read_tokens']:7d} out={y['output_tokens']:6d}")

    # 严格阈值下的候选配对
    tol = 2.0
    cands = [(abs(dt), dt, x, y) for dt, x, y in nn if abs(dt) <= tol]
    print(f"\n  |Δt|<={tol}s 的候选: {len(cands)}")

    # 贪心 1:1 匹配（按 |Δt| 升序）
    used_b, used_l, pairs = set(), set(), []
    for a, dt, x, y in sorted(cands, key=lambda t: t[0]):
        bi, li = id(x), id(y)
        if bi in used_b or li in used_l:
            continue
        used_b.add(bi)
        used_l.add(li)
        pairs.append((dt, x, y))
    print(f"\n  贪心 1:1 匹配成功: {len(pairs)} / {len(b)}")
    pds = sorted(abs(p[0]) for p in pairs)
    if pds:
        print(f"  |Δt| 分布: min={pds[0]:.2f}s 中位={pds[len(pds)//2]:.2f}s max={pds[-1]:.2f}s")

    # 统计未匹配的
    ub = [x for x in b if id(x) not in used_b]
    ul = [y for y in win if id(y) not in used_l]
    print(f"  未匹配账单项: {len(ub)}  未匹配本地项: {len(ul)}")
    for x in ub[:8]:
        print(f"    账单 {x['ts'].strftime('%H:%M')} {x['模型']:20s} y={x['credits']:5.2f} "
              f"dec={x['dec_local']}")
    for y in ul[:8]:
        print(f"    本地 {y['ts'].strftime('%H:%M:%S')} {y['model']:20s} "
              f"in={y['input_tokens']:6d} cr={y['cache_read_tokens']:7d} out={y['output_tokens']:6d}")

    # 在匹配上的配对里做拟合
    if len(pairs) >= 4:
        print(f"\n=== 用 {len(pairs)} 对精确配对做 NNLS ===")
        A = [[(y["input_tokens"] - y["cache_read_tokens"]) / 1000.0,
              y["cache_read_tokens"] / 1000.0,
              y["output_tokens"] / 1000.0] for _, x, y in pairs]
        yv = [x["credits"] for _, x, y in pairs]
        nnls_report(A, yv, pairs)


def nnls_report(A, yv, pairs):
    n, p = len(A), 3
    AtA = [[sum(A[i][a] * A[i][b] for i in range(n)) for b in range(p)] for a in range(p)]
    Aty = [sum(A[i][a] * yv[i] for i in range(n)) for a in range(p)]
    Lc = max(sum(abs(v) for v in row) for row in AtA)
    step = 1.0 / max(Lc, 1e-12)
    x = [0.0] * p
    for _ in range(80000):
        g = [sum(AtA[a][b] * x[b] for b in range(p)) - Aty[a] for a in range(p)]
        for a in range(p):
            x[a] = max(0.0, x[a] - step * g[a])
    names = ["p_in(未缓存)", "p_cr(缓存读)", "p_out(输出)"]
    print("  " + "  ".join(f"{nm}={v:.6f}" for nm, v in zip(names, x)))
    pred = [sum(A[i][a] * x[a] for a in range(p)) for i in range(n)]
    res = [yv[i] - pred[i] for i in range(n)]
    print(f"  MAE={sum(abs(r) for r in res)/n:.4f} max={max(abs(r) for r in res):.4f} sum={sum(res):+.4f}")
    for i in range(n):
        dt, bx, ly = pairs[i]
        print(f"    Δt={dt:+6.2f}s y={yv[i]:6.2f} pred={pred[i]:6.2f} res={res[i]:+7.3f} | "
              f"un={A[i][0]*1000:7.0f} cr={A[i][1]*1000:8.0f} out={A[i][2]*1000:6.0f}")


if __name__ == "__main__":
    main()
