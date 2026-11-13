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

### Changed

- 配置校验：`auth_token` 与 `backend.api_key` 改为必填，缺失时启动失败。
- 凭据语义修正：由「客户端 `Authorization` 优先透传、配置 `api_key` 兜底」改为「客户端 token 仅认证本代理、上游一律使用配置 `api_key`」。
- 错误响应统一增加 `error.code` 字段；404 的 `error.type` 由 `not_found` 调整为 `invalid_request_error`（`code` 为 `not_found`）。
