"""probe_thinking.py — 发送阶段最后一个未知项：思考 token 是否单独计价？

背景：上游 usage 同时给出
    completion_tokens              输出 token
    completion_thinking_tokens     思考 token（疑似独立计数）
    completion_tokens_details.reasoning_tokens

若 completion_tokens **不含**思考 token，且二者分别计价，则计费模型需第 4 个费率；
若 completion_tokens **已含**思考，则现有 3 参数模型仍成立。

方法：对同一模型发"必须长推理"的请求，比较 completion_tokens 与
completion_thinking_tokens 的量级关系，并用 credit 反推。
"""

import json
import urllib.request

BASE = "http://127.0.0.1:7863"
KEY = "123"


def post(payload):
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="POST")
    with urllib.request.urlopen(req, timeout=300) as r:
        return json.loads(r.read().decode())


TASK = ("A bat and a ball cost $1.10 in total. The bat costs $1.00 more than the ball. "
        "How much does the ball cost? Think step by step, then give the answer. ")

print("=" * 84)
print("思考 token 与输出的关系")
print("=" * 84)
print(f"{'模型':22s} {'effort':8s} {'prompt':>7s} {'comp':>6s} {'think':>6s} {'reason':>7s} {'credit':>7s}")
print("-" * 84)

for model, effort in [("deepseek-v4.1-flash", None),
                      ("deepseek-v4.1-flash", "high"),
                      ("deepseek-v4-pro", None),
                      ("deepseek-v4-pro", "high"),
                      ("glm-5.3", "high")]:
    payload = {"model": model,
               "messages": [{"role": "system", "content": "You are a careful reasoner."},
                            {"role": "user", "content": TASK}],
               "stream": False, "max_tokens": 2000}
    if effort:
        payload["reasoning_effort"] = effort
    try:
        resp = post(payload)
        u = resp.get("usage", {})
        comp = u.get("completion_tokens", 0)
        think = u.get("completion_thinking_tokens", 0)
        reason = (u.get("completion_tokens_details") or {}).get("reasoning_tokens", 0)
        print(f"{model:22s} {str(effort or '-'):8s} {u.get('prompt_tokens',0):7d} "
              f"{comp:6d} {think:6d} {reason:7d} {u.get('credit',0):7.2f}")
    except Exception as e:
        print(f"{model:22s} {str(effort or '-'):8s}  失败: {str(e)[:40]}")

print("-" * 84)
print("""
判读要点：
  - 若 think > 0 且 comp 与 think 数量级相当或 comp 明显偏小
      → completion_tokens 很可能**不含**思考，thinking 另计
  - 若 think > 0 但 comp >= think（comp 是二者之和）
      → completion_tokens **已含**思考，3 参数模型仍成立
  - reason 与 think 的关系也一并记录（两个字段可能同义）
""")
