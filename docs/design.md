# model-router 设计文档（M1 ～ M4.4）

> 需求以 [requirements.md](requirements.md) 为准（FR-4、§7、§8、§9）；测试用例见 [tests.md](tests.md)。

## 1. 范围

- **M1**：项目骨架、OpenAI 单模型透传、FR-4 服务访问认证、构建/版本机制。
- **M2**：Anthropic `/v1/messages`、多后端路由、别名/角色映射、协议化错误体。
- **M3**：`/v1/models`、Web 配置页、管理 REST API、原子 reload、环境变量覆盖（§8.9）。
- **M3.1**：配置改为 `instances`（提供商实例 + 模型映射）、实例展开路由、可视化配置页、probe/validate API。
- **M4.x**：实例粒度回退降级、`fallback` 配置、流式空闲超时、请求日志（客户端/来源 IP）、双认证、`[1m]` 变体、吞吐样本门槛（§18～§23）。
- **M5**：跨平台构建与容器化（§21）。
- **M6/M7**：模型列表对齐官方 schema、协议路径分离与 OpenAI 入口能力字段（§21、§22）。
- **M9**：管理页实时请求监控（WebSocket 推送 + F12 网络面板式视图，§24）。

不做：协议转换（永不）。

## 2. 模块与职责

| 模块 | 文件 | 职责 |
|---|---|---|
| 入口 | `main.go` | 解析 `-config`/`-version`、加载配置、启动 HTTP 服务、优雅停止、启动日志 |
| 配置 | `internal/config` | `instances` 解析/校验、旧字段迁移报错、原子保存、`Duration` JSON、环境变量覆盖 |
| 代理 | `internal/proxy` | 双协议入口、认证、实例展开路由、`/v1/models`、管理 API（config/reload/probe/validate）、实时监控 WS 与事件采集、原子重载 |
| Web | `internal/web` | `go:embed` 内嵌配置页（可视化 + JSON 高级 + 实时监控），原生 HTML+JS，无 CDN |
| 版本 | `internal/version` | 版本单一来源 + `-ldflags` 注入 |

## 3. 请求流程

```
Client                                model-router                         Upstream
  │ POST /v1/messages                     │                                    │
  │ x-api-key: <auth_token>               │ 1. 认证（按路径/凭据判定协议）       │
  ├──────────────────────────────────────▶│ 2. 路径匹配 → 协议                  │
  │                                       │ 3. 取该协议展开后的路由条目          │
  │                                       │ 4. 读请求体（≤10 MiB；超限 413）     │
  │                                       │ 5. R4a → R4c → R4d → R5 选择条目    │
  │                                       │ 6. 复制请求头，设置上游认证头        │
  │                                       ├───────────────────────────────────▶│
  │                                       │ 7. 原样回传状态码/响应头/响应体      │
  │◀──────────────────────────────────────┤    （逐块 flush，SSE 不缓冲）        │
```

管理面请求在 `Server.ServeHTTP` 先按路径分派，再走 Basic Auth；模型面交给当前 handler。

## 4. 配置设计

### 4.1 结构

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "client-token",
  "admin_user": "admin",
  "admin_password": "admin-password",
  "fallback": {"failure_threshold": 3, "degrade_duration": "5m", "min_tokens_per_second": 30},
  "instances": {
    "openai": [
      {
        "base_url": "https://api.deepseek.com",
        "api_key": "sk-...",
        "provider": "deepseek",
        "timeout": "15s",
        "models": ["deepseek-chat", "deepseek-reasoner"]
      }
    ],
    "anthropic": [
      {
        "base_url": "https://api.deepseek.com/anthropic",
        "api_key": "sk-...",
        "provider": "deepseek",
        "timeout": "15s",
        "roles": {"sonnet": "deepseek-v4-flash", "opus": "deepseek-v4-pro", "*": "deepseek-chat"},
        "role_names": {"sonnet": "claude-sonnet-5"},
        "model_capabilities": {
          "deepseek-v4-flash": {"max_input_tokens": 1000000, "max_tokens": 128000, "image_input": true, "thinking": true},
          "deepseek-v4-pro": {"max_input_tokens": 1000000, "max_tokens": 128000, "image_input": true, "thinking": true}
        }
      }
    ]
  }
}
```

| 字段 | 必填 | 默认 | 校验 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 由 `net.Listen` 最终校验 |
| `auth_token` | 是 | — | 去空白后非空 |
| `admin_user` | 否 | `admin` | 去空白；空则回退 `admin` |
| `admin_password` | 否 | 空 | 仅去空白；空=管理面禁用 |
| `fallback` | 否 | 见 §4.1.1 | `failure_threshold` ≥1、`degrade_duration` >0、`min_tokens_per_second` ≥0；非法值启动失败 |
| `instances` | 是 | — | 必须是 `{openai:[...], anthropic:[...]}`；两组合计 ≥1 个实例 |
| `instances.<protocol>[].base_url` | 是 | — | 绝对 http(s) URL；去尾部 `/` |
| `instances.<protocol>[].api_key` | 是 | — | 去空白后非空 |
| `instances.<protocol>[].provider` | 否 | 空 | 去空白+小写；非空必须是内置供应商 id，否则启动失败；空=自定义 |
| `instances.<protocol>[].timeout` | 否 | `15s` | Go duration，必须为正 |
| `instances.openai[].models` | 是 | — | 去空白、丢弃空项、去重、保留顺序；至少 1 项；不得出现 `roles` |
| `instances.anthropic[].roles` | 是 | — | 键 ∈ sonnet/opus/fable/haiku/*（小写归一化）；值去空白且非空；至少 1 项；不得出现 `models` |
| `instances.anthropic[].role_names` | 否 | 空 | 键 ∈ sonnet/opus/fable/haiku（小写归一化，不允许 `*`）；值去空白且非空；非法键/空值启动失败 |
| `instances.<protocol>[].model_capabilities` | 否 | 空 | 键为上游模型名（去空白且非空）；`max_input_tokens`/`max_tokens` ≥0（0 = 未声明）；四个能力字段为布尔；负值/空键启动失败 |

实例不再有 `protocol` 字段：协议由所在分组决定；实例对象中出现 `protocol` 会被严格解码拒绝。

#### 4.1.1 `fallback`

| 字段 | 默认 | 校验 |
|---|---|---|
| `failure_threshold` | 3 | 整数 ≥1 |
| `degrade_duration` | `5m` | Duration 字符串 >0 |
| `min_tokens_per_second` | 30 | 浮点 ≥0；0 = 禁用吞吐判定 |
| `idle_timeout` | `30s` | Duration ≥0；0 = 禁用流式空闲超时 |

- `fallback` 整体可省略；缺失字段使用默认值（由 `Fallback.UnmarshalJSON` 在解析时补全）。
- 显式 `min_tokens_per_second: 0` / `idle_timeout: "0s"` 与“未配置”语义不同：前者分别禁用吞吐判定/空闲超时，后者取默认值。`idle_timeout` 使用 `OptionalDuration`（接受 0、拒绝负值）。

### 4.2 旧配置迁移

`backends` 字段已移除。`Parse` 在严格解码前先探测该字段，出现（含 `null`）即返回：

```
config field "backends" is no longer supported; please migrate to "instances" (see docs/design.md §9.1)
```

`matches`、后端级 `roles` 数组等旧写法随 `backends` 一并移除。

旧 `instances` 数组形态（`instances: [...]`）同样被拒绝：`Parse` 在解码前探测到数组即以「instances 已改为分组结构 {openai, anthropic}」提示迁移。

Go 侧表示：`Config.Instances` 实现为自定义 `config.Instances`（底层 `[]Instance`），`UnmarshalJSON` 把分组对象展开为带内部 `Protocol` 标记的有序切片，`MarshalJSON` 再按协议分组输出。这样 `main.go` 等既有调用（`len`/遍历/`inst.Protocol`）无需改动，同时配置文件与 `/api/config` 始终使用分组 JSON。

### 4.3 保存与环境变量

- `config.Save`：同目录临时文件 + `os.Rename` 原子替换，权限 0600；写入规范化后的配置。
- `Duration.MarshalJSON` 输出生效值（如 `"15s"`），保证写回文件可再次 `Load`。
- 环境变量覆盖：`MODEL_ROUTER_LISTEN`/`MODEL_ROUTER_AUTH_TOKEN`/`MODEL_ROUTER_ADMIN_USER`/`MODEL_ROUTER_ADMIN_PASSWORD`，在 JSON 解码后、`applyDefaults` 前覆盖；非空才覆盖；不作用于 `instances`；不写回文件。

## 5. 实例展开与路由

### 5.1 展开

按分组展开为扁平条目（组内顺序即优先级）：

- `instances.openai` 实例：每个 `models` 项 → `{protocol=openai, base_url, api_key, model=<项>}`；
- `instances.anthropic` 实例：每个 `roles` 键值 → `{protocol=anthropic, base_url, api_key, model=<值>, role=<键>}`。

JSON 对象不保留键顺序，因此 anthropic 条目按固定顺序展开：`sonnet → opus → fable → haiku → *`。同一实例的条目共享上游与 client，角色展开顺序不影响转发目标；固定顺序只保证 `/v1/models` 与日志可复现。

### 5.2 匹配顺序

对请求体顶层 `model`（仅 JSON string 有效；missing/null/数字/布尔/对象/数组/非法 JSON 均视为“无可用 model”）：

1. **R4a 精确**：按条目顺序，`model == 条目.model` → 转发，**不改写**。
2. **R4c 角色**：按条目顺序，条目有具体 `role`（非 `*`）且 `model == role` 或 `model` 以 `claude-<role>-` 开头 → 转发，**改写**顶层 `model` = 条目.model。
3. **R4d 托底**：第一条展开后仍未命中，且有 `role == "*"` 条目 → 第一个 `*` 条目，**改写**。
4. **R5 回退**：其余情况 → 首条目，**不改写**。

说明：

- 判定以“条目”为单位（R4a→R4c），列表靠前条目的角色命中会先于靠后条目的精确命中（§9.2“对展开后条目逐个”的实现）。同一实例内 role 与 model 都命中的情况由 R4a 优先（不改写）。
- 角色匹配区分大小写（与 B7 一致）。
- 多实例声明同一角色/`*` → 按实例顺序取第一个。
- 该协议没有展开条目 → 502，不读 body、不转发。
- 改写实现：`map[string]json.RawMessage` 解析后仅替换顶层 `model` 再 `json.Marshal`；其余字段语义不变，顶层 key 顺序可能变化。
- `[1m]` 变体（`sonnet[1m]`、`opus[1m]`、`claude-opus-4-5[1m]`）在 R4c 判定前剥除尾部 `[1m]`（大小写不敏感），剥除后按角色规则命中并改写（§23.2）；`best`、`opusplan` 仍不做特殊处理：有 `*` 时被托底，否则 R5。
- M4 起，基础实例确定后还会构建候选链并执行回退状态机，详见 §18。

## 6. 认证

- 客户端认证（按入口）：
  - `/v1/chat/completions`：`Authorization: Bearer <auth_token>`；
  - `/v1/messages`、`/anthropic/v1/messages`、`/anthropic/v1/models`：**双认证**——先取 `x-api-key` 匹配，否则回退 `Authorization: Bearer`；
  - `/v1/models`：同为双认证（`x-api-key` 或 Bearer 均可，§22.3），但响应格式始终为 OpenAI，与认证头无关；
  - 常量时间比较（两个候选头各自 `subtle.ConstantTimeCompare`）；失败 401 按入口协议错误体。Anthropic 入口缺失凭据的文案为 `missing x-api-key or Authorization header`，有头但不匹配为 `invalid service access token`（§23.1）。
- 认证先于路由：未认证访问未知路径 → 401（OpenAI 格式）。
- 上游认证：openai 条目 `Authorization: Bearer <api_key>`；anthropic 条目 `x-api-key: <api_key>`；客户端 Authorization/x-api-key 均不进入上游。

## 7. 透传

| 维度 | 规则 |
|---|---|
| 方法 | 原样转发 |
| 路径 | `base_url` + 上游路径（`url.JoinPath`）：`/anthropic/v1/messages` 只在本地存在，上游仍收到 `/v1/messages`（§22.3）；其余入口原样 |
| 查询串 | 原样附加 |
| 请求头 | 剔除 hop-by-hop、`Content-Length`、`Authorization`、`x-api-key`（按协议替换）后复制 |
| 请求体 | 路由前读入内存（≤10 MiB）；R4c/R4d 命中时仅改写顶层 `model`，其余发送原始字节 |
| 响应 | 状态码/响应头/响应体原样；SSE 逐块 flush |

## 8. 请求体与超时

- `http.MaxBytesReader` 限制 10 MiB；超限 413 不转发；其他读取错误 400。
- 每个实例一个 `http.Client`，`timeout` 作用于连接、TLS 握手、等待响应头；不设置总超时，避免截断 SSE。

## 9. 模型列表入口（`/v1/models` 与 `/anthropic/v1/models`）

- 路径决定格式，与认证头无关（§22.3）：
  - `GET /v1/models` → 固定 OpenAI 格式；
  - `GET /anthropic/v1/models` → 固定 Anthropic 格式。
- 两入口都仅 GET；其他 405（`Allow: GET`），错误体按路径所属协议。
- 认证：两入口都先取 `x-api-key` 匹配，否则回退 `Authorization: Bearer`（§22.3 双认证）。
- 不调用上游，本地应答。
- OpenAI（§22.4）：
  - 按 openai 实例顺序收集各实例 `models`，按出现顺序去重；模型 id 重叠时只出现一次。
  - 基础四字段：`{"id","object":"model","created":<unix>,"owned_by":"model-router"}`。
  - 能力字段（可选，同时输出两套别名，值相同）：

    | 字段 | 来源（`model_capabilities[<模型>]`） | 未声明时 |
    |---|---|---|
    | `context_window` / `max_input_tokens` | `max_input_tokens` | 省略两字段 |
    | `max_output_tokens` / `max_tokens` | `max_tokens` | 省略两字段 |
    | `input_modalities` | 由 `image_input`/`pdf_input` 推导 | 至少一个为 `true` 才输出；否则省略 |

  - 能力来源：按 openai 实例顺序找第一个声明该模型的 `model_capabilities`；首个声明该模型的实例未配能力时继续向后找；全部未声明 → 只输出基础四字段。
  - `input_modalities` 构造：恒含 `"text"`；`image_input=true` 追加 `"image"`；`pdf_input=true` 追加 `"pdf"`；`false`/未声明不追加。两项均非 `true` → 整个字段省略。
  - 所有查询参数一律忽略（含 Anthropic 专用参数）。
- Anthropic（M6 §21 官方 schema）：
  - 每个已声明角色返回一项，固定顺序 `sonnet → opus → fable → haiku`；`*` 不返回。
  - `id` 优先取 `role_names`（按 anthropic 实例配置顺序的第一个声明），否则内置官方名：

    | 角色 | id | display_name | created_at |
    |---|---|---|---|
    | sonnet | `claude-sonnet-5` | Claude Sonnet 5 | `2026-01-01T00:00:00Z` |
    | opus | `claude-opus-5` | Claude Opus 5 | `2026-01-01T00:00:00Z` |
    | fable | `claude-fable-5` | Claude Fable 5 | `2026-01-01T00:00:00Z` |
    | haiku | `claude-haiku-4-5` | Claude Haiku 4.5 | `2025-10-01T00:00:00Z` |

  - `display_name`/`created_at` 按 `id` 查内置表；不在表中的自定义 id 回退为 `display_name=id`、`created_at=当前时间`（RFC3339）。
  - 恒定字段：`type="model"`、`lifecycle="active"`、`deprecated_at=null`、`retires_at=null`、`line=<角色>`。
  - `max_input_tokens`/`max_tokens`：取该角色映射到的上游模型（首个声明该角色的实例的 `roles[角色]`）在**首个**声明该模型的 `model_capabilities` 中的值；未声明或 0 → `null`。
  - `capabilities`：同一份声明中的 `image_input`/`pdf_input`/`thinking`/`structured_outputs`，每项输出 `{supported: <bool>}`；未声明的子项不输出；四项全未声明时整体为 `null`。
  - 列表级：`data`、`first_id`、`last_id`、`has_more`；空页时 `first_id`/`last_id` 为 `null`。
  - 查询参数：
    - `limit`：1–1000，默认 20；非整数、0、负数、>1000 → 400。
    - `before_id`/`after_id`：互斥，同时出现 → 400；未知 id → 空页（`data:[]`、`has_more:false`、first/last `null`）。`after_id` 从该 id 之后取 `limit` 条；`before_id` 取该 id 之前紧邻的 `limit` 条；`has_more` 表示翻页方向上仍有数据。
    - `lifecycle`：可重复/逗号分隔，仅 `active`/`deprecated`/`retired`，最多 3 个；缺省为 `active`+`deprecated`。本代理只产生 `active`，故 `deprecated`/`retired` 过滤结果为空。
  - 非法查询 → 400，错误体为 Anthropic 格式（`invalid_request_error`）。

## 10. 管理页、管理 API 与重载

- 路径分派：`/admin`、`/admin/...`、`/api`、`/api/...` 为管理面；`admin_password` 为空 → 404；否则 Basic Auth（用户名/密码均常量时间比较），失败 401 + `WWW-Authenticate: Basic realm="model-router"`。
- `GET /api/config`：当前生效配置（含真实 key）。
- `PUT /api/config`：`config.Parse` 全量校验 → `config.Save` 原子写回，不生效；失败 400 不写文件。
- `POST /api/reload`：`config.Load` → `proxy.New` → `atomic.Pointer` 整体替换；失败保持旧状态。不重新绑定监听地址。
- `GET /api/providers`：返回内置供应商清单（`providers.List()`），仅 GET。
- `Server` 持有 `atomic.Pointer[serverState{cfg, handler}]`；每请求加载一次，在途请求使用旧 handler。
- `Server` 另持有一个不随 reload 重建的 `monitorHub`；每次 `buildState` 把同一个 hub 注入新 handler，因此在途/后续请求与已有 WS 连接在 reload 后继续可用（§27.6）。
- 管理页：可视化模式（双 tab、实例卡片、测试连接、查询模型、勾选/手填、角色对外名、模型能力、保存）+ JSON 模式（校验通过才启用保存）+ 实时监控模式（§27）。页面为内嵌静态资源。

## 11. Probe 与 Validate

### 11.1 `POST /api/probe/test`

- 请求体 `{protocol, base_url, api_key, timeout?}`（严格解析，未知字段拒绝）。
- 校验协议/URL/密钥；timeout 默认 15s。
- **按协议选择探测端点**：
  - openai：`GET {base_url}/v1/models`（`Authorization: Bearer`）。
  - anthropic：`POST {base_url}/v1/messages`，最小请求体
    `{"model":"ping","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`，
    头 `x-api-key` + `anthropic-version: 2023-06-01` + `Content-Type: application/json`。
    原因：部分 Anthropic 兼容上游（如 DeepSeek）未实现 `/v1/models`。
- 判定（两种协议一致）：
  - 网络错误/超时 → `ok:false`（message=错误原因）；
  - `401`/`403` → `ok:false`（message=`api_key 无效（认证失败）`）；
  - `404` → `ok:false`（message=`base_url 路径无效`）；
  - `2xx` → `ok:true`；
  - 其余非 2xx（400/422/429 等）→ `ok:true`（端点存在、认证通过；占位 model 被拒属预期，message 说明原因）。
- HTTP 状态恒为 200（结果在 body）；请求体非法 → 400。不修改配置、不缓存。

### 11.2 `POST /api/probe/models`

- 请求体 `{protocol, base_url, api_key, provider?, timeout?}`；`provider` 非空且必须是已知 id，否则 400。
- 默认 `GET {base_url}/v1/models`（openai `Authorization: Bearer`；anthropic `x-api-key` + `anthropic-version: 2023-06-01`）。
- 若 `provider` 对该 `protocol` 声明了 `models_override`，改用 override 的 `base_url`，认证按 `override.auth`（`bearer` → `Authorization: Bearer`；`x-api-key` → `x-api-key` + `anthropic-version`）。解析 `data[].id`（跳过空 id）。
- 成功 → 200 `{models:[...], capabilities?:{<模型名>:{...}}}`；网络错误/非 2xx/无法解析/缺少 `data` → 502 `{error}`（前端引导手动输入）。`data: []` 视为合法的空列表。
- `capabilities` 为 M6 新增的可选字段：按上游模型名收集能力元数据（同时识别 Anthropic 官方 schema 与 OpenAI/DeepSeek 风格字段名），形状与配置项 `model_capabilities` 一致；`models` 字段形状不变（向后兼容）。仅输出至少声明了一项元数据的模型，全部未声明时省略该字段。解析为 best-effort：能力元数据缺失或类型异常不影响 `models` 解析；窗口 `0`/负数不输出；`supported:false` 保留为显式 `false`。
  字段兼容表（同一行从左到右取第一个可用值；Anthropic 官方字段与别名同时声明时官方字段优先）：

  | 能力 | 字段名（按优先级） |
  |---|---|
  | 输入窗口 | `max_input_tokens` → `context_window` → `context_length` → `contextWindow` → `limit.context` |
  | 输出上限 | `max_tokens` → `max_output_tokens` → `maxTokens` → `maxOutputTokens` → `max_completion_tokens` → `top_provider.max_completion_tokens` → `limit.output` |
  | 图片输入 | `capabilities.image_input.supported`；未声明时回退 `input_modalities` 含 `image` |
  | PDF 输入 | `capabilities.pdf_input.supported`；未声明时回退 `input_modalities` 含 `pdf` |
  | 思考 | `capabilities.thinking.supported`；未声明时回退 `effort` 存在（对象或非空字符串）或 `reasoning` 字段存在 |
  | 结构化输出 | 仅 `capabilities.structured_outputs.supported` |

  `input_modalities` 数组存在即视为对输入模态的显式声明：不含 `image`/`pdf` 时输出显式 `false`；数组缺失时对应布尔字段未声明（不输出）。数值仅接受正整数 JSON 字面量，`0`、负数、小数、指数与字符串均视为未声明。
- `404` 使用明确文案：`该上游不支持模型列表查询（未实现 /v1/models），请手动输入模型名`，而不是裸的 HTTP 状态码。
- 响应体读取上限 4 MiB。

### 11.3 `POST /api/config/validate`

- 请求体为完整配置 JSON；走 `config.Parse`（含 instances 结构、互斥、默认值、环境变量覆盖语义），不写回、不生效。
- 通过 → 200 `{ok:true}`；失败 → 400 `{ok:false, error}`。

## 12. 错误映射

OpenAI：`{"error":{"message","type","code"}}`；Anthropic：`{"type":"error","error":{"type","message"}}`。

| 场景 | HTTP | OpenAI | Anthropic |
|---|---|---|---|
| 缺/错客户端 token | 401 | `invalid_request_error` + `missing_api_key`/`invalid_api_key` | `authentication_error` |
| 未知路径 | 404 | `invalid_request_error` + `not_found` | — |
| `/v1/models` 非 GET | 405 | `invalid_request_error` + `method_not_allowed` | `invalid_request_error` |
| 该协议无实例 | 502 | `server_error` + `upstream_error` | `api_error` |
| 上游失败/超时 | 502 | `server_error` + `upstream_error` | `api_error` |
| 请求体超限 | 413 | `invalid_request_error` + `request_too_large` | `request_too_large` |

管理 API 错误体：`{"error":"..."}`；成功：`{"status":"ok"}` 或业务字段。管理面禁用时 404（OpenAI 格式）。

## 13. 版本与构建

- 版本唯一来源 `internal/version/version.txt`（`0.1.1`），构建脚本注入 `-ldflags -X`。
- `CGO_ENABLED=0`、`-trimpath`、`-s -w`；产物 `dist/model-router-<version>-<goos>-<goarch>[.exe]`。

## 14. 关键决策与未覆盖情形

- **D-M3.1-1 roles 对象顺序**：标准库 map 不保留键顺序；展开固定为 sonnet→opus→fable→haiku→`*`。同实例条目共用上游，路由结果不变；仅 `/v1/models` 与日志顺序受影响。
- **D-M3.1-2 互斥判定基于非空集合**：`models: []` / `roles: {}` 视为未配置；若要求“字段出现即违规”需额外探测原始 JSON。
- **D-M3.1-3 跨条目优先级**：按条目顺序逐条 R4a→R4c，靠前条目的角色命中会先于靠后条目的精确命中；与 §9.2“对展开后条目逐个”一致。`*` 是全局第二轮。
- **D-M3.1-4 roles 键归一化冲突**：`"Sonnet"` 与 `"sonnet"` 同时出现时归一化后互相覆盖（JSON 解码本身也会对重复键取后者）。
- **D-M3.1-5 probe 失败状态码**：`probe/test` 统一 200 + `ok:false`；`probe/models` 失败 502；需求未规定具体状态码。
- **D-M3.1-6 probe 响应上限**：4 MiB，超出会被截断并解析失败。
- **D-M3.1-7 validate 语义**：只做 `Parse`，不检查当前进程能否 build handler（两者校验集基本相同）。
- **D-M3.1-8 PUT 写回规范化配置**：默认值/trim/排序后的 JSON，不是原始字节。
- **D-M3.1-9 reload 不重绑监听**：`listen` 变更需重启。
- **D-M3.1-10 管理页保存与 env**：GET 返回 env 覆盖后的生效值，页面保存会把当前值写回文件（显式动作）。
- **D-M3.1-11 空 roles/models 语义**：见 D-M3.1-2。
- **D-M3.1-12 未认证未知路径**：401（认证优先），与 B2 一致。

## 15. 未纳入当前范围

- 回退降级/失败计数（M4）；自动健康检查；配置自动热加载（仅手动 reload）；session 登录；协议转换；跨平台 CI/Docker（M5）。

## 16. 已知限制

- 实例的模型映射是静态声明，不自动同步上游变更（可用 probe 手动查询）。
- 请求体需读入内存（≤10 MiB）。
- 服务本身不终止 TLS，公网部署应置于反向代理/网关之后。
- 管理页与 probe 无请求体大小上限（仅管理员可调用）。

## 17. 供应商预设（`internal/providers`，§10）

### 17.1 数据与查询

- 新包 `internal/providers` 保存硬编码预设：`Provider{ID, Name, OpenAIBaseURL, AnthropicBaseURL, ModelsOverride}`。
- `List()` 返回全部 11 个预设的副本（深拷贝 override map）；`Get(id)` 按 id 查询。
- 预设数据不参与路由/转发，仅用于 UI 选择与模型列表端点解析。

### 17.2 `Instance.provider`

- 可选字段，JSON `provider`；`TrimSpace` + 小写后，非空必须是已知 id，否则 `config.Parse` 失败（fail-closed）。
- 空=自定义实例。`provider` 不改变路由/转发使用的 `base_url`/`api_key`。
- `provider` 支持已知 id 但未校验“该供应商是否支持实例协议”，以数据声明为准（UI 会阻止不支持的组合）。

### 17.3 `models_override`（通用机制）

- 数据结构：`map[protocol]{base_url, auth}`；`auth ∈ {bearer, x-api-key}`。
- 解析入口：`providers.ModelsSource(providerID, protocol, fallbackBaseURL, fallbackAuth)`；命中 override 时返回 override 的 base_url/auth，否则返回 fallback。
- 目前仅 DeepSeek 声明 `anthropic → {base_url: https://api.deepseek.com, auth: bearer}`（其 Anthropic 入口未实现 `/v1/models`）。
- 不写死任何供应商判断；未来同类问题只需在预设数据中新增一条 override。

### 17.4 API 与 UI

- `GET /api/providers`（admin Basic Auth）返回预设数组；`admin_password` 为空 → 404，非 GET → 405。
- 配置页「添加实例」先弹供应商选择框：列出 name + 支持的协议 + 自定义；选内置时按当前 tab 协议填 `base_url` 并记录 `provider`，该协议无 base_url 时阻止；「查询模型」请求带上 `provider`。
- JSON 高级编辑直接支持 `provider` 字段。

### 17.5 决策与未覆盖

- **D-M3P-1 provider 大小写**：归一化为小写（如 `DeepSeek` → `deepseek`），与 protocol 处理一致；需求只要求“已知 id”。
- **D-M3P-2 provider 与协议匹配**：启动时不校验供应商是否声明了该协议（`provider` 只是来源标记）；不匹配组合仍可保存，UI 层阻止新增。
- **D-M3P-3 probe 未知 provider**：`probe/models` 显式返回 400；`probe/test` 接受并忽略该字段（不改变探测行为）。
- **D-M3P-4 override 认证**：`x-api-key` 时同时发送 `anthropic-version: 2023-06-01`；`bearer` 时使用 `Authorization: Bearer`，不再发送 x-api-key。
- **D-M3P-5 `/api/providers` 响应形状**：直接返回数组（非 `{providers:[]}` 包装）。
- **D-M3P-6 预设数据不校验协议组合**：如 `openai` 供应商没有 anthropic base_url 属正常；UI 标注“不支持”。

## 18. 回退降级（M4，§11）

### 18.1 候选链

- 先按 §9.2 规则确定**基础实例**（逐实例 R4a → R4c，`*` 为全局第二轮），再从基础实例开始收集该协议后续实例。
- 候选 = `{实例, 上游模型, 是否改写 body}`；同一实例只出现一次。
- OpenAI：实例 `models` 含请求 model → 上游 model = 请求 model、不改写；否则 → `models[0]`、改写。
- Anthropic：
  - 角色 R（`sonnet/opus/fable/haiku` 或 `claude-<role>-` 前缀）：含 `roles[R]` → 改写为该值；否则含 `*` → 改写为 `roles["*"]`；否则跳过。
  - 精确命中某实例的 roles **值**（R4a）：该实例及其后含相同值的实例不改写；其余含 `*` 的实例改写为 `roles["*"]`，无 `*` 的跳过（§11.2 未覆盖 exact 链，见 D-M4-1）。
- R5（无 model/null/非字符串/非法 JSON/未命中）：基础 = 首个实例；OpenAI 全部实例为候选且**不改写**；Anthropic 仅取声明 `*` 的实例且不改写。若 Anthropic 无任何 `*`，兜底尝试首个实例（保持 M3 R5，见 D-M4-2）。

### 18.2 状态机

每个 `instanceRuntime` 维护 `consecutiveFailures`、`degradedUntil`，由 `sync.Mutex` 保护：

1. 请求开始时按候选链顺序；跳过 `now < degradedUntil` 的实例。
2. 依次尝试未跳过的候选；成功 → 计数清零、`degradedUntil` 清零。
3. 失败 → 计数 +1：
   - `degradedUntil` 非零且已过期（回退期结束的探测）→ `degradedUntil = now + degrade_duration`（重新计时，不依赖计数）；
   - 否则计数 ≥ `failure_threshold` → `degradedUntil = now + degrade_duration`。
   本次请求继续下一个候选。
4. 所有候选都被跳过 → 只强制尝试链头一次。
5. 全部失败 → 502（协议错误体）。

成功 = 收到任意 HTTP 响应（含 4xx/5xx；§11.4 只把连接/超时/吞吐列为失败）。重试只发生在响应头写出之前；响应头一旦写出即不可回退。

### 18.3 失败判定与吞吐

- 连接失败 / dial 超时 / 等待响应头超时（实例 `timeout`）→ 可重试失败，记录计数并换下一候选。
- 响应体复制错误（含客户端断开）→ 已提交，记录失败但不重试。
- 非流式响应：边转发边捕获响应体（上限 4 MiB），完成后解析 usage：OpenAI `completion_tokens`、Anthropic `output_tokens`；生成速度 = `tokens / 生成时长`（`响应体读取完成 − 响应头到达`），满足样本门槛（≥100 token 且生成时长 >2s）后，`生成速度 < min_tokens_per_second` → 记录失败（响应已发出，不影响本次结果）。
- 流式响应（M4.1）：转发 SSE 时解析每行 `data:` JSON 中的 usage；流正常结束后按同一口径（`末块到达 − 首块到达`）与同一门槛判定，**只记失败、不打断流**。
- **样本门槛（M4.4）**：输出 token ≤100、生成时长 ≤2s 或拿不到 usage → 跳过判定并 `recordSuccess`，不记失败（§23.3）。
- 流式空闲超时（M4.1）：committed 后以每个 chunk 到达时 `Reset` 的 `time.AfterFunc` 计时；`idle_timeout` 内无新数据 → 关闭上游读取、记实例失败，并按协议补发收尾/截断（§20）。
- 缺少 usage 或非流式响应体超过捕获上限 → 不判定（按成功记录）。

### 18.4 配置页实例拖动排序

- 可视化模式实例卡片标题可拖动（HTML5 drag & drop）；仅允许同协议实例之间重排。
- 重排只调整该协议实例在 `instances` 数组中的相对顺序，另一协议实例在全局数组中的位置保持不变；保存时按当前顺序序列化。
- 顺序即候选链优先级（越靠前越优先）。

### 18.5 决策与未覆盖

- **D-M4-1 Anthropic 精确模型候选链**：§11.2 只定义“角色 R”链；实现把 R4a exact 请求按“值相等不改写 / 否则用 `*` 改写”处理，作为最小扩展。
- **D-M4-2 R5 无 `*` 兜底**：§11.2 说无 `*` 则跳过，可能导致空链；实现回退到首个实例且不改写，保持 M3 R5 行为。
- **D-M4-3 成功定义**：任意 HTTP 响应（含 4xx/5xx）都视为实例成功并清零失败计数；仅连接/超时/吞吐计失败。
- **D-M4-4 吞吐范围（M4.4 修订）**：仅 usage 可解析（非流式响应体 ≤4 MiB；流式取 SSE 中最近一次 usage）且满足样本门槛（>100 token 且生成时长 >2s）时判定；`min_tokens_per_second=0` 时完全不判定；`completion_tokens=0` 因样本不足被跳过，不会误判低吞吐。
- **D-M4-5 耗时口径（M4.4 修订）**：生成时长只计生成窗口——流式为「末个数据块 − 首个数据块」，非流式为「响应体读完 − 响应头到达」；建连/TTFT/回退耗时不计入，由 `timeout`/`first_token_timeout` 管。
- **D-M4-6 reload 清空回退状态**：reload 重建 handler，计数与回退期随之清零（状态仅内存）。
- **D-M4-7 参数来源（M4-config 修订）**：阈值 / 回退时长 / 吞吐阈值默认 3 / 5m / 30，可由顶层 `fallback` 覆盖（`min_tokens_per_second=0` 禁用吞吐判定）；`NewWithOptions` 仍可注入测试时钟与参数。
- **D-M4-8 基础实例选择**：沿用 §9.2 的逐实例 R4a→R4c 与全局 `*` 第二轮；因此靠前实例的角色命中会先于靠后实例的精确命中（与 M3 一致）。
- **D-M4-9 全降级强制链头失败**：仍按计数更新，`degradedUntil` 会从当前时间重新延长 `degrade_duration`。
- **D-M4-10 客户端断开**：响应体复制失败按失败记录（已提交，不重试）。

## 19. 配置结构修正（M4-config，§12）

### 19.1 Go 表示与 JSON 形态

- 配置文件与 `/api/config` 使用分组 JSON：`instances: {openai:[...], anthropic:[...]}`；实例不再包含 `protocol`。
- Go 运行时用 `config.Instances`（底层 `[]Instance`）保存扁平、带内部 `Protocol` 标记的有序切片：
  - `UnmarshalJSON` 按 `openai`、`anthropic` 顺序展开分组（组内顺序保留）；
  - `MarshalJSON` 按 `Protocol` 分组输出，空组输出 `[]`；
  - 实例的 `Protocol` 字段标记为 `json:"-"`，因此 JSON 中出现 `protocol` 会被 `DisallowUnknownFields` 拒绝。
- 这样 `main.go` 等依赖 `len(cfg.Instances)` / `inst.Protocol` 的既有代码无需改动。
- 跨协议全局顺序不保留（解析后固定为 openai 组在前）；路由按协议分组，跨协议顺序无语义。

### 19.2 fallback 默认值与显式 0

- `Fallback.UnmarshalJSON` 逐字段检测是否出现：缺失字段填默认值（3 / `5m` / 30），显式值校验后保留。
- `min_tokens_per_second` 的“显式 0”与“未配置”不同：前者禁用吞吐判定，后者使用 30；`Fallback.Values()` 负责该区分。
- 直接构造的 `Config`（如测试）未走解析时会使用默认值；`min_tokens_per_second` 的 0 在未配置时按默认 30 处理。
- `Duration.MarshalJSON` 使用紧凑格式（`5m` 而非 `5m0s`），保证 `fallback.degrade_duration` 与示例一致。

### 19.3 迁移与错误提示

- 旧 `backends`：沿用 `rejectLegacyBackends`。
- 旧 `instances` 数组：`Parse` 解码前探测到数组即返回 `instances 已改为分组结构 {openai, anthropic}，请迁移配置`。
- 实例内出现 `protocol`：严格解码报 `unknown field "protocol"`。
- 两组合计 0 个实例：启动/validate 失败。

### 19.4 运行时与页面

- `proxy.New` 从 `Config.Fallback` 取阈值/时长/吞吐阈值；`NewWithOptions` 的非零参数覆盖配置（供测试）。
- 候选链、状态机、失败判定、`/v1/models`、probe、provider 逻辑不变。
- 配置页可视化模式从 `instances.openai` / `instances.anthropic` 读取；拖动只在组内重排；保存按组序列化。页面顶部「回退参数」区块提供 `failure_threshold` / `degrade_duration` / `min_tokens_per_second` / `idle_timeout` 四个输入框：加载时从 `fallback` 回填（显式 0 显示 0，缺失显示默认值）；保存前前端校验（阈值 ≥1、时长 >0/空闲超时 ≥0、吞吐 ≥0），通过后再调用 `/api/config/validate` 兜底；留空字段不写入（走默认）。JSON 模式可直接编辑。

### 19.5 决策与未覆盖

- **D-M4C-1 Go 扁平表示**：JSON 分组但 Go 内部扁平，属于实现选择；对外契约只以 JSON 为准。
- **D-M4C-2 空组**：允许 `openai: []` 或 `anthropic: []`，只要合计 ≥1；空组在保存/API 输出中保留为空数组。
- **D-M4C-3 fallback 部分字段**：可只配置其中一项，其余使用默认；序列化输出总是包含三项生效值。
- **D-M4C-4 环境变量**：仍只覆盖 `listen`/`auth_token`/`admin_user`/`admin_password`，不覆盖 `fallback`/`instances`。
- **D-M4C-5 页面 fallback 编辑（M4-fallback-ui / M4.1 修订）**：可视化模式提供 fallback 输入框（含 `idle_timeout`）；加载时回填生效值（显式 0 保留为 0），保存前前端校验并复用 `/api/config/validate`；留空字段不写入、由后端默认补齐。若前端输入非法，切到 JSON 模式时使用已加载的 fallback 而不是阻止切换。

## 20. 流式中途切换（M4.1，§13）

### 20.1 空闲超时计时

- 仅对 `Content-Type: text/event-stream` 的已 committed 响应启用（`idle_timeout > 0`）。
- 使用 `time.AfterFunc(idle_timeout, ...)`；每个数据块成功读取后调用 `Timer.Reset(idle_timeout)`。回调置位 `idleHit` 并 `Close` 上游响应体，从而解除阻塞中的 `Read`。
- 首字节等待不经过该计时器：`client.Do` 仍受实例 `timeout`（连接 / TLS / 响应头）约束。
- 上游关闭导致的 `Read` 错误若由空闲回调触发（`idleHit=true`）则按空闲处理，否则按复制错误处理。

### 20.2 触发后的处理

- **Anthropic**：关闭上游后向客户端补发：
  - `event: content_block_stop`（仅当 SSE 状态显示仍在内容块内，index 取最近 `content_block_start`）；
  - `event: message_delta`，`delta.stop_reason="pause_turn"`、`stop_sequence=null`、`usage.output_tokens` 取已见值（未见到则为 0）；
  - `event: message_stop`。
  按 SSE 逐行解析 `data:` JSON 维护 `inBlock`、`blockIndex`、usage、`finished`。
- **OpenAI**：通过 `http.Hijacker` 关闭底层连接，使客户端看到截断的流（不补 `[DONE]`）。若 ResponseWriter 不支持 Hijack（如 HTTP/2），仅结束 handler（同样没有 `[DONE]`）。
- 两种协议都调用 `recordFailure` 走 §11.3 状态机；因响应已 committed，不换候选重试。
- 为避免补发的事件与上游 `Content-Length` 冲突，SSE 响应在写响应头前移除 `Content-Length`，其余响应头照常透传。

### 20.3 流式吞吐（低速）

- SSE 转发时解析每个 `data:` JSON 中的 `usage.completion_tokens`（OpenAI）或 `usage.output_tokens`（Anthropic），取最近值。
- 流正常结束后按“首块→末块”的生成时长计算 `tokens/s`；仅在满足样本门槛（>100 token 且 >2s，§23.3）时判定，低于 `min_tokens_per_second` 仅 `recordFailure`，不打断、不回退本次请求（§13.5）。
- `min_tokens_per_second=0`、未见到 usage 或样本不足时跳过判定。

### 20.4 配置与页面

- `fallback.idle_timeout`：`OptionalDuration`（接受 `0s`，拒绝负值/非法值），默认 `30s`；`Fallback.Values().IdleTimeout` 供 `proxy.New` 使用，`Options.IdleTimeout>0` 可覆盖（测试用）。
- 可视化「回退参数」新增“空闲超时”输入框：加载回填（显式 0 显示 0，缺省显示 30s），保存时按 Go duration 语法校验（允许 0），留空不写入。

### 20.5 决策与未覆盖

- **D-M4.1-1 计时竞态**：chunk 到达与计时器触发恰好同时发生时，结果取决于 `Reset` 与回调谁先执行；任一边界都视为合法（数据已到则重置，已触发则按空闲收尾）。
- **D-M4.1-2 Hijack 不可用**：HTTP/2 等无法强制截断连接时，退化为正常结束 handler（仍无 `[DONE]`），客户端可能看到“干净结束”；不额外注入错误。
- **D-M4.1-3 收尾 usage**：取 SSE 中最近一次出现的 output_tokens；完全没有则为 0。
- **D-M4.1-4 已收到 message_stop 后空闲**：不再补发 tail（避免重复 `message_stop`），但仍记录实例失败并结束响应。
- **D-M4.1-5 SSE 解析范围**：仅处理 `data:` 行与已知事件类型；格式非法的行忽略，不影响转发字节。
- **D-M4.1-6 空闲时吞吐**：空闲触发分支直接记失败并返回，不再做吞吐判定，避免重复计数。
- **D-M4.1-7 非流式不受影响**：空闲超时仅用于 SSE；非流式仍使用原有整体读取与 usage 判定。
- **D-M4.1-8 上游正常结束**：收到 EOF 即视为完成，计时器停止；不会因流结束后等待而触发空闲。

## 21. 跨平台构建与容器部署（M5，§14）

### 21.1 GitHub Actions（`.github/workflows/build.yml`）

- 触发：`push`（所有分支与 `v*` tag）、`pull_request`。
- `build` job：matrix 6 个组合（windows-latest/windows、ubuntu-latest/linux、macos-latest/darwin 各配 amd64/arm64）；`actions/setup-go@v5` 使用 `1.27.x` 与模块缓存；`CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ...Version=${version}"` 产出 `dist/model-router-<version>-<goos>-<goarch>[.exe]`，逐组合上传 artifact（同名规则）。
- 版本解析：读取 `internal/version/version.txt`；`refs/tags/v*` 时取 tag 去掉 `v` 前缀。每一步用 bash（windows-latest 亦使用 Git Bash），`fail-fast: true`。
- `release` job：仅 tag，`needs: build`；下载全部 artifact 合并后由 `softprops/action-gh-release@v2` 发布。
- `docker` job：ubuntu-latest；checkout → 解析版本与镜像名（固定为 `wuzm219/model-router`，Docker Hub）→ `docker/setup-buildx-action` → tag 时登录 Docker Hub（`DOCKERHUB_USERNAME`/`DOCKERHUB_TOKEN`，未配置则跳过推送） → `docker/build-push-action` 构建 `linux/amd64,linux/arm64`；tag 时推送 `:<version>` 与 `:latest`，非 tag 只构建不推送。无需 `packages: write`；凭证走 Docker Hub secrets，缺省时只构建不推送。

### 21.2 Dockerfile

- 两阶段：`golang:1.27-alpine AS build` → `gcr.io/distroless/static-debian12:nonroot`。
- build 阶段接收 `TARGETOS`/`TARGETARCH`（buildx 自动注入）与可选 `VERSION`；未传 `VERSION` 时读取 `internal/version/version.txt`，保证版本单一来源。
- `CGO_ENABLED=0` 静态编译；运行阶段只拷二进制，`USER nonroot:nonroot`、`EXPOSE 18080`、`ENV MODEL_ROUTER_LISTEN=0.0.0.0:18080`；入口固定 `-config /home/nonroot/config/config.json`。
- **容器时区（M4.4，§17.7）**：build 阶段 `apk add --no-cache tzdata`，运行阶段从 build 阶段 COPY `zoneinfo/Asia/Shanghai` 到同名路径并覆盖 `/etc/localtime`，`ENV TZ=Asia/Shanghai`；distroless 不自带 tzdata，容器日志时间戳为北京时间（UTC+8）。
- 镜像不内置配置；运行时挂载或通过 `MODEL_ROUTER_*` 覆盖。

### 21.3 docker-compose.yml

- 单服务 `model-router`：`build: .`、端口 `18080:18080`、`./.docker` 目录读写挂载到 `/home/nonroot/config`（容器内 `/home/nonroot/config/config.json`）、`MODEL_ROUTER_LISTEN=0.0.0.0:18080`、`restart: unless-stopped`。
- 作为部署示例，真实密钥由本地 `.docker/config.json` 提供（gitignore）；目录挂载（而非单文件）使 Web 页保存配置时的原子 rename 在同设备完成。

### 21.4 build-all.sh

- 读取 `internal/version/version.txt`，循环 `GOOS × GOARCH`，以环境变量调用现有 `build.sh`（不修改 `build.sh`）；产物落 `dist/`。
- 任一构建失败因 `set -e` 立即非零退出；结束后校验 6 个文件存在并打印清单。

### 21.5 决策与未覆盖

- **D-M5-1 版本来源**：本地构建统一用 `version.txt`；CI tag 构建以 tag 版本为准（可能不同于文件值）。`-version` 仍由 ldflags 注入值决定。
- **D-M5-2 runner 与目标**：每个 runner 只构建自身 GOOS 的两个架构（windows runner 构建 windows/arm64 等交叉编译），共 6 个产物，不额外增加 runner 数。
- **D-M5-3 CI Go 版本**：`1.27.x` 由 setup-go 解析最新补丁；`go.mod` 仍声明 `go 1.27.1`。
- **D-M5-4 PR 的 Docker 构建**：非 tag 时 buildx 仍构建双架构（arm64 走 QEMU 模拟）但不登录/不推送；fork PR 无写权限也不受影响。
- **D-M5-5 镜像不含配置/健康检查**：配置通过挂载提供；未添加 HEALTHCHECK（需求标注非必需）。
- **D-M5-6 distroless 调试能力**：运行镜像无 shell，只能通过日志/端口排查；这是非 root 最小镜像的取舍。
- **D-M5-7 本地端口冲突**：compose 示例固定 18080；若宿主机已有进程监听该端口，需要改端口映射；不属于镜像行为。
- **D-M5-8 不做**：代码签名、macOS 公证、系统包、K8s/Helm、CI 真实 E2E（§14.6）。

## 22. 请求日志：客户端识别与来源 IP（M4.3，§16）

### 22.1 客户端标识 `client=`

按 §16.2 的十一级规则逐条匹配请求头，命中即止（全部大小写不敏感，纯字符串匹配，不引依赖、不做 UA 库解析）：

| 优先级 | 判据 | 标识 |
|---|---|---|
| 1 | `User-Agent` 以 `claude-cli/` 开头 | `claude-code/<version>`（取 `/` 后、首个空格前的版本号） |
| 2 | 存在 `X-Claude-Code-Session-Id` | `claude-code` |
| 3 | `anthropic-beta` 含 `claude-code` | `claude-code` |
| 4 | `User-Agent` 含 `openai-python` / `OpenAI/Python` | `openai-python` |
| 5 | `User-Agent` 含 `openai-node` / `OpenAI/Node` | `openai-node` |
| 6 | `User-Agent` 含 `anthropic-sdk-typescript` / `Anthropic/JS` | `anthropic-js` |
| 7 | `User-Agent` 含 `anthropic-sdk-python` / `Anthropic/Python` | `anthropic-python` |
| 8 | `User-Agent` 含 `dsh` | `dsh` |
| 9 | `User-Agent` 以 `curl/` 开头 | `curl` |
| 10 | 其他且 UA 非空 | UA 前 32 字符（遇空格截断） |
| 11 | UA 为空 | `unknown` |

实测样本（Claude Code 2.1.323）：`User-Agent: claude-cli/2.1.323 (external, sdk-cli, agent-sdk/0.3.322)` + `X-Claude-Code-Session-Id: ...` → `client=claude-code/2.1.323`。

### 22.2 来源 IP `ip=`

- 先取 `X-Forwarded-For` 首段（按 `,` 切分、trim；为空或 `unknown` 视为缺失）；
- 否则取 `r.RemoteAddr` 的 host 部分（去掉端口）；
- 不采信 `X-Real-IP`，不做 GeoIP / 反向 DNS。
- **容器限制**：docker bridge 网络下容器看到的来源是 docker 网关地址（如 `172.18.0.1`），不是真实客户端 IP；需要真实 IP 时用 `network_mode: host` 或前置反代透传 `X-Forwarded-For`（README 同步写明）。

### 22.3 日志行

格式与字段顺序见 §23.4（M4.4 在 §16.4 基础上增加 `req_model`）。认证失败（401）同样打印一行，`client`/`ip` 照常带出，便于排查撞密钥来源；该行 `req_model`/`model` 为 `n/a`、不含任何 token 数值。

## 23. M4.4：双认证、`[1m]` 变体、吞吐门槛、日志与容器时区（§17）

### 23.1 Anthropic 入口双认证（修订 §6/FR-4）

- `/v1/messages`：先取 `x-api-key`，匹配 `auth_token` 即通过；否则回退 `Authorization: Bearer <token>`，匹配即通过；两者缺失或不匹配 → 401。
- OpenAI 入口不变：仅 `Authorization: Bearer`（`x-api-key` 不参与）。
- 两入口共用同一 `auth_token`；上游转发认证不变（OpenAI `Bearer api_key`、Anthropic `x-api-key api_key`），两层分离、客户端 token 永不外传。
- 常量时间比较不变：两个候选头分别 `subtle.ConstantTimeCompare`，任一命中即通过。
- 401 文案：`x-api-key` 为空且 `Authorization` 不以 `Bearer ` 开头（大小写不敏感）→ `missing x-api-key or Authorization header`；否则 → `invalid service access token`（均为 Anthropic `authentication_error`）。

### 23.2 `[1m]` 变体识别（修订 §5.2 与 R4c）

- 角色判定前置：剥除 `model` 尾部一个 `[1m]`（大小写不敏感），再按 R4c 判定（精确等于角色名，或以 `claude-<role>-` 开头）。
- 改写逻辑不变：`opus[1m]` → 角色 `opus` → 改写为 `roles["opus"]`；`claude-opus-4-5[1m]` 剥除后按前缀命中 `opus`。
- 仅 Anthropic 角色路由生效；OpenAI 入口无角色概念，不受影响。剥除后仍未命中 → 按 R4d `*` 托底 / R5 回退，行为不变。
- 剥离只针对尾部标记：`opus[1x]`、`opus [1m]`（带空格）不视为变体。

### 23.3 吞吐判定最终口径（修订 §18.3/§20.3）

- 生成时长：流式 `末个数据块 − 首个数据块`；非流式 `响应体读完 − 响应头到达`。**从响应开始计算**，建连/排队/TTFT 不计入（由 `timeout`/`first_token_timeout` 管）。
- 输出 token：OpenAI `usage.completion_tokens` / Anthropic `usage.output_tokens`。
- 样本门槛 `minThroughputSampleTokens=100`、`minThroughputSampleDuration=2s`：仅当「输出 >100 token」且「生成时长 >2s」且能拿到 usage 时才计算 `token/s`；不满足任一条 → 跳过并视为健康（`recordSuccess`，不记失败）。
- 满足门槛时：`tokens/生成时长 < min_tokens_per_second` → `recordFailure`，否则成功。
- 低速一律不打断：两协议都完整转发到结束，仅响应后记一次失败；`pause_turn` 收尾只用于 `idle_timeout`。

### 23.4 请求日志最终格式（修订 §22.3）

每请求结束打印一行，字段顺序固定：

```
POST /v1/messages -> req_model="opus[1m]" model="mti-flash" (anthropic) client=claude-code/2.1.323 ip=172.18.0.1 total=2.82s ttft=0.71s in=31 out=300 gen=142 t/s
```

| 字段 | 说明 |
|---|---|
| `req_model` | 客户端请求的原始 `model`（未改写）；拿不到写 `n/a` |
| `model` | 实际转发给上游的 `model`（改写后） |
| 协议 | `openai` / `anthropic` |
| `client` | 客户端标识（§22.1） |
| `ip` | 来源 IP（§22.2） |
| `total` | 总耗时 |
| `ttft` | 首 token 耗时（未知 `n/a`） |
| `in` / `out` | 输入/输出 token 数（未知 `n/a`） |
| `gen` | 生成速度 `t/s`；**仅满足 §23.3 样本门槛时显示数值**，否则 `n/a` |

- 生成速度未过样本门槛时 `gen` 写 `n/a`，避免小样本噪声。
- 401 行：`req_model`/`model` 写 `n/a`，`client`/`ip` 带出，不含密钥。
- 保留既有 `slow` / `slow stream` / `idle` 告警日志。

### 23.5 容器时区（§17.7）

- 见 §21.2：`tzdata` + `zoneinfo/Asia/Shanghai` + `/etc/localtime` + `ENV TZ=Asia/Shanghai`，`docker logs` 时间戳为北京时间。
- 仅影响日志时间戳与 `TZ` 环境，不影响任何业务语义。

### 23.6 决策与未覆盖

- **D-M4.4-1 双认证优先级**：`x-api-key` 先于 `Authorization`；两者都带且只有一个正确时按“任一命中即通过”处理（不做“两者必须同时匹配”的更严判定）。
- **D-M4.4-2 `[1m]` 只影响角色路由**：OpenAI 入口与 R4a 精确匹配不改写/不剥除；请求仍是 `opus[1m]`，日志 `req_model` 保留原值，`model` 展示改写后的值。
- **D-M4.4-3 门槛与 `min_tokens_per_second` 的关系**：样本门槛无条件生效；`min_tokens_per_second=0` 仍完全禁用吞吐判定（连 `gen` 数值也不在日志显示）。
- **D-M4.4-4 生成时长取不到**：流式只有一个块（`firstBlock == lastBlock`）或非流式拿不到 usage → 视为样本不足，按成功处理。
- **D-M4.4-5 401 日志不含 token**：只打印 `client`/`ip`/耗时/`n/a` 占位，绝不打印 `Authorization`/`x-api-key` 值。
- **D-M4.4-6 明确不做**：不改 OpenAI 入口认证；不改上游认证；不引入滑动窗口/历史采样；不因低速中断已 committed 的流；不持久化请求日志、不做 GeoIP/反向 DNS；不采信 `X-Real-IP`。

## 24. gzip 压缩响应体（M4.6，§20）

### 24.1 问题

客户端（Claude Code、deepseek-harness 等）带 `Accept-Encoding: gzip` 时，proxy 曾把该头透传给上游；上游返回 `Content-Encoding: gzip` 的压缩 SSE 体，proxy 原样转发所以客户端正常，但内部解析全部看到压缩字节：

- `sseState.feed` 解析不到 `usage` → 日志 `↑n/a ↓n/a`；
- §19 实时低速窗口按压缩后的字节数计算速率（小 5~10 倍）→ 误判低速 → Anthropic 错误中断流并补 `pause_turn`；
- 非流式 `capture` + `parseUsage` 同样失败。

### 24.2 修复

- 构造上游请求并复制客户端头后 `outReq.Header.Del("Accept-Encoding")`：由 Go `http.Transport` 自行协商 gzip 并透明解压，同时移除响应 `Content-Encoding` / `Content-Length`。客户端头不再决定上游编码。
- 双保险：响应仍带 `Content-Encoding` 时，`gzip`（含 `x-gzip`）用 `gzip.Reader` 包一层，转发解压后的字节，并在写响应头前删除 `Content-Encoding` / `Content-Length`；`br` / `zstd` 等保持透传，打一条含实例名与编码的 warn，说明该编码下 usage 与字节速率不可用。
- 解压发生在 `copySSE` / `copyBodyTee` / `parseUsage` 之前：SSE 状态机与 §19 字节窗口看到的都是解压后字节。客户端始终收到明文，不重新压缩；`isEventStream` 仍看 `Content-Type`。
- 流式 gzip 使用惰性 `gzip.NewReader`（首个数据块读取时初始化），上游静默时仍由 `first_token_timeout` 关闭连接；非流式立即初始化，坏 gzip 在提交响应前失败。
- gzip 解压失败按上游错误处理：`recordFailure`，响应未提交时沿候选链回退，全部失败返回 502。

### 24.3 决策与未覆盖

- **D-M4.6-1 编码选择权归代理**：客户端 `Accept-Encoding` 不再透传；即使客户端要求 `identity`，上游仍按 Transport 协商返回 gzip，由代理解压后明文下发。
- **D-M4.6-2 不可解码编码**：`br` / `zstd` 等只记 warn，不解压、不解析；客户端收到的响应不变。
- **D-M4.6-3 不回填 Content-Length**：解压后流式走 chunked；非流式由 Go server 按实际写出字节处理。

## 25. M6 `/v1/models` 官方 schema（§21）

### 25.1 配置与运行时

- `config.Instance` 新增 `role_names`（`map[string]string`）与 `model_capabilities`（`map[string]ModelCapability`）；`ModelCapability` 的四个能力字段用 `*bool` 区分“未声明”与“显式 false”，窗口用 `int`（0 = 未声明）。
- `applyDefaults` 归一化并校验：
  - `role_names` 键小写/trim，仅接受 sonnet/opus/fable/haiku（`*` 非法）；值 trim 后非空；否则启动失败。
  - `model_capabilities` 键 trim 后非空；`max_input_tokens`/`max_tokens` 不得为负；否则启动失败。
- `proxy.New` 把两者复制进 `instanceRuntime`，供 `/v1/models` 读取；不参与路由、回退、日志与转发。

### 25.2 聚合规则

- 角色集合与顺序沿用 M3：只列任一 anthropic 实例声明过的具体角色，固定 `sonnet → opus → fable → haiku`，不含 `*`。
- 对外 id：按 anthropic 实例配置顺序取第一个 `role_names[角色]`；全部未配置时用内置官方名（§9 表）。
- identity：用对外 id 查内置表；查不到则 `display_name=id`、`created_at` 取 `h.now()` 的 UTC RFC3339。
- 能力来源：先确定角色的上游模型 = 首个声明该角色的实例的 `roles[角色]`（与路由一致）；再按实例顺序找第一个 `model_capabilities[上游模型]` 声明。全部未声明 → `max_*` 与 `capabilities` 均为 `null`。
- OpenAI 入口只读 openai 实例的 `models` 与 `model_capabilities`，忽略 `role_names` 与查询参数（M7 起使用能力字段，§26）。

### 25.3 分页

- 过滤 `lifecycle` 后再按游标切页；顺序即固定角色顺序。
- 无游标：`[0, limit)`；`after_id`：`(index, index+limit]`；`before_id`：`[index-limit, index)`（下界 0）。
- 未知游标（含已被过滤掉的 id）→ 空页，`has_more=false`，first/last 为 `null`。
- `has_more` 表示翻页方向上还有数据：无游标/`after_id` 时看页面之后，`before_id` 时看页面之前。

### 25.4 决策与未覆盖

- **D-M6-1 按 id 查内置表**：`role_names` 覆盖成另一个内置 id 时，`display_name`/`created_at` 跟随该 id 的内置行；只有完全不在表中的自定义 id 才回退。
- **D-M6-2 首个声明优先**：`role_names` 或 `model_capabilities` 被多个实例声明时，一律取配置顺序的第一个声明（对应 §21.5 规则 1/2）。
- **D-M6-3 空游标参数**：`?after_id=` / `?before_id=`（值为空）视为未提供游标；两参数同时出现（即使值为空）仍按双游标报 400。
- **D-M6-4 重复/多值参数**：`limit` 只取第一个值；`lifecycle` 的所有值合并后再校验，token 数 >3 → 400。
- **D-M6-5 未覆盖情形**：需求未定义负的 `max_*`（本实现拒绝）、`model_capabilities` 空键（本实现拒绝）、`role_names` 出现在 OpenAI 实例（本实现接受但不使用）、`role_names` 未声明角色的覆盖（本实现照常参与该角色的对外 id）、两个角色映射到同一对外 id（不校验、不去重，按角色各输出一条）。

### 25.5 配置页与 probe 能力回填（§21.6）

- Anthropic 实例卡片：
  - 角色映射每个具体角色一行，左侧输入上游实际模型，右侧输入对外模型名（`role_names`）；对外名留空 = 使用内置官方名，输入框以官方名作为 placeholder 提示；`*` 托底行没有对外名。`role_names` 只写具体角色，与后端校验一致。
  - 「模型能力」区块按上游模型名列出紧凑网格：模型名 + `max_input_tokens` + `max_tokens` + 四个勾选框（`image_input` / `pdf_input` / `thinking` / `structured_outputs`），表头只出现一次。行的集合 = 角色映射值（sonnet→opus→fable→haiku→`*` 顺序去重）∪ 已配置 `model_capabilities` 的键；上游返回但未被任何角色使用的模型不生成行，避免卡片过长。
  - 窗口输入：留空或 `0` 均视为未声明；负数/非整数在行下方报错并阻止保存（前端校验，后端 `config.Parse` 兜底）。勾选框：勾选写入 `true`；取消勾选删除该字段。
  - 保存构建配置时丢弃空的能力对象（全部字段未声明 → 不写该模型条目）；`role_names` 与 `model_capabilities` 在新增/删除实例、切换协议 tab、JSON 模式往返时随实例状态保留。
- 「查询模型」probe：
  - 前端把 `/api/probe/models` 返回的 `capabilities` 存为实例级临时数据（不写配置），对**当前角色映射使用到的上游模型**合并缺失字段；已由用户/配置声明的字段不被覆盖（D-M6-7）。
  - 上游未提供能力元数据的模型（或手动输入的模型）保持空行，供手填。
- **D-M6-6 勾选框的显式 false**：UI 只有「勾选/未勾选」两态，无法区分“未声明”与“显式 false”。本实现保留数据语义：由 JSON 或上游带入的显式 `false` 在取消勾选前不会被删除，因此仅含显式 `false` 的能力行视觉上像“全部留空”，但保存后仍保留该条目。需求未定义该情形；需要表达显式 false 时建议用 JSON 高级模式（或让上游声明）。
- **D-M6-7 查询结果不覆盖已填值**：probe 能力回填只补缺失字段，用户手填/旧配置优先；重复查询不会把用户改过的值改回上游值。需求未明确覆盖策略，取“手填优先”以避免静默丢失用户输入。

## 26. M7 协议路径分离 + OpenAI 入口能力（§22）

### 26.1 路径分派

- 精确匹配，不上溯、不重定向：

  | 路径 | 协议 | 行为 |
  |---|---|---|
  | `/v1/chat/completions` | OpenAI | 转发（不变） |
  | `/v1/models` | OpenAI | 本地 OpenAI 格式列表（不再按认证头切换） |
  | `/v1/messages` | Anthropic | 转发（向前兼容） |
  | `/anthropic/v1/messages` | Anthropic | 与 `/v1/messages` 同一 handler；上游路径剥掉 `/anthropic` 前缀 |
  | `/anthropic/v1/models` | Anthropic | 本地 Anthropic 格式列表 |
  | 其他（含 `/anthropic/v1/chat/completions`） | — | 404，OpenAI 格式 |

- 认证先于路径分发：`/v1/models` 与 `/anthropic/v1/models` 都接受 `x-api-key` 或 Bearer；`/v1/chat/completions` 仍仅 Bearer；未知路径按 OpenAI 认证与错误体。
- 两条 messages 入口共用同一 handler，认证、路由、回退、状态码/响应体、日志字段完全一致；`logRequest` 记录客户端实际请求路径（`/v1/messages` 或 `/anthropic/v1/messages`），字段顺序不变。
- 能力配置不参与路由、回退与转发。

### 26.2 OpenAI 入口能力字段（§22.4）

- `instanceRuntime.capabilities` 对两种协议都填充；OpenAI 列表按模型名查能力。
- 去重后每个 id 只出现一次；能力取配置顺序中第一个声明该模型的 `model_capabilities`（首个实例未声明时继续向后找）。
- 字段映射：`context_window`/`max_input_tokens` ← `max_input_tokens`；`max_output_tokens`/`max_tokens` ← `max_tokens`；均 >0 才输出。
- `input_modalities`：恒含 `"text"`；`image_input=true` 追加 `"image"`；`pdf_input=true` 追加 `"pdf"`；至少一个为 `true` 才输出字段，否则省略。
- 未声明任何能力的模型只输出 `id`/`object`/`created`/`owned_by`，保持标准 OpenAI 兼容。

### 26.3 配置页（§22.5）

- OpenAI 实例卡片在「模型列表」下方复用 Anthropic 侧同一套 `.caps` 紧凑网格（模型名 + 两窗口 + 四勾选）。
- 行集合 = 该实例 `models`（去重保序）∪ 已配置 `model_capabilities` 键；保存时空能力对象不写入。
- 「查询模型」返回的能力自动填入当前已选模型的缺失字段，用户已填值不被覆盖；勾选新模型时立即合并其远程能力。
- `model_capabilities` 的配置校验、显式 `false` 语义、空值规则与 Anthropic 实例一致（§21.5、D-M6-6）。

### 26.4 决策与未覆盖

- **D-M7-1 `/v1/models` 接受 `x-api-key`**：§22.8 要求“同一请求带 x-api-key 而非 Bearer 时仍返回 OpenAI 格式”，故两 models 入口统一双认证；响应格式仍只由路径决定。`/v1/chat/completions` 不受影响。
- **D-M7-2 `input_modalities` 输出条件**：按 §22.8 的“两者都 false 或未声明 → 省略”，实现为“至少一个 `true` 才输出”；`true` 的顺序固定为 `text`、`image`、`pdf`。
- **D-M7-3 能力键不在 `models` 中**：需求未定义；配置校验不拒绝，配置页会把该键并入能力行并原样保存（与 Anthropic 侧“能力键 ∪ 角色值”一致），不静默删除用户数据。
- **D-M7-4 未知路径的协议归属**：`/anthropic/v1/chat/completions` 等未列入矩阵的路径按“未知 → OpenAI 格式”处理，认证也按 OpenAI 规则（Bearer）。
- **D-M7-5 查询参数**：`/v1/models` 继续忽略全部查询参数；分页/过滤仅属于 `/anthropic/v1/models`。

## 27. 管理页实时请求监控（M9，§24）

### 27.1 端点与认证

- 新增 `GET /api/monitor`（`proxy.PathMonitor`），位于现有管理面路径前缀下，因此：
  - `admin_password` 为空 → 404（管理面整体禁用）；
  - 认证失败 → 401 + `WWW-Authenticate`，**不升级** WebSocket。
- 认证凭据来源优先级：
  1. HTTP `Authorization: Basic`（浏览器对同源 WS 握手可能自动携带；命令行客户端可显式设置）；
  2. query 参数 `?user=<admin_user>&password=<admin_password>`（浏览器 `new WebSocket()` 无法自定义请求头时的回退，§24.2 F4）。
- 两种来源都使用 `subtle.ConstantTimeCompare` 比较用户名与密码；凭据值不写入任何日志。
- 认证通过后才做 WebSocket 握手；非 GET → 405，缺少升级头 → 400，`Sec-WebSocket-Version != 13` → 426，ResponseWriter 不支持 `http.Hijacker` → 500。

### 27.2 采集开关与常态开销

- `monitorHub` 用原子计数维护在线客户端数。每个 AI 请求进入 `serveChain` 时只读取一次该计数：
  - 计数为 0 → `newRequest` 返回 `nil`，`copySSE`/`serveChain` 中所有监控调用都是空操作，不采集逐 trunk 明细；
  - 计数 > 0 → 创建该请求的 `requestMonitor`。
- 监控不改变响应体、日志、`attemptMetrics` 字段或语义；§23 三档日志格式原样保留。
- 连接/断开发生在请求开始之后时，该请求保持开始时的采集决定（不会中途补采或丢弃已采数据）。

### 27.3 事件模型（§24.2 F3）

所有事件为 JSON 文本帧，字段名固定如下（`type` 区分四型）：

| type | 字段 |
|---|---|
| `start` | `id`、`ts`、`method`、`path`、`protocol`（openai/anthropic）、`reqModel`、`clientIP`、`userAgent`、`instName`、`upstreamModel` |
| `chunk` | `id`、`seq`、`tsStart`、`tsEnd`、`bytes`、`deltaTokens`、`cumTokens` |
| `retry` | `id`、`instName`、`upstreamModel`、`reason`（`error`/`first_token_timeout`/`idle_timeout`/`slow`） |
| `end` | `id`、`status`、`durationMs`、`inTokens`、`outTokens`、`chunks`、`ttftMs`、`genMs`、`tps`、`instName`、`upstreamModel`、`error`（仅失败时出现） |

- `id` 为进程内唯一字符串；`ts`/`tsStart`/`tsEnd` 为 Unix 毫秒。
- 采集范围：命中模型路由的 `POST /v1/chat/completions`、`POST /v1/messages`、`POST /anthropic/v1/messages`。认证失败（401）、未知路径（404）、`GET /v1/models`、favicon 等非 AI 请求不产生任何事件。
- `start` 在第一次实际尝试候选前发出（若全部候选处于降级而强制链头，则以链头为候选）；若候选链为空，`start` 与 `end` 仍成对出现，实例/模型为空串。
- `chunk` 与 `copySSE` 的每次 `body.Read` 成功返回一一对应：`bytes` 为该次读取字节数（gzip 时为解压后字节），`tsStart` 为阻塞读取前、`tsEnd` 为读取返回后的时间，因此首块前的空隙即 TTFT、块间空隙即卡顿。
- `deltaTokens` = 本块 `sseState.tokens` 相对上一块的增长；`cumTokens` = 已见累计输出 token（Anthropic `usage.output_tokens` / OpenAI `usage.completion_tokens`）。没有 usage 的块 delta 为 0。
- 非流式 AI 请求只产生 `start` 与 `end`（无上游 trunk）。
- `retry` 仅在“当前候选响应前失败、链中还有下一个候选”时发出；`instName`/`upstreamModel` 为**下一个**候选，供前端在时间轴上标注切换点。与普通错误相比，首 token 超时通过内部 `attemptFailure` 类型携带 `first_token_timeout`，不解析错误文本。
- `end` 在所有返回路径（成功、失败、客户端断开）最后发出一次；`status` 为已提交的上游状态码，全部候选失败时为 502；`durationMs` 从进入 `serveChain` 起算；`ttftMs = 首块到达 − 请求开始`；`genMs = 末块 − 首块`；`tps` 在 `genMs > 0` 且有输出 token 时按 `outTokens / genMs` 计算（保留 1 位小数），否则 0。未知的 token 数按 0 输出。
- 事件按每个请求的产生顺序入队；每个客户端一个容量 1024 的队列，非阻塞投递。队列已满时立即断开该客户端，代理与其它客户端不受影响（§24.2 F3）。

### 27.4 最小 WebSocket 实现

- 纯标准库，不引入第三方依赖：握手用 `Sec-WebSocket-Key + RFC 6455 GUID` 的 SHA-1/Base64 计算 `Sec-WebSocket-Accept`，通过 `http.Hijacker` 取得连接后手写 101 响应。
- 服务端只发送文本帧、ping 帧与 close 帧；客户端帧必须是 masked，支持读入 close/ping/pong 与忽略数据帧；不支持扩展与分片（浏览器端 `WebSocket` 文本消息通常单帧）。
- 写路径由单一 writer goroutine 串行化（pong 由 `writeMu` 保护），每次写入设置 10s 写超时；每 30s 发 ping，读超时 75s，无 pong/close 的连接会被回收，避免 goroutine 泄漏。
- 服务端不缓存、不回放事件：新连接只收到连接之后产生的事件（§24.2 A6）。

### 27.5 请求链埋点

- `serveChain` 创建 tracker 并 `defer tracker.end(...)`；`finalStatus`/`finalErr`/`finalMetrics` 由各返回路径写入。
- `tryCandidate` 增加 `tracker` 参数与 `status` 返回值（内部签名，不改变对外行为）。
- `copySSE` 增加 `tracker` 参数；在 `sseState.feed` 之后、写给客户端之前调用 `tracker.chunk(...)`。该方法只做 `json.Marshal` 与非阻塞入队，不做阻塞 IO，不进入客户端的写路径。
- `attemptMetrics` 的字段与 `logRequest` 未改动；监控所需的 trunk 计数、首末块时间、token 增量都由独立的 `requestMonitor` 维护。

### 27.6 reload 与并发

- `monitorHub` 属于 `Server`，`buildState` 只注入引用；`POST /api/reload` 替换 handler 后，已有 WS 连接继续存在，新 handler 继续向同一 hub 推送；在途请求使用旧 handler，但其 tracker 指向同一 hub。
- hub 的客户端集合由 `sync.Mutex` 保护，在线计数为原子值；广播先复制客户端快照再逐个非阻塞入队，避免持锁调用 `drop`。
- `requestMonitor` 只由请求 goroutine 访问，无需加锁。

### 27.7 前端视图与认证栏（§24.5 第二轮）

- 管理页顶部「实时监控」模式（默认关闭）：
  - 打开时按 ① `localStorage` 中记住的凭据 → ② 监控认证栏当前输入 → ③ 配置页 `admin_password` 输入框 的顺序取凭据；均不完整时不发起 WS，状态「未连接」并提示「请先输入管理员凭据」；
  - 有凭据 → `new WebSocket(ws(s)://<host>/api/monitor?user=…&password=…)`；
  - 切换到其它模式 → 断开 WS、清空表格，但**保留**记住的凭据；刷新页面重新开始、不补发历史（A6）。
- 监控面板为**单表格**（原左右分栏已删除）：
  - 固定列顺序：`#`、状态、方法/路径、请求模型/实例·上游、入、出、块、耗时、TTFT、t/s、**时间轴（表头显示量程，见 §27.9）**；
  - `table-layout: fixed` + `<colgroup>`：前 10 列固定 px，时间轴列自动占满剩余宽度；表格 `min-width:1100px`，窄屏时只在 `.req-list` 内部横向滚动，页面本身不横滚；
  - 行高 33px；方法/路径、模型/实例两行堆叠，主行 12px、次行 11px（line-height 1.3/1.2），两行单元格 `overflow:visible`、上下各留 ≥1px，不用 `overflow:hidden` 掩盖裁切；时间轴单元格不换行；
  - 统计列与时间轴单元格在同一 `<tr>`，`getBoundingClientRect().top` 完全对齐。
- 每行 trunk 时间轴：
  - 横轴 0 = `start.ts`，终点 = `durationMs`（已结束）或 `Date.now() - start.ts`（进行中，250ms 更新）；
  - **所有行共用** `axisSpan = max(已结束 durationMs, 进行中累计耗时, chunk 末端, retry 时刻, 100ms)`；块按百分比定位，量程变化时所有行整体缩放（已结束行也会缩放）；
  - 每个 chunk 画 `[tsStart-ts, tsEnd-ts]`，含新 token 绿色、其余蓝色；retry 画虚线；进行中行右端有当前时刻游标；
  - 表尾 `tfoot` 为共享刻度，按 §27.9 的整数秒步长生成（第三轮起不再是固定 5 个四分位点），随 `displaySpan` 重算并 sticky 在表格底部。
- 「清空」按钮（`#monitor-clear`）：只移除 ended 行，进行中行与 WS 连接保持不变；清空后无行则恢复空提示；量程按剩余请求重算（无请求时回到 100ms）。
- 列表最多保留最近 500 项：超出时从最旧的**已完成**请求开始裁剪；全部仍在进行时暂不裁剪（沿用 §24.2 F5/M10）。
- 认证与记住密码：
  - 认证栏提示明确写出「密码以明文保存在本机浏览器 localStorage，仅建议在可信设备使用」；
  - `#monitor-remember` 默认不勾选；勾选后点「连接」**成功**才把 `{"user","password"}` 写入 `localStorage['model-router.monitor.cred']`；
  - 未勾选时连接成功不落盘；页面加载时若有 localStorage 凭据，自动填入用户名/密码、勾选记住密码，进入监控时自动连接；
  - 取消勾选或认证失败立即删除 localStorage 并清空密码框；「断开」只关 WS，不删除记住的凭据；
  - 凭据仍只随 WS 握手 query 发送；页面地址栏/history 不写凭据，不写 `document.cookie`。
- 认证失败识别保持不变：握手阶段 1006 重试一次，仍失败提示「认证失败，请检查用户名/密码」并清除本机凭据。
- 兼容 V1–V8：长模型名/数字列不截断、紧凑行高、1600px 下无横向滚动、1000px 下页面不横滚（表格容器内部可横滚）。
- 调试钩子：`window.__modelRouterMonitor` 暴露当前监控状态（含 `ws`、`axisSpan`），用于验收脚本读取 `ws.readyState`；不改变正常运行行为。

### 27.8 决策与未覆盖

- **D-M9-1 retry 的实例字段**：§24.2 F3 未说明取失败候选还是下一候选；按前端需求（显示“新的实例/上游模型”）取下一候选。
- **D-M9-2 retry 原因取值**：协议枚举含 `idle_timeout`/`slow`，但这两类判定发生在响应已提交之后，不会再切换候选，因此后端实际只产生 `error` 与 `first_token_timeout`；前端仍识别全部四种。
- **D-M9-3 chunk 粒度**：一个 `chunk` 对应一次上游 `body.Read` 返回，而非 SSE `data:` 行数；TCP 合并/拆分可能使计数与 data 行不同（与现有 `outcome.chunks` 口径一致）。
- **D-M9-4 非流式 AI 请求**：只发 `start`/`end`，不发 `chunk`（F3 将 chunk 定义为上游 trunk）。
- **D-M9-5 tps 口径**：监控的 `tps` 用于实时展示，`genMs > 0` 且有输出 token 即计算，不套用 §17.4 的 100 token / 2s 样本门槛；日志 `gen` 段仍严格按 §23 门槛，二者互不影响。
- **D-M9-6 未知数值**：拿不到 usage/首末块时 `inTokens`/`outTokens`/`ttftMs`/`genMs`/`tps` 输出 0，而非 `null`（F3 未定义缺失表示）。
- **D-M9-7 认证回退语义**：Header 与 query 为“或”关系；Header 存在但不匹配时仍尝试 query，以便浏览器缓存了过期凭据时页面仍能连接。
- **D-M9-8 非 GET/非升级请求**：需求只规定 404/401；本实现分别返回 405/400/426/500，均不建立连接。
- **D-M9-9 非 HTTP/1.1 环境**：`http.Hijacker` 不可用时返回 500；本项目的监听方式（明文 HTTP/1.1）不受影响。
- **D-M9-10 连接期间的采集边界**：请求开始后才建立的连接收不到该请求的事件；已开始时采集的请求即使连接随后断开也继续在内存中累计（无客户端时广播为空操作）。
- **D-M9-11 记住密码生命周期**：勾选「记住密码」并连接成功后，凭据写入 `localStorage['model-router.monitor.cred']`（明文，页面有安全提示）；刷新后自动填入并自动连接；取消勾选或认证失败立即删除；「断开」与切离监控不删除。未勾选时只在内存/输入框中使用，不落盘。
- **D-M9-12 凭据来源与残缺组合**：按 localStorage → 监控认证栏 → 配置页 的顺序取“用户名+密码齐全”的第一组；只填了一侧的来源视为不可用并继续尝试下一来源，不把不同来源的用户名与密码拼接。
- **D-M9-13 P2 的“URL”范围**：浏览器无法为 WebSocket 握手设置 `Authorization` 头，query 凭据是浏览器可用的认证通道之一；实现只把凭据放进**本次 WS 握手请求的临时 URL**，从不写入页面地址栏、`location`、history、链接或持久化存储。P2 的“禁止写入 URL”按“不得写入页面/导航 URL”解读；这一点在验收报告中作为已知解释列出。
- **D-M9-14 401 识别方式**：浏览器只给出握手失败的 `close.code=1006`，无法读取 401；按 P5 重试一次，仍失败即提示认证失败。因此服务不可达、管理面返回 404 等同样表现为 1006 的失败在重试后也会显示“认证失败”，属于浏览器能力限制。
- **D-M9-15 open 之后的 1006**：视为普通网络断线，显示「连接已断开」，不重试、不清除会话凭据（P6 未把网络断线列为清除时机），用户可重新点击「连接」。
- **D-M9-16 URL 内嵌凭据的静默降级**：`api()` 捕获 `TypeError`（含 credentials）时打标记；`load()` 对该标记或页面 URL 含 userinfo 的失败只显示中性提示，不把页面置于错误态，监控认证栏仍可独立工作。不重写请求 URL，避免配置读取成功后配置页密码自动成为监控凭据来源，从而绕过“未输入密码应提示”的验收预期。
- **D-M9-17 来源③ 不自动落盘**：只有用户显式点击「连接」且勾选「记住密码」、并且握手成功后才写 localStorage；自动使用配置页密码（来源③）或认证栏既有输入（来源②）连接时只把凭据留在内存，避免用户未授权的持久化。
- **D-M9-18 断开/取消勾选的清空范围**：取消勾选或认证失败会删除 localStorage 并清空监控认证栏密码框；「断开」与切离监控不删除 remembered 凭据；未勾选时断开/切离会清空密码框（凭据仅用于本次连接），勾选时保留输入框以便下次自动连接。配置页的密码输入框始终不受影响。
- **D-M9-19 单表格布局**：`table-layout:fixed` + colgroup 固定统计列宽度、时间轴列取剩余宽度；表格 `min-width:1100px`，窄屏时由 `.req-list` 内部横滚，避免固定列被压扁或页面横滚。
- **D-M9-20 共享量程与进行中游标**：`axisSpan` 包含进行中请求的累计耗时。若唯一/最长的是进行中请求，量程会随它增长，游标始终贴近右端；要观察游标像素增长，需要一个已结束的更长请求先把量程固定（B4 的测试即如此）。
- **D-M9-21 retry 时间来源**：retry 事件无时间戳，按 `Date.now() - start.ts` 定位；客户端与服务端时钟偏差会影响该竖线位置（chunk 使用服务端 `tsStart/tsEnd`，不受影响）。
- **D-M9-22 清空语义**：清空只删除 ended 行，不改连接、不删凭据；量程按剩余请求重算；若剩余全为进行中，量程会随之缩小。
- **D-M9-23 验收调试钩子**：`window.__modelRouterMonitor` 只读地暴露 monitor 状态，供 B5 检查 `ws.readyState`；不参与业务逻辑。
- **D-M9-24 时间轴列与行高**：时间轴容器 16px 高、行高 33px；统计单元格与时间轴单元格同属一个 `<tr>`，行内 top 差为 0。33px 是 12px 主行（line-height 1.3）+ 11px 次行（line-height 1.2）+ 上下 ≥1px 留白的最小可行行高，既修掉两行文字被裁切，又保持在 ≤34px 预算内。

### 27.9 时间轴第三轮细化（§24.6 X1–X9）

- **等待段（X1）**：每行 `.tl-wait` 从 0 画到 `req.chunksList[0].s`（相对 `start.ts`）；已结束且零 trunk 画到 `durationMs`；进行中且零 trunk 画到 `Date.now() - start.ts`，随 250ms 定时器增长。颜色用 `--warn` + 35% 不透明度，`z-index:0` 低于 `.tl-block` 的 1；`title` 为「等待首包 N ms」；图例改为「等待首包 · trunk 蓝 · 含新 token 绿 · 虚线 重试」。
- **表头（X2）**：第 11 列表头只保留 `<span id="monitor-axis-title">0s – Ns</span>`，不再出现「时间轴」「trunk」字样；N = `displaySpan/1000`。
- **量程（X3）**：`axisSpan` 仍按 W3 取所有行最长耗时（含进行中累计、chunk 末端、retry），再 `displaySpan = max(1000, ceil(axisSpan/1000)*1000)`；块与刻度一律用 `displaySpan`。`displaySpan` 未变化时不重绘刻度/标题，避免亚秒抖动。
- **刻度（X4）**：`S = displaySpan/1000`，步长从 `{1,2,5,10,15,20,30,60,120,300}` 取使 `S/step ≤ 4` 的最小值；序列 `0, step, 2*step, …` 并强制末格落在 S。首格 `0s`、末格 `Ns`，中间刻度沿用现有 CSS 居中；相邻文字边界框重叠时按步长表自动升档。
- **每块 token（X6）**：`copySSE` 在 `outcome.state.feed` 前后读取累计值，`deltaTokens = max(0, after-before)`、`cumTokens = after`；新增 `tokensKnown` 布尔字段。非解析/非流式固定 `deltaTokens=0, tokensKnown=false`。不再用 `requestMonitor.cumTokens` 做跨事件差分。
- **块高度（X7）**：以该行当前可见块的最大 `deltaTokens` 与常量 8 的较大者为阈值；`0 → 40%` 轨道高、`≥ 阈值 → 100%`，中间线性。0 token（未知或已知零）一律按 40% 绘制。
- **气泡（X8）**：`.tl-block` 的 `title` 为 `#seq · bytes B · Δn tokens · 累计 c · 距请求开始 x ms`，x = `chunk.tsStart - start.ts` 的整数毫秒。

第三轮补充决策：

- **D-M9-25 X6 的“文本长度估算”前提与实现不符**：`sseState.tokens` 实际来自 usage（`usage.output_tokens` / `usage.completion_tokens`，见 §27.2 与 `design §24.2`），不是按文本长度估算。按 X6 规定的 before/after 差分实现后，若上游只在末尾 `message_delta` 上报 usage（MTI 的真实行为），则 `deltaTokens` 呈现「除末块外为 0」：`Σ deltaTokens == end.outTokens` 仍精确成立，但无法把 token 分摊到中间块。若要真正的逐块增量，需要改变 `sseState.tokens` 语义（会破坏 §23 日志的 usage 口径）或新增文本估算（超出 X6 决策），本轮未自行推断。**结论**：`Σ deltaTokens == end.outTokens` 仍精确成立，块高度缩放因此退化为「末块最高、其余等高」，对应验收数据见 `.claude-runs/m9-monitor3/acceptance-measurements.json` 的 `deltas` 数组（20 块为 0、末块 40，`deltaSum=40 == outTokens=40`，块高 6.39px 与 16px）。
- **D-M9-26 S=1 的刻度下限**：X4 要求“至少 3 个刻度（含两端）”且刻度必须落在整数秒；当 `S=1` 时整数秒序列只能是 `0s,1s`（2 个）。实现按步长规则输出 `0s,1s`，不引入小数刻度；该冲突已记录。
- **D-M9-27 超过 1200s 的步长**：X4 的步长表最大为 300s，`S>1200` 时不存在使 `S/step ≤ 4` 的候选；实现退化为 `ceil(S/4)` 秒步长以保持 ≤5 格，避免刻度挤在一起。
- **D-M9-28 X7 阈值口径与 C11 的边界**：X7 写“当前可见块里的最大 deltaTokens 与 8 的较大者”，C11 又要求“deltaTokens ≥ 8 的块高度等于行高”。当某行最大值 > 8 时，二者不可能对所有块同时成立。实现按**行内**最大 Δ 计算阈值（X7 的实测样本即单个请求的块序列），使每行的最大块顶满；真实 MTI 流中每行通常只有末块一个非零 Δ，因此 C11 的“≥8 的块等于行高”在该块上成立。
- **D-M9-29 C3 的“整行宽度”**：X1 规定零 trunk 已结束行的等待段覆盖 `durationMs`，X3 又把量程补齐到整数秒。因此等待段相对轨道宽度为 `durationMs/displaySpan`，只有 `durationMs` 恰好等于 `displaySpan`（例如 3000ms 对 3s 量程）时才是 100%。验收中同时报告真实失败行的 `waitPct == durationPct` 与受控 3000ms 行的 100%。

## 28. 管理界面第四轮后端（M10，§24.7 D4/D6/D7/D9/D10）

本轮只实现 §24.7 的后端契约；页面拆分、统计首页与全局重连属于下一轮前端工作。后端保证既有 `web.IndexHTML` 仍能正常加载。

### 28.1 Cookie 会话登录（D4）

| 端点 | 方法 | 鉴权 | 行为 |
|---|---|---|---|
| `/admin/login` | GET | 无需登录 | 返回独立的 `web.LoginHTML`（用户名/密码/记住我/登录按钮，表单提交 `POST /api/login`） |
| `/api/login` | POST | 无需登录 | body `{"user","password","remember"}`；成功 200 + `Set-Cookie` + `{"redirect": <next>}`；失败 401，不设 cookie，响应体不含任何凭据 |
| `/api/logout` | POST | 需登录 | 删除服务端会话，下发清除 cookie（`Max-Age=0`），返回 `{"status":"ok"}` |

- 凭据校验仍走 `adminCredentialsMatch`（`constant-time` 比较 `cfg.AdminUser`/`cfg.AdminPassword`）。
- Cookie：名称 `mr_session`，值 = `crypto/rand` 32 字节 → `base64.RawURLEncoding` 不透明 token；`HttpOnly; SameSite=Lax; Path=/`；**不加 `Secure`**，因为本地 HTTP 部署必须能登录。
- `remember:true` → `Max-Age=2592000`（30 天），服务端会话同样 30 天；`remember:false` → 浏览器会话 cookie（无 `Max-Age`），服务端会话 12 小时。
- 会话存储为进程内 `map[token]sessionEntry{user, expiresAt}` + `sync.RWMutex`，不落盘；每次鉴权顺带清理过期项，重启即全部失效。
- `next` 只接受以 `/admin` 开头的站内路径；`//`、`://`、反斜杠、绝对 URL 与非 `/admin` 前缀一律回落到 `/admin/`，防开放重定向。
- `admin_password` 为空时整块 admin 面（含登录端点）仍返回 404。
- `ServeHTTP` 鉴权顺序严格为：① 登录路径放行 → ② 有效 `mr_session` → ③ 旧 `Authorization: Basic`；`?user=&password=` 仅对 `/api/ws` 保留（供脚本与既有测试）→ ④ 页面请求（路径 `/admin*` 或 `Accept` 含 `text/html`）302 `/admin/login`（页面路径附带 `?next=`）→ ⑤ 其余 `/api/*`、WS 升级返回 401，并保留 `WWW-Authenticate: Basic` 以免破坏既有脚本。

### 28.2 独立页面路由（D6）

| 路径 | 本轮后端响应 |
|---|---|
| `/admin` | 已登录 302 `/admin/`；未登录 302 `/admin/login`（同样 302，目标不同） |
| `/admin/`、`/admin/monitor`、`/admin/config`、`/admin/json` | 已登录 200 `web.IndexHTML`；未登录 302 `/admin/login` |
| `/admin/login` | 200 `web.LoginHTML`（公开，独立登录页） |

页面内容区分、顶栏导航与登录表单由前端轮次实现；本轮只保证四个路径都返回同一份可加载资源、刷新后服务端不重定向回首页。`/admin/*` 的未知路径在鉴权后 404。当前没有静态资源，故未开放 `/admin/static/*`（前端轮次若新增再放行）。

### 28.3 统计聚合（D7）

`internal/proxy/stats.go` 的 `statsAggregator` 在进程内累积；`Server.statsSnapshot(wsClients)` 是唯一序列化入口，`GET /api/stats` 与 WS `stats` 帧的 `data` 都由它产出，键集合因此天然一致。

JSON 形状：

```json
{
  "uptime": "2d 3h 14m", "version": "0.1.2",
  "wsClients": 1, "activeRequests": 0, "qps": 0.1,
  "requests": {"total": 12, "success": 11, "failed": 1, "successRate": 91.7},
  "byProtocol": {"openai": 8, "anthropic": 4},
  "ttft": {"p50": 132.0, "p95": 410.0},
  "duration": {"p50": 900.0, "p95": 2400.0},
  "inTokens": 1200, "outTokens": 3400, "avgTps": 42.1, "retries": 2,
  "instances": [{"name":"MTI","requests":12,"percent":100.0,"success":11,"failed":1,"inTokens":1200,"outTokens":3400,"avgTtftMs":120.5,"avgTps":40.2,"degraded":false}],
  "recentErrors": [{"ts":1700000000000,"status":502,"instName":"MTI","upstreamModel":"m","model":"sonnet","error":"…"}]
}
```

`requests` 采用嵌套对象是为了与现有统计页的 `normalizeStats` 对接（它优先读取 `data.requests.*`）；`failed` 与实例行的 `failed` 同名。`GET /api/stats` 的顶层键与 WS `stats.data` 完全一致。

口径：

- **累计**：进程启动起累积，重启清零；`requests.total = requests.success + requests.failed`，只统计已结束的 AI 请求（`serveChain` 粒度），`/v1/models`、favicon、401 不计入。
- **成功/失败**：`status` 为 2xx/3xx 且 `end.error == ""` 记成功；其余（含“2xx 但流式错误”）记失败。
- **分位数**：TTFT（`ttftMs`）与总耗时（`durationMs`）各保留最近 1000 个样本的环形窗口；样本 < 10 时 `p50`/`p95` 输出 `null`。采用 nearest-rank，单位 ms。
- **QPS**：最近 60 秒内开始的 AI 请求数 ÷ 60，保留 1 位小数。
- **平均生成速率**：全部 `genMs > 0` 请求的 `Σ outTokens / Σ genMs × 1000`，保留 1 位小数；实例行同口径。
- **实例 token（M17）**：实例行 `inTokens` / `outTokens` 为该实例全部已结束请求的 `usage` 全量累加，失败/中断/截断请求同样计入；探测请求不计入。`outTokens` 含 thinking/reasoning，与上一条平均生成速率的 `genOut` 吞吐样本口径不同，两者不得互相替代。
- **实例统计**：按 `end` 时的 `instName` 分组；占比 = 该实例已结束请求 / `total`；平均 TTFT = 该实例 `ttftMs` 算术均值；降级状态在快照时从当前 handler 的 `isDegraded(now)` 读取（reload 后跟着新实例状态走）。
- **近期错误**：失败请求最多 20 行、最新在前；`ts` 为落库时间（毫秒），错误摘要按 80 个字符截断。
- **运行时长**：`NewServer` 记录 `startedAt`；格式 `2d 3h 14m` / `3h 14m` / `14m`。
- **活跃连接**：hub 的原子连接计数（唯一 WS 端点 `/api/ws`）。
- **活跃请求**：从 `serveChain` 开始到 `end` 之间的 AI 请求数。

### 28.4 全局 WS 与快照（D9/D10）

- 唯一 WebSocket 端点 `GET /api/ws`（旧 `/api/monitor` 已按 §24.11 H2 删除，带正确凭据返回 404）；新连接先收到 `snapshot`（含已结束 100 条 + 进行中 ≤200 条）与 `stats` 帧，再接收实时事件。
- 新连接在 `101` 之后、注册进 hub 之前，先入队两帧：
  1. `{"type":"snapshot","requests":[…],"stats":{…}}`，`requests` 为最近 100 条**已结束**请求，时间倒序，条目含 start 字段、end 字段、`chunks` 计数与 `ended:true`（不含逐 chunk 明细，满足“最小字段”）；
  2. `{"type":"stats","ts":<毫秒>,"data":{…}}`。
  随后是实时 `start`/`chunk`/`retry`/`end` 事件。
- 快照环形缓冲固定 100 条，超出丢最旧；`POST /api/monitor/clear`（需登录）只清空该缓冲，不动累计计数、活跃请求与 WS 连接。
- **采集开关变更**：每个 AI 请求始终创建 tracker，`start`/`end` 始终写入统计与环形缓冲；只有逐 trunk 的 `chunk`、`retry` 与事件广播受 `hasClients()` 控制。无客户端时不构造/发送任何 WS 帧，也不启动统计推送循环的空转广播。
- **统计推送**：ticker 每 1 秒检查一次；有变化时距上次推送 ≥ 2 秒才推（合并节流），无变化时每 10 秒推一次心跳快照。只推给 `/api/ws` 订阅者。
- 进行中的请求不在快照里（缓冲只存已结束请求）；前端下一轮按 `id` 合并实时事件与快照，并保证 `ended=true` 的行不被后续事件改回进行中。

### 28.5 决策与未覆盖

- **D-M10-1 旧端点的快照语义（已作废）**：§24.11 H2 已删除 `/api/monitor`，所有客户端统一走 `/api/ws` 并接收 snapshot/stats；此前的兼容取舍不再适用。
- **D-M10-2 活跃 WS 客户端口径**：使用 hub 的连接原子计数；§24.11 H2 后仅剩 `/api/ws` 一个端点。
- **D-M10-3 登录页资源**：`/admin/login` 返回独立资源 `internal/web/login.html`（`web.LoginHTML`），后端只负责路由与鉴权放行；表单交互（`next` 透传、错误提示）由前端轮次维护。
- **D-M10-4 静态资源**：当前没有 `/admin/static/*` 资源，故只保留“若未来新增则免登录”的约定，不创建空路由。
- **D-M10-5 分位数算法**：D7 未指定插值方式，统一用 nearest-rank（`ceil(p·n)-1`）。
- **D-M10-6 TTFT 样本范围**：按 D7 原文取“已结束请求的 `ttftMs`”，非流式请求的 0 值也进入窗口；若前端希望只看流式请求，需要规格补充。
- **D-M10-7 错误摘要截断**：按 80 个字符（rune）截断，不加省略号；`ts` 取结束时刻。
- **D-M10-8 实例行排序**：D7 未指定顺序，按 `name` 升序输出，前端可自行重排。
- **D-M10-9 会话与配置变更**：会话只记 token/用户/过期时间；reload 修改用户名或密码不会主动失效既有会话（D4 未要求）。
- **D-M10-10 登出鉴权**：`POST /api/logout` 属于 `/api/*`，未登录时按第 ⑤ 级返回 401；已登录时删除服务端会话并清 cookie。
- **D-M10-11 认证 cleanup 频率**：按要求在每次鉴权时清理过期项；未额外启动后台清理 goroutine（D4 的“另可”为可选）。
- **D-M10-12 快照条目字段**：为保持“最小字段”，快照不含逐 chunk 时间轴；前端可据 `ts`/`durationMs`/`chunks`/`ttftMs` 画概览，切换到实时事件后再获得完整块。
- **D-M10-13 统计循环生命周期**：首次有 `/api/ws` 客户端时启动一个 1 秒 ticker goroutine，进程生命周期内复用；无订阅者时跳过快照计算与广播。
- **D-M10-14 `next` 的额外加固**：规格要求拒绝 `//` 与绝对 URL；实现同时拒绝反斜杠（浏览器可能把 `\` 规范化为 `/`）。
- **D-M10-15 `/admin` 的 POST**：沿用页面路由 405，不参与 302；D6 的 302 仅指 GET 书签场景。

## 29. 时间轴逐行等高与块内容类型着色（M10，§24.8 Y5/Y6）

### 29.1 后端：`contentDeltas`

- `sseState` 新增 `contentDeltas int`：`processLine` 解析出 `"type":"content_block_delta"` 时 +1。它统计 SSE 事件行数，不依赖上游 `usage`，因此 MTI/DeepSeek 只在流末上报 usage 也能逐块反映“本块是否含真实增量内容”。
- `copySSE` 与 `tokens` 差分同构：`before := outcome.state.contentDeltas` → `feed(buf[:n])` → 本块 `contentDeltas = after - before`；`parse=false`（无法解码的 gzip 流）时保持 0。
- `monitorChunkEvent` 新增 `contentDeltas`（int）；`requestMonitor.chunk(...)` 增加对应入参。
- `deltaTokens` / `cumTokens` / `tokensKnown` 字段与口径完全不变（§23 日志仍按 usage）；新增字段只增不减。

### 29.2 前端：Y5 删除的高度计算路径

`.tl-wait`、`.tl-block`、`.tl-block.tok` 的几何尺寸只由 CSS 决定：`top: 50%`、`transform: translateY(-50%)`、`height: var(--tl-base-h)`（`--tl-base-h: 40%`，轨道 16px → 6.4px）。JS 不再对块或等待段写 `height`，也不再维护行内 token 阈值。

被删除的标识符（Y5a 的 grep 校验清单，源码中不应再出现）：

| 标识符 | 原用途 |
|---|---|
| `timelineTokenThreshold` | 取行内最大 `deltaTokens` 与 8 的较大者作为缩放阈值 |
| `blockHeightPercent` | 按 `deltaTokens` 在 40%–100% 间计算块高 |
| `refreshRowTimelineHeights` | 阈值变化时重算整行块高 |
| `rowBaseHeightPercent` | 取行内最小块高作为等待段高度 |
| `syncWaitHeight` | 把等待段高度同步为行内最小块高 |
| `maxDelta` | 请求对象上缓存的当前行最大 `deltaTokens` |

### 29.3 前端：Y6 着色判据与 tooltip

- `monitorChunk` 把事件映射为内部 chunk：`content: event.contentDeltas || 0`；snapshot 的历史块读 `raw.contentDeltas || raw.content || 0`（历史摘要没有逐块内容信息，按 0 处理 → 蓝色）。
- `appendTimelineChunk`：`chunk.content > 0` → `.tl-block.tok`（绿），否则蓝色。
- tooltip 固定为 `#seq · bytes B · Δn tokens · 内容块 k · 累计 c · 距请求开始 x ms`，其中 `k = contentDeltas`，`Δn = deltaTokens`（X8 字段保留）。
- 表头图例文字由「含新 token」改为「含内容块」，与判据一致。
- 块宽/等待段宽度规则不变：仍按 `tsStart/tsEnd` 与首包时刻相对 `displaySpan` 的百分比定位。

### 29.4 决策与未覆盖

- **D-M10-16 OpenAI 流**：Y6 的判据是 Anthropic 的 `content_block_delta`；OpenAI `chat.completion.chunk` 不含该 `type`，因此 OpenAI 请求的块全部为蓝色。规格未要求为 OpenAI 另设判据，未自行推断。
- **D-M10-17 历史快照**：snapshot 只保存 `end` 摘要（`chunks` 为计数，不存逐块内容）；刷新页面后历史行的摘要块按 `content=0` 画蓝。实时块从连接后的 `chunk` 事件开始着色。
- **D-M10-18 非内容事件**：`message_start`、`content_block_start`、`content_block_stop`、`message_delta`、`message_stop`、`ping`、`[DONE]` 均不计入 `contentDeltas`。
- **D-M10-19 `parse=false`**：无法解码的压缩流不解析事件，`contentDeltas` 恒为 0（与 `deltaTokens` 的未知口径一致）。
- **D-M10-20 图例文字**：Y5/Y6 未要求改图例，但为避免继续误导「含新 token」，同步改为「含内容块」。

## 30. 第六轮：tooltip 开始时间、自适应高度、快照完整化、OpenAI 判据（§24.9 Z1–Z5）

### 30.1 Z1 时间轴元素显示请求开始时间

- 新增 `formatClock(ts)`：`new Date(ts)` 本地时区，24 小时制，`HH:MM:SS.mmm`（毫秒固定 3 位）；`startTimeSuffix(ts)` 返回 ` · 开始 HH:MM:SS.mmm`。
- 追加位置：`.tl-wait`（`等待首包 N ms` 之后）、`.tl-block`（`距请求开始 x ms` 之后）、`.tl-cursor`（`当前时刻` 之后）、`.tl-retry`（`（原因）` 之后）。原有 title 内容不删不改。
- 快照恢复行走同一套 `positionRowTimeline` / `appendTimelineChunk`，`start.ts` 来自 snapshot 的 `ts`，因此同样带后缀；Z1b 的“同一请求一致”由同一个 `req.ts` 保证。
- 表头图例色块/刻度不属于行内时间轴元素，不追加。

### 30.2 Z2 页面高度自适应

监控页（`body.monitor-mode`）改为纵向 flex：

```css
body.monitor-mode { min-height: 100vh; display: flex; flex-direction: column; }
body.monitor-mode main { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; gap: 0; }
#monitor-pane, #monitor-pane > .card { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; }
.req-list { flex: 1 1 auto; min-height: 200px; min-width: 0; overflow: auto; }
```

- 删除原来的 `max-height: max(260px, min(620px, calc(100vh - 330px)))` 以及 `@media (max-width:1099px)` 的 `max-height: 60vh`。
- 其他页面不受影响（`monitor-mode` 只在监控页添加）。视口高度 <600px 时允许纵向滚动，`min-height: 200px` 保证可用性。
- 横向滚动仍只发生在 `.req-list` 内部（表格 `min-width:1100px`），整页不横滚。

### 30.3 Z3 快照倒排与序号

- `applyMonitorSnapshot` 合并完所有 snapshot 行后调用 `sortMonitorRequests()`：按 `ts` 降序排序 `monitor.reqs`，再把每行 `appendChild` 回 `<tbody>`（DOM 顺序与数组一致）。
- 新增 `refreshMonitorIndexes()`：序号列取行在列表中的位置（1-based）。插入（实时/快照）、移除（清空/上限裁剪）后都会重排。
- 实时新请求仍 `unshift` + `insertBefore(firstChild)` 插到最上面，然后重排序号。

### 30.4 Z4 快照逐块明细

- 新增 `monitorChunkSnapshot`（7 字段：`seq`/`tsStart`/`tsEnd`/`bytes`/`deltaTokens`/`contentDeltas`/`cumTokens`），`monitorRequestSnapshot.ChunksList` 以 `chunksList` 输出。
- `requestMonitor.chunk(...)` 在客户端开关判断**之前**把精简副本追加到 `m.chunksList`：无 WS 客户端时也累积，保证“无客户端期间的请求”在之后连上时仍有绿块；该累积不影响统计口径。
- `end()` 调 `compactChunkSnapshots`：≤400 原样复制；>400 时保留前 200 + 后 199 + 1 个合成块，共 400。合成块 `seq` 取被跳过区间首尾 `seq` 之和的一半（整数除），`bytes`/`deltaTokens`/`contentDeltas` 求和，`tsStart`/`tsEnd` 取区间首尾，`cumTokens` 取区间末块的累计值。
- 前端 `ingestSnapshotRequest`：`Array.isArray(item.chunksList)` 时逐块还原（绿色判据 `contentDeltas > 0`）；字段缺失时才回退到旧的单摘要块路径（`chunks > 0 && durationMs > 0`）。
- 内存边界：100 条 × 400 块 ≈ 4 万个小结构，且与快照机制一样不落盘。

### 30.5 Z5 OpenAI 等价内容判据

`sseState.processLine` 新增解析 `choices[].delta`：

- `delta.content != ""`、`delta.reasoning_content != ""`、任一 `delta.tool_calls[].function.arguments != ""` → 本块计入内容（三者同时出现也只 +1）。
- `role` 首块、`finish_reason` 块、空 `delta`、空 `choices`、`[DONE]`、`: keep-alive` 注释行不计。
- Anthropic 判据不变（每条 `content_block_delta` +1）；`deltaTokens`/`cumTokens`/`tokensKnown` 与 §23 日志口径完全不动。

### 30.6 决策与未覆盖

- **D-M10-21 400 上限与“前 200 + 后 200 + 合成块”的算术冲突**：字面为 401 块，与 Z4c 的 `≤400` 冲突。实现取 **前 200 + 合成 1 + 后 199 = 400**，保证硬上限；被跳过区间从原索引 200 到 `len-200`（含），合成块覆盖的字节/内容/时间跨度均为该区间真实值。
- **D-M10-22 逐块累积时机**：始终累积（包括无客户端），因为 D9 的统计/环形缓冲本就无客户端也工作；逐块列表只在 `end()` 时压缩一次。
- **D-M10-23 快照块类型**：采用独立精简结构而不是复用 `monitorChunkEvent`，避免在 JSON 中带出 `type`/`id`/`tokensKnown` 空字段；字段集合与 Z4 列表逐字一致。
- **D-M10-24 Z4a 的“切页不刷新”**：本应用是服务端多页路由，点击“统计”再点“实时监控”是一次导航（页面重载 + 新 WS 快照），并非 SPA 保留 DOM。因此 Z4a 与 Z4b 都依赖同一份 `chunksList`；若未来改成 SPA，DOM 不会被销毁，绿块自然保留。
- **D-M10-25 排序稳定性**：`sortMonitorRequests` 按 `ts` 降序；同一毫秒的多个请求保持数组原顺序（现代 JS 的稳定排序）。序号按位置而非创建顺序。
- **D-M10-26 历史快照的 retry 标记**：快照只保存块，不保存 retry 事件，因此刷新后历史行不显示重试虚线；规格未要求。
- **D-M10-27 OpenAI 旧式 `delta.function_call`**：Z5 只列 `tool_calls[].function.arguments`，旧式 `function_call.arguments` 未纳入判据；未自行扩展。
- **D-M10-28 非字符串 delta**：`content`/`reasoning_content` 为 JSON null 时解码为空串（不计）；若为对象等非字符串类型，整条事件解析失败而跳过，与现有解析容错一致。
- **D-M10-29 Z5b 的观测边界**：判据按 SSE **事件**定义。真实上游可能把若干内容事件与 `finish_reason` / `[DONE]` 合并在同一次 TCP 写里，此时该 trunk 的 `contentDeltas` 为正（内容事件仍然正确计数）。验收用「Σ 块 contentDeltas == 原始 SSE 内容事件数」证明边界事件未额外计数；单测 `TestContentDeltasOpenAICriteria` / `TestChunkEventContentDeltasOpenAIOverWS` 逐事件覆盖 `role`/`finish_reason`/`[DONE]` 为 0。
- **D-M10-30 真实 OpenAI 验收模型**：按要求以 `POST /v1/chat/completions` 验证，但样本 body 里的 `claude-sonnet-5` 在本环境上游没有 OpenAI 通道（503 `model_not_found`），因此改用已配置的 `mti-pro` 获取真实流；协议与判据不变。

## 31. 第七轮：OpenAI 模型语义、统计落盘、数值可读化（§24.10 A/B/C）

### 31.1 A 组：OpenAI 协议模型语义

- **Anthropic 入口**（`/v1/messages`、`/anthropic/v1/messages`）：请求 `model` 是角色名（`sonnet`/`opus`/`fable`/`haiku` 或对外 `claude-*` 名称），由实例 `roles`/`role_names` 映射到上游模型。
- **OpenAI 入口**（`/v1/chat/completions`）：请求 `model` 就是上游模型名自身，代理不做角色映射，原样透传；可用集合是各实例 `models` 列表的并集。
- 因此向 OpenAI 入口传 `claude-*` 属于用户错误，上游的 4xx/5xx 原样透传；路由层不拦截（纯透传，§24.10 A1）。
- `/v1/models`（OpenAI）：按「实例顺序 + 实例内顺序」遍历 `models`，首次出现去重；`owned_by` 为该模型的**首个声明实例名**；实例没有 `name` 时回退为 `model-router`。集合不会混入 Anthropic 角色名（§24.10 A2）。

### 31.2 B 组：统计落盘（D11）

文件：`filepath.Dir(configPath)/stats.json`，与配置同目录。

```json
{
  "schema": 1,
  "requests": {"total": 12, "success": 11, "failed": 1},
  "byProtocol": {"openai": 8, "anthropic": 4},
  "inTokens": 1200, "outTokens": 3400,
  "genOutTokens": 3000, "genMs": 71000,
  "retries": 2,
  "instances": [{"name":"MTI","requests":12,"success":11,"failed":1,"inTokens":1200,"outTokens":3400,"ttftCount":12,"ttftSumMs":1440,"genOutTokens":3000,"genMs":71000}],
  "recentErrors": [{"ts":1700000000000,"status":502,"instName":"MTI","upstreamModel":"m","model":"sonnet","error":"…"}]
}
```

- 落盘内容：累计计数、按协议计数、in/out tokens、`genOutTokens`/`genMs`（用于重启后恢复平均 t/s）、重试数、各实例累计（请求/成功/失败/输入 tokens/输出 tokens/TTFT 样本数与和/生成 tokens 与时长）、近期错误 ≤20。
- 实例级 `inTokens`/`outTokens` 于 §24.15 M17 加入；schema 仍为 1，旧文件缺少这两个字段时按 0 恢复，不因升级丢弃其余累计值（历史 token 不可追溯）。
- 不落盘：100 条请求快照、TTFT/耗时 1000 样本窗口、活跃连接/请求、QPS、运行时长（D11）。
- 写入：`statsAggregator` 在 `finish`/`retry`/clear 时置 dirty；后台每秒检查一次，距上次写 ≥5 秒且 dirty 才写；`os.CreateTemp` 同目录临时文件 → `Sync` → `Rename` 原子替换；失败保留 dirty 并在下一 tick 重试，只记 warning。
- 生命周期：`NewServer` 只做加载（`attachStore`）；`main` 调用 `Server.StartPersistence()` 启动写盘回调，`Server.Close()` 关闭并强制补写，`SIGINT`/`SIGTERM` 与 HTTP 优雅关闭走同一路径。`NewServer` 不启动 goroutine，避免测试中产生额外写盘。
- 加载：文件缺失 → 零值；JSON 解析失败、schema != 1、计数器为负或 `total < success + failed` → `warning` 日志 + 改名为 `stats.json.bad`（先删旧 `.bad`）+ 零值启动，绝不阻止启动；恢复时近期错误截断到 20 条、分位数窗口保持空（样本 <10 → null）。
- 实例键为 `name`；加载后若某实例已不在配置中，D7 的实例表仍列出该行并带 `removed: true`，前端显示「已移除」（D11）。
- `POST /api/stats/clear`（需登录，POST）：清零累计值、实例计数、近期错误与分位数窗口，立即落盘；**不动** 100 条请求快照（与 `/api/monitor/clear` 互不调用）。

### 31.3 C 组：统计页数值可读化

- **时间**（TTFT/总耗时 P50、P95、实例平均 TTFT）：`<1000ms` → 整数 `ms`；`≥1000ms` → 秒 2 位小数；`≥3600s` → 小时 1 位小数（`1.0h`）。运行时长保持 `2d 3h 14m`。
- **计数**（累计/成功/失败、按协议、输入/输出 tokens、重试、实例请求/成功/失败）：`<1000` 原样；`≥1000` → `K` 1 位小数；`≥1e6` → `M` 2 位小数；先按原值选单位再舍入，若舍入后跨单位（999.96K → 1.00M）重新取单位。
- **速率/百分比**：t/s、成功率、QPS、实例占比保持 1 位小数。
- **精确值**：所有缩写单元格的 `title` 显示千分位精确值（`62,147`）；P50/P95 的 title 为 `P50 1,234ms · P95 5,678ms`。
- 验收钩子：`window.__modelRouterStatsRender(statsObject)` 暴露统计渲染函数，供 CDP 构造数据验证，不参与业务逻辑。

### 31.4 镜像与卷

- `Dockerfile` 在运行阶段复制一份非 root 所有的占位 `config.json` 到 `/home/nonroot/config`，然后声明 `VOLUME ["/home/nonroot/config"]`：具名卷首次创建会继承 65532 属主，非 root 进程可直接写 `stats.json`。
- `docker-compose.yml` 保留 `./.docker:/home/nonroot/config:rw` 并注明统计与配置一起持久化；新增 `docker-compose.named.yml`，用具名卷 `model-router-config`，端口可用 `MODEL_ROUTER_PORT` 覆盖。
- distroless 以 uid/gid 65532 运行；Linux bind mount 由 root 创建时，正确做法是 `sudo chown -R 65532:65532 ./.docker`（或用部署系统的 `fsGroup`），不使用 `chmod 777`。

### 31.5 决策与未覆盖

- **D-M10-31 `owned_by` 回退**：实例没有 `name` 时用 `model-router`，保持旧测试与默认配置的可读性；有名字时必须用名字。
- **D-M10-32 持久化字段**：在 D11 列表基础上显式保存 `genOutTokens`/`genMs`，以便重启后平均 t/s 与实例行一致；这仍属于累计值，不违反“只落盘累计统计”。
- **D-M10-33 clear 与分位数**：规格只写“清零累计值与近期错误”；实现同时清空分位数窗口，避免出现“累计为 0 但 P50 有值”的矛盾展示。请求快照不受影响。
- **D-M10-34 写盘生命周期**：由 `main` 显式 `StartPersistence`，而不是 `NewServer` 自动启动，避免所有测试 Server 都起一个写盘 goroutine；忘记启动时统计仍工作，只是不落盘。
- **D-M10-35 损坏判定**：除解析失败/schema 不符外，负计数与 `total < success+failed` 也按损坏处理；未知 JSON 字段忽略，便于向后兼容。
- **D-M10-36 具名卷初始配置**：镜像内置 `config.example.json` 的占位副本，使 `docker-compose.named.yml` 首次启动即可运行；密钥为占位值，README 要求替换。
- **D-M10-37 权限验证**：本机为 Windows + Docker Desktop，bind mount 本身可写；Linux 宿主 root 属主目录的 `chown 65532:65532` 是标准做法，但本机无法复现 Linux 属主场景，报告里注明验证边界。
- **D-M10-38 计数舍入**：`979999` → `980.0K`（先按原值选 K，再对 `979.999` 四舍五入到 1 位）；`999960` → `1.00M`（跨单位重取）。M 及以上未定义新单位，不自行扩展 G/T。
- **D-M10-39 title 精确值**：缩写单元格都设置 `title`；未缩写的时间/百分比不加 title，避免噪声。
- **D-M10-40 已移除实例**：`removed` 标志由快照时当前配置推导，不写入 `stats.json`（配置变化后自动重算）。

## 32. 第八/九轮：进行中快照、删除旧 WS 路径、JSON 页自适应（§24.11 H / §24.12 J）

### 32.1 H1 进行中请求进快照

- `monitorHub` 新增活动集合 `active []*requestMonitor`（`activeMu` 保护，按创建顺序，硬上限 **200**；超出丢弃最早项）。`trackRequest` 创建 tracker 即注册，`end()` 完成后移除。
- `requestMonitor` 新增 `mu` 与 `finished`/`cumTokens`：`begin`/`chunk`/`end` 在锁内更新状态；`snapshotInProgress(now)` 在锁内生成 `ended:false` 行，字段包括 `id/ts/method/path/protocol/reqModel/clientIP/userAgent/instName/upstreamModel/status(0)/durationMs(now-start)/inTokens(0)/outTokens(累计 cumTokens)/chunks/chunksList(同一 400 块压缩规则)/ttftMs/genMs/tps`。
- `snapshotRequests()` 合并「已结束 100 条环形缓冲」与「进行中 ≤200 条」，按 `ts` 降序稳定排序；同一 `id` 同时出现时保留 `ended:true` 的记录。
- `end()` 顺序：锁内生成 ended 快照并置 `finished=true` → `stats.finish`（写入环形缓冲）→ `removeActive` → 广播 `end`。这样快照若在结束瞬间生成，要么看到 ended 记录，要么看到进行中记录并能继续收到 `end`。
- `serveWS` 在 `h.mu` 内完成「生成 snapshot/stats 帧 → 入队 → 注册连接」：广播无法插入期间，连接不会漏掉结束事件；之后才启动统计循环与读写 goroutine。
- 前端：`ended:false` 的快照行按进行中渲染（`baseDurationMs` + 本地流逝时间），时间轴右端保留游标；同一 `id` 的 `chunk`/`end` 走正常 `monitorChunk`/`applyMonitorEnd` 续接。`monitorChunk` 按 `seq` 去重，避免「快照已含该块 + 该块实时事件随后到达」重复绘制；`end` 到达后游标移除、行结束。

### 32.2 H2 删除 `/api/monitor`

- 删除 `PathMonitor` 常量、路由与 `serveWS` 的兼容分支；`/api/monitor` 现在是普通未匹配 API 路径：带正确凭据走管理面默认分支返回 **404**，无凭据仍是 401，均不升级连接。
- `?user=&password=` 回退只保留给 `/api/ws`；`isWSPath` 只匹配 `/api/ws`。
- 所有客户端（唯一端点）都接收 snapshot/stats 帧；`hasStatsClients`/`wantsStats` 随之删除。
- 测试改写：`dialMonitor` 目标改为 `/api/ws`；`readEventsUntilEnd` 跳过 snapshot/stats 帧；`TestMonitorNoHistoryReplay` 改写为 `TestWSHistoryOnlyViaSnapshot`（历史只在 snapshot 出现、不再作为实时事件回放）；新增 `TestRemovedMonitorRoute` 覆盖 404/401/101。其余 monitor 测试断言未改。

### 32.3 J JSON 高级页编辑框自适应

```css
body.json-mode { min-height: 100vh; display: flex; flex-direction: column; }
body.json-mode main { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; }
#json-pane { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; }
#json-pane > .card { flex: 1 1 auto; min-height: 0; display: flex; flex-direction: column; }
#json-editor { flex: 1 1 auto; min-height: 240px; resize: none; }
```

- `#json-pane` 的状态行、标题、提示、结果区与按钮组 `flex: 0 0 auto`，编辑框占据剩余空间。
- 全局 `textarea { min-height:480px; resize:vertical }` 未改；`#json-editor` 以 ID 优先级覆盖高度与 resize，配置页 textarea 保持原行为。
- 视口 <600px 时允许整页滚动，`min-height:240px` 兜底。

### 32.4 决策与未覆盖

- **D-M10-41 进行中字段口径**：`inTokens` 在流中不可知，固定 0；`outTokens` 用 `cumTokens`（usage 累计，通常流末才非 0）；`status` 为 0；`durationMs` 为取快照时刻的耗时。这些字段在 `end` 后用最终值覆盖。
- **D-M10-42 200 条上限**：被丢弃的活动请求仍在运行并继续向已连接客户端广播事件，只是不再出现在后续快照中；不中断请求。
- **D-M10-43 快照/事件竞态**：后端用 `h.mu` 原子化「快照+注册」，前端按 `seq` 去重；两层共同保证 H3b 的无缝续接。
- **D-M10-44 快照顺序**：`ts` 降序使用稳定排序，同毫秒请求保持「已结束在前、进行中在后」的输入顺序。
- **D-M10-45 `/api/monitor` 鉴权前行为**：H3f 只规定带正确凭据 404；无凭据时仍按管理面规则 401（不会 101），不额外放行。
- **D-M10-46 旧路径 `/api/monitor/clear`**：这是清空接口而非 WS 端点，按 D10 保留，不受 H2 影响。
- **D-M10-47 J 页 body class**：按 `PAGE === 'json'` 添加 `json-mode`，与监控页 `monitor-mode` 并列；其他页面不受影响。

## 33. 第十轮：项目品牌化（§24.13 K）

### 33.1 展示名与英文标识

- 展示名「模型智能路由」只用于 `<title>`、页面左上角、PAGE_TITLES、README 标题与文档描述。
- `model-router` 保持不变：Go module 路径、二进制名、镜像 tag `wuzm219/model-router`、compose 服务名、`config.json`、`MODEL_ROUTER_*` 环境变量、JSON 字段、HTTP 头、Cookie、日志行、包名、测试名、URL。`index.html` / `login.html` 中已不存在 `model-router` 可见文字。

### 33.2 资源分工与路由

- `internal/web/logo.svg`：手写矢量，`viewBox 0 0 256 256`，深色圆角底板 `<rect rx=58>` + 上下两段渐变圆弧 + 三条汇聚短线 + 白色箭头；24px 与 16px 下保持可辨认。页面用 `<img src="/logo.svg" width="24" height="24">`。
- `internal/web/favicon.ico`：由 `docs/assets/logo-source-256.png` 离线转换（Node 无损解码 PNG → 面积平均缩放 → 16/32/48 用 32 位 BMP 条目、64/128/256 用 PNG 条目），提交进仓库，运行期不做转换。多尺寸原因：浏览器标签页/书签/任务栏会选取不同尺寸，缺 16/32/48 会导致缩放模糊。
- `GET /favicon.ico`：在 `Server.ServeHTTP` 最前面、鉴权之前直接处理。浏览器请求 favicon 不会带 Cookie/Authorization，若走管理面鉴权会被 302 到登录页，标签栏图标永远不显示；因此该路径必须匿名 200。`admin_password` 为空时同样返回 200。
- `GET /logo.svg`：同样在鉴权之前处理，匿名 200（§24.14 L1，见 33.5）。`Cache-Control: public, max-age=86400`。

### 33.3 布局

- `<h1 class="brand">` 内含 `<img>` 与 `<span>模型智能路由</span>`；`.brand { display:flex; align-items:center; gap:8px }`，图标 `24×24`、`flex:none`，文字 `white-space:nowrap`。登录页复用同一规则。
- 现有 header 的 `flex-wrap: wrap` 保留；1000×900 下品牌区 + 四个导航按钮 + 状态文字仍在同一行。

### 33.4 决策与未覆盖

- **D-M10-48 favicon 与鉴权的关系**：将 `/favicon.ico` 放在鉴权判断之前，而不是加入 `adminRequestAllowed` 白名单；因为它本身不在 `/admin` 或 `/api` 前缀下，放白名单也无法到达静态处理器。
- **D-M10-49 ICO 编码**：16/32/48 使用传统 BMP 条目以兼容较旧的图标读取器，64/128/256 使用 PNG 条目；源图四角 alpha=0，无需额外抠图。
- **D-M10-50 Logo 与源图**：SVG 按源图造型重绘（圆弧角度、三段渐变、箭头比例），未引入新元素；不同屏幕尺寸下渐变色标位置由 `gradientUnits="userSpaceOnUse"` 固定。
- **D-M10-51 非 AI 日志测试调整**：`TestNonAICallOmitsTokenSegment` 原用 `/favicon.ico` 触发“未路由请求”日志；favicon 现在是静态资源、不再经过 AI handler，因此改用 `/health` 未知路径，断言意图（无模型/ token 段）不变。

### 33.5 第十一轮修订：Logo 改为匿名资源（§24.14 L）

- **问题**：Logo 原先只在 `/admin/logo.svg` 上提供，该路径在管理面鉴权之后。登录页属于登录路径但页面里的 `<img>` 是子资源，`<img>` 不能携带 HTTP Basic 凭据；未登录访问时 `/admin/logo.svg` 返回 302 `/admin/login`（SVG 请求不是页面请求，进的是 401/302 分支），浏览器把 HTML 当图片解码失败，登录页左上角 Logo 始终空白。
- **D-M10-52 改为匿名路径**：新增顶层常量 `PathLogo = "/logo.svg"`（`internal/proxy/session.go`），`Server.ServeHTTP` 在鉴权之前、与 `/favicon.ico` 同一位置处理，移交 `(*Server).serveLogo`；`login.html` 与 `index.html` 的 `<img src>` 都改为 `/logo.svg`。
- **D-M10-53 删除旧路由**：`serveAdmin` 中的 `case "/admin/logo.svg"` 一并删除，避免同一资源存在「匿名」与「需登录」两条路径。管理员登录后仍可匿名访问 `/logo.svg`，行为无差异。
- **D-M10-54 测试调整**：`TestLogoSVGAuth` 更名为 `TestLogoSVGAnonymous`，断言匿名 `GET /logo.svg` = 200 且 `Content-Type: image/svg+xml`、`Cache-Control` 含 `max-age=86400`、body 以 `<svg`/`<?xml` 开头且含 `viewBox="0 0 256 256"`；追加断言旧路径 `/admin/logo.svg` 非 200、登录后 `/logo.svg` 仍 200、`admin_password` 为空时仍 200。

## 34. 第十二轮：服务熔断（§24.15 M）

> §24.17 已删除本节描述的 `current` 链头指针与 `probe` 态，选路改为「第一个 `closed`，全 `open` 从链首逐个实例向后重试」，见 §37。

### 34.1 状态与链头指针

- 实例自身只有 `closed` / `open` / `probe` 三种内部状态；`locked` 与 `-` 由 `protocolRuntime.stateFor` 按协议在展示层给出（M1/M10）。
- `protocolRuntime.current` 按协议维护、只存内存，`Handler` 重建即回到索引 0（M9/M11）。
- **失败顺延**：真实请求使 `current` 指向的实例转入 `open` 时，`Handler.recordFailure` 调用 `advanceCurrent`，把 `current` 推进到其后第一个非 `open` 实例；若其后全部 `open`，`current` 不动，`serveChain` 的全 open 分支继续打链尾（M8）。
- **成功回退**：探测成功由 `setCurrent` 回退；链尾兜底成功由 `noteRealSuccess` 就地设为 `current`（仅当原 `current` 为 `open`，避免模型定向请求把健康的高优先实例降级）。
- 锁定期间 `recordFailure` / `recordSuccess` / `probeTick` 全部跳过；`unlock` 把 `current` 设为被解锁实例并清零全部实例计数（M10/M15l）。

### 34.2 统一探测 tick 与请求顺序

- §24.16 起探测不再由请求触发：`serveChain` 不执行探测，独立后台循环每 1s 调用 `probeTick`（详见 §35）。本节其余顺序与判定语义继续适用。
- 一轮最多一个并发 tick；探测失败只把该实例 `degradedUntil` 重置为 `now + degrade_duration`，不退避（M6/M15h）。首轮探测与轮次间隔改由 §35 的 M23/M20 规则控制。
- 探测请求：OpenAI `POST /v1/chat/completions`、Anthropic `POST /anthropic/v1/messages`（M21），`max_tokens:1`、非流式、使用实例自己的 `timeout`；模型按 M3 取映射（OpenAI `models[0]`；Anthropic `* → haiku → sonnet → opus → fable`）。
- 判定：2xx 与 4xx 为成功；连接失败、超时、3xx/5xx 为失败（M5）。
- 探测行经 `beginProbeMonitor` / `probeMonitor.end` 进入最近 100 条环形缓冲（`probe=true`、`probeResult`），只调用 `stats.addProbe`，不增加累计请求数、成功率分母与分位样本（M13/§24.15.4）。

### 34.3 非流式响应的写出顺序（D-M12-1）

- `copyBody` / `copyBodyTee` 不再逐块 `Flush`：net/http 在写缓冲满时自动出队，末尾不足一缓冲的部分在 handler 返回时统一写出。这样 `serveChain` defer 中的 `tracker.end`（累计统计与环形缓冲）一定先于客户端读到 EOF 完成，避免“响应已结束但请求未入账”的统计窗口。SSE 仍由 `copySSE` 逐块 flush（§20 与需求「SSE 逐块 flush」）。

### 34.4 测试装置

- `internal/proxy/circuit_internal_test.go` 的 `cbBackend` 提供四种上游行为：`fail`（连接中断）、`truncate`（200 已提交后断流，用于构造“已提交的失败请求”）、`probeStatus`（仅探测请求返回指定状态）、`blockProbes`（阻塞探测以观察 `probe` 态）。
- `TestProbeNotCountedInStats` 的两条真实请求分别是“提交后断流的失败请求（记在 `first`）”与“成功请求（记在 `second`）”，故 `total=2`、`success/failed=1/1`、实例计数 1/1 同时成立；探测行只进环形缓冲，不改变累计值。
- 探测行为测试通过“只有目标实例声明的模型”把真实请求留在其它实例上，从而在显式调用 `probeTick` 后仍能检查探测报文（`first.last()`）；`probeTick` 在测试中同步返回，替代原先「请求触发 tick」的时序。

### 34.5 实例级输入/输出 token 两列（M17）

- `instanceCounters` 新增 `inTokens` / `outTokens`；`statsAggregator.finish` 对每个已结束请求无条件累加 `req.InTokens` / `req.OutTokens`（成功与失败一致），因此失败、中断、被截断的请求只要拿到了 usage 就计入。
- 探测请求走 `statsAggregator.addProbe`，只进最近 100 条环形缓冲，不经过 `finish`，故两列与 M13 的「探测不进累计」保持一致。
- `statsInstance` 新增 JSON 字段 `inTokens` / `outTokens`（`int64`，缺省 0）；`Server.statsSnapshot` 在合并配置行、旧版无协议行与 `removed` 行时同步复制这两个值。
- `stats.json` 的 `instances[]` 同步新增 `inTokens` / `outTokens`；schema 保持 1，旧文件按 0 恢复（§31.2）。
- 前端实例表在「平均 t/s」之前插入「输入 token」「输出 token」两列，`<colgroup>` 由 9 列同步为 11 列；数值用 `formatExactNumber` 以千分位原值展示，缺省 `0`，不做 K/M 缩写。
- **D-M12-2 输出口径**：输出列取上游 `usage` 的全量输出（Anthropic `output_tokens` / OpenAI `completion_tokens`，均含 thinking/reasoning），不是只统计 `genMs > 0` 吞吐样本的 `genOut`。`TestStatsInstanceOutTokensIncludeThinking` 构造单 trunk 的 thinking-only 流（`genMs == 0`）：`genOut` 不增长，`outTokens` 仍增加 512。
- **D-M12-3 输入 token 口径**：Anthropic 的 `usage` 把输入拆成 `input_tokens` + `cache_creation_input_tokens` + `cache_read_input_tokens`，三者求和作为输入全量（含缓存命中）；OpenAI 系 `prompt_tokens` 已包含缓存（`prompt_tokens_details.cached_tokens` 是其子集），不再重复相加。流式 `message_start` 的 usage 嵌在 `message.usage` 中，现在也参与输入解析；输出仍以 `message_delta.usage.output_tokens` 为准。该口径同时用于 §23 日志 `↑`、聚合 `inTokens` 与实例列，三者保持同源。

### 34.6 实例统计拆表与排序（M15q/M15r）

- 统计页「实例统计」拆成上下两张同构表：上表 `OpenAI 实例`、下表 `Anthropic 实例`。两张表各自拥有 11 列 `<colgroup>`、表头、`<tbody>` 与空状态（`暂无 OpenAI 实例。` / `暂无 Anthropic 实例。`），列顺序与列宽沿用拆分前的单表；某协议无行时只隐藏该表并显示对应空状态，另一张表不受影响。
- 行渲染抽成 `appendStatInstanceRow(tbody, item)`，两张表共用：`stat-current`、`stat-muted`、`stat-degraded`/`stat-probe`/`stat-locked`/`stat-shadowed`/`stat-removed` 以及锁按钮调用 `/api/instances/lock` / `/api/instances/unlock` 的行为完全一致。
- `GET /api/stats` 与 WS `stats` 帧的 `instances[]` 由同一比较器排序：协议分组（`openai` → `anthropic` → 其它/旧版无协议行）、组内 `priority` 升序、同优先级 `name` 升序。聚合器快照与 `Server.statsSnapshot` 合并后的最终列表都调用该比较器；`removed` 行不再统一追加到末尾，而是按同一规则归入协议组。JSON 字段不变，仅顺序变化。
- 前端不依赖抵达顺序：`renderStatInstanceTable` 按 `String(item.protocol)` 过滤后分别渲染，行内顺序沿用后端给出的顺序。

- **D-M12-4 拆表方式**：保留一个「实例统计」`stat-block`，用紧凑的 `.stat-subhead` 分节，而不是拆成两个卡片；这样两张表之间只有 10px 分节间距，不产生额外卡片边框与 `#stats-pane` 的 12px 网格间距，满足「不要大块留白」。
- **D-M12-5 排序键**：协议组用显式 rank（openai=0、anthropic=1、其它=2）而非字符串比较，避免 `anthropic < openai` 的字典序把 Anthropic 排到前面；`priority` 取配置数组下标（配置无显式 priority 字段）；`removed` 行没有配置优先级，按 0 参与组内排序。
- **D-M12-6 实例 token 阈值回归 1000**：M15s 最初为实例列单独设了 10000 阈值以控制列宽，但用户要求页面内只保留一套数值规则（M15t），因此实例两列与顶部「输入 tokens / 输出 tokens」磁贴统一走 `formatStatCount`（阈值 1000、K 1 位、M 2 位）。`formatInstanceTokenCount` 仅保留为薄封装，负责把缺省/非法值显示为 `0`；`title` 仍给出精确千分位值。
- **D-M12-7 全局唯一格式化**：K/M 计数统一由 `formatStatCount` 提供（请求数、成功/失败、token、重试、WS 连接、活跃请求）。时长统一由 `formatDuration` 提供：`< 1000ms` 整数毫秒；`1s–60s` 秒保留 2 位；`60s–3600s` 分保留 2 位；`≥ 3600s` 小时保留 2 位；单位按原值所在区间选择，不在舍入后跨档重选（`59999ms → 60.00s`，避免与 `60s 起用分` 的规则产生第二套判断），监控页耗时/首包与统计页平均 TTFT、P50/P95 共用该函数。百分比与速率分别走 `formatPercent` / `formatRate`（均 1 位小数），调用点不再散落 `toFixed`。`formatUptime` 不属于该缩写体系：它是运行时长专用的 d/h/m 复合格式（超过 24h 显示天），语义与请求耗时的 ms/s/m/h 缩写不同，故保留独立实现。

## 35. 第十三轮：后台周期探测（§24.16 M）

> §24.17 已整体删除本节描述的主动探测子系统（探测器、探测报文、探测行展示），恢复完全由真实请求驱动，见 §36。

### 35.1 探测器生命周期

- `Server` 持有唯一的探测 goroutine：`StartPersistence` 启动（main 在监听前调用），`Close` 停止；`NewServer`/`reload` 不启动后台任务，直接构造 `Handler` 的测试无需额外清理。
- 循环使用 `time.NewTicker(probeCheckInterval)`，`probeCheckInterval = 1s`；每个检查点从 `s.state.Load()` 取当前 `serverState` 并调用其 `handler.probeTick(handler.now())`，因此热重载后自动跟随新 Handler（旧 Handler 只服务仍在使用它的在途请求）。
- `probeTick` **同步**执行一轮探测；循环 goroutine 在轮次结束前不会进入下一次检查，天然保证「同时进行的轮次不超过 1」。`Close` 关闭 stop channel 后等待轮次结束再返回，goroutine 不泄漏；若探测仍在飞，关闭最多等待该实例的 `timeout`。
- 没有任何 `open` 实例时 `probeTick` 只做一次廉价状态检查后返回：不发请求、不启动 goroutine、不推进 `nextProbe`。

### 35.2 检查点与调度

- `Handler.nextProbe` 的零值表示「可立即探测」。一轮结束后统一设为「本轮结束时刻 + `degrade_duration`」，即 M20 的「所有协议中最晚的 nextProbe」——两个协议在同一轮内并发结束，轮次截止时间取最晚完成者。
- M23 首轮不等周期：`Handler.probeOpen` 记录各协议上一次观察到的 open 状态；某协议从 no-open 变为 has-open 时，下一次 1s 检查点立即起一轮，不受 `nextProbe` 限制。轮次结束或观察到 no-open 时清除该标记。
- 实例级首轮由 `instanceRuntime.probeAttempted` 控制：closed→open 时置 false，`probeWindow` 据此让实例立即到期；一次探测尝试后置 true，之后只有 `now >= degradedUntil` 才再次到期。因此新降级的实例在下一个检查点就被探测，探测失败后按 `degrade_duration` 重新排期，不做指数退避（M7/M15h）。
- 每个协议从索引 0 开始遍历**全部**实例，只对 `open && due && !probing` 的发探测；首个探测成功后该实例 `recordSuccess`、`setCurrent(i)` 并结束本轮（M31 修订原 `[0, current)` 区间，M7/M30 的「一次成功即停」语义不变）。锁定协议整体跳过，锁定实例不探测。
- 单实例协议按 M32 永不进入 `open`，因此 `openProtocols` 不会为它启动探测轮次，`probeProtocol` 也不会命中（详见 §35.8）。

### 35.3 并发粒度

- `probeTick` 为每个「有 open 实例且未锁定」的协议启动一个 goroutine，`WaitGroup` 等待全部结束；不同协议之间并发，避免两个 15s 探测串成 30s 长链（M24/M15x）。
- 同一协议内部保持顺序探测与「首个成功即停」，顺序覆盖整条链（M31）；`inst.probing` 继续保证同一实例同一时刻最多一个探测在飞。
- 探测请求路径：OpenAI `POST /v1/chat/completions`，Anthropic `POST /anthropic/v1/messages`（M21）；报文 `max_tokens: 1`、`messages[0].content = "ping"`、非流式，使用实例自己的 `timeout`（M22/M29）。

### 35.4 与请求路径解耦

- `serveChain` 不再调用探测，真实请求的候选链直接按调用时刻的 `current`/open 状态构建；探测阻塞或超时不影响请求时延（M19/M15v）。
- 探测行仍通过 `beginProbeMonitor` / `probeMonitor.end` 进入最近 100 条环形缓冲并带 `probe=true`/`probeResult`，不计入累计统计（M28）。

### 35.5 监控页探测展示（§24.16.4）

- 状态列：未结束 `探测中`；结束后直接显示 `成功` / `失败`，不再拼接 `探测` 前缀。
- 方法/路径列：与真实请求完全同构，固定两行（第一行 `POST`、第二行路径），不含任何探测相关节点（M26）。
- 客户端列：探测徽标渲染在这里，探测行该列只显示 `探测` 徽标，IP 与 UA 两个文本节点内容为空（不是 `-`、不显示 `model-router-probe`）；真实请求仍显示 IP 与 UA 两行、不带徽标。后端 `userAgent` 字段不变（M27）。

### 35.6 决策与未覆盖

- **D-M13-1 探测器归属**：放在 `Server` 而不是 `Handler`，因为热重载会替换 Handler；`Server` 每 tick 读取最新 state，生命周期与进程一致。
- **D-M13-2 检查点周期**：固定 1s 而非按 `degrade_duration` 定时，保证新降级实例最迟 1s 内被探测（M23），同时空闲时每秒只做一次状态遍历。
- **D-M13-3 首轮与轮次间隔**：`probeAttempted` 区分「实例首次降级」与「探测失败后的重排」，前者立即到期，后者等待 `degradedUntil`；轮次级别的 `nextProbe` 从本轮结束起算。
- **D-M13-4 并发维度**：并发发生在协议之间；协议内部保留顺序与「首个成功即停」，但探测范围改为整条链（M31），不再依赖 `current` 裁剪。
- **D-M13-5 关闭语义**：`Close` 等待在飞轮次结束以保证无 goroutine 泄漏；代价是探测阻塞时关闭最多等待一个实例 `timeout`。
- **D-M13-6 测试驱动**：`probeTick` 同步且可由测试直接调用；外部 `proxy_test` 通过 `export_test.go` 的 `ProbeTickForTest` 触发检查点，新增的 Server 级测试则用真实 1s 循环验证空闲、首轮与停止行为。

### 35.7 第十三轮修订（§24.16 M31/M15af/M15aa–M15ab）

- **D-M13-7 全链探测边界**：`probeProtocol` 删除基于 `current` 的位置裁剪，按索引遍历该协议全部实例，只对 `open && due && !probing` 发探测；成功后 `recordSuccess` + `setCurrent(i)` 并结束本轮。修复链尾实例降级而 `current` 停在其前方健康实例时、链尾 open 实例落在原 `[0, current)` 区间外而永久不探测的问题；单实例协议已由 M32 判定为永不 open，不再依赖探测恢复。`locked` 协议整体跳过、锁定实例不探测的语义不变（M31/M15ae）。
- **D-M13-8 非流式写出时机**：`copyBody` / `copyBodyTee` 保持不逐块 `Flush`（同 D-M12-1），非流式响应的末尾缓冲在 handler 返回时统一写出，使 `serveChain` defer 中的统计入账先于客户端读到 EOF；`copySSE` 仍逐块 flush。本轮补充 `flush_internal_test.go` 用计数 ResponseWriter 固化该差异（M15af）。
- **D-M13-9 探测徽标列位置**：`探测` 徽标由「方法/路径」列移至「客户端」列；路径列只保留 `POST` 与路径两行，客户端列在探测行只渲染徽标，IP/UA 文本节点写空字符串（不写 `-`，也不写 `model-router-probe`）。真实请求的客户端列不变（M26/M27/M15aa/M15ab）。

### 35.8 单实例协议不参与熔断（M32）

- **判定依据**：`NewWithOptions` 在构建 `Handler.protocols` 之后，对每个 `len(protocolRuntime.insts) == 1` 的协议设置其唯一实例的 `instanceRuntime.singleInstanceProtocol = true`。判定只读配置后的实例总数，不使用 `index`、`current` 或运行时健康实例数；多实例协议即使当前只剩一个健康实例，该标记仍为 false，熔断行为完全不变。
- **实现位置**：`instanceRuntime.markOpenLocked` 是所有熔断开闸的唯一入口（`recordFailure` 达到阈值与 `recordProbeFailure` 都会调用它），在该函数开头对 `singleInstanceProtocol` 直接返回。因此 `degradedUntil` 恒为零、`degradedSince` 恒为空、`reason` 恒为空，`state()`/`displayState()` 恒为 `closed`，监控页状态列显示「正常」。
- **探测推论**：单实例不会 `open`，`Handler.openProtocols` 看不到该协议，`probeTick` 不会启动轮次；即使直接调用 `probeProtocol`，`probeWindow` 也返回 `open=false`。`advanceCurrent` 与 `allInstancesOpen` 在单实例协议下不会被触发。
- **统计不变**：`serveChain` 仍照常调用 `h.recordFailure`（`consecutiveFailures` 继续累加），`statsAggregator.finish` 独立统计 `requests`/`failed`/`recentErrors` 与失败日志，熔断状态迁移是唯一被抑制的行为。
- **D-M13-10 判定时点**：标记在 Handler 构造期一次性写入，随后只读；配置热重载会重建 Handler 与新实例，因此更换实例数后无需额外同步。该设计避免在每次失败时读取协议链长，同时保证「单实例」定义始终等于配置实例总数。

## 36. 第十四轮：取消主动探测，真实请求驱动半开熔断（§24.17 M33–M40）

### 36.1 删除主动探测子系统（M33）

- 生产代码中与后台探测相关的实体全部物理删除，不留停用分支：`internal/proxy/circuit.go` 删除 `probeCheckInterval`、`probeProtocols`、`probeTick`、`openProtocols`、`probeProtocol`、`probeWindow`、`sendProbe`、`probeSucceeded`、`probeModel`、`newProbeChatBody`、`probeChatBody`、`probeMessage`、`probeChatPath`、`errProbeNoModel`、`reasonProbeFailed`、`recordProbeFailure`、`probeMonitor`/`beginProbeMonitor`/`emitSkippedProbe`，以及 `instanceRuntime` 的 `probing`/`probeAttempted`、`Handler` 的 `probeMu`/`probing`/`nextProbe`/`probeOpen`。
- `internal/proxy/proxy.go` 删除 `Server.startProber`/`stopProber` 与 `proberStarted`/`proberStartOnce`/`proberStopOnce`/`proberStop`/`proberDone` 字段；`StartPersistence` 只启动统计落盘，`Close` 只停止统计落盘。`NewWithOptions` 不再初始化 `probeOpen`。
- `internal/proxy/monitor.go` 删除 `monitorStartEvent`/`monitorEndEvent`/`monitorRequestSnapshot` 的 `probe`/`probeResult` 字段；`internal/proxy/stats.go` 删除 `addProbe` 与 `appendRecentLocked`。最近请求环形缓冲只承载真实请求。
- 删除 `internal/proxy/periodic_probe_internal_test.go` 与 `internal/proxy/export_test.go`（后者只为测试暴露 `probeTick`）。`/api/probe/test` 与 `/api/probe/models` 是配置页的「上游连通性/模型列表」管理接口，与本轮删除的后台熔断探测无关，保持原样（`probeResponseLimit` 因此保留）。
- 删除理由（§24.17.1）：1 token 的探测报文对 12–18 万 token 的真实请求没有预测力，且只读 HTTP 状态码会把「纯 thinking、无正文」的残废响应判为健康；上游过载时探测仍秒回成功，导致 `current` 被拉回故障实例。

### 36.2 状态收窄（M34）

- `circuit.go` 常量只剩 `stateClosed`/`stateOpen`/`stateLocked`/`stateShadowed`；`stateProbe` 与所有产出 `probe` 的分支删除。`instanceRuntime.state()` 与 `displayState()` 只返回 `closed`/`open`。
- `/api/stats` 的 `instances[].state` 取值域收窄为 `closed`/`open`/`locked`/`-`；`statsInstance.Current` 字段为 wire 兼容而保留，但因 `current` 指针删除而恒为 false（对应 M15ak 的「或其恒为 false」）。
- 单实例协议（M32）继续不参与熔断：`markOpenLocked` 开头的 `singleInstanceProtocol` 判断保留，实例恒为 `closed`。

### 36.3 半开判定：只看向回退时间（M35）

- `internal/proxy/circuit.go` 的 `instanceRuntime.isOpen()` / `isOpenAt(now)` 只读取 `degradedUntil`：`!degradedUntil.IsZero() && now.Before(degradedUntil)`。到期后实例自动回到 `closed`，不需要探测、不需要请求，也没有任何到期回调或定时器——空闲时靠时间自然恢复（用户原话「空闲的时候回退时间到了实例就回到 closed」）。
- 实例时钟来自 `NewWithOptions` 的 `opts.Now`（写入 `instanceRuntime.now`），生产环境即 `time.Now`，测试用假时钟可验证 M15aj。`degradedUntil` 到期后字段本身不清零，因此「到期后再次失败」仍会走 `recordFailure` 的重新计时分支（见 §36.5）。
- 说明：§24.17.2 M35 的字面表述 `isOpen() 即 !degradedUntil.IsZero()` 单独成立时无法让实例在到期后自动变回 `closed`；验收 M15aj 要求在推进时钟后 `state` 变为 `closed` 且下一请求打到该实例，故实现取「只由 `degradedUntil` 与当前时间决定」的时间比较语义。

### 36.4 选路：第一个 `closed`，全 `open` 从链首逐个重试（M36/M37，§24.19 M45–M48 修订）

- 删除 `protocolRuntime.current`、`setCurrent`、`advanceCurrent`、`noteRealSuccess` 与 `buildChainFrom` 的 `start` 参数及 `base = start` 截断；`findBase` 的模型能力匹配规则（`exactChain`/`roleChain`/`wildcardChain`/`fallbackChain`）保持不变。
- `internal/proxy/proxy.go` 的 `serveChain`：构造候选链后，按链顺序收集 `!isOpen()` 的候选作为尝试列表；列表为空（链上全部 `open`）时**把整条链按链序放入尝试列表**。随后依次尝试，一个候选在响应头写出前失败就重试下一个，第一个成功即返回，全部失败才 502。`chain[0]` 只是重试的**起点**，不再是唯一尝试对象（§24.19 M45–M47，取代 M36 的「只打第一个」）。
- 这样「哪个实例通，后续请求就往这里走」由状态本身自然达成：成功实例保持 `closed`，下一次选路仍先命中它；无需维护链头指针。也消除了旧 `current` 停在后面时、前面已恢复健康的实例被 `base = start` 永久跳过的缺陷。
- 锁定时仍只发往被锁定实例（`lockedCandidate`），锁定实例即使是 `open` 也照常承接（`attempts` 的兜底分支保证单元素链不会被过滤掉）；`/api/instances/lock`、`/api/instances/unlock` 的签名与「解锁清零全部计数」行为不变。

### 36.5 失败重置回退时间（M38/M39）

- `instanceRuntime.recordFailure`：`consecutiveFailures` 累加，达到 `failure_threshold` 调用 `markOpenLocked`；若 `degradedUntil` 非零（已处于 `open`，或到期后的半开试探再次失败），直接重置为 `now + degrade_duration`。没有退避、没有指数增长，`degradedSince` 保留本次故障的首次失败时刻。
- 失败重试语义沿用既有回退：一个候选失败即试下一个候选（closed 候选跳过 open 候选；全 open 时下一候选就是链上后一个 open 实例），全部失败才 502。候选顺序完全由 §36.4 决定。

### 36.6 前端与接口（M40）

- `internal/web/index.html` 删除 `.probe-badge` CSS 与 DOM 创建、`req.cells.probeBadge`、`tr.probe-row` 琥珀色标识、`.stat-probe` 状态色、`monitorStatus()` 的「探测中」分支、`statStateText`/`statStateClass` 的 `probe` 分支，以及 `buildMonitorRequest`/`applyMonitorEnd`/`ingestSnapshotRequest` 中的 `probe`/`probeResult`；`current` 行高亮（`.stat-current`）随指针删除一并移除。实例统计表状态列只渲染 `正常` / `降级 HH:MM:SS` / `已锁定` / `-`。
- 后端不再产生 `model-router-probe` 用户代理的请求；`/api/stats` 除 `state` 取值域收窄与 `current` 恒 false 外，其余字段保持。

### 36.7 决策与未覆盖

- **D-M14-1 状态由时间派生**：不为「到期恢复」增加定时器或惰性清零回调，避免后台任务重新进入系统；`degradedUntil` 保留到被成功请求或下一次失败覆盖，`displayState`/`isOpen` 只在需要时与当前时间比较。
- **D-M14-2 全 `open` 兜底从链首重试（§24.19 M45–M47 撤订）**：链上全部 `open` 时，尝试列表为整条链按顺序的全部候选，从索引 0 开始逐个向后重试，第一个成功即返回。M36 原「只打第一个、其它 `open` 实例不会被无谓尝试」的限制已撤订——「发往链上第一个」仅表示**起点**。每个失败的 `open` 实例仍照常记账并把自己的 `degradedUntil` 重置为 `now + degrade_duration`（§24.19 M15ay），不因重试而跳过失败记账。
- **D-M14-3 保留 `current` JSON 字段**：M15ak 允许「不再输出」或「恒为 false」二选一；为满足「除 state 外其余字段保持」的接口约束，选择保留字段并恒为 false，前端不再读取。
- **D-M14-4 探测行字段直接删除**：§24.17.3 允许保留但永不置真或直接删除；因唯一生产者已删除、前端已清理，保留字段会成为永久死数据，故选择直接删除 `probe`/`probeResult`。
- **未覆盖**：并发请求同时竞争同一实例时，`open` 状态以各请求读取时刻为准；规格未定义该竞态下「选路快照」的一致性，实现按请求各自的读取结果执行。

## 37. 第十六轮：全 open 时逐实例向后重试（§24.19 M45–M48）

### 37.1 实现

- `internal/proxy/proxy.go` 的 `serveChain`：全 open 兜底由 `attempts = append(attempts, chain[0])` 改为 `attempts = append(attempts, chain...)`，即整条候选链按链序进入尝试列表（M45）。`attempts` 的容量、过滤 `!isOpen()` 的构造方式、其后的 `for i, cand := range attempts` 重试循环与失败记账全部未改动。
- 起点与方向（M46）：第一个候选仍是链上索引 0，其后按链序逐个向后；第一个成功即 `return`，后续候选不会被预发（M15av-b）；全部失败才走循环后的统一 502 分支。
- 失败记账（M15ay）：每个失败候选仍由 `tryCandidate` 内的 `h.recordFailure` 记账，`consecutiveFailures` 累加与 `degradedUntil = now + degrade_duration` 重置和 closed 候选完全一致，不因处于全 open 重试而被跳过。
- 混合场景（M48）：`!isOpen()` 过滤逻辑不变；只要链上存在 closed 候选，open 候选依旧被跳过。本次只改「过滤结果为空」时的兜底构造。
- 锁定协议不受影响：锁定分支的链只有被锁定实例一个候选，`len(chain) == 1`，新旧兜底等价；被锁定实例即使 `open` 也照常承接（§24.15 M10）。
- `serveChain` 与 `buildChainFrom` 签名未变，未新增依赖；`/api/stats` 字段与 `state` 取值域不变。

### 37.2 决策与未覆盖

- **D-M15-1 沿用同一重试循环**：不为全 open 另写一套重试/记账路径，避免与 closed 候选语义分叉；差异只在 `attempts` 的构造。
- **未覆盖**：§24.19 未给全 open 重试增加总时长上限——一次请求最多尝试 `len(chain)` 个实例，总耗时受各实例 `timeout` 与上游返回共同约束；实现不引入额外总超时。
