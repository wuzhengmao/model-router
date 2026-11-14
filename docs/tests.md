# model-router 测试用例（M1 ～ M3.1）

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

## 4. `/v1/models`（`internal/proxy`）

| 编号 | 用例 | 入口 | 覆盖点 | 预期 |
|---|---|---|---|---|
| UT-121 | OpenAI 格式 | `TestModelsOpenAI` | 跨 openai 实例 models 顺序/去重；排除 anthropic；`object/owned_by/created` | 列表正确，上游 0 调用 |
| UT-122 | Anthropic 格式 | `TestModelsAnthropic` | 具体 roles 值（不含 `*`）去重；`data/type/display_name/created_at/has_more/first_id/last_id` | 格式正确，`*` 值不出现 |
| UT-123 | 认证与方法 | `TestModelsAuthAndMethod` | 无/错 Bearer、错 x-api-key、POST | 401 按协议；405 + `Allow: GET` |

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
- `-version` 输出 `0.0.1-rc`，产物位于 `dist/`。

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
| `/v1/models` 双协议与过滤去重 | UT-121、UT-122、UT-123 |
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
| UT-405 | 回退期探测失败重新计时 | `TestFallbackRecoveryProbeFailure` | 回退期结束后主实例探测再失败 | 立即重新进入回退，后续请求跳过 |
| UT-406 | 成功恢复清零 | `TestFallbackSuccessResets` | 回退期后主实例成功 | 计数/回退清零，之后失败重新按 3 次累计 |
| UT-407 | 全降级强制链头 | `TestFallbackAllDegradedForcesHead` | 阈值 1、两实例均失败后全部降级 | 后续请求只强制尝试链头一次，其余不试，返回 502 |
| UT-408 | 并发安全 | `TestFallbackConcurrent`（`go test -race`） | 20 并发请求 + 失败实例 | 全部 200，调用次数一致，无 data race |
| UT-409 | 吞吐判定 | `TestThroughputFailure` | OpenAI `completion_tokens`、Anthropic `output_tokens`、无 usage；慢响应 | 低吞吐响应仍透传但记录失败（下次跳过）；无 usage 不判失败 |
| UT-413 | Anthropic 空闲超时收尾 | `TestIdleTimeoutAnthropicPauseTurn` | mock 上游发首块后静默；`idle_timeout=50ms` | 客户端收到 `content_block_stop`、`message_delta(stop_reason=pause_turn)`、`message_stop` |
| UT-414 | OpenAI 空闲超时截断 | `TestIdleTimeoutOpenAITruncates` | mock 上游发首块后静默 | 客户端读取报流截断错误、无 `[DONE]`；实例记失败（后续走备用实例） |
| UT-415 | OpenAI 慢速流不打断 | `TestSlowStreamIsNotInterrupted` | mock 持续慢速输出、低 usage | 流完整结束（含 `[DONE]`），仅记失败，下次请求跳过 |
| UT-416 | 配置 idle_timeout 生效 | `TestIdleTimeoutFromConfig` | JSON `fallback.idle_timeout="50ms"` + 静默 anthropic 上游 | 收到 pause_turn 收尾，usage 复用已见 output_tokens |
| UT-410 | 配置阈值生效 | `TestFallbackFromConfigThreshold` | `fallback.failure_threshold=2`（JSON 配置） | 连续失败 2 次后持续回退 |
| UT-411 | 配置回退时长生效 | `TestFallbackFromConfigDegradeDuration` | `degrade_duration="100ms"` | 回退期结束后重新探测主实例 |
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
| 状态机：回退期结束恢复探测失败重新计时 | UT-405 |
| 状态机：成功恢复清零 | UT-406 |
| 状态机：全部降级强制链头 | UT-407 |
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
