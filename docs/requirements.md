# 需求规格说明书（model-router）

> 本文档由产品经理（主代理）规格化编写，是需求侧的单一事实来源。
> 设计文档（`docs/design.md`）与测试用例（`docs/tests.md`）由 Claude Code 编写。
> 版本规则、分支规则、提交规则见 `AGENTS.local.md`。

## 1. 项目概述

一个**模型路由代理程序**：对外提供 OpenAI（OpenAPI）与 Anthropic 两种标准协议入口，按配置把请求**透传**给后端的多个候选模型，并根据可用性/速度自动做模型选择与降级回退。

关键原则：**不转协议，只代理透传**。提供 OpenAI 协议的 path 只能接入兼容 OpenAI 协议的模型；提供 Anthropic 协议的 path 只能接入兼容 Anthropic 协议的模型。

## 2. 功能需求（FR）

### FR-1 监听与协议入口
- 监听一个指定端口（配置项 `listen`）。
  > ~~监听一个（或多个，后续扩展）指定端口~~——**多端口监听已取消**（2026-11-15 用户确认）：实际需求是单个监听端口同时承载两个协议入口；确需多端口时启动多个实例即可。
- 提供 OpenAPI 标准协议入口（对应独立 path，如 `/v1/chat/completions`）。
- 提供 Anthropic 标准协议入口（对应独立 path，如 `/v1/messages`）。
- 两个协议互不转换，各自只对接各自兼容协议的后端模型。

### FR-2 模型发现与选择
- ~~能自动查询后端可用模型（如调用后端 `/v1/models`），并据此选择/过滤可用模型。~~
  > **自动查询后端模型已取消**（2026-11-15 用户确认）：模型由配置显式声明。运行时的 `/v1/models` 只聚合配置里声明的 `models`/`roles`（§8.2）；配置页的「查询模型」是管理面辅助功能（§9.4 `probe/models`），不参与运行时路由。
- 每种协议维护一个**有序**的实例/模型列表，可配置排序（M3.1 起以「实例」为粒度，见 §9）。
- ~~Anthropic 协议下模型可按类别分组（如 `sonnet` / `opus`），组内各自有序；回退时优先在组内、再跨组。~~
  > **「组内有序 / 跨组回退」已取消**（2026-11-15 用户确认）：该需求演进为 `roles` 角色映射（§7.8「模型角色映射」）+ 实例顺序候选链（§11.2）。即：同一角色由声明该角色的实例承担，回退按 `instances` 的配置顺序逐级下移，不存在「组内排序」这一层结构。

### FR-3 回退与降级（核心）
> 下述为规格化的状态机，默认参数见 §4。多模型"链式降级"的确切边界见 §5 开放问题 D-1。

- 模型列表有序 `[M0, M1, M2, ...]`，**主模型 = 列表首元素 M0**，默认优先使用。
- **失败判定**（任一成立即为该模型本次"失败"）：
  1. 连接失败；
  2. 超时（默认 15 秒）；
  3. 吞吐 `token/s < 阈值`（默认 30；M4.2 起按 §15.4 以「首 token → 末 token」的纯生成速度计算，小样本跳过判定；token 取自响应 usage）。
- **临时回退**：主模型失败时，本次请求立即改用下一层模型服务，并记录主模型失败次数 +1。
- **失败计数 < 阈值（默认 3）阶段**：每次连接仍先尝试主模型（"临时回退前的模型"）；主模型成功则失败计数清 0。
- **失败计数 ≥ 阈值**：进入**持续回退**——后续连接直接使用下一层模型（不再先试主模型），回退时长默认 5 分钟。
- **回退期结束**：下一个请求回到主模型 M0 开始尝试；此时**失败一次就回退一层**（不再累计失败阈值），回退时长重新计时。
- **恢复正常**：M0 尝试成功即恢复正常，下次失败计数从 0 开始。

### FR-4 服务访问认证（M1 增量，M2 修订）
- 代理服务对外提供**服务访问 token**：客户端（调用方）访问任意协议入口时必须携带，携带方式符合对应协议标准：
  - OpenAI 入口 `/v1/chat/completions`：`Authorization: Bearer <auth_token>`
  - Anthropic 入口 `/v1/messages`：`x-api-key: <auth_token>` 或 `Authorization: Bearer <auth_token>`（双认证，任一匹配即可，详见 §17.2）
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
| token/s 阈值 | 30 | 低于此值判为"慢"（按生成速度，见 §15.4） |
| 首 token 超时 | 10 s | 收到响应头后等待首个数据块的上限（§15.3） |
| 流式空闲超时 | 30 s | 相邻数据块最大间隔（§13.2） |
| 失败阈值 | 3 次 | 触发持续回退的失败次数 |
| 回退时长 | 5 min | 持续回退的持续时间 |

> 以上参数均可通过配置覆盖（配置方式在 M1 阶段为静态配置文件，后续为 Web 页面）。

## 5. 开放问题（已全部关闭）

- **D-1 链式降级**：✅ 已定（§11.2）——沿 `instances` 配置顺序逐级下移，即链式降级；每级独立按原始请求的 model/角色判断，不继承中间改写。
- **D-2 token/s 统计口径**：✅ 已定（§15.4）——流式按「首个数据块 → 末个数据块」计算纯生成速度，非流式按响应体读取时长近似；不设滑动窗口，输出 token <100 或生成时长 <2s 时跳过判定；另有 §19 实时低速检测（10s 滑动字节窗口）用于流式中途判定。
- **D-3 并发回退一致性**：✅ 已定（§11.3）——每个实例一份状态（`consecutiveFailures` / `degradedUntil`），由 `sync.Mutex` 统一保护（`internal/proxy/proxy.go` 的 `Handler.mu`）。
- **D-4 配置热更新**：✅ 已定（§8.5）——`POST /api/reload` 重新读取 `-config` 指定文件，校验通过后原子切换 handler（`atomic.Pointer`），失败保留旧配置。

## 6. 里程碑规划

| 里程碑 | 内容 | 状态 |
|---|---|---|
| M1 | 项目骨架 + 单模型透传壳子（OpenAI 协议）+ 服务访问认证（FR-4）+ 静态配置 + 构建脚本（先 windows amd64） | 已完成 |
| M2 | 增加 Anthropic 协议入口 + 多模型列表与排序（含 matches/roles 模型映射） | 已完成 |
| M3 | `/v1/models`（按配置聚合）+ Web 配置页面 + 环境变量覆盖 + 供应商预设 | 已完成 |
| M4 | 回退/降级机制（FR-3 完整实现，实例粒度）+ fallback 可配置 + 实时低速检测 + gzip 解压修复 | 已完成 |
| M5 | 跨平台构建（GitHub Actions）+ Docker（amd64/arm64） | 已完成 |

> 全部里程碑已于 0.1.0（2026-11-15）交付。三项原始需求（多端口监听、自动查询后端模型、Anthropic 组内/跨组回退）已确认取消，见 §2。容器镜像发布名为 `wuzm219/model-router`。

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
- 不处理（可后续扩展）：`best`（等同 opus）、`opusplan` 特殊模式；这些 `model` 值未命中时按 R5 回退 L[0]。`[1m]` 后缀变体已由 §17.3 处理（`sonnet[1m]`/`opus[1m]` → 剥除后按角色命中）。

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
- 模型清单：
  - **OpenAI 入口**：收集所有 openai 实例的 `models` 值，按实例顺序、出现顺序去重。
  - **Anthropic 入口**：返回**预定义角色名** `sonnet`/`opus`/`fable`/`haiku`（不是 `roles` 映射的值），只返回至少有一个 anthropic 实例配置了该角色的角色名，按固定顺序 `sonnet → opus → fable → haiku` 去重；不返回 `*`。
  - 理由：Anthropic 客户端（Claude Code）从 `/v1/models` 选模型后用该 id 发请求；只有返回角色名，客户端才能发 `sonnet`/`opus` 等，代理再经 `roles` 映射到实际模型（§9.2）。
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
- `/v1/models` 对外入口仍不调上游（§8.2；OpenAI 返回实例 `models` 值、Anthropic 返回角色名；probe 是管理面独立能力）。

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

## §11 M4 回退降级（实例粒度）

### 11.1 目标与范围

把 FR-3 的回退状态机从「模型粒度」落到**实例粒度**：每个协议下，实例按配置顺序构成候选链；当前实例失败时，按顺序回退到下一个能提供**等价模型**的实例，直到成功或全部失败。

- 回退单位 = 实例（一个实例 = base_url + api_key + timeout + 一组模型映射）。
- 顺序 = `instances` 数组中该协议实例的声明顺序（配置页可拖动排序）。
- 沿用 FR-3 的失败判定、失败计数、持续回退、恢复探测与默认参数（§4）。
- 不做跨协议回退；不做后台健康检查；不做全局轮询/负载均衡。

### 11.2 候选链（candidate chain）

对一次请求，先按 §9.2 的路由规则确定**基础实例**（第一个能处理该请求的实例），然后从基础实例开始、按该协议实例顺序收集候选，每个候选给出「上游模型」：

- **OpenAI 请求 `model=X`**
  - 实例 `models` 含 `X` → 上游模型 = `X`（**不改写**）。
  - 实例不含 `X` → 上游模型 = 该实例 `models[0]`（**改写** `model=models[0]`）。
  - 实例 `models` 为空 → 跳过（校验保证不会发生）。
- **Anthropic 请求角色 `R`**（`R` 为 `sonnet`/`opus`/`fable`/`haiku`，或以 `claude-<role>-` 前缀识别为对应角色）
  - 实例 `roles` 含 `R` → 上游模型 = `roles[R]`（**改写** `model=roles[R]`）。
  - 实例不含 `R` 但含 `*` → 上游模型 = `roles["*"]`（**改写**）。
  - 实例既无 `R` 也无 `*` → 跳过（不作为候选）。
- **请求 model 缺失 / null / 非字符串 / 未命中任何实例**
  - 基础实例 = 该协议第一个实例（R5 语义），候选链 = 该协议全部实例，上游模型 = 各实例 `models[0]`（OpenAI）或 `roles["*"]`（Anthropic，无 `*` 则跳过）；**不改写** body（保持 M3 透传语义，R5 不改写）。

候选链顺序 = 实例声明顺序；同一实例只出现一次。

**每级独立判断（重要）**：候选链中每一级都基于**原始请求的 model / 角色**判断，**不继承上一级改写后的 model**。**普通路由（§9.2 的 R4a/R4c/R4d）同样适用**：精确匹配、角色匹配、托底都基于**原始请求的 model/角色**判断，不因任何中间改写改变匹配依据。
- 请求 `model=X` 时：即使第 2 级因无 `X` 用了 `Y`（`models[0]`），第 3 级若同时有 `X` 和 `Y`，仍**优先用 `X`**；只有该级没有 `X` 时才用其 `models[0]`。
- Anthropic 请求角色 `R` 时：即使第 2 级因无 `R` 用了 `*` 托底，第 3 级若同时有 `R` 和 `*`，仍**优先用 `roles[R]`**；只有该级没有 `R` 时才用 `*`。

### 11.3 状态机（每个实例一份，线程安全）

每个实例维护：
- `consecutiveFailures`：连续失败次数（int）。
- `degradedUntil`：持续回退截止时间（time.Time，零值 = 未回退/已恢复）。

请求处理：

1. 计算候选链。
2. 从链头开始，**跳过** `now < degradedUntil` 的候选（持续回退中）。
3. 依次尝试未被跳过的候选：
   - 成功 → 该实例 `consecutiveFailures=0`、`degradedUntil=零`；返回响应。
   - 失败 → `consecutiveFailures++`：
     - 若该实例 `degradedUntil` 非零且已过期（**恢复探测**：回退期结束后的首次尝试）→ `degradedUntil = now + 回退时长`（重新计时，**不再累计**）。
     - 否则若 `consecutiveFailures >= 失败阈值`（默认 3）→ `degradedUntil = now + 回退时长`（进入持续回退）。
     - 继续尝试链上下一个候选（**本次请求临时回退**）。
4. 若链上所有候选都被跳过（全部处于持续回退中）→ **退化为尝试链头候选一次**（保证至少尝试，符合「回退期结束回主实例」）。
5. 所有候选尝试都失败 → 返回最后一个错误（502，按协议错误体格式）。

**临时回退** vs **持续回退**：
- `failures < 3` 且未进入持续回退：每次请求仍从链头（主实例）试起（临时回退只在本次请求内生效）。
- `failures >= 3`：`degradedUntil` 设置，后续请求跳过该实例，直接用下一个候选（持续回退，默认 5 分钟）。
- 回退期结束：`degradedUntil` 过期后，实例重新可尝试；若再次失败，立即重新进入持续回退（「失败一次就回退一层」）。

### 11.4 失败判定与回退时机

**失败判定**（任一成立）：
1. 连接失败（dial 错误）。
2. 超时：连接 / 响应头超时，默认 15s（沿用实例 `timeout`）。
3. 吞吐：生成速度低于阈值（默认 `min_tokens_per_second` = 30 token/s；M4.2 起按 §15.4 以「首 token → 末 token」的纯生成速度计算，并对小样本跳过判定）。

> 注：本节原文曾写「计数窗口 30 秒」，该表述已废弃——M4.2 起不设滑动窗口，每次请求独立按生成速度判定（详见 §15.4）。

**回退时机**：
- 情况 1、2 发生在响应体转发前 → **本次请求立即回退到下一候选重试**。
- 情况 3 发生在响应完成后（响应已发出）→ **本次不回退**，只记录失败状态（`consecutiveFailures++` / 更新 `degradedUntil`），影响**后续请求**的回退决策。
- 流式响应一旦开始转发（已写出响应头/首块），不再中断回退（避免客户端收到不一致数据）；吞吐不达标按「响应完成后记录失败」处理。

> 工程化说明：透传代理无法在「响应已发出」后回退而不破坏客户端数据；因此 FR-3 的「吞吐失败」在 M4 中体现为**失败计数/回退状态更新**，而非本次请求换模型。

### 11.5 默认参数（沿用 §4）

| 参数 | 默认 |
|---|---|
| 连接/响应超时 | 15 s（每实例 `timeout`） |
| token/s 阈值 | 30 |
| 失败阈值 | 3 次 |
| 回退时长 | 5 分钟 |

### 11.6 Web 配置页：实例拖动排序

- 可视化模式两个 tab 的实例卡片支持**拖动排序**（HTML5 drag & drop 或等价交互）。
- 排序结果即保存时 `instances` 数组顺序，决定候选链优先级（越靠前越优先）。
- JSON 高级模式直接编辑数组顺序，等效。

### 11.7 明确不做

- 不做跨协议回退（OpenAI 失败不转 Anthropic 实例）。
- 不做后台健康检查/定时探测（回退纯请求驱动）。
- 不做全局轮询/随机/最少连接等负载均衡（严格按配置顺序）。
- 不做流式中断回退（已开始转发的响应不中断）。
- 不做回退状态持久化（进程内存，重启清零）。

### 11.8 验收标准

- `go build`/`go vet`/`go test` 退出码 0。
- 单测：
  - 候选链构建：OpenAI 同名命中不改写、无同名用 `models[0]` 改写；Anthropic 同角色改写、无角色用 `*` 改写、两者皆无则跳过；R5 未命中候选链与不改写。
  - 状态机：首次失败临时回退；失败计数 <3 下次仍先试主实例；≥3 进入持续回退（跳过主实例）；回退期结束恢复探测、失败即重新计时；成功恢复清零。
  - 并发安全（`go test -race` 或等价）。
  - 吞吐判定：mock 上游慢响应/低 usage → 记录失败（响应仍透传）。
- 端到端（主代理补）：两实例 mock，主实例连接失败 → 回退到第二实例（同名/第一个模型）；主实例连续失败 3 次 → 后续直接走第二实例；回退期（可缩短用于测试）结束后回主实例。
- UI：拖动排序改变 `instances` 顺序，保存后生效（`/api/config` 顺序与拖动一致）。
- README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。

## 12. M4 配置结构修正（fallback 分组 + instances 分组）

### 12.1 目标

M4 验收后，用户要求两点配置结构修正：
1. 回退相关参数不再硬编码，改为可配置，放在 `Config` 顶层 `fallback` 对象下。
2. `instances` 不再用混合数组（含 `protocol` 字段），改为按协议分成 `openai` / `anthropic` 两个数组。

### 12.2 配置结构

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "...",
  "admin_user": "admin",
  "admin_password": "...",
  "fallback": {
    "failure_threshold": 3,
    "degrade_duration": "5m",
    "min_tokens_per_second": 30
  },
  "instances": {
    "openai": [
      {"base_url": "...", "api_key": "...", "provider": "...", "timeout": "15s", "models": ["..."]}
    ],
    "anthropic": [
      {"base_url": "...", "api_key": "...", "provider": "...", "timeout": "15s", "roles": {"sonnet": "...", "*": "..."}}
    ]
  }
}
```

#### fallback 对象（可选）

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `failure_threshold` | int | 3 | 连续失败 ≥ 该值进入持续回退；≥1 |
| `degrade_duration` | Duration 字符串 | `"5m"` | 持续回退时长；>0 |
| `min_tokens_per_second` | float | 30 | 吞吐阈值；0 = 禁用吞吐判定 |

- `fallback` 整体可省略；省略或某项缺失 → 该项用默认值。
- 校验失败（如 `failure_threshold` ≤0、`degrade_duration` ≤0、`min_tokens_per_second` <0）→ 启动/校验失败。

#### instances 分组（必填，至少一个实例）

- 废弃 `instances []Instance`（含 `protocol` 字段），改为 `instances` 对象含两个数组 `openai` / `anthropic`。

**OpenAI 实例字段**：

| 字段 | 必填 | 说明 |
|---|---|---|
| `base_url` | 是 | 去尾斜杠，绝对 http(s) URL |
| `api_key` | 是 | 非空 |
| `models` | 是 | 字符串数组，至少 1 项，去空去重保序 |
| `provider` | 否 | 内置供应商 id，空 = 自定义（沿用 §10） |
| `timeout` | 否 | 默认 15s |

**Anthropic 实例字段**：

| 字段 | 必填 | 说明 |
|---|---|---|
| `base_url` | 是 | 去尾斜杠，绝对 http(s) URL |
| `api_key` | 是 | 非空 |
| `roles` | 是 | 对象，键 ∈ {`sonnet`,`opus`,`fable`,`haiku`,`*`}，值非空，至少 1 项 |
| `provider` | 否 | 同上 |
| `timeout` | 否 | 默认 15s |

校验规则：
- 去掉 `protocol` 字段（由所在分组决定）；实例内若出现 `protocol` → 视为未知字段，解析失败（DisallowUnknownFields）。
- 每类数组可为空，但 `openai` + `anthropic` 总实例数 ≥ 1，否则启动/校验失败（fail-closed）。
- openai 实例不得有 `roles`，anthropic 实例不得有 `models`（沿用 §9.1 互斥）。
- 旧 `instances` 数组形态（顶层为数组）→ 解析失败，报错提示「instances 已改为分组结构 {openai, anthropic}」。

### 12.3 运行时语义（不变）

- 候选链、状态机、每级独立判断、失败判定、回退时机全部沿用 §11，仅参数改从 `fallback` 读。
- 路由（§9.2）简化：`openai` 组 / `anthropic` 组各自独立展开，不再跨协议过滤；R4a/R4c/R4d/R5 匹配顺序与改写规则不变。
- `provider` / `models_override` / `probe` / `/v1/models` / 环境变量逻辑不变（实例协议由所在分组确定）。
- 环境变量仍只覆盖 `listen/auth_token/admin_user/admin_password`（§8.9）；`fallback` 与 `instances` 不支持环境变量。

### 12.4 Web 配置页

- 可视化模式：两个 tab（openai/anthropic）实例列表改从 `instances.openai` / `instances.anthropic` 读取；拖动排序在**各组内**生效，保存按组内顺序序列化。
- JSON 模式：展示新结构（含 `fallback` 对象与分组 `instances`）。
- `fallback` 参数在 JSON 模式可见可编辑；可视化模式在页面顶部（实例列表上方）加「回退参数」区块，提供三个输入框：
  - `failure_threshold`（失败阈值）：整数 ≥1，默认 3；
  - `degrade_duration`（回退时长）：时长字符串（如 `5m`/`30s`/`1h`）>0，默认 `5m`；
  - `min_tokens_per_second`（最低吞吐 token/s）：浮点数 ≥0，默认 30，填 `0` 表示禁用吞吐判定（留空 = 缺省默认 30）。
  - 加载时从 `fallback` 读回填入；保存时读取并序列化进 `config.fallback`；非法值（阈值<1、时长非法、吞吐<0）阻止保存并就地提示（复用 `/api/config/validate`）。

### 12.5 明确不做

- 不做旧 `instances` 数组的自动迁移（直接报错要求迁移）。
- 不做 `fallback` / `instances` 的环境变量覆盖。
- 其余 §11.7「明确不做」继续有效。

### 12.6 验收标准

- `go build`/`go vet`/`go test`（含 `-race`）退出码 0。
- `fallback` 三项缺省/显式配置生效：改 `failure_threshold` 后主实例连续失败 N 次即持续回退；`degrade_duration` 缩短后恢复探测更快；`min_tokens_per_second=0` 禁用吞吐判定。
- `instances` 分组：openai 组 / anthropic 组各自展开；总实例 0 个启动失败；实例含 `protocol` 字段报错；旧数组形态报错提示迁移。
- 可视化「回退参数」区块：加载读回、保存序列化进 `fallback`、非法值阻止保存并提示。
- 端到端（主代理补）：新结构 2 实例 mock 回退 + fallback 参数生效 + 拖动排序保存分组顺序。
- `config.example.json` 迁移到新结构；README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。

## §13 M4.1 流式中途切换（空闲超时 + pause_turn + OpenAI 打断）

### 13.1 目标

在 M4（§11）回退降级基础上，为「已 committed（首块已转发给客户端）的流式响应」新增**空闲超时**判定，并按协议提供「中途切换」处理：

- Anthropic 入口：空闲超时 → 以 `pause_turn` 优雅收尾，客户端 SDK 自动续接。
- OpenAI 入口：空闲超时 → 直接中断连接；低速（token/s 低）→ 不打断、只记失败。

### 13.2 配置新增：`fallback.idle_timeout`

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `idle_timeout` | Duration 字符串 | `"30s"` | 流式空闲超时；0 = 禁用空闲超时判定 |

- `fallback.idle_timeout` 可省略（用默认 30s）；校验非法 Duration 或 <0 → 启动/校验失败（fail-closed）。
- 其余 `fallback` 字段沿用 §12.2。

### 13.3 空闲超时判定

- 仅在**流式响应 committed 之后**生效：自最后一个数据块到达起，连续 `idle_timeout` 秒内无任何新数据块 → 触发空闲超时。
- 首字节等待（响应头未返回）仍走实例 `timeout`（§11.4 情况 2），不占用 `idle_timeout`。
- 非流式响应不受空闲超时影响。

### 13.4 触发后的按协议处理

**Anthropic 入口**：
- 终止读取上游流，向客户端补发收尾事件：若当前内容块未结束先补 `content_block_stop`，随后补 `message_delta`（`delta.stop_reason="pause_turn"`、`delta.stop_sequence=null`）与 `message_stop`。
- 客户端 SDK 识别 `pause_turn` 后，把已生成的 assistant message（含 content）加回 messages 并自动重发续接；重发请求按 §11 候选链正常路由（该实例已记失败，视失败计数决定是否切备用实例）。

**OpenAI 入口**：
- 终止读取上游流，直接中断客户端连接（流截断，不补 `[DONE]`）。是否重试由客户端 SDK 决定，代理不负责。

**共同**：两种协议触发空闲超时均记该实例失败（`recordFailure`，计入 `consecutiveFailures`，走 §11.3 状态机）。

### 13.5 低速（token/s）处理

- 吞吐不达标（判定口径见 §15.4，M4.2 起改为纯生成速度）时：committed 后**不打断流**、只记失败，影响后续请求回退决策。
- **OpenAI 与 Anthropic 完全一致**：任何协议的低速都**不补 `pause_turn`、不中断连接**，流完整转发到结束，仅记一次失败（§15.5）。
- 空闲超时与低速是**互补判定**：空闲超时捕获「完全卡死」，低速捕获「慢但在动」。`pause_turn` 只属于空闲超时（§13.4）。

### 13.6 明确不做

- OpenAI 空闲超时不尝试 `pause_turn` 等价（OpenAI Chat Completions 无该机制，`finish_reason` 无 pause 语义）。
- 空闲超时不触发「响应前无感切换」（那是首字节超时职责，§11.4 情况 2）。
- 不新增客户端重试协调逻辑（pause_turn 由客户端 SDK 自动续接；OpenAI 由客户端 SDK 决定是否重试）。
- 低速不升级为「中断流」。

### 13.7 Web 配置页

- 「回退参数」区块新增「空闲超时（idle_timeout）」输入框（Duration，默认 30s，填 0 禁用）。
- 加载读回、保存序列化进 `config.fallback.idle_timeout`、非法值阻止保存并提示（复用 §12.4 机制）。

### 13.8 验收标准

- `go build`/`go vet`/`go test`（含 `-race`）退出码 0。
- 配置：`fallback.idle_timeout` 缺省 30s / 显式生效 / 0 禁用 / 非法报错。
- Anthropic：mock 上游发出首块后静默超过 `idle_timeout` → 客户端收到 `message_delta(stop_reason="pause_turn")` + `message_stop` 收尾事件（不收到上游的残留数据）。
- OpenAI：mock 上游发出首块后静默超过 `idle_timeout` → 客户端连接被中断（流截断）。
- OpenAI 低速：mock 上游持续慢速输出（< `min_tokens_per_second`）→ 流不被中断、完整结束，仅记失败。
- README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。

## 14. M5 增量规格：跨平台构建 + Docker

> 本节由产品经理（主代理）在 M5 启动前规格化，是 M5 派发与验收的需求依据。对应 NFR：一键构建脚本 + GitHub Actions 跨平台构建；Docker 容器部署（amd64/arm64）。

### 14.1 目标

1. **GitHub Actions 跨平台构建** 6 个组合：`windows-linux-darwin` × `amd64-arm64`，产物上传/发布。
2. **Docker 多架构镜像**（amd64/arm64），纯静态二进制、非 root 运行。
3. **本地一键全平台构建**（`build-all.sh`），方便无 CI 时出包。
4. `docker-compose.yml` 部署示例。

### 14.2 GitHub Actions

文件 `.github/workflows/build.yml`：

- **触发**：`push`（所有分支）、`pull_request`、`push tag 'v*'`。
- **build job（matrix 6 组合）**：`os=[windows-latest, ubuntu-latest, macos-latest]` × `arch=[amd64, arm64]`；步骤：checkout → `actions/setup-go`（go-version 从 `go.mod` 或固定 `1.27.x`）→ `go build -trimpath -ldflags "-s -w -X model-router/internal/version.Version=$VERSION" -o dist/model-router-$VERSION-$GOOS-$GOARCH[.exe]` → `actions/upload-artifact`（6 个产物，命名一致）。
- **版本号**：从 `internal/version/version.txt` 读取；tag 触发时以 tag 去 `v` 前缀作为版本（如 `v0.0.1` → `0.0.1`），非 tag 用文件值。
- **release job（仅 tag）**：`softprops/action-gh-release` 或 `gh release create` 上传 6 个产物。
- **docker job（仅 ubuntu-latest）**：`docker/setup-buildx-action` → `docker buildx build --platform linux/amd64,linux/arm64`，tag 时推 `wuzm219/model-router:<version>` 与 `:latest`（Docker Hub）；非 tag 只构建不推送。
- Go 模块缓存（`actions/setup-go` 自带），失败即终止。

### 14.3 Dockerfile

- **multi-stage**：`golang:1.27-alpine AS build` → `gcr.io/distroless/static-debian12:nonroot`（纯静态二进制，无 shell，非 root uid 65532）。
- build 阶段 `CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X ...Version=$VERSION" -o /model-router .`（用 `--build-arg TARGETARCH` 支持多架构）。
- 运行阶段：`COPY --from=build /model-router /model-router`；`USER nonroot:nonroot`；`EXPOSE 18080`；`ENTRYPOINT ["/model-router", "-config", "/home/nonroot/config/config.json"]`。
- 支持环境变量 `MODEL_ROUTER_*`（§8.9）覆盖配置文件；默认 `MODEL_ROUTER_LISTEN=0.0.0.0:18080`（容器内需监听 0.0.0.0）。
- **容器时区设为 `Asia/Shanghai`（UTC+8）**：build 阶段 `apk add --no-cache tzdata`，运行阶段从 build 阶段 `COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /usr/share/zoneinfo/Asia/Shanghai`（供 Go 读 zoneinfo）+ `COPY --from=build /usr/share/zoneinfo/Asia/Shanghai /etc/localtime`（兜底）+ `ENV TZ=Asia/Shanghai`。这样日志时间戳为北京时间，避免与宿主机差 8 小时。
- 不做健康检查（可选加 `HEALTHCHECK`，非必需）。

### 14.4 docker-compose.yml

- 服务 `model-router`：`build: .` 或 `image: wuzm219/model-router:0.1.0`；`ports: ["18080:18080"]`；`volumes: ["./.docker:/home/nonroot/config:rw"]`；`environment` 可放 `MODEL_ROUTER_LISTEN=0.0.0.0:18080` 等。
- `restart: unless-stopped`。
- 作为部署示例，不强制要求生产配置。

### 14.5 build-all.sh

- 循环 `GOOS in windows linux darwin` × `GOARCH in amd64 arm64`，调用 `build.sh`（已支持环境变量）或直接 `go build`，产物统一落 `dist/`。
- 输出汇总清单；任一失败即非零退出。

### 14.6 明确不做

- 不做代码签名 / macOS 公证 / Windows 安装包。
- 不做 Homebrew / apt / rpm 包。
- 不做 K8s manifest / Helm chart。
- 不做 CI 中的真实 E2E（保持 `go test` + 构建）。

### 14.7 验收标准

- `.github/workflows/build.yml` YAML 语法正确（至少能被 `python -c "import yaml"` 解析；如本机有 `actionlint` 则必须通过）。
- `Dockerfile` 在本地可 `docker build`（或 `docker buildx build --platform linux/amd64`）成功；镜像可 `docker run` 启动并响应 `/v1/models`（带 token）。
- `docker-compose.yml` 语法正确（`docker compose config` 通过）。
- 容器日志时间戳为北京时间（UTC+8）：`docker run` 后 `docker logs` 的时间与宿主机本地时间一致。
- `build-all.sh` 可生成 6 个产物到 `dist/`（本机跨平台交叉编译可行）。
- README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。

## §15 M4.2 吞吐判定修正 + 请求日志增强

> 本节由产品经理（主代理）在 M4.2 启动前规格化，是本次派发与验收的需求依据。
> 起因：实测发现吞吐判定把「排队/TTFT」算进了生成速度，健康实例被误记失败并触发无谓回退（用户 m02757 拍板修正）。

### 15.1 目标

1. 吞吐判定只衡量**纯生成速度**，不受排队/首 token 等待干扰。
2. 样本过小（短请求 / 输出太少）时不参与吞吐判定，避免假阳性。
3. 「接了但长时间不出字」由**独立的首 token 超时**管住，不再依赖吞吐判定兜底。
4. 低速（token/s 低）在**任何协议**下都不打断流、不补 `pause_turn`，只记一次失败。
5. 每个请求的日志打印**输入 token / 输出 token / 首 token 耗时 / 生成速度**，便于定位"是排队慢还是生成慢"。

### 15.2 配置新增：`fallback.first_token_timeout`

| 字段 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `first_token_timeout` | Duration 字符串 | `"10s"` | 首 token 超时；0 = 禁用该判定 |

- 可省略（用默认 `10s`）；非法 Duration 或 <0 → 启动/校验失败（fail-closed）。
- 与 `idle_timeout`、实例 `timeout` 并列，语义互不重叠（见 §15.3）。
- 可视化「回退参数」区块新增该输入框（Duration，默认 10s，填 0 禁用），加载读回、保存序列化、非法值阻止保存（复用 §12.4 机制）。

### 15.3 三段超时的职责划分（不重叠）

| 超时 | 计时区间 | 默认 | 触发后 |
|---|---|---|---|
| 实例 `timeout` | 发出请求 → **收到响应头** | 15s | 响应未 committed，可回退（§11.4 情况 2） |
| `fallback.first_token_timeout` | 收到响应头 → **收到首个响应体数据块** | 10s | 同上（此时通常仍未 committed，可回退） |
| `fallback.idle_timeout` | 相邻两个数据块之间 | 30s | 已 committed，按 §13.4 收尾 + 记失败 |

- 三者取各自区间独立计时；实例 `timeout` 不覆盖首 token 等待（这是此前"卡 15 秒"能绕过超时的根因）。
- 非流式请求：`first_token_timeout` 不适用（响应体一次性到达，由实例 `timeout` 覆盖）。

### 15.4 吞吐判定修正

**判定式改为纯生成速度**：

```
生成时长 = 末个数据块到达时间 − 首个数据块到达时间   （流式）
          响应体读取完成时间 − 响应头到达时间        （非流式，作为近似）
生成速度 = 输出 token 数 / 生成时长
```

- **输出 token 数**：OpenAI 取 `usage.completion_tokens`；Anthropic 取 `usage.output_tokens`。
- 判定：`生成速度 < min_tokens_per_second` → 记该实例失败（`recordFailure`），否则 `recordSuccess`。
- **不再**使用「总耗时」作分母。

**最小样本保护（不判定，直接视为成功）**：满足任一即跳过吞吐判定
- 生成时长 `≤ 2s`；
- 输出 token 数 `≤ 100`；
- 拿不到 `usage`（无法测量）。

> 修订（§17.4）：样本门槛为「生成时长 > 2s 且输出 token 数 > 100 才计算」，其余跳过视为成功。

> 说明：跳过判定时**不计失败也不计成功判定的惩罚**——按成功处理（`recordSuccess`），避免因样本不足误伤健康实例。

### 15.5 低速一律不打断、不补 `pause_turn`

- 低速（§15.4 判定命中）在 **OpenAI 与 Anthropic 入口表现一致**：流继续完整转发到结束，**不中断连接、不补任何收尾事件（含 `pause_turn`）**，仅在响应结束后记一次失败。
- `pause_turn` 收尾**只**用于 `idle_timeout` 触发（§13.4），不得用于低速场景。
- 非流式低速同理：响应照常返回，仅记失败。

### 15.6 请求日志增强

每个请求结束时打印一行结构化日志，包含：

| 字段 | 说明 |
|---|---|
| 方法 + 路径 | 如 `POST /v1/messages` |
| `model` | 上游实际使用的模型（改写后的值） |
| 协议 | `openai` / `anthropic` |
| `in` | 输入 token 数（OpenAI `prompt_tokens` / Anthropic `input_tokens`），拿不到写 `n/a` |
| `out` | 输出 token 数（同上），拿不到写 `n/a` |
| `ttft` | 首 token 耗时（毫秒或秒，保留 2~3 位），非流式或拿不到写 `n/a` |
| `gen` | 生成速度 `t/s`（按 §15.4 口径），不可测写 `n/a` |
| `total` | 本次请求总耗时 |

示例：

```
POST /v1/messages -> model="deepseek-v4-flash" (anthropic) total=2.82s ttft=0.71s in=31 out=300 gen=142 t/s
POST /v1/chat/completions -> model="mti-flash" (openai) total=16.67s ttft=14.84s in=28 out=200 gen=109 t/s
```

- 保留既有「slow / slow stream / idle」等告警日志，可与请求行合并或分行，但不得丢失其判定结果。
- 现有 `in <duration>` 的写法可保留或替换为 `total=`，二者均可，但需在设计文档写明最终格式。

### 15.7 明确不做

- 不引入滑动窗口 / 历史采样统计（每次请求独立判定）。
- 不因吞吐判定中断已 committed 的流。
- 不新增除 `first_token_timeout` 外的其他 fallback 配置项。
- 不做请求级指标持久化 / Prometheus 指标导出。

### 15.8 验收标准

- `go build` / `go vet` / `go test`（含 `-race`）退出码 0。
- 配置：`fallback.first_token_timeout` 缺省 10s / 显式生效 / 0 禁用 / 非法报错；可视化页可读写。
- 首 token 超时：mock 上游立即返回响应头、静默超过 `first_token_timeout` 才发数据 → 判失败并回退到下一实例（未 committed，本次请求可切）。
- 吞吐判定：mock 上游快速发头 + 慢速生成（生成速度低于阈值、输出 token ≥20、生成时长 ≥1s）→ 记失败，但**流完整转发、不补 `pause_turn`、不断连接**（两协议均需验证）。
- 最小样本：输出 token <20 或生成时长 <1s 的请求 → 不记失败（即使总耗时较长）。
- 日志：请求行包含 `in` / `out` / `ttft` / `gen` / `total`，取值与实际 usage 一致。
- 既有能力不回归：空闲超时 → Anthropic `pause_turn` 收尾、OpenAI 截断；候选链回退、持续回退、每级独立判断全部保持。
- README / CHANGELOG / design / tests 同步；`-version` 仍 `0.0.1-rc`。

## §16 M4.3 请求日志：客户端标识与来源 IP

> 本节由产品经理（主代理）规格化。用户需求（m02818 / m02838）："我能从代理请求处看出请求来自于哪个客户端的么？比如是claude code还是dsh之类的，如果可以的话，也在日志里显示出来"、"包括来源的ip最好也能显示"。
> 识别依据来自对 Claude Code v2.1.323 的实测抓包（见 §16.2 表）。

### 16.1 目标

在 §15.6 的请求日志行上，追加两个字段：

- `client=` 客户端标识（如 `claude-code/2.1.323`、`openai-python`、`curl`、`unknown`）
- `ip=` 来源 IP

### 16.2 客户端识别规则（按优先级，命中即止）

| 优先级 | 判据 | 标识 |
|---|---|---|
| 1 | `User-Agent` 以 `claude-cli/` 开头 | `claude-code/<version>`（取 UA 中 `/` 后的版本号，直到空格） |
| 2 | 存在请求头 `X-Claude-Code-Session-Id` | `claude-code` |
| 3 | `anthropic-beta` 值包含 `claude-code` | `claude-code` |
| 4 | `User-Agent` 含 `openai-python` 或 `OpenAI/Python` | `openai-python` |
| 5 | `User-Agent` 含 `openai-node` 或 `OpenAI/Node` | `openai-node` |
| 6 | `User-Agent` 含 `anthropic-sdk-typescript` 或 `Anthropic/JS` | `anthropic-js` |
| 7 | `User-Agent` 含 `anthropic-sdk-python` 或 `Anthropic/Python` | `anthropic-python` |
| 8 | `User-Agent` 含 `dsh`（不区分大小写） | `dsh` |
| 9 | `User-Agent` 以 `curl/` 开头 | `curl` |
| 10 | 其他且 `User-Agent` 非空 | UA 前 32 字符（空格前） |
| 11 | `User-Agent` 为空 | `unknown` |

实测样本（Claude Code 2.1.323，供实现参照）：

```
User-Agent: claude-cli/2.1.323 (external, sdk-cli, agent-sdk/0.3.322)
X-Claude-Code-Session-Id: b9333a2e-0164-4149-bede-78341d4ec2fd
x-app: cli
anthropic-beta: claude-code-20250219,context-1m-2025-08-07,...
X-Stainless-Lang: js / X-Stainless-Runtime: node / X-Stainless-Package-Version: 0.128.0
```

- 规则只做**字符串匹配**，不引入新依赖、不做 UA 库解析。
- 匹配大小写：`claude-cli/`、`curl/`、`OpenAI/`、`Anthropic/` 等按不区分大小写匹配。

### 16.3 来源 IP

- 取值顺序：`X-Forwarded-For` 首段（trim 后非空、且不为 `unknown`）→ 否则 `r.RemoteAddr`。
- **必须去掉端口**（`RemoteAddr` 形如 `10.0.0.5:54321`，取 host 部分）。
- **容器部署限制（必须在 README 写明）**：docker bridge 网络下，容器内看到的来源 IP 是 **docker 网关地址**（如 `172.18.0.1`），不是真实客户端 IP；只有 `network_mode: host` 或前置反代透传 `X-Forwarded-For` 才能拿到真实地址。

### 16.4 日志格式（在 §15.6 基础上扩展）

```
POST /v1/messages -> model="mti-flash" (anthropic) client=claude-code/2.1.323 ip=172.18.0.1 total=2.82s ttft=0.71s in=31 out=300 gen=142 t/s
```

- 字段顺序固定：`model` → 协议 → `client` → `ip` → `total` → `ttft` → `in` → `out` → `gen`。
- 认证失败的请求（401）也打印一行，`client` / `ip` 同样带出，便于排查是谁在撞密钥；该行不含 token 数值（禁止打印密钥）。

### 16.5 明确不做

- 不持久化请求日志到文件/数据库（仍为 stdout）。
- 不做 GeoIP 解析、反向 DNS。
- 不采信 `X-Real-IP`（只认 `X-Forwarded-For` 与 `RemoteAddr`）。
- 不打印任何密钥、`Authorization` 值、`x-api-key` 值。

### 16.6 验收标准

- `go build` / `go vet` / `go test`（含 `-race`）退出码 0。
- 单测（构造带不同 header 的请求）：
  - `User-Agent: claude-cli/2.1.323 (...)` → `client=claude-code/2.1.323`；
  - 仅 `X-Claude-Code-Session-Id` → `claude-code`；
  - `anthropic-beta` 含 `claude-code-*` → `claude-code`；
  - `OpenAI/Python 1.40.0` → `openai-python`；
  - `curl/8.1.2` → `curl`；
  - 无 UA → `unknown`；
  - `X-Forwarded-For: 203.0.113.9, 10.0.0.1` → `ip=203.0.113.9`；
  - 无 XFF 时 `RemoteAddr=10.0.0.5:54321` → `ip=10.0.0.5`；
  - 401 请求也打印日志行且不含密钥。
- 端到端：用 `curl -A "claude-cli/2.1.323 (external, cli)"` 打代理，日志出现 `client=claude-code/2.1.323`；用 `curl -H "X-Claude-Code-Session-Id: x"` 无 UA 特征时出现 `client=claude-code`。
- README 写明容器部署下 `ip` 为网关地址的限制。
- README / CHANGELOG / design / tests 同步；`-version` 仍 `0.0.1-rc`。

## §17 M4.4 双认证 + `[1m]` 变体 + 吞吐判定/日志最终口径 + 容器时区

> 本节由产品经理（主代理）规格化，是本次派发与验收的唯一需求依据。本节对 FR-4（认证）、§7.8（`[1m]` 后缀）、§15.4（最小样本）、§15.6/§16.4（日志字段）的修订以本节为准。
> 用户需求（m05033）：①token/s 起始时间从响应开始算，之前的算在请求超时里；②请求时长>2s 且 output_tokens>100 才计算 token/s；③token/s 低于阈值只记失败不打断（含 anthropic）；④日志显示客户端类型/IP/请求模型/实际转发模型/输入输出长度/token/s（满足②才显示）；⑤镜像时区 Asia/Shanghai。

### 17.1 目标

1. Anthropic 入口双认证，兼容 Claude Code 只发 `Authorization: Bearer` 的行为。
2. `role[1m]` 变体正确命中对应角色（`opus[1m]`→`opus` 等），不再漏到 `*` 托底。
3. 吞吐判定最终口径：纯生成速度 + 样本门槛（生成时长 >2s 且输出 >100 token）。
4. 请求日志最终格式：`req_model`（请求原始模型）+ `model`（实际转发模型）+ `client` + `ip` + `in`/`out` + `gen`（满足门槛才显示）。
5. 容器时区 `Asia/Shanghai`。

### 17.2 Anthropic 入口双认证（修订 FR-4）

- Anthropic 入口 `/v1/messages` 认证改为**双认证**：先取 `x-api-key`，若匹配 `auth_token` 则通过；否则回退取 `Authorization: Bearer <token>`，若匹配 `auth_token` 则通过；两者均缺失/不匹配 → 401。
- OpenAI 入口不变（仍 `Authorization: Bearer`）。
- 两入口仍共用同一 `auth_token`；上游转发认证不变（OpenAI `Bearer api_key` / Anthropic `x-api-key api_key`），两层分离、不透传客户端 token。
- 常量时间比较不变（对两个候选头分别 `subtle.ConstantTimeCompare`）。
- 401 错误体（Anthropic 格式）：缺失判定改为「`x-api-key` 为空 且 `Authorization` 不以 `Bearer ` 开头（大小写不敏感）」，此时 message 写 `missing x-api-key or Authorization header`；否则（有头但不匹配）写 `invalid service access token`。

### 17.3 `[1m]` 变体识别（修订 §7.8 末尾）

- R4c 角色命中判定增加 `[1m]` 变体：将 `model` 尾部 `[1m]`（大小写不敏感，`[1m]`/`[1M]` 均可）剥除后，再按 §7.8 判定（精确等于角色名，或以 `claude-<role>-` 开头）。
- 命中后改写逻辑不变：改写为 `roles[<role>]` 对应的模型（`opus[1m]`→`opus`→`roles["opus"]`）。
- 仅 Anthropic 入口角色路由（R4c）生效；OpenAI 入口无角色概念，不受影响。
- 例：`opus[1m]`→`opus` 命中；`sonnet[1m]`→`sonnet`；`claude-opus-4-5[1m]`→剥除后 `claude-opus-4-5`→`opus`。
- 剥除后仍未命中任何角色 → 按 R4d `*` 托底 / R5 回退，行为不变。

### 17.4 吞吐判定最终口径（修订 §15.4）

- 生成时长口径不变：`末个数据块到达时间 − 首个数据块到达时间`（流式）；非流式 `响应体读取完成时间 − 响应头到达时间`（近似）。**起始时间从响应（首数据块）开始，建连/排队/TTFT 不算入**（由 §15.3 的 `timeout`/`first_token_timeout` 管）。
- 输出 token 数：OpenAI `usage.completion_tokens` / Anthropic `usage.output_tokens`。
- **样本门槛（满足全部才计算，否则跳过视为成功）**：
  - 生成时长 `> 2s`；
  - 输出 token 数 `> 100`；
  - 能拿到 `usage`。
- 判定：`生成速度 = 输出 token 数 / 生成时长`；`生成速度 < min_tokens_per_second` → 记失败（`recordFailure`），否则成功。
- 跳过判定（任一生效：生成时长 ≤2s、或输出 ≤100、或拿不到 usage）→ 按成功（`recordSuccess`），**不记失败**。

> 注：本节以「生成时长」作为需求②的"请求时长"，与目标 1 的"从响应开始计算"一致；若用户本意是"总请求时长"，验收时再澄清。

### 17.5 低速的打断策略（修订 §15.5）

- **实时低速**（§19 滑动窗口检测命中）：Anthropic 入口中断流并补 `pause_turn` 收尾；OpenAI 入口不中断、仅记失败。
- **事后低速**（§17.4 判定命中、流已正常结束）：两协议一致，仅在响应结束后记一次失败，不打断、不补 `pause_turn`。
- `pause_turn` 收尾用于 `idle_timeout`（§13.4）与实时低速（§19），两处均为流未自然结束时才补。

### 17.6 请求日志最终格式（修订 §15.6/§16.4）

每请求结束打印一行，字段顺序固定：

```
POST /v1/messages -> req_model="opus[1m]" model="mti-flash" (anthropic) client=claude-code/2.1.323 ip=172.18.0.1 total=2.82s ttft=0.71s in=31 out=300 gen=142 t/s
```

| 字段 | 说明 |
|---|---|
| `req_model` | 客户端请求的原始 `model`（未改写）；拿不到写 `n/a` |
| `model` | 实际转发给上游的 `model`（改写后） |
| 协议 | `openai`/`anthropic` |
| `client` | 客户端标识（§16.2 规则） |
| `ip` | 来源 IP（§16.3 规则） |
| `total` | 总耗时 |
| `ttft` | 首 token 耗时 |
| `in` | 输入 token 数 |
| `out` | 输出 token 数 |
| `gen` | 生成速度 `t/s`，**仅在满足 §17.4 样本门槛（>2s 且 >100 token）时显示数值，否则 `n/a`** |

- 认证失败（401）也打印一行：`req_model`/`model` 写 `n/a`，`client`/`ip` 带出，不含任何密钥。
- 保留既有 `slow`/`slow stream`/`idle` 告警日志（判定结果不丢）。

### 17.7 容器时区 Asia/Shanghai（实现 §14.3）

- 按 §14.3 已规格化内容实现：build 阶段 `apk add --no-cache tzdata`，运行阶段 COPY zoneinfo + `/etc/localtime` + `ENV TZ=Asia/Shanghai`。使容器日志时间戳为北京时间（UTC+8）。

### 17.8 明确不做

- 不改 OpenAI 入口认证（仍 Bearer）。
- 不改上游转发认证方式。
- 不引入滑动窗口/历史采样统计。
- 不因低速中断已 committed 的流（两协议一致）。
- 不持久化请求日志、不做 GeoIP/反向 DNS。
- 不打印密钥、`Authorization` 值、`x-api-key` 值。

### 17.9 验收标准

- `go build`/`go vet`/`go test`（含 `-race`）退出码 0。
- 双认证：仅 `x-api-key` → 200；仅 `Authorization: Bearer` → 200；两者都无 → 401 `missing x-api-key or Authorization header`；两者都有但不匹配 → 401 `invalid service access token`。
- `[1m]` 变体：`opus[1m]` → 命中 `opus` 角色并改写为 `roles["opus"]`；`sonnet[1m]` → `sonnet`；不回归普通 `opus`/`claude-opus-4-5`。
- 吞吐样本门槛：生成时长 ≤2s 或输出 ≤100 → 不记失败；生成时长 >2s 且输出 >100 且速度 < 阈值 → 记失败但流完整（两协议）。
- 日志：含 `req_model`/`model`/`client`/`ip`/`in`/`out`/`total`/`ttft`；`gen` 仅满足门槛时显示数值否则 `n/a`。
- 容器时区：`docker logs` 时间戳为北京时间（UTC+8）。
- README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。

---

## §18 M4.5 实例命名 + 请求日志格式改版

### 18.1 目标

每个实例可配置一个显示名称（`name`），用于请求日志；日志格式改为简洁可读形态。

### 18.2 Instance.name 字段

- `Instance` 新增可选字段 `name`（JSON `name`，`omitempty`），字符串，**仅用于日志显示**，不参与路由/匹配/回退。
- 空/未配置 = 缺省：日志 `name` 位置回退为实例 `base_url`（完整 URL 字符串）。
- 不要求唯一（允许重名，可读性由用户自行保证）。

### 18.3 日志格式改版

单行格式：

```
<timestamp> POST <path> -> <name>/<model> (<req_model>) in <total>s ↑<in> ↓<out> <gen>t/s from <ip> <client>
```

字段映射（原 → 新）：

| 新格式 | 含义 | 原字段 |
|---|---|---|
| `<name>` | 实例显示名（`name` 或 `base_url` 缺省） | 新增 |
| `<model>` | 实际转发模型 | `model` |
| `(<req_model>)` | 请求原始模型（括号内） | `req_model` |
| `in <total>s` | 总耗时，保留 3 位小数 | `total` |
| `↑<in>` | 输入 token（`↑` U+2191） | `in` |
| `↓<out>` | 输出 token（`↓` U+2193） | `out` |
| `<gen>t/s` | 生成速度，**满足样本门槛才显示，否则省略整段** | `gen` |
| `from <ip>` | 来源 IP | `ip` |
| `<client>` | 客户端标识 | `client` |

去掉的字段：`(协议)`、`ttft`。

示例（`DeepSeek` 为实例 `name`）：

```
2026/11/15 11:42:45 POST /v1/messages -> DeepSeek/deepseek-flash (sonnet) in 10.376s ↑1402 ↓674 133t/s from 192.0.2.10 claude-cli/2.1.323
```

### 18.4 边界

- 认证失败（401）/ 无候选：`name`/`model`/`req_model` 显示 `n/a`，`↑`/`↓`/`t/s` 段省略，仍带 `from <ip> <client>`。
- `gen` 满足 §17.4 样本门槛（生成时长 >2s 且输出 >100）才显示 `t/s`，否则省略（不再显示 `n/a`）。
- 保留既有 `slow`/`slow stream`/`idle` 告警日志；其中实例定位信息改用 `name`（与主日志一致）。
- 不打印密钥、`Authorization` 值、`x-api-key` 值（沿用 §17.8）。

### 18.5 配置页

- 可视化配置页实例卡片加「名称」输入框（可选），保存时序列化 `name` 字段。

### 18.6 明确不做

- 不改路由/匹配/回退逻辑（`name` 不参与）。
- 不做 name 唯一性校验。
- 不持久化日志、不做 GeoIP。

### 18.7 验收标准

- `go build`/`go vet`/`go test`（含 `-race`）退出码 0。
- 日志格式匹配 18.3 新格式（含 `↑`/`↓`/`t/s`、`name/model (req_model)` 顺序）。
- `name` 缺省回退 `base_url`；配置了 `name` 则显示 `name`。
- 401 请求：`n/a` 段正确、不含密钥。
- `gen` 满足门槛显示 `t/s`、否则省略。
- README/CHANGELOG/design/tests 同步；`-version` 仍 `0.0.1-rc`。

## §19 M4.5.1 实时低速检测（滑动窗口）

### 19.1 目标

流式响应进行中实时检测「生成速度过低」，Anthropic 入口用 `pause_turn` 打断让客户端续接到下一实例，OpenAI 入口保持不打断只记失败。

### 19.2 度量与换算

- 实时度量：**字节速率**（bytes/s）。token 数在流式中途拿不到（Anthropic `output_tokens` 只在 `message_delta` 给、OpenAI `usage` 只在最后给）。
- 换算常量 `bytesPerToken = 3`（1 token ≈ 3 字节，中英混合粗略近似）。

### 19.3 滑动窗口

- 窗口时长固定 `10s`（常量，不配置化）。
- 维护最近 10s 内收到的数据块队列，每块记录 `(时间戳, 字节数)`。

### 19.4 判定

- 每收到一个数据块，更新窗口：加入新块、剔除 10s 外的旧块。
- 若窗口已填满（从首块起 ≥10s）且 `窗口内总字节 / 10s < min_tokens_per_second × bytesPerToken` → 判定低速。
- 窗口未填满（生成时长 <10s）不判定，避免冷启动误判。

### 19.5 触发动作

- **Anthropic**：关闭上游 body 中断流 → 写 `pause_turn` 尾（`content_block_stop` 若在块内 + `message_delta` `stop_reason=pause_turn` + `message_stop`）→ `recordFailure`。
- **OpenAI**：不中断流（保持「openai 低速不打断」决策），仅 `recordFailure`，流继续正常结束。

### 19.6 与事后吞吐判定的关系

- 实时低速触发（流被中断）时，不再做事后 `judgeThroughput`。
- 流正常结束（未触发实时低速）时，仍做事后 `judgeThroughput`（token 数从 usage 拿）。

### 19.7 日志

- 实时低速触发：`instance <name> slow stream (realtime): <avgBytes>/s`（与事后 `slow stream` 区分）。

### 19.8 明确不做

- 不配置化窗口时长（10s 固定）与 `bytesPerToken`（3 固定）。
- OpenAI 低速不打断（保持 M4.2/M4.4 决策）。

### 19.9 验收标准

- `go build`/`go vet`/`go test`（含 `-race`）退出码 0。
- 流式中途生成速度低于阈值 → Anthropic 补 `pause_turn` 尾并 `recordFailure`、OpenAI 不中断仅 `recordFailure`。
- 生成时长 <10s（窗口未满）不误判。
- 日志 `slow stream (realtime)` 与事后 `slow stream` 可区分。

## 20. gzip 压缩响应体导致解析失败（M4.6）

### 20.1 问题

客户端（Claude Code、deepseek-harness 等）会带 `Accept-Encoding: gzip`。当前 proxy 把该头透传给上游，上游返回 `Content-Encoding: gzip` 的压缩 SSE 体。proxy 原样转发（客户端能解压，功能正常），但 proxy 自身解析全部失效：

- 内部 SSE 状态机（`sseState.feed`）读到的是压缩字节 → 解析不到 `usage` → 日志 `↑n/a ↓n/a`。
- §19 实时低速检测按**字节数**计算速率 → 压缩后字节数小 5~10 倍 → **误判低速** → Anthropic 会错误中断流并补 `pause_turn`（严重）。
- 非流式路径 `capture` 到的也是压缩字节 → `parseUsage` 失败 → `n/a`。

根因证据：`pendingTail` 为 gzip 二进制；带 `Accept-Encoding: gzip` 的请求必然复现、不带则正常。

### 20.2 修复要求

- 转发到上游时**不向上游请求压缩**：`outReq.Header.Del("Accept-Encoding")`。删掉后 Go `http.Transport` 会自行加 `Accept-Encoding: gzip` 并在收到 gzip 响应时**透明解压**、同时移除响应头的 `Content-Encoding` 与 `Content-Length`。
- 双保险：若上游仍返回 `Content-Encoding`（无视请求头）：
  - `gzip`：用 `gzip.Reader` 解压后再转发，并删除响应头的 `Content-Encoding`/`Content-Length`。
  - 其他编码（`br`/`zstd` 等）：不解析（保持透传），记一条 warn 日志说明该编码下无法解析 usage / 字节速率。
- 解压必须发生在 **SSE 解析与 §19 字节窗口之前**——即状态机与速率统计看到的都是解压后字节。
- 发给客户端的响应为明文（不重新压缩）；`isEventStream` 判定仍按 `Content-Type`。

### 20.3 边界

- gzip 解压失败（数据损坏）→ 按上游错误处理：返回 502、`recordFailure`。
- 解压后不需要回填 `Content-Length`（流式走 chunked）。
- 只涉及响应方向；客户端请求体不压缩，无需处理。
- 不解压也不解析的编码，必须在日志中可见，便于排查。

### 20.4 验收标准

- 带 `Accept-Encoding: gzip` 的流式请求：日志 `↑<in> ↓<out>` 正常显示（不再 `n/a`）。
- 同一请求带 gzip 与不带 gzip，§19 低速判定结果一致（不因压缩误判）。
- 真实 DeepSeek Anthropic 入口（会压缩）实测 in/out 正确。
- 客户端仍能收到完整、可解析的响应体。
- `go build`/`go vet`/`go test`（含 `-race`）退出码 0。
