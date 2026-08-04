# 中转站设计文档（CodeAgentRouter）

- 日期：2026-08-04
- 状态：已评审待实现
- 技术栈：Go（单进程轻量应用）

## 1. 背景与目标

团队内多人使用 LLM API（OpenAI 协议，含 DeepSeek、通义等兼容模型）。上游对每个 key 有 token 用量限制，且不同用户用量不均，导致"有人浪费、有人不够用"。本中转站将用户各自的 key 汇聚为一个池，统一入口、统一配额管理、动态复用空闲额度，并记录请求明细用于精细流控与月度报表。

**核心目标：**
1. 单入口代理 OpenAI 协议请求（含流式）。
2. 按用户执行配额控制：每小时 1000 万 token（仅工作时段生效）、每天 4 亿 token、每分钟 10 次调用。
3. 动态共享池：用户触顶后自动借用池内其他 key 的剩余额度。
4. Admin 通过 Web 后台配置用户额度、管理 key。
5. 记录每次请求明细，支持月度报表与流控分析。

**非目标（YAGNI，本版本不做）：**
- 多实例横向扩展、K8s、消息队列。
- 除 OpenAI 协议以外的多供应商代理。
- 计费/扣费。
- 接入 Redis、数据库（纯内存 + 本地文件持久化，适配 <100 人单实例局域网场景）。

## 2. 术语

| 术语 | 含义 |
|---|---|
| 中转 key | 用户调用中转站时使用的凭证（Bearer token），由中转站签发，与用户绑定 |
| 上游 key | 用户上传的真实模型 API key，存于中转站，用于实际调用上游 |
| 共享池 | 所有上游 key 的集合，供溢出路由复用 |
| 工作时段 | 小时额度生效的时段（9:00–12:00、14:00–18:00，本地时间） |
| 预占 | 请求开始时按估算 token 量对计数器加锁式预留 |
| 结算 | 响应结束后用真实 usage 修正计数器 |

## 3. 需求 → 设计映射

| # | 需求 | 设计落点 |
|---|---|---|
| 1 | 每小时 1000 万 token，整点刷新 | 小时窗口计数器（内存，跨小时自动重置） |
| 2 | 有人浪费、有人不够用 | 动态共享池溢出路由 |
| 3 | 9–12、14–18 有每小时限制 | 工作时段判定，小时额度仅在该时段生效 |
| 4 | 每分钟每用户 10 次调用 | 滑动窗口限流器 |
| 5 | 每天 4 亿 token | 日窗口计数器（始终生效） |
| 6 | 用户上传 key、请求经中转站、合理管理用量 | 单入口代理 + 配额引擎 + 路由 |
| 7 | Admin 配置各用户额度 | Web 后台 + Admin API |
| 8 | 记录请求量供流控与月报 | 请求明细日志 + 月度聚合 |

## 4. 关键设计决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 存储 | 纯内存 + 本地文件（无 Redis/DB） | <100 人、单实例、局域网，状态小且热；窗口清零用"跨期自动重置"替代 Redis TTL |
| 架构 | Go 单体单进程 | 部署即单二进制，Docker Compose 或裸机皆可 |
| 上游协议 | OpenAI 协议 | 与 DeepSeek、通义等国内兼容模型统一，token 计数/流式格式单一 |
| 配额复用 | 动态共享池 | 消除"浪费与不足并存" |
| 防超卖 | 预占 + 结算 | 同用户并发请求不会互相超卖 |
| token 计数 | 优先上游 usage，流式注入 `stream_options.include_usage`，缺失时 tiktoken 估算兜底 | 兼容不支持 usage 的模型 |

## 5. 整体架构

```
┌────────────┐   OpenAI 协议（/v1/chat/completions 等）
│  客户端      │───────┐  Bearer: 用户的中转 key
│ (Claude Code│       ▼
│  OpenAI SDK)│   ┌────────────────────────────────────────────┐
└────────────┘   │            Go 中转站（单进程）                 │
                 │  ┌──────────────┐  ┌───────────────────────┐ │
                 │  │  API 层(SSE)  │─▶│ 认证 / 鉴权            │ │
                 │  └──────────────┘  └──────────┬────────────┘ │
                 │                               ▼              │
                 │        ┌──────────────────────────────────┐  │
                 │        │  配额引擎：小时/日窗口 + 预占结算    │  │
                 │        │  限流器：每分钟10次/用户            │  │
                 │        │  路由：本人key → 共享池             │  │
                 │        └────────────────┬─────────────────┘  │
                 │                          ▼                    │
                 │        ┌──────────────────────────────────┐  │
                 │        │ 上游客户端（流式代理 + token计数）    │  │
                 │        └──────────────────────────────────┘  │
                 │  ┌─────────────────────────────────────────┐ │
                 │  │ Web 后台 / Admin API（额度、用量、报表）    │ │
                 │  └─────────────────────────────────────────┘ │
                 └──────────────────┬────────────────────────────┘
                                    │ 落盘
              ┌─────────────────────┼──────────────────────┐
              ▼                     ▼                      ▼
    data/state.json          logs/requests-YYYY-MM-DD.jsonl  config.yaml
  用户/key/配额/计数           每请求一行（月报数据源）        端口/时段/管理员
```

### 5.1 组件清单

| 组件 | 职责 |
|---|---|
| `api/relay` | OpenAI 兼容端点：`POST /v1/chat/completions`、`POST /v1/completions`、`GET /v1/models`；透传非流式 JSON 与流式 SSE |
| `auth` | 校验中转 key → 用户；Admin 登录（会话）；校验权限 |
| `ratelimit` | 每用户每分钟 10 次的滑动窗口 |
| `quota` | 小时/日窗口计数器；工作时段判定；预占与结算；原子性保证 |
| `router` | 从"本人 key + 共享池"中按规则选出最优上游 key |
| `upstream` | 上游 HTTP 客户端；SSE 流式代理（支持客户端断连取消）；失败重试 |
| `tokenize` | tiktoken-go 封装，估算 prompt/completion tokens |
| `report` | 读取日志文件，按用户/模型/天聚合，产出月报 |
| `store` | 内存状态（用户/key/配额/计数器），变更防抖落盘 `state.json` |
| `api/admin` + `web/` | 管理后台：用户管理、配额配置、key 管理、用量图表、月报导出 |

## 6. 内存状态模型与持久化

### 6.1 内存状态（`sync.RWMutex` 保护全局 map，细粒度锁见 §7.5）

```
users           map[userID]*User           // 用户信息
accessKeys      map[accessKey]userID       // 中转 key → 用户
upstreamKeys    map[keyID]*UpstreamKey     // 上游 key（加密存储）
counters        map[userID]*WindowCounter  // 用户小时/日用量
keyHourCounters map[keyID]*WindowCounter   // key 小时用量（供路由判定）
rateState       map[userID]*SlidingWindow  // 每分钟调用时间戳
routeType       per-request                // own | pool
```

`WindowCounter`：`{windowStart time.Time, value int64, mu sync.Mutex}`。访问时若 `now - windowStart ≥ windowLen` 则重置为 0 并更新 `windowStart`。小时窗口跨整点重置，日窗口跨本地零点重置——**无需定时任务**。

### 6.2 持久化

| 文件 | 内容 | 策略 |
|---|---|---|
| `config.yaml` | 服务端口、工作时段、默认配额、管理员账号、加密密钥来源、日志目录 | 启动时读取，admin 手工编辑后重启生效 |
| `data/state.json` | 用户、中转 key、上游 key（AES-GCM 加密）、配额、计数器 | 变更防抖 2s 落盘；收到 SIGTERM/SIGINT 时立即落盘；启动时加载 |
| `logs/requests-YYYY-MM-DD.jsonl` | 每请求一行 JSON | 追加写，按天分文件，供月报聚合 |

**权衡声明**：进程重启后内存中的小时/日计数器归零，可能短暂突破额度；用户/key/配额配置不丢。对轻量局域网工具可接受。计数器可随 `state.json` 一起落盘作为缓解（见 §14 扩展）。

## 7. 配额引擎与路由（核心）

### 7.1 三道闸门

```
请求进入
  ├─ 闸门1 每分钟限流：10次/min/用户 → 超限 429 + Retry-After
  ├─ 闸门2 日额度：4亿/天/用户，始终生效 → 超限 429
  ├─ 闸门3 小时额度：1000万/小时/用户，仅工作时段生效 → 超限 429
  └─ 选 key 路由
```

### 7.2 工作时段判定

`isWorkingHour(t)`：本地时间 `9 ≤ hour < 12` 或 `14 ≤ hour < 18`。
- 工作时段内：小时额度 + 日额度同时检查。
- 时段外：仅检查日额度，小时额度不拦截（但小时用量仍计入供工作时段首小时参考/报表）。

### 7.3 选 key 路由（动态共享池）

```
1. 优先本人 key：本人 key 中"本小时用量 < 小时额度"者，选用量最少的
2. 次选共享池：其他用户 key 中同样小时有余量者，选用量最少；用量相同取在途并发最少
3. 均不可用 → 429："本小时额度已用尽，HH:MM 后重试"
```

- key 的小时用量以 `keyHourCounters` 为准；工作时段外小时额度不生效，路由仍优先本人 key。
- 路由结果记录 `route_type: own | pool`，写入请求日志。

### 7.4 预占与结算（防超卖）

1. **预占**：请求开始时估算 `promptTokens + estCompletionTokens`（§8），持用户锁 + key 锁，原子加到用户小时/日计数器和 key 小时计数器。
2. **转发**：请求上游。
3. **结算**：响应结束（或失败）后：
   - 成功：取真实 usage，将差值 `(真实 - 预占)` 加到计数器；若真实 < 预占，差值可为负。
   - 失败：将预占值从计数器扣回，释放额度。

同一用户同一小时的并发请求因全程持锁，不会互相超卖。

### 7.5 并发与锁顺序

- 预占/结算使用**用户锁 → key 锁**的一致顺序，避免死锁。
- 读路径（窗口检查）用 RLock，写路径（预占/结算）用 Lock。
- 单进程内所有状态在锁保护下变更，无跨进程一致性问题。

## 8. Token 计数

| 场景 | 策略 |
|---|---|
| 非流式 | 读取上游 JSON 响应的 `usage` 字段，精确 |
| 流式 + 上游支持 | 代理时自动注入 `stream_options: {"include_usage": true}`，取末个 chunk 的 `usage`，精确 |
| 流式 + 上游不支持 usage | 提示 tokens 请求前用 tiktoken 精确估算；completion tokens 按流式 chunk 增量估算 |
| 预占估算 | `promptTokens`（tiktoken） + `estCompletion`（取请求体 `max_tokens`/`n` 折减，缺省按模型默认，如 4096；存在 `max_tokens` 则取 `min(max_tokens×n, 上限)`） |

- 上游不认 `stream_options` 时静默忽略，落到估算兜底。
- 计数结果用于：用户小时/日额度、key 小时用量（路由依据）、请求日志（报表）。

## 9. API 设计

### 9.1 代理端点（OpenAI 协议，透传）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | 流式/非流式 |
| POST | `/v1/completions` | 流式/非流式 |
| GET | `/v1/models` | 返回模型列表（聚合上游） |
| POST | `/v1/embeddings` | 可选，实现同前 |

- 认证：`Authorization: Bearer <中转 key>`。
- 请求体校验：`model` 必填；`stream` 控制流式。
- 响应：非流式透传 JSON；流式透传 SSE，实时转发。

### 9.2 Admin API

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/admin/login` | 管理员登录，返回会话 |
| GET/POST | `/admin/users` | 用户增删查 |
| PUT | `/admin/users/{id}/quota` | 配置用户小时/日额度（覆盖默认值） |
| GET/POST/DELETE | `/admin/keys/{userID}` | 用户上游 key 管理 |
| GET | `/admin/keys` | 共享池 key 状态（本小时用量/在途） |
| GET | `/admin/usage/user/{id}` | 实时用量 |
| GET | `/admin/reports/monthly?month=2026-08` | 月度报表（聚合日志） |
| GET | `/admin/reports/user/{id}?month=2026-08` | 单用户月度明细 |

### 9.3 用户自助

- Web 端登录后上传/更换自己的上游 key、查看本人用量与报表。

## 10. 请求日志与月报

### 10.1 日志字段（JSONL，每行一条）

```json
{
  "ts": "2026-08-04T09:01:23+08:00",
  "user_id": "u_abc",
  "access_key": "sk-relay-xxx",
  "request_id": "req_xxx",
  "model": "deepseek-chat",
  "stream": true,
  "prompt_tokens": 1200,
  "completion_tokens": 345,
  "total_tokens": 1545,
  "upstream_key_id": "k_xyz",
  "route_type": "own",
  "status": 200,
  "error": "",
  "latency_ms": 812,
  "client_ip": "192.168.1.10"
}
```

### 10.2 月度报表

- `report` 读取当月所有 `logs/requests-*.jsonl`，聚合维度：用户 × 模型 × 日。
- 指标：请求数、prompt/completion/total tokens、错误数、平均/最大延迟。
- 结果在内存按 `(month)` 缓存，有新日志写入时失效。
- 供流控精细化管理与月度分析；可按需导出 CSV。

## 11. 错误处理

| 场景 | 返回 |
|---|---|
| 中转 key 无效 | 401 |
| 无权限调用 Admin API | 401/403 |
| 每分钟超限 | 429 + `Retry-After: <秒>` |
| 日额度用尽 | 429 + `"daily quota exceeded"` |
| 小时额度用尽且池内无余量 | 429 + `"hourly quota exhausted, retry after HH:MM"` |
| 上游 429（key 级限流）/5xx | 换一个合格 key 重试一次（最大 2 次尝试）；仍失败则透传上游错误 |
| 上游超时 | 透传 504；预占回滚 |
| 客户端断连（流式） | 取消上游请求，结算预占 |

## 12. 配置（config.yaml 示例）

```yaml
server:
  addr: "0.0.0.0:8080"
  timezone: "Asia/Shanghai"
  working_hours:             # 小时额度生效时段（本地时间）
    - {start: 9, end: 12}
    - {start: 14, end: 18}

quota:
  default_hourly_tokens: 10000000    # 默认每小时 1000 万
  default_daily_tokens: 400000000    # 默认每天 4 亿
  per_minute_requests: 10            # 每分钟调用上限

security:
  encrypt_key_env: "RELAY_ENCRYPT_KEY"       # 上游 key 的 AES-GCM 加密密钥（环境变量）
  admin_username: "admin"
  admin_password_env: "RELAY_ADMIN_PASSWORD" # 管理员密码（环境变量）

logging:
  dir: "logs"
```

## 13. 目录结构（Go）

```
cmd/relay/main.go               # 入口：加载配置/状态、装配模块、启动 HTTP
internal/config/config.go       # config.yaml 解析
internal/model/model.go         # 领域结构体
internal/store/state.go         # 内存状态 + 防抖落盘/加载
internal/auth/auth.go           # 中转 key 校验、Admin 会话
internal/ratelimit/ratelimit.go # 每分钟滑动窗口
internal/quota/quota.go         # 窗口计数器、工作时段、预占/结算
internal/router/router.go       # 选 key 路由
internal/upstream/upstream.go   # 上游客户端、SSE 代理、失败重试
internal/tokenize/tokenize.go   # tiktoken 封装
internal/api/relay.go           # OpenAI 兼容端点
internal/api/admin.go           # Admin API + 月报
internal/api/middleware.go      # 认证/日志/请求ID 中间件
internal/report/report.go       # 日志聚合、月报
web/                            # Vue 前端（go:embed 内嵌）
```

## 14. 测试策略

| 层级 | 覆盖 |
|---|---|
| 单元 | 窗口计数器跨期重置、工作时段判定、key 合格性/路由选择、tiktoken 估算 |
| 集成（httptest 假上游） | 配额拦截、共享池溢出、限流 429、流式透传 + usage 计数、上游失败重试、断连取消 |
| E2E | 起真服务 + 假上游，模拟 Claude Code/OpenAI SDK 客户端调用全链路 |
| 报表 | 构造若干天日志 → 校验月报聚合数值 |

## 15. 未来扩展（仅记录，不在本版本范围）

- 规模上升：将计数器/日志迁移到 Redis/Postgres（§6 的接口已隔离）。
- 实时用量推送（SSE/WebSocket 到后台）。
- 更细粒度的流控（模型级/时段级配额，成本告警）。
- 多供应商聚合（Anthropic 等）。
