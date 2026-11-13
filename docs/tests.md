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
| UT-001 | 默认值与归一化 | `TestLoadAppliesDefaults` | `listen`/`timeout` 默认、protocol 小写、base_url 去尾斜杠、api_key trim、models 去空去重保序 | 字段归一化正确，openai 无 roles |
| UT-002 | 实例顺序与 timeout | `TestLoadPreservesInstanceOrder` | 两个实例顺序、20s/1m | 顺序与 timeout 保留 |
| UT-003 | anthropic roles 归一化 | `TestLoadNormalizesAnthropicRoles` | 键大小写/空白、值 trim、`*` | map 键小写、值 trim |
| UT-004 | Duration 序列化往返 | `TestDurationJSONRoundTrip` | `MarshalJSON` 输出 `"20s"`/默认 `"15s"`，再 Parse | 往返时长一致 |
| UT-005 | admin 默认与 trim | `TestLoadAdminDefaults` | `admin_user` 默认/trim、`admin_password` trim | 符合 §8.1 |
| UT-006 | 原子保存往返 | `TestSaveRoundTrip` | Save → Load（openai + anthropic 实例） | 全字段一致 |
| UT-007 | 空 instances 拒绝 | `TestLoadRejectsEmptyInstances` | `instances: []` | Load 报错 |
| UT-008 | 缺 auth_token 拒绝 | `TestLoadRejectsMissingAuthToken` | fail-closed | Load 报错 |
| UT-009 | 未知字段拒绝 | `TestLoadRejectsUnknownField` | 顶层拼写错误 | Load 报错 |
| UT-010 | 旧 backends 迁移提示 | `TestLoadRejectsLegacyBackends` | 出现 `backends` | 报错且包含 `instances` 提示 |
| UT-011 | 非法实例拒绝 | `TestLoadRejectsInvalidInstance` | 14 个子用例：缺/非法 protocol、缺/相对 base_url、缺 api_key、openai 缺/空 models、openai 带 roles、anthropic 缺 roles、anthropic 带 models、非法角色键、空角色值、非法 timeout、未知 provider | 全部报错 |
| UT-016 | `provider` 校验 | `TestLoadProvider` | 已知 provider、空 provider、未知 provider | 已知/空通过（已知归一化），未知报错 |
| UT-012 | 示例配置可解析 | `TestExampleConfigIsValid` | `config.example.json` | Parse 成功 |
| UT-013 | 环境变量覆盖 | `TestEnvOverridesConfig` | 四个 `MODEL_ROUTER_*`（含空格）覆盖文件值 | 生效值为 env（trim 后），instances 不变 |
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
| `config.example.json` 为 instances 结构且可解析 | UT-012 |
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
