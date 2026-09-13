"""verify_precision.py — 蒙特卡洛验证 docs/credit-rate-inference.md 的精度论断。

关键建模决定（第一版曾搞错，此处已修正）：
  真实积分是**确定性**的 c_i = p . x_i，观测值 y_i = round(c_i, 2)。
  误差 e_i = y_i - c_i 完全由 c_i 的小数部分决定，**不是独立噪声**。
  因此：重复同样 token 规模的请求 = 零新增信息；只有 x 的**分散**才带来信息。
  误差在 c_i 小数部分等分布时表现为 std = sigma = 0.002887 的准随机序列。

待验证命题：
  P1. 舍入误差的等效 std = 0.002887（c 的小数部分等分布时）。
  P2. 单参数识别区间（无分布假设，严格）宽度 = 0.01 / x。
  P3. 恒定 token 规模：重复请求不缩小识别区间（零信息）。
  P4. 分散 token 规模：识别区间近似按 1/n 坍缩（丢番图裁剪）。
  P5. 共线性按 1/sqrt(1-R^2) 放大 se。
  P6. 4 参数（含 p_cw = p_in + d 重参数化）在自然共线负载 vs 探针梯度下的可辨识性。
"""

import math
import random
import statistics

SIGMA = 0.01 / math.sqrt(12)  # 0.0028868


# ---------------- 线性代数（纯 Python） ----------------
def transpose(A):
    return [list(r) for r in zip(*A)]


def matmul(A, B):
    m, k, n = len(A), len(B), len(B[0])
    return [[sum(A[i][t] * B[t][j] for t in range(k)) for j in range(n)] for i in range(m)]


def inverse(A):
    n = len(A)
    M = [row[:] + [1.0 if i == j else 0.0 for j in range(n)] for i, row in enumerate(A)]
    for c in range(n):
        piv = max(range(c, n), key=lambda r: abs(M[r][c]))
        if abs(M[piv][c]) < 1e-300:
            raise ValueError("singular")
        M[c], M[piv] = M[piv], M[c]
        pv = M[c][c]
        M[c] = [v / pv for v in M[c]]
        for r in range(n):
            if r != c and M[r][c] != 0.0:
                f = M[r][c]
                M[r] = [a - f * b for a, b in zip(M[r], M[c])]
    return [row[n:] for row in M]


def ols(X, y, ridge=0.0, sigma=SIGMA):
    """WLS/OLS + ridge。cov 用 sigma^2 (X'X + ridge I)^-1 近似（误差按准随机处理）。"""
    XtX = matmul(transpose(X), X)
    for i in range(len(XtX)):
        XtX[i][i] += ridge
    Xty = [sum(X[k][i] * y[k] for k in range(len(y))) for i in range(len(X[0]))]
    inv = inverse(XtX)
    beta = [sum(inv[i][j] * Xty[j] for j in range(len(Xty))) for i in range(len(inv))]
    cov = [[sigma**2 * inv[i][j] for j in range(len(inv))] for i in range(len(inv))]
    return beta, cov


def vif(X, col):
    others = [row[:col] + row[col + 1:] for row in X]
    y = [row[col] for row in X]
    try:
        beta, _ = ols(others, y, sigma=1.0)
    except ValueError:
        return float("inf")
    ybar = sum(y) / len(y)
    ss_tot = sum((v - ybar) ** 2 for v in y)
    if ss_tot == 0:
        return float("inf")
    ss_res = sum((y[i] - sum(others[i][j] * beta[j] for j in range(len(beta)))) ** 2 for i in range(len(y)))
    r2 = 1 - ss_res / ss_tot
    return 1 / max(1 - r2, 1e-12)


def scaled_condition(X):
    """列归一化后的条件数（消除量纲影响）。"""
    cols = transpose(X)
    Xs = [[v / math.sqrt(sum(c * c for c in col)) for v in col] for col in cols]
    Xs = transpose(Xs)
    XtX = matmul(transpose(Xs), Xs)
    n = len(XtX)
    inv = inverse(XtX)

    def power(M, iters=600):
        v = [1.0] * n
        lam = 0.0
        for _ in range(iters):
            w = [sum(M[i][j] * v[j] for j in range(n)) for i in range(n)]
            norm = math.sqrt(sum(t * t for t in w))
            if norm == 0:
                return 0.0
            v = [t / norm for t in w]
            lam = norm
        return lam

    lmax = power(XtX)
    lmin = 1 / power(inv)
    return math.sqrt(lmax / lmin) if lmin > 0 else float("inf")


# ---------------- P1 ----------------
def check_p1():
    """c 的小数部分等分布时，舍入误差 e = round(c,2) - c 的 std。"""
    errs = []
    c = 0.0
    for i in range(200000):
        c += 0.000137  # 无理步长 -> 小数部分等分布
        y = round(c, 2)
        errs.append(y - c)
    print(f"P1 舍入误差等效 std: 实测={statistics.pstdev(errs):.6f}  理论 sigma={SIGMA:.6f}")


# ---------------- P2 / P3 / P4：单参数严格识别区间 ----------------
def identified_interval(xs, p_true):
    """由 y_i = round(p*x_i, 2)、|y_i - p*x_i| <= 0.005 反解 p 的严格识别区间。"""
    lo, hi = 0.0, float("inf")
    for x in xs:
        y = round(p_true * x, 2)
        lo = max(lo, (y - 0.005) / x)
        hi = min(hi, (y + 0.005) / x)
    return lo, hi


def check_p2():
    p = 0.1
    print("P2 单条请求的严格识别区间宽度（p_in = 0.1 积分/1K）")
    for x, label in ((1.0, "1K"), (8.0, "8K"), (32.0, "32K"), (100.0, "100K"), (200.0, "200K")):
        lo, hi = identified_interval([x], p)
        w = hi - lo
        print(f"   {label:>5s} 输入: 宽度={w:.6f}  相对={w/p:7.3%}   理论 0.01/x={0.01/x:.6f}")


def check_p3_p4():
    p = 0.1234  # 非网格值，避免人为整除
    print("\nP3/P4 识别区间相对宽度 vs 请求数（中位数，40 次重复）")
    designs = (
        ("恒定 x=5 (重复无效)", lambda r, n: [5.0] * n),
        ("恒定 x=100 (重复无效)", lambda r, n: [100.0] * n),
        ("分散 x~U(1,10)", lambda r, n: [r.uniform(1, 10) for _ in range(n)]),
        ("分散 x~U(10,100)", lambda r, n: [r.uniform(10, 100) for _ in range(n)]),
        ("几何梯度 1,4,16,64,...", lambda r, n: [4.0**k for k in range(n)]),
    )
    for label, gen in designs:
        cells = []
        for n in (1, 2, 5, 10, 50):
            rels = []
            for seed in range(40):
                r = random.Random(1000 + seed)
                lo, hi = identified_interval(gen(r, n), p)
                rels.append(max(hi - lo, 0) / p)
            cells.append(f"n={n:<3d}:{statistics.median(rels):8.3%}")
        print(f"   {label:26s} " + " ".join(cells))


# ---------------- P5 ----------------
def check_p5():
    print("\nP5 共线性对 se(p_in) 的放大（对照 rho=0 基线）")
    rnd = random.Random(7)
    base = None
    for rho in (0.0, 0.9, 0.99, 0.999):
        X, y = [], []
        for _ in range(300):
            u = rnd.uniform(1, 50)
            w = rho * u + (1 - rho) * rnd.uniform(1, 50)
            X.append([u, w])
            y.append(round(0.1 * u + 0.1 * w, 2))
        _, cov = ols(X, y)
        se = math.sqrt(cov[0][0])
        if base is None:
            base = se
        print(f"   rho={rho:.3f}  VIF={vif(X,0):9.1f}  实测 se={se:.6f}  "
              f"实测放大={se/base:8.2f}x  理论 VIF^0.5={math.sqrt(vif(X,0)):7.2f}x")


# ---------------- P6 ----------------
def check_p6():
    p = [0.1, 0.5, 0.01, 0.1]  # p_in, p_out, p_cr, p_cw ; true d = p_cw - p_in = 0
    print("\nP6 4 参数可辨识性（重参数化：p_in 由 (u+w) 识别，d = p_cw - p_in）")
    for name in ("自然共线负载", "探针梯度设计"):
        rnd = random.Random(42)
        X, y = [], []
        for _ in range(300):
            if name == "自然共线负载":
                u = rnd.uniform(0.5, 40)
                w = u * rnd.uniform(0.9, 1.0)
                r = u * rnd.uniform(0.0, 0.3)
                o = rnd.uniform(0.1, 3)
            else:
                u = rnd.choice([1, 4, 8, 16, 32, 64, 100]) * rnd.uniform(0.9, 1.1)
                w = u * rnd.choice([0.0, 0.05, 0.3, 1.0])
                r = u * rnd.choice([0.0, 0.2, 0.6, 0.95])
                o = rnd.choice([0.1, 0.5, 2, 8]) * rnd.uniform(0.9, 1.1)
            c = p[0] * u + p[3] * w + p[2] * r + p[1] * o
            X.append([u + w, r, o, w])
            y.append(round(c, 2))
        # 无正则（暴露病态）
        beta, cov = ols(X, y)
        beta_r, cov_r = ols(X, y, ridge=1e-6)
        names = ["p_in", "p_cr", "p_out", "d = p_cw - p_in"]
        true = [p[0], p[2], p[1], p[3] - p[0]]
        print(f"\n  [{name}]  n=300  列归一化条件数={scaled_condition(X):.1f}")
        for i, nm in enumerate(names):
            se = math.sqrt(cov[i][i])
            se_r = math.sqrt(cov_r[i][i])
            # 用 p_in 作参照：d 的绝对值本身为 0，不能用相对误差
            ref = se / p[0]
            verdict = "不可识别" if ref > 0.5 else ("偏弱" if ref > 0.1 else "可识别")
            print(f"    {nm:18s} true={true[i]:+.4f} est={beta[i]:+.4f}  "
                  f"se={se:.5f}  相对 p_in={ref:7.2%}  ridge后se={se_r:.5f}  [{verdict}]")


if __name__ == "__main__":
    check_p1()
    check_p2()
    check_p3_p4()
    check_p5()
    check_p6()
