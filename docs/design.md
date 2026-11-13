# model-router 设计文档（M1 / M2）

> 需求以 [requirements.md](requirements.md) 为准（FR-4 与 §7 M2 增量规格）；测试用例见 [tests.md](tests.md)。

## 1. 范围

已完成：

- **M1**：单模型 OpenAI 透传、服务访问认证（FR-4）、静态配置、构建/版本机制。
- **M2**：Anthropic 入口 `/v1/messages`、`backends` 多后端有序列表、按 model/别名/角色路由（R1～R7、R4a～R4d）、按入口协议的错误体与认证方式。

不做（见 §13）：模型自动发现（M3）、回退/降级状态机（M4）、协议转换、配置热更新（M5）。

## 2. 模块与职责

| 模块 | 文件 | 职责 |
|---|---|---|
| 入口 | `main.go` | 解析 `-config` / `-version`，加载配置，启动 HTTP 服务，SIGINT/SIGTERM 优雅停止；启动日志打印后端概览（不打印密钥） |
| 配置 | `internal/config` | 严格解析/校验 JSON；`backends` 数组、协议归一化、超时解析、默认值 |
| 透传 | `internal/proxy` | 认证、路径路由、协议分组、按 model 选择后端、请求转发、响应回传、协议化错误体 |
| 版本 | `internal/version` | 版本号唯一来源 `version.txt`（`go:embed`）+ 构建期 `-ldflags -X` 覆盖 |

依赖方向：`main → config / proxy / version`，`proxy → config`；无第三方依赖。

## 3. 请求处理流程

```
Client                              model-router                         Backend
  │ POST /v1/messages                   │                                    │
  │ x-api-key: <token>                  │ 1. 按入口协议认证，失败 401          │
  ├────────────────────────────────────▶│ 2. 路径匹配 → 协议（openai/anthropic）
  │                                     │ 3. 取该协议的有序后端列表 L         │
  │                                     │ 4. 读请求体（≤10 MiB），超限 413    │
  │                                     │ 5. 按 model/别名/角色选后端（未命中→* 或 L[0]）
  │                                     │ 6. 复制请求头，按协议设置上游认证头 │
  │                                     ├───────────────────────────────────▶│
  │                                     │ 7. 原样回传状态码/响应头/响应体     │
  │◀────────────────────────────────────┤    （逐块 flush，SSE 不缓冲）        │
```

处理顺序固定为：认证 → 路径 → 该协议是否有后端（R3）→ 读取/限制请求体（R7）→ 选择后端 → 转发。认证先于路由，未认证请求统一 401，不泄露路由信息。

## 4. 配置设计

### 4.1 结构

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "client-access-token",
  "backends": [
    {"protocol": "openai", "base_url": "https://api.deepseek.com", "api_key": "sk-...", "model": "deepseek-chat", "timeout": "15s"},
    {"protocol": "anthropic", "base_url": "https://api.deepseek.com/anthropic", "api_key": "sk-...", "model": "deepseek-v4-flash", "matches": ["claude-sonnet-4-5"], "roles": ["sonnet", "haiku"], "timeout": "15s"},
    {"protocol": "anthropic", "base_url": "https://api.deepseek.com/anthropic", "api_key": "sk-...", "model": "deepseek-chat", "roles": ["*"], "timeout": "15s"}
  ]
}
```

| 字段 | 必填 | 默认 | 校验 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 由 `net.Listen` 最终校验 |
| `auth_token` | 是 | — | 去空白后非空 |
| `backends` | 是 | — | 非空数组 |
| `backends[].protocol` | 是 | — | `openai` / `anthropic`，大小写不敏感，归一化小写 |
| `backends[].base_url` | 是 | — | 绝对 http(s) URL；去尾部 `/` |
| `backends[].api_key` | 是 | — | 去空白后非空 |
| `backends[].model` | 是 | — | 去空白后非空；参与路由，也是别名/角色改写目标 |
| `backends[].matches` | 否 | 空 | 数组；每项 `TrimSpace`，空项被忽略；去重不做 |
| `backends[].roles` | 否 | 空 | 枚举 `sonnet`/`opus`/`fable`/`haiku`/`*`；`TrimSpace` + 小写；非法值（含空项）启动失败；空数组视为未配置 |
| `backends[].timeout` | 否 | `15s` | Go duration，必须为正 |

### 4.2 解析策略与 fail-closed

- 标准库 `encoding/json` + `DisallowUnknownFields()`；拒绝多余文档内容。
- 旧格式 `backend`（单对象）现在是未知字段，启动失败；需迁移到 `backends`。
- `proxy.New` 会再次校验 `auth_token`、`backends` 非空、协议值、`api_key` 与 `base_url`，防止绕过 `config.Load` 的调用路径产生不安全状态。
- 所有校验错误都在启动时暴露，不进入半可用状态。

## 5. 协议入口与路由（R1～R7）

| 路径 | 协议 | 使用的后端列表 |
|---|---|---|
| `/v1/chat/completions` | openai | `backends` 中 `protocol=openai` 的有序子列表 |
| `/v1/messages` | anthropic | `backends` 中 `protocol=anthropic` 的有序子列表 |
| 其他 | — | 404（OpenAI 错误体） |

路由步骤：

1. 认证通过后按路径取对应协议列表 L；L 为空 → 502（按入口协议错误体），不读请求体、不转发（R3）。
2. 用 `http.MaxBytesReader(w, r.Body, 10 MiB)` 读取请求体；超限 → 413，不转发（R7）。
3. 从请求体解析**顶层** `model` 字符串（仅接受 JSON string；`null`、数字、布尔、对象、数组、非法 JSON 均视为"无可用 model"）：
   - 第一轮按 `backends` 列表顺序逐项判断：`model` 精确等于 `backend.model`（R4a，**不改写**）→ 命中 `backend.matches` 任一项（R4b，**改写**）→ 命中 `backend.roles` 的具体角色（R4c，**改写**）；
   - 第一轮全部未命中、且列表中存在声明 `*` 的后端 → 取**第一个**声明 `*` 的后端（R4d，**改写**）；
   - 其余（无 `model`、非字符串、非法 JSON、无任何命中且无 `*`）→ L[0]（R5/R6），**不改写**。
4. 转发到选中后端：R4b/R4c/R4d 命中时发送改写后的 body（仅顶层 `model` 变化），否则发送原始字节。

R4c 命中判定（区分大小写，与 B7 一致）：`model` 精确等于后端声明的角色名（`sonnet`/`opus`/`fable`/`haiku`），或以 `claude-<role>-` 开头（如 `claude-sonnet-4-5-20250929` 命中 `sonnet`）。

补充约定：

- 具体匹配（R4a/R4b/R4c）按后端为单位单轮扫描：列表靠前但仅角色/别名命中的后端，优先于靠后的精确 `model` 后端（与 M2-fix 一致，见 §12 未覆盖项）。
- `*` 托底是**第二轮**扫描，因此任何后端的具体命中都优先于任意后端的 `*`；多个 `*` 取列表第一个。
- 别名/角色改写实现：`map[string]json.RawMessage` 解析后仅替换 `payload["model"]` 再 `json.Marshal`；其余字段保留各自 raw JSON 语义，顶层 key 顺序可能变化。
- 同一协议内多个后端配置相同 `model` 或相同角色时，按列表顺序取第一个（配置未禁止重复）。
- model 匹配区分大小写；`matches`/`roles` 内重复不拒绝。
- `roles` 与 `matches` 可同时配置；同后端冲突按 R4a→R4b→R4c 顺序。
- `best` / `sonnet[1m]` / `opus[1m]` / `opusplan` 不做特殊处理：它们不命中任何具体角色/别名，会被 `*` 托底（若配置了）否则 R5。需求原文另写“未命中时按 R5”，与 `*` 的语义存在张力，见 §12。

## 6. 认证与上游凭据

- 客户端认证（FR-4，按入口）：
  - OpenAI 入口 `/v1/chat/completions`：`Authorization: Bearer <auth_token>`，scheme 大小写不敏感、token 区分大小写；
  - Anthropic 入口 `/v1/messages`：`x-api-key: <auth_token>`（精确匹配，无 Bearer 前缀），**不接受** `Authorization: Bearer`；
  - 两者均使用 `crypto/subtle.ConstantTimeCompare` 常量时间比较；缺失 message 为 `missing Authorization header` / `missing x-api-key header`。
- 认证先于路径判断：未认证请求一律 401（即使路径未知）。
- 上游认证按后端协议设置，且总是覆盖客户端提供的凭据：

| 后端协议 | 上游认证头 | 客户端 `Authorization` | 客户端 `x-api-key` |
|---|---|---|---|
| openai | `Authorization: Bearer <api_key>` | 替换为配置 key | 按 M1 原样透传（OpenAI 上游不使用该头） |
| anthropic | `x-api-key: <api_key>` | 剔除，不转发 | 替换为配置 key |

## 7. 透传规则

| 维度 | 规则 |
|---|---|
| 方法 | 原样转发 |
| 路径 | 目标路径 = `base_url` + 原始请求路径（`url.JoinPath`）；不带前缀场景与 M1 一致 |
| 查询串 | `RawQuery` 原样附加 |
| 请求头 | 除 hop-by-hop、`Content-Length`、认证头（见 §6）外原样复制；`Content-Length` 按实际 body 长度重算 |
| 请求体 | 路由前读入内存（≤10 MiB）；R4b/R4c/R4d 命中时仅改写顶层 `model`，其余情况发送原始字节 |
| 响应状态码 | 原样回写 |
| 响应头 | 除 hop-by-hop 外原样复制 |
| 响应体 | 流式回传，每块 `Write` + `Flush` |

hop-by-hop 头：`Connection`、`Proxy-Connection`、`Keep-Alive`、`Proxy-Authenticate`、`Proxy-Authorization`、`Te`、`Trailer`、`Transfer-Encoding`、`Upgrade`，以及 `Connection` 中列出的 token。

## 8. 请求体读取与上限

- 上限 10 MiB；读入内存以便解析 `model`。chat/messages 请求体通常远小于此。
- 超限（`*http.MaxBytesError`）→ 413，按入口协议返回错误体，且**不转发**。
- 其他读取错误（如客户端中断）→ 400（需求 §7.6 B3，属已确认默认）。
- 别名/角色命中（R4b/R4c/R4d）时在内存中重写顶层 `model`；改写后的 body 重新计算 `Content-Length`。

## 9. 超时与流式

- `timeout` 按后端单独配置，作用于 TCP 连接、TLS 握手、等待上游响应头；每个后端使用独立 `http.Client`/`http.Transport`。
- 不设置 `http.Client.Timeout`：避免总超时截断长 SSE 流。
- 响应体逐块 flush，流式行为与 M1 一致。

## 10. 错误处理映射

OpenAI 入口：`{"error":{"message":"...","type":"...","code":"..."}}`
Anthropic 入口：`{"type":"error","error":{"type":"...","message":"..."}}`

| 场景 | HTTP | OpenAI type / code | Anthropic error.type |
|---|---|---|---|
| 缺少客户端 token | 401 | `invalid_request_error` / `missing_api_key` | `authentication_error` |
| token 不匹配 / 认证方式不对 | 401 | `invalid_request_error` / `invalid_api_key` | `authentication_error` |
| 未知路径 | 404 | `invalid_request_error` / `not_found` | —（见 §12） |
| 该协议无后端（R3） | 502 | `server_error` / `upstream_error` | `api_error` |
| 上游连接失败 / 超时 | 502 | `server_error` / `upstream_error` | `api_error` |
| 请求体超限（R7） | 413 | `invalid_request_error` / `request_too_large` | `request_too_large` |
| 请求体读取失败（非超限） | 400 | `invalid_request_error` / `invalid_request` | `invalid_request_error` |

详细原因（上游地址、网络错误）只写日志，不返回客户端。

## 11. 版本与构建

- 版本唯一来源 `internal/version/version.txt`（`0.0.1-rc`），`go:embed` 兜底；`build.sh` 从同一文件读取并以 `-ldflags -X` 注入。
- 构建 `CGO_ENABLED=0`、`-trimpath`、`-s -w`；产物 `bin/model-router-<version>-<goos>-<goarch>[.exe]`。
- `-version` 打印构建版本。

## 12. 关键设计决策与未覆盖情形

- **D-M2-1 路由只选后端，仅在别名/角色命中时改写 model**：精确 `model` 与未命中路径保持原始字节；R4b/R4c/R4d 命中时仅替换顶层 `model`，满足 §7.7/§7.8 的改写范围约束。
- **D-M2-2 认证先于路径与协议判断**：统一 401，不泄露路由存在性。
- **D-M2-3 协议列表相互隔离**：OpenAI 请求绝不会落到 anthropic 后端，反之亦然；即使 model 名称相同。
- **D-M2-4 未知路径使用 OpenAI 错误体**：按需求 §7.6 B1 确认，仅 `/v1/messages` 使用 Anthropic 错误体，其余（含未知路径）沿用 M1 OpenAI 格式。
- **D-M2-5 400 映射**：请求体读取的非超限错误未在决策表定义，实现为 400 + `invalid_request_error`。
- **D-M2-6 校验顺序**：先判断该协议是否有后端（502）再读取请求体（413）；因此"无后端 + 超大请求体"返回 502。决策表未规定两者优先级。
- **D-M2-7 未认证 + 未知路径**：先认证 → 401（OpenAI 格式），而非 404。
- **D-M2-8 OpenAI 入口的 `x-api-key`**：§7.3 只要求 Anthropic 入口剔除该头；OpenAI 入口保持 M1 原样透传。
- **D-M2-9 重复 model**：同一协议列表重复 `model` 时取第一个；配置不拒绝重复（未覆盖）。
- **D-M2-10 model 匹配大小写**：精确、区分大小写（未规定）。
- **D-M2-11 空 model 字符串**：`model: ""` 是合法字符串但不会命中任何已配置 model/别名/角色；若存在 `*` 会按 R4d 托底，否则回退 L[0]。
- **D-M2-12 未覆盖配置热加载/自动发现/回退**：按 §7.5 不实现。
- **D-M2-13 `matches` 归一化**：配置与 `proxy.New` 均对 `matches` 逐项 `TrimSpace` 并丢弃空项；不拒绝重复别名；`matches` 与同后端 `model` 同名时 R4a 优先（不改写）。
- **D-M2-14 别名改写实现**：`map[string]json.RawMessage` 解析后仅替换顶层 `model` 再 `json.Marshal`；嵌套字段保留 raw JSON，语义不变，顶层 key 顺序可能变化（§7.7 明确允许）。
- **D-M2-15 认证方式与入口绑定**：Anthropic 入口只看 `x-api-key`，即使 `Authorization: Bearer` 正确也不通过；OpenAI 入口只看 `Authorization`，仅有 `x-api-key` 不通过。
- **D-M2-16 `roles` 校验**：枚举固定为 sonnet/opus/fable/haiku/*；每项 `TrimSpace` + 小写后校验，**空项视为非法**并启动失败（与 `matches` 丢弃空项不同，依据 §7.8“非法值启动失败”）；空数组/缺省视为未配置；不改写用户的其他配置。
- **D-M2-17 `*` 托底为第二轮扫描**：先扫描所有后端的具体匹配（R4a→R4b→R4c，按列表顺序），全部未命中后才取第一个声明 `*` 的后端；因此具体角色永远优先于托底。`*` 不参与 R4c 判定。
- **D-M2-18 特殊值与 `*` 的张力**：§7.8 一方面说 `best`/`sonnet[1m]`/`opus[1m]`/`opusplan` 未命中按 R5，另一方面 `*` 定义为“合法字符串但匹配不到”的托底。实现按统一的规则顺序处理：若配置了 `*`，这些值会命中 R4d 而非 R5；未配置 `*` 时回退 L[0]。需产品经理确认是否要为这些值显式豁免 `*`。
- **D-M2-19 空字符串 model 与 `*`**：`""` 是合法 JSON 字符串，按 §7.8 字面规则会被 `*` 托底；若期望视为“无 model”走 R5，需要显式规定。

## 13. 未纳入 M2

- 自动调用上游 `/v1/models`（M3）；
- 回退/降级/失败计数/状态机（M4）；
- 协议转换（永不实现）；
- 配置热更新（M5）；
- 跨模型重试（单请求单后端，不做隐式重试）。

## 14. 已知限制

- 仅支持两个固定入口路径；
- 单静态服务访问 token；
- 请求体需完整读入内存（≤10 MiB）后才能转发，超限直接 413；别名/角色命中时顶层 `model` 会被改写；
- 不支持配置热加载；
- 响应 Trailer、`Upgrade`（WebSocket）不转发；
- 服务本身不终止 TLS，公网部署应置于反向代理/网关之后。
