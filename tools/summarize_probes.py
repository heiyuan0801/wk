"""Compact probe-batch summary. Prints counts only — do not dump records."""

from __future__ import annotations

import json
import sys
from collections import Counter, defaultdict
from pathlib import Path

root = Path("docs/probes")
files = sorted(root.glob("batch-*.json"))
if not files:
    print("no probe batches")
    sys.exit(0)

for path in files[-3:]:
    d = json.loads(path.read_text(encoding="utf-8"))
    recs = d.get("records") or []
    ok = sum(1 for r in recs if r.get("ok"))
    fail = len(recs) - ok
    spent = d.get("spent_credits") or 0
    print(f"\n{path.name}  n={len(recs)} ok={ok} fail={fail} credit={spent:.2f}  finished={d.get('finished_at') or 'running'}")
    by = defaultdict(lambda: Counter())
    for r in recs:
        m = r.get("requested_model") or "?"
        by[m]["n"] += 1
        by[m]["ok"] += int(bool(r.get("ok")))
        by[m]["has_id"] += int(bool(r.get("response_id")))
        by[m]["hit"] += int((r.get("prompt_cache_hit_tokens") or 0) > 0)
        if r.get("credit"):
            by[m]["credit_nz"] += 1
        if r.get("http_status") == 429:
            by[m]["429"] += 1
    for m, c in by.items():
        print(
            f"  {m:20s} n={c['n']:2d} ok={c['ok']:2d} id={c['has_id']:2d} "
            f"cache_hit_rows={c['hit']:2d} credit>0={c['credit_nz']:2d} 429={c['429']}"
        )
    skipped = d.get("skipped_cooldown") or {}
    if skipped:
        print("  skipped cooldown:", ", ".join(skipped))
