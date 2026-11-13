# model-router

OpenAI 兼容的**模型路由代理**——不转协议，只代理透传。

> 当前版本：`0.0.1-rc`（M1 里程碑：单模型透传壳子 + 服务访问认证 + 静态配置 + 构建脚本）
> 需求：[docs/requirements.md](docs/requirements.md) ｜ 设计：[docs/design.md](docs/design.md) ｜ 测试：[docs/tests.md](docs/tests.md) ｜ 变更：[CHANGELOG.md](CHANGELOG.md)

## M1 能力范围

- 监听配置端口，提供 OpenAI 协议入口 `POST /v1/chat/completions`（其他路径返回 404）。
- **服务访问认证（FR-4）**：客户端必须携带 `Authorization: Bearer <auth_token>`，缺失或不匹配返回 401（OpenAI 兼容错误体）。
- 请求**透传**到后端模型：HTTP 方法、路径、查询串、请求体及其他请求头均不修改；`Authorization` 例外——代理统一替换为配置的 `api_key`，客户端 token 不会转发给上游。
- 响应**原样回传**：状态码、响应头、响应体均不修改；SSE 流式响应逐块转发不缓冲。
- 后端 `base_url`、`api_key`、模型名、超时（默认 15s）、监听地址、服务访问 token 全部来自静态 JSON 配置。
- 版本号单一来源，构建时通过 `-ldflags` 注入；`-version` 打印构建版本。

M2 及之后（Anthropic 入口、多模型排序、自动发现、回退降级、Web 配置）见需求文档第 6 节。

## 快速开始

1. 准备配置（示例见 [config.example.json](config.example.json)）：

   ```bash
   cp config.example.json config.json
   # 编辑 config.json：填入 auth_token（客户端访问本代理用）、
   # backend.base_url / backend.api_key（上游密钥）/ backend.model
   ```

2. 启动代理：

   ```bash
   go run . -config config.json
   ```

   启动日志会打印监听地址、后端地址、模型名与版本，例如：

   ```
   model-router 0.0.1-rc listening on 127.0.0.1:18080 -> https://api.openai.com (model "gpt-4o-mini", timeout 15s)
   ```

3. 调用（`Authorization` 携带的是**本代理**的服务访问 token，不是上游 key）：

   ```bash
   curl -sS -X POST http://127.0.0.1:18080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer <auth_token>" \
     -d '{
       "model": "gpt-4o-mini",
       "messages": [{"role": "user", "content": "ping"}]
     }'
   ```

   代理校验通过后，转发上游时会把 `Authorization` 固定替换为 `Bearer <backend.api_key>`；客户端 token 仅用于访问本代理，绝不外传。缺失或错误 token 返回 `401`，错误体形如：

   ```json
   {"error":{"message":"invalid service access token","type":"invalid_request_error","code":"invalid_api_key"}}
   ```

## 配置说明

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 监听地址，形如 `host:port` |
| `auth_token` | 是 | — | 客户端访问本代理必须携带的 Bearer token（FR-4） |
| `backend.base_url` | 是 | — | 后端模型地址，如 `https://api.openai.com`；可带路径前缀，请求路径会追加在其后 |
| `backend.api_key` | 是 | — | 上游密钥；转发时固定使用 `Authorization: Bearer <api_key>` |
| `backend.model` | 否 | — | 模型名（M1 仅用于启动日志与后续路由，不修改请求体） |
| `backend.timeout` | 否 | `15s` | 连接与等待响应头超时，Go duration 字符串（如 `15s`、`1m`）；不截断流式响应体 |

配置采用严格解析：未知字段、缺失 `auth_token` / `backend.api_key` / `backend.base_url`、非法 URL 或非法超时都会使启动失败并给出明确错误。

## 构建

一键构建脚本（默认构建当前平台，M1 验收平台为 windows/amd64）：

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
│   ├── config/                # 静态 JSON 配置加载与校验
│   ├── proxy/                 # /v1/chat/completions 透传处理器
│   └── version/               # 版本号唯一来源 + ldflags 注入
├── config.example.json        # 配置样例
├── build.sh                   # 一键构建脚本
├── README.md
├── CHANGELOG.md
└── docs/
    ├── requirements.md        # 需求规格（产品经理维护）
    ├── design.md              # M1 设计（Claude Code 维护）
    └── tests.md               # M1 测试用例（Claude Code 维护）
```

## 版本规则

- 开发版本从 `0.0.1` 起递增 patch 位；构建版本带 `-rc` 后缀（未封版）。
- 封版后正式版本去掉 `-rc`；正式发布才定为 `1.0.0`。
- 版本升级由用户决定，详见 [AGENTS.local.md](AGENTS.local.md)。
