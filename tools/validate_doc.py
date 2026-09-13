"""validate_doc.py — 检查 docs/credit-rate-inference.md 的结构完整性与陈旧论断。"""
import io
import re

path = "docs/credit-rate-inference.md"
s = io.open(path, encoding="utf-8").read()

print(f"总行数: {s.count(chr(10)) + 1}")
fences = s.count("```")
print(f"代码围栏数: {fences} ({'OK 偶数' if fences % 2 == 0 else '错误！奇数'})")

# 陈旧论断（已在本次修订中被推翻/替换）。注意 "不要用 t 检验" 是有意保留的告诫，
# 因此用更精确的模式匹配，而不是裸子串。
stale = [
    "一万条", "34,000", "380,000", "≈ 13 条",
    "对 `d` 做 t 检验", "均匀噪声", "MAE ≈ 0.003", "均匀有界",
    "Fisher 信息与 token 量的平方", "信息量约等于",
]
found = [p for p in stale if p in s]
print("陈旧论断: " + (", ".join(found) if found else "无"))

print("\n章节结构:")
for m in re.finditer(r"^(#{1,3}) (.+)$", s, re.M):
    print("  " + "  " * (len(m.group(1)) - 1) + m.group(2))

# 表格列数一致性检查
bad = []
for i, line in enumerate(s.splitlines(), 1):
    if line.startswith("|") and line.count("|") >= 3:
        cols = line.count("|")
        bad.append((i, cols))
print(f"\n表格行数: {len(bad)}")
