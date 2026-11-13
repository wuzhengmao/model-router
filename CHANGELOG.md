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
- 一键构建脚本 `build.sh`：构建当前平台（windows/amd64）可执行文件，`CGO_ENABLED=0`，产物输出到 `bin/`。
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

### Changed

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
