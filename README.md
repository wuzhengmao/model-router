# model-router

OpenAI / Anthropic 兼容的**模型路由代理**——不转协议，只代理透传。

> 当前版本：`0.1.0`（M4.6：gzip 解压修复 + 实时低速检测 + 实例 name 日志 + 请求日志 in/out/速度）
> 需求：[docs/requirements.md](docs/requirements.md) ｜ 设计：[docs/design.md](docs/design.md) ｜ 测试：[docs/tests.md](docs/tests.md) ｜ 变更：[CHANGELOG.md](CHANGELOG.md)

## 能力范围（M1 ～ M4）

- 两个协议入口，互不转换：`POST /v1/chat/completions`（OpenAI）、`POST /v1/messages`（Anthropic）。
- **服务访问认证**：OpenAI 入口 `Authorization: Bearer <auth_token>`；Anthropic 入口 `x-api-key: <auth_token>` 或 `Authorization: Bearer <auth_token>`（双认证，任一匹配即通过）；失败 401（按入口协议错误体）。
- **实例化配置**：配置 `instances` 按协议分组为 `openai` / `anthropic` 两个有序数组；每个实例 = `base_url` + `api_key` + `timeout`，并声明自己的模型映射（OpenAI 用 `models` 数组，Anthropic 用 `roles` 对象）。
- **回退参数可配置**：顶层 `fallback` 对象可设置 `failure_threshold`（默认 3）、`degrade_duration`（默认 `5m`）、`first_token_timeout`（默认 `10s`，0=禁用）、`min_tokens_per_second`（默认 30，0=禁用吞吐判定）、`idle_timeout`（默认 `30s`，0=禁用流式空闲超时）。
- **供应商预设**：内置 11 个供应商（deepseek/openai/anthropic/zhipu/volcengine/moonshot/dashscope/siliconflow/groq/mistral/xai）。实例可用可选 `provider` 标记来源；`provider` 只影响「查询模型」的端点判定（通用 `models_override` 机制），不影响路由/转发。
- **实例展开路由**：
  - 精确匹配（R4a）：请求 `model` 等于某个声明的模型 → 转发该实例，**不改写**。
  - 角色匹配（R4c）：`model` 等于 `sonnet`/`opus`/`fable`/`haiku` 或以 `claude-<role>-` 开头 → 命中声明该角色的条目，改写顶层 `model` 为实例映射的模型；尾部 `[1m]` 后缀（如 `opus[1m]`）在判定前剥除，大小写不敏感。
  - 托底（R4d）：未命中精确/角色且存在 `"*"` 角色 → 命中第一个，改写顶层 `model`。
  - 回退（R5）：都未命中 → 转发展开后的首条目，不改写。
- **实例粒度回退降级（M4）**：每个协议下实例按配置顺序构成候选链；连接失败/超时自动在本次请求内回退到下一候选；连续失败 3 次进入 5 分钟持续回退（跳过该实例）；回退期结束后首次探测失败立即重新计时；成功清零。吞吐判定仅在「生成时长 >2s 且输出 >100 token」时进行，低于 `min_tokens_per_second` 记为失败（已发出的响应不回退、不打断）。
- **透传**：方法、路径、查询串、请求体（别名/角色命中时仅改顶层 `model`）与非认证请求头原样转发；响应原样回传，SSE 逐块 flush。
- **上游认证分离**：OpenAI 实例使用 `Authorization: Bearer <api_key>`，Anthropic 实例使用 `x-api-key: <api_key>`；客户端 token 永不外传。
- **请求日志**：每个请求打印 `req_model`（客户端原始模型）、`model`（实际转发模型）、`client`（客户端标识，十一级识别）、`ip`（来源 IP）与 `total`/`ttft`/`in`/`out`/`gen`（`gen` 满足吞吐门槛才显示数值，否则 `n/a`）。
- **模型列表 `/v1/models`**：本地聚合配置声明（OpenAI 返回 `models` 值；Anthropic 返回已声明的角色名 `sonnet`/`opus`/`fable`/`haiku`，固定顺序、不含 `*`），按认证头判定协议并返回对应格式；不调上游。
- **Web 配置页 `/admin`**：可视化模式（回退参数输入框、OpenAI/Anthropic 双 tab、添加实例时先选供应商或自定义、实例卡片可拖动排序、测试连接、查询上游模型、勾选/手动填写）+ JSON 高级模式（含 `fallback` 与分组 `instances`；校验通过才可保存）。
- **管理 API**：`GET/PUT /api/config`、`POST /api/reload`、`POST /api/config/validate`、`POST /api/probe/test`、`POST /api/probe/models`、`GET /api/providers`；HTTP Basic Auth。
- **手动重载**：重读 `-config` 文件并原子切换，进程不重启；失败保留旧配置。
- **环境变量覆盖**：`listen`/`auth_token`/`admin_user`/`admin_password` 可用 `MODEL_ROUTER_*` 覆盖。
- 版本号单一来源，构建通过 `-ldflags` 注入；`-version` 输出构建版本。

M5 及之后（跨平台 CI/Docker）见需求文档第 6 节。

## 快速开始

1. 准备配置（示例见 [config.example.json](config.example.json)）：

   ```json
   {
     "listen": "127.0.0.1:18080",
     "auth_token": "replace-with-your-client-token",
     "admin_user": "admin",
     "admin_password": "replace-with-your-admin-password",
     "fallback": {
       "failure_threshold": 3,
       "degrade_duration": "5m",
       "first_token_timeout": "10s",
       "min_tokens_per_second": 30,
       "idle_timeout": "30s"
     },
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

   `model=sonnet` 命中角色映射，代理会改写为实例声明的实际模型（上例 `deepseek-v4-flash`）；也可直接发送 `claude-sonnet-4-5` 这类官方版本 ID（`claude-<role>-` 前缀）。Anthropic 入口也接受 `Authorization: Bearer <auth_token>`（双认证，Claude Code 等 SDK 只发 Bearer 时可用）。

## 配置说明

顶层字段：

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `listen` | 否 | `127.0.0.1:8080` | 监听地址，形如 `host:port` |
| `auth_token` | 是 | — | 客户端访问本代理的 token；OpenAI 入口走 Bearer，Anthropic 入口走 `x-api-key` 或 Bearer（双认证） |
| `admin_user` | 否 | `admin` | 管理页/管理 API 的 Basic Auth 用户名 |
| `admin_password` | 否 | 空（管理面禁用） | 管理页/管理 API 的 Basic Auth 密码 |
| `fallback` | 否 | 见下 | 回退状态机参数 |
| `instances` | 是 | — | 分组对象 `{openai:[...], anthropic:[...]}`；两组顺序各自即优先级，总数 ≥1 |

`fallback` 字段（可视化页面顶部「回退参数」区块可直接编辑；JSON 模式同样可编辑）：

| 字段 | 默认 | 校验 |
|---|---|---|
| `failure_threshold` | 3 | 整数 ≥1 |
| `degrade_duration` | `5m` | Duration 字符串 >0 |
| `first_token_timeout` | `10s` | Duration ≥0；0 = 禁用首 token 超时（留空=缺省 10s） |
| `min_tokens_per_second` | 30 | 浮点 ≥0；0 = 禁用吞吐判定（留空=缺省 30）；仅当生成时长 >2s 且输出 >100 token 时判定 |
| `idle_timeout` | `30s` | Duration ≥0；0 = 禁用流式空闲超时（留空=缺省 30s） |

`instances.openai[]` 字段：

| 字段 | 必填 | 说明 |
|---|---|---|
| `base_url` | 是 | 绝对 http(s) URL；去尾部 `/` |
| `api_key` | 是 | 上游密钥，使用 `Authorization: Bearer` |
| `provider` | 否 | 内置供应商 id（见下节）；非空必须是已知 id，否则启动失败；空=自定义 |
| `timeout` | 否 | 连接/等待响应头超时，默认 `15s` |
| `models` | 是 | 模型列表；去空去重、保留顺序；不得出现 `roles` |

`instances.anthropic[]` 字段：

| 字段 | 必填 | 说明 |
|---|---|---|
| `base_url` | 是 | 绝对 http(s) URL；去尾部 `/` |
| `api_key` | 是 | 上游密钥，使用 `x-api-key` |
| `provider` | 否 | 同上 |
| `timeout` | 否 | 同上 |
| `roles` | 是 | 角色→实际模型映射，键 ∈ `sonnet`/`opus`/`fable`/`haiku`/`*`，值非空；不得出现 `models` |

实例内不再有 `protocol` 字段（由所在分组决定）；出现即解析失败。旧 `instances` 数组形态会报错并提示迁移为分组结构；旧字段 `backends` / `matches` 等同样已废弃。

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

运行时按 `instances` 分组展开：`instances.openai` 的每个实例按 `models` 展开，`instances.anthropic` 的每个实例按 `roles` 键值展开（组内顺序即候选优先级，角色展开顺序固定为 sonnet、opus、fable、haiku、`*`）。

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

## 回退降级（实例粒度，M4）

- 先按路由规则确定**基础实例**，再从基础实例开始按该协议实例顺序构建**候选链**（每个候选给出上游模型与是否需要改写）：
  - OpenAI：实例含请求 `model` → 原样；不含 → 改用该实例 `models[0]` 并改写。
  - Anthropic：实例含角色 R → 用 `roles[R]` 并改写；不含但有 `*` → 用 `roles["*"]` 并改写；两者皆无 → 跳过。
  - R5（无 model/非字符串/null/非法 JSON/未命中）：以首个实例为基础，候选链覆盖该协议实例；Anthropic 只取声明 `*` 的实例；**正文不改写**。
- 每个实例维护连续失败次数与持续回退截止时间（参数来自顶层 `fallback`，默认 3 次 / 5 分钟 / 30 token/s）：
  1. 跳过仍在持续回退期的实例；
  2. 连接失败 / 超时（实例 `timeout`，默认 15s）→ 本次请求立即尝试下一候选（临时回退）；
  3. 连续失败 ≥ `failure_threshold` 次 → 进入持续回退 `degrade_duration`，后续请求跳过该实例；
  4. 回退期结束后的首次探测若再失败 → 立即重新计时；成功则清零恢复；
  5. 所有候选都在回退中 → 强制尝试链头一次；全部失败 → 502。
- 吞吐（M4.4 修订）：耗时按「首数据块→末数据块」的**纯生成时长**计算（建连/TTFT 不计入）。仅当「生成时长 >2s」且「输出 >100 token」且「能拿到 usage」时才计算 token/s，低于 `min_tokens_per_second` 记为失败（仅影响后续请求，已发出的响应不回退、不打断）。不满足样本门槛 → 跳过并视为健康。`min_tokens_per_second=0` 禁用该判定。
- 空闲超时（M4.1）：流式响应 committed 后，连续 `idle_timeout`（默认 30s）无新数据块 → 终止上游读取并记实例失败：
  - Anthropic：先补 `content_block_stop`（若在块内），再补 `message_delta`（`stop_reason=pause_turn`）与 `message_stop`，客户端 SDK 可自动续接。
  - OpenAI：直接中断客户端连接（流截断，不补 `[DONE]`）。
  - 首字节等待仍由实例 `timeout` 控制；`idle_timeout=0` 禁用空闲判定。
- 回退状态仅在进程内存；重启或 reload（重建 handler）后清零。

## 模型列表 `/v1/models`

```bash
# OpenAI 格式
curl -sS http://127.0.0.1:18080/v1/models -H "Authorization: Bearer <auth_token>"
# Anthropic 格式（仅带 x-api-key 时）
curl -sS http://127.0.0.1:18080/v1/models -H "x-api-key: <auth_token>"
```

- OpenAI：收集 openai 实例的 `models` 值，按实例顺序、出现顺序去重。
- Anthropic：返回**预定义角色名** `sonnet`/`opus`/`fable`/`haiku`（不是 `roles` 映射的实际模型名），只返回至少有一个 anthropic 实例声明的角色，固定顺序、去重，不返回 `*`。客户端据此发角色名，代理再经 `roles` 映射到真实模型。
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

当前平台一键构建（产物输出到 `dist/`）：

```bash
bash build.sh
./dist/model-router-0.1.0-windows-amd64.exe -version
# 输出：0.1.0
```

本地全平台构建（windows/linux/darwin × amd64/arm64，共 6 个产物）：

```bash
bash build-all.sh
# 输出汇总：dist/model-router-0.1.0-<os>-<arch>[.exe]
```

- 纯 Go，`CGO_ENABLED=0`。版本号唯一来源 `internal/version/version.txt`，构建脚本通过 `-ldflags -X` 注入。
- 产物命名 `model-router-<version>-<os>-<arch>[.exe]`；`build.sh` 可用 `GOOS`/`GOARCH` 覆盖平台。
- GitHub Actions（`.github/workflows/build.yml`）在 push / PR / tag `v*` 上运行 6 组合构建并上传产物；tag 时创建 GitHub Release，并在 ubuntu runner 上用 buildx 构建 `linux/amd64,linux/arm64` 镜像推送 `ghcr.io/<owner>/model-router:<version>` 与 `:latest`。

## Docker 部署

多阶段构建：`golang:1.27-alpine` 编译静态二进制，运行阶段使用 `gcr.io/distroless/static-debian12:nonroot`（非 root，无 shell）。

```bash
# 本地构建并运行（需要挂载配置文件）
docker build -t model-router:0.1.0 .
docker run -d --name model-router -p 18080:18080 \
  -e MODEL_ROUTER_LISTEN=0.0.0.0:18080 \
  -v "$PWD/.docker:/home/nonroot/config:rw" \
  model-router:0.1.0

curl -sS http://127.0.0.1:18080/v1/models -H "Authorization: Bearer <auth_token>"
```

或使用 docker compose：

```bash
docker compose up -d --build
docker compose logs -f model-router
```

- 镜像内入口固定为 `/model-router -config /home/nonroot/config/config.json`，配置文件由挂载目录 `./.docker` 提供（主机 `.docker/config.json`）；配置含真实密钥，请勿提交。
- 容器默认 `MODEL_ROUTER_LISTEN=0.0.0.0:18080`；也可用 `MODEL_ROUTER_AUTH_TOKEN` 等环境变量覆盖顶层配置（§8.9）。
- 多架构构建：`docker buildx build --platform linux/amd64,linux/arm64 -t <tag> .`（Dockerfile 使用 `TARGETARCH`/`TARGETOS` build args）。
- 镜像不包含健康检查；生产环境请置于 TLS 反向代理之后。

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
├── build.sh                   # 当前平台一键构建（dist/）
├── build-all.sh               # 本地全平台构建（6 个 dist/ 产物）
├── Dockerfile                 # multi-stage、distroless nonroot 镜像
├── docker-compose.yml         # 部署示例（端口/配置挂载/环境变量）
├── .dockerignore
├── .github/workflows/build.yml # CI：6 组合构建 + tag release + 多架构镜像
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
