"""uuid_decode.py — 解码账单 crb- 与本地上游 cmb- ID 内嵌的 UUIDv1 时间戳。

动机：账单 ID 与本地 ID 前缀不同（crb- vs cmb-），字符串无交集。
但两者结构相同（prefix + 32 hex，第3段为 "11f1" → UUID 版本 1）。
UUIDv1 内嵌 60 位时间戳（100ns 粒度，1582-10-15 起）。

若两者都能解码出与各自请求时间一致的时刻，则：
  (a) 说明 crb-/cmb- 同源（同一套 UUIDv1 生成器）；
  (b) 退路：可用"解码时间 + 模型"作为 JOIN 键，绕过前缀差异。
"""

import datetime
import sqlite3
import sys
from collections import Counter

sys.path.insert(0, "tools")
from parse_billing_xlsx import read_sheet

BILLING = r"C:\Program Files\Netease\GameViewer\Download\request-usage-2026-09-10.xlsx"
DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"

# UUID v1 纪元：1582-10-15 00:00:00 UTC
GREGORIAN_OFFSET = 0x01B21DD213814000  # 100ns 间隔数


def decode_v1(uid):
    """crb-XXXXXXXXXXXX11f1XXXXXXXXXXXX 形式 → (datetime_utc, version, variant_ok)"""
    if "-" not in uid:
        return None
    hexpart = uid.split("-", 1)[1].replace("-", "")
    if len(hexpart) != 32:
        return None
    try:
        time_low = int(hexpart[0:8], 16)
        time_mid = int(hexpart[8:12], 16)
        ver_hi = int(hexpart[12:16], 16)
        version = (ver_hi >> 12) & 0xF
        time_hi = ver_hi & 0x0FFF
    except ValueError:
        return None
    ts = (time_hi << 48) | (time_mid << 32) | time_low
    if ts < GREGORIAN_OFFSET:
        return None
    secs = (ts - GREGORIAN_OFFSET) / 1e7
    return datetime.datetime(1970, 1, 1, tzinfo=datetime.timezone.utc) + datetime.timedelta(seconds=secs), version


def main():
    rows = read_sheet(BILLING)
    h = rows[0]
    b = []
    for r in rows[1:]:
        r = r + [""] * (len(h) - len(r))
        if not any(x.strip() for x in r):
            continue
        d = dict(zip(h, r))
        d["ts"] = datetime.datetime.strptime(d["时间"], "%Y-%m-%d %H:%M:%S")
        b.append(d)

    print("=== 账单 crb- ID 解码 ===")
    print(f"{'账单时间(本地)':20s} {'解码UTC':26s} {'版本':4s} {'差(秒)':>9s}")
    deltas = []
    for x in b[:12]:
        res = decode_v1(x["RequestID"])
        if not res:
            print(f"  {x['RequestID']} 解码失败")
            continue
        dt, ver = res
        # 账单时间是本地时间；本地时区 = UTC+8
        local_from_utc = dt.replace(tzinfo=None) + datetime.timedelta(hours=8)
        delta = (local_from_utc - x["ts"]).total_seconds()
        deltas.append(delta)
        print(f"{str(x['ts']):20s} {dt.strftime('%Y-%m-%d %H:%M:%S.%f')[:26]:26s} {ver:<4d} {delta:9.1f}")

    print(f"\n  前12条 delta 中位数: {sorted(deltas)[len(deltas)//2]:.1f}s")
    print("  → delta ≈ 0 说明 ID 内嵌时间 == 请求时间（UTC+8 解释正确）")

    # 全部
    alld = []
    for x in b:
        res = decode_v1(x["RequestID"])
        if res:
            dt, ver = res
            alld.append((dt.replace(tzinfo=None) + datetime.timedelta(hours=8) - x["ts"]).total_seconds())
    if alld:
        alld.sort()
        print(f"  全部 {len(alld)} 条: min={alld[0]:.0f}s 中位={alld[len(alld)//2]:.0f}s max={alld[-1]:.0f}s")
    print(f"  版本位分布: {dict(Counter(decode_v1(x['RequestID'])[1] for x in b if decode_v1(x['RequestID'])))}")

    # ---------- 本地 cmb- ----------
    c = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
    recs = []
    for i, ca, m in c.execute("select id, created_at, model from request_logs where id like 'cmb-%' limit 200"):
        recs.append((i, datetime.datetime.fromtimestamp(ca), m))
    c.close()

    print("\n=== 本地 cmb- ID 解码 ===")
    print(f"{'本地时间':20s} {'解码UTC':26s} {'版本':4s} {'差(秒)':>9s}")
    ld = []
    for i, ts, m in recs[:12]:
        res = decode_v1(i)
        if not res:
            print(f"  {i} 解码失败")
            continue
        dt, ver = res
        local = dt.replace(tzinfo=None) + datetime.timedelta(hours=8)
        d = (local - ts).total_seconds()
        ld.append(d)
        print(f"{str(ts)[:19]:20s} {dt.strftime('%Y-%m-%d %H:%M:%S.%f')[:26]:26s} {ver:<4d} {d:9.1f}")

    allc = []
    for i, ts, m in recs:
        res = decode_v1(i)
        if res:
            dt, ver = res
            allc.append((dt.replace(tzinfo=None) + datetime.timedelta(hours=8) - ts).total_seconds())
    if allc:
        allc.sort()
        print(f"  全部 {len(allc)} 条: min={allc[0]:.0f}s 中位={allc[len(allc)//2]:.0f}s max={allc[-1]:.0f}s")

    print("\n=== 结论 ===")
    print("  若 crb- 与 cmb- 的 delta 分布相似（都≈0），说明同源 UUIDv1，")
    print("  前缀差异不代表命名空间不同；可用 (解码时刻, 模型) 作 JOIN 键。")


if __name__ == "__main__":
    main()
