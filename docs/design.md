# model-router 设计文档（M1 ～ M3.1）

> 需求以 [requirements.md](requirements.md) 为准（FR-4、§7、§8、§9）；测试用例见 [tests.md](tests.md)。

## 1. 范围

- **M1**：项目骨架、OpenAI 单模型透传、FR-4 服务访问认证、构建/版本机制。
- **M2**：Anthropic `/v1/messages`、多后端路由、别名/角色映射、协议化错误体。
- **M3**：`/v1/models`、Web 配置页、管理 REST API、原子 reload、环境变量覆盖（§8.9）。
- **M3.1**：配置改为 `instances`（提供商实例 + 模型映射）、实例展开路由、可视化配置页、probe/validate API。

不做：回退降级（M4）、跨平台 CI/Docker（M5）、协议转换（永不）。

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
  "instances": [
    {
      "protocol": "openai",
      "base_url": "https://api.deepseek.com",
      "api_key": "sk-...",
      "provider": "deepseek",
      "timeout": "15s",
      "models": ["deepseek-chat", "deepseek-reasoner"]
    },
    {
      "protocol": "anthropic",
      "base_url": "https://api.deepseek.com/anthropic",
      "api_key": "sk-...",
      "provider": "deepseek",
      "timeout": "15s",
      "roles": {"sonnet": "deepseek-v4-flash", "opus": "deepseek-v4-pro", "*": "deepseek-chat"}
    }
  ]
}
```

| 字段 | 必填 | 默认 | 校验 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 由 `net.Listen` 最终校验 |
| `auth_token` | 是 | — | 去空白后非空 |
| `admin_user` | 否 | `admin` | 去空白；空则回退 `admin` |
| `admin_password` | 否 | 空 | 仅去空白；空=管理面禁用 |
| `instances` | 是 | — | 非空数组 |
| `instances[].protocol` | 是 | — | `openai`/`anthropic`，大小写不敏感，归一化小写 |
| `instances[].base_url` | 是 | — | 绝对 http(s) URL；去尾部 `/` |
| `instances[].api_key` | 是 | — | 去空白后非空 |
| `instances[].provider` | 否 | 空 | 去空白+小写；非空必须是内置供应商 id，否则启动失败；空=自定义 |
| `instances[].timeout` | 否 | `15s` | Go duration，必须为正 |
| `instances[].models` | openai 必填 | — | 去空白、丢弃空项、去重、保留顺序；至少 1 项 |
| `instances[].roles` | anthropic 必填 | — | 键 ∈ sonnet/opus/fable/haiku/*（小写归一化）；值去空白且非空；至少 1 项 |

互斥：openai 实例必须有非空 `models` 且 `roles` 为空；anthropic 实例必须有非空 `roles` 且 `models` 为空。空数组/空对象按“未配置”处理。

### 4.2 旧配置迁移

`backends` 字段已移除。`Parse` 在严格解码前先探测该字段，出现（含 `null`）即返回：

```
config field "backends" is no longer supported; please migrate to "instances" (see docs/design.md §9.1)
```

`matches`、后端级 `roles` 数组等旧写法随 `backends` 一并移除。

### 4.3 保存与环境变量

- `config.Save`：同目录临时文件 + `os.Rename` 原子替换，权限 0600；写入规范化后的配置。
- `Duration.MarshalJSON` 输出生效值（如 `"15s"`），保证写回文件可再次 `Load`。
- 环境变量覆盖：`MODEL_ROUTER_LISTEN`/`MODEL_ROUTER_AUTH_TOKEN`/`MODEL_ROUTER_ADMIN_USER`/`MODEL_ROUTER_ADMIN_PASSWORD`，在 JSON 解码后、`applyDefaults` 前覆盖；非空才覆盖；不作用于 `instances`；不写回文件。

## 5. 实例展开与路由

### 5.1 展开

按 `instances` 顺序展开为扁平条目：

- openai 实例：每个 `models` 项 → `{protocol=openai, base_url, api_key, model=<项>}`；
- anthropic 实例：每个 `roles` 键值 → `{protocol=anthropic, base_url, api_key, model=<值>, role=<键>}`。

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
- `best`、`sonnet[1m]`、`opus[1m]`、`opusplan` 不做特殊处理：有 `*` 时被托底，否则 R5。

## 6. 认证

- 客户端认证（按入口）：
  - `/v1/chat/completions`、`/v1/models`（OpenAI 判定）：`Authorization: Bearer <auth_token>`；
  - `/v1/messages`、`/v1/models`（仅带 `x-api-key`）：`x-api-key: <auth_token>`；
  - 常量时间比较；失败 401 按入口协议错误体。
- 认证先于路由：未认证访问未知路径 → 401（OpenAI 格式）。
- 上游认证：openai 条目 `Authorization: Bearer <api_key>`；anthropic 条目 `x-api-key: <api_key>`；客户端 Authorization/x-api-key 均不进入上游。

## 7. 透传

| 维度 | 规则 |
|---|---|
| 方法 | 原样转发 |
| 路径 | `base_url` + 原始请求路径（`url.JoinPath`） |
| 查询串 | 原样附加 |
| 请求头 | 剔除 hop-by-hop、`Content-Length`、`Authorization`、`x-api-key`（按协议替换）后复制 |
| 请求体 | 路由前读入内存（≤10 MiB）；R4c/R4d 命中时仅改写顶层 `model`，其余发送原始字节 |
| 响应 | 状态码/响应头/响应体原样；SSE 逐块 flush |

## 8. 请求体与超时

- `http.MaxBytesReader` 限制 10 MiB；超限 413 不转发；其他读取错误 400。
- 每个实例一个 `http.Client`，`timeout` 作用于连接、TLS 握手、等待响应头；不设置总超时，避免截断 SSE。

## 9. `/v1/models`

- 协议判定：同时带 `x-api-key` 且无 `Authorization` → Anthropic；其余 → OpenAI。
- 仅 GET；其他 405（`Allow: GET`）。
- 聚合：按协议取展开条目，依次取 `model`，按出现顺序去重；anthropic 跳过 `role="*"` 条目。
- OpenAI 格式：`{"object":"list","data":[{"id","object":"model","created":<unix>,"owned_by":"model-router"}]}`。
- Anthropic 格式：`{"data":[{"id","type":"model","display_name","created_at":"<RFC3339>"}],"has_more":false,"first_id","last_id"}`；空列表时 first/last 为 `""`。
- 不调用上游。

## 10. 管理页、管理 API 与重载

- 路径分派：`/admin`、`/admin/...`、`/api`、`/api/...` 为管理面；`admin_password` 为空 → 404；否则 Basic Auth（用户名/密码均常量时间比较），失败 401 + `WWW-Authenticate: Basic realm="model-router"`。
- `GET /api/config`：当前生效配置（含真实 key）。
- `PUT /api/config`：`config.Parse` 全量校验 → `config.Save` 原子写回，不生效；失败 400 不写文件。
- `POST /api/reload`：`config.Load` → `proxy.New` → `atomic.Pointer` 整体替换；失败保持旧状态。不重新绑定监听地址。
- `GET /api/providers`：返回内置供应商清单（`providers.List()`），仅 GET。
- `Server` 持有 `atomic.Pointer[serverState{cfg, handler}]`；每请求加载一次，在途请求使用旧 handler。
- 管理页：可视化模式（双 tab、实例卡片、测试连接、查询模型、勾选/手填、保存）+ JSON 模式（校验通过才启用保存）。页面为内嵌静态资源。

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
- 成功 → 200 `{models:[...]}`；网络错误/非 2xx/无法解析/缺少 `data` → 502 `{error}`（前端引导手动输入）。`data: []` 视为合法的空列表。
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

- 版本唯一来源 `internal/version/version.txt`（`0.0.1-rc`），构建脚本注入 `-ldflags -X`。
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
