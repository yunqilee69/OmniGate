# 仓库指南

## 项目概述

**OmniGate** 是用 Go 编写的 OpenAI 兼容 AI 网关。它将多个模型提供商聚合为统一端点，提供加权负载均衡、密钥轮询、熔断保护和多维统计。打包为单个约 34MB 的静态二进制文件，内嵌 React 18 管理界面，使用 SQLite 存储，零外部依赖。

**核心定位**：本地优先的模型代理，面向需要跨多个 LLM 提供商（智谱、OpenAI 兼容端点、Anthropic）路由请求的开发者，具备自动故障转移、密钥管理和隐私默认的请求日志。

**分发方式**：发布到 npm 为 `@cloudomni/omnigate`（封装平台二进制）；也提供独立的 GitHub releases 跨平台 tarball。

---

## 架构与数据流

### 高层结构

```
客户端(任意 OpenAI SDK) → OmniGate 代理 → 提供商选择 → 上游 API
                              ↓
                         SQLite 存储 ← 管理 REST API ← React UI
```

**第一级 HTTP 路由**：
- `/v1/*` — 模型调用
- `/api/*` — 管理台前端请求后端
- `/manage/*` — 管理台页面（`GET /` 302 到 `/manage/`）

**三级路由**：
1. **逻辑路由**（如 `glm`）→ 加权选择物理模型
2. **物理模型** → 模型内绑定密钥的轮询
3. **熔断器**在选择前过滤不可用的模型/密钥

### 核心模块

| 模块 | 路径 | 职责 |
|--------|------|----------------|
| **Proxy** | `internal/proxy/` | `/v1/*` 处理器、SSE 流式传输、协议适配器（OpenAI/Anthropic/Responses）、重试逻辑 |
| **Router** | `internal/router/` | 加权模型选择、密钥轮询游标、会话亲和、基于快照的路由 |
| **Breaker** | `internal/breaker/` | 熔断器状态机（模型级 30s→1m→3m 阶梯，密钥级 401/403/429 处理）|
| **Store** | `internal/store/` | GORM 模型、迁移、SQLite 访问（纯 Go `modernc.org/sqlite`，无 CGO）|
| **Config** | `internal/config/` | 两层配置：启动层（YAML）+ 运行层（SQLite，`atomic.Pointer` 热更新）|
| **API** | `internal/api/` | 管理 REST（实体 CRUD、统计聚合、健康检查）、鉴权中间件 |

### 数据流

**请求路径**：
1. 客户端 → `/v1/chat/completions`，逻辑 `model="glm"`
2. `proxy.Handler` 加载配置快照（`atomic.Pointer`）
3. `router.Selector.Pick()` → 加权模型 + 轮询密钥
4. 适配器将请求转换为上游协议（OpenAI/Anthropic/Responses）
5. 使用提供商专用 `http.Client` 转发（每提供商缓存，支持代理）
6. SSE 流式或完整响应 → 客户端
7. `breaker.Recorder` 更新熔断器状态
8. 统计写入 `request_log` + `request_attempt` 表

**配置热更新**：
- 管理 UI → REST API → SQLite `app_config` 表 → 发出事件 → `config.RuntimeManager.NotifyUpdate()` → 所有处理器原子交换快照

---

## 关键目录

```
cmd/omnigate/          # 入口：flag 解析、守护进程模式（Unix/Windows）、信号处理
internal/
  ├── api/             # 管理 REST + 代理 HTTP 处理器、CORS、鉴权中间件
  ├── breaker/         # 熔断器状态机（模型 + 密钥级别）
  ├── config/          # 启动层（YAML）+ 运行层配置（SQLite 快照）
  ├── proxy/           # 核心转发：协议适配器、SSE、重试、费用计算
  ├── router/          # 两级选择：加权 + 轮询、亲和缓存
  ├── store/           # GORM 模型（Provider、Model、ApiKey、Route、RequestLog 等）
  └── webui/           # go:embed 封装 React 构建产物
web/                   # React 18 + Ant Design 5 + ECharts 前端
  ├── src/
  │   ├── pages/       # Dashboard、Stats、Routes、Models、Keys、Logs、Settings
  │   ├── components/  # StatusTag、Chart、ModelTestModal、CurrencyToggle
  │   └── utils/       # format.ts（货币、字节、时长）
  └── vite.config.ts   # 构建到 internal/webui/dist/，代理 /api、/v1 到 :17777
docs/
  ├── design.md        # 实体模型、熔断器 FSM、重试策略、schema DDL
  ├── protocol-conversion.md  # OpenAI ↔ Anthropic ↔ Responses 字段映射
  └── RELEASE.md       # goreleaser 工作流、npm 打包策略
npm/                   # npm 封装包，平台专用 postinstall
```

---

## 开发命令

### 前置要求

- **Go 1.27+**（使用 Go 1.27 特性；`go.mod` 声明 `go 1.27.0`）
- **Node 18+**（仅用于 web 构建）
- **无配置的 linter**（不存在 golangci-lint；依赖 `go vet`）

### 构建

```bash
# 完整构建（后端 + 内嵌前端）
go build -o omnigate ./cmd/omnigate

# 仅后端（复用已有 web/dist）
go build -o omnigate ./cmd/omnigate

# 仅前端（输出到 internal/webui/dist/）
cd web && npm ci && npm run build
```

### 开发模式

```bash
# 热重载：后端 :17777 + Vite 开发服务器 :17778
./start.sh

# 生产模式：后端 :17777 内嵌前端
./start.sh --prod
```

**`start.sh` 行为**：
- 构建后端，启动在 `:17777`
- 启动 Vite 开发服务器在 `:17778`，管理台在 `/manage`，代理后端 `/api` 和 `/v1`
- 两者日志输出到 `logs/backend.log` 和 `logs/frontend.log`
- `Ctrl+C` 杀死整个进程树
- 使用本地 `./data/omnigate.db` 和 `./config.yaml`

### 测试

```bash
# 运行所有测试
go test ./...

# 特定包
go test ./internal/router

# 带覆盖率
go test -cover ./...
```

**测试模式**：
- 表驱动测试配合 `t.Run(name, func(t *testing.T))`
- 辅助函数 `t.TempDir()` 为每个测试创建 SQLite 数据库
- `httptest.NewServer()` 模拟上游提供商
- 种子函数返回 fixture ID：`seed(t, st) (routeName string)`

### Lint / 格式化

**无配置的 linter**。使用内置 Go 工具：

```bash
go fmt ./...
go vet ./...
```

### 本地运行

```bash
# 前台模式（日志到 stdout，Ctrl+C 停止）
./omnigate start --foreground

# 后台守护进程（默认）
./omnigate start
./omnigate status
./omnigate stop

# 自定义 config/db/log 路径
./omnigate start --config ./my-config.yaml --db ./data/test.db --log stdout
```

**默认路径**：`~/.omnigate/`（首次运行自动创建）
- `omnigate.db` – SQLite 数据库
- `config.yaml` – 启动层配置（监听地址、管理鉴权）
- `omnigate.log` – 结构化日志（slog）
- `omnigate.pid` – 守护进程 PID 文件

---

## 代码约定与常见模式

### 语言与格式化

- **Go 1.27** 标准库风格
- 4 空格制表符（Go 默认）
- import 分组：stdlib → 第三方 → internal
- **错误处理**：显式 `if err != nil` 检查，用 `fmt.Errorf("context: %w", err)` 包装
- **日志**：`log/slog` 结构化日志（`slog.Info`、`slog.Error` 配合键值对）

### 命名

- **包名**：小写，尽可能单词（`proxy`、`router`、`breaker`）
- **类型**：PascalCase（`Provider`、`ApiKey`、`RouteTarget`）
- **函数**：camelCase，导出以大写开头
- **常量**：PascalCase 或 SCREAMING_SNAKE_CASE（包级常量）
- **数据库表**：snake_case（GORM 自动转换或使用 `TableName()` 覆盖）

### 错误处理

- **客户端错误（4xx）**：返回 `writeJSON(w, http.StatusBadRequest, errResp{...})`
- **服务器错误（5xx）**：用 `slog.Error` 记录，返回通用消息给客户端
- **上游错误**：捕获在 `error_body` 字段（截断至 2KB），不记录到 stdout

### 异步模式

- **并发**：最小化（单进程设计）
- **原子操作**：`atomic.Pointer[T]` 用于热配置重载
- **互斥锁**：`sync.RWMutex` 用于熔断器状态，`sync.Mutex` 用于路由亲和映射
- **Context**：`context.Context` 贯穿 HTTP 处理器，SSE 流中尊重取消

### 状态管理

**两层配置**：
1. **启动层**（`config.yaml`）：启动时读取一次，更改需要重启
2. **运行层**（`app_config` SQLite 表）：通过 `atomic.Pointer[Snapshot]` 热重载

**熔断器状态**：存储在 SQLite（`model.status`、`model.cooldown_until`、`api_key.status`），每次请求通过快照检查

**请求隔离**：每个请求在开始时加载不可变配置快照，不受并发配置更改影响

### 数据库约定

- **GORM 模型**在 `internal/store/models.go`
- **迁移**：启动时自动迁移（`store.Open()`）
- **时间**：Unix 时间戳（`int64`），GORM 标签 `autoCreateTime`、`autoUpdateTime`
- **级联删除**：外键使用 `ON DELETE CASCADE`
- **索引**：在迁移注释中显式 `CREATE INDEX`（design.md §3.2）
- **事务**：多表写入使用显式 `tx := db.Begin()`

### 测试约定

- **表驱动测试**：`tests := []struct{ name, input, want }{...}`
- **辅助函数**：`newStore(t) *store.Store` 创建临时 SQLite，注册 `t.Cleanup()`
- **断言**：`if got != want { t.Errorf(...) }`
- **无测试框架**：纯 `testing` 包，无 testify/ginkgo
- **模拟上游**：`httptest.NewServer()` 配合自定义处理器

---

## 重要文件

### 入口点

- `cmd/omnigate/main.go` – CLI 入口、守护进程生命周期（`start`、`stop`、`status`）、信号处理
- `web/src/main.tsx` – React 应用入口、React Router 设置

### 核心配置

- `config.yaml.example` – 启动层配置模板（监听地址、管理鉴权）
- `internal/config/runtime.go` – 运行层配置 schema + 原子快照管理器
- `internal/config/bootstrap.go` – YAML 解析、环境变量覆盖、默认值

### 关键模块

- `internal/proxy/proxy.go` – 主代理处理器、请求生命周期
- `internal/proxy/adapter.go` – 协议转换（OpenAI ↔ Anthropic ↔ Responses）
- `internal/router/router.go` – 加权选择 + 轮询、快照加载
- `internal/breaker/breaker.go` – 熔断器 FSM、冷却阶梯
- `internal/store/store.go` – SQLite 连接、迁移、清理任务
- `internal/api/api.go` – 管理 REST 服务器、实体 CRUD、统计聚合

### 构建与发布

- `.goreleaser.yaml` – 跨平台二进制构建（linux/darwin/windows × amd64/arm64）
- `npm/package.json` – npm 封装包，平台专用 `postinstall` 脚本
- `.github/workflows/release.yaml` – GitHub Actions CI/CD

---

## 运行时/工具链偏好

### 运行时要求

- **Go 1.27+**（必需；使用 Go 1.27 stdlib 特性）
- **CGO 禁用**（goreleaser 中 `CGO_ENABLED=0`）– 纯 Go SQLite 驱动
- **单二进制**：无运行时依赖，所有资源通过 `go:embed` 内嵌

### 包管理器

- **后端**：Go modules（`go.mod`、`go.sum`）
- **前端**：npm（`web/package-lock.json`，npm ci 确保可重现构建）
- **未使用 pnpm/yarn/bun**

### 数据库

- **SQLite** 通过 `modernc.org/sqlite`（纯 Go，无 CGO）
- **GORM** ORM 配合自动迁移
- **WAL 模式**：默认启用以支持并发

### HTTP

- **路由器**：`github.com/go-chi/chi/v5`（轻量级，stdlib 兼容）
- **客户端**：stdlib `net/http` 配合连接池
- **SSE**：手工实现 `data: ...\n\n` 流式传输，`text/event-stream` 内容类型

### 前端

- **React 18** + **Ant Design 5** + **@ant-design/x 1.x**（对话组件 Bubble）+ **@ant-design/x-markdown**（流式 Markdown 渲染）+ **ECharts 5**
- **Playground 对话输出**：`Bubble`（`variant="outlined"`）内嵌 `XMarkdown`，流式期间传 `streaming={{ hasNextChunk: true, tail: true }}`，结束后传 `undefined` 完成整体渲染；注意 @ant-design/x 2.x 要求 antd 6，本仓库锁定 1.x
- **TypeScript 5.5**（strict 模式）
- **构建目标**：ES2020，输出到 `internal/webui/dist/`

### 工具链约束

- **无配置的 linter**：依赖 `go vet` 和编辑器集成
- **无 pre-commit 钩子**：提交前手动 `go fmt`
- **无 Docker**：单二进制分发模式
- **无 Makefile**：开发用 `start.sh`，生产用 `go build`

---

## 测试与 QA

### 测试框架

- **纯 `testing` 包**（无 testify、ginkgo 或其他框架）
- **表驱动测试**用于多用例逻辑
- **`httptest`** 用于 HTTP 处理器和上游模拟

### 运行测试

```bash
# 所有测试
go test ./...

# 特定包
go test ./internal/router -v

# 带覆盖率
go test -cover ./internal/...

# 竞态检测器（耗时较长）
go test -race ./...
```

### 测试组织

- **同位置**：`*_test.go` 文件与生产代码并列
- **包级别**：测试在同一包（白盒），或 `_test` 后缀包（黑盒）
- **Fixtures**：`internal/store/models.go` 定义种子辅助函数如 `seed(t, st)`

### 覆盖率预期

- **无正式覆盖率目标**
- **关键路径覆盖**：路由选择、熔断器 FSM、协议适配器
- **集成测试**：代理重试逻辑、SSE 流式传输、上游错误处理
- **无前端测试**：React UI 依赖手工 QA

### 测试模式

```go
func TestWeightedPick(t *testing.T) {
    tests := []struct {
        name    string
        weights []int
        want    int
    }{
        {"single", []int{10}, 0},
        {"zero", []int{0, 10}, 1},
    }
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            got := weightedPick(tt.weights)
            if got != tt.want {
                t.Errorf("got %d, want %d", got, tt.want)
            }
        })
    }
}
```

### 提交前检查清单

**每次提交代码前必须完整运行一次测试用例，确保所有测试通过**：

```bash
# 必须：运行全部测试
go test ./...

# 必须：检查代码问题
go vet ./...

# 推荐：检查竞态条件（耗时较长）
go test -race ./...
```

**PR 前额外检查**：
1. ✅ **所有测试通过**：`go test ./...` 无失败
2. ✅ **代码检查通过**：`go vet ./...` 无问题
3. ✅ 构建成功：`go build -o omnigate ./cmd/omnigate`
4. ✅ 前端构建成功：`cd web && npm run build`
5. ✅ 冒烟测试：启动守护进程，访问 `/v1/models` 与 `/manage/`，检查 UI 加载

**不要提交**：
- `internal/webui/dist/*`（构建时重新生成）
- 本地 `omnigate.db`、`config.yaml`、`logs/`
- 二进制 `./omnigate`
---

## 附加说明

### 协议支持

通过协议适配器（`internal/proxy/adapter.go`）支持三种协议族：
1. **OpenAI**（`/v1/chat/completions`）– 默认，与其他协议互转
2. **Anthropic**（`/v1/messages`）– 直通到 Anthropic 模型，保留 `thinking` 块
3. **OpenAI Responses**（`/v1/responses`）– 为 o1/o3 模型保留 `reasoning_content`

详见 `docs/protocol-conversion.md` 获取字段映射细节和已知限制。

### 隐私默认

- **请求元数据始终记录**：路由、模型、token、延迟、错误码
- **请求内容记录默认关闭**：需要显式的全局 + 按路由标志
- **密钥值**：明文存储（本地工具假设），UI 中掩码，显式"显示"按钮

### 熔断器调优

默认阶梯（可在管理 UI 中配置）：
- 模型级别：30s → 1m → 3m（连续 3 次失败 → 禁用）
- 密钥级别：401/403 → 立即禁用，429 → 60s 冷却

手动覆盖：管理 UI "解禁"按钮强制模型/密钥回到活跃状态。

### 会话亲和

可选功能（默认开启）：记住会话上次成功的模型（通过请求头识别），下次请求优先使用该模型以最小化对话中的模型切换。

**检查的请求头**（可配置）：`X-Session-ID`、`X-Conversation-ID` 等。

---

## 快速参考

### 常见文件路径

| 用途 | 路径 |
|---------|------|
| 主入口 | `cmd/omnigate/main.go` |
| 代理处理器 | `internal/proxy/proxy.go` |
| 路由逻辑 | `internal/router/router.go` |
| 熔断器 | `internal/breaker/breaker.go` |
| 数据库模型 | `internal/store/models.go` |
| 管理 API | `internal/api/api.go` |
| 前端入口 | `web/src/main.tsx` |

### 常用命令

```bash
# 开发模式（热重载）
./start.sh

# 测试所有
go test ./...

# 构建发布二进制
go build -o omnigate ./cmd/omnigate

# 前端构建
cd web && npm ci && npm run build

# 格式化所有 Go 代码
go fmt ./...
```

### 环境变量

通过环境变量覆盖启动层配置：
- `SERVER_HOST` – 监听地址（默认 `127.0.0.1`）
- `SERVER_PORT` – 监听端口（默认 `17777`）
- `ADMIN_USERNAME` – 管理登录用户名
- `ADMIN_PASSWORD` – 管理登录密码
- `DATABASE_PATH` – SQLite 文件路径
- `LOG_PATH` – 日志输出路径（或 `stdout`、`stderr`、`off`）

### 实用技巧

- **调试 SSE 流**：在运行时配置中设置 `debug_stream_log: true`（记录每个 SSE 块）
- **手动数据库检查**：`sqlite3 ~/.omnigate/omnigate.db`（纯 Go 驱动与 sqlite3 CLI 兼容）
- **配置热重载测试**：在 UI 中更改运行时配置，立即影响下一个请求（无需重启）
- **用 curl 测试**：`/v1/models` 端点列出可用路由（默认配置无需鉴权）
