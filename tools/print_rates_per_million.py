import json
from pathlib import Path


def per_m(v):
    return None if v is None else v * 1000.0


def fmt(v, ident=True):
    if v is None:
        return "—"
    if ident is False:
        return f"{v:.1f}?"
    return f"{v:.1f}"


joined = json.loads(Path("docs/probes/joined.json").read_text(encoding="utf-8"))
flash = json.loads(Path("docs/probes/rate_model_from_0911.json").read_text(encoding="utf-8"))
rows = []
for model, entry in joined["models"].items():
    rates = entry.get("rates") or {}
    iv = entry.get("intervals") or {}
    rows.append((
        model,
        per_m(rates.get("input_per_1k")),
        (iv.get("input_per_1k") or {}).get("identifiable"),
        per_m(rates.get("cache_read_per_1k")),
        (iv.get("cache_read_per_1k") or {}).get("identifiable"),
        per_m(rates.get("output_per_1k")),
        (iv.get("output_per_1k") or {}).get("identifiable"),
    ))
fr = flash["models"]["deepseek-v4.1-flash"]["rates"]
rows.append((
    "deepseek-v4.1-flash",
    per_m(fr["input_per_1k"]), True,
    per_m(fr["cache_read_per_1k"]), True,
    per_m(fr["output_per_1k"]), True,
))
rows.sort(key=lambda x: -(x[1] or 0))
print(f"{'model':20s} {'input':>10s} {'cache_read':>12s} {'output':>10s}")
for model, pin, iin, pcr, icr, pou, iou in rows:
    print(f"{model:20s} {fmt(pin, iin):>10s} {fmt(pcr, icr):>12s} {fmt(pou, iou):>10s}")
