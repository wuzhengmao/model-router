# model-router

OpenAI / Anthropic 兼容的**模型路由代理**——不转协议，只代理透传。

> 当前版本：`0.0.1-rc`（M2 里程碑：OpenAI + Anthropic 双入口 + 多后端有序列表与按 model 路由 + 服务访问认证）
> 需求：[docs/requirements.md](docs/requirements.md) ｜ 设计：[docs/design.md](docs/design.md) ｜ 测试：[docs/tests.md](docs/tests.md) ｜ 变更：[CHANGELOG.md](CHANGELOG.md)

## 能力范围（M1 + M2）

- 两个协议入口，互不转换：
  - OpenAI：`POST /v1/chat/completions`
  - Anthropic：`POST /v1/messages`
  - 其他路径返回 404。
- **服务访问认证（FR-4，按协议）**：
  - OpenAI 入口：`Authorization: Bearer <auth_token>`
  - Anthropic 入口：`x-api-key: <auth_token>`
  - 缺失或不匹配返回 401（错误体按入口协议格式）。
- **多后端有序列表**：配置 `backends` 数组，`protocol` 为 `openai` / `anthropic`；同一协议的配置顺序即路由优先级。
- **按 model / 别名 / 角色路由**：
  - 请求体顶层 `model` 精确命中某后端 `model` → 转发到该后端，**不改写**请求体。
  - 命中某后端 `matches`（模型别名）→ 转发到该后端，并把请求体顶层 `model` 改写为该后端 `model`。
  - 命中某后端 `roles`（模型角色）→ 转发并改写顶层 `model`；命中方式：精确等于 `sonnet`/`opus`/`fable`/`haiku`，或以 `claude-<role>-` 开头（如 `claude-sonnet-4-5-20250929`）。
  - 以上均未命中且某后端声明了 `*` 通配角色 → 转发到第一个声明 `*` 的后端并改写顶层 `model`。
  - 未命中 / 缺失 / 非字符串 / null / 非法 JSON → 转发到该协议列表第一个后端，**不改写**。
- **透传**：方法、路径、查询串、请求体及非认证请求头原样转发（别名命中时仅改写顶层 `model`）；响应状态码、响应头、响应体原样回传，SSE 逐块 flush。
- **上游认证分离**：OpenAI 后端使用 `Authorization: Bearer <api_key>`，Anthropic 后端使用 `x-api-key: <api_key>`；客户端 token 永不外传。
- 错误体按入口协议格式返回（OpenAI / Anthropic 各自结构）。
- 版本号单一来源，构建时通过 `-ldflags` 注入；`-version` 打印构建版本。

M3 及之后（自动发现、回退降级、Web 配置、跨平台 CI/Docker）见需求文档第 6 节。

## 快速开始

1. 准备配置（示例见 [config.example.json](config.example.json)）：

   ```json
   {
     "listen": "127.0.0.1:18080",
     "auth_token": "replace-with-your-client-token",
     "backends": [
       {
         "protocol": "openai",
         "base_url": "https://api.deepseek.com",
         "api_key": "sk-...",
         "model": "deepseek-chat",
         "timeout": "15s"
       },
       {
         "protocol": "anthropic",
         "base_url": "https://api.deepseek.com/anthropic",
         "api_key": "sk-...",
         "model": "deepseek-v4-flash",
         "matches": ["claude-sonnet-4-5", "claude-sonnet-4"],
         "roles": ["sonnet", "haiku"],
         "timeout": "15s"
       },
       {
         "protocol": "anthropic",
         "base_url": "https://api.deepseek.com/anthropic",
         "api_key": "sk-...",
         "model": "deepseek-chat",
         "roles": ["*"],
         "timeout": "15s"
       }
     ]
   }
   ```

   `roles` 中 `*` 表示托底：具体 model / 别名 / 角色都未命中时，转发到第一个声明 `*` 的后端。

2. 启动代理：

   ```bash
   go run . -config config.json
   ```

   启动日志会打印监听地址、后端数量及每个后端的协议 / 地址 / 模型（不打印密钥）。

3. 调用 OpenAI 入口（`Authorization` 是本代理的 token，不是上游 key）：

   ```bash
   curl -sS -X POST http://127.0.0.1:18080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer <auth_token>" \
     -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"ping"}]}'
   ```

4. 调用 Anthropic 入口（客户端认证使用 `x-api-key`，与官方 SDK 一致）：

   ```bash
   curl -sS -X POST http://127.0.0.1:18080/v1/messages \
     -H "Content-Type: application/json" \
     -H "x-api-key: <auth_token>" \
     -H "anthropic-version: 2023-06-01" \
     -d '{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}'
   ```

   上例中 `claude-sonnet-4-5` 命中 anthropic 后端的 `matches`，代理会把它改写为 `deepseek-v4-flash` 后转发到 DeepSeek 的 Anthropic 兼容入口；也可直接发送 `"model":"sonnet"`（或以 `claude-sonnet-` 开头的官方版本 ID）由 `roles` 命中同一后端。请求中的 `x-api-key` 会被替换为配置的 `backend.api_key`。

## 配置说明

顶层字段：

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 监听地址，形如 `host:port` |
| `auth_token` | 是 | — | 客户端访问本代理的 token；OpenAI 入口走 Bearer，Anthropic 入口走 `x-api-key` |
| `backends` | 是 | — | 后端有序数组，非空；顺序即同协议内的路由优先级 |

`backends[]` 字段：

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `protocol` | 是 | — | `openai` / `anthropic`（大小写不敏感，内部归一化小写） |
| `base_url` | 是 | — | 后端地址，绝对 http(s) URL；可带路径前缀（如 `https://api.deepseek.com/anthropic`） |
| `api_key` | 是 | — | 上游密钥；OpenAI 后端用于 `Authorization: Bearer`，Anthropic 后端用于 `x-api-key` |
| `model` | 是 | — | 该后端对应的真实模型名；路由与别名改写都以它为目标 |
| `matches` | 否 | — | 客户端模型别名列表；每项去首尾空白，空项被忽略；命中时把请求体顶层 `model` 改写为 `model` |
| `roles` | 否 | — | 模型角色列表，枚举 `sonnet`/`opus`/`fable`/`haiku`/`*`（大小写不敏感）；非法值启动失败，空数组视为未配置；命中时把顶层 `model` 改写为 `model` |
| `timeout` | 否 | `15s` | 连接与等待响应头超时，Go duration 字符串（如 `15s`、`1m`）；不截断流式响应体 |

配置采用严格解析：未知字段、`backends` 为空、缺失 `auth_token` / `protocol` / `base_url` / `api_key` / `model`、非法协议、非绝对 URL 或非法超时都会使启动失败并给出明确错误。

### 路由规则

| 请求 | 行为 |
|---|---|
| `/v1/chat/completions` | 使用 `protocol=openai` 的有序子列表 |
| `/v1/messages` | 使用 `protocol=anthropic` 的有序子列表 |
| 该协议无后端 | 502（按入口协议错误体） |
| `model` 精确命中某后端 `model`（R4a） | 转发到该后端，**不改写**请求体 |
| `model` 命中某后端 `matches`（R4b） | 转发到该后端，顶层 `model` 改写为该后端 `model`，其余字段语义不变 |
| `model` 命中某后端 `roles`（R4c） | 角色名精确相等，或以 `claude-<role>-` 开头；转发并改写顶层 `model` |
| 以上均未命中，且某后端声明 `*`（R4d） | 转发到第一个声明 `*` 的后端并改写顶层 `model` |
| 未命中 / 无 model / 非字符串 / null / 非法 JSON（R5/R6） | 转发到该协议列表第一个后端，**不改写** |
| 请求体超过 10 MiB | 413，不转发 |

判断顺序：按 `backends` 列表顺序逐个后端判断，先判精确 `model`（R4a），再判 `matches`（R4b），再判具体角色（R4c）；全部未命中后才进入 `*` 托底（R4d）。

### 错误体

OpenAI 入口：`{"error":{"message":"...","type":"...","code":"..."}}`。
Anthropic 入口：`{"type":"error","error":{"type":"...","message":"..."}}`。

| 场景 | HTTP | OpenAI | Anthropic |
|---|---|---|---|
| 缺 / 错客户端 token | 401 | `invalid_request_error` + `missing_api_key` / `invalid_api_key` | `authentication_error` |
| 未知路径 | 404 | `invalid_request_error` + `not_found` | — |
| 无对应协议后端 | 502 | `server_error` + `upstream_error` | `api_error` |
| 上游连接失败 / 超时 | 502 | `server_error` + `upstream_error` | `api_error` |
| 请求体超限 | 413 | `invalid_request_error` + `request_too_large` | `request_too_large` |

## 构建

一键构建脚本（默认构建当前平台，验收平台为 windows/amd64）：

```bash
bash build.sh
./bin/model-router-0.0.1-rc-windows-amd64.exe -version
# 输出：0.0.1-rc
```

- 纯 Go 构建，始终 `CGO_ENABLED=0`。
- 版本号唯一来源：[`internal/version/version.txt`](internal/version/version.txt)，构建脚本读取并以 `-ldflags "-X model-router/internal/version.Version=<version>"` 注入；未注入时（如 `go run .`）回退到该文件内容。
- 产物输出到 `bin/`，命名 `model-router-<version>-<os>-<arch>[.exe]`。
- 可覆盖平台：`GOOS=linux GOARCH=arm64 bash build.sh`（M6 前不作为验收项）。

## 目录结构

```
.
├── main.go                    # 入口：参数解析、启动/优雅停止
├── internal/
│   ├── config/                # 静态 JSON 配置加载与校验（backends、matches、roles）
│   ├── proxy/                 # 双协议入口、按协议认证、按 model/别名/角色路由与透传
│   └── version/               # 版本号唯一来源 + ldflags 注入
├── config.example.json        # 配置样例（openai + DeepSeek anthropic 入口）
├── build.sh                   # 一键构建脚本
├── README.md
├── CHANGELOG.md
└── docs/
    ├── requirements.md        # 需求规格（产品经理维护）
    ├── design.md              # 设计与决策（Claude Code 维护）
    └── tests.md               # 测试用例（Claude Code 维护）
```

## 版本规则

- 开发版本从 `0.0.1` 起递增 patch 位；构建版本带 `-rc` 后缀（未封版）。
- 封版后正式版本去掉 `-rc`；正式发布才定为 `1.0.0`。
- 版本升级由用户决定，详见 [AGENTS.local.md](AGENTS.local.md)。
