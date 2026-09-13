import json
from pathlib import Path

LIST = {
    "deepseek-v4.1-flash": {"in": 1.00, "cr": 0.02, "out": 4.00},
    "deepseek-v4-pro": {"in": 3.00, "cr": 0.025, "out": 6.00},
    "glm-5.3": {"in": 8.00, "cr": 2.00, "out": 28.00},
    "glm-5.3-flash": {"in": 0.80, "cr": 0.23, "out": 2.80},
    "kimi-k2.6": {"in": 6.50, "cr": 1.10, "out": 27.00},
    "kimi-k3": {"in": 20.00, "cr": 2.00, "out": 100.00},
}

YPC = 7 / 2000  # 0.0035 元/积分

joined = json.loads(Path("docs/probes/joined.json").read_text(encoding="utf-8"))
flash = json.loads(Path("docs/probes/rate_model_from_0911.json").read_text(encoding="utf-8"))
ours = {}
for model, entry in joined["models"].items():
    rates = entry.get("rates") or {}
    iv = entry.get("intervals") or {}
    ours[model] = {
        "in": (rates.get("input_per_1k") or 0) * 1000,
        "cr": (rates.get("cache_read_per_1k") or 0) * 1000,
        "out": (rates.get("output_per_1k") or 0) * 1000,
        "in_ok": (iv.get("input_per_1k") or {}).get("identifiable"),
        "cr_ok": (iv.get("cache_read_per_1k") or {}).get("identifiable"),
        "out_ok": (iv.get("output_per_1k") or {}).get("identifiable"),
    }
fr = flash["models"]["deepseek-v4.1-flash"]["rates"]
ours["deepseek-v4.1-flash"] = {
    "in": fr["input_per_1k"] * 1000,
    "cr": fr["cache_read_per_1k"] * 1000,
    "out": fr["output_per_1k"] * 1000,
    "in_ok": True,
    "cr_ok": True,
    "out_ok": True,
}


def ratio(credits_per_m, listed, ok):
    if not listed:
        return "—"
    x = credits_per_m * YPC / listed
    return f"{x:.3f}x" + ("?" if ok is False else "")


print(f"7元=2000积分  单价={YPC:.4f}元/积分")
print("倍数 = 实测人民币 / 你给的人民币牌价   (<1 表示更便宜)")
print(f"{'model':22s} {'输入':>10s} {'缓存':>10s} {'输出':>10s} {'中位':>10s} {'约几分之一':>12s}")
for model, listed in LIST.items():
    o = ours.get(model)
    if not o:
        print(f"{model:22s} {'无拟合费率':>10s}")
        continue
    xs = []
    for key, okk in (("in", "in_ok"), ("cr", "cr_ok"), ("out", "out_ok")):
        if o.get(okk) is False:
            continue
        if listed[key]:
            xs.append(o[key] * YPC / listed[key])
    mid = sorted(xs)[len(xs) // 2] if xs else None
    inv = f"1/{1/mid:.1f}" if mid else "—"
    print(
        f"{model:22s} "
        f"{ratio(o['in'], listed['in'], o['in_ok']):>10s} "
        f"{ratio(o['cr'], listed['cr'], o['cr_ok']):>10s} "
        f"{ratio(o['out'], listed['out'], o['out_ok']):>10s} "
        f"{(f'{mid:.3f}x' if mid else '—'):>10s} "
        f"{inv:>12s}"
    )
