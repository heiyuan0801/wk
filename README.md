

## 快速开始

### 1. 克隆 & 配置

```bash
git clone https://github.com/Sliverkiss/workbuddy2api.git
cd workbuddy2api
cp config.example.json config.json
# 编辑 config.json，设置 api_key
```

### 2. 添加账号

```bash
./login.sh
# 打开浏览器登录 → 按 y → 自动落盘 auths/ → 重启容器
```

### 3. 启动服务

```bash
docker compose up -d --build
```

### 4. 验证

```bash
# 模型列表
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer your-api-key"

# 账号状态（汇总 + 每账号详情）
curl -s http://localhost:7863/status -H "Authorization: Bearer your-api-key"

# 健康检查（无健康账号时 503）
curl -s http://localhost:7863/healthz

# 聊天补全（流式）
curl -sN http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":true}'

# 聊天补全（非流式，本地聚合）
curl -s http://localhost:7863/v1/chat/completions \
  -H "Authorization: Bearer your-api-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}],"stream":false}'
```

## 配置说明

> **权威字段定义见 [`config.example.json`](config.example.json)**：它是当前 schema 的唯一权威，下方样例与之保持一致。`cp config.example.json config.json` 即可得到完整默认配置。

```json
{
  "listen": ":7863",
  "api_key": "your-api-key-here",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "cooldown": {
    "soft_rate": "60s"
  },
  "schedule": {
    "checkin_hours": [9, 21],
    "keepalive_hours": [22]
  },
  "upstream": {
    "timeout_seconds": 120
  },
  "features": {
    "sanitize_blacklist_fingerprints": true,
    "passthrough": false
  },
  "billing": {
    "input_credits_per_1k_tokens": 0,
    "output_credits_per_1k_tokens": 0,
    "cached_input_credits_per_1k_tokens": 0
  },
  "upstash": {
    "url": "",
    "token": ""
  },
  "pool": {
    "max_in_flight": 3,
    "breaker_threshold": 3,
    "breaker_cooldown": "30m",
    "breaker_cooldown_max": "6h",
    "idle_weight_per_hour": 0.5,
    "idle_weight_max": 5.0
  },
  "session_sticky": {
    "enabled": true,
    "ttl": "30m",
    "gc_interval": "5m"
  }
}
```

**注意**：`cooldown.hard_credit` / `cooldown.err_threshold` / `cooldown.err_cooldown` 三个历史键已退役。硬冷却固定为**次日 04:00**（本地时区，`CooldownUntilTomorrow4AM`），连续错误语义并入熔断器（`pool.breaker_threshold` 触发指数退避）。旧配置中的这些键因 JSON 未知字段被自然忽略，不报错。

## 账号轮换与冷却策略

### 状态机

```
Healthy → Cooling → (签到恢复) → Healthy
   ↓           ↑
Disabled ←────┘ (session 死亡，永久)
```

### 错误分类

| 错误类型 | 冷却策略 | 恢复方式 |
|---|---|---|
| **402 + 余额关键词** | 冷却到**次日 04:00** | 签到任务（09:00/21:00）自动恢复 |
| **429 限流** | 60s 短冷却 | 到期自动恢复 |
| **401 + session 死亡** | **永久禁用** | 人工重新登录 |
| **404 上游偶发** | 60s 短冷却（不累计错误计数） | 到期自动恢复 |
| **5xx 上游故障** | 喂熔断计数（`pool.breaker_threshold` 触发指数退避熔断） | 熔断到期自动恢复 / 成功清零 |
| **网络抖动** | **不计失败**，立即换号重试 | 即时 |

### 挑选策略

1. **状态过滤**：Disabled / Cooling / 熔断 / 在途占满 不选
2. **Top-5 候选**：按三因子权重降序取前 5（credits 只是权重的一个因子，闲置补偿与成功率同样决定谁进短名单）
3. **三因子加权随机**：权重 = credits 比例 ×10 + 闲置补偿 + 成功率 ×3（credits 全 0 仍按闲置+成功率加权）
4. **防惊群**：跳过 100ms 内刚被选中的账号（除非 top5 全部刚被用过，退回 LRU）

## 账号池 v3

在 v2 基础上吸收外部项目成熟设计，引入四块能力：

- **熔断器（指数退避）**：连续 `pool.breaker_threshold` 次失败熔断，退避 `breaker_cooldown × 2^retryCount` 封顶 `breaker_cooldown_max`；成功清零。单一连续失败计数器 `fails`，签到解冻只清冷却（余额恢复）不动熔断——熔断作为"连续 5xx"信号要到退避到期或下次 chat 成功才恢复。
- **三因子加权选取**：`credits 比例 ×10 + idleWeight + successRate ×3`。闲置补偿每小时 `+idle_weight_per_hour`（封顶 `idle_weight_max`），成功率无记录给中性 1.5。
- **在途租约**：单账号并发上限 `pool.max_in_flight`（0 = 不限），`Pick` 跳过占满账号。
- **会话粘性路由**：同一 `metadata.conversation_id`/`conversation_id`/`metadata.user_id` 尽量绑定同一账号，TTL 滚动续期；请求失败自动解绑回落轮换，请求成功后会话绑定**跟随最终成功号**。
- **全冷却兜底**：无 healthy 账号时从冷却账号选最早到期者顶班（禁用与余额耗尽号永不参与）。

### Redis（Upstash）镜像

- 配置 `upstash.url/token`（空 = 纯内存模式，一切功能照常，只打一条启动警告）。
- Redis 仅做异步镜像（粘性会话映射防重启丢失 + 池状态快照恢复备份），**不在请求热路径同步调用**。
- 池状态快照：每次本地 `state.json` 落盘同步镜像一份到 Redis（带 `saved_at`）；启动时**择新恢复**——Redis 快照比本地新才采用，否则本地优先。
- `/status` 透出 `redis_mode`（`upstash`/`noop`）与池级 `sticky_sessions`。
- 账号状态同时包含上游 `capacity_size/remain/used`、`cycle_capacity_size/remain/used` 和 `credit_updated_at`；服务启动、定时签到或调用积分刷新接口时更新，并随 `state.json` 与 Redis 快照持久化。

### 请求日志、token 与积分

每个 Chat Completions 和 Responses 请求结束后都会写入 `state_file` 同目录的
`metrics.db`。记录只包含模型、路由、状态、账号 UID、token、耗时、积分来源和错误码，
不会保存提示词或模型输出，但会保存最多 1,024 字符的错误详情。数据库保留最近 10,000 条，`GET /requests?limit=50`
可查询最近记录，控制台也会展示同一份数据。
控制台“请求日志”菜单支持按模型、端点、账号、错误、流式/同步/透传和成功状态筛选，并可展开查看缓存 token、工具调用与完整错误详情。

stdout 同时保留一行便于排查的表格日志：

```
| #001 | 18:31:31 | deepseek-v4 | stream | 200 | uid=0851ce35 | TTFB=801ms | tok=60 | 23.5tok/s | total=2.6s | credits=0.24(upstream) |
```

字段说明：
- `#001`：请求序号（进程级 atomic counter）
- `TTFB`：首 token 到达时间（stream 模式）
- `tok`：输出 token 数（从上游 usage.completion_tokens 精确读取，非估算）
- `uid`：账号 UID 前 8 位
- `credits`：积分消耗及来源；`upstream` 为上游真实值，`estimated` 为本地费率估算

上游没有返回积分字段时，可用 `billing` 中三个“每 1,000 token 积分”费率估算。
默认值均为 `0`，此时不猜测消耗并在请求记录中标记为 `unknown`。

### 原始流透传

设置 `features.passthrough=true` 后，流式 Chat Completions 会原样转发上游 SSE 字节，
保留上游扩展字段，不再执行帧规范化或补写 `[DONE]`。启用总开关后，可在单次请求中
发送 `X-WorkBuddy-Passthrough: false` 临时关闭。请求体中的未知字段本来就会保留；模型别名、
`tool_choice`、`reasoning_effort` 和 `stream` 仍会按上游兼容要求转换。

## 工具脚本

| 脚本 | 用途 |
|---|---|
| `./login.sh` | OAuth 登录，落盘 auth 文件 |
| `./credit.sh` | 积分日报（美化输出） |
| `./credit.sh -json` | 积分原始 JSON |
| `./signin.sh` | 批量签到（遍历 auths/ 下所有账号） |

## API 端点

| 端点 | 鉴权 | 说明 |
|---|---|---|
| `POST /v1/chat/completions` | Bearer | OpenAI 兼容聊天补全（流式/非流式） |
| `POST /v1/responses` | Bearer | Responses API 适配（流式/非流式、工具调用、`previous_response_id`） |
| `GET /v1/models` | Bearer | 模型列表（动态拉取 + 静态兜底） |
| `GET /status` | Bearer | 账号状态汇总（total/healthy/cooling/disabled + 每账号详情） |
| `GET /requests?limit=50` | Bearer | 最近请求日志（最多 200 条，不含提示词和响应正文） |
| `POST /admin/credits/refresh` | 前端会话/Bearer | 异步刷新所有账号的上游积分明细，不执行签到 |
| `POST /admin/account/{uid}/enable` | 前端会话/Bearer | 手动启用账号并清除禁用/冷却状态 |
| `POST /admin/account/{uid}/disable` | 前端会话/Bearer | 手动禁用账号，停止新请求使用 |
| `DELETE /admin/account/{uid}` | 前端会话/Bearer | 删除账号池记录及对应授权文件 |
| `GET /healthz` | 无 | 健康检查（无健康账号时 503） |

ZCode 等使用 OpenAI Compatible 提供商的客户端，Base URL 应填写
`https://你的域名/v1`（直连本服务时为 `http://服务器地址:7863/v1`），模型名称单独填写。
客户端会在 Base URL 后追加 `/chat/completions`；如果填写域名根路径，中间网关可能
返回 HTML 页面，使客户端误报“模型未返回任何内容”。检查响应应为
`text/event-stream`（流式）或 `application/json`（非流式）。
本服务也提供同鉴权的 `/chat/completions`、`/responses`、`/models` 别名，方便直连客户端。
上游流读取超时会返回 `upstream_timeout` 错误帧和 `[DONE]`；Responses 使用
`response.failed`，不会把中断保存为已完成的会话。

上游 WorkBuddy 返回的请求 ID 会原值传给客户端：兼容响应体中的 `id`、`request_id`、
`requestId`、`requestID`、`record_id`、`recordId`、`recordID` 以及常见请求 ID 响应头。Chat Completions、Responses
和请求日志使用同一个上游 ID，不添加前缀，也不重新生成；只有上游完全未返回 ID 时才使用本地兜底 ID。

重复调用 `/v1/responses` 时应复用稳定的 `prompt_cache_key` 或 `conversation`；使用上一轮返回的 `previous_response_id` 时，服务会恢复该轮上下文并继续使用同一账号。响应历史默认保留 1 小时；配置 Upstash 后会同步到 Redis，可跨进程重启和多实例继续会话，未配置时使用进程内存。

## 稳定性设计

- **防雪崩**：上游 4xx/5xx 轮转重试（不直接返回），404 短冷却 60s 不累计失败
- **错误分流**：网络层错误不计失败（避免抖动连坐）；HTTP 5xx 喂单一连续失败计数器，达 `breaker_threshold`（默认 3）触发指数退避熔断
- **请求日志**：SQLite 持久化模型、token、积分、TTFB、总耗时和错误码，stdout 保留精简表格
- **连接池**：`MaxIdleConnsPerHost=20` 减少 TLS 握手
- **凭证续期**：token 临近过期自动 refresh，失败禁用账号
- **状态持久化**：`data/state.json` dirty flag + 5s 周期异步落盘，进程退出前强制 flush
- **防惊群**：100ms 窗口内不重复选中同一账号（高并发时打散热点）

## 开发

### 测试

```bash
go build ./...
go test ./... -count=20  # 20 次全绿（无 flake）
go vet ./...
gofmt -l .  # 应为空
```

### 代码结构

```
cmd/
  server/     # 主服务入口
  login/      # OAuth 登录工具
  credit/     # 积分查询工具
  signin/     # 批量签到工具
internal/
  auth/       # auth 文件解析 + token 刷新
  pool/       # 账号池（状态机 + 冷却 + 持久化）
  scheduler/  # 定时签到 + 积分查询
  server/     # HTTP handler + 请求日志
  upstream/   # 上游 API 封装（chat/billing/auth）
```

## 免责声明

本项目仅供学习和研究使用。使用者需遵守 WorkBuddy / CodeBuddy 的服务条款，自行承担使用风险。作者不对任何因使用本项目产生的直接或间接损失负责。

## License

MIT
