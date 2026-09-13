"""join_probes_to_billing.py — 把探针记录和官网账单 xlsx 配上。

探针侧有 token，账单侧有积分。两边 ID 字符串通常对不上
（探针 cmb-/裸 hex，账单 crb-），但都可能是 UUIDv1，或至少有分钟级时间。

用法:
  python tools/join_probes_to_billing.py --billing path/to/request-usage.xlsx
  python tools/join_probes_to_billing.py --billing a.xlsx --probes docs/probes --json docs/probes/joined.json
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import sys
from collections import defaultdict
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fit_credit_rates import (  # type: ignore
    CAP,
    load_billing,
    lp_fit,
    param_intervals,
)
from probe_models_for_billing import decode_cmb  # type: ignore

TZ = dt.timezone(dt.timedelta(hours=8))


def load_probes(root: Path) -> list[dict]:
    recs = []
    for path in sorted(root.glob("batch-*.json")):
        d = json.loads(path.read_text(encoding="utf-8"))
        for r in d.get("records") or []:
            r = dict(r)
            r["_batch"] = path.name
            recs.append(r)
    return recs


def probe_unix(r: dict) -> float | None:
    cmb = r.get("cmb") or {}
    if cmb.get("unix"):
        return float(cmb["unix"])
    decoded = decode_cmb(r.get("response_id") or "")
    if decoded.get("unix"):
        return float(decoded["unix"])
    if r.get("sent_unix"):
        return float(r["sent_unix"])
    return None


def model_key(name: str | None) -> str:
    s = (name or "").strip().lower()
    # glm-5.2 请求可能返回 glm-5.2-x，账单通常记返回名。
    if s.endswith("-x"):
        s = s[:-2]
    return s


def billing_unix(b: dict) -> float | None:
    """Prefer UUIDv1 embedded time; else treat the minute column as UTC+8."""
    dec = b.get("decoded")
    if dec is not None:
        return dec.replace(tzinfo=TZ).timestamp()
    ts = b.get("ts")
    if ts is None:
        return None
    if getattr(ts, "tzinfo", None) is None:
        return ts.replace(tzinfo=TZ).timestamp()
    return ts.timestamp()


def id_body(value: str | None) -> str:
    s = (value or "").strip().lower()
    if "-" in s:
        s = s.split("-", 1)[-1]
    return s.replace("-", "")


def pair_row(b: dict, p: dict, bunix: float, punix: float, how: str) -> dict:
    miss = p.get("prompt_cache_miss_tokens") or 0
    hit = p.get("prompt_cache_hit_tokens") or 0
    out = p.get("completion_tokens") or 0
    return {
        "billing_id": b.get("RequestID"),
        "probe_id": p.get("response_id"),
        "match": how,
        "model": b.get("model"),
        "requested_model": p.get("requested_model"),
        "label": p.get("label"),
        "delta_sec": punix - bunix,
        "credits": b.get("credits"),
        "usage_credit": p.get("credit"),
        "miss": miss,
        "hit": hit,
        "out": out,
        "think": p.get("completion_thinking_tokens") or 0,
        "features_1k": [miss / 1000.0, hit / 1000.0, out / 1000.0],
    }


def greedy_join(billing: list[dict], probes: list[dict], tol: float = 3.0) -> list[dict]:
    used_p: set[int] = set()
    used_b: set[int] = set()
    pairs = []

    probe_by_body = {}
    for i, p in enumerate(probes):
        body = id_body(p.get("response_id"))
        if len(body) >= 16:
            probe_by_body[body] = i

    for bi, b in enumerate(billing):
        body = id_body(b.get("RequestID"))
        pi = probe_by_body.get(body)
        if pi is None or pi in used_p:
            continue
        p = probes[pi]
        bunix = billing_unix(b) or 0.0
        punix = probe_unix(p) or bunix
        used_b.add(bi)
        used_p.add(pi)
        pairs.append(pair_row(b, p, bunix, punix, "id_body"))

    buckets: dict[str, list[tuple[int, float]]] = defaultdict(list)
    for i, p in enumerate(probes):
        unix = probe_unix(p)
        if unix is None or not p.get("ok") or i in used_p:
            continue
        buckets[model_key(p.get("returned_model") or p.get("requested_model"))].append((i, unix))

    candidates = []
    for bi, b in enumerate(billing):
        if bi in used_b:
            continue
        bunix = billing_unix(b)
        if bunix is None:
            continue
        bm = model_key(b.get("model"))
        for pi, punix in buckets.get(bm, []):
            candidates.append((abs(punix - bunix), bi, pi, bunix, punix))
    candidates.sort()
    for dt_abs, bi, pi, bunix, punix in candidates:
        if dt_abs > tol:
            break
        if bi in used_b or pi in used_p:
            continue
        used_b.add(bi)
        used_p.add(pi)
        pairs.append(pair_row(billing[bi], probes[pi], bunix, punix, "uuid_time"))
    return pairs


def fit_by_model(pairs: list[dict]) -> dict:
    grouped: dict[str, list[dict]] = defaultdict(list)
    for p in pairs:
        grouped[p["model"]].append(p)
    out = {}
    names = ["input_per_1k", "cache_read_per_1k", "output_per_1k"]
    for model, rows in sorted(grouped.items()):
        import numpy as np

        X = np.array([r["features_1k"] for r in rows], dtype=float)
        y = np.array([r["credits"] for r in rows], dtype=float)
        entry = {"n_pairs": len(rows), "status": "insufficient_samples"}
        if len(rows) < 4:
            out[model] = entry
            continue
        p, viol = lp_fit(X, y)
        if p is None:
            entry["status"] = "fit_failed"
            out[model] = entry
            continue
        resid = y - X @ p
        entry.update({
            "status": "ok",
            "rates": {n: float(v) for n, v in zip(names, p)},
            "residual": {
                "mae": float(abs(resid).mean()),
                "max": float(abs(resid).max()),
                "total_violation": float(viol),
                "hard_cap_ok": bool(abs(resid).max() <= CAP + 1e-9 or viol <= 1e-9),
            },
        })
        _, iv = param_intervals(X, y, 0.01)
        if iv:
            entry["intervals"] = {}
            for n, (lo, hi), pt in zip(names, iv, p):
                rel = 100 * (hi - lo) / max(pt, 1e-12)
                entry["intervals"][n] = {"lo": lo, "hi": hi, "rel_width_pct": rel, "identifiable": rel <= 100}
        if p[0] > 0:
            entry["ratios"] = {
                "output_over_input": float(p[2] / p[0]),
                "cache_read_over_input": float(p[1] / p[0]),
            }
        if any(v.get("rel_width_pct", 0) > 100 for v in (entry.get("intervals") or {}).values()):
            entry["status"] = "partially_identifiable"
        out[model] = entry
    return out


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--billing", required=True)
    ap.add_argument("--probes", default="docs/probes")
    ap.add_argument("--tol", type=float, default=3.0)
    ap.add_argument("--json", default="docs/probes/joined.json")
    args = ap.parse_args()

    billing = load_billing(args.billing)
    probes = load_probes(Path(args.probes))
    ok_probes = [p for p in probes if p.get("ok") and p.get("response_id")]
    print(f"账单 {len(billing)} 行 / 探针成功 {len(ok_probes)} / 探针总 {len(probes)}")
    pairs = greedy_join(billing, probes, tol=args.tol)
    print(f"配对 {len(pairs)}  (tol={args.tol}s)")
    by = defaultdict(int)
    for p in pairs:
        by[p["model"]] += 1
    for m, n in sorted(by.items()):
        print(f"  {m:24s} {n}")

    models = fit_by_model(pairs)
    print("\n分模型费率（账单积分 × 探针 token）")
    for model, e in models.items():
        print(f"[{model}] n={e['n_pairs']} status={e['status']}")
        rates = e.get("rates") or {}
        if rates:
            print("  " + "  ".join(f"{k}={v:.6f}" for k, v in rates.items()))
            iv = e.get("intervals") or {}
            for k, spec in iv.items():
                flag = "" if spec.get("identifiable") else "  ← 不可识别"
                print(f"    {k:18s} [{spec['lo']:.6f}, {spec['hi']:.6f}] 宽={spec['rel_width_pct']:.1f}%{flag}")

    report = {
        "billing": args.billing,
        "probes": args.probes,
        "matched": len(pairs),
        "billing_rows": len(billing),
        "probe_ok": len(ok_probes),
        "pairs": pairs,
        "models": models,
    }
    Path(args.json).parent.mkdir(parents=True, exist_ok=True)

    def _json(o):
        if isinstance(o, (np.bool_,)):
            return bool(o)
        if isinstance(o, (np.integer,)):
            return int(o)
        if isinstance(o, (np.floating,)):
            v = float(o)
            return None if v != v else v
        raise TypeError(type(o))

    Path(args.json).write_text(
        json.dumps(report, ensure_ascii=False, indent=2, default=_json),
        encoding="utf-8",
    )
    print(f"\n写出 {args.json}")


if __name__ == "__main__":
    main()
