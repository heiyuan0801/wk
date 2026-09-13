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
    "in_ok": True, "cr_ok": True, "out_ok": True,
}


def cell(v, ok, listed):
    if listed in (None, 0) or v is None:
        return "—"
    if ok is False:
        return f"{v/listed:.2f}x?"
    return f"{v/listed:.2f}x"


print(f"{'model':22s} {'输入倍数':>10s} {'缓存倍数':>10s} {'输出倍数':>10s} {'三项中位':>10s}")
for model, listed in LIST.items():
    o = ours.get(model)
    if not o:
        print(f"{model:22s} {'无样本':>10s} {'无样本':>10s} {'无样本':>10s}")
        continue
    xs = []
    for key, ok_key in (("in", "in_ok"), ("cr", "cr_ok"), ("out", "out_ok")):
        if o.get(ok_key) is False:
            continue
        if listed[key]:
            xs.append(o[key] / listed[key])
    mid = sorted(xs)[len(xs)//2] if xs else None
    mid_s = f"{mid:.2f}x" if mid else "—"
    print(
        f"{model:22s} "
        f"{cell(o['in'], o['in_ok'], listed['in']):>10s} "
        f"{cell(o['cr'], o['cr_ok'], listed['cr']):>10s} "
        f"{cell(o['out'], o['out_ok'], listed['out']):>10s} "
        f"{mid_s:>10s}"
    )

print("\n实测(积分/百万) vs 表内数字")
print(f"{'model':22s} {'in ours/list':>22s} {'cr ours/list':>22s} {'out ours/list':>22s}")
for model, listed in LIST.items():
    o = ours.get(model)
    if not o:
        continue
    print(
        f"{model:22s} "
        f"{o['in']:.1f}/{listed['in']} "
        f"{o['cr']:.2f}/{listed['cr']} "
        f"{o['out']:.1f}/{listed['out']}"
    )
