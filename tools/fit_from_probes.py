"""fit_from_probes.py — 用探针记录里的 usage.credit 拟合各模型费率。

账单还没覆盖探针时间窗时，用这个先出一版。credit 与账单同为 0.01 精度。
"""

from __future__ import annotations

import json
import sys
from collections import defaultdict
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fit_credit_rates import CAP, lp_fit, param_intervals  # type: ignore
from join_probes_to_billing import load_probes, model_key  # type: ignore


def main() -> None:
    probes = [p for p in load_probes(Path("docs/probes")) if p.get("ok") and p.get("credit") is not None]
    grouped: dict[str, list] = defaultdict(list)
    for p in probes:
        grouped[p.get("requested_model") or p.get("returned_model") or "?"].append(p)

    names = ["input_per_1k", "cache_read_per_1k", "output_per_1k"]
    report = {"source": "probe usage.credit", "n": len(probes), "models": {}}
    print(f"探针成功且有 credit: {len(probes)}")
    for model, rows in sorted(grouped.items()):
        X = np.array([
            [
                (r.get("prompt_cache_miss_tokens") or 0) / 1000.0,
                (r.get("prompt_cache_hit_tokens") or 0) / 1000.0,
                (r.get("completion_tokens") or 0) / 1000.0,
            ]
            for r in rows
        ], dtype=float)
        y = np.array([float(r.get("credit") or 0) for r in rows], dtype=float)
        stds = X.std(axis=0)
        print(f"\n[{model}] n={len(rows)}  miss/hit/out std={stds.round(3).tolist()}  credit[{y.min():.2f},{y.max():.2f}]")
        entry = {"n": len(rows), "status": "insufficient_samples", "credit_sum": float(y.sum())}
        if len(rows) < 4:
            report["models"][model] = entry
            print("  样本不足")
            continue
        p, viol = lp_fit(X, y)
        if p is None:
            entry["status"] = "fit_failed"
            report["models"][model] = entry
            print("  拟合失败")
            continue
        resid = y - X @ p
        entry.update({
            "status": "ok",
            "rates": {n: float(v) for n, v in zip(names, p)},
            "residual": {
                "mae": float(np.abs(resid).mean()),
                "max": float(np.abs(resid).max()),
                "total_violation": float(viol),
                "hard_cap_ok": bool(float(np.abs(resid).max()) <= CAP + 1e-9 or viol <= 1e-9),
            },
        })
        _, iv = param_intervals(X, y, 0.01)
        if iv:
            entry["intervals"] = {}
            print("  " + "  ".join(f"{n}={v:.6f}" for n, v in zip(names, p)))
            print(f"  viol={viol:.5f} MAE={entry['residual']['mae']:.5f} max={entry['residual']['max']:.5f}")
            for n, (lo, hi), pt in zip(names, iv, p):
                rel = 100 * (hi - lo) / max(pt, 1e-12)
                ident = bool(rel <= 100)
                entry["intervals"][n] = {
                    "lo": float(lo) if lo == lo else None,
                    "hi": float(hi) if hi == hi else None,
                    "rel_width_pct": None if rel != rel else float(rel),
                    "identifiable": ident,
                }
                print(f"    {n:18s} [{lo:.6f}, {hi:.6f}] 宽={rel:6.1f}%" + ("" if ident else "  ← 不可识别"))
            if any(not v["identifiable"] for v in entry["intervals"].values()):
                entry["status"] = "partially_identifiable"
        if p[0] > 0:
            entry["ratios"] = {
                "output_over_input": float(p[2] / p[0]),
                "cache_read_over_input": float(p[1] / p[0]),
            }
        report["models"][model] = entry

    out = Path("docs/probes/rates_from_credit.json")
    def _json(o):
        if isinstance(o, (np.bool_,)):
            return bool(o)
        if isinstance(o, (np.integer,)):
            return int(o)
        if isinstance(o, (np.floating,)):
            v = float(o)
            return None if v != v else v
        raise TypeError(type(o))

    out.write_text(json.dumps(report, ensure_ascii=False, indent=2, default=_json), encoding="utf-8")
    print(f"\n写出 {out}")


if __name__ == "__main__":
    main()
