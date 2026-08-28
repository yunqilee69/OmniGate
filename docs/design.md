# OmniGate 设计文档

> 本地大模型代理：聚合多个模型提供商，提供加权负载均衡、密钥轮询、阶梯熔断与多维统计。
> 单进程 + 单二进制 + SQLite，零外部依赖，默认不记录任何请求内容。

- 版本：v1.0（设计定稿）
- 日期：2026-08-27
- 技术栈：Go + SQLite（纯 Go 驱动）+ GORM + React 18 / Ant Design 5（go:embed 内嵌）

---

## 1. 目标与非目标

### 1.1 目标

| # | 能力 | 说明 |
|---|------|------|
| G1 | OpenAI 兼容代理 | 对下游暴露 `/v1/chat/completions`（含 SSE 流式）、`/v1/models` |
| G2 | 逻辑模型路由 | 请求一个逻辑 modelId（如 `glm`），按权重分发到 N 个真实模型（可以是不同模型） |
| G3 | 提供商/密钥/模型实体 | Provider → ApiKey；模型与密钥多对多绑定（须同提供商）；模型内 key 轮询 |
| G4 | 阶梯熔断 | 模型级：30s → 1m → 3m，连续 3 次禁用并明确报错；key 级：401/403 立即禁用，429 短冷却 |
| G5 | 多维统计 | 次数 / token / 首字延迟 / 总耗时 / 费用，按 路由·模型·提供商·key·状态·时间 聚合 |
| G6 | 隐私默认 | 默认仅记录元数据；请求内容捕获为显式开关（默认关，全局 + 按路由两级） |
| G7 | Web 管理界面 | 实体 CRUD、健康状态、手动解禁、统计报表、请求日志查询 |
| G8 | 分层配置 + 热生效 | 启动层（监听地址、管理鉴权）在本地 config.yaml/命令行；运行层（熔断、限流、捕获等）全存 SQLite，管理面保存即生效 |

### 1.2 非目标（v1 明确不做）

- 多实例集群 / 高可用（单机单进程）
- 用户体系、配额计费（仅一个可选的管理 token）
- 协议转换仅覆盖 chat/completions（openai ↔ responses ↔ anthropic，见 `model.protocol`）；embeddings 等其余端点不做转换
- 按用户/按 key 的限流

---

## 2. 总体架构

```
客户端（OpenAI SDK / 任意工具）
   │  OpenAI 兼容协议
   ▼
┌─────────────────────────── OmniGate 单进程 ───────────────────────────┐
│                                                                          │
│  代理面                     核心引擎                    管理面            │
│  ┌────────────┐   ┌──────────────────────────┐   ┌────────────────────┐  │
│  │ /v1/chat/  │──▶│ 路由解析(逻辑modelId)      │   │ Admin REST API     │  │
│  │ completions│   │  → 加权选模型目标          │   │  实体CRUD/统计/健康 │  │
│  │ /v1/models │   │  → key轮询(按模型)        │   └─────────┬──────────┘  │
│  └────────────┘   │  → 熔断态key跳过          │             │ 写库后事件    │
│        │          │  → 转发+SSE透传           │             ▼             │
│        ▼          │  → 失败转移重试            │   ┌────────────────────┐  │
│  ┌────────────┐   └──────────┬───────────────┘   │ 配置快照原子替换     │  │
│  │ 统计记录器  │◀─────────────┘  生命周期埋点      │ (RWMutex/atomic)   │  │
│  └─────┬──────┘                                  └────────────────────┘  │
│        ▼                                            │ 读快照              │
│  ┌────────────────────────────────────┐             ▼                     │
│  │ SQLite (modernc.org/sqlite 纯Go)   │◀── 熔断状态机(模型级+key级)       │
│  │ 实体/配置/请求日志/(可选内容日志)    │                                   │
│  └────────────────────────────────────┘                                   │
│  Web UI（React+AntD，go:embed 内嵌，随二进制分发）                            │
└──────────────────────────────────────────────────────────────────────────┘
          │                          │
          ▼                          ▼
   提供商 A（智谱）              提供商 B（任意 OpenAI 兼容）
   ├─ 密钥[key1..keyN]           ├─ 密钥[key1..keyM]
   └─ ...                        └─ ...
```

---

## 3. 数据模型

### 3.1 实体关系

```
Provider(提供商) 1──N ApiKey(密钥)
Provider 1──N Model(真实模型，熔断状态挂在这一层)
Model M──N ApiKey             ← model_key 关联表（须同提供商）
Route(逻辑modelId) 1──N RouteTarget ──N──1 Model（带 weight）
```

> **熔断状态在 Model（物理模型）而非 RouteTarget 上**：模型挂了是对所有路由的物理事实，
> 与 one-api/new-api 把健康状态放渠道层同理。同一模型在多条路由中共享熔断状态。

### 3.2 建表 DDL（SQLite）

```sql
-- 提供商
CREATE TABLE provider (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  name        TEXT NOT NULL UNIQUE,
  base_url    TEXT NOT NULL,                      -- 如 https://open.bigmodel.cn/api/paas/v4
  protocol    TEXT NOT NULL DEFAULT 'openai',     -- 预留：openai | anthropic
  timeout_ms  INTEGER NOT NULL DEFAULT 120000,    -- 单次尝试的"首字节超时"
  remark      TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);

-- 密钥
CREATE TABLE api_key (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  provider_id    INTEGER NOT NULL REFERENCES provider(id) ON DELETE CASCADE,
  key_value      TEXT NOT NULL,                   -- 明文存储（本地工具），UI 脱敏展示 + 显式明文查看按钮
  name           TEXT NOT NULL DEFAULT '',         -- 名称（新增必填、同提供商内唯一）：日志/统计/模型绑定处的回显标识
  status         TEXT NOT NULL DEFAULT 'active',  -- active | cooldown | disabled
  cooldown_until INTEGER NOT NULL DEFAULT 0,      -- 429 短冷却截止时间戳
  rate_limited_count INTEGER NOT NULL DEFAULT 0,  -- 429 限流次数（统计用）
  disable_reason TEXT NOT NULL DEFAULT '',        -- 401/403 禁用原因
  last_used_at   INTEGER NOT NULL DEFAULT 0,
  last_error     TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

-- 真实模型（熔断状态机在这一层；protocol 决定上游调用格式）
CREATE TABLE model (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  provider_id    INTEGER NOT NULL REFERENCES provider(id) ON DELETE CASCADE,
  name           TEXT NOT NULL,                   -- 真实模型名，如 glm-4.6
  protocol       TEXT NOT NULL DEFAULT 'openai',  -- openai(chat/completions) | responses(/responses) | anthropic(/v1/messages)
  input_price    REAL NOT NULL DEFAULT 0,         -- 每 1M prompt token 价格
  output_price   REAL NOT NULL DEFAULT 0,         -- 每 1M completion token 价格
  price_currency TEXT NOT NULL DEFAULT 'USD',     -- 价格币种：USD | CNY；计费统一折算为 USD 入库（汇率见 pricing.usd_cny）
  -- 熔断状态机（模型级，跨路由共享）
  status         TEXT NOT NULL DEFAULT 'active',  -- active | cooldown | disabled
  fail_count     INTEGER NOT NULL DEFAULT 0,      -- 连续失败次数
  cooldown_until INTEGER NOT NULL DEFAULT 0,      -- 冷却截止时间戳
  disable_reason TEXT NOT NULL DEFAULT '',
  last_error     TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  UNIQUE(provider_id, name)
);

-- 模型 × 密钥 多对多（密钥须与模型同提供商）
CREATE TABLE model_key (
  model_id INTEGER NOT NULL REFERENCES model(id) ON DELETE CASCADE,
  key_id   INTEGER NOT NULL REFERENCES api_key(id) ON DELETE CASCADE,
  PRIMARY KEY (model_id, key_id)
);

-- 逻辑路由
CREATE TABLE route (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  name       TEXT NOT NULL UNIQUE,                -- 逻辑 modelId，如 glm
  remark     TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);

-- 路由目标
CREATE TABLE route_target (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  route_id INTEGER NOT NULL REFERENCES route(id) ON DELETE CASCADE,
  model_id INTEGER NOT NULL REFERENCES model(id) ON DELETE CASCADE,
  weight   INTEGER NOT NULL DEFAULT 1,
  UNIQUE(route_id, model_id)
);

-- 运行配置（key-value，全热生效；见 §9 配置清单）
CREATE TABLE app_config (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL                             -- JSON 编码
);

-- 请求日志（统计事实表，只增不改；无任何请求内容字段）
CREATE TABLE request_log (
  id                 INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id         TEXT NOT NULL,
  route              TEXT NOT NULL,               -- 逻辑 modelId
  model              TEXT NOT NULL,               -- 真实模型名
  provider           TEXT NOT NULL,
  key_id             INTEGER NOT NULL DEFAULT 0,  -- 命中的密钥 id（脱敏，不存 key 值）
  status             TEXT NOT NULL,               -- success | error | client_error
  error_code         TEXT NOT NULL DEFAULT '',    -- http 状态码 / timeout / conn / all_backends
  error_body         TEXT NOT NULL DEFAULT '',    -- 上游错误体截断（≤2KB），诊断用；不含请求正文
  is_stream          INTEGER NOT NULL DEFAULT 0,
  prompt_tokens      INTEGER NOT NULL DEFAULT 0,
  completion_tokens  INTEGER NOT NULL DEFAULT 0,
  tokens_estimated   INTEGER NOT NULL DEFAULT 0,  -- 1=上游未返回 usage，为估算值
  ttft_ms            INTEGER NOT NULL DEFAULT 0,  -- 首 token 延迟（非流式=总耗时）
  total_ms           INTEGER NOT NULL DEFAULT 0,
  cost               REAL NOT NULL DEFAULT 0,     -- 按 model 价格计算
  retries            INTEGER NOT NULL DEFAULT 0,  -- 本次请求发生的目标/key 转移次数
  created_at         INTEGER NOT NULL
);
CREATE INDEX idx_rl_time       ON request_log(created_at);
CREATE INDEX idx_rl_route      ON request_log(route, created_at);
CREATE INDEX idx_rl_model      ON request_log(model, created_at);
CREATE INDEX idx_rl_provider   ON request_log(provider, created_at);
CREATE INDEX idx_rl_key        ON request_log(key_id, created_at);

-- 单次尝试日志（一次客户端请求可含多跳重试，每跳一行；最终落客户端的结果在 request_log）
CREATE TABLE request_attempt (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        TEXT NOT NULL,
  attempt           INTEGER NOT NULL,               -- 从 1 起
  route             TEXT NOT NULL,
  model             TEXT NOT NULL,
  provider          TEXT NOT NULL,
  key_id            INTEGER NOT NULL DEFAULT 0,
  status            TEXT NOT NULL,                  -- success | error | client_error
  http_status       INTEGER NOT NULL DEFAULT 0,
  error_code        TEXT NOT NULL DEFAULT '',
  error_body        TEXT NOT NULL DEFAULT '',       -- 上游错误体截断（≤2KB）
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  ttft_ms           INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL
);
CREATE INDEX idx_ra_request ON request_attempt(request_id, attempt);

-- 每日统计预聚合（写入路径同步 UPSERT；查询优先走此表，当日/特殊维度回退 request_log 现算）
CREATE TABLE request_log_daily (
  day               INTEGER NOT NULL,               -- YYYYMMDD
  route             TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  provider          TEXT NOT NULL DEFAULT '',
  status            TEXT NOT NULL DEFAULT '',
  total             INTEGER NOT NULL DEFAULT 0,
  success           INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost              REAL NOT NULL DEFAULT 0,
  retries_sum       INTEGER NOT NULL DEFAULT 0,
  ttftb0..ttftb9    INTEGER NOT NULL DEFAULT 0,      -- TTFT 10 桶直方图（桶边界见 store.TTFTBucketBounds）
  totalb0..totalb9  INTEGER NOT NULL DEFAULT 0,      -- 总耗时 10 桶直方图（桶边界见 store.TotalBucketBounds）
  updated_at        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (day, route, model, provider, status)
);

-- 内容日志（可选；全局+路由开关均开启时才写入；独立短保留期）
CREATE TABLE content_log (
  request_id    TEXT PRIMARY KEY,
  route         TEXT NOT NULL,
  request_body  TEXT NOT NULL,
  response_body TEXT NOT NULL,                    -- 非流式=完整body；流式=拼接的文本增量
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_cl_time ON content_log(created_at);
```

---

## 4. 路由与负载均衡

### 4.1 选择算法（单次尝试的决策链）

```
① 解析 route → 候选 route_target 列表
   过滤：model.status == disabled 或 处于 cooldown 的目标 → 权重临时归零
   加权随机：r = rand(Σw)，顺序累减命中
   （LiteLLM simple-shuffle 同款语义；全被过滤 → §5.4 全不可用流程）

② 命中 model → 取其绑定的候选 api_key 列表（模型与密钥须同提供商）
   过滤：disabled / cooldown 中的 key

③ 模型内 key 轮询（每模型独立原子游标）
   游标环扫一周全跳过 → 该模型视为不可用，回到①换目标
```

进行中请求持有当前配置快照（atomic.Pointer 加载的不可变视图），配置热替换不影响已开始的请求。

### 4.2 权重语义示例

路由 `glm`：

| 目标模型 | 权重 | 期望流量 |
|---|---|---|
| 智谱 / glm-4.6 | 7 | 70% |
| 智谱 / glm-4.5-flash | 3 | 30% |

glm-4.6 绑定 3 个企业 key，glm-4.5-flash 绑定 2 个轻量 key（各自模型内轮询）。
glm-4.6 的 key 全部冷却时，glm-4.6 临时不可用，流量自然全部落到 glm-4.5-flash 上。

---

## 5. 失败处理与熔断

### 5.1 失败分类

| 上游表现 | 归因层级 | 处理 | 计入熔断 |
|---|---|---|---|
| 401 / 403 | key（密钥失效） | **立即禁用该 key**，`disable_reason` 记录，UI 告警 | 否（key 已禁用，无需阶梯） |
| 429 | key（限流） | key 短冷却：优先 `Retry-After`，缺省 60s | **否** |
| 超时 / 连接错误 / 5xx | 模型（上游故障） | **阶梯熔断**（5.2） | 是 |
| 4xx 其他（400 等） | 调用方 | 不重试不转移，直接透传给客户端，记 `client_error` | 否 |

### 5.2 模型级阶梯熔断状态机

默认配置：阶梯 `30s, 1m, 3m`；禁用阈值 3（均可改，见 §9）。

```
             失败(超时/5xx/conn)                 失败                失败
  ┌──────┐ ──────────────────▶ ┌──────┐ ─────▶ ┌──────┐ ─────▶ ┌──────────┐
  │active│                     │cool30│         │cool60│         │ DISABLED │
  └──────┘ ◀─────────────────  └──────┘         └──────┘         └──────────┘
     ▲        半开探测成功          │   到期半开     │                │
     └────────────────(计数清零)────┴───探测────────┘          手动解禁(Web UI)
```

- 第 N 次连续失败 → 冷却 `ladder[min(N, len)-1]`；**连续失败 ≥ 阈值(3) → disabled**
  （默认参数下的实际序列：失败1→30s，失败2→1m，失败3→禁用；若把阈值调大，3m 档生效，末档自动重复）
- **冷却到期 = 半开**：恢复正常候选，下一次真实流量即探测；探测成功 → `fail_count` 清零回归 active；探测失败 → 阶梯继续升级
- **disabled 仅能手动恢复**：Web UI / `POST /api/models/{id}/enable`；重启不丢（状态在 DB）
- 状态转移即时落库（本地 SQLite 写放大可忽略）

### 5.3 密钥级（状态机见 §5.1，无独立阶梯）

模型可达性 = 其绑定密钥中是否存在可用 key。全部不可用 → 路由选择时跳过该模型。不落库、不告警派生态，key 状态即事实来源。

### 5.4 全部不可用（防阻断兜底）

某路由所有目标模型均 disabled/cooldown 且无可用 key 时，返回：

```json
HTTP 503
{
  "error": {
    "code": "all_backends_unavailable",
    "message": "路由 'glm' 无可用后端：glm-4.6 已禁用（连续3次超时，最近错误: dial tcp ...）；glm-4.5-flash 冷却中（剩余42s）",
    "backends": [
      {"model": "glm-4.6", "status": "disabled", "reason": "连续3次超时，最近错误: dial tcp ..."},
      {"model": "glm-4.5-flash", "status": "cooldown", "retry_after_s": 42}
    ]
  }
}
```

明确告诉调用方**哪个后端因什么不可用**，而非无限挂起或含糊报错。记一条 `error_code=all_backends` 的请求日志。

---

## 6. 请求生命周期与重试

```
收到请求
  ├─ 解析逻辑 modelId（body.model）→ 未配置 → 404 model_not_found
  ├─ 转移预算检查（max_hops 默认 3 次）
  ├─ 【尝试】选目标模型 → 选 key → 改写 body.model 为真实模型名 → 转发
  │    ├─ 首字节前失败（超时/conn/5xx/429/401/403）
  │    │     ├─ 按 §5 归因更新状态
  │    │     ├─ 429/401/403/5xx/超时 → 消耗一跳，回到【尝试】
  │    │     │    转移顺序：同模型下一个 key → 下一个目标模型
  │    │     └─ 4xx 其他 → 透传错误给客户端，结束
  │    ├─ 首字节到达（流式：首个 SSE data；非流式：响应完整）
  │    │     ├─ 记录 ttft_ms —— ⚠️ 从此不可再换后端重试
  │    │     └─ SSE 逐块透传给客户端，边转发边累计 token
  │    └─ 流中途上游断开：无法重试，记 error(stream_broken)，客户端收到已截断的流结束
  └─ 收尾：写 request_log（成功/失败均写），更新 key.last_used_at、成本计算
```

要点：

- **重试窗口 = 收到首字节之前**。一旦开始向客户端吐流，错误只能就地终结（LLM 代理标准做法）
- 每次尝试独立计时：`provider.timeout_ms` 约束"建连 + 首字节"；流式读取阶段设 300s 空闲超时兜底
- token 统计：流式请求自动注入 `stream_options.include_usage=true` 获取上游精确 usage（可按提供商关闭）；上游不给则按 字符数/4 估算并标记 `tokens_estimated=1`
- `retries` 字段记录本次请求消耗的跳数，用于报表观察后端质量

---

## 7. API 设计

### 7.1 代理面（OpenAI 兼容，无需鉴权或沿用透传）

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | 聊天补全，支持 `stream:true` SSE |
| GET  | `/v1/models` | 返回所有逻辑路由名（客户端模型列表） |

上游鉴权：代理替换 `Authorization: Bearer <选中的key>`，客户端无需带真实 key。

### 7.2 管理面（`/api/*`，按启动层鉴权配置受保护，见 §9.1）

```
# 鉴权（公开端点，引导登录页）
GET   /api/auth-info                    # {"mode":"password"|"open"}，登录页据此渲染表单
POST  /api/login                        # {username,password} → 签发 7 天滑动会话 {token,expires_at}
POST  /api/logout                       # 撤销当前 Bearer 会话
# 管理面放行凭据：Authorization: Bearer <会话令牌> | Basic base64(账号:密码)
# 代理面 /v1/*（账号密码或 api_key 任一设置即保护）：Basic base64(账号:密码) | Bearer base64(账号:密码)
#   | Bearer "账号:密码" 原文 | Bearer <api_key>，401 附 WWW-Authenticate
# api_key 是 /v1 专用凭据，刻意不可打开管理面（调用密钥泄露不等于管理权泄露）

# 实体 CRUD（均为标准 REST，保存后配置快照原子重建、即时生效）
GET/POST         /api/providers            PUT/DELETE /api/providers/{id}
GET/POST         /api/keys                 PUT/DELETE /api/keys/{id}     # 新增单个 key（名称必填、同提供商内唯一）；GET ?reveal=1 返回明文
GET/POST         /api/models               PUT/DELETE /api/models/{id}
GET/POST         /api/routes               PUT/DELETE /api/routes/{id}

# 健康与恢复
GET              /api/health               # 全量状态：模型熔断态、key 态、冷却倒计时
POST             /api/models/{id}/enable   # 手动解禁（重置 fail_count/status）
POST             /api/models/{id}/disable
POST             /api/models/{id}/test-keys  # 逐密钥并发探测：返回绑定全部 key 的成功/耗时/错误/当前状态
# 密钥启停无独立端点：PUT /api/keys/{id} 可改 name/key_value/status（回传脱敏值则不修改 key_value）

# 统计查询（三个接口均支持 &currency=USD|CNY，CNY 时费用按 pricing.usd_cny 汇率换算输出）
GET  /api/stats/overview?from=&to=            # 总次数/成功率/token/费用/平均TTFT/平均耗时/p95（优先走每日预聚合）
GET  /api/stats/timeseries?dim=&from=&to=&bucket=1h   # 时间桶聚合；points 含 avg_ttft_ms/avg_total_ms
GET  /api/stats/breakdown?dim=&from=&to=     # 按维度分组聚合（含错误率、token、费用、延迟分位）；dim: route|model|provider|key|status|error_code

# 请求日志
GET  /api/logs?route=&model=&status=&from=&to=&page=&size=
GET  /api/logs/{request_id}/content           # 内容捕获开启时查看请求/响应体

# 配置
GET/PUT  /api/settings                        # §9 全部配置项；保存即热生效

# 维护
POST /api/maintenance/cleanup                 # 立即按保留期清理过期日志；返回 {"deleted":{"request_log":N,…}}
POST /api/maintenance/clear-stats             # body {"confirm":true}；清空请求日志/尝试日志/每日统计（内容日志保留）
```

---

## 8. 统计与隐私

### 8.1 记录维度

每条 `request_log` = 一次**最终落到客户端的请求**（内部重试不单独立行，跳数记在 `retries`）。

| 维度 | 来源字段 |
|---|---|
| 时间 | created_at（支持任意时间窗、bucket 聚合） |
| 逻辑模型 | route |
| 真实模型 / 提供商 | model / provider |
| 密钥 | key_id（可算单 key 错误率、使用倾斜） |
| 成败 | status（success/error/client_error）+ error_code |
| token | prompt_tokens / completion_tokens / tokens_estimated |
| 延迟 | ttft_ms（首 token）/ total_ms |
| 费用 | cost（按 model 价格表计算，未配价格则为 0） |
| 重试 | retries |

统计查询优先走每日预聚合表 `request_log_daily`（写入路径同步 UPSERT，`day × route × model × provider × status` 粒度 + 10 桶延迟直方图，均值/p95 由桶反查）；当日增量、`error_code` 维度等 rollup 未覆盖的查询回退 `request_log` 现算（索引已按维度建好）。清空统计与保留期清理同时覆盖两类表。

### 8.2 隐私设计

- `request_log` **表结构上不存在任何请求/响应正文**——错误场景仅落 `error_code` + `error_body`（上游错误体截断至 2KB），默认物理上无法泄露
- 内容捕获需 **全局开关 且（路由未配置白名单 或 路由在白名单内）** 双重条件，写入独立 `content_log` 表
- 内容日志独立保留期（`capture.retention_days` 默认 3 天，独立于 `log.retention_days`）；保留期由后台任务每小时自动清理落实（启动 30 秒后先跑一轮），也可经 `POST /api/maintenance/cleanup` 手动触发
- key 值仅存 `api_key` 表，日志与统计中只出现 `key_id`

---

## 9. 配置清单（分层）

### 9.1 启动层 —— 本地 `config.yaml` + 命令行参数（重启生效）

启动时就要确定、运行期不变的配置放本地文件；数据库路径存在鸡生蛋问题，只能走命令行。

| 来源 | 项 | 默认值 | 说明 |
|---|---|---|---|
| 命令行 `--db` | 数据库路径 | `./data/omnigate.db` | 路径无法存进数据库自身 |
| 命令行 `--config` | 配置文件路径 | `./config.yaml` | 文件不存在时自动生成含注释的默认模板 |
| 命令行 `--listen` | 监听地址 | 空（不覆盖） | 调试用，优先级最高 |
| `config.yaml` → `server.listen` | 监听地址 | `127.0.0.1:17777` | 启动即需确定；dev 模式下 vite 独立前端跑 17778 反代到本端口 |
| `config.yaml` → `admin.username` / `admin.password` | 管理账号密码 | `""`（关闭） | Web 登录 + /api 保护；同时可作 /v1 凭据：api_key = `base64(账号:密码)`（Basic/Bearer 皆可，RFC 7617）。用户名禁冒号；重启生效 |
| `config.yaml` → `admin.api_key` | 网关调用密钥 | `""`（关闭） | /v1 专用：`Authorization: Bearer <api_key>`；不用于 Web 登录；仅设此项 = 本地免登录 + 远程带密钥 |

优先级：`--listen` > config.yaml > 内置默认值。

### 9.2 运行层 —— `app_config` 表（保存即热生效）

| key | 默认值 | 说明 |
|---|---|---|
| `breaker.cooldown_ladder` | `["30s","1m","3m"]` | 阶梯序列，末档重复 |
| `breaker.disable_threshold` | `3` | 连续失败禁用阈值 |
| `breaker.max_hops` | `3` | 单请求最大转移次数 |
| `ratelimit.key_cooldown_s` | `60` | 429 缺省冷却（有 Retry-After 时优先） |
| `stream.idle_timeout_s` | `300` | 流式读取空闲超时 |
| `stream.inject_usage` | `true` | 自动注入 include_usage 获取精确 token |
| `capture.enabled` | `false` | 全局内容捕获开关 |
| `capture.routes` | `[]` | 路由白名单（空=开启后全路由捕获） |
| `capture.retention_days` | `3` | 内容日志保留期 |
| `log.retention_days` | `0`（永久） | 请求日志保留期，0=不清理；>0 时每小时自动清理 |
| `affinity.enabled` | `false` | 会话亲和开关（同会话粘住上次成功模型，最大化上游缓存命中） |
| `affinity.header` | `X-Session-ID` | 会话 ID 请求头（未传时按消息前缀哈希自动识别会话） |
| `affinity.ttl_s` | `3600` | 亲和记忆时长（秒） |
| `pricing.usd_cny` | `7.25` | 美元兑人民币汇率；CNY 定价模型折算为 USD 计费入库，统计接口按它换算展示 |

**启动引导**：首次启动自动建表、为运行层写入全部默认值、生成默认 config.yaml 模板。

---

## 10. Web UI 页面清单（React 18 + Ant Design 5 + ECharts，go:embed 内嵌）

| 页面 | 内容 |
|---|---|
| Dashboard | 今日请求量/成功率/token/费用卡片；TTFT 与耗时趋势图；后端健康卡片（熔断态一目了然，禁用项红色+解禁按钮） |
| 配置中心 | 左右布局：左侧提供商卡片（选中高亮、编辑/删除）；右侧 Tab = 模型 / 密钥，内容随左侧选中过滤。模型表单含上游协议（openai/responses/anthropic）+ 绑定密钥多选 |
| 路由管理 | 逻辑 modelId CRUD + 目标模型加权编辑器（可视化权重占比条） |
| 统计报表 | 维度切换（路由/模型/提供商/key/状态）+ 时间范围；分位数延迟；错误码分布 |
| 请求日志 | 筛选列表；详情抽屉（元数据 + 捕获内容查看，未开启时明确提示） |
| 设置 | 全部配置项表单；危险操作（清空统计）二次确认 |

---

## 11. 技术栈与项目结构

| 组件 | 选型 | 理由 |
|---|---|---|
| 语言 | Go | 本细分领域 5 个独立项目（one-api/new-api/Bifrost/glide/BricksLLM）的共同验证 |
| HTTP | `net/http` + chi | 标准库够用，chi 提供轻量中间件 |
| ORM/存储 | GORM + modernc.org/sqlite | 纯 Go 无 CGO，交叉编译单二进制 |
| 热更新 | 写库后事件 → atomic.Pointer 快照替换 | 无锁读路径，进行中请求不受影响 |
| 协议适配 | `anthropic-sdk-go`（MIT，wire 类型+SSE 事件解析）+ `sashabaranov/go-openai`（Apache-2.0，OpenAI/Responses 类型） | 复用 SDK 类型而非手写结构体；映射表参考 one-api（MIT）；new-api（AGPL）仅作行为参考零拷贝 |
| 前端 | React 18 + Ant Design 5 + ECharts | 团队技术栈一致；产物 embed 进二进制 |
| 日志 | slog（结构化） | 标准库 |

```
OmniGate/
├── cmd/omnigate/main.go          # 启动引导：--db/--listen、建表、装配
├── internal/
│   ├── store/                     # SQLite 初始化、GORM 模型、迁移
│   ├── config/                    # app_config 读写 + 快照构建 + 热更新事件
│   ├── router/                    # 两级选择算法（模型→key）
│   ├── breaker/                   # 模型级阶梯状态机 + key 状态流转
│   ├── proxy/                     # OpenAI 兼容转发、SSE 透传、重试转移
│   ├── stats/                     # 请求日志写入、聚合查询、成本计算
│   └── api/                       # 管理面 REST handlers
├── web/                           # React 源码（构建产物 embed）
├── docs/design.md
└── go.mod
```

---

## 12. 实施里程碑

| 阶段 | 内容 | 验收标准 |
|---|---|---|
| M1 脚手架 | 项目结构、建表迁移、实体 CRUD API、配置快照机制 | curl 完成 provider/key/model/route 全链路配置，保存即生效 |
| M2 代理核心 | 两级加权选择、转发、SSE 透传、token/延迟统计落库 | 两个不同真实模型按 7:3 分流（100 次采样偏差 <10%），流式可用 |
| M3 熔断重试 | 阶梯状态机、失败转移、全不可用报错、手动恢复 | mock 上游注入 超时/429/401 验证全部路径与状态转移 |
| M4 Web UI | React+AntD 8 个页面全部完成 | 浏览器完成全部管理操作与统计查看 |
| M5 收尾 | 内容捕获、保留期清理、单元/集成测试 | 覆盖选择算法与状态机的测试通过 |

测试策略：加权选择与熔断状态机走单元测试（表驱动）；转发/重试用 httptest 模拟上游注入 429/401/5xx/超时/流断开做集成测试。
