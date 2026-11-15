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

不做：协议转换（永不）。

## 2. 模块与职责

| 模块 | 文件 | 职责 |
|---|---|---|
| 入口 | `main.go` | 解析 `-config`/`-version`、加载配置、启动 HTTP 服务、优雅停止、启动日志 |
| 配置 | `internal/config` | `instances` 解析/校验、旧字段迁移报错、原子保存、`Duration` JSON、环境变量覆盖 |
| 代理 | `internal/proxy` | 双协议入口、认证、实例展开路由、`/v1/models`、管理 API（config/reload/probe/validate）、原子重载 |
| Web | `internal/web` | `go:embed` 内嵌配置页（可视化 + JSON 高级），原生 HTML+JS，无 CDN |
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
- 管理页：可视化模式（双 tab、实例卡片、测试连接、查询模型、勾选/手填、角色对外名、模型能力、保存）+ JSON 模式（校验通过才启用保存）。页面为内嵌静态资源。

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
