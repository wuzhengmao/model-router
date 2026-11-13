# 需求规格说明书（model-router）

> 本文档由产品经理（主代理）规格化编写，是需求侧的单一事实来源。
> 设计文档（`docs/design.md`）与测试用例（`docs/tests.md`）由 Claude Code 编写。
> 版本规则、分支规则、提交规则见 `AGENTS.local.md`。

## 1. 项目概述

一个**模型路由代理程序**：对外提供 OpenAI（OpenAPI）与 Anthropic 两种标准协议入口，按配置把请求**透传**给后端的多个候选模型，并根据可用性/速度自动做模型选择与降级回退。

关键原则：**不转协议，只代理透传**。提供 OpenAI 协议的 path 只能接入兼容 OpenAI 协议的模型；提供 Anthropic 协议的 path 只能接入兼容 Anthropic 协议的模型。

## 2. 功能需求（FR）

### FR-1 监听与协议入口
- 监听一个（或多个，后续扩展）指定端口。
- 提供 OpenAPI 标准协议入口（对应独立 path，如 `/v1/chat/completions`）。
- 提供 Anthropic 标准协议入口（对应独立 path，如 `/v1/messages`）。
- 两个协议互不转换，各自只对接各自兼容协议的后端模型。

### FR-2 模型发现与选择
- 能自动查询后端可用模型（如调用后端 `/v1/models`），并据此选择/过滤可用模型。
- 每种协议维护一个**有序**的模型列表，可配置排序。
- Anthropic 协议下模型可按类别分组（如 `sonnet` / `opus`），组内各自有序；回退时优先在组内、再跨组。

### FR-3 回退与降级（核心）
> 下述为规格化的状态机，默认参数见 §4。多模型"链式降级"的确切边界见 §5 开放问题 D-1。

- 模型列表有序 `[M0, M1, M2, ...]`，**主模型 = 列表首元素 M0**，默认优先使用。
- **失败判定**（任一成立即为该模型本次"失败"）：
  1. 连接失败；
  2. 超时（默认 15 秒）；
  3. 吞吐 `token/s < 阈值`（默认 30，计数窗口默认 30 秒；token 取自响应 usage）。
- **临时回退**：主模型失败时，本次请求立即改用下一层模型服务，并记录主模型失败次数 +1。
- **失败计数 < 阈值（默认 3）阶段**：每次连接仍先尝试主模型（"临时回退前的模型"）；主模型成功则失败计数清 0。
- **失败计数 ≥ 阈值**：进入**持续回退**——后续连接直接使用下一层模型（不再先试主模型），回退时长默认 5 分钟。
- **回退期结束**：下一个请求回到主模型 M0 开始尝试；此时**失败一次就回退一层**（不再累计失败阈值），回退时长重新计时。
- **恢复正常**：M0 尝试成功即恢复正常，下次失败计数从 0 开始。

### FR-4 服务访问认证（M1 增量，M2 修订）
- 代理服务对外提供**服务访问 token**：客户端（调用方）访问任意协议入口时必须携带，携带方式符合对应协议标准：
  - OpenAI 入口 `/v1/chat/completions`：`Authorization: Bearer <auth_token>`
  - Anthropic 入口 `/v1/messages`：`x-api-key: <auth_token>`
- token 由服务端配置（配置项 `auth_token`，两个入口共用同一个值）；缺失或不匹配 → 返回 `401`（错误体符合对应协议格式）。
- **上游认证与客户端认证分离**：代理转发给上游模型时，使用服务端**配置的 `api_key`**（不再透传客户端携带的 token）；客户端 token 仅用于访问本代理服务本身。

## 3. 非功能需求（NFR）

### NFR-1 技术栈
- Golang 开发，**纯 Go，不用 cgo**。
- 提供 Web 页面进行配置（后续里程碑）。

### NFR-2 平台与构建
- 支持 `windows / linux / macos` × `amd64 / arm64`。
- 一键构建脚本：构建当前平台版本；跨平台构建走 GitHub Actions。
- Docker 容器部署，支持 `amd64 / arm64`。

### NFR-3 文档
- `README.md`：项目说明。
- `CHANGELOG.md`：变更记录。
- `docs/`：需求 / 设计 / 测试用例，便于 AI 快速理解项目。

## 4. 默认参数表

| 参数 | 默认值 | 说明 |
|---|---|---|
| 服务访问 token（`auth_token`） | 无（必填） | 客户端访问代理需携带的 Bearer token（FR-4） |
| 上游 api_key（`backend.api_key`） | 无（必填） | 代理转发上游时使用（FR-4，不与客户端 token 混用） |
| 连接超时 | 15 s | FR-3 失败判定之超时 |
| token/s 阈值 | 30 | 低于此值判为"慢" |
| token/s 计数窗口 | 30 s | 吞吐统计窗口 |
| 失败阈值 | 3 次 | 触发持续回退的失败次数 |
| 回退时长 | 5 min | 持续回退的持续时间 |

> 以上参数均可通过配置覆盖（配置方式在 M1 阶段为静态配置文件，后续为 Web 页面）。

## 5. 开放问题（待 Claude Code 在设计文档中给出方案）

- **D-1 链式降级**：持续回退态中，若备用模型（M1）自身也连续失败，是否继续降级到 M2、M3…？默认倾向：是（沿列表链式下移）；需在设计文档明确状态迁移与恢复规则。
- **D-2 token/s 统计口径**：流式（SSE）响应与一次性响应分别如何统计 token 与耗时；计数窗口如何滑动。
- **D-3 并发回退一致性**：多并发请求下，失败计数、回退态、回退计时器如何共享与加锁。
- **D-4 配置热更新**：M1 静态配置文件是否支持运行时热加载（后续 Web 配置必然需要）。

## 6. 里程碑规划

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1 | 项目骨架 + 单模型透传壳子（OpenAI 协议）+ 服务访问认证（FR-4）+ 静态配置 + 构建脚本（先 windows amd64） | 已完成 |
| M2 | 增加 Anthropic 协议入口 + 多模型列表与排序（含 matches/roles 模型映射） | 已完成 |
| M3 | 模型自动查询/选择（`/v1/models`）+ Web 配置页面 | 未开始 |
| M4 | 回退/降级机制（FR-3 完整实现） | 未开始 |
| M5 | 跨平台构建（GitHub Actions）+ Docker（amd64/arm64） | 未开始 |

## 7. M2 增量规格

> 本节由产品经理（主代理）在 M2 启动前规格化，是 M2 派发与验收的需求依据。

### 7.1 配置结构：多后端有序列表

`backend`（单对象）改为 `backends`（数组，**有序，顺序即优先级**）：

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "...",
  "backends": [
    {"protocol": "openai",  "base_url": "https://api.deepseek.com",   "api_key": "sk-...",     "model": "deepseek-chat",     "timeout": "15s"},
    {"protocol": "anthropic","base_url": "https://api.anthropic.com", "api_key": "sk-ant-...", "model": "claude-sonnet-4-5", "timeout": "15s"}
  ]
}
```

校验规则（fail-closed）：

- `backends` 非空数组，否则启动失败；
- 每项 `protocol` 必填，取值 `openai` / `anthropic`（大小写不敏感，内部归一化小写），非法值启动失败；
- 每项 `base_url` / `api_key` / `model` 必填（M2 起 `model` 参与路由，改为必填）；`timeout` 可选，默认 15s；
- 新增可选字段 `matches`（§7.7 模型别名映射）与 `roles`（§7.8 模型角色映射）；不配置时保持原路由行为。
- 其余校验同 M1（`base_url` 绝对 http/https、去尾部 `/`；`api_key` 去空白非空；未知字段拒绝）。

### 7.2 路由规则（决策表）

| # | 场景 | 行为 |
|---|---|---|
| R1 | 请求到 `/v1/chat/completions` | 取 `backends` 中 `protocol=openai` 的有序子列表 L |
| R2 | 请求到 `/v1/messages` | 取 `backends` 中 `protocol=anthropic` 的有序子列表 L |
| R3 | L 为空（该协议未配置后端） | 502，错误体按入口协议格式（见 §7.4），不转发 |
| R4 | 请求体可解析出顶层 `model` 字符串，且在 L 中命中某 `backend.model` | 转发到该 backend |
| R5 | 请求体无 `model`、`model` 非字符串、或未命中 | 转发到 L[0]（列表第一个） |
| R6 | 请求体不是合法 JSON（无法解析 model） | 不阻断，按 R5 回退 L[0] 透传（让上游返回协议级错误） |
| R7 | 请求体超过上限 | 413，错误体按入口协议格式，不转发 |

- 路由只为选 backend；**转发时仍透传原始请求体字节，不改写**（不注入/覆盖 model）。
- 为实现路由，M2 起请求体读入内存（`http.MaxBytesReader`，上限 10 MiB；超限 413）。chat 请求体通常远小于此。

### 7.3 Anthropic 入口透传（`/v1/messages`）

- 路径 `/v1/messages`，上游 URL = `base_url` + `/v1/messages`（`url.JoinPath`，同 M1）。
- 上游认证头：`x-api-key: <backend.api_key>`（Anthropic 标准）；**不是** `Authorization`。
- 请求头透传：除 hop-by-hop、`Content-Length`、`Authorization`、`x-api-key` 外原样透传（含 `anthropic-version`、`anthropic-beta`、`content-type`）。
- 客户端访问本代理的认证：使用 `x-api-key: <auth_token>`（与官方 SDK 一致），**不接受** `Authorization: Bearer`。
- 请求体 / 响应体：原样透传，不转协议；SSE 流式同样逐块 flush。

### 7.4 代理侧错误体按入口协议格式

| 入口 | 错误体格式 |
|---|---|
| `/v1/chat/completions` | OpenAI：`{"error":{"message","type","code"}}`（保持 M1） |
| `/v1/messages` | Anthropic：`{"type":"error","error":{"type","message"}}` |

映射（Anthropic 入口）：

| 场景 | HTTP | `error.type` |
|---|---|---|
| 缺 `Authorization` / 错 token | 401 | `authentication_error` |
| 未知路径 | 404 | `not_found_error` |
| 无对应协议后端（R3） | 502 | `api_error` |
| 上游连接失败 / 超时 | 502 | `api_error` |
| 请求体超限（R7） | 413 | `request_too_large` |

OpenAI 入口映射保持 M1 不变（401 `invalid_request_error` + `missing_api_key`/`invalid_api_key`，404 `not_found`，502 `upstream_error`），新增 413 `invalid_request_error` + `request_too_large`。

### 7.5 明确不在 M2 范围（不实现）

- 不调用上游 `/v1/models` 自动发现（M3）。
- 不做回退/降级/失败计数/状态机（M4）。
- 不做协议转换（永远不做）。
- 不做配置热更新（M5）。
- 不做跨模型重试：单请求单后端，不隐式重试（同 D-M1-5）。

### 7.6 边界决策（规格未逐字覆盖处的已确认默认）

以下情形在 §7.1–§7.4 未逐字覆盖，产品经理确认如下（与实现一致，作为 M2 及后续里程碑的判据）：

| # | 情形 | 决策 |
|---|---|---|
| B1 | 未知路径（非两个入口）的 404 格式 | 统一 OpenAI 格式（保持 M1 向后兼容）；§7.4 表中 Anthropic 的 404 仅对 `/v1/messages` 理论上存在、实际不可达 |
| B2 | 未认证访问未知路径 | 认证优先于路由（FR-4），返回 401 而非 404 |
| B3 | 请求体读取的非超限错误（如客户端中断） | 400 + `invalid_request_error`（OpenAI `code=invalid_request`） |
| B4 | R3 与 R7 同时触发（无对应协议后端 + 超大 body） | 先判后端列表，无后端直接 502，不读 body |
| B5 | OpenAI 入口是否剔除客户端 `x-api-key` | 不剔除（保持 M1 透传）；仅 Anthropic 入口剔除 `x-api-key` |
| B6 | 同协议列表内重复 `model` | 按顺序取第一个匹配项，配置层不拒绝重复 |
| B7 | `model` 匹配大小写 | 精确匹配、区分大小写 |
| B8 | 请求体 `model` 为 `null` | 视为“无可用 model”，按 R5 回退 L[0] |
| B9 | 请求体上限语义 | 超过 10 MiB 拒绝（10 MiB 整允许，10 MiB+1 拒绝） |

### 7.7 模型别名映射（`matches` 字段，M2 增量）

为让客户端使用习惯的模型名（如 `claude-sonnet-4-5`）映射到后端真实模型（如 `deepseek-v4-flash`），`backend` 新增可选字段 `matches`（字符串数组）。

```json
{
  "protocol": "anthropic",
  "base_url": "https://api.deepseek.com/anthropic",
  "api_key": "...",
  "model": "deepseek-v4-flash",
  "matches": ["claude-sonnet-4-5", "claude-sonnet-4"],
  "timeout": "15s"
}
```

- `matches`：客户端可用模型名（别名）列表，**可选**；提供时每项非空、去首尾空白。
- 路由规则扩展（在 §7.2 基础上，按 `backends` 列表顺序取第一个命中）：
  | # | 场景 | 行为 |
  |---|---|---|
  | R4a | 请求体 `model` 精确命中某 `backend.model` | 转发到该 backend，**不改写** body |
  | R4b | 请求体 `model` 命中某 `backend.matches` 任一项 | 转发到该 backend，并把 body 顶层 `model` **改写**为该 `backend.model` |
  | R5 | 未命中（无 `model` / 非字符串 / `null` / 未命中） | 回退 L[0]，**不改写**（保持 M2 原行为） |
- 判断顺序：对每个 backend 先判 R4a（`model` 精确匹配），再判 R4b（`matches` 命中）。
- 改写范围：仅当 R4b 命中时，把 JSON 顶层 `model` 字段的值改写为 `backend.model`；其余字段与字节不动（实现可用 `map[string]json.RawMessage` 解析后仅替换 `model` 再序列化；key 顺序/空白可能与原始不同，语义不变）。
- 冲突与优先级：`matches` 内重复不拒绝；`matches` 与 `model` 同名时 R4a 优先（不改写）。

### 7.8 模型角色映射（`roles` 字段，M2 增量）

参照 Claude Code / cc-switch 的 4 个模型角色（`sonnet` / `opus` / `fable` / `haiku`），让客户端按角色名或官方模型 ID 发请求，由代理映射到后端真实模型，避免手动维护每个版本号的别名。

`backend` 新增可选字段 `roles`（字符串数组，枚举 `sonnet` / `opus` / `fable` / `haiku`）：

```json
{
  "protocol": "anthropic",
  "base_url": "https://api.deepseek.com/anthropic",
  "api_key": "...",
  "model": "deepseek-v4-flash",
  "roles": ["sonnet", "haiku"],
  "timeout": "15s"
}
```

示例中的 backend 接 `sonnet` / `haiku` 两个具体角色；若要托底，可让某个 backend 声明 `"roles": ["*"]`（也可与具体角色混用，如 `["sonnet", "*"]`）。

- `roles`：该 backend 承接的模型角色，**可选**；每项必须是 5 个枚举值之一：`sonnet` / `opus` / `fable` / `haiku`（具体角色）或 `*`（通配托底），大小写不敏感（内部归一化小写），非法值启动失败；空数组视为未配置。
- 路由规则扩展（在 §7.2 / §7.7 基础上，按 `backends` 列表顺序取第一个命中）：
  | # | 场景 | 行为 |
  |---|---|---|
  | R4c | 请求体 `model` 命中某 `backend.roles` 声明的具体角色 | 转发到该 backend，并把 body 顶层 `model` **改写**为 `backend.model` |
  | R4d | 请求体 `model` 是合法字符串、未命中任何具体角色/别名，且某 `backend.roles` 含 `*` | 转发到第一个声明 `*` 的 backend，并把 body 顶层 `model` **改写**为 `backend.model` |

- R4c 命中判定（对 `model` 字符串，区分大小写，与 B7 一致）：
  - 精确等于角色名：`sonnet` / `opus` / `fable` / `haiku`；
  - 或以 `claude-<role>-` 开头：`claude-sonnet-*` / `claude-opus-*` / `claude-fable-*` / `claude-haiku-*`（覆盖官方各版本 ID）。
- 判断顺序（对每个 backend）：R4a（`model` 精确）→ R4b（`matches` 精确）→ R4c（具体角色/前缀）→ R4d（`*` 托底）。显式别名优先于角色规则，具体角色优先于托底；R4a 命中不改写，R4b 命中按别名改写，R4c/R4d 命中按角色改写。
- 未命中任何 backend 的 `model` / `matches` / `roles`（含 `*`）时，按 R5 回退 L[0]，不改写；无 `model` / `model` 非字符串 / `null` 时也按 R5（`*` 只托底“有合法 model 字符串但匹配不到”的情况）。
- 冲突：多个 backend 声明同一角色（含 `*`）→ 按列表顺序取第一个（同 B6）；`roles` 内重复不拒绝；`roles` 与 `matches`/`model` 冲突时按判断顺序 R4a→R4b→R4c→R4d。
- 不处理（可后续扩展）：`best`（等同 opus）、`sonnet[1m]` / `opus[1m]` 后缀、`opusplan` 特殊模式；这些 `model` 值未命中时按 R5 回退 L[0]。

> 说明：`roles` 与 `matches` 是两种表达方式——`matches` 写精确别名（如 `claude-sonnet-4-5`），`roles` 写角色语义（自动覆盖所有 `claude-sonnet-*` 版本）。二者可同时使用，判断顺序保证显式别名优先。

## 8. M3 增量规格（模型自动查询/选择 + Web 配置页）

> 本节由产品经理（主代理）在 M3 启动前规格化，是 M3 派发与验收的需求依据。
> 用户已拍板三项决策：① 配置生效 = 保存文件 + 重载按钮（进程内重读，不重启）；② 管理页认证 = 独立管理员账号密码（与 auth_token 分离）；③ `/v1/models` = 只列配置声明的模型，不真正调上游。

### 8.1 配置结构新增（管理账号）

`Config` 新增两个顶层字段：

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "...",
  "admin_user": "admin",
  "admin_password": "...",
  "backends": ["..."]
}
```

- `admin_user`：管理员登录名，可选，默认 `admin`。
- `admin_password`：管理员密码，可选；**非空则启用 Web 管理页并强制认证，空则管理页禁用**（`/admin` 与 `/api/*` 返回 404，不暴露管理面）。
- 校验：`admin_user` 省略置为 `admin`；`admin_password` 仅 trim 空格，不设长度下限。
- 向后兼容：现有仅含 `listen`/`auth_token`/`backends` 的配置，缺省 `admin_password` 时管理页禁用、模型入口照常工作，**不启动失败**。

### 8.2 `/v1/models` 端点

- 路径 `GET /v1/models`，按入口协议返回对应标准格式（**本地生成，不转发上游**）：
  - OpenAI 入口（认证 `Authorization: Bearer <auth_token>`）→ `{"object":"list","data":[{"id":<model>,"object":"model","created":<unix秒>,"owned_by":"model-router"}]}`
  - Anthropic 入口（认证 `x-api-key: <auth_token>`）→ `{"data":[{"id":<model>,"type":"model","display_name":<model>,"created_at":"<RFC3339>"}],"has_more":false,"first_id":<首项id>,"last_id":<末项id>}`
- 模型清单（按入口协议过滤 backend 后聚合）：对每个匹配协议的 backend 依次收集 ① `backend.model` ② `matches` 每项 ③ `roles` 中具体角色（`sonnet`/`opus`/`fable`/`haiku`，**不含 `*`**）；按出现顺序去重。
- 认证失败：返回对应协议格式的 401（复用现有错误体逻辑，格式由入口路径决定）。
- 方法限定 `GET`；其他方法（POST 等）→ 405。
- 不调用上游 `/v1/models`（用户拍板）。

### 8.3 Web 配置页（`/admin`）

- 内嵌静态页面（`go:embed`），纯 Go 无 cgo，**不依赖外部 CDN**（离线可用）。
- `GET /admin`（或 `/admin/`）→ 返回管理页 HTML；页面用原生 HTML+JS（fetch），不引入前端构建工具链。
- 页面功能：
  1. 展示当前生效配置：`listen`、`auth_token`、`admin_user`、`admin_password`、`backends` 数组（每项 protocol/base_url/api_key/model/timeout/matches/roles）。
  2. 编辑配置：增/删/改 backend，编辑 auth_token 与 admin 账号密码。
  3. 「保存」→ `PUT /api/config` 写回文件（不生效）。
  4. 「重载」→ `POST /api/reload` 重读文件并生效。
- 页面静态资源（HTML/JS/CSS）由 Claude Code 编写，随二进制内嵌。

### 8.4 管理 REST API

- 全部走 **HTTP Basic Auth**（`admin_user`:`admin_password`），失败返回 401 + `WWW-Authenticate: Basic realm="model-router"`；`admin_password` 为空时这些路径 404。
- `GET /api/config` → 返回**当前生效**配置的完整 JSON（含真实 `api_key`/`auth_token`，已认证管理员可见，用于编辑回填）。
- `PUT /api/config` → 接收完整新配置 JSON，**校验通过后写回**启动时 `-config` 指定的文件（原子写：临时文件 + rename），**不触发生效**；校验失败返回 400 + 错误信息，不写文件。
- `POST /api/reload` → 重读 `-config` 指定文件，校验通过则**原子切换**到新配置（重建 handler），返回成功；失败返回 400/500，保持旧配置不变。

### 8.5 重载机制

- 进程启动时记住 `-config` 文件路径；`reload` 针对该路径重读。
- 并发安全：当前生效的 handler/config 用原子机制（如 `atomic.Value`/`sync.RWMutex`）持有；reload 时整体替换，在途请求继续用旧 handler 完成，新请求走新 handler，进程不重启。
- reload 失败（文件不存在/解析错误/校验失败）：返回错误，**不改变**当前生效配置。
- 配置序列化：`Duration` 需能序列化回 `"15s"` 字符串（当前仅实现反序列化，需补 `MarshalJSON`），保证 `PUT /api/config` 写回的文件可被再次 `Load`。

### 8.6 路由与认证优先级

按路径分派，再各自认证（保持「认证优先于路由」）：

| 路径 | 认证 | 处理 |
|---|---|---|
| `/admin`、`/admin/...` | admin Basic Auth | 静态页面/资源 |
| `/api/config`、`/api/reload` | admin Basic Auth | 管理 REST API |
| `/v1/chat/completions` | OpenAI Bearer auth_token | 现有转发 |
| `/v1/messages` | Anthropic x-api-key auth_token | 现有转发 |
| `/v1/models` | 按入口协议认证 | 本地生成模型列表 |
| 其他 | — | 404（统一 OpenAI 格式，保持 B1） |

- `admin_password` 为空时：`/admin*`、`/api/*` 一律 404（管理面禁用）。
- admin 路径先判 Basic Auth；模型路径按协议判 token。

### 8.7 明确不做（M3 边界）

- 不调用上游 `/v1/models` 自动发现（用户拍板，仅列配置声明模型）。
- 不自动监控文件变化热加载（仅手动 reload）。
- 不做 session/cookie 登录（M3 用 Basic Auth）。
- 不做协议转换。
- 不实现回退降级（FR-3，M4）。
- 不实现跨平台 CI/Docker（M5）。

### 8.8 验收标准

- `go build`/`go vet`/`go test` 退出码 0。
- 单测覆盖：`/v1/models` 双协议格式与过滤去重、admin Basic Auth 401、`PUT /api/config` 校验与写回、`POST /api/reload` 成功/失败切换、`Duration` 序列化往返。
- 端到端（主代理补）：真实配置下 `GET /v1/models` 返回配置声明模型；PUT 改配置 → reload 生效；admin 认证失败 401；无 `admin_password` 时 `/admin` `/api` 404。
- `config.example.json` 补 `admin_user`/`admin_password` 示例；README/CHANGELOG/design/tests 同步。
- `-version` 仍 `0.0.1-rc`。
- 环境变量覆盖：设置 `MODEL_ROUTER_AUTH_TOKEN` 等变量后启动/reload，生效值以环境变量为准。

### 8.9 环境变量覆盖（`listen`/`auth_token`/`admin_user`/`admin_password`，M3 之后增量）

> 本节为 M3 收尾后的增量需求（用户 m00734 提出，为 Docker 部署准备），**不属当前 M3 任务范围**，待 M3 验收后另行派发。

为方便 Docker 部署（NFR-2），四个顶层标量字段支持用环境变量覆盖配置文件值：

| 字段 | 环境变量 | 说明 |
|---|---|---|
| `listen` | `MODEL_ROUTER_LISTEN` | 监听地址 |
| `auth_token` | `MODEL_ROUTER_AUTH_TOKEN` | 客户端访问 token |
| `admin_user` | `MODEL_ROUTER_ADMIN_USER` | 管理员登录名 |
| `admin_password` | `MODEL_ROUTER_ADMIN_PASSWORD` | 管理员密码 |

- **优先级**：环境变量 > 配置文件；环境变量**非空**才覆盖（未设置或空值 = 忽略，沿用配置文件值）。
- **应用时机**：在 JSON 解析之后、应用默认值与校验（`applyDefaults`）**之前**统一覆盖，使环境变量值参与后续 trim / 必填校验 / 默认值逻辑；`POST /api/reload` 重读同样遵守。
- **fail-closed 不变**：覆盖后仍执行现有必填校验（`auth_token` 最终为空 → 启动/reload 失败）；`backends` 数组不支持环境变量，仍必须写在配置文件。
- **只覆盖不写回**：环境变量只影响内存中的生效配置，不修改 `-config` 文件本身；`GET /api/config` 返回覆盖后的生效值。
- 覆盖值同样经 trim 空格处理（沿用 `applyDefaults`）。

## 9. 配置页可视化升级（M3.1：实例化配置结构 + 引导式 UI + 连接测试/模型查询）

> 本节由产品经理（主代理）规格化，是 M3 配置页升级的派发与验收依据。
> 用户需求（m00958 + m00963 问答 + m00978）：
> ① 配置页按 openai/anthropic 分两个 tab，添加提供商（base_url/api_key）后可测试连接、查询上游模型（成功后列模型列表），openai 勾选/输入多个模型，anthropic 按 4 角色 + 托底选择/输入模型，保留 JSON 高级编辑。
> ② 用户拍板「实例」语义：添加提供商 = 新增一个实例（同一供应商可用不同 api_key 建多个实例），每个实例有各自映射，回退降级以实例为粒度。
> ③ JSON 编辑后需校验，校验通过才能保存。

### 9.1 配置结构演进：`backends` → `instances`

`Config` 顶层废弃 `backends []Backend`，改为 `instances []Instance`。一个 `Instance` = 一个「提供商实例」（协议 + base_url + api_key + timeout），承载一组模型映射。

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "...",
  "admin_user": "admin",
  "admin_password": "...",
  "instances": [
    {
      "protocol": "openai",
      "base_url": "https://api.deepseek.com",
      "api_key": "sk-...",
      "timeout": "15s",
      "models": ["deepseek-v4-flash", "deepseek-v4-pro"]
    },
    {
      "protocol": "anthropic",
      "base_url": "https://api.deepseek.com/anthropic",
      "api_key": "sk-...",
      "timeout": "15s",
      "roles": {
        "sonnet": "deepseek-v4-flash",
        "opus": "deepseek-v4-pro",
        "fable": "deepseek-v4-pro",
        "haiku": "deepseek-v4-flash",
        "*": "deepseek-v4-flash"
      }
    }
  ]
}
```

字段与校验：
- `protocol`（必填）：`openai` / `anthropic`，小写归一化。
- `base_url`（必填）：去尾部斜杠，绝对 http(s) URL（沿用 §7 校验）。
- `api_key`（必填）：非空。
- `timeout`（可选）：默认 15s（沿用 Duration 语义）。
- `models`（**openai 实例专用**）：字符串数组，至少 1 项，去空去重、保留顺序；该实例对外提供的模型（精确匹配）。
- `roles`（**anthropic 实例专用**）：对象，键 ∈ {`sonnet`,`opus`,`fable`,`haiku`,`*`}，值为实际模型名（非空），至少 1 项；角色→模型映射。
- 互斥：`openai` 实例必须 `models` 且不得有 `roles`；`anthropic` 实例必须 `roles` 且不得有 `models`。违反 → 启动/校验失败。
- 废弃字段：`Backend.model`（由 `models` / `roles` 值取代）、`Backend.matches`（由 `roles` 的 `claude-<role>-` 前缀覆盖，见 §7.8）、`Backend.roles` 数组（由 `Instance.roles` 对象取代）。旧 `backends` 字段不再识别，出现即解析失败（实现方可给出「请迁移到 instances」的友好报错）。

### 9.2 路由语义（实例展开）

运行时把 `instances` 展开成扁平「路由条目」，保持 §7 的匹配顺序与改写规则：

- **展开**：按 `instances` 顺序、实例内声明顺序展开。
  - openai 实例 `models` 每项 → 条目 `{protocol=openai, base_url, api_key, model=<项>}`；
  - anthropic 实例 `roles` 每个键值 → 条目 `{protocol=anthropic, base_url, api_key, model=<值>, role=<键>}`。
- **精确匹配 R4a**：请求 `model` 精确命中任一条目的 `model`（openai 的 models 项，或 anthropic 的 roles 值）→ 转发到该条目，**不改写**。
- **角色匹配 R4c**：请求 `model` 精确等于角色名（`sonnet`/`opus`/`fable`/`haiku`）或以 `claude-<role>-` 开头 → 命中声明该角色的 anthropic 条目（`role` 匹配）→ 转发并**改写** body 顶层 `model` = 条目 `model`。
- **托底 R4d**：请求 `model` 是合法字符串、未命中任何精确/角色，且存在声明 `role="*"` 的条目 → 命中第一个 → 转发并**改写** = 该条目 `model`。
- **回退 R5**：都未命中 → 转发到展开后首条目，**不改写**。
- 判定顺序（对展开后条目逐个）：R4a → R4c → R4d；显式精确优先于角色，具体角色优先于托底。冲突（多实例声明同一角色/`*`）→ 按实例顺序取第一个（同 B6）。
- 无对应协议实例 → 502（同 §7.2）；`instances` 为空 → 启动失败（fail-closed）。

### 9.3 Web 配置页（可视化 + JSON 高级编辑）

在 §8.3 基础上重做前端，两种模式共存：

**可视化模式（默认）**：
- 两个 tab：**OpenAI** / **Anthropic**，各自管理该协议的实例列表。
- 「添加实例」表单：`base_url`、`api_key`、`timeout`（可选）。添加后生成实例卡片。
- 实例卡片操作：
  - 「测试连接」→ `POST /api/probe/test`（见 9.4），展示连通结果（延迟/错误）。
  - 「查询模型」→ `POST /api/probe/models`（见 9.4），成功后列出上游模型清单供选择；失败提示「该上游不支持模型列表查询，请手动输入」。
  - OpenAI 实例：模型清单勾选（可多选）或手动输入模型名，生成 `models`。
  - Anthropic 实例：4 个角色（sonnet/opus/fable/haiku）+ 托底（*），每个从模型清单选择或手动输入，生成 `roles`。
- 「保存」→ 序列化为 `instances` 配置 → `PUT /api/config`；「重载」→ `POST /api/reload`。

**JSON 高级模式（保留）**：
- 一个 JSON 编辑框（回填当前生效配置）。
- 「校验」→ `POST /api/config/validate`（见 9.4，dry-run，不保存），通过才启用「保存」。
- 「保存」→ `PUT /api/config`（后端仍会再校验）。

### 9.4 管理 REST API 新增

全部走 admin Basic Auth（同 §8.4），`admin_password` 空则 404。

- `POST /api/probe/test`：body `{protocol, base_url, api_key, timeout?}` → 探测上游连通性，**按协议区分探测端点**：
  - `openai`：`GET {base_url}/v1/models`（`Authorization: Bearer`）。
  - `anthropic`：`POST {base_url}/v1/messages` 最小请求（`{"model":"ping","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`，`x-api-key` + `anthropic-version: 2023-06-01`）。原因：部分 Anthropic 兼容上游（如 DeepSeek）**未实现 `/v1/models`**，用真实存在的 messages 端点探测更可靠。
  - 判定：网络错误/超时 → `ok:false`（不可达）；`401`/`403` → `ok:false`（api_key 无效）；`404` → `ok:false`（base_url 路径无效）；其余 HTTP 响应（`2xx`/`400`/`422`/`429`）→ `ok:true`（`400`/`422` 表示认证通过、端点存在，仅探测占位 model 无效，属预期）。返回 `{ok, latency_ms, message}`。不修改配置、不缓存。
- `POST /api/probe/models`：body `{protocol, base_url, api_key, timeout?}` → 调上游模型列表 `GET {base_url}/v1/models`（openai `Authorization: Bearer` / anthropic `x-api-key` + `anthropic-version: 2023-06-01`），解析 `data[].id` → 返回 `{models: [...]}`；上游不支持（404/无 data，DeepSeek anthropic 入口即属此类）→ 返回明确错误，前端引导手动输入。
- `POST /api/config/validate`：body 完整配置 JSON → 走 `Parse` 全量校验（含 fail-closed、instances 结构、互斥、环境变量覆盖语义），**不写回、不生效** → 通过返回 200 `{ok:true}`，失败返回 400 `{ok:false, error}`。

### 9.5 边界（明确不做）

- 不实现回退降级 FR-3（M4；但 `instances` 结构已为其铺垫「实例粒度回退」）。
- probe 不持久化、不缓存结果；不做自动连接健康检查（M4）。
- 不做 session/cookie 登录（仍 Basic Auth）。
- 不做协议转换；不做跨平台 CI/Docker（M5）。
- `/v1/models` 对外入口仍只列配置声明模型、不调上游（§8.2 不变；probe 是管理面独立能力）。

### 9.6 验收标准

- `go build`/`go vet`/`go test` 退出码 0。
- 单测：`instances` 解析与校验（互斥、必填、roles 键合法性、models 去空去重）；实例展开路由（精确/角色/托底/回退/502）；`/api/probe/test`、`/api/probe/models`（httptest mock 上游）；`/api/config/validate`（通过/失败）；旧 `backends` 字段报错。
- 端到端（主代理补）：真实配置下可视化流程（添加实例→probe→保存→reload→路由验证）；JSON 校验通过才可保存；`config.local.json` / `config.example.json` 迁移为 `instances` 结构且路由行为不变。
- `config.example.json` 改为 `instances` 结构示例；README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。
- 迁移：`config.local.json`（5 个后端）→ 2 个实例（1 openai + 1 anthropic），角色映射与托底行为与迁移前一致。

## §10 供应商预设（Provider Presets）

### 10.1 目标

「增加实例」改为两步：先弹「选择供应商」框（内置供应商或「自定义」）。内置供应商为**纯数据预设**，用通用 `models_override` 机制解决「部分供应商的 Anthropic 兼容入口未实现 `/v1/models`」的问题（如 DeepSeek），不做 hardcode 特判。

### 10.2 内置供应商清单

内置 11 个供应商（数据，可后续增改）。`base_url` 为公开文档常见值，DeepSeek 已实测确认：

| id | name | openai_base_url | anthropic_base_url | models_override |
|---|---|---|---|---|
| deepseek | DeepSeek | https://api.deepseek.com | https://api.deepseek.com/anthropic | anthropic → {base_url: https://api.deepseek.com, auth: bearer} |
| openai | OpenAI | https://api.openai.com/v1 | — | — |
| anthropic | Anthropic | — | https://api.anthropic.com | — |
| zhipu | 智谱 GLM | https://open.bigmodel.cn/api/paas/v4 | https://open.bigmodel.cn/api/anthropic | — |
| volcengine | 火山方舟(豆包) | https://ark.cn-beijing.volces.com/api/v3 | https://ark.cn-beijing.volces.com/api/anthropic | — |
| moonshot | 月之暗面 Kimi | https://api.moonshot.cn/v1 | https://api.moonshot.cn/anthropic | — |
| dashscope | 通义千问 | https://dashscope.aliyuncs.com/compatible-mode/v1 | — | — |
| siliconflow | 硅基流动 | https://api.siliconflow.cn/v1 | — | — |
| groq | Groq | https://api.groq.com/openai/v1 | — | — |
| mistral | Mistral | https://api.mistral.ai/v1 | — | — |
| xai | xAI Grok | https://api.x.ai/v1 | — | — |

- `models_override` 语义：声明「某协议实例拉取模型列表时，改从哪个 base_url + 认证方式拉」。目前仅 DeepSeek 需要（其 anthropic 入口无 `/v1/models`，模型列表从 openai 入口 `GET /v1/models` 拉，`auth=bearer`）。其他供应商若后续发现同类问题，仅在数据里补一条 override，不改逻辑。
- 「自定义」不是供应商，是选择框里的独立入口（`provider` 留空）。

### 10.3 数据结构与校验

- 新包 `internal/providers`：内置供应商清单（硬编码数据）+ 查询 `List()` / `Get(id)`；通过 `GET /api/providers` 暴露（admin Basic Auth，`admin_password` 空则 404）。
- `Instance` 新增可选字段 `provider`（内置供应商 id，字符串，空=自定义）。
  - 校验：非空时必须是已知内置供应商 id，否则启动失败（fail-closed）；空=自定义，合法。
  - `provider` 仅作「来源标记 + models_override 判定」，不影响路由/转发（转发仍用实例自己的 `base_url`/`api_key`）。

### 10.4 probe/models 的 override

- `POST /api/probe/models` body 新增可选 `provider` 字段。
- 拉模型列表时：若 `provider` 非空、该供应商对该 `protocol` 声明了 `models_override` → 用 `override.base_url` 发起 `GET /v1/models`，认证按 `override.auth`（`bearer` → `Authorization: Bearer <api_key>`；`x-api-key` → `x-api-key` + `anthropic-version`）；否则按现有逻辑（`base_url` + `protocol` 认证）。
- `POST /api/probe/test` 不变（仍按协议区分探测端点，不涉及 provider）。

### 10.5 UI 交互

- 「增加实例」→ 先弹「选择供应商」框：列内置供应商（name + 支持的协议）+「自定义」。
- 选内置供应商 → 按当前 tab 的 protocol 自动填该供应商对应协议的 `base_url`，实例记 `provider`；若该协议无对应 base_url（如 Anthropic tab 选了仅 openai 的供应商）→ 提示不支持并阻止。
- 选「自定义」→ 手动填 `base_url`，`provider` 留空。
- 「查询模型」请求带当前 `provider`，让后端走 override（DeepSeek anthropic 页从 openai `/v1/models` 拉模型）。
- 保存后实例带 `provider` 字段；JSON 高级编辑同样支持该字段。

### 10.6 边界（明确不做）

- 不做供应商账号/OAuth；内置清单为静态数据，不做联网自动更新。
- 不联网校验 base_url（已有 probe 能力覆盖）。
- 供应商「是否支持某协议」以内置数据声明为准，不动态探测。

### 10.7 验收标准

- `go build`/`go vet`/`go test` 退出码 0。
- 单测：`providers` 清单完整性（11 个 + id 唯一）；`Instance.provider` 校验（合法/非法/空）；`probe/models` 的 override 分支（DeepSeek anthropic 走 openai base_url + bearer）；无 override 时走原逻辑；`GET /api/providers` 返回清单。
- 端到端（主代理补）：`GET /api/providers` 返回 11 供应商；真实 DeepSeek anthropic 实例「查询模型」从 `https://api.deepseek.com/v1/models` 拉到 `deepseek-flash`/`deepseek-v4-pro`；UI 选择框 + base_url 自动带入 + 自定义手动填。
- README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。
