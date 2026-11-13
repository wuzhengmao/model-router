# model-router 测试用例（M1）

> 对应需求：[requirements.md](requirements.md) M1 里程碑（含 FR-4 服务访问认证）；设计：[design.md](design.md)。

## 1. 测试策略

- 单元测试使用 `net/http/httptest`：mock 一个 OpenAI 兼容后端，真实启动代理 Handler，**断言认证与透传行为本身**（不 mock 代理内部逻辑）。
- 认证断言覆盖：缺失 token、错误 token、正确 token 三种路径，以及上游收到的是配置 `api_key` 而非客户端 token。
- 端到端用例验证构建产物与 `go run .` 启动路径。
- 全部用例可离线执行，不依赖真实模型供应商。

## 2. 单元测试用例

### 2.1 配置（`internal/config`）

| 编号 | 用例 | 命令 / 入口 | 步骤 | 预期 |
|---|---|---|---|---|
| UT-001 | 缺省值生效 | `TestLoadAppliesDefaults` | 配置含 `auth_token`、`backend.base_url`（带尾部 `/`）、`api_key`、`model`，不含 `listen` / `timeout` | `listen` = `127.0.0.1:8080`；`timeout` = 15s；`base_url` 去尾部斜杠；`auth_token` 与其余字段保留 |
| UT-002 | 超时解析 | `TestLoadParsesTimeout` | `timeout: "20s"` | `Timeout.Duration()` = 20s |
| UT-003 | 缺失 base_url 拒绝 | `TestLoadRejectsMissingBaseURL` | 其余字段合法，无 `backend.base_url` | `Load` 返回错误 |
| UT-004 | 非法超时拒绝 | `TestLoadRejectsInvalidTimeout` | `timeout: "soon"` | `Load` 返回错误 |
| UT-005 | 未知字段拒绝 | `TestLoadRejectsUnknownField` | 顶层多出 `listne` 拼写错误字段 | `Load` 返回错误 |
| UT-006 | 缺失 auth_token 拒绝 | `TestLoadRejectsMissingAuthToken` | 其余字段合法，无 `auth_token` | `Load` 返回错误（fail closed） |
| UT-007 | 缺失 api_key 拒绝 | `TestLoadRejectsMissingAPIKey` | 其余字段合法，无 `backend.api_key` | `Load` 返回错误 |

### 2.2 认证与透传（`internal/proxy`）

| 编号 | 用例 | 命令 / 入口 | 步骤 | 预期 |
|---|---|---|---|---|
| UT-101 | 正确 token：认证通过并原样透传 | `TestPassThroughVerbatim` | 配置 `auth_token=service-token`、`api_key=config-key`；客户端 `POST /v1/chat/completions?trace=abc`，`Authorization: Bearer service-token`，带 `Content-Type`、自定义头与 JSON body；mock 后端返回 201、自定义响应头与 JSON body | 后端收到：方法 `POST`、路径 `/v1/chat/completions`、查询 `trace=abc`、`Content-Type`/`X-Custom-Header` 原值、body 与请求完全一致；`Authorization` 为 `Bearer config-key`（配置 api_key），**不是** `Bearer service-token`；客户端收到：状态 201、后端响应头与 body 完全一致 |
| UT-102 | 缺少 Authorization → 401 | `TestMissingAuthorizationReturns401` | 客户端不带 `Authorization` 调用代理 | 返回 401；错误体为 JSON，含非空 `error.message` / `error.type` / `error.code`，`code=missing_api_key`；后端调用次数为 0 |
| UT-103 | 错误 token → 401 | `TestWrongTokenReturns401` | `Authorization: Bearer wrong-token` | 返回 401；错误体 JSON 含 `message`/`type`/`code`，`code=invalid_api_key`；后端调用次数为 0 |
| UT-104 | 未知路径不转发 | `TestUnknownPathNotForwarded` | 携带正确 token，`GET /v1/models` | 返回 404；错误体 `code=not_found`；后端调用次数为 0 |

`decodeError` 辅助函数对 401/404 响应统一断言：`Content-Type` 为 `application/json`，body 可解析为 `{"error":{...}}` 且 `message`/`type`/`code` 非空。

## 3. 端到端用例

### ET-001 启动监听并认证透传（本地 mock 后端）

1. 在 `127.0.0.1:18999` 启动一个最小 mock 后端（返回固定 chat completion JSON）。
2. 准备配置：`listen=127.0.0.1:18080`、`auth_token=service-token`、`backend.base_url=http://127.0.0.1:18999`、`backend.api_key=config-key`、`timeout=15s`。
3. 启动：`go run . -config <上述配置>`。
4. 先验证未认证请求：

   ```bash
   curl -sS -i -X POST http://127.0.0.1:18080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{"model":"mock-model","messages":[{"role":"user","content":"ping"}]}'
   ```

   预期：`401`，错误体含 `error.message` / `error.type` / `error.code`，mock 后端无请求。

5. 再验证认证请求：

   ```bash
   curl -sS -i -X POST http://127.0.0.1:18080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer service-token" \
     -d '{"model":"mock-model","messages":[{"role":"user","content":"ping"}]}'
   ```

预期：

- 认证请求返回 200 与后端原始 JSON；
- mock 后端日志显示收到 `POST /v1/chat/completions`，`Authorization` 为 `Bearer config-key`（不是 `service-token`），body 与请求一致；
- 代理启动日志含 `listening on 127.0.0.1:18080`，且不出现 token / api_key。

### ET-002 一键构建与版本号

1. `bash build.sh`
2. `./bin/model-router-0.0.1-rc-windows-amd64.exe -version`

预期：产物为 windows/amd64 可执行文件；`-version` 输出 `0.0.1-rc`。

### ET-003 配置错误快速失败

1. 以缺失 `auth_token` 的配置运行 `go run . -config <文件>`。
2. 以缺失 `backend.api_key` 的配置重复一次。

预期：两种情况下进程启动失败，错误信息指出配置文件与缺失字段，不监听端口。

### ET-004 示例配置包含认证字段

1. 检查 `config.example.json`。

预期：顶层存在 `auth_token` 示例字段，且 `backend.api_key` 非空。

## 4. 执行方式

```bash
go build ./...   # 退出码 0
go vet ./...     # 退出码 0
go test ./...    # 退出码 0
bash build.sh    # 产物在 bin/
```

## 5. 验收对照

| 验收项 | 对应用例 |
|---|---|
| `go build ./...` / `go vet ./...` / `go test ./...` 通过 | §4 + UT-001～UT-104 |
| 无 Authorization → 401 | UT-102、ET-001 步骤 4 |
| token 错误 → 401 | UT-103 |
| token 正确 → 转发，且上游 Authorization 为配置 api_key | UT-101、ET-001 步骤 5 |
| 401 错误体为 OpenAI 兼容 JSON（含 `error` 对象） | UT-102、UT-103（`decodeError`） |
| `config.example.json` 含 `auth_token` | ET-004 |
| 构建产物 `-version` 输出 `0.0.1-rc` | ET-002 |
| 配置错误不被静默忽略（含认证配置） | UT-003～UT-007、ET-003 |
