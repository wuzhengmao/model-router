# model-router 设计文档（M1）

> 本文档描述 M1 里程碑的设计与实现约定。需求以 [requirements.md](requirements.md) 为准（含 FR-4 服务访问认证）；测试用例见 [tests.md](tests.md)。

## 1. 范围

M1 交付一个**单模型、单向透传**的 OpenAI 兼容代理壳子：

- 只在 `/v1/chat/completions` 上提供服务；
- 客户端访问必须通过服务访问 token 认证（FR-4）；
- 只读取静态 JSON 配置文件；
- 不做模型选择、回退、降级、协议转换、配置热加载（均属 M2+）。

目标是把"入口认证 → 透传 → 后端"的边界、配置契约与构建/版本机制打牢，后续里程碑只在其上扩展，不推翻。

## 2. 模块与职责

| 模块 | 文件 | 职责 |
|---|---|---|
| 入口 | `main.go` | 解析 `-config` / `-version`，加载配置，启动 HTTP 服务，SIGINT/SIGTERM 优雅停止（5s 排水） |
| 配置 | `internal/config` | 读取/严格解析/校验 JSON；超时按 Go duration 字符串解析；提供默认值 |
| 透传 | `internal/proxy` | 服务访问认证、路径路由、请求转发、响应回传、错误响应、流式刷新 |
| 版本 | `internal/version` | 版本号唯一来源 `version.txt`（`go:embed`）+ 构建期 `-ldflags -X` 覆盖 |

依赖方向：`main → config / proxy / version`，`proxy → config`；无第三方依赖，纯标准库。

## 3. 请求处理流程

```
Client                         model-router                          Backend
  │ POST /v1/chat/completions      │                                     │
  │ Authorization: Bearer <token>  │                                     │
  ├───────────────────────────────▶│ 1. 校验服务访问 token，失败 401      │
  │                                │ 2. 路径精确匹配，否则 404 JSON       │
  │                                │ 3. 拼接 target = base_url + path + ?query
  │                                │ 4. 复制请求头（剔除 hop-by-hop）      │
  │                                │ 5. Authorization 替换为配置 api_key   │
  │                                ├────────────────────────────────────▶│
  │                                │ 6. 原样回传状态码/响应头/响应体       │
  │◀───────────────────────────────┤    （逐块 flush，SSE 不缓冲）         │
```

认证先于路由：未携带有效 token 的请求（包括未知路径）一律 401，避免向未认证方暴露路由信息。日志只在代理侧输出：启动参数（不含 token / api_key）、每请求一行 `METHOD PATH -> STATUS 耗时`、上游失败原因；**不记录任何凭据**。

## 4. 配置设计

### 4.1 结构

```json
{
  "listen": "127.0.0.1:18080",
  "auth_token": "client-access-token",
  "backend": {
    "base_url": "https://api.openai.com",
    "api_key": "sk-...",
    "model": "gpt-4o-mini",
    "timeout": "15s"
  }
}
```

| 字段 | 类型 | 必填 | 默认 | 校验 |
|---|---|---|---|---|
| `listen` | string | 否 | `127.0.0.1:8080` | 由 `net.Listen` 最终校验 |
| `auth_token` | string | 是 | — | 去除首尾空白后不得为空 |
| `backend.base_url` | string | 是 | — | 必须是绝对 http/https URL；尾部 `/` 被去除 |
| `backend.api_key` | string | 是 | — | 去除首尾空白后不得为空 |
| `backend.model` | string | 否 | 空 | M1 仅记录日志，不参与转发（见 §10 D-M1-2） |
| `backend.timeout` | string | 否 | `15s` | Go duration，必须为正 |

### 4.2 解析策略

- 标准库 `encoding/json`，`DisallowUnknownFields()` 严格解析：未知字段视为配置错误，防止拼写错误静默生效。
- 拒绝 JSON 文档后的多余内容。
- 配置错误使进程启动失败并输出带文件路径的错误，不进入半可用状态（fail closed）。
- `proxy.New` 对 `auth_token` / `backend.api_key` 再做一次非空校验，避免绕过 `config.Load` 的调用路径产生"空 token 放行"。

## 5. 服务访问认证（FR-4）

- 客户端必须携带 `Authorization: Bearer <auth_token>`。
- 认证规则：
  - 头缺失或不是 `Bearer` 方案 → 401；
  - token 与配置值必须完全一致（区分大小写）；scheme `Bearer` 按 RFC 6750 大小写不敏感；
  - 比较使用 `crypto/subtle.ConstantTimeCompare`，不因内容提前返回。
- 认证与上游认证分离：客户端 token **只**用于访问本代理；转发上游时 `Authorization` 一律被替换为 `Bearer <backend.api_key>`，客户端 token 不进入上游请求。
- 401 响应（OpenAI 兼容错误体）：

  ```json
  {"error":{"message":"missing Authorization header","type":"invalid_request_error","code":"missing_api_key"}}
  {"error":{"message":"invalid service access token","type":"invalid_request_error","code":"invalid_api_key"}}
  ```

## 6. 透传规则

| 维度 | 规则 |
|---|---|
| 方法 | 原样转发（不做 GET/POST 限制，路径语义交给后端） |
| 路径 | 仅精确匹配 `/v1/chat/completions`；目标路径 = `base_url`（含可选前缀路径）+ 请求路径，经 `url.JoinPath` 拼接 |
| 查询串 | `RawQuery` 原样附加，不重排序、不转义 |
| 请求头 | 除 hop-by-hop 头、`Content-Length`、`Authorization` 外全部原样复制；`Host` 由目标 URL 决定；`Content-Length` 由 `r.ContentLength`（含 chunked 的 -1）驱动 |
| 凭据 | 客户端 token 仅用于本代理认证；上游 `Authorization` 固定为 `Bearer <backend.api_key>`（FR-4） |
| 请求体 | 以流形式直接转交（不读入内存、不解析、不改写、不限制大小） |
| 响应状态码 | 原样回写 |
| 响应头 | 除 hop-by-hop 头外原样复制（含 `Content-Type`、`Content-Length` 等） |
| 响应体 | 流式回传，每读到一块立即 `Write` + `Flush`，保证 SSE 实时性 |

hop-by-hop 头清单：`Connection`、`Proxy-Connection`、`Keep-Alive`、`Proxy-Authenticate`、`Proxy-Authorization`、`Te`、`Trailer`、`Transfer-Encoding`、`Upgrade`，以及 `Connection` 头中列出的自定义 token。

## 7. 超时与流式

- `backend.timeout` 作用于：TCP 连接、TLS 握手、等待上游响应头。
- **不设置** `http.Client.Timeout`：总超时会截断长时 SSE 流；响应体读取的终止由客户端断开（请求 context 取消）或上游关闭决定。这与 FR-3 中"连接超时 15s"的失败判定语义一致。

## 8. 错误处理

错误体统一为 OpenAI 兼容结构：

```json
{"error":{"message":"...","type":"...","code":"..."}}
```

| 场景 | HTTP | type | code |
|---|---|---|---|
| 缺少 `Authorization` 头 | 401 | `invalid_request_error` | `missing_api_key` |
| token 不匹配 / 方案不是 Bearer | 401 | `invalid_request_error` | `invalid_api_key` |
| 非 `/v1/chat/completions` 路径 | 404 | `invalid_request_error` | `not_found` |
| 上游连接失败 / 超时 / 响应头等待超时 | 502 | `server_error` | `upstream_error` |
| 响应体复制中途失败（如客户端断开） | 已发出的状态码 | — | 仅记录日志 |

详细失败原因（上游地址、网络错误）只写日志，不返回客户端。M1 不做失败计数与回退——失败直接暴露给客户端，符合"壳子"定位。

## 9. 版本与构建

- 版本唯一来源：`internal/version/version.txt`（内容 `0.0.1-rc`），通过 `go:embed` 在未注入时兜底。
- 构建脚本 `build.sh` 从该文件读取版本，`-ldflags "-X model-router/internal/version.Version=<version>"` 注入；两条路径同源，避免版本分叉。
- `-version` 打印 `version.Get()` 后退出。
- 构建：`CGO_ENABLED=0`、`-trimpath`、`-s -w`；默认 `GOOS/GOARCH` 取当前平台，可用环境变量覆盖。
- 产物：`bin/model-router-<version>-<goos>-<goarch>[.exe]`。

## 10. 关键设计决策

- **D-M1-1 认证与上游凭据分离（FR-4 修正）**：客户端 token 仅用于访问本代理；上游 `Authorization` 固定使用配置的 `api_key`。M1 旧实现"客户端凭据优先透传、配置 key 兜底"已废弃，避免把服务访问 token 泄露给上游，也避免上游 key 被调用方覆盖。
- **D-M1-2 不改写请求体**：`backend.model` 不注入/覆盖请求体中的 `model`。M1 的验收要求是请求体逐字节透传；模型改写属于路由语义，应在 M2+ 由显式规则实现。该字段目前只用于启动日志，为后续路由保留。
- **D-M1-3 超时不含响应体**：见 §7，优先保证流式可用性。
- **D-M1-4 严格配置解析与 fail closed**：未知字段、缺少凭据直接启动失败；未配置 `auth_token` 的服务不会以"无认证"状态运行。
- **D-M1-5 单一后端、无重试**：透传层不做隐式重试，避免非幂等请求被重复投递；重试/回退由 M4 的状态机显式实现。
- **D-M1-6 认证先于路由**：未认证请求统一 401，不区分路径是否存在。

## 11. 未纳入 M1（及处理计划）

- **D-1 链式降级**：M4 实现。建议持续回退态内按有序列表逐层下移，具体状态迁移在 M4 设计文档补充。
- **D-2 token/s 统计口径**：M4 实现。需区分流式（基于 usage 与时间窗口滑动统计）与一次性响应。
- **D-3 并发回退一致性**：M4 实现。计划以单锁保护的模型选择器 + 每模型原子计数。
- **D-4 配置热更新**：M5 随 Web 配置实现；M1 明确不支持，修改配置需重启。
- **多 token / 权限粒度 / 限流**：未纳入当前路线图；FR-4 只要求单一服务访问 token。
- **Anthropic 入口 / 多模型 / 自动发现 / Web / 跨平台 CI / Docker**：分别为 M2、M2、M3、M5、M6。

## 12. 已知限制

- 仅支持一个后端、一个入口路径、一个静态服务访问 token（轮换需改配置并重启）；
- 不支持配置热加载；
- 未做请求体大小限制与代理层限流（依赖后端与部署侧反向代理）；
- 响应 Trailer、`Upgrade`（WebSocket）不转发；
- `backend.model` 在 M1 中不参与转发；
- 服务本身不终止 TLS，暴露公网时应置于反向代理/网关之后。
