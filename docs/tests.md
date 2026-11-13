# model-router 测试用例（M1 / M2）

> 对应需求：[requirements.md](requirements.md)（FR-4 修订、§7.3、§7.7 `matches`、§7.8 `roles`）；设计：[design.md](design.md)。

## 1. 测试策略

- 配置用例直接驱动 `config.Load`，覆盖结构、`matches`/`roles` 归一化与 fail-closed 校验。
- 代理用例用 `net/http/httptest` 启动 mock 上游与真实 Handler，断言认证方式、路由选择、别名/角色改写与透传字节，不 mock 代理内部逻辑。
- 每个后端 mock 记录最后一次请求与调用次数，用于验证"发到了哪个后端""是否转发""body 是否被改写"。
- 全部用例离线执行，不依赖真实供应商。

## 2. 配置用例（`internal/config`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-001 | 默认值与归一化 | `TestLoadAppliesDefaults` | `listen`/`timeout` 默认、`protocol` 大小写归一化、`base_url` 去尾斜杠、`api_key` 去空白 | 默认监听、默认 15s、`protocol=openai`、字段保留 |
| UT-002 | 多后端顺序与超时 | `TestLoadPreservesBackendOrder` | `backends` 有序、逐项 timeout、省略 `matches`/`roles` | 顺序不变；20s / 1m 正确；`matches`/`roles` 为空 |
| UT-003 | `matches` 归一化 | `TestLoadNormalizesMatches` | `[" alias-a ", "", "   ", "alias-b"]` | 结果为 `["alias-a","alias-b"]`（去空白、丢弃空项） |
| UT-004 | `roles` 归一化 | `TestLoadNormalizesRoles` | `[" Sonnet ", "HAIKU", "*"]`、空数组 | 结果为 `["sonnet","haiku","*"]`；空数组视为未配置 |
| UT-005 | 非法 `roles` 拒绝 | `TestLoadRejectsInvalidRole` | 子用例：gemini / best / sonnet[1m] / opusplan / 空串 / 全空白 / 合法+非法混合 | 全部 `Load` 返回错误 |
| UT-006 | 空 backends 拒绝 | `TestLoadRejectsEmptyBackends` | `backends: []` | `Load` 返回错误 |
| UT-007 | 缺 auth_token 拒绝 | `TestLoadRejectsMissingAuthToken` | fail-closed | `Load` 返回错误 |
| UT-008 | 未知字段拒绝 | `TestLoadRejectsUnknownField` | 旧 `backend` 字段视为未知 | `Load` 返回错误 |
| UT-009 | 非法 backend 条目拒绝 | `TestLoadRejectsInvalidBackend` | 子用例：missing_protocol / invalid_protocol / missing_base_url / relative_base_url / missing_api_key / missing_model / invalid_timeout | 每个子用例均返回错误 |

## 3. 代理用例（`internal/proxy`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-101 | OpenAI 透传（R1） | `TestOpenAIPassThroughVerbatim` | 客户端 `Authorization: Bearer <auth_token>`；方法/路径/查询串/body/非认证头透传；上游 `Authorization: Bearer <openai.api_key>` | 后端收到原值；响应 201 与响应头/响应体逐字节一致 |
| UT-102 | Anthropic 透传（R2/§7.3） | `TestAnthropicPassThroughVerbatim` | 客户端认证 `x-api-key: <auth_token>`；同时携带 `Authorization: Bearer client-token` 证明被忽略/剔除；上游 `x-api-key=<anthropic.api_key>`；`anthropic-version`/`anthropic-beta`/`Content-Type`/自定义头透传 | 后端收到原值与原 body；上游无 `Authorization`；响应原样回传 |
| UT-103 | 缺客户端 token（两入口） | `TestMissingAuthorizationReturns401` | openai 无 Authorization / anthropic 无 x-api-key | 401；OpenAI `missing_api_key`；Anthropic `authentication_error` + `missing x-api-key header`；后端调用 0 |
| UT-104 | 错 token（两入口） | `TestWrongTokenReturns401` | openai 错 Bearer / anthropic 错 x-api-key | 401；OpenAI `invalid_api_key`；Anthropic `authentication_error` + `invalid service access token`；后端调用 0 |
| UT-105 | Anthropic 拒绝 Bearer | `TestAnthropicRejectsAuthorizationBearer` | 仅带正确 `Authorization: Bearer` | 401 `authentication_error`；后端调用 0 |
| UT-106 | OpenAI 拒绝 x-api-key | `TestOpenAIRejectsXAPIKeyOnly` | 仅带正确 `x-api-key` | 401 `missing_api_key`；后端调用 0 |
| UT-107 | 未知路径 | `TestUnknownPathNotForwarded` | `/v1/models` + 合法 Bearer | 404 `not_found`；后端调用 0 |
| UT-108 | 按 model 路由（R4a/R5） | `TestRouteByModel` | 命中第一个、命中第二个、未命中 | 命中项被调用；未命中落到首个，body 不改写 |
| UT-109 | 回退首后端（R5/R6） | `TestRouteFallsBackToFirst` | 无 model、数字、null、非法 JSON、顶层数组 | 均转发首个后端；body 原样 |
| UT-110 | 别名路由与改写（R4a/R4b/§7.7） | `TestRouteByMatches` | alias 命中改写、第二别名命中、精确 model 不改写、未命中不改写、同后端 model/matches 同名 R4a 优先 | 改写仅发生在 R4b；其余字段语义不变 |
| UT-111 | 角色路由与改写（R4c/§7.8） | `TestRouteByRoles` | 子用例：`sonnet`/`haiku` 精确命中；`claude-sonnet-*`、`claude-opus-*` 前缀命中；精确 model 优先于同后端角色（不改写）；同角色多后端取第一个；`best`/`sonnet[1m]`/`opus[1m]`/`opusplan`/大小写变体未命中走 R5；`roles` 与 `matches` 共存均可改写 | 命中正确后端；R4c 改写顶层 model 且其余字段语义不变；R4a 不改写；未处理值回退 L[0] 且不改写 |
| UT-112 | `*` 托底（R4d/§7.8） | `TestRouteWildcardFallback` | 未知 model 命中第一个 `*` 后端并改写；具体角色优先于 `*`；显式别名优先于 `*`；多个 `*` 取第一个；无 model/数字/null/布尔/对象/非法 JSON/顶层数组不触发 `*` | 未知字符串走第一个 `*` 并改写；其余走 R5 L[0] 且 body 不变 |
| UT-113 | 协议列表隔离（R1/R2） | `TestProtocolListsAreIndependent` | 两协议各一后端，模型名交叉 | 请求只到同协议后端 |
| UT-114 | 无对应协议后端（R3） | `TestNoBackendForProtocol` | 有 openai 无 anthropic；有 anthropic 无 openai | 502 按入口协议；后端调用 0 |
| UT-115 | 请求体超限（R7） | `TestRequestTooLarge` | 两入口各发送 `10 MiB + 1` 字节 | 413 按入口协议；后端调用 0 |
| UT-116 | 上游失败错误体 | `TestUpstreamFailureErrorFormat` | 两入口，upstream 已关闭 | 502 按入口协议 |

错误体断言：OpenAI 错误体必须可解析且 `message`/`type`/`code` 非空；Anthropic 错误体顶层 `type=error`，内层 `type`/`message` 非空；两者 `Content-Type` 均为 `application/json`。

## 4. 端到端用例（手工）

### ET-001 OpenAI 入口

1. 启动 mock 后端，配置一个 `protocol=openai` 后端指向它。
2. `go run . -config <配置>`。
3. 带 `Authorization: Bearer <auth_token>` 调用 `/v1/chat/completions`。

预期：200；mock 收到 `POST /v1/chat/completions`、`Authorization: Bearer <openai.api_key>`、原始 body。

### ET-002 Anthropic 入口与别名改写

1. 配置一个 `protocol=anthropic` 后端：`model=deepseek-v4-flash`、`matches=["claude-sonnet-4-5"]`。
2. 带 `x-api-key: <auth_token>` 调用 `/v1/messages`，body 中 `model=claude-sonnet-4-5`。

预期：200；mock 收到 `x-api-key: <api_key>`、无 `Authorization`、body 顶层 `model` 已改为 `deepseek-v4-flash`。

### ET-003 角色与 `*` 托底（推荐）

1. 配置 anthropic 后端：flash `roles=["sonnet","haiku"]`、pro `roles=["opus","fable"]`、fallback `roles=["*"]`。
2. 分别发送 `model=sonnet`、`model=claude-haiku-4-5-20251001`、`model=some-unknown`、`model=best`。

预期：sonnet/haiku 前缀改写为 flash 的 model；unknown 与 best（含 `*` 时）改写为 fallback 的 model；无 `*` 时 best 回退 L[0] 且不改写。

### ET-004 认证与错误格式

1. Anthropic 入口仅带 `Authorization: Bearer <auth_token>` → 401 `authentication_error`。
2. OpenAI 入口仅带 `x-api-key: <auth_token>` → 401 `missing_api_key`。
3. 两入口不带/错 token → 401；`/v1/messages` 无 anthropic 后端（R3）→ 502；两入口 >10 MiB body（R7）→ 413。

### ET-005 示例配置

检查 `config.example.json`：`backends` 含 openai / anthropic 示例；anthropic 的 flash 项含 `roles=["sonnet","haiku"]`，pro 项含 `roles=["opus","fable"]`，并有一个 `roles=["*"]` 的托底后端。

### ET-006 构建与版本

1. `bash build.sh`
2. `./bin/model-router-0.0.1-rc-windows-amd64.exe -version`

预期：产物为 windows/amd64 可执行文件；输出 `0.0.1-rc`。

## 5. 执行方式

```bash
go build ./...
go vet ./...
go test ./...
bash build.sh
```

## 6. 验收对照（§7.8）

| 验收项 | 对应用例 |
|---|---|
| `roles` 枚举合法解析（含 `*`）、大小写归一化、空数组 | UT-004 |
| 非法 `roles` 启动失败 | UT-005 |
| R4c：角色名精确命中 → 改写 | UT-111 |
| R4c：`claude-<role>-` 前缀命中 → 改写 | UT-111 |
| R4c：区分大小写 | UT-111（`Sonnet` 未命中） |
| R4a 优先于 R4c（同后端不改写） | UT-111 |
| R4b/R4c 与 `roles`/`matches` 共存、判断顺序 | UT-110、UT-111 |
| R4d：未知合法字符串命中第一个 `*` 后端并改写 | UT-112 |
| R4c 优先于 R4d | UT-112 |
| R4b 优先于 R4d | UT-112 |
| 多个 `*` 取列表第一个 | UT-112 |
| 无 model / null / 非字符串不触发 `*`，走 R5 不改写 | UT-109、UT-112 |
| 未命中任何规则（无 `*`）→ R5 首后端不改写 | UT-108、UT-109、UT-111 |
| `best`/`sonnet[1m]`/`opus[1m]`/`opusplan` 不特殊处理 | UT-111（无 `*` 时 R5）；UT-112（有 `*` 时托底，见 design.md D-M2-18） |
| `config.example.json` 含 roles 与 `*` 示例 | ET-005 |
| 文档同步（README/CHANGELOG/design/tests） | 本文件与 design.md、README.md、CHANGELOG.md |
| 构建产物与 `-version` 0.0.1-rc | ET-006 |
