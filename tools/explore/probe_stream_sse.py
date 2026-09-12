"""probe_stream_sse.py — 发送阶段两个决定性问题的原始抓包：

Q1. **流式**（主流量的形态）最后一帧 usage 里有没有 `credit`？
    —— 决定发送阶段能否在 stream 路径直接拿到费用。
Q2. `prompt_cache_write_tokens` 是否**曾经非零**？
    —— 若它非零且与 `prompt_cache_miss_tokens` 独立，
       就能把 p_cw 与 p_in 分离（阶段一判定"不可识别"，那是**因为离线
       日志把两者混成一列**，而不是上游真的不区分！）。

只做最少量的请求（每次约 0.01~0.05 积分）。
"""

import json
import urllib.request

BASE = "http://127.0.0.1:7863"
KEY = "123"


def post(payload, stream):
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        BASE + "/v1/chat/completions", data=data,
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json",
                 "Accept": "text/event-stream" if stream else "application/json"},
        method="POST")
    if not stream:
        with urllib.request.urlopen(req, timeout=180) as r:
            return json.loads(r.read().decode())
    # 流式：逐行读，收集所有含 usage 的帧
    frames = []
    with urllib.request.urlopen(req, timeout=180) as r:
        for raw in r:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data:"):
                continue
            body = line[5:].strip()
            if body == "[DONE]":
                break
            try:
                obj = json.loads(body)
            except json.JSONDecodeError:
                continue
            if "usage" in obj and obj["usage"]:
                frames.append(obj["usage"])
    return frames


def show(tag, usage):
    keys = ["prompt_tokens", "prompt_cache_hit_tokens", "prompt_cache_miss_tokens",
            "prompt_cache_write_tokens", "cache_creation_input_tokens",
            "cache_read_input_tokens", "cached_tokens", "completion_tokens",
            "completion_thinking_tokens", "credit", "total_tokens"]
    print(f"  {tag}:")
    for k in keys:
        if k in usage:
            v = usage[k]
            flag = "  ← 非零！" if (k == "prompt_cache_write_tokens" and v) else ""
            print(f"    {k:32s} = {v}{flag}")
    extra = set(usage) - set(keys)
    if extra:
        print(f"    (其它字段: {sorted(extra)})")
    # 检查是否有任何 credit 类字段
    for k, v in usage.items():
        if "credit" in k.lower() or "cost" in k.lower() or "bill" in k.lower():
            print(f"    ★ 费用类字段 {k} = {v}")


print("=" * 80)
print("Q1. 流式请求的 usage（最后一帧）")
print("=" * 80)
long_text = "The quick brown fox jumps over the lazy dog. " * 300
frames = post({"model": "deepseek-v4.1-flash",
               "messages": [{"role": "system", "content": "You are terse."},
                            {"role": "user", "content": long_text}],
               "stream": True, "max_tokens": 5}, stream=True)
print(f"  收到含 usage 的帧数: {len(frames)}")
if frames:
    show("末帧 usage", frames[-1])
    if len(frames) > 1:
        show("首帧 usage", frames[0])
    # 全部帧里是否出现过非零 credit
    credits = [f.get("credit") for f in frames if "credit" in f]
    print(f"  各帧 credit 取值: {credits}")
    print(f"  → 流式路径{'可' if credits else '不可'}直接拿到 credit")
else:
    print("  ⚠ 流式响应里**没有**任何带 usage 的帧")

print()
print("=" * 80)
print("Q2. prompt_cache_write_tokens 是否会非零（决定 p_cw 能否分离）")
print("=" * 80)
# 用不同长度制造"写入量 ≠ 未命中量"的情形；并尝试不同模型
tests = [
    ("首次写入（冷）", "deepseek-v4.1-flash", "Alpha beta gamma delta epsilon. " * 400),
    ("重复（应命中）", "deepseek-v4.1-flash", "Alpha beta gamma delta epsilon. " * 400),
    ("换新内容（再写入）", "deepseek-v4.1-flash", "Zeta eta theta iota kappa. " * 400),
]
for tag, model, text in tests:
    try:
        u = post({"model": model,
                  "messages": [{"role": "system", "content": "You are terse."},
                               {"role": "user", "content": text}],
                  "stream": False, "max_tokens": 5}, stream=False).get("usage", {})
        print(f"\n  [{tag}] model={model}")
        show("usage", u)
    except Exception as e:
        print(f"  [{tag}] 失败: {e}")
