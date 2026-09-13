import sqlite3
from datetime import datetime, timedelta, timezone

DB = r"E:\DeepSeekHarness\dsh-custom-reasoning\_cmp\wk-run\data\metrics.db"
TZ = timezone(timedelta(hours=8))
con = sqlite3.connect("file:" + DB.replace("\\", "/") + "?mode=ro", uri=True)
for model in ["deepseek-v4.1-flash", "glm-5.3"]:
    print("=" , model)
    print("status/error 分布:")
    for row in con.execute(
        "select status, error_code, count(*) from request_logs where model=? group by 1,2 order by 3 desc",
        (model,),
    ):
        print(" ", row)
    print("最后 8 条:")
    for row in con.execute(
        """select datetime(created_at,'unixepoch','+8 hours'), status, input_tokens, output_tokens,
                  cache_read_tokens, cache_write_tokens, error_code
           from request_logs where model=? order by created_at desc limit 8""",
        (model,),
    ):
        print(" ", row)
con.close()
