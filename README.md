# model-router

OpenAI / Anthropic 兼容的**模型路由代理**——不转协议，只代理透传。

> 当前版本：`0.0.1-rc`（M3-providers：供应商预设 + 实例 provider 标记 + 通用 models_override）
> 需求：[docs/requirements.md](docs/requirements.md) ｜ 设计：[docs/design.md](docs/design.md) ｜ 测试：[docs/tests.md](docs/tests.md) ｜ 变更：[CHANGELOG.md](CHANGELOG.md)

## 能力范围（M1 ～ M3.1）

- 两个协议入口，互不转换：`POST /v1/chat/completions`（OpenAI）、`POST /v1/messages`（Anthropic）。
- **服务访问认证**：OpenAI 入口 `Authorization: Bearer <auth_token>`；Anthropic 入口 `x-api-key: <auth_token>`；失败 401（按入口协议错误体）。
- **实例化配置**：配置 `instances` 有序数组；每个实例 = 协议 + `base_url` + `api_key` + `timeout`，并声明自己的模型映射（OpenAI 用 `models` 数组，Anthropic 用 `roles` 对象）。
- **供应商预设**：内置 11 个供应商（deepseek/openai/anthropic/zhipu/volcengine/moonshot/dashscope/siliconflow/groq/mistral/xai）。实例可用可选 `provider` 标记来源；`provider` 只影响「查询模型」的端点判定（通用 `models_override` 机制），不影响路由/转发。
- **实例展开路由**：
  - 精确匹配（R4a）：请求 `model` 等于某个声明的模型 → 转发该实例，**不改写**。
  - 角色匹配（R4c）：`model` 等于 `sonnet`/`opus`/`fable`/`haiku` 或以 `claude-<role>-` 开头 → 命中声明该角色的条目，改写顶层 `model` 为实例映射的模型。
  - 托底（R4d）：未命中精确/角色且存在 `"*"` 角色 → 命中第一个，改写顶层 `model`。
  - 回退（R5）：都未命中 → 转发展开后的首条目，不改写。
- **透传**：方法、路径、查询串、请求体（别名/角色命中时仅改顶层 `model`）与非认证请求头原样转发；响应原样回传，SSE 逐块 flush。
- **上游认证分离**：OpenAI 实例使用 `Authorization: Bearer <api_key>`，Anthropic 实例使用 `x-api-key: <api_key>`；客户端 token 永不外传。
- **模型列表 `/v1/models`**：本地聚合配置声明的模型（Anthropic 不含 `*` 托底值），按认证头判定协议并返回对应格式；不调上游。
- **Web 配置页 `/admin`**：可视化模式（OpenAI/Anthropic 双 tab、添加实例时先选供应商或自定义、测试连接、查询上游模型、勾选/手动填写）+ JSON 高级模式（校验通过才可保存）。
- **管理 API**：`GET/PUT /api/config`、`POST /api/reload`、`POST /api/config/validate`、`POST /api/probe/test`、`POST /api/probe/models`、`GET /api/providers`；HTTP Basic Auth。
- **手动重载**：重读 `-config` 文件并原子切换，进程不重启；失败保留旧配置。
- **环境变量覆盖**：`listen`/`auth_token`/`admin_user`/`admin_password` 可用 `MODEL_ROUTER_*` 覆盖。
- 版本号单一来源，构建通过 `-ldflags` 注入；`-version` 输出构建版本。

M4 及之后（回退降级、跨平台 CI/Docker）见需求文档第 6 节。

## 快速开始

1. 准备配置（示例见 [config.example.json](config.example.json)）：

   ```json
   {
     "listen": "127.0.0.1:18080",
     "auth_token": "replace-with-your-client-token",
     "admin_user": "admin",
     "admin_password": "replace-with-your-admin-password",
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
         "roles": {
           "sonnet": "deepseek-v4-flash",
           "opus": "deepseek-v4-pro",
           "fable": "deepseek-v4-pro",
           "haiku": "deepseek-v4-flash",
           "*": "deepseek-chat"
         }
       }
     ]
   }
   ```

2. 启动代理：

   ```bash
   go run . -config config.json
   ```

   启动日志会打印监听地址、实例数量与每个实例的协议/地址/模型映射（不打印密钥）。配置了 `admin_password` 时，浏览器访问 `http://127.0.0.1:18080/admin`（HTTP Basic Auth，用户名/密码为 `admin_user`/`admin_password`）。

3. 调用 OpenAI 入口：

   ```bash
   curl -sS -X POST http://127.0.0.1:18080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer <auth_token>" \
     -d '{"model":"deepseek-chat","messages":[{"role":"user","content":"ping"}]}'
   ```

4. 调用 Anthropic 入口：

   ```bash
   curl -sS -X POST http://127.0.0.1:18080/v1/messages \
     -H "Content-Type: application/json" \
     -H "x-api-key: <auth_token>" \
     -H "anthropic-version: 2023-06-01" \
     -d '{"model":"sonnet","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}'
   ```

   `model=sonnet` 命中角色映射，代理会改写为实例声明的实际模型（上例 `deepseek-v4-flash`）；也可直接发送 `claude-sonnet-4-5` 这类官方版本 ID（`claude-<role>-` 前缀）。

## 配置说明

顶层字段：

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 监听地址，形如 `host:port` |
| `auth_token` | 是 | — | 客户端访问本代理的 token；OpenAI 入口走 Bearer，Anthropic 入口走 `x-api-key` |
| `admin_user` | 否 | `admin` | 管理页/管理 API 的 Basic Auth 用户名 |
| `admin_password` | 否 | 空（管理面禁用） | 管理页/管理 API 的 Basic Auth 密码 |
| `instances` | 是 | — | 实例有序数组，非空；顺序即路由优先级 |

`instances[]` 字段：

| 字段 | 必填 | 说明 |
|---|---|---|
| `protocol` | 是 | `openai` / `anthropic`（大小写不敏感，归一化小写） |
| `base_url` | 是 | 绝对 http(s) URL；去尾部 `/` |
| `api_key` | 是 | 上游密钥；OpenAI 用 `Authorization: Bearer`，Anthropic 用 `x-api-key` |
| `provider` | 否 | 内置供应商 id（见下节）；非空必须是已知 id，否则启动失败；空=自定义 |
| `timeout` | 否 | 连接/等待响应头超时，默认 `15s` |
| `models` | openai 必填 | 该实例对外提供的模型列表；去空去重、保留顺序；不得同时出现 `roles` |
| `roles` | anthropic 必填 | 角色→实际模型映射，键 ∈ `sonnet`/`opus`/`fable`/`haiku`/`*`，值非空；不得同时出现 `models` |

旧字段 `backends` 已废弃：出现即解析失败并提示迁移到 `instances`。`matches`/角色数组等旧写法也随 `backends` 一并废弃。

### 供应商预设

`GET /api/providers` 返回内置 11 个供应商（纯数据，可后续增改）：

| id | name | OpenAI base_url | Anthropic base_url |
|---|---|---|---|
| deepseek | DeepSeek | https://api.deepseek.com | https://api.deepseek.com/anthropic |
| openai | OpenAI | https://api.openai.com/v1 | — |
| anthropic | Anthropic | — | https://api.anthropic.com |
| zhipu | 智谱 GLM | https://open.bigmodel.cn/api/paas/v4 | https://open.bigmodel.cn/api/anthropic |
| volcengine | 火山方舟(豆包) | https://ark.cn-beijing.volces.com/api/v3 | https://ark.cn-beijing.volces.com/api/anthropic |
| moonshot | 月之暗面 Kimi | https://api.moonshot.cn/v1 | https://api.moonshot.cn/anthropic |
| dashscope | 通义千问 | https://dashscope.aliyuncs.com/compatible-mode/v1 | — |
| siliconflow | 硅基流动 | https://api.siliconflow.cn/v1 | — |
| groq | Groq | https://api.groq.com/openai/v1 | — |
| mistral | Mistral | https://api.mistral.ai/v1 | — |
| xai | xAI Grok | https://api.x.ai/v1 | — |

其中 DeepSeek 声明了通用 `models_override`：`anthropic → {base_url: https://api.deepseek.com, auth: bearer}`。这样 DeepSeek 的 Anthropic 实例（其入口未实现 `/v1/models`）在「查询模型」时改从 OpenAI 入口拉取；其他供应商以后若遇到同类问题，只需在预设数据中补一条 override，无需改逻辑。

## 路由规则

运行时按 `instances` 顺序展开为扁平条目：OpenAI 实例的每个 `models` 项一条；Anthropic 实例的每个 `roles` 键值一条（展开顺序固定为 sonnet、opus、fable、haiku、`*`）。

| 场景 | 行为 |
|---|---|
| `/v1/chat/completions` | 只使用 openai 实例展开的条目 |
| `/v1/messages` | 只使用 anthropic 实例展开的条目 |
| 该协议没有实例 | 502（按入口协议错误体） |
| 请求 `model` 精确命中条目的模型（R4a） | 转发该条目，**不改写** |
| 请求 `model` 命中具体角色（R4c） | 转发并改写顶层 `model` 为该条目实际模型 |
| 未命中精确/角色且有 `*`（R4d） | 转发第一个 `*` 条目并改写 |
| 无 model / 非字符串 / null / 非法 JSON / 未命中（R5） | 转发首条目，**不改写** |
| 请求体超过 10 MiB | 413，不转发 |

判定顺序：对展开后的条目逐条检查 R4a → R4c；全部未命中后再扫描 `*`（R4d）；仍无则 R5。显式模型优先于角色，具体角色优先于托底。

## 模型列表 `/v1/models`

```bash
# OpenAI 格式
curl -sS http://127.0.0.1:18080/v1/models -H "Authorization: Bearer <auth_token>"
# Anthropic 格式（仅带 x-api-key 时）
curl -sS http://127.0.0.1:18080/v1/models -H "x-api-key: <auth_token>"
```

- 只列配置声明：OpenAI 实例的 `models`、Anthropic 实例 `roles` 的具体角色值（不含 `*`），按实例/条目顺序去重。
- 仅 GET；其他方法 405。
- 同时带两种认证头时按 OpenAI 处理。

## 管理页与管理 API

设置非空 `admin_password` 后启用；否则 `/admin*`、`/api/*` 一律 404。所有管理请求使用 HTTP Basic Auth，失败 401 + `WWW-Authenticate: Basic realm="model-router"`。

页面两种模式：

- **可视化模式**：OpenAI / Anthropic 双 tab；「添加实例」先弹出供应商选择框（内置供应商 + 自定义），选内置会按当前协议自动填 `base_url` 并记录 `provider`，自定义则手动填写；每张实例卡片可「测试连接」「查询模型」（带上 `provider`，自动走 models_override）、勾选/手填模型；保存 → `PUT /api/config`。
- **JSON 高级模式**：编辑完整配置；「校验」→ `POST /api/config/validate`，通过后才启用「保存」。

| 路径 | 方法 | 行为 |
|---|---|---|
| `/admin`、`/admin/` | GET | 内嵌配置页（原生 HTML+JS，无外部 CDN） |
| `/api/config` | GET | 返回当前**生效**配置（含真实 key） |
| `/api/config` | PUT | 校验后原子写回 `-config` 文件，**不生效** |
| `/api/config/validate` | POST | 对请求体做全量校验（dry-run），返回 `{ok}`；不写回 |
| `/api/reload` | POST | 重读文件并原子切换生效；失败保持旧配置 |
| `/api/probe/test` | POST | 按协议探测上游：openai `GET {base_url}/v1/models`；anthropic `POST {base_url}/v1/messages` 最小请求。返回 `{ok, latency_ms, message}`（2xx 或认证通过的 4xx 视为可达；401/403/404 视为失败） |
| `/api/probe/models` | POST | `GET {base_url}/v1/models`（openai Bearer / anthropic x-api-key + version）；`provider` 命中 models_override 时改用 override 的 base_url/auth。返回 `{models:[...]}`；上游未实现该端点（404）时返回友好错误，引导手动输入 |
| `/api/providers` | GET | 返回内置供应商清单（id/name/base_urls/models_override） |

probe 请求体：`{protocol, base_url, api_key, provider?, timeout?}`。`provider` 仅 `probe/models` 使用（通用 override）；`probe/test` 按协议选择端点（openai `GET /v1/models`；anthropic `POST /v1/messages` 最小请求），忽略 `provider`。probe 不修改配置、不缓存。

保存与重载分离：先「保存」写文件，再「重载」生效。重载不会改变监听地址（`listen` 变更需重启进程）。环境变量只影响生效值；若在页面上保存，会把当前生效值写回文件。

## 环境变量覆盖

| 字段 | 环境变量 |
|---|---|
| `listen` | `MODEL_ROUTER_LISTEN` |
| `auth_token` | `MODEL_ROUTER_AUTH_TOKEN` |
| `admin_user` | `MODEL_ROUTER_ADMIN_USER` |
| `admin_password` | `MODEL_ROUTER_ADMIN_PASSWORD` |

非空才覆盖；覆盖发生在默认值与校验之前；`instances` 不支持环境变量。

## 错误体

OpenAI 入口：`{"error":{"message":"...","type":"...","code":"..."}}`。
Anthropic 入口：`{"type":"error","error":{"type":"...","message":"..."}}`。

| 场景 | HTTP | OpenAI | Anthropic |
|---|---|---|---|
| 缺 / 错客户端 token | 401 | `invalid_request_error` + `missing_api_key` / `invalid_api_key` | `authentication_error` |
| 未知路径 | 404 | `invalid_request_error` + `not_found` | — |
| `/v1/models` 非 GET | 405 | `invalid_request_error` + `method_not_allowed` | `invalid_request_error` |
| 该协议无实例 | 502 | `server_error` + `upstream_error` | `api_error` |
| 上游连接失败 / 超时 | 502 | `server_error` + `upstream_error` | `api_error` |
| 请求体超限 | 413 | `invalid_request_error` + `request_too_large` | `request_too_large` |

## 构建

```bash
bash build.sh
./dist/model-router-0.0.1-rc-windows-amd64.exe -version
# 输出：0.0.1-rc
```

- 纯 Go，`CGO_ENABLED=0`。版本号唯一来源 `internal/version/version.txt`，构建脚本通过 `-ldflags -X` 注入。
- 产物输出到 `dist/`，命名 `model-router-<version>-<os>-<arch>[.exe]`；可用 `GOOS`/`GOARCH` 覆盖平台。

## 目录结构

```
.
├── main.go                    # 入口：参数解析、启动/优雅停止
├── internal/
│   ├── config/                # 配置解析/校验/原子保存、instances、环境变量覆盖
│   ├── providers/             # 内置供应商预设（纯数据）与 models_override 解析
│   ├── proxy/                 # 双协议入口、认证、实例路由、/v1/models、管理 API、原子重载
│   ├── version/               # 版本号唯一来源 + ldflags 注入
│   └── web/                   # 内嵌管理页（go:embed，无外部 CDN）
├── config.example.json        # instances 配置样例
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
