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
| M2 | 增加 Anthropic 协议入口 + 多模型列表与排序 | 进行中 |
| M3 | 模型自动查询/选择（`/v1/models`） | 未开始 |
| M4 | 回退/降级机制（FR-3 完整实现） | 未开始 |
| M5 | Web 配置页面 | 未开始 |
| M6 | 跨平台构建（GitHub Actions）+ Docker（amd64/arm64） | 未开始 |

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
