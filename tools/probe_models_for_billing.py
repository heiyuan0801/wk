"""probe_models_for_billing.py — 给后续账单反推准备请求级探针。

目标不是当场算出费率，而是发出「token 规模有梯度、缓存冷/热成对、输出有长短」
的请求，并把 pairing 所需字段全部落盘：

  - 上游响应 id（cmb-… UUIDv1，可解码到秒级时刻）
  - X-Request-Id
  - 请求/返回模型名
  - prompt / miss / hit / write / out / think
  - usage.credit（网关目前不会记进 metrics.db，这里单独保存）
  - 本地发送时刻（UTC+8）

配对时用 (UUIDv1 时刻, 模型)，账单 xlsx 的 RequestID 也是 UUIDv1。
请求会刻意拉开间隔，避免同一分钟挤太多条导致账单分钟桶歧义。

用法:
  python tools/probe_models_for_billing.py
  python tools/probe_models_for_billing.py --models glm-5.2,kimi-k3 --budget 20
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import time
import urllib.error
import urllib.request
from pathlib import Path

BASE = "http://127.0.0.1:7863"
KEY = "123"
TZ = dt.timezone(dt.timedelta(hours=8))
GREGORIAN_OFFSET = 0x01B21DD213814000
SKIP_MODELS = {"auto"}  # 路由名，账单会记到真实模型上
SYS = {
    "role": "system",
    "content": "You are a terse assistant. Answer with the minimum text requested. Do not add commentary.",
}
LOREM = "lorem ipsum dolor sit amet consectetur adipiscing elit "


def now_iso() -> str:
    return dt.datetime.now(TZ).isoformat(timespec="seconds")


def uuid1_unix(hex32: str) -> float | None:
    try:
        u = int(hex32, 16)
    except ValueError:
        return None
    version = (u >> 76) & 0xF
    if version != 1:
        return None
    time_low = u >> 96
    time_mid = (u >> 80) & 0xFFFF
    time_hi = (u >> 64) & 0x0FFF
    ts_100ns = (time_hi << 48) | (time_mid << 32) | time_low
    unix = (ts_100ns - GREGORIAN_OFFSET) / 1e7
    # Windows fromtimestamp 对过远的时间会抛 OSError。
    if unix < 0 or unix > 4102444800:  # 2100-01-01
        return None
    return unix


def decode_cmb(rid: str) -> dict:
    out = {"raw": rid, "unix": None, "iso_utc8": None}
    if not rid:
        return out
    hex32 = rid.split("-", 1)[-1].replace("-", "")
    if len(hex32) != 32:
        return out
    unix = uuid1_unix(hex32)
    if unix is None:
        return out
    try:
        out["unix"] = unix
        out["iso_utc8"] = dt.datetime.fromtimestamp(unix, TZ).isoformat(timespec="seconds")
    except (OSError, OverflowError, ValueError):
        out["unix"] = None
        out["iso_utc8"] = None
    return out


def api(path: str, payload: dict | None = None, timeout: int = 30):
    data = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(
        BASE + path,
        data=data,
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="GET" if payload is None else "POST",
    )
    with urllib.request.urlopen(req, timeout=timeout) as r:
        headers = {k: v for k, v in r.headers.items()}
        body = json.loads(r.read().decode())
        return r.status, headers, body


def filler(n_chars: int, nonce: str) -> str:
    body = (LOREM * (n_chars // len(LOREM) + 2))[:n_chars]
    return f"PROBE_NONCE={nonce}\n{body}\nReply with exactly: OK"


def build_plan(nonce: str) -> list[dict]:
    text_8k = filler(16_000, nonce)
    text_32k = filler(64_000, nonce)
    return [
        {
            "label": "tiny",
            "messages": [SYS, {"role": "user", "content": f"PROBE_NONCE={nonce}\nReply with exactly: OK"}],
            "max_tokens": 8,
            "purpose": "round-vs-ceil / 最小计费",
        },
        {
            "label": "in-8K-cold",
            "messages": [SYS, {"role": "user", "content": text_8k}],
            "max_tokens": 8,
            "purpose": "p_in 中档输入",
        },
        {
            "label": "in-32K-cold",
            "messages": [SYS, {"role": "user", "content": text_32k}],
            "max_tokens": 8,
            "purpose": "p_in 长输入",
        },
        {
            "label": "in-32K-hot",
            "messages": [SYS, {"role": "user", "content": text_32k}],
            "max_tokens": 8,
            "purpose": "p_cr 热缓存（与 in-32K-cold 同文）",
        },
        {
            "label": "out-mid",
            "messages": [SYS, {"role": "user", "content": f"PROBE_NONCE={nonce}\nCount from 1 to 80, space separated, no other text."}],
            "max_tokens": 200,
            "purpose": "p_out 中档输出",
        },
        {
            "label": "out-long",
            "messages": [SYS, {"role": "user", "content": f"PROBE_NONCE={nonce}\nCount from 1 to 300, space separated, no other text."}],
            "max_tokens": 800,
            "purpose": "p_out 长输出",
        },
    ]


def extract_usage(body: dict) -> dict:
    u = body.get("usage") or {}
    return {
        "prompt_tokens": u.get("prompt_tokens") or 0,
        "completion_tokens": u.get("completion_tokens") or 0,
        "total_tokens": u.get("total_tokens") or 0,
        "prompt_cache_miss_tokens": u.get("prompt_cache_miss_tokens") or 0,
        "prompt_cache_hit_tokens": u.get("prompt_cache_hit_tokens") or 0,
        "prompt_cache_write_tokens": u.get("prompt_cache_write_tokens") or 0,
        "completion_thinking_tokens": u.get("completion_thinking_tokens") or 0,
        "credit": float(u["credit"]) if "credit" in u and u["credit"] is not None else None,
        "usage_keys": sorted(u.keys()),
    }


def cooldown_models() -> dict[str, dict]:
    try:
        _, _, status = api("/status")
    except Exception as e:
        print(f"读取 /status 失败: {e}")
        return {}
    out = {}
    for acct in status.get("accounts") or []:
        for model, info in (acct.get("model_cooldowns") or {}).items():
            out[model] = info
    return out


def list_models() -> list[str]:
    _, _, body = api("/v1/models")
    return [m["id"] for m in body.get("data") or []]


def chat(model: str, messages: list, max_tokens: int) -> dict:
    payload = {
        "model": model,
        "messages": messages,
        "stream": False,
        "max_tokens": max_tokens,
    }
    sent_unix = time.time()
    sent_iso = now_iso()
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            headers = {k: v for k, v in r.headers.items()}
            body = json.loads(r.read().decode())
            status = r.status
            err = None
    except urllib.error.HTTPError as e:
        raw = e.read().decode(errors="replace")
        try:
            body = json.loads(raw)
        except Exception:
            body = {"raw": raw}
        headers = {k: v for k, v in e.headers.items()} if e.headers else {}
        status = e.code
        err = raw[:800]
    except Exception as e:
        return {
            "ok": False,
            "http_status": 0,
            "error": str(e),
            "sent_unix": sent_unix,
            "sent_iso_utc8": sent_iso,
            "elapsed_sec": round(time.time() - sent_unix, 3),
        }

    rid = body.get("id") or ""
    usage = extract_usage(body) if isinstance(body, dict) else {}
    return {
        "ok": 200 <= status < 300,
        "http_status": status,
        "error": err,
        "sent_unix": sent_unix,
        "sent_iso_utc8": sent_iso,
        "elapsed_sec": round(time.time() - sent_unix, 3),
        "response_id": rid,
        "x_request_id": headers.get("X-Request-Id") or headers.get("X-Request-ID"),
        "cmb": decode_cmb(rid),
        "returned_model": body.get("model") if isinstance(body, dict) else None,
        **usage,
        "error_body_excerpt": (err[:400] if err else None),
    }


def atomic_write(path: Path, obj) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(obj, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(path)


def append_jsonl(path: Path, obj) -> None:
    with path.open("a", encoding="utf-8") as f:
        f.write(json.dumps(obj, ensure_ascii=False) + "\n")


def is_rate_limited(rec: dict) -> bool:
    if rec.get("http_status") == 429:
        return True
    text = (rec.get("error") or "") + (rec.get("error_body_excerpt") or "")
    return "6004" in text or "rate limit" in text.lower()


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default="", help="逗号分隔；空=全部可用模型")
    ap.add_argument("--budget", type=float, default=40.0, help="usage.credit 累计上限")
    ap.add_argument("--delay", type=float, default=6.0, help="请求间隔秒（拉开账单分钟桶）")
    ap.add_argument("--out-dir", default="docs/probes")
    ap.add_argument("--skip-models", default="", help="额外跳过的模型，逗号分隔")
    args = ap.parse_args()

    nonce = dt.datetime.now(TZ).strftime("RATEPROBE-%Y%m%dT%H%M%S")
    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    stamp = dt.datetime.now(TZ).strftime("%Y%m%d-%H%M%S")
    json_path = out_dir / f"batch-{stamp}.json"
    jsonl_path = out_dir / f"batch-{stamp}.jsonl"

    cooling = cooldown_models()
    available = list_models()
    if args.models.strip():
        wanted = [m.strip() for m in args.models.split(",") if m.strip()]
    else:
        wanted = [m for m in available if m not in SKIP_MODELS]

    extra_skip = {m.strip() for m in args.skip_models.split(",") if m.strip()}
    skipped_cool = [m for m in wanted if m in cooling]
    models = [m for m in wanted if m not in cooling and m not in extra_skip]
    plan = build_plan(nonce)

    report = {
        "generated_at": now_iso(),
        "nonce": nonce,
        "base": BASE,
        "purpose": "collect request IDs + token usage for later billing xlsx join",
        "pairing_hint": "join billing RequestID (UUIDv1) to cmb.unix within ~2s, plus model",
        "delay_sec": args.delay,
        "budget": args.budget,
        "available_models": available,
        "skipped_cooldown": {m: cooling[m] for m in skipped_cool},
        "skipped_alias": sorted(SKIP_MODELS),
        "plan": [{"label": p["label"], "max_tokens": p["max_tokens"], "purpose": p["purpose"]} for p in plan],
        "spent_credits": 0.0,
        "records": [],
        "by_model": {},
    }
    atomic_write(json_path, report)
    print(f"nonce={nonce}")
    print(f"out={json_path}")
    print(f"冷却跳过: {skipped_cool or '无'}")
    print(f"将探测: {models}")
    print(f"{'model':20s} {'probe':14s} {'id':38s} {'miss':>6s} {'hit':>6s} {'out':>5s} {'credit':>7s} {'st':>4s}")
    print("-" * 110)

    spent = 0.0
    stop_all = False
    for model in models:
        if stop_all:
            break
        model_spent = 0.0
        model_ok = 0
        for probe in plan:
            if spent >= args.budget:
                print("全局预算用尽，停止")
                stop_all = True
                break
            rec = {
                "requested_model": model,
                "label": probe["label"],
                "purpose": probe["purpose"],
                "nonce": nonce,
                "max_tokens": probe["max_tokens"],
            }
            rec.update(chat(model, probe["messages"], probe["max_tokens"]))
            credit = rec.get("credit")
            if isinstance(credit, (int, float)):
                spent += float(credit)
                model_spent += float(credit)
            rec["spent_credits_after"] = spent
            report["records"].append(rec)
            report["spent_credits"] = spent
            report["by_model"].setdefault(model, {"ok": 0, "fail": 0, "spent": 0.0, "stopped": None})
            if rec.get("ok"):
                model_ok += 1
                report["by_model"][model]["ok"] += 1
            else:
                report["by_model"][model]["fail"] += 1
            report["by_model"][model]["spent"] = model_spent
            append_jsonl(jsonl_path, rec)
            atomic_write(json_path, report)

            rid = (rec.get("response_id") or "-")[:36]
            print(
                f"{model:20s} {probe['label']:14s} {rid:38s} "
                f"{rec.get('prompt_cache_miss_tokens') or 0:6d} "
                f"{rec.get('prompt_cache_hit_tokens') or 0:6d} "
                f"{rec.get('completion_tokens') or 0:5d} "
                f"{(credit if credit is not None else float('nan')):7.2f} "
                f"{rec.get('http_status') or 0:4d}"
            )
            if not rec.get("ok") and is_rate_limited(rec):
                report["by_model"][model]["stopped"] = "rate_limited"
                print(f"  → {model} 触发限流，跳过该模型剩余探针")
                break
            time.sleep(args.delay)
        else:
            report["by_model"][model]["stopped"] = "complete" if model_ok == len(plan) else "partial"
            atomic_write(json_path, report)
            time.sleep(max(2.0, args.delay / 2))

    report["finished_at"] = now_iso()
    report["spent_credits"] = spent
    atomic_write(json_path, report)
    print("-" * 110)
    print(f"完成 {len(report['records'])} 条，usage.credit 累计 {spent:.2f}")
    print(f"JSON  {json_path}")
    print(f"JSONL {jsonl_path}")
    print("下一步：从官网导出覆盖这段时间的请求账单 xlsx，再跑 tools/fit_credit_rates.py")


if __name__ == "__main__":
    os.chdir(Path(__file__).resolve().parents[1])
    main()
