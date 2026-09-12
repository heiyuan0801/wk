"""probe_all_models.py — 发送阶段关键问题：**所有模型**都在 usage 里返回 credit 吗？

结论直接影响发送阶段的降级设计：
  - 若全部返回 → 可把 usage.credit 作为权威费用，彻底取代离线配对
  - 若有模型缺失 → 需要 per-model 降级（回退到本地费率估算）

用极小请求（max_tokens=8）控制成本。
"""

import json
import urllib.request

BASE = "http://127.0.0.1:7863"
KEY = "123"

MODELS = ["auto", "hy4-preview", "hy3", "hy3-x", "deepseek-v4.1-flash",
          "glm-5.3", "glm-5.3-flash", "glm-5.2", "glm-5.1", "glm-5v-turbo",
          "kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "minimax-m3", "deepseek-v4-pro"]


def probe(model):
    payload = {"model": model,
               "messages": [{"role": "system", "content": "Be terse."},
                            {"role": "user", "content": "Say OK"}],
               "stream": False, "max_tokens": 8}
    req = urllib.request.Request(BASE + "/v1/chat/completions",
                                 data=json.dumps(payload).encode(),
                                 headers={"Authorization": f"Bearer {KEY}",
                                          "Content-Type": "application/json"},
                                 method="POST")
    with urllib.request.urlopen(req, timeout=180) as r:
        return json.loads(r.read().decode())


print("=" * 100)
print("跨模型 usage 字段检查（发送阶段能否统一读 credit）")
print("=" * 100)
hdr = (f"{'模型':20s} {'prompt':>7s} {'miss':>6s} {'hit':>6s} {'cw':>4s} "
       f"{'comp':>5s} {'think':>6s} {'credit':>7s} {'有credit':>8s}")
print(hdr)
print("-" * len(hdr))

rows = []
for m in MODELS:
    try:
        resp = probe(m)
        u = resp.get("usage", {}) or {}
        # 兼容上游可能返回不同模型名
        real = resp.get("model", m)
        has = "credit" in u
        rows.append((m, real, u, has))
        print(f"{m:20s} {u.get('prompt_tokens',0):7d} {u.get('prompt_cache_miss_tokens',0):6d} "
              f"{u.get('prompt_cache_hit_tokens',0):6d} {u.get('prompt_cache_write_tokens',0):4d} "
              f"{u.get('completion_tokens',0):5d} {u.get('completion_thinking_tokens',0):6d} "
              f"{u.get('credit',0):7.2f} {'✅' if has else '❌':>8s}")
    except Exception as e:
        print(f"{m:20s} 失败: {str(e)[:50]}")
        rows.append((m, m, {}, False))

print("-" * len(hdr))
ok = sum(1 for _, _, _, h in rows if h)
print(f"返回 credit 的模型: {ok}/{len(rows)}")

print("\n" + "=" * 100)
print("字段名一致性（决定解析代码能否统一）")
print("=" * 100)
allkeys = {}
for m, real, u, _ in rows:
    for k in u:
        allkeys[k] = allkeys.get(k, 0) + 1
for k in sorted(allkeys, key=lambda x: -allkeys[x]):
    print(f"  {k:36s} 出现于 {allkeys[k]:2d}/{len(rows)} 个模型")

print("\n  关键字段缺失情况:")
for want in ["credit", "prompt_cache_miss_tokens", "prompt_cache_hit_tokens",
             "completion_thinking_tokens", "prompt_cache_write_tokens"]:
    missing = [m for m, _, u, _ in rows if want not in u]
    if missing:
        print(f"    {want:32s} 缺失于: {missing}")
    else:
        print(f"    {want:32s} 全部模型都有 ✅")

print("\n" + "=" * 100)
print("prompt_cache_write_tokens 是否恒为 0")
print("=" * 100)
vals = sorted(set(u.get("prompt_cache_write_tokens") for _, _, u, _ in rows if u))
print(f"  取值集合: {vals}")
print(f"  → {'恒为 0，缓存写入不单独计量' if vals in ([0], []) else '存在非零值，需重新评估 p_cw'}")

print("\n" + "=" * 100)
print("credit 取值分布")
print("=" * 100)
cs = sorted(u.get("credit", 0) for _, _, u, _ in rows if u)
print(f"  {cs}")
print(f"  非零数: {sum(1 for c in cs if c)}/{len(cs)}")
