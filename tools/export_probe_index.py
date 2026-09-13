"""export_probe_index.py — 把探针批次压成对照表，不含 prompt/响应正文。"""

from __future__ import annotations

import csv
import json
from pathlib import Path

ROOT = Path("docs/probes")
FIELDS = [
    "batch", "sent_iso_utc8", "requested_model", "returned_model", "label",
    "response_id", "cmb_iso_utc8", "http_status", "prompt_tokens",
    "prompt_cache_miss_tokens", "prompt_cache_hit_tokens", "completion_tokens",
    "completion_thinking_tokens", "credit",
]


def main() -> None:
    rows = []
    for path in sorted(ROOT.glob("batch-*.json")):
        d = json.loads(path.read_text(encoding="utf-8"))
        for r in d.get("records") or []:
            cmb = r.get("cmb") or {}
            rows.append({
                "batch": path.name,
                "sent_iso_utc8": r.get("sent_iso_utc8"),
                "requested_model": r.get("requested_model"),
                "returned_model": r.get("returned_model"),
                "label": r.get("label"),
                "response_id": r.get("response_id"),
                "cmb_iso_utc8": cmb.get("iso_utc8"),
                "http_status": r.get("http_status"),
                "prompt_tokens": r.get("prompt_tokens"),
                "prompt_cache_miss_tokens": r.get("prompt_cache_miss_tokens"),
                "prompt_cache_hit_tokens": r.get("prompt_cache_hit_tokens"),
                "completion_tokens": r.get("completion_tokens"),
                "completion_thinking_tokens": r.get("completion_thinking_tokens"),
                "credit": r.get("credit"),
            })
    csv_path = ROOT / "index.csv"
    json_path = ROOT / "index.json"
    with csv_path.open("w", encoding="utf-8-sig", newline="") as f:
        w = csv.DictWriter(f, fieldnames=FIELDS)
        w.writeheader()
        w.writerows(rows)
    json_path.write_text(json.dumps(rows, ensure_ascii=False, indent=2), encoding="utf-8")
    ok = sum(1 for r in rows if r.get("http_status") == 200)
    print(f"rows={len(rows)} ok={ok}  {csv_path}  {json_path}")


if __name__ == "__main__":
    main()
