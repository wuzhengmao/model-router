# Changelog

本文件记录项目的重要变更，遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的结构与语义化版本号规则。

## [0.0.1-rc] - 2026-11-13

### Added

- M1 项目骨架：`main.go` + `internal/config`、`internal/proxy`、`internal/version`。
- 单模型 OpenAI 兼容透传：入口 `POST /v1/chat/completions`，请求方法/路径/查询串/关键请求头/请求体原样转发，后端状态码/响应头/响应体原样回传；SSE 流式响应逐块刷新不缓冲。
- 静态 JSON 配置：`listen`、`auth_token`、`backend.base_url`、`backend.api_key`、`backend.model`、`backend.timeout`（默认 15s），严格解析并在启动时校验。
- 服务访问认证（FR-4）：新增必填配置 `auth_token`；客户端访问必须携带 `Authorization: Bearer <auth_token>`，缺失或不匹配返回 `401`，错误体为 OpenAI 兼容格式（`error.message` / `error.type` / `error.code`）；token 比较使用 `crypto/subtle.ConstantTimeCompare` 防时序侧信道。
- 上游认证与客户端认证分离：转发上游时 `Authorization` 固定替换为 `Bearer <backend.api_key>`，客户端服务 token 不再透传；其余请求头仍原样透传。
- 版本号单一来源 `internal/version/version.txt`，构建时通过 `-ldflags -X` 注入，`-version` 输出 `0.0.1-rc`。
- 一键构建脚本 `build.sh`：构建当前平台（windows/amd64）可执行文件，`CGO_ENABLED=0`，产物输出到 `dist/`。
- 单元测试：透传（方法/路径/查询串/请求头/请求体/响应）、认证（缺失 token 401、错误 token 401、正确 token 转发且上游使用配置 api_key）、未知路径 404、配置解析与校验。
- M2 Anthropic 协议入口 `POST /v1/messages`：上游 URL 为 `base_url + /v1/messages`，上游认证使用 `x-api-key: <backend.api_key>`；请求体/响应体原样透传，SSE 逐块 flush。
- M2 多后端有序列表：`backends` 数组 + `protocol`（`openai` / `anthropic`，大小写不敏感、归一化小写），同协议内顺序即优先级；`backends` 为空、协议非法、必填字段缺失均启动失败。
- M2 按 model 路由（R1～R7）：请求体顶层 `model` 字符串命中同协议某后端 `model` 时转发到该后端；否则转发到该协议列表首个后端；非法 JSON 不阻断，按首后端透传；该协议无后端返回 502。
- M2 模型别名映射（§7.7，R4a/R4b）：`backends[].matches` 可选别名列表（去首尾空白、忽略空项）；按 `backends` 顺序逐项判断，先判 `model` 精确命中（R4a，不改写），再判 `matches` 命中（R4b，把请求体顶层 `model` 改写为该后端 `model`，其余字段语义不变）；未命中回退首后端且不改写。
- M2 Anthropic 入口客户端认证修订（FR-4）：/v1/messages 使用 `x-api-key: <auth_token>`（与官方 SDK 一致），不再接受 `Authorization: Bearer`；OpenAI 入口仍使用 `Authorization: Bearer <auth_token>`。两者均使用 `crypto/subtle.ConstantTimeCompare`。
- M2 请求体上限 10 MiB（`http.MaxBytesReader`），超限返回 413 且不转发；为路由将请求体读入内存，除别名命中（R4b）外转发字节保持原样。
- M2 按入口协议返回错误体：Anthropic 入口为 `{"type":"error","error":{"type":...,"message":...}}`（401 `authentication_error`、404 `not_found_error`、502 `api_error`、413 `request_too_large`）；OpenAI 入口保持 M1 结构并新增 413 `request_too_large`。
- M2 单元测试：双协议透传（含 `anthropic-version`/`anthropic-beta`/`x-api-key` 断言）、按协议认证（Anthropic 拒绝 Bearer、OpenAI 拒绝 x-api-key）、按 model/别名路由与回退、别名改写 body 断言（改写前后语义对比）、协议列表隔离、无后端 502、请求体超限 413、上游失败错误体格式。
- config.example.json 的 anthropic 项改为 DeepSeek Anthropic 兼容入口（`https://api.deepseek.com/anthropic`）并新增 `matches` 示例。
- M2 模型角色映射（§7.8）：`backends[].roles` 可选，枚举 `sonnet`/`opus`/`fable`/`haiku`/`*`（大小写不敏感、归一化小写），非法值启动失败，空数组视为未配置。
- M2 角色路由（R4c）：请求体 `model` 精确等于角色名，或以 `claude-<role>-` 开头时，转发对应后端并把顶层 `model` 改写为该后端 `model`（复用别名改写机制）。
- M2 `*` 通配托底（R4d）：具体角色/别名均未命中且存在声明 `*` 的后端时，转发到第一个声明 `*` 的后端并改写；无 `model`/`null`/非字符串不触发托底，仍按 R5 回退 L[0] 且不改写。
- M2 角色相关测试：配置归一化/非法值拒绝/空数组，R4c 精确与前缀命中及改写、R4a/R4b 优先于 R4c、R4c 优先于 R4d、多后端同角色取首个、`roles` 与 `matches` 共存、无 model/null/非字符串不触发 `*`。
- config.example.json 为 anthropic flash/pro 后端补充 `roles`（`["sonnet","haiku"]` / `["opus","fable"]`），并新增一个 `roles: ["*"]` 的托底后端示例。
- M3 `GET /v1/models`：本地聚合配置声明的 OpenAI `models` 与 Anthropic 具体角色值（不含 `*`）并按出现顺序去重；按认证头判定协议，返回 OpenAI（`object:list`）或 Anthropic（`data`/`has_more`/`first_id`/`last_id`）格式；不调用上游；非 GET 返回 405。
- M3 管理账号：顶层配置新增可选 `admin_user`（默认 `admin`）与 `admin_password`（空=管理面禁用）；向后兼容旧配置。
- M3 Web 配置页：`internal/web` 用 `go:embed` 内嵌原生 HTML+JS 页面（无外部 CDN），`GET /admin`、`/admin/`；支持查看/编辑基础配置与 backends、保存、重载。
- M3 管理 REST API：`GET/PUT /api/config`、`POST /api/reload`，全部 HTTP Basic Auth（失败 401 + `WWW-Authenticate: Basic realm="model-router"`）；`admin_password` 为空时管理路径 404。
- M3 配置持久化：`PUT /api/config` 严格校验后原子写回（临时文件 + rename，权限 0600），不触发生效；校验失败 400 且不写文件。
- M3 原子重载：进程记住 `-config` 路径，`atomic.Pointer` 持有当前 config/handler；reload 整体替换，在途请求使用旧 handler；失败保持旧配置。
- M3 `Duration` 增加 `MarshalJSON`（输出 `"15s"` 形式），保证 `GET /api/config` 与 `PUT` 写回的文件可被再次 `Load`。
- M3 单元测试：`/v1/models` 双协议格式、过滤/去重、401/405；admin Basic Auth 401 与页面；`GET /api/config`；`PUT` 校验/写回/不生效；reload 成功切换与失败保留；`Duration` 序列化往返；配置 Save/Load 往返与 admin 默认值。
- M3-env（§8.9）环境变量覆盖：`MODEL_ROUTER_LISTEN` / `MODEL_ROUTER_AUTH_TOKEN` / `MODEL_ROUTER_ADMIN_USER` / `MODEL_ROUTER_ADMIN_PASSWORD` 在 JSON 解码后、`applyDefaults` 前覆盖对应顶层字段；非空才覆盖，覆盖值参与 trim 与必填校验，fail-closed 不变；`instances` 不支持环境变量。测试：`TestEnvOverridesConfig`、`TestEnvEmptyKeepsFileValues`、`TestEnvAuthTokenSatisfiesRequired`（均用 `t.Setenv` 隔离）。
- M3.1 配置结构 `backends` → `instances`：每个实例承载一组模型映射；OpenAI 实例用 `models`（去空去重、至少 1 项），Anthropic 实例用 `roles` 对象（键 ∈ sonnet/opus/fable/haiku/*、值非空、至少 1 项）；二者互斥；旧 `backends` 字段出现即解析失败并提示迁移。
- M3.1 实例展开路由：运行时把实例按声明顺序展开为扁平条目；R4a 精确匹配不改写，R4c 角色/`claude-<role>-` 前缀命中改写顶层 model，R4d `*` 托底改写，R5 回退首条目不改写；`matches` 字段随 `backends` 一并移除。
- M3.1 管理 API：`POST /api/probe/test`（GET `{base_url}/v1/models` 探测连通性，返回 ok/latency/message）、`POST /api/probe/models`（解析 `data[].id` 返回模型列表）、`POST /api/config/validate`（`Parse` dry-run，不写回不生效）；均走 admin Basic Auth，admin_password 为空时 404。
- M3.1 配置页重做：可视化模式（OpenAI/Anthropic 双 tab、添加实例、测试连接、查询上游模型、OpenAI 勾选/手填 models、Anthropic 四角色+托底选择/手填 roles）+ JSON 高级模式（校验通过后才启用保存）；纯原生 HTML+JS，无外部 CDN。
- M3.1 测试：instances 解析与校验（互斥、必填、roles 键/值、models 去空去重、旧 backends 报错）、实例展开路由（精确/角色/托底/回退/502）、`/v1/models` 双协议、probe test/models（httptest mock 上游与认证头）、validate 通过/失败、admin 鉴权与重载。
- M3.1 probe 修复：`probe/test` 按协议选择探测端点——openai 仍 `GET /v1/models`，anthropic 改为 `POST /v1/messages` 最小请求（占位 model `ping`），解决 DeepSeek 等 Anthropic 兼容入口未实现 `/v1/models` 时的误报；判定改为 401/403/404 → `ok:false`，2xx 与 400/422/429 等 → `ok:true`。`probe/models` 对 404 返回「该上游不支持模型列表查询（未实现 /v1/models），请手动输入模型名」。新增 anthropic 200/400/401/404 单测。
- M3-providers（§10）供应商预设：新增 `internal/providers` 包，内置 11 个供应商（deepseek/openai/anthropic/zhipu/volcengine/moonshot/dashscope/siliconflow/groq/mistral/xai），提供 `List()`/`Get(id)` 与通用 `models_override` 解析（仅 DeepSeek 为 anthropic 声明 `{base_url: https://api.deepseek.com, auth: bearer}`）。
- M3-providers `Instance.provider` 可选字段：非空必须是已知内置供应商 id（大小写不敏感、归一化小写），否则启动/validate 失败；空=自定义。只作来源标记与模型列表端点判定，不影响路由/转发。
- M3-providers `GET /api/providers`：admin Basic Auth，返回内置供应商清单（含 base_urls 与 models_override）；admin_password 为空时 404；非 GET 405。
- M3-providers `probe/models` 支持可选 `provider`：命中该协议 models_override 时改用 override 的 base_url/auth 拉 `GET /v1/models`，否则沿用实例 base_url 与原认证；未知 provider → 400。`probe/test` 不涉及 provider。
- M3-providers 配置页：新增实例先弹「选择供应商」框（内置供应商 + 自定义），选内置按当前 tab 自动填 base_url 并记录 provider，不支持当前协议时阻止；「查询模型」请求携带 provider；保存/JSON 高级编辑均保留该字段。
- M3-providers 测试：providers 清单完整性（11 个、id 唯一、base_url/override 合法）、Get/override/ModelsSource 解析、Instance.provider 合法/空/非法、`GET /api/providers` 清单与鉴权、probe/models 无 override 回退与未知 provider 拒绝。

### Changed

- M3.1 配置结构不兼容变更：移除 `backends` 与 `matches`，改为 `instances`（OpenAI `models` / Anthropic `roles`）；旧配置需迁移，出现 `backends` 会得到迁移提示。
- M3.1 配置示例与文档迁移为 `instances`：`config.example.json` 为 1 个 openai 实例（2 个模型）+ 2 个 anthropic 实例（角色映射与 `*` 托底）；README/design/tests 同步更新。
- M3.1 运行时内部结构由 `backend`/`matches`/`roles[]` 改为展开后的 route 条目；Anthropic roles 因 JSON 对象无顺序，展开顺序固定为 sonnet→opus→fable→haiku→`*`（同一实例内条目共用上游，路由结果不受影响）。
- M3-providers `config.example.json` 的三个实例补充 `provider: "deepseek"` 示例。

- M3 路由优先级：`/admin*`、`/api/*` 走 admin Basic Auth（未配置密码则 404）；`/v1/chat/completions`、`/v1/messages`、`/v1/models` 走模型认证；其他路径 404。
- M3 `main.go` 改为使用 `proxy.NewServer(configPath, cfg)`：由 Server 统一分派管理面与模型面，并支持原子重载；启动日志增加管理页启用状态（不打印密码）。
- M3 reload 仅切换 handler/config，不重新绑定监听地址；`listen` 变更需重启进程。
- M2 配置结构：`backend`（单对象）改为 `backends`（有序数组，含必填 `protocol` 与 `model`）；旧配置格式不再接受（启动失败），需迁移。
- M2 `backend.model` 由“仅日志”改为必填并参与按 model 路由；未命中别名时转发不修改请求体中的 `model`。
- M2 Anthropic 入口客户端认证由 `Authorization: Bearer` 改为 `x-api-key`；携带 Bearer 不再通过认证（返回 401 `authentication_error`）。
- M2 路由判断顺序扩展为 R4a（model）→ R4b（matches）→ R4c（具体角色/前缀）→ R4d（`*` 托底）；`*` 为第二轮扫描，任何具体命中都优先于托底。
- M2 `roles` 的校验规则与 `matches` 不同：`roles` 每项必须是枚举值（空项视为非法、启动失败），`matches` 空项则被丢弃。
- M2 请求体改为先读入内存（≤10 MiB）再转发，以支持 model 路由；`Content-Length` 按实际字节重算。
- M2 启动日志改为打印后端数量与每个后端的协议/地址/模型（不打印任何密钥）。

- 配置校验：`auth_token` 与 `backend.api_key` 改为必填，缺失时启动失败。
- 凭据语义修正：由「客户端 `Authorization` 优先透传、配置 `api_key` 兜底」改为「客户端 token 仅认证本代理、上游一律使用配置 `api_key`」。
- 错误响应统一增加 `error.code` 字段；404 的 `error.type` 由 `not_found` 调整为 `invalid_request_error`（`code` 为 `not_found`）。
