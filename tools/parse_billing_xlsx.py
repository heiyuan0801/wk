"""parse_billing_xlsx.py — 解析 WorkBuddy 官网导出的请求级账单 xlsx。

不依赖 pandas/openpyxl（本机未安装），直接用 zipfile + ElementTree 读 sheet1.xml。
导出文件为 inline/shared-less 形式：<c r="A2" t="str"><v>...</v></c>
"""

import io
import sys
import zipfile
import xml.etree.ElementTree as ET
from collections import Counter, defaultdict

NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"


def col_index(ref):
    """A2 -> 0, B2 -> 1, AA2 -> 26"""
    letters = "".join(ch for ch in ref if ch.isalpha())
    n = 0
    for ch in letters:
        n = n * 26 + (ord(ch) - ord("A") + 1)
    return n - 1


def read_sheet(path):
    with zipfile.ZipFile(path) as z:
        names = z.namelist()
        shared = []
        if "xl/sharedStrings.xml" in names:
            root = ET.fromstring(z.read("xl/sharedStrings.xml"))
            for si in root.findall(f"{NS}si"):
                shared.append("".join(t.text or "" for t in si.iter(f"{NS}t")))
        sheet_name = next(n for n in names if n.startswith("xl/worksheets/sheet") and n.endswith(".xml"))
        root = ET.fromstring(z.read(sheet_name))

    rows = []
    for row in root.iter(f"{NS}row"):
        cells = {}
        for c in row.findall(f"{NS}c"):
            idx = col_index(c.get("r"))
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
            width = max(cells) + 1
            rows.append([cells.get(i, "") for i in range(width)])
    return rows


def main(path):
    rows = read_sheet(path)
    header = rows[0]
    print(f"表头: {header}")
    data = [r for r in rows[1:] if any(x.strip() for x in r)]
    print(f"数据行数: {len(data)}")

    # 正规化为 dict
    recs = []
    for r in data:
        r = r + [""] * (len(header) - len(r))
        recs.append(dict(zip(header, r)))

    print("\n--- 模型分布 ---")
    for m, n in Counter(r.get("模型", "") for r in recs).most_common():
        print(f"  {m:32s} {n:4d}")

    print("\n--- 客户端分布 ---")
    for c, n in Counter(r.get("客户端", "") for r in recs).most_common():
        print(f"  {repr(c):32s} {n:4d}")

    print("\n--- 时间范围 ---")
    times = sorted(r.get("时间", "") for r in recs if r.get("时间"))
    print(f"  最早: {times[0]}")
    print(f"  最晚: {times[-1]}")
    print(f"  不同时间戳数: {len(set(times))}")

    print("\n--- 积分消耗分布 ---")
    credits = []
    for r in recs:
        try:
            credits.append(float(r.get("积分消耗", "")))
        except ValueError:
            pass
    if credits:
        print(f"  n={len(credits)} min={min(credits)} max={max(credits)} "
              f"sum={sum(credits):.2f} mean={sum(credits)/len(credits):.4f}")
        print(f"  最小值 {min(credits)} / 是否全是 0.01 的整数倍: "
              f"{all(abs(c*100 - round(c*100)) < 1e-9 for c in credits)}")
        print(f"  出现 0.00 的行数: {sum(1 for c in credits if c == 0)}")
        print("  取值频次（前 15）:")
        for v, n in Counter(credits).most_common(15):
            print(f"    {v:6.2f}  x{n}")

    print("\n--- RequestID 样本 ---")
    ids = [r.get("RequestID", "") for r in recs]
    for i in ids[:3]:
        print(f"  {i}  (len={len(i)})")
    print(f"  唯一 ID 数: {len(set(ids))} / {len(ids)}")
    prefixes = Counter(i.split("-")[0] for i in ids if i)
    print(f"  ID 前缀: {dict(prefixes)}")

    print("\n--- 按模型 × 积分 ---")
    by_model = defaultdict(list)
    for r in recs:
        try:
            by_model[r.get("模型", "")].append(float(r.get("积分消耗", "")))
        except ValueError:
            pass
    for m, vs in sorted(by_model.items(), key=lambda kv: -len(kv[1])):
        print(f"  {m:32s} n={len(vs):3d} sum={sum(vs):8.2f} mean={sum(vs)/len(vs):7.4f} "
              f"min={min(vs):5.2f} max={max(vs):6.2f}")
    return recs


if __name__ == "__main__":
    p = sys.argv[1] if len(sys.argv) > 1 else \
        r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
    main(p)
