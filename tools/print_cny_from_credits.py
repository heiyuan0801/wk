import json
from pathlib import Path


def per_m(v):
    return None if v is None else v * 1000.0


def load_rows():
    joined = json.loads(Path("docs/probes/joined.json").read_text(encoding="utf-8"))
    flash = json.loads(Path("docs/probes/rate_model_from_0911.json").read_text(encoding="utf-8"))
    rows = []
    for model, entry in joined["models"].items():
        rates = entry.get("rates") or {}
        iv = entry.get("intervals") or {}
        rows.append({
            "model": model,
            "in": per_m(rates.get("input_per_1k")),
            "in_ok": (iv.get("input_per_1k") or {}).get("identifiable"),
            "cr": per_m(rates.get("cache_read_per_1k")),
            "cr_ok": (iv.get("cache_read_per_1k") or {}).get("identifiable"),
            "out": per_m(rates.get("output_per_1k")),
            "out_ok": (iv.get("output_per_1k") or {}).get("identifiable"),
        })
    fr = flash["models"]["deepseek-v4.1-flash"]["rates"]
    rows.append({
        "model": "deepseek-v4.1-flash",
        "in": per_m(fr["input_per_1k"]), "in_ok": True,
        "cr": per_m(fr["cache_read_per_1k"]), "cr_ok": True,
        "out": per_m(fr["output_per_1k"]), "out_ok": True,
    })
    rows.sort(key=lambda r: -(r["in"] or 0))
    return rows


def fmt_cny(credits, ident, yuan_per_credit):
    if credits is None:
        return "—"
    yuan = credits * yuan_per_credit
    s = f"{yuan:.3f}".rstrip("0").rstrip(".")
    if ident is False:
        return s + "?"
    return s


def dump(title, yuan_per_credit):
    rows = load_rows()
    print(title)
    print(f"{'model':20s} {'输入':>8s} {'缓存读':>8s} {'输出':>8s} {'1M入+1M出':>10s}")
    for r in rows:
        inn = fmt_cny(r["in"], r["in_ok"], yuan_per_credit)
        cr = fmt_cny(r["cr"], r["cr_ok"], yuan_per_credit)
        out = fmt_cny(r["out"], r["out_ok"], yuan_per_credit)
        mix = None
        mix_ok = True
        if r["in"] is not None and r["out"] is not None:
            mix = (r["in"] + r["out"]) * yuan_per_credit
            mix_ok = (r["in_ok"] is not False) and (r["out_ok"] is not False)
        mix_s = "—" if mix is None else f"{mix:.3f}".rstrip("0").rstrip(".") + ("" if mix_ok else "?")
        print(f"{r['model']:20s} {inn:>8s} {cr:>8s} {out:>8s} {mix_s:>10s}")
    print()


dump("A) 10元=5000积分  →  0.002 元/积分   单位: 元 / 百万 tokens", 0.002)
dump("B) 10元=10000积分 →  0.001 元/积分   单位: 元 / 百万 tokens", 0.001)
