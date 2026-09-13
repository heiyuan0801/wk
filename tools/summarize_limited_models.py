"""Summarize token/credit usage for currently limited models."""
from __future__ import annotations

import sqlite3
import sys
from collections import defaultdict
from datetime import datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fit_credit_rates import load_billing  # type: ignore

TZ = timezone(timedelta(hours=8))
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"
BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-11 (1).xlsx"
MODELS = ["deepseek-v4.1-flash", "glm-5.3"]
LIMITED_AT = {
    "deepseek-v4.1-flash": datetime(2026, 9, 12, 22, 48, 24, tzinfo=TZ),
    "glm-5.3": datetime(2026, 9, 12, 5, 57, 55, tzinfo=TZ),
}


def fmt_int(n):
    return f"{n:,}"


def fmt_dt(ts):
    if ts is None:
        return "-"
    if isinstance(ts, datetime):
        if ts.tzinfo is None:
            ts = ts.replace(tzinfo=TZ)
        return ts.astimezone(TZ).strftime("%Y-%m-%d %H:%M:%S")
    return datetime.fromtimestamp(ts, TZ).strftime("%Y-%m-%d %H:%M:%S")


def load_local():
    con = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    cols = [
        "id", "created_at", "model", "status", "input_tokens", "output_tokens",
        "total_tokens", "cache_read_tokens", "cache_write_tokens",
        "credits_consumed", "credit_source", "error_code",
    ]
    recs = []
    for row in con.execute(f"select {','.join(cols)} from request_logs"):
        recs.append(dict(zip(cols, row)))
    con.close()
    return recs


def bucket_local(recs, model):
    rows = [r for r in recs if r["model"] == model]
    ok = [r for r in rows if r["status"] == 200]
    limited = [r for r in rows if "6004" in (r.get("error_code") or "") or r["status"] == 429]
    by_day = defaultdict(lambda: {"n": 0, "ok": 0, "in": 0, "out": 0, "hit": 0, "miss": 0})
    for r in rows:
        day = datetime.fromtimestamp(r["created_at"], TZ).strftime("%Y-%m-%d")
        b = by_day[day]
        b["n"] += 1
        if r["status"] == 200:
            b["ok"] += 1
            b["in"] += r["input_tokens"] or 0
            b["out"] += r["output_tokens"] or 0
            b["hit"] += r["cache_read_tokens"] or 0
            b["miss"] += r["cache_write_tokens"] or 0
    first = min((r["created_at"] for r in rows), default=None)
    last_ok = max((r["created_at"] for r in ok), default=None)
    last_lim = max((r["created_at"] for r in limited), default=None)
    return {
        "n": len(rows),
        "ok": len(ok),
        "limited": len(limited),
        "in": sum(r["input_tokens"] or 0 for r in ok),
        "out": sum(r["output_tokens"] or 0 for r in ok),
        "hit": sum(r["cache_read_tokens"] or 0 for r in ok),
        "miss": sum(r["cache_write_tokens"] or 0 for r in ok),
        "first": first,
        "last_ok": last_ok,
        "last_lim": last_lim,
        "by_day": dict(by_day),
    }


def bucket_billing(rows, model):
    items = [r for r in rows if r["model"] == model]
    by_day = defaultdict(lambda: {"n": 0, "credits": 0.0})
    for r in items:
        by_day[str(r["ts"].date())]["n"] += 1
        by_day[str(r["ts"].date())]["credits"] += r["credits"]
    return {
        "n": len(items),
        "credits": sum(r["credits"] for r in items),
        "zero": sum(1 for r in items if r["credits"] == 0),
        "first": min((r["ts"] for r in items), default=None),
        "last": max((r["ts"] for r in items), default=None),
        "by_day": dict(by_day),
    }


local = load_local()
billing = load_billing(BILLING)

print("限流状态")
print("  deepseek-v4.1-flash  到 2026-09-12 22:48:24 +08  (code=6004)")
print("  glm-5.3              到 2026-09-12 05:57:55 +08  (code=6004)")
print()

for model in MODELS:
    L = bucket_local(local, model)
    B = bucket_billing(billing, model)
    print("=" * 72)
    print(model)
    print("- 本地日志（token 准，积分字段当时没记上）")
    print(f"  请求 {L['n']}  成功 {L['ok']}  限流 {L['limited']}")
    print(f"  时间 {fmt_dt(L['first'])} -> 最后成功 {fmt_dt(L['last_ok'])} / 最后限流 {fmt_dt(L['last_lim'])}")
    print(f"  input {fmt_int(L['in'])}  miss {fmt_int(L['miss'])}  hit {fmt_int(L['hit'])}  output {fmt_int(L['out'])}")
    print("- 账单（积分准）")
    print(f"  行数 {B['n']}  积分 {B['credits']:.2f}  其中 0.00 行 {B['zero']}")
    print(f"  时间 {fmt_dt(B['first'])} -> {fmt_dt(B['last'])}")
    days = sorted(set(L["by_day"]) | set(B["by_day"]))
    print("  按日:")
    print(f"    {'日期':12s} {'本地成功':>8s} {'input':>12s} {'output':>10s} {'账单行':>8s} {'账单积分':>10s}")
    for day in days:
        lb = L["by_day"].get(day, {})
        bb = B["by_day"].get(day, {})
        print(
            f"    {day:12s} {lb.get('ok',0):8d} {fmt_int(lb.get('in',0)):>12s} "
            f"{fmt_int(lb.get('out',0)):>10s} {bb.get('n',0):8d} {bb.get('credits',0):10.2f}"
        )
    # credit estimate from fitted flash rates if flash
    if model == "deepseek-v4.1-flash":
        est = L["miss"] / 1000 * 0.014301 + L["hit"] / 1000 * 0.000290 + L["out"] / 1000 * 0.057487
        print(f"  用拟合费率估算本地成功请求积分 ≈ {est:.2f}  （账单合计 {B['credits']:.2f}）")
    print()
