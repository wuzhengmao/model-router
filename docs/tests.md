# model-router 测试用例（M1 ～ M6）

> 对应需求：[requirements.md](requirements.md)（FR-4、§7、§8、§9）；设计：[design.md](design.md)。

## 1. 测试策略

- 配置用例直接驱动 `config.Load` / `config.Parse` / `config.Save`，覆盖 `instances` 结构、互斥、迁移报错与 fail-closed。
- 代理用例用 `net/http/httptest` 启动 mock 上游与真实 Handler/Server，断言认证、展开路由、改写、管理 API 与 probe 行为。
- 环境变量用例用 `t.Setenv` 隔离。
- 全部用例离线执行，不依赖真实供应商。

## 2. 配置用例（`internal/config`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-001 | 默认值与归一化 | `TestLoadAppliesDefaults` | `listen`/`timeout`/`fallback` 默认、protocol 由分组派生、base_url 去尾斜杠、api_key trim、models 去空去重保序 | 字段归一化正确，参数默认 3/5m/30 |
| UT-002 | 分组建序与 timeout | `TestLoadPreservesInstanceOrder` | openai 组内顺序、anthropic 组、20s/1m | 组内顺序与 timeout 保留 |
| UT-003 | anthropic roles 归一化 | `TestLoadNormalizesAnthropicRoles` | 键大小写/空白、值 trim、`*` | map 键小写、值 trim |
| UT-004 | Duration 序列化往返 | `TestDurationJSONRoundTrip` | `MarshalJSON` 输出 `"20s"`/默认 `"15s"`，再 Parse | 往返时长一致 |
| UT-005 | admin 默认与 trim | `TestLoadAdminDefaults` | `admin_user` 默认/trim、`admin_password` trim | 符合 §8.1 |
| UT-006 | 原子保存往返 | `TestSaveRoundTrip` | Save → Load（分组 instances + fallback） | 全字段一致，文件为分组结构 |
| UT-007 | 空实例拒绝 | `TestLoadRejectsEmptyInstances` | `{"openai":[],"anthropic":[]}` 或 `instances:{}` | Load 报错 |
| UT-008 | 缺 auth_token 拒绝 | `TestLoadRejectsMissingAuthToken` | fail-closed | Load 报错 |
| UT-009 | 未知字段拒绝 | `TestLoadRejectsUnknownField` | 顶层拼写错误 | Load 报错 |
| UT-010 | 旧 backends 迁移提示 | `TestLoadRejectsLegacyBackends` | 出现 `backends` | 报错且包含 `instances` 提示 |
| UT-011 | 非法实例拒绝 | `TestLoadRejectsInvalidInstance` | 12 个子用例：缺/相对 base_url、缺 api_key、openai 缺/空 models、openai 带 roles、anthropic 缺 roles、anthropic 带 models、非法角色键、空角色值、非法 timeout、未知 provider | 全部报错 |
| UT-016 | `provider` 校验 | `TestLoadProvider` | 已知 provider、空 provider | 已知/空通过（已知归一化） |
| UT-017 | fallback 默认/显式/非法 | `TestLoadAppliesDefaults`、`TestFallbackExplicit`、`TestFallbackIdleZero`、`TestFallbackInvalid` | 缺省 3/5m/30/30s；显式值生效；显式 `min_tokens_per_second=0` / `idle_timeout=0s` 保留；阈值/时长/负吞吐/负载空闲超时/未知字段非法 | 默认与显式正确，非法值报错 |
| UT-018 | 分组实例解析/迁移 | `TestInstancesMarshalGrouped`、`TestLoadRejectsLegacyInstancesArray`、`TestLoadRejectsInstanceProtocolField` | 分组 flat 展开、Marshal 分组且无 protocol；旧数组提示「分组结构」；实例含 `protocol` 报错 | 分组结构正确，迁移报错 |
| UT-019 | `role_names` 校验与往返 | `TestRoleNamesConfig`、`TestRoleNamesConfigRejectsInvalid` | 键小写/trim、值 trim；marshal 往返；非法角色键、`*`、空值 | 合法归一化并通过；非法报错 |
| UT-020 | `model_capabilities` 校验与往返 | `TestModelCapabilitiesConfig`、`TestModelCapabilitiesConfigRejectsInvalid`、`TestModelCapabilitiesAllowedOnOpenAIInstance` | 键为上游模型名并 trim；窗口与四布尔解析（含 false）；0 合法；负数/空键报错；openai 实例允许 | 合法通过且往返一致；非法报错 |
| UT-012 | 示例配置可解析 | `TestExampleConfigIsValid` | `config.example.json`（fallback + 分组） | Parse 成功且实例非空 |
| UT-013 | 环境变量覆盖 | `TestEnvOverridesConfig` | 四个 `MODEL_ROUTER_*`（含空格）覆盖文件值 | 生效值为 env（trim 后），instances/fallback 不变 |
| UT-014 | 空 env 沿用文件 | `TestEnvEmptyKeepsFileValues` | 变量显式置空 | 使用文件值 |
| UT-015 | env 满足 auth_token | `TestEnvAuthTokenSatisfiesRequired` | 文件缺 auth_token，无 env / 有 env | 无 env 报错；有 env 通过 |

## 3. 代理路由与认证（`internal/proxy`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-101 | OpenAI 透传 | `TestOpenAIPassThroughVerbatim` | method/path/query/header/body；上游 `Authorization: Bearer <api_key>` | 请求与响应逐字节透传 |
| UT-102 | Anthropic 透传 | `TestAnthropicPassThroughVerbatim` | 客户端 `x-api-key`；客户端 Authorization 被剔除；上游 `x-api-key=<api_key>`；`anthropic-version`/`anthropic-beta` 透传 | 请求与响应逐字节透传 |
| UT-103 | 缺客户端 token | `TestMissingAuthorizationReturns401` | 两入口 | 401 按协议错误体，上游 0 调用 |
| UT-104 | 错 token | `TestWrongTokenReturns401` | 两入口 | 401 按协议错误体 |
| UT-105 | Anthropic 拒绝 Bearer | `TestAnthropicRejectsAuthorizationBearer` | 仅 Authorization | 401 `authentication_error` |
| UT-106 | OpenAI 拒绝 x-api-key | `TestOpenAIRejectsXAPIKeyOnly` | 仅 x-api-key | 401 `missing_api_key` |
| UT-107 | 未知路径 | `TestUnknownPathNotForwarded` | `/v1/unknown` + 合法 Bearer | 404 `not_found`，不转发 |
| UT-108 | 精确模型路由（R4a/R5） | `TestRouteByExactModel` | 命中第一/第二/未命中 | 正确实例被调用；body 不改写 |
| UT-109 | 回退首条目（R5/R6） | `TestRouteFallsBackToFirst` | 无 model/数字/null/非法 JSON/顶层数组 | 首条目收到逐字节原始 body |
| UT-110 | 角色路由（R4c） | `TestRouteByRoles` | 角色名、`claude-<role>-` 前缀；R4a 优先（同实例）；同角色取首个实例；未处理值走 R5 | 命中实例正确、R4c 改写、R4a/R5 不改写 |
| UT-111 | `*` 托底（R4d） | `TestRouteWildcardFallback` | 未知模型托底、具体角色优先、精确模型优先、多个 `*` 取首个、无/非字符串不触发 `*` | 托底改写；其余 R5 且 body 不变 |
| UT-112 | 协议隔离 | `TestProtocolListsAreIndependent` | 两协议实例交叉模型名 | 只落到同协议实例，R5 不改写 |
| UT-113 | 无对应协议实例 | `TestNoBackendForProtocol` | 两入口缺实例 | 502 按入口协议 |
| UT-114 | 请求体超限 | `TestRequestTooLarge` | 10 MiB + 1 | 413 按协议，不转发 |
| UT-115 | 上游失败 | `TestUpstreamFailureErrorFormat` | 上游已关闭 | 502 按协议 |

## 4. 模型列表入口（`internal/proxy`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-121 | OpenAI 格式 | `TestModelsOpenAI` | 跨 openai 实例 models 顺序/去重；排除 anthropic；`object/owned_by/created`；Anthropic 专用查询参数被忽略 | 列表正确，上游 0 调用；带非法查询参数仍 200 |
| UT-122 | Anthropic 官方 schema | `TestModelsAnthropic` | `/anthropic/v1/models`：4 条官方 id（sonnet/opus/fable/haiku 固定顺序、不含 `*`）；`display_name`/`created_at` 内置表；`line`/`lifecycle`；`max_input_tokens`/`max_tokens`/`capabilities`/`deprecated_at`/`retires_at` 未声明为 `null`；`first_id`/`last_id`/`has_more` | id 为官方名，角色名与上游名不出现 |
| UT-123 | 认证与方法 | `TestModelsAuthAndMethod` | 两入口：无/错 Bearer、错 x-api-key、POST | 401 按路径协议；405 + `Allow: GET`，错误体按路径协议 |
| UT-124 | 能力元数据 | `TestModelsAnthropicCapabilities` | 首个声明生效、首个实例未声明时继续向后、未声明 `null`、`0`→`null`、布尔 `false` 输出、未声明子项省略 | 字段与取值符合 §21.5 |
| UT-125 | 模型名映射 | `TestModelsAnthropicRoleNamesFromConfig` | `role_names` 覆盖官方名；自定义 id 的 `display_name`/`created_at` 回退；内置 id 跟随内置表；配置键归一化 | 官方 schema 下 id/display/created 正确 |
| UT-126 | 分页与 lifecycle | `TestModelsAnthropicPagination` | `limit`、`after_id`、`before_id`、未知/边界游标、`lifecycle` 重复/逗号/过滤、空页 `first_id`/`last_id` | 分页与 `has_more` 正确，空页为 null |
| UT-127 | 非法查询 | `TestModelsAnthropicInvalidQuery` | `limit` 非数字/0/负数/>1000、非法/空 `lifecycle`、双游标 | 400 `invalid_request_error`（Anthropic 格式） |
| UT-128 | 路径决定格式（M7） | `TestModelsSchemaIsDecidedByPath` | `/v1/models` 带 Bearer / 带 x-api-key 均为 OpenAI；`/anthropic/v1/models` 带 x-api-key / Bearer 均为 Anthropic；两入口无凭据 401 按路径协议；`/anthropic/v1/chat/completions` → 404 | 认证头不切换格式；仅路径决定 schema |
| UT-129 | OpenAI 能力字段（M7） | `TestModelsOpenAICapabilities` | 首个声明实例取值；首个实例未声明时向后找；两实例同名去重且取首个；`context_window`/`max_input_tokens` 与 `max_output_tokens`/`max_tokens` 别名相等；`input_modalities` 的 image/pdf/两者/显式 false 组合；未声明模型只有基础四字段 | 字段与省略规则符合 §22.4 |
| UT-130 | messages 路径别名（M7） | `TestAnthropicMessagesPathAlias` | 同一 body 发 `/v1/messages` 与 `/anthropic/v1/messages`；x-api-key/Bearer 两种认证；上游路径与查询串、模型改写、上游认证头；两条日志行字段格式一致；无凭据 401 | 返回体一致、转发目标一致、日志字段一致 |

## 5. 管理面（`internal/proxy`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-131 | 管理面禁用 | `TestAdminDisabled` | admin_password 空，访问 `/admin*`、`/api/*`（含 probe/validate/providers） | 全部 404 |
| UT-132 | Basic Auth 与页面 | `TestAdminAuth` | 无/错凭据、正确凭据、GET 配置 | 401 + `WWW-Authenticate`；页面 200 HTML；配置含真实 key |
| UT-133 | PUT 校验与写回 | `TestAPIConfigPut` | 未认证/非法/合法 PUT | 401/400 不写；合法写回且不生效 |
| UT-134 | reload | `TestAPIReload` | PUT + reload；破坏文件后 reload | 新配置生效（旧 token 401/新 token 新模型）；失败保留旧配置 |
| UT-135 | validate | `TestValidateConfig` | 需鉴权；合法；旧 backends；非法实例 | 401；200 `{ok:true}`；400 `{ok:false,error 含 instances}` |
| UT-136 | probe/test | `TestProbeTest` | openai GET `/v1/models`；anthropic POST `/v1/messages`（最小 body、x-api-key/version/content-type）；anthropic 200/400/401/404 分支；非 2xx、网络错误、非法请求 | 200/400 → ok:true；401/404/网络错误 → ok:false + 对应提示；非法 → 400 |
| UT-137 | probe/models | `TestProbeModels` | 解析 `data[].id`；上游 404 友好文案；缺 data；需鉴权 | 200 `{models}`；404 → 502 + 「不支持模型列表查询…请手动输入」；缺 data → 502；401 |

## 6. 端到端（手工，主代理补）

- 可视化流程：添加实例 → 测试连接 → 查询模型 → 勾选/手填 → 保存 → 重载 → 用新模型路由验证。
- JSON 流程：修改 JSON → 校验（失败时保存禁用）→ 保存 → 重载生效。
- 配置迁移：`config.local.json` / `config.example.json` 迁移为 `instances` 后路由行为与迁移前一致。
- 无 `admin_password` 时 `/admin`、`/api/*` 404；Basic Auth 失败 401。
- `-version` 输出 `0.1.1`，产物位于 `dist/`。

## 7. 执行方式

```bash
go build ./...
go vet ./...
go test ./...
bash build.sh
```

## 8. 验收对照（§9.6）

| 验收项 | 对应用例 |
|---|---|
| `go build` / `go vet` / `go test` 退出码 0 | §7 |
| instances 解析与校验：互斥、必填、roles 键合法性、models 去空去重 | UT-001～UT-003、UT-011 |
| 旧 `backends` 字段报错（含迁移提示） | UT-010、UT-135 |
| 实例展开路由：精确/角色/托底/回退/502 | UT-108～UT-113 |
| `/api/probe/test`（httptest mock 上游） | UT-136 |
| `/api/probe/models`（httptest mock 上游） | UT-137 |
| `/api/config/validate`（通过/失败） | UT-135 |
| 模型列表双路径、双协议与过滤去重 | UT-121～UT-130 |
| 管理鉴权、PUT 写回不生效、reload 成功/失败 | UT-131～UT-134 |
| `config.example.json` 为 fallback + 分组 instances 且可解析 | UT-012 |
| 文档同步（README/CHANGELOG/design/tests） | 本文件与 design.md、README.md、CHANGELOG.md |
| `-version` 仍 `0.0.1-rc`，产物在 `dist/` | 手工验证（`bash build.sh` + `dist/... -version`） |

## 9. 供应商预设与 provider（§10）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-301 | 预设清单完整性 | `TestListContainsBuiltinProviders` | 11 个供应商、id 唯一、name 非空、base_url 为绝对 URL、override 协议/auth 合法 | 全部通过 |
| UT-302 | `Get(id)` | `TestGet` | 已知 deepseek、未知 id | 命中返回名称；未知 miss |
| UT-303 | DeepSeek override 数据 | `TestDeepSeekModelsOverride` | deepseek+anthropic override；deepseek+openai 无；openai+anthropic 无 | base_url=`https://api.deepseek.com`、auth=`bearer` |
| UT-304 | `ModelsSource` 解析 | `TestModelsSource` | deepseek anthropic 命中 override；deepseek openai 回退；空/未知 provider 回退 | 返回 override 或 fallback 的 base_url/auth |
| UT-305 | 清单返回副本 | `TestListReturnsCopies` | 修改 List() 返回的 map | 不影响后续查询，List 稳定 |
| UT-138 | `GET /api/providers` | `TestProvidersAPI` | 鉴权、清单、非 GET | 401；200 返回 11 项（含 deepseek override）；POST 405 |
| UT-139 | probe/models provider | `TestProbeModelsWithProvider` | openai+provider=deepseek（无该协议 override）走请求 base_url；未知 provider | 走 mock base_url 并返回模型；未知 provider 400 |
| UT-140 | 配置 provider 透传 | `TestSaveRoundTrip` / `TestLoadAppliesDefaults` | provider 的归一化、保存往返 | provider 保留为小写 id |
| UT-141 | probe/models 能力字段兼容 | `TestProbeModelsCapabilities` | DeepSeek 真实响应夹具（`context_window`/`max_output_tokens`/`input_modalities`/`effort`）；其余别名（`context_length`/`contextWindow`/`maxTokens`/`maxOutputTokens`/`max_completion_tokens`/`top_provider`/`limit`/`reasoning`）；Anthropic 官方字段与别名并存；MTI 无元数据条目 | flash：窗口 1048576、输出 393216、`image_input:true`、`thinking:true`；pro：`input_modalities` 仅 text → `image_input:false`；官方字段优先；无元数据时省略 `capabilities` 且 `models` 正常 |

## 10. 验收对照（§10.7）

| 验收项 | 对应用例 |
|---|---|
| `go build` / `go vet` / `go test` 退出码 0 | §7 |
| providers 清单完整性（11 个 + id 唯一） | UT-301 |
| `Instance.provider` 合法/非法/空 | UT-016、UT-011（unknown_provider） |
| probe/models 的 override 分支（deepseek anthropic → openai base_url + bearer） | UT-303、UT-304（真实网络由主代理 e2e 验证） |
| 无 override 时走原逻辑 | UT-139、UT-137 |
| `GET /api/providers` 返回清单 | UT-138 |
| UI 选择框 + 自动填 base_url + 自定义 | 手工 e2e（主代理） |
| 文档同步 / `-version` 0.0.1-rc | 本文件与 design.md、README、CHANGELOG；手工验证 |

## 11. 回退降级（M4，§11）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-401 | OpenAI 候选链 | `TestCandidateChainOpenAI` | 精确模型原样转发到后续候选；无该模型的候选改写为 `models[0]`；基础实例之前的实例被排除 | 转发体/改写正确 |
| UT-402 | Anthropic 候选链 | `TestCandidateChainAnthropic` | 角色逐候选改写；缺角色时用 `*`；既无角色又无 `*` 的实例跳过；角色值精确命中不改写 | 命中实例与改写正确 |
| UT-403 | R5 无 `*` 兜底 | `TestFallbackChainR5WithoutWildcard` | 无 model、实例无 `*` | 仍尝试首实例且正文不改写 |
| UT-404 | 临时回退与持续回退 | `TestFallbackTemporaryAndSustained` | 失败 1/2 次仍先试主实例；第 3 次后进入 5 分钟回退并跳过 | 调用次数符合状态机 |
| UT-405 | 回退期到期后真实请求重新试主实例 | `TestFallbackExpiredWindowRealRequestFails` | 回退期结束后主实例仍未恢复 | 真实请求重试主实例，失败后立即重新进入回退，后续请求跳过 |
| UT-406 | 成功恢复清零 | `TestFallbackSuccessResets` | 回退期后主实例成功 | 计数/回退清零，之后失败重新按 3 次累计 |
| UT-407 | 全降级时逐实例重试（§24.19 M45–M48 修订） | `TestFallbackAllOpenRetriesEveryInstance` | 阈值 1、两实例均失败后全部降级 | 后续请求按链序依次尝试两个实例（各 +1 次调用），全部失败返回 502 |
| UT-408 | 并发安全 | `TestFallbackConcurrent`（`go test -race`） | 20 并发请求 + 失败实例 | 全部 200，调用次数一致，无 data race |
| UT-409 | 吞吐判定与样本门槛 | `TestThroughputFailure` | OpenAI `completion_tokens`、Anthropic `output_tokens`、无 usage；输出 ≤100 或生成时长 ≤2s；慢响应 | 低吞吐响应仍透传；满足门槛才记失败（下次跳过），样本不足/无 usage 不判失败 |
| UT-413 | Anthropic 空闲超时收尾 | `TestIdleTimeoutAnthropicPauseTurn` | mock 上游发首块后静默；`idle_timeout=50ms` | 客户端收到 `content_block_stop`、`message_delta(stop_reason=pause_turn)`、`message_stop` |
| UT-414 | OpenAI 空闲超时截断 | `TestIdleTimeoutOpenAITruncates` | mock 上游发首块后静默 | 客户端读取报流截断错误、无 `[DONE]`；实例记失败（后续走备用实例） |
| UT-415 | OpenAI 慢速流不打断 | `TestSlowStreamIsNotInterrupted` | mock 持续慢速输出、低 usage | 流完整结束（含 `[DONE]`），仅记失败，下次请求跳过 |
| UT-416 | 配置 idle_timeout 生效 | `TestIdleTimeoutFromConfig` | JSON `fallback.idle_timeout="50ms"` + 静默 anthropic 上游 | 收到 pause_turn 收尾，usage 复用已见 output_tokens |
| UT-410 | 配置阈值生效 | `TestFallbackFromConfigThreshold` | `fallback.failure_threshold=2`（JSON 配置） | 连续失败 2 次后持续回退 |
| UT-411 | 配置回退时长生效 | `TestFallbackFromConfigDegradeDuration` | `degrade_duration="100ms"` | 回退期结束后由真实请求重试主实例 |
| UT-412 | 配置禁用吞吐判定 | `TestFallbackFromConfigDisableThroughput` | `min_tokens_per_second=0` + 慢响应/低 usage | 不记录吞吐失败，主实例持续使用 |

执行补充：

```bash
go test ./...
go test -race ./internal/proxy
```

## 12. 验收对照（§11.8）

| 验收项 | 对应用例 |
|---|---|
| `go build` / `go vet` / `go test` 退出码 0 | 实际执行；`go test -race ./internal/proxy` 通过 |
| 候选链：OpenAI 同名不改写 / 无同名用 `models[0]` 改写 | UT-401 |
| 候选链：Anthropic 同角色改写 / 无角色用 `*` / 皆无跳过 | UT-402 |
| R5 未命中候选链与不改写 | UT-403、`TestRouteWildcardFallback`（R5 子用例） |
| 状态机：临时回退、<3 仍试主实例、≥3 持续回退 | UT-404 |
| 状态机：回退期结束由真实请求重试并重新计时 | UT-405 |
| 状态机：成功恢复清零 | UT-406 |
| 状态机：全部降级时逐实例向后重试 | UT-407、UT-844～UT-848 |
| 并发安全（race） | UT-408 + `go test -race` |
| 吞吐判定（mock 慢响应/低 usage） | UT-409 |
| UI 拖动排序改变 instances 顺序 | 手工 e2e（主代理） |
| README/CHANGELOG/design/tests 同步；`-version` 0.0.1-rc | 手工验证 |

## 13. 验收对照（§12.6 M4-config）

| 验收项 | 对应用例 |
|---|---|
| `go build` / `go vet` / `go test`（含 `-race`）退出码 0 | §7；实际执行 `go test -race ./internal/proxy` 通过 |
| `fallback` 缺省 3/5m/30/30s | UT-001、UT-017 |
| `fallback` 显式配置生效（阈值/回退时长/吞吐阈值） | UT-017、UT-410、UT-411、UT-412 |
| `min_tokens_per_second=0` 禁用吞吐判定 | UT-017、UT-412 |
| fallback 非法值启动失败 | UT-017（`TestFallbackInvalid`） |
| 分组 instances 解析/展开正确 | UT-002、UT-018；路由回归 UT-101～UT-115 |
| 两组合计 0 实例失败 | UT-007 |
| 实例含 `protocol` 字段报错 | UT-018（`TestLoadRejectsInstanceProtocolField`） |
| 旧数组形态报错并提示迁移 | UT-018（`TestLoadRejectsLegacyInstancesArray`）、UT-135（validate 子用例） |
| 回退/候选链/每级独立判断行为不变 | UT-401～UT-412（M4 回归） |
| `config.example.json` 新结构可加载 | UT-012 |
| 文档同步 / `-version` 0.0.1-rc | 本文件与 README/CHANGELOG/design；手工验证 |
| UI 分组读取与组内拖动排序 | 手工 e2e（主代理） |

## 14. 可视化回退参数（M4-fallback-ui / M4.1，§12.4）

### 14.1 手工端到端用例（主代理执行）

| 编号 | 用例 | 步骤 | 预期 |
|---|---|---|---|
| ET-1401 | 加载回填 | 打开 `/admin` 可视化模式 | 顶部「回退参数」显示四项；配置文件缺 fallback 时显示 3 / 5m / 30 / 30s；显式 `min_tokens_per_second=0` / `idle_timeout="0s"` 时显示 0 / 0s |
| ET-1402 | 保存生效 | 修改四项后点「保存到文件」 | 先通过 `/api/config/validate`，再 `PUT /api/config`；`GET /api/config` 的 `fallback` 与界面一致 |
| ET-1403 | 非法值阻止保存 | 阈值填 0、时长填 `abc`、吞吐填 -1、空闲超时填 `-5s` | 就地显示错误，不发起 PUT；修正后可保存 |
| ET-1404 | 留空字段 | 清空四项中某一项后保存 | 该项不写入配置，后端使用默认值 |
| ET-1405 | 重载生效 | 保存后点「重载生效」 | 新 fallback 生效（例如改小 `failure_threshold` 更快进入持续回退） |
| ET-1406 | 功能回归 | 检查实例分组读取、组内拖动排序、provider 选择、probe、JSON 高级模式 | 行为不变 |

### 14.2 验收对照（§12.4 可视化部分）

| 验收项 | 对应 |
|---|---|
| 四个输入框与默认值回填（含显式 0 与缺省区别） | ET-1401 |
| 保存后 `fallback` 与界面一致 | ET-1402 |
| 非法值阻止保存并提示（含空闲超时） | ET-1403 |
| reload 后新参数生效 | ET-1405 |
| 实例分组/拖动/JSON 模式回归 | ET-1406 |
| 前端脚本语法 | `node --check`（提取内嵌脚本）通过 |

## 15. 验收对照（§13.8 M4.1）

| 验收项 | 对应用例 |
|---|---|
| `go build` / `go vet` / `go test`（含 `-race`）退出码 0 | 实际执行：全 0，`go test -race ./...` 通过 |
| `fallback.idle_timeout` 缺省 30s | UT-001、UT-017 |
| `fallback.idle_timeout` 显式生效 | UT-017、UT-416 |
| `fallback.idle_timeout=0` 禁用 | UT-017（`TestFallbackIdleZero`） |
| `fallback.idle_timeout` 非法（负数/非法时长）报错 | UT-017（`TestFallbackInvalid`） |
| Anthropic 静默流补发 pause_turn 收尾 | UT-413、UT-416 |
| OpenAI 静默流连接截断（无 `[DONE]`） | UT-414 |
| OpenAI 慢速流不中断、完整结束、仅记失败 | UT-415 |
| 首字节等待仍由实例 timeout 控制 | 代码审查（`client.Do` + transport 响应头超时）；非流式超时测试 UT-114/115 保持通过 |
| 页面「空闲超时」输入框（回填/保存/0 禁用/非法阻止） | UT-1401～1406 扩展 + 手工 |
| 文档同步 / `-version` 0.0.1-rc | README/CHANGELOG/design/tests；`bash build.sh` + `-version` |

## 16. 跨平台构建与 Docker（M5，§14）

### 16.1 验证项

| 编号 | 验证项 | 命令 / 方法 | 预期 |
|---|---|---|---|
| ET-1601 | Workflow YAML 语法 | `python -c "import yaml; yaml.safe_load(open('.github/workflows/build.yml'))"` | 解析成功，jobs = build/release/docker |
| ET-1602 | Workflow 内容 | 人工检查 matrix/trigger/permissions | 6 组合（windows/linux/darwin × amd64/arm64）；push+PR+tag；tag release；docker 双架构推 GHCR |
| ET-1603 | 本地全平台构建 | `bash build-all.sh` | `dist/` 下 6 个 `model-router-0.0.1-rc-<os>-<arch>[.exe]`，退出码 0 |
| ET-1604 | Compose 语法 | `docker compose config --quiet` | 退出码 0 |
| ET-1605 | Docker 镜像构建 | `docker build -t wuzm219/model-router:m5-test .` | 成功（golang:1.27-alpine → distroless nonroot） |
| ET-1606 | 容器运行与 /v1/models | 挂载测试配置运行容器，`curl /v1/models -H "Authorization: Bearer <token>"` | 200 + 本地生成的模型列表；无 token → 401；容器 `User=nonroot:nonroot` |
| ET-1607 | 版本 | 构建产物 `-version` | `0.0.1-rc` |
| ET-1608 | actionlint（可选） | `actionlint` | 若本机存在则通过；本次环境未安装，以 PyYAML 解析为准 |

### 16.2 验收对照（§14.7）

| 验收项 | 对应 |
|---|---|
| Workflow YAML 可解析（actionlint 如有） | ET-1601、ET-1608 |
| matrix 6 组合 + tag release + 多架构 Docker | ET-1602 |
| Dockerfile 本地 build 成功 | ET-1605 |
| 容器 /v1/models 带 token 返回 200 | ET-1606 |
| docker compose config 通过 | ET-1604 |
| build-all.sh 生成 6 个产物 | ET-1603 |
| 文档同步 / `-version` 0.0.1-rc | ET-1607；README/CHANGELOG/design/tests 已更新 |

## 17. 请求日志：客户端与来源 IP（M4.3，§16）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-501 | 日志字段与顺序 | `TestRequestLogFormat` | `req_model="opus[1m]"` / `model="deepseek-v4-pro"` / `(anthropic)` / `client=claude-code/2.1.323` / `ip=203.0.113.9` / `total` / `ttft` / `in=31` / `out=300` / `gen=n/a` | 字段齐全且顺序固定（`req_model` → `model` → 协议 → `client` → `ip` → `total` → `ttft` → `in` → `out` → `gen`） |
| UT-502 | 客户端识别十一级规则 | `TestClientIdentification` | `claude-cli/2.1.323 ...`、仅 `X-Claude-Code-Session-Id`、`anthropic-beta` 含 `claude-code`、`OpenAI/Python`、`OpenAI/NodeJS`、`Anthropic/JS`、`Anthropic/Python`、`dsh`、`curl/`、其他 UA 截断、无 UA | 分别得到 `claude-code/2.1.323`、`claude-code`（×2）、`openai-python`、`openai-node`、`anthropic-js`、`anthropic-python`、`dsh`、`curl`、UA 前缀、`unknown` |
| UT-503 | 来源 IP | `TestClientIP` | `X-Forwarded-For: 203.0.113.9, 10.0.0.1`；`X-Forwarded-For: unknown`；无 XFF | 取首段；回退 `RemoteAddr` 去端口（`127.0.0.1`） |
| UT-504 | 401 也打日志且不含密钥 | `TestAuthFailureIsLoggedWithoutSecrets` | 无凭据请求 + UA + XFF | 401；日志含 `req_model="n/a"`、`model="n/a"`、`client`、`ip`，不含 service token / api_key |
| ET-1701 | 端到端客户端识别（手工） | `curl -A "claude-cli/2.1.323 (external, cli)"` 打代理 | 日志出现 `client=claude-code/2.1.323`；仅带 `X-Claude-Code-Session-Id` 时出现 `client=claude-code` |
| ET-1702 | 容器来源 IP 限制（手工） | docker bridge 下从宿主机调用 | `ip` 为网关地址（如 `172.18.0.1`）；仅 `network_mode: host` 或反代透传 `X-Forwarded-For` 才能拿到真实 IP（README 已写明） |

## 18. M4.4 验收对照（§17）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-505 | 双认证（Anthropic） | `TestAnthropicDoubleAuth` | 仅 `x-api-key`；仅 `Authorization: Bearer`；小写 `bearer`；无凭据；有头但错（单独/同时） | 前三种 200 且上游用实例 key；无凭据 401 `missing x-api-key or Authorization header`；有头但错 401 `invalid service access token` |
| UT-506 | 双认证不外传 | `TestAnthropicDoubleAuth`（Bearer 子用例） | 客户端 `Authorization` 与上游头 | 上游 `x-api-key` 为实例 key，客户端 `Authorization` 不出现在上游请求 |
| UT-105 | OpenAI 仍只认 Bearer | `TestOpenAIRejectsXAPIKeyOnly` | 仅 `x-api-key` | 401 `missing_api_key`（OpenAI 认证未改） |
| UT-507 | `[1m]` 变体命中角色 | `TestRouteByRoles`（`opus[1m]`/`sonnet[1m]`/`opus[1M]`/`claude-opus-4-5[1m]`/`claude-sonnet-4-5-20250929[1m]` 子用例） | 剥除尾部 `[1m]`（大小写不敏感）后按角色/前缀命中 | 正确落到 `opus`/`sonnet` 实例并改写为 `roles[role]` |
| UT-508 | `[1m]` 非变体不误判 | `TestRouteWildcardFallback`（`opus[1x]`、`opus [1m]` 子用例） | 非法/带空格标记 | 按未命中处理（`*` 托底 / R5），不误命中角色 |
| UT-509 | 双认证回归 | `TestMissingAuthorizationReturns401`、`TestWrongTokenReturns401` | 两入口 | 401 按协议错误体，文案与 §17.2 一致 |
| UT-409 | 吞吐样本门槛 | `TestThroughputFailure` | 输出 ≤100（OpenAI `completion_tokens=1` / Anthropic `output_tokens=1`）、无 usage、慢响应 | 样本不足或无 usage → 不记失败、实例仍被使用（慢后端调用 2 次）；达标且低于阈值 → 记失败，响应已提交不受影响 |
| UT-415 | 低速流不打断 | `TestSlowStreamIsNotInterrupted` | 低 usage 慢速 SSE | 流完整结束（含 `[DONE]`），仅记失败 |
| UT-502～504 | 日志 `req_model`/`client`/`ip` | 见 §17 | 同 §16.4/§17.6 | 字段齐全、顺序固定、401 行不含密钥 |
| ET-1703 | 容器时区（手工） | `docker build` + `docker run` 后 `docker logs` | 日志时间戳 | 为北京时间（UTC+8），容器内 `TZ=Asia/Shanghai` |
| ET-1704 | `gen` 数值分支（手工） | 长输出（>100 token 且 >2s）请求 | 日志 `gen=` | 显示数值（如 `142 t/s`）；短请求显示 `gen=n/a` |
| ET-1705 | `go build` / `go vet` / `go test`（含 `-race`） | 命令行 | 全量 | 退出码 0 |
| ET-1706 | 文档同步 / 版本 | README/CHANGELOG/design/tests；`-version` | 同步与版本号 | 四文档含 §16/§17 内容；`-version` 仍 `0.0.1-rc` |

执行：

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./internal/proxy
```

## 19. gzip 压缩响应体（M4.6，§20）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-601 | 客户端 gzip 不决定上游编码 | `TestGzipStreamingUsageParsed`（`client requests gzip` / `client requests identity`） | 客户端 `Accept-Encoding` 是否透传；上游看到的值；流式 usage | 上游收到 Transport 协商的 gzip；客户端收到明文 SSE；日志 `↑31 ↓300` |
| UT-602 | 上游强制 gzip（流式） | `TestForcedGzipUpstreamStreaming` | `Range` 关闭 Transport 协商，走代理自身 `gzip.Reader` 分支；响应头 | 客户端收到明文；无 `Content-Encoding` / `Content-Length`；日志 `↑31 ↓300` |
| UT-603 | 上游强制 gzip（非流式） | `TestForcedGzipUpstreamNonStreaming` | `capture` + `parseUsage` 位于解压之后 | 客户端 body 为明文 JSON；日志 `↑31 ↓300` |
| UT-604 | gzip 损坏 | `TestForcedGzipUpstreamCorruptReturns502`（流式/非流式） | 解压失败（立即与首个数据块读取时） | 502 且 `recordFailure` |
| UT-605 | §19 判定与 gzip 无关 | `TestRealtimeSlowVerdictUnaffectedByGzip`（客户端带/不带 gzip，并行） | 明文速率达标、压缩后字节速率远低于阈值 | 两用例均无 `pause_turn`（窗口按解压后字节计算） |
| ET-1801 | 真实 DeepSeek Anthropic 入口（手工） | `curl -H "Accept-Encoding: gzip"` 流式请求 | 生产上游会压缩 | 客户端响应完整可解析；日志 `↑<in> ↓<out>` 有值 |

执行：

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./internal/proxy
```

## 20. M6 `/v1/models` 官方 schema（§21）

### 20.1 自动化用例

- 配置：UT-019（`role_names`）、UT-020（`model_capabilities`）。
- 端点：UT-122（官方 schema 与未声明 `null`）、UT-124（能力元数据取值规则）、UT-125（`role_names` 映射）、UT-126（分页/lifecycle）、UT-127（400 错误体）。
- 路由不变：`TestRouteByRoles` 覆盖 `claude-sonnet-5`；`TestOfficialModelNameRoutesLikeRoleAndLogsRequestModel` 断言 `req_model` 分别为 `sonnet`/`claude-sonnet-5`，转发目标与改写值一致。
- OpenAI 兼容：UT-121 断言 Anthropic 查询参数不影响 OpenAI 响应。

### 20.2 验收对照（§21.9）

| 验收项 | 对应用例 |
|---|---|
| 4 个官方 id + `line`/`lifecycle`/窗口/能力/`deprecated_at`/`retires_at` | UT-122 |
| 配置能力后字段出现；未配置为 `null` | UT-122、UT-124、UT-125 |
| `?limit=2`、`?after_id=`、`?lifecycle=retired` | UT-126 |
| `claude-sonnet-5` 与 `sonnet` 路由一致、日志 `req_model` 不同 | `TestRouteByRoles`、`TestOfficialModelNameRoutesLikeRoleAndLogsRequestModel` |
| 非法 `limit`/`lifecycle`/双游标 → 400 | UT-127 |
| `go build`/`go vet`/`go test`（含 `-race`）退出码 0 | §7 执行命令 |
| DSH 发图链路（真实环境） | 手工：DSH 选 `claude-sonnet-5` 发图，不再提示未知图片能力，代理日志 `↑` 输入 token 增大 |

## 21. M6 配置页：`role_names` / `model_capabilities`（§21.6）

### 21.1 自动化用例

- `TestProbeModelsCapabilities`（`internal/proxy`）：
  - 上游返回 `max_input_tokens` / `max_tokens` / `capabilities.*.supported` → 响应保留原样 `models`，并新增 `capabilities` 映射（显式 `false` 保留；`0`/负数与无元数据模型不输出）。
  - 上游没有任何能力元数据 → 响应省略 `capabilities` 字段，`models` 形状不变（向后兼容）。
  - 能力元数据类型异常 → 仅忽略能力映射，`models` 仍正常返回（best-effort）。
  - OpenAI/DeepSeek 风格字段：`context_window` / `max_output_tokens` / `input_modalities` / `effort` 等自动带出窗口与模态；`input_modalities` 不含某模态时为显式 `false` 而非未声明；Anthropic 官方字段与别名并存时官方字段优先；仅 `id`/`object` 的无元数据模型（MTI）不产生能力条目，`capabilities` 省略、`models` 正常返回。
- 配置往返：UT-019（`role_names`）、UT-020（`model_capabilities`）覆盖 `config.Parse` / `Marshal` 与 `PUT /api/config` 写回。
- 执行：`go test -race ./internal/proxy -run TestProbeModelsCapabilities`。

### 21.2 手工端到端用例（主代理执行）

准备：管理员开启配置页；一个 Anthropic 实例至少配置 `sonnet`/`opus` 角色；上游 `/v1/models` 至少返回一个带能力元数据的模型。

1. 打开 `/admin` 并切到 Anthropic tab：每个具体角色一行，左侧上游模型、右侧「对外模型名」；对外名输入框 placeholder 为内置官方名（`claude-sonnet-5` 等），`*` 托底行没有对外名。
2. 给 sonnet 填对外名 `my-sonnet`，其余角色留空；保存到文件 →「重载生效」→「重新读取」：sonnet 回显 `my-sonnet`，未填写对外名的角色不出现在 `role_names`。
3. 「模型能力」区块只列出角色映射使用到的上游模型（另加已有 `model_capabilities` 键），行内为横向紧凑布局（模型名 + 两个窗口 + 四个勾选），不出现纵向大块堆叠。
4. 填写 `max_input_tokens` / `max_tokens` 并勾选部分能力，保存/重载/重新读取后回显一致；数值留空或填 `0` 视为未声明，不写入。
5. 点击「查询模型」：
   - 上游返回能力元数据时，已被角色使用的模型自动填入缺失的窗口与勾选（`supported:true` 勾选，`supported:false` 不勾选但保留显式 `false`）；
   - 用户已填字段不被覆盖；上游未声明的模型或手填模型保持空行。
6. 能力行全部留空（无已声明字段）→ 保存后 `model_capabilities` 不含该模型；若行内存在显式 `false` 则保留（D-M6-6）。
7. 新增实例、删除实例、切换 OpenAI/Anthropic tab、可视化 ↔ JSON 往返后保存：`role_names` 与 `model_capabilities` 不丢失。
8. 窗口填负数 → 保存被前端阻止并显示错误；`0` 合法（视为未声明）。
9. 视觉检查：1 个实例（4 个具体角色 + 3 条能力行）卡片高度约 600px，与新增前同一量级；能力行为一行多列的紧凑网格。

### 21.3 验收对照（§21.6）

| 验收项 | 对应验证 |
|---|---|
| 角色行可编辑对外模型名；留空用内置默认；保存写 `role_names` | 手工用例 1–2；UT-019 |
| 每实例可编辑模型能力（窗口/输出上限/四勾选）；空值不写 | 手工用例 3–4、6；UT-020；`TestProbeModelsCapabilities` |
| 查询上游把能力自动填入对应行；未提供/手填留空 | 手工用例 5；`TestProbeModelsCapabilities` |
| 新增/删除/切换/保存后回读不丢失（含 `PUT /api/config` 往返） | 手工用例 7；UT-019、UT-020 |
| `probeModelsResponse` 保留 `models` 并新增能力映射 | `TestProbeModelsCapabilities`（八个子用例） |
| 布局紧凑、不产生过高卡片 | 手工用例 3、9 |
| `go build` / `go vet` / `go test`（含 `-race`）退出码 0 | §7 执行命令 |

## 22. M7 协议路径分离 + OpenAI 入口能力（§22）

### 22.1 自动化用例

- UT-128（`TestModelsSchemaIsDecidedByPath`）：`/v1/models` 无论 Bearer 还是 `x-api-key` 都返回 OpenAI 格式；`/anthropic/v1/models` 无论 `x-api-key` 还是 Bearer 都返回 Anthropic 格式；两入口无凭据 401 的错误体按路径协议；`/anthropic/v1/chat/completions` 精确匹配失败 → 404。
- UT-129（`TestModelsOpenAICapabilities`）：OpenAI 实例能力字段的构造、别名、去重与“首个声明”回退；`input_modalities` 四种组合（image-only / pdf-only / both / 显式 false）。
- UT-130（`TestAnthropicMessagesPathAlias`）：两条 messages 路径 × 两种认证头，响应体、上游路径/查询串/认证头、模型改写与日志字段一致；无凭据 401。
- 既有用例按新契约迁移：UT-123 的 Anthropic 分支改走 `/anthropic/v1/models`；UT-122/124～127 全部改走 `/anthropic/v1/models`。
- 配置：UT-020 已覆盖 OpenAI 实例的 `model_capabilities` 解析与校验，无需新增。

### 22.2 手工端到端用例

准备：管理员开启配置页；一个 OpenAI 实例勾选至少两个模型，其中一个配置能力；同时存在 Anthropic 实例。

1. OpenAI tab 的实例卡片在「模型列表」下方出现「模型能力」网格（模型名 + 输入窗口 + 输出上限 + 图片/PDF/思考/结构化勾选），与 Anthropic 侧同一紧凑样式；卡片不出现整块纵向堆叠。
2. 勾选/取消模型、手动修改 `models` 输入框：能力网格行随之增删；已配置能力的模型回显窗口与勾选。
3. 填写窗口/勾选能力 → 保存 → 「重载生效」→「重新读取」：OpenAI 实例的 `model_capabilities` 回显一致；`GET /api/config` 与落盘文件均含该字段。
4. 点击「查询模型」：上游返回能力元数据时，已选模型自动填入缺失字段，用户已填值不被覆盖；勾选新模型时其远程能力立即出现。
5. 能力行全部留空 → 保存后该模型不出现在 `model_capabilities`。
6. 用 `curl` 验证：`/v1/models` 带 Bearer 或 `x-api-key` 均返回 OpenAI 格式且带能力字段；`/anthropic/v1/models` 无论带哪种头均返回 Anthropic 格式；`/anthropic/v1/messages` 与 `/v1/messages` 对同一请求体结果一致。

### 22.3 验收对照（§22.8）

| 验收项 | 对应验证 |
|---|---|
| `/v1/models` 带 Bearer 返回 OpenAI 格式且含能力字段 | UT-121、UT-129 |
| `/v1/models` 带 `x-api-key` 仍返回 OpenAI 格式 | UT-128 |
| `/anthropic/v1/models` 返回 Anthropic 格式（即使带 Bearer） | UT-122、UT-128 |
| `/anthropic/v1/messages` 与 `/v1/messages` 结果与日志一致 | UT-130 |
| 配置能力后出现字段；未配置模型不出现 | UT-129 |
| 两实例同名模型只出现一次且取第一个实例能力 | UT-129 |
| `input_modalities` 构造（image/pdf/两者 false 或未声明） | UT-129 |
| 配置页 OpenAI 能力编辑与保存 | 手工用例 1–5；UT-020 |
| `go build` / `go vet` / `go test`（含 `-race`）退出码 0 | §7 执行命令 |

| UT-131 | 非 AI 请求与三档请求日志（M8，§23） | `TestNonAICallOmitsTokenSegment`、`TestAuthFailureIsLoggedWithoutSecrets`、`TestAnthropicMessagesPathAlias`、`TestAllCandidatesFailedLogsAttempt`、`TestRequestLogFormat` | favicon 与 401 走代理；已路由但无 usage 的 messages 请求；上游候选全部失败；有 usage 的正常请求 | D1：无模型段与 token 段；D2：保留模型段、无 `↑`/`↓`；D3：完整格式与顺序不变；候选全失败时 502 汇总行之前有一行带最后候选实例名/上游模型名的 D2 日志（R3） |

## 23. M9 实时请求监控（§24）

### 23.1 自动化用例

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-701 | 无客户端不采集/不泄露 | `TestMonitorHubNoClientNoCollection`、`TestMonitorNoHistoryReplay` | 无 WS 客户端时 `newRequest` 返回 nil、广播为空操作；请求完成后才连接 | 不创建 tracker；连接后 300ms 内收不到任何历史事件；之后的实时事件正常 |
| UT-702 | WS 认证 | `TestMonitorAuth` | `admin_password` 为空；无凭据；query 密码错误；query 正确；Basic header 正确 | 404；401；401；101；101；失败均不升级 |
| UT-703 | 流式事件序列与 trunk 计数 | `TestMonitorStreamEvents` | 上游 4 次 SSE 写入（末次含 usage 与结束事件）；`/v1/messages` 流式请求 | 首事件 `start`、末事件 `end`；`chunk` 4 条且 `seq` 0–3；`bytes > 0`；`tsEnd ≥ tsStart`；Δtoken 合计 6；`end.chunks=4`、`inTokens=11`、`outTokens=6`、`status=200`、无 `error` |
| UT-704 | 非 AI 请求零事件 | `TestMonitorNonAIRequestsSilent` | favicon（已认证但未知路径）、401 的 AI 请求、正常 AI 请求 | 只有 1 条 `start`（路径为 AI 请求）；所有事件 `id` 一致，非 AI 调用不产生事件 |
| UT-705 | 候选全失败：retry 顺序与 502 | `TestMonitorRetryThen502` | 3 个不可达候选（openai） | `retry` 2 条，`instName` 依次为 B、C，`reason=error`；无 `chunk`；`end.status=502`、`end.error` 非空、`end.instName=C` |
| UT-706 | 慢客户端断开 | `TestMonitorSlowClientDisconnected` | `net.Pipe` 对端不读；连续入队超过 1024 条 | 客户端 `done` 关闭、从 hub 移除；后续广播不 panic/不阻塞 |
| UT-707 | reload 后监控继续可用 | `TestMonitorSurvivesReload` | 已连接 WS；PUT 新配置 + `POST /api/reload`；用新 token 发起请求 | 旧 WS 连接不中断；收到新请求的 `start`/`end`；新上游被调用 1 次；`end.status=200` |
| UT-708 | 首 token 超时 retry 原因 | `TestMonitorFirstTokenTimeoutRetry` | 首候选返回 SSE 头后静默；`fallback.first_token_timeout=50ms`；次候选正常 | `retry.reason=first_token_timeout`、`retry.instName=FAST`；`end.status=200`、`end.instName=FAST`；两后端各 1 次调用 |
| UT-709 | query 凭据优先于过期请求头 | `TestMonitorQueryCredentialFallback` | 同时携带错误的 `Authorization: Basic` 与正确的 `?user=&password=` | 握手 101（header 与 query 为“或”关系，前端缓存过期凭据时仍可连接） |

### 23.2 手工端到端用例

> 说明：以下第 1–9 条记录第一轮「左右分栏 + 选中详情」布局；第二轮（§24.5）已改为单表格 + 每行时间轴，布局类验证以 §24 为准。第 10–15 条已按第二轮「记住密码 → localStorage」更新。

1. 正常加载 `/admin` → 点击「实时监控」：状态从「连接中…」变为「已连接」；表格为空。
2. 发起一次流式 AI 请求：左侧出现「进行中」行，耗时与出 token 实时变化；结束后变为「成功」并填入总耗时、TTFT、t/s。
3. 选中请求：右侧显示方法/路径/协议/IP/UA/开始时间/状态码/token/速率；trunk 时间轴出现色块，块间可见 TTFT 与卡顿空隙；悬停块显示 bytes 与 Δtoken。
4. 构造上游失败/首 token 超时：时间轴出现虚线重试标注，悬停显示新实例/上游模型与原因。
5. 关闭监控（切到其它模式）：WS 断开、列表与详情清空；重新打开后从空开始。
6. 刷新页面：回到可视化配置模式（监控默认关闭），重新开启后无历史请求。
7. 缩放窗口到 ≤980px：左右两栏堆叠，表格仍保持紧凑行高。
8. 连续发起 500+ 请求：列表保留最近 500 条，优先裁剪最旧的已完成请求，进行中请求不被裁剪。
9. URL 内嵌凭据：用 `http://admin:secret@127.0.0.1:18080/admin` 打开，配置读取显示中性提示，不出现 `Failed to execute 'fetch' … credentials` 的阻断；切到「实时监控」且认证栏为空时，状态为「未连接」+「请先输入管理员凭据」，不发起 WS。
10. 认证栏连接（不勾选记住密码）：输入正确用户名/密码 → 点「连接」→ 状态「已连接」；发起流式请求可收到事件并渲染；`localStorage['model-router.monitor.cred']` **不存在**。
11. 认证失败：输入错误密码 → 点「连接」→ 自动重试一次后状态「认证失败，请检查用户名/密码」，`localStorage` 键被删除、密码输入框清空。
12. 记住密码：勾选「记住密码」后连接成功 → `localStorage['model-router.monitor.cred']` 存在且值只含 user/password；点「断开」或切到其它模式时键**保留**；取消勾选时键删除、密码框清空。
13. 刷新页面：记住密码已勾选时，刷新后用户名/密码自动填入、复选框保持勾选；进入「实时监控」自动连接成功。
14. 来源优先级：`localStorage` remembered → 认证栏输入 → 配置页管理员密码；只有显式点「连接」且勾选记住密码、握手成功才写入 localStorage。
15. 安全检查：页面 URL/地址栏、`document.cookie`、`console.log`、可见文本中均不出现密码；localStorage 中的明文密码有页面安全提示；密码只出现在 `type="password"` 输入框、localStorage（勾选记住时）与本次 WS 握手的临时 query 中。

### 23.3 验收对照（§24.3）

| 验收项 | 对应用例 |
|---|---|
| A1 `WS /api/monitor` 无 admin 密码 404、认证失败 401 | UT-702 |
| A2 无客户端连接时不影响响应体与日志（既有测试全绿） | UT-701；§7 全量回归 |
| A3 `start → chunk… → end`，chunk 数等于上游 trunk 数 | UT-703 |
| A4 非 AI 请求（favicon、401）零事件 | UT-704 |
| A5 全候选失败时 retry 按候选顺序、`end.status=502` | UT-705、UT-708 |
| A6 刷新/重连不补发历史 | UT-701 |
| A7 build/vet/gofmt/test -race 全绿 | §7 执行命令 |
| A8 覆盖未连接客户端、chunk 计数、慢客户端、认证拒绝 | UT-701、UT-703、UT-706、UT-702、UT-708 |
| A9 既有 130+ 用例不改断言仍通过 | `go test ./... -race -count=1` 全量 |

执行：

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./internal/proxy -run TestMonitor -v
go test ./... -race -count=1
```

### 23.4 M9 认证断链修复（P1–P7）

前端用最小 DOM 桩（Node `vm` + 假 `document`/`fetch`/`WebSocket`/`localStorage`）执行 `index.html` 内嵌脚本，覆盖以下行为；另用 `node --check` 校验脚本语法。

| 编号 | 用例 | 覆盖点 | 预期 |
|---|---|---|---|
| FT-901 | P1 认证栏存在性 | `#monitor-auth-user`、`#monitor-auth-password`（`type=password`）、`#monitor-auth-connect`、`#monitor-auth-disconnect`、提示文案 | 全部存在，密码框为 `type="password"` |
| FT-902 | P3 URL 内嵌凭据静默降级 | `location.href` 含 `user:pass@`，`fetch` 抛 `TypeError … credentials` | `load()` 显示中性提示、非错误态；不阻断打开监控 |
| FT-903 | P4 无凭据 | 三个来源均为空 | 状态「未连接」、提示「请先输入管理员凭据」、不创建 WebSocket |
| FT-904 | P4 来源③ 配置页密码 | 配置页 `admin_user`/`admin_password` 有值，认证栏为空 | 自动建立 WS，握手 query 携带该凭据；不写 localStorage |
| FT-905 | P4 来源② 认证栏 | 认证栏填用户名/密码后点「连接」 | 建立 WS；P2 写入 `model-router.monitor.cred`，值只含 user/password |
| FT-906 | P5 认证失败 | 握手未 open 即 `close code=1006`，首次失败后重试一次仍失败 | 状态「认证失败，请检查用户名/密码」；恰好 2 个连接（首次+重试）；删除 localStorage 凭据、清空密码框、提示重新输入 |
| FT-907 | P5 普通断线 | 已 open 后 `close code=1006` | 状态「连接已断开」；不重试；不删除 localStorage 已记住的凭据 |
| FT-908 | P6 断开/切离 | 点「断开」或切到其它模式 | WS 关闭、状态「未连接」；**不删除** localStorage 已记住的凭据（第二轮 W5 起） |
| FT-909 | P6 页面卸载 | 触发 `pagehide` / `beforeunload` | localStorage 凭据保留以便下次自动连接；不追加删除动作 |
| FT-910 | P2 禁止项 | 静态扫描 `index.html` | 不出现 `localStorage`、`document.cookie`；密码不出现在 `console.log` 或可见文本 |

验收对照：

| 验收项 | 对应验证 |
|---|---|
| `go build` / `go vet` / `go test ./... -race` | §7 执行命令；本次全量通过 |
| URL 内嵌凭据打开后不输入密码 → 「未连接」+「请先输入管理员凭据」 | FT-902、FT-903 |
| 认证栏输入正确凭据点「连接」→「已连接」，流式请求可渲染 | FT-905；UT-703 |
| 输入错误密码 →「认证失败，请检查用户名/密码」 | FT-906 |
| 关闭/切换后连接关闭且记住的凭据保留 | FT-908、FT-909 |
| P1–P7 决策表 | FT-901～FT-910、UT-709、§27.7/§27.8 |

> 注：§23.4 的 FT-901～FT-910 已按第二轮 W5 改写为「记住密码 → localStorage」的口径；更细的凭据生命周期断言见 §24 的 UT-715/UT-716/UT-722。

## 24. M9 第二轮调整：单表格 / 每行时间轴 / 清空 / 记住密码（§24.5）

### 24.1 用例

| 编号 | 用例 | 入口/方法 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-710 | 单表格结构（B1） | 真实 Chrome / `window.__modelRouterMonitor` | 不存在 `.monitor-wrap`、`.monitor-card-list`、`.monitor-card-detail`；`#monitor-table` 唯一；每行 11 个 `<td>`（统计列 10 + 时间轴） | 单表格；统计与时间轴同一 `<tr>` |
| UT-711 | 行内对齐与每行 trunk（B2） | 真实 Chrome，8 个真实流式请求 | 每行统计单元格与时间轴单元格 `getBoundingClientRect().top` 差；每行 `.tl-block` 数量 | top 差 ≤1px；每行至少 1 个 trunk 块 |
| UT-712 | 共享量程 4:1（B3） | 真实 Chrome，`slow4`（约 3.75s）与 `fast1`（约 0.95s） | 短请求最后一个块右端占时间轴宽度的比例；`axisSpan` | 约 25%（±3%），实测 25.54% |
| UT-713 | 进行中时间轴增长（B4） | 真实 Chrome，先完成 `axis6`（约 5.81s），再启动 `run3` | 间隔 1s 读取进行中行 `.tl-cursor` 的 `getBoundingClientRect().left` | 第二次采样比第一次大（实测 +110.3px） |
| UT-714 | 清空保留进行中与连接（B5） | 真实 Chrome，先有已完成 + 进行中行，点 `#monitor-clear` | 行数/状态、`window.__modelRouterMonitor.ws.readyState`、连接状态 | 只保留进行中行；`readyState=1`；状态「已连接」 |
| UT-715 | 记住密码写入 / 刷新自动连接 / 取消删除（B6） | 真实 Chrome，勾选 `#monitor-remember` 后连接、刷新、进入监控、取消勾选 | `localStorage['model-router.monitor.cred']`；刷新后输入框/复选框；自动连接；取消后删除 | 连接成功后 localStorage 有凭据；刷新后自动填入并自动连接；取消勾选后键删除、密码框清空 |
| UT-716 | 不勾选不落盘（B7） | 真实 Chrome，未勾选记住密码连接成功 | localStorage 键 | 不存在 |
| UT-717 | 1600×1019 无截断/无横滚（B8） | 真实 Chrome，真实长模型名 `claude-sonnet-5`、`deepseek-v4-pro`、`deepseek-v4.1-flash`，数字 `524288`/`999` | 所有模型/数字单元格 `scrollWidth <= clientWidth`；`.req-list.scrollWidth <= clientWidth`；时间轴列宽 | 零截断；表格无横向滚动；时间轴列约 642px |
| UT-718 | 1000×900 窄屏（B9） | 真实 Chrome | 页面 `documentElement.scrollWidth`；`.req-list` 内部 `scrollWidth/clientWidth` 与 `overflow-x` | 页面宽 1000；容器内部可横滚（1100 > 922） |
| UT-719 | 构建/静态检查（B10） | 命令行 | `go build ./...`、`go vet ./...`、`gofmt -l`、`go test ./... -race -count=1` | 全绿；`internal/proxy`、`internal/web` 无 gofmt 输出 |
| UT-720 | 清空后量程重算 | 真实 Chrome / `window.__modelRouterMonitor.axisSpan` | 清空已完成行后剩余进行中的量程 | 量程按剩余请求重算（截图中 5.81s → 1.89s） |
| UT-721 | 断开保留 remembered 凭据 | 真实 Chrome，勾选记住密码后点「断开」 | localStorage 键 | 仍存在；仅取消勾选/认证失败删除 |
| UT-722 | 凭据来源优先级 | 静态/真实 | localStorage → 认证栏 → 配置页；未勾选只在内存 | 优先使用 remembered；无 remembered 时可用输入框/配置页 |

### 24.2 验收对照（§24.5 B1–B10）

| 验收项 | 对应验证 |
|---|---|
| B1 无左右分栏、只有一张表格、统计与时间轴同一 `<tr>` | UT-710；真实 Chrome `hasWrap=0`、`tableCount=1`、`cellCounts=[11…]` |
| B2 每行都有 trunk 条、统计与时间轴单元格 top 差 ≤1px | UT-711；`rowTopDiffs=[0…]`、`blocksPerRow` 均 >0 |
| B3 共享量程，短请求块右端约 25%（±3%） | UT-712；实测 `fastRight=25.54%`、`axisSpan=3750ms` |
| B4 进行中时间轴终点随 1s 增长 | UT-713；游标 918.8 → 1029.1（+110.3px） |
| B5 清空只删除已完成、进行中保留、WS 未断 | UT-714；清空后 `count=1`、`statuses=['进行中']`、`readyState=1` |
| B6 勾选记住密码 → localStorage、刷新自动填入/自动连接、取消勾选删除 | UT-715；`b7Stored=null`、`b6AfterReload={user:'admin',password:'secret',remember:true}`、`b6Unchecked={stored:null,password:'',remember:false}` |
| B7 不勾选不落盘 | UT-716；`b7Stored=null` |
| B8 1600×1019 模型/数字/时间轴无截断、无横向滚动 | UT-717；`truncations=[]`、`listScrollWidth=1450=clientWidth`、`timelineWidth=642` |
| B9 1000×900 页面无横滚、表格容器内部可横滚 | UT-718；`documentWidth=1000`、`listScrollWidth=1100 > clientWidth=922` |
| B10 build/vet/gofmt/test | UT-719；执行结果见报告 |

真实浏览器脚本（不属于仓库测试）：

```bash
node .claude-runs/m9-monitor2/acceptance.js
```

截图与测量：`.claude-runs/m9-monitor2/monitor-1600x1019.png`、`monitor-1000x900.png`、`monitor-1600x1019-running.png`、`monitor-1600x1019-cleared.png`、`acceptance-measurements.json`。

## 25. M9 表格两行文本裁切修复（C1–C7）

### 25.1 修复内容

- 行高：`tbody tr` 由 26px 增至 33px（C2 预算 ≤34px）。
- 两行单元格：`td.req-path` / `td.req-route` 改为 `overflow:visible`，不再用 `overflow:hidden` 掩盖；`.req-model`/`.req-url` line-height 1.3，`.req-target`/`.req-method` line-height 1.2。
- 重试标记补充 `.retry` 类（与 `.tl-retry` 并存），保持旧选择器可用。
- 空态：无请求时保持表格隐藏 + 空提示，卡片高度 177px（≤200px）。

### 25.2 用例

| 编号 | 用例 | 入口/方法 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-723 | C1 两行文本不裁切 | 真实 Chrome，`claude-sonnet-5`/`MTI/mti-flash`、`deepseek-v4-pro`/`DeepSeek/deepseek-v4-pro` | 每行 route/path 单元格两行 span 相对 td 的上下留白；td.scrollHeight/clientHeight；computed overflow | 上下各 ≥1px；scrollHeight ≤ clientHeight；overflow visible |
| UT-724 | C2 行高预算 | 真实 Chrome | 每行 `getBoundingClientRect().height` | ≤34px（实测 33px） |
| UT-725 | C3 无横向滚动/零截断 | 真实 Chrome 1600×1019、1000×900 | documentElement.scrollWidth；全部单元格 scrollWidth/clientWidth | 两视口页面不横滚；1600 下全部单元格零截断 |
| UT-726 | C4 空态高度 | 真实 Chrome 1600×1019，清空后 | `#monitor-pane > .card` 高度 | ≤200px（实测 177px） |
| UT-727 | C5 功能元素 | 真实 Chrome | `#monitor-clear`、`#monitor-remember`、localStorage 键、`window.__modelRouterMonitor`（ws/axisSpan）、`.tl-cursor`、`.retry`、`.tl-block`、共享刻度 | 全部存在且可用；清空后 WS 仍 `readyState=1` |
| UT-728 | C6 静态检查 | 命令行 | `go build ./...`、`go vet ./...`、`go test ./... -race -count=1`、`gofmt -l internal/web internal/proxy` | 全绿；gofmt 无输出 |
| UT-729 | C7 截图 | 真实 Chrome | 1600×1019 有请求、1600×1019 空态、1000×900 | 三张截图保存到 `.claude-runs/m9-monitor2c/` |

### 25.3 验收对照（C1–C7）

| 验收项 | 对应验证 |
|---|---|
| C1 文字不被裁切 | UT-723；route 上行 top=2.11px、下行 bottom=2.11px；td scrollHeight=32=clientHeight；overflow=visible；主行 12px/次行 11px |
| C2 行高 ≤34px | UT-724；实测每行 33px |
| C3 无横向滚动、零截断 | UT-725；1600 documentWidth=1600、全部 cell scrollWidth=clientWidth；1000 documentWidth=1000、容器 922/1100 |
| C4 空态卡片 ≤200px | UT-726；实测 cardHeight=177px |
| C5 功能零回归 | UT-727；clear/remember 存在、localStorage 有凭据、hook wsState=1/axisSpan=956、blocks=12、cursors=1、retries=2、5 个刻度 |
| C6 静态检查 | UT-728；执行结果见报告 |
| C7 截图 | UT-729；`monitor-1600x1019.png`、`monitor-1600x1019-empty.png`、`monitor-1000x900.png` |

真实浏览器脚本（不属于仓库测试）：`node .claude-runs/m9-monitor2c/verify.js`；测量：`.claude-runs/m9-monitor2c/verify-measurements.json`。

## 26. M9 时间轴第三轮细化（§24.6 C1–C12）

### 26.1 用例

| 编号 | 检查项 | 环境 | 测量方法 | 判定阈值 |
|---|---|---|---|---|
| UT-730 | C1 等待段宽度与 title | 真实 Chrome 1600×1019，容器内真实 MTI/Claude Code 流式请求（该行已收 ≥3 个 trunk） | 读取该行 `.tl-wait` 的 `getBoundingClientRect().width` 与轨道宽度，计算 `waitPct = width / trackWidth × 100`；对照 `firstRelMs / displaySpan × 100`；读取 `title` 文本 | `waitPct` 与理论值误差 ≤2%；`title` 含「等待」；实测首块 135ms / 3000ms → `waitPct 4.5 == waitExpectedPct 4.5`，`title="等待首包 135 ms"` |
| UT-731 | C2 等待段配色 | 同上 | `getComputedStyle(.tl-wait).backgroundColor` 与同请求 `.tl-block` 的 `backgroundColor` 对比 | 两者不相等且肉眼可区分；实测 wait `rgb(251, 191, 36)`、block `rgb(91, 140, 255)` |
| UT-732 | C3 零 trunk 失败行等待段覆盖 | 真实 Chrome：真实失败行（500，0 chunk）+ 注入受控 3000ms 零 trunk 行 | 失败行 `.tl-wait` 宽度百分比 vs `durationMs / displaySpan × 100`；受控 3000ms 行（量程 3s）的 `.tl-wait` 宽度百分比与 `title` | 真实失败行 `waitPct == durationPct`（实测 1.37 == 1.37）；受控行 `waitPct = 100%`（±2%），`title="等待首包 3000 ms"` |
| UT-733 | C4 表头量程文本 | 真实 Chrome | 读取表头第 11 列 `#monitor-axis-title` 文本；检查该列是否出现「时间轴」「trunk」字样 | 文本匹配 `0s – <N>s` 且 N 为整数、`N ≥ ceil(表中最大耗时/1000)`；无「时间轴」「trunk」；实测 `"0s – 3s"`（最大耗时 2948ms） |
| UT-734 | C5 首尾刻度 | 真实 Chrome | 读取全部刻度文本及其边界框；与 `#monitor-axis-title` 的 N 对比 | 最左刻度严格等于 `0s`，最右等于 `<N>s` 且 N 与表头一致；实测刻度 `0s/1s/2s/3s`，最左 left=877、最右 right=1519，右端按既有 CSS 右对齐 |
| UT-735 | C6 量程向上取整到 4s | 真实 Chrome，注入受控最大耗时 3400ms 的请求 | 读取 `#monitor-axis-title`、刻度文本与数量、`axisSpan` | 量程显示 `0s – 4s`；刻度数 3–5 个且全部落在整数秒；实测标题 `"0s – 4s"`、刻度 `0s/1s/2s/3s/4s`、`axisSpan=4000` |
| UT-736 | C7 小数抖动与跨秒重绘 | 真实 Chrome，注入受控时间戳 3020 / 3080 / 3990 / 4010ms | 逐点读取 `axisTitle`、刻度文本与 `axisSpan` | 3020/3080/3990ms 三点标题与刻度完全不变（不抖动）；4010ms 跨秒后标题变 `0s – 5s` 且刻度重绘；实测 3.99s 内均为 `0s – 4s`→`[0s,1s,2s,3s,4s]`，4.01s 变为 `0s – 5s`→`[0s,2s,4s,5s]` |
| UT-737 | C8 两档视口无裁切、刻度不重叠 | 真实 Chrome 1600×1019 与 1000×900 | `documentElement.scrollWidth`；时间轴 td / 轨道的 `scrollWidth` 与 `clientWidth`；相邻刻度 `getBoundingClientRect()` 是否相交 | 两视口页面均不横向滚动（实测 docWidth=1600/1000）；时间轴列零裁切（1600 下 cell 654/654、1000 下 304/304、轨道 292/292）；`tickOverlap=false` |
| UT-738 | C9 静态检查 | 命令行 | `go build ./...`、`go vet ./...`；`gofmt -l internal/web internal/proxy`；`go test ./... -race -count=1` | build/vet 退出码 0；gofmt 无输出；test 全绿 |
| UT-739 | C10 Σ deltaTokens == outTokens | 真实 Chrome + WS 原始 `chunk`/`end` 事件（真实流式请求，21 个 chunk） | 对同一请求的 `chunk.deltaTokens` 求和，与末块 `cumTokens`、`end.outTokens` 对比；统计非零块数 | 三者精确相等（±0）；实测 `deltaSum 40 == cumTokens 40 == outTokens 40`，21 块中仅末块非零（40） |
| UT-740 | C11 块高度随 Δ 缩放 | 真实 Chrome | 逐块读取 `.tl-block` 的 `getBoundingClientRect().height`，按 `deltaTokens` 分组；`deltaTokens ≥ 8` 的块与轨道高度对比 | Δ 不同的块高度不同；Δ 最小块 ≈ 轨道高 40%（实测 Δ0 → 6.39px / 16px）；`deltaTokens ≥ 8` 的块高度等于行高（实测 Δ40 → 16px） |
| UT-741 | C12 title 含「距请求开始」 | 真实 Chrome | 遍历该请求全部 `.tl-block`，检查 `title` 是否含「距请求开始」与毫秒数 | 全部块命中（实测 `titlesHaveDistance=true`）；样例 `"#20 · 418 B · Δ40 tokens · 累计 40 · 距请求开始 901 ms"` |

### 26.2 验收对照（C1–C12）

证据统一引用 `.claude-runs/m9-monitor3/acceptance-measurements.json`（2026-11-16T01:04:39Z，`http://127.0.0.1:18080`）；C1/C2/C4/C5/C8/C10/C11/C12 与截图来自容器真实 MTI/Claude Code 流量，C3/C6/C7 在真实页面注入受控时长/时间戳，走同一套渲染代码。

| 验收项 | 对应验证 |
|---|---|
| C1 等待段宽度 ≈ 首块时刻/量程，title 含「等待」 | UT-730；`waitPct 4.5 == waitExpectedPct 4.5`（135ms / 3000ms），`waitLeftPx=0`、`waitWidthPx=28.88`、`trackWidthPx=642`，`waitTitle="等待首包 135 ms"` |
| C2 等待段颜色 ≠ trunk 块颜色 | UT-731；wait `rgb(251, 191, 36)` vs block `rgb(91, 140, 255)` |
| C3 零 trunk 失败行等待段覆盖整行 | UT-732；真实失败行 `waitPct 1.37 == durationPct 1.37`；受控 3000ms 行 `waitPct=100`、`title="等待首包 3000 ms"`（量程对齐时 100%，见 D-M9-29） |
| C4 表头显示 `0s – Ns`、无「时间轴/trunk」 | UT-733；`headerTimeline="0s – 3s"`、`axisTitle="0s – 3s"`，N=3 ≥ ceil(2948ms/1000) |
| C5 首刻度 `0s`、末刻度 `<N>s` 且与标题一致 | UT-734；刻度 `0s/1s/2s/3s`，`axisTitle="0s – 3s"`，`tickOverlap=false` |
| C6 3.4s 量程 → `4s`，3–5 个整秒刻度 | UT-735；`controlled.c6`：`axisTitle="0s – 4s"`、刻度 `0s/1s/2s/3s/4s`、`axisSpan=4000` |
| C7 小数位变化不抖动、跨秒重绘 | UT-736；3020/3080/3990ms 均为 `0s – 4s` + `[0s,1s,2s,3s,4s]`；4010ms 变为 `0s – 5s` + `[0s,2s,4s,5s]`、`axisSpan=5000` |
| C8 两档视口无裁切、刻度不重叠 | UT-737；1600×1019 `docWidth=1600`、timeline cell `654/654`；1000×900 `docWidth=1000`、cell `304/304`、track `292/292`，两档 `tickOverlap=false` |
| C9 build/vet/gofmt/test | UT-738；本轮复跑 `go build`/`go vet` 退出码 0、`gofmt -l` 无输出、`go test ./... -race -count=1` 全绿（`internal/proxy` 45.2s） |
| C10 Σ deltaTokens == outTokens | UT-739；`deltas` 为 20 个 0 + 末块 40，`deltaSum 40 == cumTokens 40 == outTokens 40`，`nonZeroDeltas=1`。真实 MTI 只在末尾 `message_delta` 上报 usage，非零仅出现在末块（D-M9-25），不出现「除首块外恒为 0」 |
| C11 块高度随 Δ 缩放、≥8 的块顶满 | UT-740；`blockHeights` 最小 6.39px（Δ0，40% × 16px）、最大 16px（Δ40），`ge8BlockHeights=[{delta:40,height:16}]` |
| C12 块 title 含「距请求开始」 | UT-741；`titlesHaveDistance=true`，样例 `"#0 · 359 B · Δ0 tokens · 累计 0 · 距请求开始 135 ms"`、`"#20 · 418 B · Δ40 tokens · 累计 40 · 距请求开始 901 ms"` |

真实浏览器脚本（不属于仓库测试）：`node .claude-runs/m9-monitor3/real-acceptance.js`；测量：`.claude-runs/m9-monitor3/acceptance-measurements.json`；截图：`monitor-1600x1019.png`、`monitor-1000x900.png`、`monitor-1000x900-timeline.png`。

## 27. M10 管理界面第四轮后端（§24.7 D4/D6/D7/D9/D10）

本轮只覆盖后端契约（E6–E9、E12、E14–E16）；E1–E5、E10–E11、E13、E17、E19 属于下一轮前端。

### 27.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-750 | E6 登录页公开、四个页面未登录 302、`/admin` 302 | `TestLoginAndPageRoutes` | `/admin/login` 200 + `text/html`，页面体含 `password`、`remember`、`/api/login`；`/admin/`、`/admin/monitor`、`/admin/config`、`/admin/json`、`/admin` 未登录均 302 且 `Location` 以 `/admin/login` 开头；Basic 登录后 `/admin` 302 到 `/admin/`，四个页面 200 |
| UT-751 | E7 记住我 cookie | `TestLoginCookieBehavior/remember_true` | 200；`Set-Cookie` 含 `Max-Age=2592000`、`HttpOnly`、`SameSite=Lax`、`Path=/`，不含 `Secure`；token ≥ 43 个 base64url 字符且不含用户名/密码；`{"redirect":"/admin/"}` |
| UT-752 | E7 会话 cookie | `TestLoginCookieBehavior/remember_false` | `Set-Cookie` 不含 `Max-Age`；解析出的 cookie `MaxAge=0` |
| UT-753 | E7 错误凭据 | `TestLoginCookieBehavior/bad_credentials` | 401、无 `Set-Cookie`、响应体不含密码 |
| UT-754 | `next` 开放重定向防护 | `TestLoginCookieBehavior/next_is_validated`、`TestSafeNext` | 仅 `/admin` 前缀通过；`//evil`、`https://evil`、`/\evil`、`/otherapp` 回落 `/admin/` |
| UT-755 | E8 Cookie 登录 / 登出失效 | `TestSessionCookieAuth` | 带 cookie 访问 `/admin/monitor` 200 且页面体不含 token；logout 200 且 `Max-Age=0`；再用旧 cookie 访问 `/admin/monitor` 302、`/api/stats` 401 |
| UT-756 | E9 旧认证继续可用 | `TestLegacyAuthStillWorks`、既有 `TestMonitorAuth`、`TestMonitorQueryCredentialFallback` | Basic 与 query 都能打开 `/api/stats`、`/api/ws`；§24.11 H2 后 `/api/monitor` 已删除（正确凭据 404） |
| UT-757 | E12 `/api/stats` 与 WS `stats.data` 键集合一致 | `TestStatsAPIAndWSKeysMatch` | 递归收集键集合（含 `ttft`/`duration`/`instances[]`/`recentErrors[]`），两侧无缺失/多余 |
| UT-758 | E14 120 条历史 → snapshot 100 条 | `TestSnapshotHoldsLastHundred` | 无客户端连发 120 个请求后连接 `/api/ws`，首帧 `snapshot`，`requests.length == 100`，`ts` 非递增、每条 `ended=true` |
| UT-759 | E15 无客户端仍累计 | `TestStatsAccumulateWithoutClients` | 初始 `requests.total=0`；3 个请求后 `requests.total=3`、`requests.success=3`、`byProtocol.openai=3` |
| UT-760 | E16 清除环形缓冲 | `TestMonitorClear` | `POST /api/monitor/clear` 200；重连后 `snapshot.requests=[]`；累计 `requests.total` 仍为 2 |
| UT-761 | API 未登录行为 | `TestClearAndStatsRequireLogin` | `/api/stats`、`/api/monitor/clear`、`/api/logout` 未登录均 401（不 302） |
| UT-762 | 统计口径单元测试 | `TestPercentilesNeedTenSamples`、`TestSampleRingKeepsLastWindow`、`TestTrackerRecordsStatsWithoutClients`、`TestStatsErrorSummaryTruncated`、`TestFormatUptime` | <10 样本 `p50/p95` 为 null；1000 样本环形窗口丢最旧；无客户端 tracker 仍记 total/success/failure/错误/环形缓冲；错误摘要 80 字符；uptime 格式 `2d 3h 14m` |
| UT-763 | E18 静态检查 | 命令行 | `go build ./...`、`go vet ./...` 退出码 0；`gofmt -l internal/proxy internal/web` 无输出；`go test ./... -race -count=1` 全绿 |

### 27.2 既有测试调整说明

`proxy_test.go` 的 `TestAdminAuth` 原断言“无凭据 `GET /admin` → 401 + `WWW-Authenticate`”。§24.7 D4/D6 明确改为“页面请求 302 到 `/admin/login`”，故该用例改为断言 302 + `Location`；授权访问改为 Basic 打开 `/admin/` 返回 200。除此以外未改动任何既有断言，§24 的 10 个 monitor 测试（`TestMonitorAuth`、`TestMonitorQueryCredentialFallback`、`TestMonitorStreamEvents`、`TestMonitorNonAIRequestsSilent`、`TestMonitorRetryThen502`、`TestMonitorFirstTokenTimeoutRetry`、`TestMonitorNoHistoryReplay`、`TestMonitorSurvivesReload`、`TestMonitorHubNoClientNoCollection`、`TestMonitorSlowClientDisconnected`）全部保持原样通过。

### 27.3 验收对照

| 验收项 | 证据 |
|---|---|
| E6 | UT-750 |
| E7 | UT-751～UT-753 |
| E8 | UT-755 |
| E9 | UT-756 + 既有 §24 测试 |
| E12 | UT-757 |
| E14 | UT-758 |
| E15 | UT-759 |
| E16 | UT-760 |
| E18 | UT-763 |

### 27.4 未覆盖（下一轮前端）

E1–E5（时间轴对齐/表头图例/客户端列）、E10–E11（四页主体区分与统计首页渲染）、E13（前端自动连接/重连）、E17（删除监控认证 UI）、E19（视口布局）不在本轮后端范围。

## 28. 时间轴逐行等高与内容块着色（§24.8 Y1–Y6）

### 28.1 用例

| 编号 | 检查项 | 方法 | 判定 |
|---|---|---|---|
| UT-764 | Y6 后端计数 | `TestChunkContentDeltasCountsContentBlockDelta` | 逐块 `contentDeltas = [0,0,0,1,2,0,0,0,0]`；Σ = 3 = `content_block_delta` 行数；`message_start`/`content_block_start`/`ping`/`content_block_stop`/`message_delta`/`message_stop`/`[DONE]` 均为 0；`tokens/inTokens` 仍只取 usage |
| UT-765 | Y4 chunk 事件字段 | `TestChunkEventContentDeltasOverWS` | 三个上游 trunk 的 `chunk.contentDeltas = 0/2/0`，Σ=2 等于事件行数；`Σ deltaTokens=2` 仍等于 `usage.output_tokens` |
| UT-766 | Y1–Y3 真实浏览器 | headless Chrome + CDP，1600×1019 与 1000×900，真实 Anthropic 流式请求 | Y1 同行所有条 height/top 最大差 ≤0.5px 且 ≈6.4px；Y2 绿块数 >0 且 >50%；Y3 绿块 title 含「内容块」且 k>0，蓝块 k=0 |
| UT-767 | Y5a 死代码 grep | `grep -nE 'timelineTokenThreshold|blockHeightPercent|refreshRowTimelineHeights|rowBaseHeightPercent|syncWaitHeight|maxDelta' internal/web/index.html` | 无匹配（函数名清单见 `design §29.2`） |
| UT-768 | Y6 回归与静态检查 | `go build ./...`、`go vet ./...`、`gofmt -l internal/proxy internal/web`、`go test ./... -race -count=1` | 全部通过；既有 §24/§24.5/§24.6/§24.7 断言未放宽 |

### 28.2 验收对照

| 验收项 | 证据 |
|---|---|
| Y1 逐行等高 | UT-766；1600×1019 与 1000×900 下 173 条（1 等待段 + 172 块）`height` 全为 6.391px（差 0 ≤0.5），`top` 差 0；轨道 `height=16`。原始数据：`.claude-runs/m9-monitor5/measurements.json` |
| Y2 绿块占比 | UT-766；同一真实流式请求（300 个 `content_block_delta`，37,817 B）共 172 块，绿块 169（98.3% > 50%），3 个蓝块分别为起始帧、心跳/块边界帧与末帧（`内容块 0`） |
| Y3 tooltip | UT-766；绿块 title 形如 `#2 · 136 B · Δ0 tokens · 内容块 1 · 累计 0 · 距请求开始 320 ms`，解析出的 `内容块` 均 >0（首个 8 个为 1/1/1/1/4/2/2/4）；蓝块 title `内容块 0`（如 `#0 · 359 B · Δ0 tokens · 内容块 0 · 累计 0 · 距请求开始 319 ms`），末块 `累计 300` |
| Y4 contentDeltas 求和 | UT-764 + UT-765；真实请求 `Σ chunk.contentDeltas = 300` 与 SSE `content_block_delta` 实际 300 条精确相等 |
| Y5a 无 token 高度路径 | UT-767 |
| Y6 回归/静态检查 | UT-768 |

说明：本轮真实请求使用 `model: claude-sonnet-5` 走 `/anthropic/v1/messages`；按容器配置的实例顺序由 MTI（`mti-flash`）承接（首个 anthropic 实例），而非 DeepSeek。MTI 返回了 300 条 `content_block_delta`（含 141 条 `thinking_delta`、158 条 `text_delta`、1 条 `signature_delta`），满足“真实流式、输出 ≥100 tokens”的判定条件；若必须指定 DeepSeek，需要调整实例顺序或降级 MTI，属于环境选择而非实现差异。

## 29. 第六轮：tooltip 开始时间、自适应高度、快照完整化、OpenAI 判据（§24.9 Z1–Z6）

### 29.1 用例

| 编号 | 检查项 | 测试/方法 | 判定 |
|---|---|---|---|
| UT-769 | Z5 OpenAI 判据 | `TestContentDeltasOpenAICriteria`、`TestChunkEventContentDeltasOpenAIOverWS` | 单测：`role`、空 content/reasoning、空 tool arguments、`finish_reason`、空 choices、`[DONE]`、`: keep-alive` 均为 0；content/reasoning/非空 arguments 各 +1；三者同现仍 +1；`usage.completion_tokens` 仍更新 tokens。WS 端到端：5 个 trunk 的 `contentDeltas = 0/1/1/0/0` |
| UT-770 | Z4 chunksList 生成与 JSON 字段 | `TestSnapshotChunksListRecordsBlocks`、`TestSnapshotJSONCarriesChunksList` | 无客户端时 3 个 chunk 仍写入快照；JSON `chunksList` 每项恰好 7 键；非流式请求输出空数组而非 null |
| UT-771 | Z4c 400 块截断与合成块 | `TestSnapshotChunksListCompaction` | 500 块 → 400 块；前 200 原样、后 199 从 seq 301 起；合成块 seq=250、时间跨度 [200ms,301ms]、cum=300；bytes/content/delta 总和与原始 500 块一致 |
| UT-772 | Z1/Z2/Z3 真实浏览器 | headless Chrome + CDP | Z1a 任意条 title 末尾匹配 `· 开始 \d{2}:\d{2}:\d{2}\.\d{3}$` 且原字段仍在；Z1b 同一请求等待段与所有块时间戳一致；Z2a 两档视口 `scrollHeight <= innerHeight+1`；Z2b 视口 1400/900 的 `.req-list` clientHeight 差 >100；Z3a 刷新后 ts 严格降序、序号递增；Z3b 新请求插最上且序号重排 |
| UT-773 | Z4/Z5 真实浏览器 | headless Chrome + CDP | Z4a 切到 `/admin/` 再回监控页绿块数不变；Z4b 完整刷新后仍见绿块且 `chunksList` 含 `contentDeltas>0`；Z5a OpenAI 真实流式绿块占比 >50%；Z5b role/finish_reason/[DONE] 块为 0 |
| UT-774 | Z6 回归与静态检查 | `go build`/`go vet`/`gofmt`/`go test -race` | 全部通过；既有断言未放宽 |

### 29.2 验收对照

真实环境（2026-11-16 10:28 本地时间，headless Chrome + CDP，容器 `wuzm219/model-router:0.1.1` 重新构建并清空环形缓冲后；先 `POST /api/login` 再 `Network.setCookie` 注入 `mr_session`）：

| 验收项 | 证据 |
|---|---|
| Z1a/Z1b | UT-772；真实 Anthropic 请求（38,500 B、300 个 `content_block_delta`）共 129 个时间轴元素（1 等待段 + 128 块），全部 title 匹配 `· 开始 \d{2}:\d{2}:\d{2}\.\d{3}$`；唯一时间戳 `10:28:28.137` 等于该请求 `ts` 的本地时钟；等待段保留「等待首包 147 ms」，块保留「内容块 k」「距请求开始 x ms」。原始数据 `.claude-runs/m9-monitor6/measurements.json` |
| Z2a | UT-772；`scrollHeight == innerHeight`：1600×1019 → 1019/1019，1600×900 → 900/900，1000×900 → 900/900；`scrollWidth == innerWidth`：1600/1600、1000/1000 |
| Z2b | UT-772；`.req-list.clientHeight`：视口高 1400 → 1232px，视口高 900 → 732px，差 500px（>100） |
| Z3a/Z3b | UT-772；清空后完整刷新时 2 行 `ts` 严格降序、序号 1..2；新请求到达后其 `id` 位于第一行、`ts` 最大，序号重排为 1..N |
| Z4a | UT-773；真实流式请求切到 `/admin/` 再回 `/admin/monitor`：绿块 125 → 125，块数 128 → 128，`chunksList` 存在 |
| Z4b | UT-773；完整刷新后同一行绿块仍为 125，`chunksList` 的 `contentDeltas` 求和 300，等于该请求 SSE 的 `content_block_delta` 条数 300；至少一个块 `contentDeltas > 0` |
| Z4c | UT-771；500 块 → 400 块，合成块 seq=250、时间跨度 [200ms,301ms]、bytes/content/delta 总和与原始一致 |
| Z5a | UT-773；真实 OpenAI 协议流（`POST /v1/chat/completions`，模型 `mti-pro`，92,780 B）：174 块、绿块 173（99.4% > 50%） |
| Z5b | UT-769 + UT-773；单测逐事件 `role/空 delta/finish_reason/[DONE]/keep-alive` 均 0；真实流 294 个内容事件、1 个 `finish_reason`、1 个 `[DONE]`、0 条解析失败，Σ 块 `contentDeltas` = 294 = 内容事件数（证明 finish/[DONE] 未计数），首 trunk 为 0；末 trunk 因上游把 3~5 个内容事件与 `finish_reason`/`[DONE]` 合并写出而为正（见 D-M10-29） |
| Z6 | UT-774 |

> §24.10 A3 修正：OpenAI 协议的验收样本模型必须取自 `GET /v1/models` 的真实上游模型名（本环境为 `mti-pro`）。原 §24.9 样本 `claude-sonnet-5` 只适用于 Anthropic 角色入口；传给 `/v1/chat/completions` 会被原样透传并得到上游 503 `model_not_found`，不能作为 OpenAI 验收样本。

## 30. 第七轮：OpenAI 模型语义、统计落盘、数值可读化（§24.10 A/B/C）

### 30.1 用例

| 编号 | 检查项 | 测试/方法 | 判定 |
|---|---|---|---|
| UT-775 | A2 `owned_by` 与模型并集 | `TestModelsOwnedByInstance` | 模型集合 = 实例顺序并集去重；`owned_by` 为首个声明实例名；无 `claude-*`；未命名实例回退 `model-router`（既有测试） |
| UT-776 | B1 落盘/加载/损坏回退 | `TestStatsPersistenceRoundTrip`、`TestStatsPersistenceCorruptFile` | 原子写 `stats.json`，`schema=1`，累计计数/实例/错误可恢复；分位数与请求快照不恢复；截断 JSON、未知 schema、负计数 → `.bad` + 零值启动 |
| UT-777 | B1 节流与清空语义 | `TestStatsPersistenceThrottle`、`TestStatsClearCumulative` | 5 秒内第二次 flush 不改文件；超过窗口才写；clear 清零累计/实例/错误/分位数，保留 100 条请求环 |
| UT-778 | B1 clear 接口 | `TestStatsClearEndpoint` | 未登录 401；登录 200；累计归零；`stats.json` 立即改写；snapshot 请求数不变 |
| UT-779 | B3a–B3f 真实容器 | 容器 + curl + docker inspect | 见 30.2 |
| UT-780 | C4a–C4g 真实浏览器 | headless Chrome + CDP 注入统计快照 | 见 30.3 |
| UT-781 | G1–G6 回归 | `go build`/`go vet`/`gofmt`/`go test -race` + 端点 | 全部通过，既有断言未放宽 |

> 既有断言调整：`TestModelsOpenAI`、`TestModelsOpenAICapabilities`、`TestModelsSchemaIsDecidedByPath` 原先断言 `owned_by == "model-router"`；按 §24.10 A2 改为断言首个声明实例名（实例未显式命名时后端以 `base_url` 作为实例名）。这是规格要求的语义变更，不是放宽。

### 30.2 B3 验收对照（2026-11-16 10:51 实测）

| 验收项 | 证据 |
|---|---|
| B3a | 3 个 Anthropic 请求均 200；等待 6.5s 后宿主 `./.docker/stats.json` 存在，`schema=1`，`requests.total=118`（含此前重启前累计值，≥3；同时验证重启恢复） |
| B3b | `docker compose restart` 前 118 → 后 118，delta=0（±1 允许） |
| B3c | 先 `stop`（避免退出补写覆盖）、写入 `{`、再 `start`：服务正常启动；日志含 `warning: invalid /home/nonroot/config/stats.json; moved to .../stats.json.bad and starting with zero statistics`；`.bad` 内容为 `{`；`/api/stats` total=0 |
| B3d | `POST /api/stats/clear` 200；`requests.total=0`；`stats.json` 立即改写为 total=0；WS snapshot 请求数 3 → 3（清空不影响 100 条环） |
| B3e | 100 个 OpenAI 请求在 20ms 内全部发出（并发），1.756s 内全部结束；`stats.json` mtime 变化 1 次（≤3），最终 total=100 |
| B3f | `docker inspect` 镜像 `Config.Volumes = {"/home/nonroot/config":{}}`；`MODEL_ROUTER_PORT=18081 docker compose -p mr-named -f docker-compose.named.yml up -d` 成功，`/admin/login` 200，`/model-router -version` exec 0 |

原始数据：`.claude-runs/m9-monitor7/measurements-b.json`。

### 30.3 C4 验收对照（真实 Chrome 注入受控快照）

| 验收项 | 实测 |
|---|---|
| C4a | `ttft.p50=999`、`p95=1234` → `999ms / 1.23s`，title `P50 999ms · P95 1,234ms` |
| C4b | `outTokens=62147` → `62.1K`，title `62,147` |
| C4c | `inTokens=1234567` → `1.23M`，title `1,234,567` |
| C4d | `duration.p50=3600000` → 显示 `1.0h / 123.46s`（P95=123456ms） |
| C4e | `requests.total=999` → `999`（不缩写） |
| C4f | `retries=979999` → `980.0K`，title `979,999` |
| C4g | 实例表：请求 `62.1K`/title `62,147`，成功 `1.23M`/title `1,234,567`，失败 `999`，平均 TTFT `1.23s`/title `1,234ms`；已移除实例行显示「已移除」；所有 K/M 缩写单元格均有 title（`abbreviatedWithoutTitle=[]`） |

原始数据：`.claude-runs/m9-monitor7/measurements-c.json`；截图 `stats-1600x1019.png`。

### 30.4 G 组对照

| 验收项 | 证据 |
|---|---|
| G1 | `/v1/models` 返回 `mti-pro, mti-flash, deepseek-v4-pro, deepseek-v4.1-flash, qwen3.8-flash, deepseek-flash`，与配置并集顺序一致；`owned_by` 与首个声明实例名一致；无 `claude-*` |
| G2 | `mti-pro` 请求 200；`claude-sonnet-5` 返回上游 503 `model_not_found ... No available channel for model claude-sonnet-5`，原样透传 |
| G3 | 既有 Anthropic 路由/角色映射测试全绿 |
| G4 | B3a–B3f 全过 |
| G5 | C4a–C4g 全过 |
| G6 | UT-781：build/vet/gofmt/test -race 全绿 |

## 31. 第八/九轮：进行中快照、删除旧 WS 路径、JSON 页自适应（§24.11 H / §24.12 J）

### 31.1 用例

| 编号 | 检查项 | 测试/方法 | 判定 |
|---|---|---|---|
| UT-782 | H1 进行中行进快照 | `TestSnapshotIncludesInProgress` | 快照含 ended=false 行，chunksList 数量与已产生块一致，整体 ts 降序 |
| UT-783 | H1 进行中 200 上限 | `TestSnapshotActiveLimit` | 205 个进行中 → 注册与快照均 200，最早 5 个被丢弃 |
| UT-784 | H1 续接与结束转换 | `TestSnapshotInProgressContinuation`、`TestSnapshotPrefersEndedRecord` | 快照后继续 chunk，end 后只保留 ended 记录；同 id 出现时 ended 优先 |
| UT-785 | H2 旧路径删除 | `TestRemovedMonitorRoute`、`TestLegacyAuthStillWorks` | `/api/monitor` 带正确 Basic 404、无凭据 401；`/api/ws` 101 |
| UT-786 | H3a–H3e 真实浏览器 | headless Chrome + CDP | 进行中快照、续接、刷新结束态、降序、200 上限见 31.2 |
| UT-787 | J2a–J2e 真实浏览器 | headless Chrome + CDP | 见 31.3 |
| UT-788 | H3h 回归 | `go build`/`go vet`/`gofmt`/`go test -race` | 全绿，既有断言未放宽 |

### 31.2 H3 验收对照（真实 Chrome + CDP，2026-11-16 11:10 前后）

| 验收项 | 证据 |
|---|---|
| H3a | 长流进行中刷新页面：snapshot 行 `ended=false`、`chunks=17`、`chunksList` 长度 17（相等）、`cursor=true`、耗时显示 `870ms` |
| H3b | 同一行块数在快照后 900ms 内从 17 增长到 85（不因来自快照而断流） |
| H3c | 流结束后再次刷新：同 id `ended=true`；刷新前 blocks 239 / tok 236，刷新后 blocks 239 / tok 236，`chunksList` 239 |
| H3d | 快照混排 13 条 ended + 1 条进行中，`ts` 严格降序 |
| H3e | UT-783：205 个进行中 → 注册与快照均 200，最早 5 个被丢弃 |
| H3f | `GET /api/monitor`（正确 Basic）=404；`/api/ws` 握手 101 |
| H3g | `dialMonitor` 改为 `/api/ws`；`TestWSHistoryOnlyViaSnapshot` 替代旧无回放测试（历史仅在 snapshot 出现），`TestRemovedMonitorRoute` 覆盖 404/401/101，其余 monitor 断言未改 |
| H3h | UT-788：build/vet/gofmt/test -race 全绿 |

原始数据：`.claude-runs/m9-monitor8/measurements.json`；截图 `monitor-running-snapshot.png`。

### 31.3 J2 验收对照

| 验收项 | 实测 |
|---|---|
| J2a | 1600×1019：scrollHeight/innerHeight=1019/1019；1000×900：900/900；横向 1600/1600、1000/1000 |
| J2b | `#json-editor` clientHeight：视口 1400 → 1095px，视口 900 → 595px，差 500px（>150） |
| J2c | computed `resize` 在 1600 与 1000 均为 `none` |
| J2d | 配置页无内置 textarea；注入临时 textarea 测得全局规则 `min-height:480px`、`resize:vertical`、clientHeight 478（border-box 减去 2px 边框），body 无 `json-mode` |
| J2e | 300 行内容下 editor `scrollHeight=5905 > clientHeight=714` 可滚动；校验/保存/重载按钮均可见；点击「校验」返回错误提示（`400 parse config: json: unknown field "line"`），功能正常 |

原始数据：`.claude-runs/m9-monitor8/measurements.json`；截图 `json-1600x1019.png`、`json-1000x900.png`。

## 32. 第十轮：项目品牌化（§24.13 K6）

### 32.1 用例

| 编号 | 检查项 | 测试/方法 | 判定 |
|---|---|---|---|
| UT-789 | K6d 匿名 favicon | `TestFaviconAnonymous` | 无凭据 GET `/favicon.ico` → 200、`Content-Type: image/x-icon`、`Cache-Control` 含 `max-age=86400`；响应体以 `00 00 01 00` 开头；ICONDIR count ≥3；含 16/32/48 条目；`admin_password` 为空时同样 200 |
| UT-790 | §24.14 L3a–L3d Logo 匿名 | `TestLogoSVGAnonymous` | 匿名 GET `/logo.svg` → 200、`image/svg+xml`、体以 `<svg` 开头且含 `viewBox="0 0 256 256"`、缓存 1 天；旧路径 `/admin/logo.svg` 非 200；带会话 Cookie 仍 200；`admin_password` 为空仍 200 |
| UT-791 | K6a–K6c/K6f 页面品牌 | headless Chrome + CDP | 四页 `<title>` 与左上角文字为「模型智能路由」，登录页相同；可见文本无 `model-router` |
| UT-792 | K6b 布局 | headless Chrome，1600×1019 与 1000×900 | 四页 Logo 24×24、与文字垂直居中、gap 8px；1000×900 header 不换行、无溢出 |
| UT-793 | K6g 英文标识未误改 | `grep -rn "model-router" go.mod Dockerfile docker-compose.yml` | module/镜像 tag/服务名保持原值 |
| UT-794 | K6h 回归 | `go build`/`go vet`/`gofmt -l internal`/`go test ./... -race` | 全绿 |

### 32.2 K6 验收对照（2026-11-16 12:06 实测）

| 验收项 | 证据 |
|---|---|
| K6a | `/admin/`、`/admin/monitor`、`/admin/config`、`/admin/json` 的 `<title>` 分别为「模型智能路由 · 统计 / 实时监控 / 配置 / JSON 高级」，左上角 `h1` 文本均为「模型智能路由」 |
| K6b | 四页 Logo 渲染尺寸 24×24、`display:flex`、`gap:8px`、图标与文字中心差 0px；1000×900 下 header 高度 31.5px（与 1600 相同，未换行）、`scrollWidth<=clientWidth`、整页无横向滚动 |
| K6c | `/admin/login` `<title>` 为「模型智能路由 · 登录」，卡片顶部 Logo 24×24 + 「模型智能路由」 |
| K6d | 匿名 `curl -s -o /dev/null -w "%{http_code} %{content_type}"` → `200 image/x-icon`；响应体 `magic=00000100`、ICONDIR count=6（含 16/32/48） |
| K6e / L3a | **已修订**：匿名 `/logo.svg` → 200、`image/svg+xml`、体以 `<svg` 开头、含 `viewBox="0 0 256 256"`；`/admin/logo.svg` 不再返回 200 |
| K6f | `grep -c model-router internal/web/index.html internal/web/login.html` 均为 0；浏览器逐页 `document.body.innerText` 不含 `model-router` |
| K6g | `grep -rn model-router go.mod Dockerfile docker-compose.yml` 仍为 `module model-router`、服务名 `model-router`；镜像标签后按用户要求统一改为 `wuzm219/model-router:latest`（两个 compose 文件同步） |
| K6h | UT-794：`go build`/`go vet`/`gofmt -l internal main.go`/`go test ./... -race -count=1` 全绿 |

原始数据：`.claude-runs/m9-brand/measurements.json`；截图：`stats-1600x1019.png`、`stats-1000x900.png`、`monitor-1600x1019.png`、`monitor-1000x900.png`、`login-1600x1019.png`。

## 33. 第十二轮：服务熔断（§24.15 M）

### 33.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-800 | M10/M15e 锁定与解锁 | `TestLockRoutesEverythingAndUnlockResets`、`TestLockedOpenInstanceStillServes` | 锁定后全部流量到被锁实例（含处于 open 的被锁实例）、熔断暂停；移动锁生效；解锁清零状态并按第一个 `closed` 重新选路 |
| UT-807 | M14/M15a/M15c/M15d/M15l 统计行与锁 API | `TestStatsInstanceFieldsOrderAndLockAPI` | openai 在前的优先级排序；`state/locked/degradedSince/reason` 字段，`state` 仅取 `closed`/`open`/`locked`/`-` 且 `current` 恒为 false；lock/unlock 幂等与校验；重启后全 closed、unlocked |
| UT-808 | M15n 实例 token 两列 | 前端 `internal/web/index.html`（`renderStatInstances`） | OpenAI / Anthropic 两张实例表各在「平均 t/s」之前有「输入 token」「输出 token」两列；两张表各自的 `<colgroup>` 均为 11 列且与表头一致；数值千分位、缺省 `0` |
| UT-809 | M15o 实例 token 累计与失败计入 | `TestStatsInstanceTokenColumns` | 成功请求累加 `100/40`，失败请求再累加 `10/5`；实例行 `110/45` 与聚合值一致；JSON 键为 `inTokens`/`outTokens` |
| UT-810 | M15p 输出全量含 thinking、与 genOut 不同口径 | `TestStatsInstanceOutTokensIncludeThinking` | 单 trunk thinking-only 流（`usage.output_tokens=512`、`genMs=0`）：`genOut` 不增长，实例 `outTokens` 增加 512；再加一条 100 token/1s 的普通请求后，输出列为 `612`、平均 t/s 仍按 `genOut=100` 计算 |
| UT-811 | M17 实例 token 落盘与旧文件升级 | `TestStatsInstanceTokensPersistence` | `stats.json` 的实例 `inTokens`/`outTokens` 可写可恢复；旧 schema-1 文件缺字段时请求计数保留、token 列恢复为 0 |
| UT-812 | M17 输入全量（缓存分字段求和） | `TestUsageInputTokensIncludeCache` | Anthropic `100 + 25 + 50 = 175`（非流式与 `message_start.message.usage` 流式一致）；OpenAI `prompt_tokens=100` 不叠加 `cached_tokens=50`；输出仍为 `output_tokens=7` |
| UT-813 | M15q 实例统计按协议拆表 | 前端 `internal/web/index.html`（`renderStatInstances` / `appendStatInstanceRow`） | 上表标注「OpenAI 实例」、下表标注「Anthropic 实例」；两张表各有独立 `<colgroup>`（均 11 列且顺序与表头一致）、`<tbody>` 与空状态，文案分别为 `暂无 OpenAI 实例。` / `暂无 Anthropic 实例。`；按 `item.protocol` 各自取行，某协议为空时只隐藏该表；`stat-muted`/状态色与锁按钮由同一行渲染函数生成，两张表行为一致 |
| UT-814 | M15r 实例排序（协议分组 + 优先级） | `TestStatsInstanceOrderByProtocolAndPriority` | `/api/stats` 的 `instances[]` 为 openai 组在前、anthropic 组在后；组内 `priority` 升序、同优先级按 `name` 升序；`removed` 行使用同一比较器，不再统一追加到所有配置行之后 |
| UT-815 | M15s 实例 token K/M 缩写 | 前端 `internal/web/index.html`（`formatInstanceTokenCount` → `formatStatCount`） | 与顶部磁贴共用阈值 `1000`：`< 1000` 为整数原值（`999`/`0`）；`≥ 1000` 且 `< 1e6` 为 K 保留 1 位（`1.0K`/`1.2K`/`441.4K`）；`≥ 1e6` 为 M 保留 2 位（`1.00M`/`1.05M`）；`999960 → 1.00M`（舍入后跨单位重新取单位）；`exact` 为精确千分位并作为单元格 `title`；缺省/`NaN`/非有限值为 `0` |
| UT-816 | M15t 数值与单位展示全局唯一 | 前端 `internal/web/index.html`（`formatStatCount` / `formatDuration` / `formatPercent` / `formatRate`） | 页面内仅一套 K/M 计数与一套时长规则。计数：`999→999`、`1000→1.0K`、`1234→1.2K`、`999960→1.00M`、`1000000→1.00M`、`1050000→1.05M`。时长：`0→0ms`、`999→999ms`、`1000→1.00s`、`1500→1.50s`、`9999→10.00s`、`10000→10.00s`、`59999→60.00s`、`60000→1.00m`、`3600000→1.00h`；监控页耗时/首包与统计页平均 TTFT、P50/P95 调用同一函数；百分比/速率统一 1 位小数。`formatMs`、`formatStatDuration`、`formatInstanceTokens` 已删除，`formatInstanceTokenCount` 仅薄封装 `formatStatCount`；`formatUptime` 保留为独立的 d/h/m 复合格式 |

### 33.2 场景说明

- §24.17 起主动探测子系统已删除，本节原探测用例（`probe` 瞬态、探测报文、探测模型优先级、`current` 回退、全 open 链尾等）同步作废；状态机用例改由 §35 的 UT-829～UT-841 覆盖。
- M17 用例在 `stats_tokens_internal_test.go`：`recordTokenRequest` 以单 trunk（`genMs=0`）模拟 thinking-only 流，以双 trunk 模拟 1s 生成窗口；`TestStatsInstanceOutTokensIncludeThinking` 先把含 `thinking_delta` 与 `message_delta.usage.output_tokens=512` 的 SSE 喂给 `sseState`，再断言实例 `genOut` 与 `outTokens` 的差异；`TestUsageInputTokensIncludeCache` 覆盖 Anthropic 缓存分字段求和与 `message_start.message.usage` 嵌套解析。

## 34. 第十三轮：后台周期探测与监控页探测展示修正（§24.16 M12）

> §24.17（第十四轮）已整体删除本节描述的主动探测子系统与其监控展示，下列用例中仅构建检查、单实例协议与非流式 flush 语义继续有效；探测调度、探测报文、探测行展示、`current` 推进相关用例作废，替代覆盖见 §35。

### 34.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-823 | M15ac 构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race` | 全部通过 |
| UT-824 | M32/M15ap 单实例协议不参与熔断 | `TestSingleInstanceProtocolNeverOpens` | 单实例协议连续 3 次失败（超过阈值 1）后：`stateFor` 恒为 `closed`、`degradedSince = 0`、`reason` 为空，`degradedUntil` 为零；推进假时钟后仍为 `closed` 且上游请求数不变；`requests.total = failed = 3`、`recentErrors` 3 条、实例行 requests/failed 均为 3 |
| UT-826 | M15af 非流式不逐块 flush、SSE 仍逐块 flush | `TestNonStreamingCopyDoesNotFlush`、`TestStreamingCopyStillFlushes` | 3×32KiB 以上的 body 被分成多次写入；`copyBody`/`copyBodyTee` 的 `Flush` 计数为 0 且内容/捕获完整；`copySSE` 每个 chunk 恰好 flush 一次 |

### 34.2 场景说明

- UT-823 与 UT-826 与探测无关，继续有效；M15af 的 flush 语义在本轮未改动。
- UT-824 的测试名由 `TestProbeCoversSingleInstanceProtocol` 改为 `TestSingleInstanceProtocolNeverOpens`，去掉 `probeTick` 调用后仍断言单实例恒为 `closed`、统计与 `recentErrors` 正常累加。
- 本节原 UT-817～UT-822、UT-825、UT-827、UT-828 依赖探测定时器、探测报文、探测行 DOM 与 `current` 推进，已随 §24.17 删除。

## 35. 第十四轮：取消主动探测，真实请求驱动半开熔断（§24.17 M33–M40）

### 35.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-829 | M33/M15al 无主动探测 | `TestNoActiveProbing` | 三实例全 `open`、推进假时钟 5 分钟且不发请求：上游请求数不变、`requests.total` 不变；到期实例自动 `closed` |
| UT-830 | M34/M15ak 状态域与 `current` 消失 | `TestStatsInstanceFieldsOrderAndLockAPI` | `/api/stats` 的 `instances[].state` 只取 `closed`/`open`/`locked`/`-`；`current` 恒为 false（字段保留以兼容既有 wire 格式） |
| UT-831 | M35/M15aj 空闲自动恢复 | `TestIdleRecoveryWithoutRequests` | 实例转 `open` 后不发任何请求，仅推进时钟越过 `degradedUntil`：`state()` 与 `stateFor` 均为 `closed`，下一条请求打到它 |
| UT-832 | M36/M15ag 第一个 `closed` 选路 | `TestFirstClosedRouting` | 三实例 [A,B,C]，A `open`、B/C `closed` → 请求打 B；A 的 `degradedUntil` 到期后再发请求 → 打 A |
| UT-833 | ~~M36/M15ah 全 `open` 打第一个~~（§24.19 M47 撤订） | ~~`TestAllOpenUsesFirst`~~ | 该语义已撤订，用例按新语义改写为 `TestAllOpenRetriesEveryInstance` 等，见 §37 UT-844～UT-848 |
| UT-834 | M37/M15ak 删除 `current` 指针 | 源码 grep | 不再出现 `protocolRuntime.current`、`setCurrent`、`advanceCurrent`、`noteRealSuccess`、`base = start` |
| UT-835 | M38/M15ai 失败重置回退时间 | `TestFailureRetimesBackoff` | 实例 `open` 后推进时钟至 `degradedUntil` 前 1s 再失败：`degradedUntil` 重置为 `now + degrade_duration`，无指数增长；§24.19 后全 `open` 的两实例都会被重试并各自重新计时 |
| UT-836 | M39 失败后继续尝试下一个 `closed` | `TestLockRoutesEverythingAndUnlockResets`、`TestFallbackTemporaryAndSustained` | 一个候选失败即重试下一个 `closed` 候选；`open` 候选跳过；全部失败才 502 |
| UT-837 | M40/M15am 监控页无探测 | `TestMonitorPageHasNoProbeUI` | 页面源码不含 `probe-badge`、`probe-row`、`stat-probe`、`探测中`、`探测成功`、`探测失败`；状态列只会显示 `正常` / `降级 HH:MM:SS` / `已锁定` / `-` |
| UT-838 | M15an 请求不被探测阻塞 | `TestFirstClosedRouting`、`TestNoActiveProbing` | 源码无后台探测路径；`open` 实例不因内部行为收到请求，请求时延只取决于上游本身 |
| UT-839 | M15ao 锁定仍然生效 | `TestLockRoutesEverythingAndUnlockResets`、`TestLockedOpenInstanceStillServes` | 锁定后所有请求只发往被锁实例（即使它处于 `open`）；解锁后按第一个 `closed` 重新选路；锁定状态只存内存、不落盘 |
| UT-840 | M15ap 单实例协议 | `TestSingleInstanceProtocolNeverOpens` | 连续失败超过阈值仍恒为 `closed`、`degradedUntil` 为零；`requests`/`failed`/`recentErrors` 照常累加；无后台请求 |
| UT-841 | M15aq 构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race` | 全部通过 |

### 35.2 场景说明

- 新增用例全部在 `internal/proxy/circuit_internal_test.go`，沿用 `cbClock` 假时钟与 `cbBackend` 可控上游；`cbBackend` 已删除 `probeStatus`/`blockProbes`/`probeCalls`/`probeTimes` 等探测专用装置。
- 旧语义用例的处理：`TestFallbackAllDegradedUsesTail` 更名为 `TestFallbackAllDegradedUsesFirst` 并断言打链上第一个（§24.19 后又更名为 `TestFallbackAllOpenRetriesEveryInstance`，见 §37）；`TestFallbackSuccessResets` 的 `firstCalls` 期望由 5 改为 4、后续由 7 改为 6（探测请求不再发生）；`TestFallbackRecoveryProbeFailure` 更名为 `TestFallbackExpiredWindowRealRequestFails`；`TestStatsInstanceTokenColumns` 删除 `addProbe` 探测行夹具。其余断言保持不变。
- M15am 为前端源码级检查（无浏览器测试框架），由 `TestMonitorPageHasNoProbeUI` 读取 `go:embed` 的 `web.IndexHTML` 直接断言四个禁用字符串不存在。

## 36. 第十五轮：回退时间默认值改回 5 分钟（§24.18 M41–M44）

### 36.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-842 | M15as 配置缺省回退时间为 5m | `TestLoadAppliesDefaults`（`internal/config`） | 未显式填写 `fallback.degrade_duration` 时解析结果为 `5m`，同时等于 `config.DefaultDegradeDuration`；显式 `"1m"` 仍按 1m 生效（`TestFallbackExplicit` 断言不变，证明只改默认值、不改行为） |
| UT-843 | M15ar/M15at 示例与文档同步 | `config.example.json`、`README.md` | 示例配置为 `"5m"`；README 参数总览/示例片段/字段表均为 `5m`，且不含「同时是半开探测周期」「同时是统一半开探测周期」 |

### 36.2 场景说明

- UT-842 在既有的 `TestLoadAppliesDefaults` 中新增一条字面量断言 `5*time.Minute`；原先只与 `config.DefaultDegradeDuration` 比较，常量改错时测试会跟着一起错，字面量断言用于固定 §24.18 的默认值。
- 既有 `TestFallbackExplicit` 显式配置 `"1m"`，继续断言 1m 生效，覆盖 M44「显式值不受影响」。
- 本轮未改动任何运行时语义、校验规则与前端行为。

## 37. 第十六轮：全 open 时逐实例向后重试（§24.19 M45–M48）

### 37.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-844 | M15av/M46 全 `open` 逐实例向后重试 | `TestAllOpenRetriesEveryInstance` | 三实例全 `open`：第一个仍失败后请求继续打第二个并成功；第三个不被调用；第二个成功后恢复 `closed` |
| UT-845 | M15av-b 首实例成功不预发 | `TestAllOpenFirstSuccessNoPrefetch` | 三实例全 `open` 且第一个成功：只有第一个收到请求，第二、三个调用次数不变 |
| UT-846 | M15aw 全 `open` 全部失败 | `TestAllOpenAllFail` | 三实例全 `open` 且全部失败：返回 502，本次请求中三个实例各被调用恰好 1 次 |
| UT-847 | M15ax/M48 混合场景跳过 `open` | `TestMixedClosedAndOpenSkipsOpen` | 一实例 `open`、两实例 `closed`：请求只打两个 `closed` 候选，`open` 实例调用次数为 0 |
| UT-848 | M15ay 失败记账不跳过 | `TestAllOpenFailureBookkeeping` | 全 `open` 重试链上每个失败实例的 `degradedUntil` 均重置为 `now + degrade_duration`，调用次数各 +1 |
| UT-849 | M15az 构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race` | 全部通过 |

### 37.2 场景说明

- 新增用例全部在 `internal/proxy/circuit_internal_test.go`；公共装置 `cbAllOpenThree` 先用一条请求让三个失败实例全部转 `open`（`failure_threshold = 1`），再发全 `open` 请求验证重试链，避免每个用例重复铺陈。
- 旧用例处理：`TestAllOpenUsesFirst`（UT-833）的语义已被 §24.19 M47 撤订，用例删除，由 UT-844～UT-848 取代；`TestFailureRetimesBackoff`（UT-835）追加断言全 `open` 重试中第二个实例同样被调用并重新计时；`TestFallbackAllDegradedUsesFirst` 更名为 `TestFallbackAllOpenRetriesEveryInstance`（UT-407），第二次全 `open` 请求的期望由「首实例 +1、次实例 0 次追加调用」改为「两实例各 +1 次追加调用」。
- `attempts` 的构造与既有重试循环、`tracker.begin`/`tracker.retry` 记账未变，因此监控统计与请求日志的三档格式不受影响。

## 38. 第十七轮：上游非 2xx 分类处置（§24.20 M49–M55）

### 38.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-850 | M15ba 上游 400 换实例并记失败 | `TestUpstream400FailsOverAndRecordsFailure` | 首实例返回 400、响应头未提交：请求改打第二实例并得到 200；两实例各调用 1 次；首实例 `consecutiveFailures = 1` |
| UT-851 | M15bb 上游 500 换实例并记失败 | `TestUpstream500FailsOverAndRecordsFailure` | 与 UT-850 相同，首实例返回 500 |
| UT-852 | M15bc 候选耗尽转发原始 400 | `TestUpstream400AllCandidatesFailedForwardsOriginal` | 两实例都返回 400：客户端收到 400 与原始 body；`recentErrors[0]` 的 `status=400`、`error`/`upstreamBody` 非空、`instName` 为最后失败实例 |
| UT-853 | M15bd 已提交流不换实例 | `TestCommittedStreamIsNotRetried` | 首实例写出首个 SSE 块后断开连接：客户端收到已提交的 200，首实例调用 1 次、第二实例 0 次 |
| UT-854 | M15be 连续 5xx 打开熔断 | `TestUpstream5xxOpensBreakerAtThreshold` | 阈值 2、两实例持续 500：两次请求后首实例 `consecutiveFailures = 2` 且 `state = open` |
| UT-855 | M15bf 本地错误不计入实例 | `TestLocalAuthErrorDoesNotCountAgainstInstance` | 未带凭据请求返回 401：上游调用 0 次，所有实例失败计数 0、`degradedUntil` 为零 |
| UT-856 | M54 重试原因 `upstream_status` | `TestRetryReasonUpstreamStatus` | `retryReason(*upstreamResponseError)` 返回 `upstream_status`；`fallback_retry` 常量值正确；错误文本非空 |
| UT-857 | M15bg 构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race` | 全部通过 |

### 38.2 场景说明

- 新增用例全部位于 `internal/proxy/upstream_fallback_internal_test.go`（`package proxy`），复用 `circuit_internal_test.go` 的 `cbClock`、`cbBackend`、`cbOpenAIInstance`、`newCBHandler`、`cbSend`/`cbPath` 装置，并额外提供 `instFailureCount` 与 `sendProxyRequest`（可断言响应体）。
- UT-852 通过手工挂载 `newMonitorHub()` 让处理器记录统计，再从 `stats.snapshot` 读取 `recentErrors`；`upstreamBody` 字段由 `attemptMetrics` → `requestMonitor.end` → `statsAggregator.finish` 传递。
- UT-853 用 `panic(http.ErrAbortHandler)` 模拟首块之后连接中断；代理端 `copySSE` 判定 `committed=true`，因此第二实例不会被调用。

## 39. 第十八轮：全候选失败后的补漏尝试与错误透传（§24.21 M56–M62）

### 39.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-858 | M15bh 闭包失败后依次尝试开断实例 | `TestOpenInstancesTriedAfterClosedCandidates` | 5 实例（3 closed + 2 open）全部连接失败：5 个实例各被调用 1 次，两个 open 实例按配置顺序被调用 |
| UT-859 | M15bi 最后一个上游 400 透传 | `TestLastUpstream400ForwardedToClient` | 前两个实例失败（500 / 连接失败），开断的第三实例在补漏段返回 400：客户端收到 400 与原始 body，而不是 502；三者各调用 1 次 |
| UT-860 | M15bj 全连接失败仍 502 | `TestAllConnectionFailuresReturn502` | 全部实例连接失败：客户端收到 502 且 body 含 `upstream request failed` |
| UT-861 | M15bk 补漏使用实例映射模型 | `TestFallbackUsesInstanceMappedModel` | 首轮 sonnet 实例收到自身映射 `model-a`；链外 opus 实例在补漏段收到自身映射 `model-opus`，不是客户端原模型 `claude-sonnet-4-5` |
| UT-862 | M15bl 五实例每实例最多一次 | `TestFiveInstancesCalledAtMostOnce` | 5 实例协议（3 closed + 2 open）全部失败时 5 个实例各调用 1 次；5 个全 closed 全部失败时同样各调用 1 次（首轮已覆盖全部实例，补漏段为空） |
| UT-863 | M15bm 补漏失败仍记账 | `TestFallbackFailuresStillCount` | 补漏段实例失败计数 3→4；链外实例失败计数 0→1 |
| UT-864 | M15bn 构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race` | 全部通过 |

### 39.2 场景说明

- 旧用例处理：`TestOriginalModelRerunKeepsRequestBody`（原 M15bk「原模型重跑」）与 `TestFallbackStagesCallEachInstanceOnce`（原三实例）的语义已被改写后的 §24.21 M56–M62 推翻，分别由 UT-861/UT-862 取代；`TestLastUpstream400ForwardedToClient` 扩展为「最后一个实例在补漏段返回 400」。
- 补漏段只使用「首轮尚未调用过的实例」：`serveChain` 用 `seen` 集合去重；链内实例直接复用首轮候选（映射一致），链外实例用 `fallbackCandidate` 按自身 `roles`/`models` 映射。M15bl 的「每实例 ≤1 次」与 M62 的「首轮已覆盖全部实例时不产生补漏」同时成立。
- UT-861/UT-863 使用 Anthropic 角色链构造「链内实例 + 链外实例」：请求 `claude-sonnet-4-5` 只把声明 `sonnet` 的实例放进链；声明 `opus` 的实例在补漏段以自身映射（`model-opus`/`model-c`）被尝试，从而可机械区分「首轮映射」与「补漏映射」，并证明补漏没有回退到客户端原始模型名。
- UT-858/UT-862 用 `handler.recordFailure` 预先把实例置为 `open`（阈值 1），避免依赖请求顺序制造状态。
- 错误透传：只要任一候选拿到过真实非 2xx 响应，最终失败就转发最后一条（UT-859）；全部停在连接失败/超时才回退到 502（UT-860）。

## 40. 第十九轮：失败请求的客户端请求体取证（§24.22 M63–M69）

### 40.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-865 | M15bo 失败请求记录 `reqShape` 且条数一致 | `TestReqShapeAnthropicFormat` | 4 条消息的 Anthropic 请求返回上游 400：`recentErrors[0].reqShape` 等于 `msgs=4 \| 0:user[text] \| 1:assistant[thinking,text,tool_use] \| 2:user[tool_result] \| 3:user[text]`，且不含任何消息正文 |
| UT-866 | M15bp 缺 `thinking` 的助手消息带标记 | `TestReqShapeToolUseWithoutThinking` | Anthropic `[text,tool_use]` 与 OpenAI `tool_calls`（content 为 null）两种消息均输出 `...]!!tool_use_without_thinking`，标记紧跟在 `]` 之后 |
| UT-867 | M15bq 同时含 `tool_use` 与 `thinking` 不带标记 | `TestReqShapeToolUseWithThinking` | Anthropic `[thinking,text,tool_use]`、OpenAI `reasoning_content`+`tool_calls`、`reasoning`+`tool_calls` 均不含标记 |
| UT-868 | M15br 大请求体仍 ≤ 1024 字符 | `TestReqShapeLargeBodyLimit` | 1 MiB / 20 条消息的 `reqShape` 字节数与 rune 数均 ≤ 1024 且以 `msgs=20` 开头；400 条消息时被截断并以 `…` 结尾 |
| UT-869 | M15bs 非 JSON 不改变转发且记 `unparsable:` | `TestReqShapeUnparsable` | 请求体 `not-json` 原样到达上游、客户端仍收到上游 400；`recentErrors[0].reqShape` 为 `unparsable: not-json`；无 `messages` 的合法 JSON 同样走 `unparsable:` |
| UT-870 | M15bt 成功请求不出现 `reqShape` | `TestReqShapeSuccessNotRecorded` | 200 请求后 `recentErrors` 为空，监控快照记录的 `ReqShape` 为空且序列化后不含 `reqShape` 键 |
| UT-871 | M15bu 取证不改变转发字节 | `TestReqShapeDoesNotChangeForwardedBody` | `buildReqShape` 不修改入参切片；无需改写时上游收到逐字节相同的客户端 body，需改写时上游收到与 `rewriteModel` 输出完全一致的 body |
| UT-872 | M15bv 构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race` | 全部通过 |

### 40.2 场景说明

- 新增用例全部位于 `internal/proxy/req_shape_internal_test.go`（`package proxy`），复用 `circuit_internal_test.go` 的 `cbClock`、`cbBackend`、`cbOpenAIInstance`、`cbAnthropicInstance`、`newCBHandler` 与 `upstream_fallback_internal_test.go` 的 `sendProxyRequest`。
- UT-865/UT-869/UT-870 通过手工挂载 `newMonitorHub()` 让处理器记录统计，再从 `stats.snapshot` 读取 `recentErrors`、从 `snapshotRequests()` 读取监控终态快照。
- UT-868 用 `encoding/json` 生成 20 条各含 55 KiB 正文的消息（> 1 MiB），断言摘要长度与 `msgs=20` 前缀；再用 400 条消息触发截断分支，断言 `…` 结尾。
- 格式与标记断言采用全等比较（UT-865～UT-867），因此 `msgs=` 前缀、` | ` 分隔符、块类型逗号分隔、`!!tool_use_without_thinking` 的位置均被机械校验；UT-865 额外断言摘要不含构造请求中的任何正文片段。
- `reqShape` 只在下发给失败请求的 `statsError`、`monitorRequestSnapshot`、`monitorEndEvent` 上出现，三者均为 `string` + `omitempty`；UT-870 与 `TestReqShapeJSONField` 共同验证空值不序列化。

## 41. 第二十轮：流式无内容超时与阶段日志（§24.23 M70–M80）

### 41.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-873 | M15bw Anthropic 只发心跳被终止并写 `pause_turn` | `TestContentIdleTimeoutAnthropic` | 实例 `timeout=200ms`、`idle_timeout=5s`：上游先发 `message_start`/`content_block_start`/`content_block_stop`、随后每 10ms 只发 `event: ping`；请求在 3s 内结束，返回 200 且含 `"stop_reason":"pause_turn"` 与 `message_stop`，实例 `consecutiveFailures == 1` |
| UT-874 | M15bw OpenAI 只发空 delta 被截断 | `TestContentIdleTimeoutOpenAI` | 实例 `timeout=200ms`：上游每 10ms 发一个空 `delta` chunk；客户端读到连接截断错误、body 不含 `[DONE]`，实例失败计数 +1 |
| UT-875 | M15bz 块边界事件不重置无内容定时器 | `TestContentIdleIgnoresBoundaryEvents` | 上游每 10ms 重复 `message_start`/`content_block_start`/`content_block_stop` 且无 delta；200ms 实例超时后仍以 `pause_turn` 终止 |
| UT-876 | M15bx 正常内容流不被误杀且字节一致 | `TestContentIdleContentKeepsStreamAlive` | 10 组 ping+`content_block_delta`（间隔 20ms）、实例 `timeout=150ms`：响应体与上游写入**逐字节相同**、无 `pause_turn`、失败计数 0，阶段摘要 `content=10` |
| UT-877 | M15by 静默上游仍由 `first_token_timeout` 处置 | `TestSilentUpstreamFirstTokenTimeoutUnchanged` | `first_token_timeout=150ms`、实例 timeout 5s：单候选请求在 3s 内返回 502，失败计数 1，日志含 `timeout: first-token for 150ms` 且不含 `content-idle`；摘要 `first_block=-`、`first_content=-` |
| UT-878 | M15ca 无内容超时与 idle 的客户端字节相同 | `TestContentIdleMatchesIdleClientBytes` | Anthropic 与 OpenAI 各自用同一上游前缀比较两条路径：Anthropic 的 `pause_turn` 尾巴逐字节相同；OpenAI 均以截断连接结束且已收到字节相同 |
| UT-879 | M15cb/M15cc 每流一行摘要、非流式不打印 | `TestStreamPhaseSummaryOncePerStream` | 一个 SSE 请求 + 一个 JSON 请求：`stream phase-probe hdr=` 恰好出现 1 次；字段顺序为 `hdr=`、`first_block=`、`first_content=`、`blocks=`、`pings=`、`content=`、`last_gap=`，且 `content=2`、`pings=1`；JSON 响应体逐字节等于上游输出 |
| UT-880 | M15cf 构建与全量测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race -count=1` | 全部通过 |

### 41.2 场景说明

- 新增用例全部位于 `internal/proxy/stream_content_idle_internal_test.go`（`package proxy`），使用 `NewWithOptions` 直接构造处理器，实例名用于断言阶段行与原因行。
- UT-873/UT-875/UT-876/UT-878 的上游在写完前缀后持续发心跳并阻塞，直到代理关闭响应体；因此断言的是「定时器真的触发」而不是上游主动 EOF。
- UT-878 复用同一段上游字节，仅通过「实例 timeout 100ms + idle 5s」与「idle 100ms + 实例 timeout 5s」互换，证明两条路径落到同一个处置分支。
- `TestRealtimeSlowAnthropicPauseTurn` 与 `TestRealtimeSlowOpenAINotInterrupted` 的实例 `timeout` 从 5s 调整为 30s：这两个 fixture 故意只发注释块（无内容），按 §24.23 会在实例 timeout 处被终止；提高到 30s 后 10s 慢窗口仍先到，断言未改动。
- 既有测试的断言未改动；唯一签名适配是 `TestStreamingCopyStillFlushes` 的 `copySSE` 调用新增了一个 `contentIdle=0` 实参（禁用）。

## 42. 第二十一轮：流式空 signature 归一化（§24.24 M81–M91）

### 42.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-881 | M15cg thinking、`signature:""` 归一化 | `TestSignatureNormalizeEmptySignature` | 整包与 7 字节分片两种上游写法下，客户端 body 的 `signature` 均为 `model-router-placeholder`，其余事件逐字节不变；CRLF 行尾同样保留 |
| UT-882 | M15cp thinking、`signature` 缺失 | `TestSignatureNormalizeMissingSignature` | 缺失字段被插入为 `"signature":"model-router-placeholder"` 且位于 `content_block` 开头；对象内既有空白与字段顺序保留 |
| UT-883 | M15ch thinking、`signature` 非空 | `TestSignatureNormalizeNonEmptySignature` | 客户端 body 与上游 payload 逐字节相同 |
| UT-884 | M15ci `content_block_start` 非 thinking | `TestSignatureNormalizeNonThinkingStart` | `text` / `redacted_thinking` 块（即使带空 `signature`）全部逐字节不变 |
| UT-885 | M15cj 其它事件 | `TestSignatureNormalizeOtherEvents` | `message_start`、`ping`、`signature_delta`、`content_block_stop`、`message_stop` 全部逐字节不变 |
| UT-886 | M15ck 不可解析/多行 data/超长/半行 | `TestSignatureNormalizeUnparsablePassthrough` | 非 JSON、多行 `data:`、超 32 KiB 事件原样透传且后续 thinking 事件仍被归一化；末尾未闭合事件逐字节补写 |
| UT-887 | M15cl usage/token 统计不受改写影响 | `TestSignatureNormalizeStatsUnaffected` | 空签名与预签名两份同构流的 `sseState` 字段一致（in=11/out=22）；处理器统计 `InTokens/OutTokens = 11/22` |
| UT-888 | M15cm 日志每请求最多一行 | `TestSignatureNormalizeLogsOnce` | 一次请求改写 2 个块只出现 1 行 `thinking signature normalized: blocks=2 inst=sig-inst`；无改写时该行不出现 |
| UT-889 | M15co OpenAI 流式 / Anthropic 非流式不受影响 | `TestSignatureNormalizeOpenAIAndNonStreamUnaffected` | OpenAI SSE（即使含 Anthropic 形状事件）与 Anthropic JSON 响应均与上游逐字节相同 |
| UT-890 | 构建与全量测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race -count=1` | 全部通过 |

### 42.2 场景说明

- 新增用例全部位于 `internal/proxy/signature_normalize_internal_test.go`（`package proxy`），复用 `stream_content_idle_internal_test.go` 的 `newStreamHandler`、`streamAnthropicInstance`、`streamOpenAIInstance`、`doStreamRequest`、`captureStreamLogs` 与 `flush_internal_test.go` 的 `newFlushRecorder`。
- UT-881 的 7 字节分片上游每片 `Flush` 并间隔 2ms，确保 `content_block_start` 事件跨多次 `copySSE` 读取；UT-886 的 80 KiB 事件强制走 `maxSignatureEventBytes` 的透传分支并验证下一事件恢复归一化。
- UT-887 直接调用 `copySSE` 比较同一 payload 在空签名/预签名两种情况下的 `sseState`，再从挂载 `newMonitorHub()` 的处理器读取累计 token；tps 的输入 `outTokens/genDuration` 同源于该 state。
- 唯一既有测试适配：`flush_internal_test.go` 的 `copySSE` 调用新增 `instName` 与 `normalizeSignature=false` 实参，断言未改。

## 43. 第二十二轮：锁定豁免、无内容转交与映射统一（§24.25 M92–M102）

### 43.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-891 | M15cq/M15cr 锁定流超时豁免与日志 | `TestLockedStreamExemptFromTimeouts` | 锁定实例 `timeout=50ms`，上游每 20ms 发一个 `ping`（共 20 个）且无内容；请求耗时 ≥250ms（>3× timeout），客户端 body 与上游字节**完全相同**、不含 `pause_turn`；日志含 `instance locked-inst locked; stream not interrupted`，不含 `timeout: content-idle` |
| UT-892 | M15ct 锁定实例连接层无响应头超时 | `TestLockedNoResponseHeaderTimeout` | 上游接受连接后阻塞 300ms（远超 50ms 实例 timeout）再回响应头：网关在 300ms 内不返回、不主动断开；日志无 `timeout awaiting response headers`、无 `instance locked-inst timeout`；释放后客户端拿到 200 |
| UT-893 | M15cs/M15cu 锁定失败不计数且继续走其余实例 | `TestLockedFailureNotCountedAndFallsThrough` | 锁定实例返回 400、其余实例返回 200：客户端拿到 200 与其余实例响应；调用次数 1/1；锁定实例 `consecutiveFailures=0`、`degradedUntil` 为零 |
| UT-894 | M15da 全部失败时透传最后一次真实响应 | `TestLockedFailureAllPeersFailForwardsLast` | 锁定实例与其余实例均返回 400：客户端收到锁定实例最后一次 400 与原始响应体；调用次数 2/1（锁定、其余、锁定最终重试）；锁定实例失败数 0、其余实例失败数 1 |
| UT-895 | M15cv 锁定成功时其余实例零调用 | `TestLockedSuccessSkipsPeers` | 锁定实例 200、其余实例 200：客户端拿到锁定实例响应；调用次数 1/0 |
| UT-896 | M15cw 解锁清零且锁定期间失败不追溯 | `TestUnlockClearsLockedPeriodCounters` | 阈值 1：锁定实例 400 不计数、其余实例 400 计 1；`unlock()` 后两个实例的 `consecutiveFailures` 与 `degradedUntil` 均归零 |
| UT-897 | M15cx 锁定场景使用映射模型名 | `TestLockedUsesMappedModel` | 锁定实例 `roles={"sonnet":"deepseek-flash"}`，客户端发 `claude-sonnet-5`：上游收到的 body 中 `model` 为 `deepseek-flash` |
| UT-898 | M15cy 锁定与补漏映射完全一致 | `TestLockedAndFallbackMappingAgree` | 12 个子用例：`*`/`sonnet`/`fable`/`opus`/`haiku` 五种角色 × 有无能力申明，另加精确模型与无映射；`lockedCandidate` 与 `fallbackCandidate` 的 model/rewrite 相同；无映射时 `fallbackCandidate` 为 nil、`lockedCandidate` 保持原模型名且 `rewrite=false` |
| UT-899 | M15db/M15dc 无内容立即转交且不依赖超时 | `TestLockedHandoffBeforeContent` | 锁定实例首次 400、第二次返回完整内容流；非锁定实例只发 `message_start`+`ping`，其 `timeout=10min`：网关立即关闭该上游（请求耗时 <2s），客户端只收到锁定实例的完整 payload；调用次数 2/1；非锁定上游请求 context 被取消 |
| UT-900 | M94d 转交只发生一次 | `TestLockedHandoffHappensOnce` | 锁定实例两次均 400；非锁定实例只发 `ping`：流程为锁定 400 → 非锁定中断 → 锁定最终 400，客户端收到最后一次 400；调用次数 2/1，不再回到非锁定实例 |
| UT-901 | M15dg/M94a 非内容事件不算有 token | `TestLockedHandoffIgnoresNonContentEvents` | 6 个子用例：`ping`、`message_start`、`content_block_start`、空 `text_delta`、空 `thinking_delta`、`signature_delta`；每种都触发转交，客户端拿到锁定实例 payload，调用次数 2/1，非锁定上游被取消 |
| UT-902 | M15dd/M94 已产出内容后不中断 | `TestLockedHandoffAfterContentDoesNotInterrupt` | 锁定实例首次 400；非锁定实例先发一个非空 `text_delta`，100ms 后再发 `ping`/`message_stop`：不转交，客户端收到非锁定实例完整流；调用次数 1/1 |
| UT-903 | M15de 非锁定实例首字节失败后改发锁定实例 | `TestLockedHandoffOnPeerFirstByteFailure` | 锁定实例首次 400、第二次返回完整内容流；非锁定实例返回 400：客户端只收到锁定实例的完整响应，无重复响应头/半截字节；调用次数 2/1 |
| UT-904 | M15df 未锁定路径逐字节不变 | `TestNoLockStreamBytesUnchanged` | 无锁定实例时，`message_start`/`content_block_start`/非空 `text_delta`/`ping`/`message_stop` 组成的流与上游 payload 全等 |
| UT-905 | M15cz 既有锁定测试按新语义调整 | `TestLockRoutesEverythingAndUnlockResets` | 锁定实例 400 → 其余实例 200（不再 502）；锁定实例失败数 0、`degradedUntil` 零值；移动锁定后两者均失败 → 调用 3/3（锁定、其余、锁定最终重试）、锁定失败数 0、其余失败数 1；解锁后全部清零；解锁后首实例失败、次实例成功，调用 4/4 |
| UT-906 | M15cz 构建与全量测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race -count=1` | 全部通过 |

### 43.2 场景说明

- 新增用例全部位于 `internal/proxy/locked_exempt_internal_test.go`（`package proxy`），复用 `stream_content_idle_internal_test.go` 的 `newStreamHandler`、`doStreamRequest`、`captureStreamLogs`；`lxPacedSSE` 按事件间隔写出，用于验证超过实例 timeout 的锁定流；`newLXJSONBackend` 记录上游收到的 body。
- UT-891 的 20 个 ping 每 20ms 一个，总时长约 400ms，明显超过 3×50ms；断言 `elapsed ≥ 250ms` 与完整字节，证明不是提前 EOF。
- UT-892 先用 channel 确认上游已接受请求，再等待 300ms 断言客户端仍未返回；之后释放上游并断言 200，排除「网关直接断开」。
- UT-899/UT-900/UT-901 使用 `lxRunNoContentHandoff`：锁定实例第一次返回 400、第二次成功，非锁定实例只发非内容事件并阻塞；通过上游请求 context 的 `Done` 断言非锁定上游被提前关闭。
- UT-899 的非锁定实例 `timeout=10min`，同时断言请求总耗时 <2s，证明转交不依赖实例超时（M15dc）。
- UT-898 不启动网络，直接构造 `instanceRuntime` 并比较 `lockedCandidate`/`fallbackCandidate` 的 `model`/`rewrite`；能力申明只影响 `capabilities`，不参与映射选择。
- UT-905 是既有用例 `TestLockRoutesEverythingAndUnlockResets` 的语义调整：旧断言「锁定失败 → 502」按 M93/M96 改为「继续走其余实例」；锁定期间非锁定实例失败会按 M92 计数（其余实例 +1）；两者都失败时锁定实例获得一次最终重试（M15de），因此调用次数由 1/2 调整为 3/3；解锁后的调用次数相应为 4/4。其余断言（状态显示、解锁清零、解锁后从首实例恢复路由）未改。

## 44. 第二十三轮：thinking 补位、响应头前总预算与诊断埋点（§24.26 M103–M120）

### 44.1 用例

| 编号 | 检查项 | 测试 | 判定 |
|---|---|---|---|
| UT-907 | M15dh/M15dk/M15dl 注入位置、index 与 usage | `TestThinkingInjectionAddsBlock` | 三个子用例：有 `message_delta` 时注入块位于其前、仅有 `message_stop` 时位于其前、多个块 index 0/2 时注入 index=3；删除注入字节后与上游 payload 全等；`sseState.tokens=7` |
| UT-908 | M15di/M15dj 已有 thinking 时不注入 | `TestThinkingInjectionSkipped` | thinking+tool_use、仅 thinking、仅 text 三种流客户端字节与上游全等，`outcome.injected=false` |
| UT-909 | M15dm 非流式不注入 | `TestThinkingInjectionSkippedNonStreaming` | Anthropic JSON 响应含 tool_use 无 thinking：客户端 body 与上游全等，日志无 `thinking block injected:` |
| UT-910 | M15dn parse=false 不注入 | `TestThinkingInjectionSkippedUnparseable` | 上游 `Content-Encoding: br` 且 `Content-Type: text/event-stream`：客户端收到原始字节，日志无注入 |
| UT-911 | M15do 注入日志每尝试一行 | `TestThinkingInjectionLogOnce` | 3 个 tool_use 块的流只打印 1 行 `thinking block injected: inst=ti-inst tool_use=3 index=3`；不注入的流不打印 |
| UT-912 | M113 注入与签名归一化互相独立 | `TestThinkingInjectionIndependentOfNormalization` | normalize=true/inject=false 只出现 placeholder、不注入；normalize=false/inject=true 正常注入且注入块 signature 为空串 |
| UT-913 | M15dv signature_empty 口径 | `TestRespShapeSignatureEmpty` | 空 start + 空 delta → `signature_empty=1`；空 start + 非空 delta → 0；非空 start + 空 delta → 0；tool_use-only → `injected=1` |
| UT-914 | M15dp 小 body 不受预算影响 | `TestHeaderBudgetFastRequestUnaffected` | 实例 timeout 2s、小请求 200 且总耗时 <500ms，日志无 `hdrprof` |
| UT-915 | M15dq 慢上传触发预算 | `TestHeaderBudgetExceeded` | 上游只 accept 不读 body、实例 timeout 200ms、2 MiB 请求体：客户端收到 502，日志含 `response header budget exceeded`，实例失败计数 1 |
| UT-916 | M15dr 锁定实例不施加预算 | `TestHeaderBudgetLockedExempt` | 同一慢上传场景锁定实例 timeout 200ms：600ms 后网关仍未返回；释放上游后请求结束，日志不含预算错误 |
| UT-917 | M15ds 预算失败后换候选 | `TestHeaderBudgetFallsThrough` | 慢上传实例失败后第二个实例返回 200；日志含预算错误，第一实例失败 1、第二实例调用 1 |
| UT-918 | M15dw hdrprof 字段与触发条件 | `TestHeaderProfileSlowHeaders` | 上游延迟 400ms 回响应头、实例 timeout 600ms：打印 `hdrprof inst=slow-hdr`，含 `connect=`/`tls=`/`upload=`/`server_wait=`/`body_bytes=`/`reused=`/`remote=`/`budget=600ms` 且 server_wait 已测量；快速请求不打印 |
| UT-919 | M15dt/M15du/M15dx 请求形态日志与会话哈希 | `TestReqShapeDiagnosticsLog` | 成功请求（thinking+redacted_thinking+未配对）打印 2 次同一行：`reqshape msgs=5 tool_use_msgs=3 paired_msgs=2 unpaired_idx=3 thinking=absent last=tool_result session=<sha256 前 8 位>`；`recentErrors` 为空 |
| UT-920 | M15dx session 缺失/非 JSON | `TestReqShapeSessionMissing` | 无 metadata、user_id 非 JSON、内层无 session_id 三种情况均打印 `session=-` |
| UT-921 | M15dt redacted 计入 paired 与 unpaired 截断 | `TestReqShapeRedactedPairedAndUnpairedCap`、`TestReqShapeCountSuffix`、`TestReqShapeCountSuffixBeyondTruncation` | redacted_thinking+tool_use → `paired_msgs=1 unpaired_idx=-` 且无 `!!`；12 个未配对 → `unpaired_idx=1,2,3,4,5,6,7,8,9,10,+`；40 个交替未配对 → `1,3,...,19,+` |
| UT-922 | M15dy 无 tool_use 请求不变 | `TestNoToolUseRequestUnchanged` | 普通 SSE 请求客户端字节与上游全等，无 `reqshape`/注入日志，`recentErrors` 为空 |
| UT-923 | 回归：全量构建与测试 | `go build ./...`、`go vet ./...`、`gofmt -l .`、`go test ./... -race -count=1` | 全部通过；既有 stats/WS 结构测试未改 |

### 44.2 场景说明

- 注入用例位于 `internal/proxy/thinking_inject_internal_test.go`：`tiRun` 直接调用 `copySSE`（`normalize/inject` 单独开关），用 `newFlushRecorder` 捕获客户端字节并从 `sseOutcome` 读取 `state.tokens`、块计数与 `injected`；端到端用例复用 `newStreamHandler`/`doStreamRequest`。
- 预算与 `hdrprof` 用例位于 `internal/proxy/header_budget_internal_test.go`：`newSlowUploadListener` 是只 `Accept` 不读 body 的裸 TCP listener（`SetReadBuffer(4096)`），用 2 MiB 请求体确保上传阻塞；预算测试用实例 timeout 200ms，锁定用例在 600ms 后断言网关仍未返回。
- 诊断用例位于 `internal/proxy/reqshape_diag_internal_test.go`：挂载 `newMonitorHub()` 以断言成功请求不进入 `recentErrors`；会话哈希在测试内用 `sha256` 复算。
- 既有断言按 M114 改为 `tool_use_msgs`/`paired_msgs`/`unpaired_idx`；`TestReqShapeToolUseWithoutThinking` 等块级标记用例未改语义，仅 redacted 块不再产生标记。
- 既有 `copySSE` 调用新增 `injectThinking=false` 实参；`flush_internal_test.go` 与 `signature_normalize_internal_test.go` 只做签名适配，断言未改。
