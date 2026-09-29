# 03 API 设计

## 1. 对外接口（OpenAI 兼容，`/v1` 前缀）

客户端把 `base_url` 指向 `https://你的域名/v1`、key 填虚拟 key，即可无缝使用任何
OpenAI SDK / CherryStudio / LobeChat / Cursor 等。

### 1.1 `POST /v1/chat/completions`（核心）

请求/响应结构 100% 对齐 OpenAI 官方，支持：

- `stream: true/false`
- `tools` / `tool_choice`（function calling）
- `model` 填映射表里的 `public_name`（如 `gpt-4o`、`claude-sonnet-5`）
- `response_format`（json mode，视上游支持透传）

错误返回也用 OpenAI 风格（客户端才认）：

```json
{
  "error": {
    "message": "Incorrect API key provided",
    "type": "invalid_request_error",
    "code": "invalid_api_key"
  }
}
```

HTTP 状态码约定：

| 状态码 | 场景 |
|---|---|
| 401 | key 无效/过期/被禁用 |
| 402 | 配额不足 |
| 403 | 模型不在白名单 / IP 被限 |
| 429 | 限流（RPM/TPM 超限） |
| 502 | 上游失败且所有渠道重试耗尽 |
| 200 | 其余（流式即 SSE） |

### 1.2 `POST /v1/messages`（Claude 原生格式）

给 Anthropic SDK 用户直连用，内部走同一套路由/计费，出口可路由到任意渠道。

### 1.3 其余端点（按需逐步支持）

| 端点 | 优先级 | 说明 |
|---|---|---|
| `GET /v1/models` | P0 | 列出可用模型（客户端下拉框靠它） |
| `POST /v1/embeddings` | P1 | 向量接口 |
| `POST /v1/images/generations` | P2 | 画图，计费按次不按 token |
| `POST /v1/audio/transcriptions` | P3 | 转写（multipart 转发） |
| `POST /v1/audio/speech` | P3 | TTS |

### 1.4 `GET /v1/dashboard/billing/usage` 等

One API 兼容的用量查询接口，让 LobeChat 这类前端的"余额"页面能显示数字。低成本高感知，建议 P1。

## 2. 管理接口（go-zero api 文件生成，前缀 `/api/admin`，JWT 鉴权）

```api
syntax = "v1"

// ---- 渠道 ----
POST   /api/admin/channel              // 创建（body 带 keys[]，AES 加密入 lr_channel_key）
PUT    /api/admin/channel/:id          // 更新
DELETE /api/admin/channel/:id
GET    /api/admin/channel/list         // 分页
POST   /api/admin/channel/:id/test     // 手动探活（一般只用于禁用渠道的恢复验证）
// 渠道/映射保存后统一动作：重建 lr_ability 对应行 + 刷新内存路由快照

// ---- 模型映射 ----
POST   /api/admin/model-mapping
PUT    /api/admin/model-mapping/:id
DELETE /api/admin/model-mapping/:id
GET    /api/admin/model-mapping/list

// ---- 定价 ----
POST   /api/admin/pricing/batch        // 批量导入定价
GET    /api/admin/pricing/list

// ---- 令牌 ----
POST   /api/admin/token                // 创建，响应里带明文 key(仅此一次)
PUT    /api/admin/token/:id
DELETE /api/admin/token/:id
GET    /api/admin/token/list

// ---- 用户 ----
POST   /api/admin/user
PUT    /api/admin/user/:id             // 调配额/分组/禁用
GET    /api/admin/user/list

// ---- 日志/看板 ----
GET    /api/admin/log/list             // 筛选: user/channel/model/status/时间
GET    /api/admin/dashboard/summary    // 今日请求数/成功率/消耗/首字耗时
```

## 3. 配置文件 `etc/llm-relay.yaml`

```yaml
Name: llm-relay
Host: 0.0.0.0
Port: 8888

Mysql:
  DataSource: ${LLM_RELAY_MYSQL_DSN}

Redis:
  Host: ${LLM_RELAY_REDIS}
  Type: node

Relay:
  MasterKeyEnv: LLM_RELAY_MASTER_KEY   # 上游key AES 加密主密钥
  DefaultMaxTokens: 4096               # 客户端没传 max_tokens 时的预扣费上限
  MaxRetryTimes: 3                     # 未出首字节前的换渠道重试次数
  UpstreamTimeoutMs: 600000            # 流式整体读超时
  ConnectTimeoutMs: 10000
  HealthCheckIntervalSec: 60           # 禁用渠道的恢复探活周期(正常渠道不探测)
  AutoDisableFailCount: 3              # 被动统计连续失败N次自动禁用
  EnforceIncludeUsage: true            # OpenAI兼容上游注入 stream_options.include_usage
  BatchUpdateIntervalSec: 5            # 配额增量批量落库周期
  LogBuffer: 1024                      # 异步日志 channel 容量
  TokenCacheSec: 30
```

## 4. 中间件链（对外接口）

```
Recover → RequestID(trace_id) → TokenAuth → RateLimit → (handler 内: 预扣费 → 路由 → 转发 → 结算)
```

与主工程 gateway 的差异：这里**不接 go-zero rest 自带的 auth 中间件**，TokenAuth 手写
（要支持 401 OpenAI 错误格式 + sha256 查 Redis）。

## 5. 部署（docker-compose 骨架）

```yaml
services:
  llm-relay:
    image: llm-relay:latest
    ports: ["8888:8888"]
    environment:
      LLM_RELAY_MYSQL_DSN: user:pass@tcp(mysql:3306)/llm_relay?parseTime=true
      LLM_RELAY_REDIS: redis:6379
      LLM_RELAY_MASTER_KEY: ${RELAY_MASTER_KEY}
    depends_on: [mysql, redis]

  mysql:
    image: mysql:8
    environment: { MYSQL_DATABASE: llm_relay, MYSQL_ROOT_PASSWORD: ${MYSQL_PW} }
    volumes: ["./data/mysql:/var/lib/mysql"]

  redis:
    image: redis:7

  nginx:
    image: nginx:alpine
    ports: ["443:443"]
    # 关键: proxy_buffering off; proxy_read_timeout 600s;
```

## 6. P0 验收清单

- [ ] `curl -N` 流式对话，chunk 逐条到达（肉眼可见打字机效果）
- [ ] 客户端 Ctrl+C 后，服务端日志出现"upstream canceled"，上游连接关闭
- [ ] 无效 key → 401 OpenAI 格式错误
- [ ] 配额耗尽 → 402
- [ ] 杀掉主渠道（改错 base_url）→ 请求自动切备用渠道成功
- [ ] 并发压测同一令牌（如 `wrk -c 50`）→ 不超扣，Redis 配额与 DB flush 后对账一致
- [ ] 日志表能查到本次请求的 token 数与扣费
