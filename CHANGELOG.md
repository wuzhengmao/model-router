# Changelog

本文件记录项目的重要变更，遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的结构与语义化版本号规则。

## [0.1.0] - 2026-11-15

### Added

- M4.2 三段超时职责划分：新增 `fallback.first_token_timeout`（默认 `10s`，`0` 禁用）——实例 `timeout` 管到响应头、首 token 超时管响应头→首个数据块（未提交可回退）、`idle_timeout` 管块间间隔。
- M4.2 流式吞吐改为纯生成速度判定（首数据块→末数据块），并加最小样本保护：生成时长 `<2s` 或输出 `<100` token 时不判定、按成功计。
- M4.2 每请求日志打印输入/输出 token 数与生成速度（`in` / `out` / `ttft` / `gen` / `total`）。
- M4.2 低速在 OpenAI 与 Anthropic 一律不打断（仅记一次失败），`pause_turn` 只由 `idle_timeout` 触发。
- M4.3 / M4.4 请求日志显示客户端类型（`client=`，十一级识别规则：dsh / claude-cli / curl / python / node / go-http-client 等）与来源 IP（`ip=`，优先 `X-Forwarded-For` 首段，回退 `RemoteAddr`）。
- M4.4 Anthropic 入口双认证：客户端可携带 `x-api-key: <auth_token>` **或** `Authorization: Bearer <auth_token>`，两者任一匹配即通过（官方 Anthropic SDK 用前者、Claude Code 用后者，两侧兼容）。
- M4.4 `[1m]` 长上下文变体：`opus[1m]` / `sonnet[1m]` 等带后缀的模型名剥除后缀后参与角色匹配，不再漏到 `*` 托底。
- M4.5 实例 `name` 字段：仅用于日志显示（不参与路由、不改变转发），缺省回退 `base_url`；Web 配置页实例卡片新增名称输入框。
- M4.5 日志格式改版：`POST <path> -> <name>/<model> (<req_model>) in <total>s ↑<in> ↓<out> <gen>t/s from <ip> <client>`；生成速度不满足样本门槛时整段省略。
- M4.5.1 实时低速检测（§19）：10 秒滑动窗口按字节速率（`bytesPerToken = 3`）计算，Anthropic 触发后中断流并补 `pause_turn` 收尾、OpenAI 触发后只记失败不打断；窗口未满 10 秒不判定（避免冷启动误判）。
- M4.6 gzip 响应体修复（§20）：不再透传客户端 `Accept-Encoding`（交给 Go Transport 自主协商并透明解压），并对上游强制压缩的响应按 `Content-Encoding` 解压后转发（`br` / `zstd` 等不可解码编码保留透传并标记不可解析，跳过 usage 与字节速率判定）。

### Changed

- 版本号升为 `0.1.0`（开发阶段最后一个小版本，正式发布前仍可能调整）。
- 容器时区默认 `Asia/Shanghai`（Dockerfile 打入 `tzdata` 与 `zoneinfo/Asia/Shanghai`，`ENV TZ=Asia/Shanghai`），`docker logs` 时间与宿主机一致。
- 文档示例与占位符统一使用中性名称（`DeepSeek` / `192.0.2.10` / `claude-cli`），不再出现私有环境名称。

### Fixed

- 请求日志中 `↑n/a ↓n/a`：客户端带 `Accept-Encoding: gzip` 时上游返回压缩 SSE，内部解析读到压缩字节导致 usage 解析失败（同时会造成低速误判）；现已解压后再解析。
- 日志断言测试偶发失败（时序竞态）：`logRequest` 在响应体写完、客户端读完后才执行，测试改为轮询等待日志出现（`waitForLog`，10ms 轮询 / 2s 超时）。

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
- M4 实例粒度回退降级（§11）：候选链（基础实例 + 按实例顺序的后续候选，含上游模型与改写标记）；OpenAI 精确/`models[0]` 改写，Anthropic 角色/`*` 改写与不可服务实例跳过，R5 候选链不改写正文。
- M4 回退状态机：每实例 `consecutiveFailures` + `degradedUntil`（`sync.Mutex` 保护）；跳过持续回退实例、连接/超时本次请求立即临时回退、连续失败 3 次进入 5 分钟持续回退、回退期探测失败立即重新计时、成功清零、全部降级强制尝试链头。
- M4 失败判定：连接失败与 dial/响应头超时在响应前触发回退；非流式响应按 usage 计算 `tokens/elapsed < 30` 记录失败（响应已发出不回退）；SSE 不做吞吐判定。
- M4 配置页实例拖动排序：实例卡片标题可拖动（HTML5 drag & drop），保存时按界面顺序序列化 `instances`。
- M4 测试：候选链（OpenAI 精确/改写/基础实例、Anthropic 角色/`*`/跳过/精确、R5 无 `*` 兜底）、状态机（临时回退、<3 重试主实例、≥3 持续回退、回退期探测失败重新计时、成功恢复、全降级强制链头）、并发（20 并发 + race detector）、吞吐（OpenAI `completion_tokens` / Anthropic `output_tokens` / 无 usage 不判失败）。
- M4-config `fallback` 配置对象（§12.2）：`failure_threshold`（默认 3，≥1）、`degrade_duration`（默认 `5m`，>0）、`min_tokens_per_second`（默认 30，≥0，0=禁用吞吐判定）；整体可省略，非法值启动/validate 失败；状态机参数改从配置读取。
- M4-config `instances` 分组结构：顶层改为 `{"openai":[...],"anthropic":[...]}`；实例不再含 `protocol` 字段（由分组决定），两组合计至少 1 个实例；旧数组形态报错提示迁移为分组结构。
- M4-config 运行时适配：配置解析后内部仍保持带协议标记的有序切片（`config.Instances`），`proxy` 的候选链/状态机/路由/`/v1/models`/probe 逻辑不变，仅回退参数来源改为 `Config.Fallback`。
- M4-config 配置页：可视化模式从 `instances.openai` / `instances.anthropic` 读取，拖动排序在组内生效、按组序列化；JSON 模式展示并保留 `fallback` 与分组结构。
- M4-config 测试：fallback 缺省/显式/`min_tokens_per_second=0`/非法值；分组解析与保序、空组合计 0 失败、实例含 `protocol` 报错、旧数组迁移提示；配置驱动的阈值/回退时长/禁用吞吐行为。
- M4-fallback-ui 可视化回退参数：页面顶部新增「回退参数」区块（failure_threshold / degrade_duration / min_tokens_per_second），加载时从 `fallback` 回填（显式 0 显示 0、缺失显示默认 3/5m/30）；保存时前端校验（阈值 ≥1、Go 风格时长 >0、吞吐 ≥0）并入 `config.fallback`，通过后再调用 `/api/config/validate` 兜底；留空字段不写入（走默认）。
- M4-fallback-ui 交互细节：非法值就地提示并阻止保存；切到 JSON 模式遇到非法输入时使用已加载的 fallback；输入变更清除错误提示。
- M4.1 `fallback.idle_timeout`（默认 `30s`，0=禁用）：OptionalDuration 支持显式 0，负值/非法值启动失败；`Fallback.Values()` 返回生效值，配置页新增「空闲超时」输入框（加载回填、保存校验、留空走默认）。
- M4.1 流式空闲超时：committed 后按 chunk 到达 Reset 的 `time.AfterFunc` 计时；超时关闭上游读取并记实例失败。Anthropic 补发 `content_block_stop`（若在块内）+ `message_delta(stop_reason=pause_turn)` + `message_stop`；OpenAI 通过 Hijack 直接中断客户端连接（不补 `[DONE]`）。
- M4.1 流式吞吐：SSE 解析 `usage.output_tokens` / `completion_tokens`，流结束后低于 `min_tokens_per_second` 仅记失败、不打断；非流式逻辑不变。
- M4.1 测试：idle_timeout 缺省/显式/0/非法；Anthropic 静默流收到 pause_turn 收尾（含 content_block_stop 与 usage）；OpenAI 静默流连接截断；OpenAI 慢速流完整结束且仅记失败；配置驱动的 idle_timeout 生效。
- M4.3 请求日志增加客户端标识与来源 IP（§16）：每请求一行追加 `client=`（按十一级规则识别 `claude-code/<version>`、`claude-code`、`openai-python`、`openai-node`、`anthropic-js`、`anthropic-python`、`dsh`、`curl`、UA 前缀、`unknown`）与 `ip=`（`X-Forwarded-For` 首段，否则 `RemoteAddr` 去端口）；401 请求同样打印一行，`client`/`ip` 带出但不含任何密钥。规则为纯字符串匹配、大小写不敏感。
- M4.3 测试：`TestRequestLogFormat`（字段顺序与取值）、`TestClientIdentification`（十一级 UA/头规则）、`TestClientIP`（XFF 首段/`unknown`/RemoteAddr 去端口）、`TestAuthFailureIsLoggedWithoutSecrets`（401 行含 client/ip 且不泄露 token）。
- M4.4 Anthropic 入口双认证（§17.2）：先取 `x-api-key` 匹配，否则回退 `Authorization: Bearer`（前缀大小写不敏感）；两者均缺失/不匹配 → 401。OpenAI 入口仍只认 Bearer。两候选头各自 `subtle.ConstantTimeCompare`，上游仍使用实例 `api_key`、客户端 token 不外传。
- M4.4 `[1m]` 变体识别（§17.3）：R4c 角色判定前剥除 `model` 尾部 `[1m]`（大小写不敏感），`opus[1m]`/`sonnet[1m]`/`claude-opus-4-5[1m]` 正确命中角色并改写为 `roles[<role>]`，不再漏到 `*` 托底。
- M4.4 吞吐判定样本门槛（§17.4）：生成时长改为“首数据块→末数据块”（响应开始计时，建连/TTFT 不计入）；仅当输出 >100 token 且生成时长 >2s 且能拿到 usage 时才计算 token/s，不满足门槛 → 跳过并视为健康（不记失败）。
- M4.4 请求日志最终格式（§17.6）：新增 `req_model=`（客户端原始模型，拿不到为 `n/a`），`model=` 为实际转发模型；`gen=` 仅在满足样本门槛时显示数值，否则 `n/a`。字段顺序固定：`req_model` → `model` → 协议 → `client` → `ip` → `total` → `ttft` → `in` → `out` → `gen`。
- M4.4 容器时区（§17.7）：Dockerfile build 阶段安装 `tzdata`，运行阶段 COPY `zoneinfo/Asia/Shanghai` 与 `/etc/localtime`，`ENV TZ=Asia/Shanghai`，容器日志时间戳为北京时间（UTC+8）。
- M4.4 测试：`TestAnthropicDoubleAuth`（x-api-key / Bearer / 小写 bearer / 缺失 / 有头但错）、`TestRouteByRoles` 的 `[1m]` 子用例（含 `opus[1M]`、`claude-*[1m]`）、`TestRouteWildcardFallback` 的 `opus[1x]`/`opus [1m]` 反例、`TestThroughputFailure` 改为断言样本不足不记失败、`TestRequestLogFormat` 断言 `req_model` 与 `gen=n/a`。

### Changed

- M4.6 修复 gzip 压缩响应体导致 usage 解析与 §19 实时低速判定失效：转发上游前删除客户端 `Accept-Encoding`，交给 Go `http.Transport` 协商并透明解压；上游强制 `Content-Encoding: gzip` 时再用 `gzip.Reader` 解压后转发并移除 `Content-Encoding`/`Content-Length`。SSE 状态机、`parseUsage` 与字节速率窗口改为按解压后字节计算，客户端仍收到明文响应；`br`/`zstd` 等不可解码编码保持透传并打 warn，gzip 解压失败按上游错误返回 502 并 `recordFailure`。
- M4.1 流式响应会先移除上游 `Content-Length`（补发收尾事件时需要 chunked 帧）；SSE 响应头其余字段仍原样透传。
- M4.1 流式吞吐判定由“跳过”改为“按 SSE usage 判定但从不打断流”。
- M4.4 Anthropic 入口 401 文案调整：`x-api-key` 空且 `Authorization` 不以 `Bearer ` 开头 → `missing x-api-key or Authorization header`；有头但不匹配 → `invalid service access token`。
- M4.4 吞吐耗时口径由“单次候选尝试总耗时（含建连/等待响应头）”改为“纯生成窗口”（流式首块→末块、非流式响应头→响应体读完），与 §17.4 一致；样本不足时不再产生失败记录。
- M4.3 请求日志行新增 `req_model`（客户端原始模型）与 `client` / `ip` 字段；`model` 仍为实际转发模型，原有 `total`/`ttft`/`in`/`out`/`gen` 不变。
- M4.4 `[1m]` 后缀不再作为未命中处理：`sonnet[1m]`、`opus[1m]` 等由角色路由正确处理（此前会落到 `*` 托底或 R5）。
- M4.4 Docker 运行镜像增加时区数据（`zoneinfo/Asia/Shanghai` + `/etc/localtime` + `TZ`）；distroless 本身无 tzdata，镜像仅因此增加一个 zoneinfo 文件。
- M5 GitHub Actions 跨平台构建：`.github/workflows/build.yml` 在 push / PR / tag `v*` 上以 6 组合（windows/linux/darwin × amd64/arm64）构建并上传产物；tag 时创建 Release，并在 ubuntu runner 上用 buildx 构建 `linux/amd64,linux/arm64` 镜像推送 GHCR（`:<version>` 与 `:latest`）。
- M5 Docker 镜像：multi-stage `golang:1.27-alpine` → `gcr.io/distroless/static-debian12:nonroot`；`CGO_ENABLED=0`、`TARGETOS/TARGETARCH` 多架构构建、非 root、`EXPOSE 18080`，入口 `-config /home/nonroot/config/config.json`，默认 `MODEL_ROUTER_LISTEN=0.0.0.0:18080`。
- M5 `docker-compose.yml` 部署示例：端口 18080、挂载 `./.docker` 目录（`.docker/config.json`）、环境变量覆盖、`restart: unless-stopped`。
- M5 `build-all.sh` 本地全平台构建脚本与 `.dockerignore`（排除 dist/.git/.claude-runs/本地配置等）。
- M5 文档：README 增加构建/CI/Docker 部署说明与目录结构；design/tests 增加构建与部署设计与验证用例。
- M5 版本来源保持 `internal/version/version.txt`（`0.0.1-rc`）；CI tag 构建时版本改用 tag 去掉 `v` 前缀（如 `v0.0.1` → `0.0.1`）。
- M3-models-roles 修复：Anthropic 入口 `/v1/models` 改为返回预定义角色名 `sonnet`/`opus`/`fable`/`haiku`（固定顺序、去重、仅已声明角色、不含 `*`），不再返回 `roles` 映射的实际模型值；OpenAI 入口行为不变。测试更新为断言角色名。
- M4-fallback-ui 可视化「保存到文件」流程改为先 `POST /api/config/validate` 再 `PUT /api/config`；`fallback` 不再原样透传已加载对象，而是由三个输入框序列化。
- M4-config 配置不兼容变更：`instances` 由混合数组改为分组对象，实例移除 `protocol` 字段；旧配置需迁移（出现旧数组会得到「分组结构」错误提示）。
- M4-config `proxy.New` 现在从 `Config.Fallback` 读取 3 个回退参数；`NewWithOptions` 仍可注入测试用参数（0 表示沿用配置）。
- M4-config `config.example.json` 迁移为 `fallback` + 分组 `instances`。
- M4 路由执行由「单条 route」改为「候选链 + 状态机」；`proxy.New` 保持兼容，新增 `proxy.NewWithOptions` 供阈值/时钟注入（默认 3 次 / 5 分钟 / 30 token/s）。
- M4 Anthropic R5（无可用 model）候选链改为「声明 `*` 的实例」且不改写正文；无任何 `*` 时兜底尝试第一个实例，保持 M3 R5 行为。
- M4 reload 会重建 handler，因此回退计数/持续回退状态随 reload 清零（仅进程内存，无需持久化）。
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
