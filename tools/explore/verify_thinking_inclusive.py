"""verify_thinking_inclusive.py — 判定 completion_tokens 是否**已包含**思考 token。

这决定计费模型是 3 参数还是 4 参数：
  假设 I（inclusive）：completion_tokens 已含思考，思考按输出价计费 → 3 参数成立
  假设 E（exclusive）：completion_tokens 不含思考，思考另计 → 需第 4 个费率

用已知费率的模型（deepseek-v4.1-flash）做判别：
  p_in = 0.014236, p_cr = 0.000285, p_out = 0.057009  （阶段一离线拟合，已被在线标定复现）
"""

P_IN, P_CR, P_OUT = 0.014236, 0.000285, 0.057009


def r2(x):
    return round(x + 1e-12, 2)


# (label, miss, hit, comp, think, credit)
SAMPLES = [
    ("effort=none  ", 55, 0, 172,   0, 0.01),
    ("effort=high  ", 80, 0, 182,  73, 0.01),
    ("out-mid      ", 30, 0, 239,   0, 0.01),
    ("out-long     ", 30, 0, 1199,  0, 0.07),
]


def pred_i(miss, hit, comp):
    return (P_IN * miss + P_CR * hit + P_OUT * comp) / 1000.0


def pred_e(miss, hit, comp, think):
    return (P_IN * miss + P_CR * hit + P_OUT * (comp + think)) / 1000.0


print("=" * 92)
print("假设 I：completion_tokens 已含思考（思考按输出价，不另计）")
print("假设 E：completion_tokens 不含思考（思考需另加）")
print("=" * 92)
hdr = (f"{'样本':16s} {'miss':>5s} {'comp':>5s} {'think':>6s} "
       f"{'I预测':>9s} {'I→round':>8s} {'E预测':>9s} {'E→round':>8s} {'实测':>6s} {'胜':>4s}")
print(hdr)
print("-" * len(hdr))

score = {"I": 0, "E": 0}
for label, miss, hit, comp, think, credit in SAMPLES:
    pi, pe = pred_i(miss, hit, comp), pred_e(miss, hit, comp, think)
    ri, re = r2(pi), r2(pe)
    mi = abs(ri - credit) < 1e-9
    me = abs(re - credit) < 1e-9
    win = "I" if (mi and not me) else ("E" if (me and not mi) else ("=" if (mi and me) else "-"))
    if mi:
        score["I"] += 1
    if me:
        score["E"] += 1
    print(f"{label:16s} {miss:5d} {comp:5d} {think:6d} {pi:9.5f} {ri:8.2f} "
          f"{pe:9.5f} {re:8.2f} {credit:6.2f} {win:>4s}")

print("-" * len(hdr))
print(f"吻合数：假设 I = {score['I']}/{len(SAMPLES)}   假设 E = {score['E']}/{len(SAMPLES)}")

print("\n" + "=" * 92)
print("判别性分析")
print("=" * 92)
# 关键样本：think>0 且两假设给出不同 round 结果
decisive = []
for label, miss, hit, comp, think, credit in SAMPLES:
    ri, re = r2(pred_i(miss, hit, comp)), r2(pred_e(miss, hit, comp, think))
    if ri != re:
        decisive.append((label, ri, re, credit))

if decisive:
    print("以下样本能**区分**两假设（两假设给出不同的四舍五入结果）：")
    for label, ri, re, credit in decisive:
        verdict = "I 胜" if abs(ri - credit) < 1e-9 else ("E 胜" if abs(re - credit) < 1e-9 else "都不吻合")
        print(f"  {label}: I→{ri:.2f}  E→{re:.2f}  实测={credit:.2f}   → {verdict}")
else:
    print("没有样本能区分两假设（think 太小或对结果无影响）。")

print()
if score["I"] > score["E"]:
    print("结论：**假设 I 成立** —— completion_tokens 已包含思考 token。")
    print("      → 计费模型为 3 参数（输入/缓存读/输出），思考无需单独费率。")
    print("      → 这也解释了为何上游同时给 completion_thinking_tokens 与")
    print("        completion_tokens_details.reasoning_tokens（后者是前者的子集，")
    print("        均与 OpenAI 语义一致：reasoning_tokens ⊆ completion_tokens）。")
elif score["E"] > score["I"]:
    print("结论：**假设 E 成立** —— 思考 token 需在 completion_tokens 之外另计。")
    print("      → 计费模型需增加第 4 个费率 p_think。")
else:
    print("结论：无法区分，需补充 think 占比更大的样本（如 effort=high + 长任务）。")

print("\n" + "=" * 92)
print("对发送阶段的影响")
print("=" * 92)
print("""
- 若假设 I 成立：发送阶段只需记录 completion_tokens，无需关心 thinking。
- 无论哪种情况，都应**同时记录** completion_thinking_tokens：
  它是 Reasoning 模型的成本驱动因素，若将来上游把思考改为独立计价，
  有这一列就能立刻重新拟合而不丢历史。
""")
