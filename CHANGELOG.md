# Changelog

本文件记录项目的重要变更，遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 的结构与语义化版本号规则。

## [Unreleased]

## [0.1.3] - 2026-11-16

### Changed

- Logo 改为匿名静态资源（§24.14 L1）：新增 `GET /logo.svg`（`image/svg+xml`，`Cache-Control: public, max-age=86400`），在鉴权之前处理，未登录也返回 200；`/admin/logo.svg` 路由删除。四个管理页与登录页左上角的 `<img src>` 统一改为 `/logo.svg`。

### Fixed

- 未登录时登录页左上角 Logo 始终不显示：原先 `/admin/logo.svg` 位于管理面鉴权之后，匿名请求被 302 重定向到登录页，`<img>` 拿到 HTML 无法解码。改为匿名路径后登录页与管理页一致显示。

### Docs

- `README.md` 标题下展示 Logo（`docs/assets/logo.svg`），并按新的匿名路由更新接口表；`docs/design.md` §33.5、`docs/requirements-monitor.md` §24.14 记录本轮修订与验收。

## [0.1.2] - 2026-11-16

### Added

- 项目品牌化（§24.13 K1–K4）：中文展示名改为「模型智能路由」，管理页四个页面与登录页的 `<title>`、左上角标题统一使用中文名；新增矢量 Logo `internal/web/logo.svg` 与多尺寸 favicon `internal/web/favicon.ico`（16/32/48/64/128/256），以 `go:embed` 打包；新增匿名可访问的 `GET /favicon.ico`（`image/x-icon`，缓存 1 天）与需登录的 `GET /admin/logo.svg`（`image/svg+xml`）；英文标识（Go module、二进制名、镜像 tag、服务名、配置名、环境变量、日志、URL）保持不变。

- 统计持久化（§24.10 B1，D11）：`stats.json` 落在配置文件同目录，保存累计计数、按协议、in/out tokens、重试、各实例累计与最近 20 条错误（带 `"schema":1`）；同目录临时文件 + `rename` 原子写、≥5 秒且有变化才写、`SIGINT`/`SIGTERM` 补写；文件损坏/未知 schema/负计数会改名为 `stats.json.bad` 并零值启动，不阻塞启动；实例从配置删除后历史行仍显示并标注「已移除」。新增需登录的 `POST /api/stats/clear`（清零并立即落盘，不影响 100 条请求快照）。
- 镜像与卷（§24.10 B2）：`Dockerfile` 声明 `VOLUME ["/home/nonroot/config"]`，并复制 65532 属主的占位配置使具名卷可直接使用；新增 `docker-compose.named.yml`（具名卷 `model-router-config`）；README 增加统计持久化、卷与 distroless 非 root 写权限说明。
- 新增测试 UT-775～UT-781：`owned_by` 实例名与模型并集、统计落盘/加载/损坏回退/节流、`/api/stats/clear`、真实容器与浏览器数值格式。

- M10 管理界面第四轮后端（§24.7 D4/D6/D7/D9/D10）：`POST /api/login`、`POST /api/logout` 与 `GET /admin/login`；`mr_session` 会话 cookie（32 字节 CSPRNG base64url 不透明 token、`HttpOnly; SameSite=Lax; Path=/`、不加 `Secure`；记住我 30 天 / 默认 12 小时，均存进程内存不落盘），`next` 仅允许 `/admin` 开头的站内路径；`/admin` 302 `/admin/`，`/admin/`、`/admin/monitor`、`/admin/config`、`/admin/json` 四个页面路径可刷新停留，未登录统一 302 `/admin/login`。
- M10 统计聚合（§24.7 D7）：`GET /api/stats` 与 WS `stats` 帧共用同一快照结构，含运行时长/版本/活跃 WS 与请求数/QPS/累计成功失败/成功率/按协议/最近 1000 样本的 TTFT 与总耗时分位数（<10 样本为 null）/输入输出 tokens/平均生成速率/重试数/实例分组（含降级状态）/最近 20 条错误；JSON 形状见 `docs/design.md` §28.3。
- M10 统计首页（§24.7 D7 / §24.10 C）：`/admin/` 展示运行与连接、请求统计、实例统计、近期错误四块指标；时间按 ms/s/h、计数按 K/M 缩写，所有缩写值带千分位精确 `title`。
- M10 全局 WS（§24.7 D9 / §24.11 H1）：`GET /api/ws` 是唯一 WebSocket 端点，新连接立即收到 `{"type":"snapshot","requests":[...],"stats":{...}}`（最近 100 条已结束请求 + 最多 200 条进行中请求，按 `ts` 降序，含逐块 `chunksList`）与 `{"type":"stats","ts":…,"data":{…}}`；统计变化后最快 2 秒、无变化每 10 秒推送一次。
- M10 采集语义变更（§24.7 D9）：无 WS 客户端时仍累计统计并维护最近 100 条环形缓冲，`start`/`end` 始终记录；`chunk`/`retry` 与事件广播仍受客户端开关控制，无客户端时不产生任何 WS 写操作。
- M10 清除接口（§24.7 D10）：`POST /api/monitor/clear`（需登录）清空后端最近 100 条环形缓冲，返回 200，不影响累计统计。
- 新增后端测试 UT-750～UT-763（登录 cookie 三种行为、页面 302/200、next 防开放重定向、会话登出失效、统计键集合一致、120→100 快照、无客户端累计、清除、分位数/采样窗口/错误摘要单元测试）。

- M9 管理页实时请求监控（§24）：管理页新增 WebSocket 实时事件流（最终端点为 `GET /api/ws`，旧 `/api/monitor` 已在 §24.11 删除）；认证复用管理面（会话 Cookie / Basic，脚本可用 `?user=&password=` 回退），`admin_password` 为空仍返回 404。
- 监控事件四型 `start` / `chunk` / `retry` / `end`，字段按 §24.2 F3 推送；`chunk` 对应每次上游 trunk 读取，含 `tsStart` / `tsEnd` / `bytes` / `deltaTokens` / `cumTokens`，`end` 含状态码、总耗时、in/out token、chunk 数、TTFT、生成时长与速率。
- 逐 trunk 事件的广播仅在至少一个 WS 客户端连接期间启用（原子计数判断）；无客户端时仍累计统计与快照，但不产生 WS 写操作。现有日志格式、`attemptMetrics` 语义与 §23 三档规则不变。
- WS 使用标准库最小实现（`http.Hijacker` 握手、文本/ping/pong/close 帧），不引入新依赖；单连接积压超过 1024 条未发事件即断开该连接，不影响代理与其它客户端；每 30s ping、75s 读超时回收死连接。
- 管理页新增四个独立路由页面（§24.7 D6）：统计首页 `/admin/`、实时监控 `/admin/monitor`、可视化配置 `/admin/config`、JSON 高级 `/admin/json`；登录后全局 WS 自动连接，不再需要监控页单独的认证栏或连接/断开按钮。实时监控主体是**单表格**（`#`、状态、方法/路径、客户端、请求模型/实例·上游、入、出、块、耗时、TTFT、t/s、时间轴）；每行独立时间轴，等待首包（琥珀）/ trunk（蓝）/ 含内容块（绿）三色、重试虚线、进行中当前游标，所有行共享按最长耗时动态更新的量程，刻度与图例在表头；「清空」只移除已完成行并同时清空后端最近 100 条环形缓冲，列表最多保留最近 500 条已完成请求。
- M9 第二轮（§24.5 W1–W6）：删除左右分栏与选中详情，统计列与时间轴在同一 `<tr>`；每行按 `[tsStart, tsEnd]` 画 trunk 块、进行中行显示当前时刻游标；「清空」只移除已完成行、保留进行中与 WS 连接。（监控页内「记住密码」认证栏在 §24.7 被全局会话认证取代。）
- 新增测试（UT-701～UT-709）：WS 认证拒绝与 query 回退、流式 chunk 序列计数、非 AI 请求零事件、候选全失败 retry 顺序与 502、首 token 超时的 retry 原因、慢客户端断开、reload 后监控继续可用、过期 Authorization 头与有效 query 凭据并存。（§24.7 起无客户端仍累计统计，历史改由 snapshot 提供。）
- M9 第三轮（§24.6 X1–X9）：时间轴新增等待段 `.tl-wait`（`--warn` 35% 不透明度，画到首块时刻；零 trunk 的已结束行画满 `durationMs`，进行中随 250ms 刷新增长）；量程向上取整到整数秒，刻度取整秒步长（3–5 个，首格强制 `0s`、末格强制 `Ns`），量程小数位变化时不重绘刻度；`chunk` 事件的 `deltaTokens` 改为 `feed` 前后累计值差分并新增 `tokensKnown` 字段；块 `title` 增加「距请求开始 N ms」。（表尾刻度在 §24.7 D2 移入表头，块高缩放于 §24.8 Y5 取消。）

### Changed

- D12 收尾（§24.11 H1/H2）：`/api/ws` 的 snapshot 现在同时包含进行中请求（`ended:false`，带完整渲染字段与已产生的 `chunksList`，单独上限 200 条、不占已结束 100 条配额），整个数组按 `ts` 降序；前端快照行可与后续 `chunk`/`end` 无缝续接并按 250ms 定时器更新耗时/游标。旧 WebSocket 路径 `/api/monitor` 已删除，带正确凭据返回 404，只保留 `/api/ws`；相关测试改写为 `/api/ws` 且能力等效。
- JSON 高级页自适应（§24.12 J）：`#json-pane` 改为纵向 flex，卡片撑满 header 以下空间，`#json-editor` 弹性填充（`min-height:240px`、`resize:none`）；全局 textarea 规则与配置页行为不变。
- OpenAI 模型语义纠错（§24.10 A1/A2）：`/v1/models` 的 `owned_by` 改为首个声明该模型的实例名（未命名实例仍回退 `model-router`），模型集合为实例顺序并集去重、不混入 Anthropic 角色名；文档明确 OpenAI 入口 `model` 就是上游模型名（纯透传），`claude-*` 只适用于 Anthropic 角色入口。§24.9 Z5 的 OpenAI 验收样本改为从 `/v1/models` 取真实模型名。
- 统计页数值可读化（§24.10 C）：TTFT/耗时 `<1000ms` 显示整数 ms、`≥1000ms` 显示 2 位小数秒、`≥3600s` 显示 1 位小数小时；累计/成功/失败/tokens/重试与实例表计数按 K/M 缩写（含进位跨单位重取，`979999 → 980.0K`、`999960 → 1.00M`），所有缩写单元格的 `title` 给出千分位精确值；运行时长、t/s、成功率、QPS 口径不变。
- 监控页第六轮（§24.9 Z1–Z5）：时间轴所有元素（等待段/块/进行中游标/重试虚线）的 tooltip 末尾追加本地时间 `· 开始 HH:MM:SS.mmm`；监控页改为纵向 flex，`.req-list` 弹性填满 header 以下高度（删除 `calc(100vh - 330px)` 固定高度），1600×1019 与 1000×900 不再出现整页纵向滚动；快照恢复的行按 `ts` 降序排列、序号按行位置显示；`monitorRequestSnapshot` 新增 `chunksList`（每条最多 400 块，超过时保留前 200 + 后 199 + 1 个合成块），使切页/刷新后绿块不再退化为蓝色；`sseState` 为 OpenAI 流新增等价判据（`delta.content` / `delta.reasoning_content` / `tool_calls[].function.arguments` 非空，本块计 1 次内容），`deltaTokens` 与 §23 日志口径不变。
- 时间轴逐行等高与内容块着色（§24.8 Y5/Y6）：删除按 `deltaTokens` 缩放块高的全部路径（`timelineTokenThreshold`、`blockHeightPercent`、`refreshRowTimelineHeights`、`rowBaseHeightPercent`、`syncWaitHeight`、`maxDelta`），`.tl-wait`/`.tl-block`/`.tl-block.tok` 统一为轨道高 40%（16px → 6.4px）居中；绿色判据由 `deltaTokens > 0` 改为本块含 `content_block_delta`。后端 `sseState` 新增按块计数的 `contentDeltas`，`chunk` 事件新增同名 int 字段（`deltaTokens`/`cumTokens`/`tokensKnown` 口径不变），tooltip 改为 `#seq · bytes B · Δn tokens · 内容块 k · 累计 c · 距请求开始 x ms`，图例文字改为「含内容块」。
- 双协议内容判据（§24.8 Y6 / §24.9 Z5）：Anthropic 流按每条 `content_block_delta` 计数；OpenAI 流按 `delta.content`、`delta.reasoning_content` 或 `tool_calls[].function.arguments` 非空的块计数（三者同现仍只 +1），`role`/`finish_reason`/空 delta/`[DONE]` 不计；该判据只影响时间轴着色与 tooltip，不改 `deltaTokens` 与 §23 日志口径。
- M10 管理面认证语义（§24.7 D4/D6）：浏览器访问 `/admin*` 未登录不再弹 HTTP Basic 401，而是 302 到 `/admin/login`；`/admin` 已登录时 302 到 `/admin/`。旧 `Authorization: Basic` 与 `/api/ws` 的 `?user=&password=` 继续可用，既有脚本与 §24 测试不受影响。`proxy_test.go` 的 `TestAdminAuth` 随规格更新为断言 302 + `Location`，其余既有断言未改动。
- M10 采集开关（§24.7 D9）：由“无客户端完全不采集”改为“无客户端仍统计与保留最近 100 条，但不广播任何事件”；逐 trunk `chunk` 事件仍只在有客户端时发送。
- `docs/design.md` 新增 §28（M10 后端：会话登录、页面路由、统计口径、全局 WS/快照、决策与未覆盖）；`docs/tests.md` 新增 §27（UT-750～UT-763 与 E6–E9/E12/E14–E16 对照）。

- `docs/design.md` 新增 §27（端点/认证、采集开关、事件模型、最小 WS 实现、请求链埋点、reload 与并发、前端认证栏与视图、决策与未覆盖）；`docs/tests.md` 新增 §23（UT-701～UT-709、FT-901～FT-910 与 A1–A9/P1–P7 对照）；README 管理页章节补充实时监控与认证栏说明。
- M9 第二轮前端结构调整（§24.5）：左右分栏改为单表格 + 每行时间轴；时间轴列使用 `table-layout:fixed` + colgroup 占满剩余宽度，表格 `min-width:1100px`，窄屏时仅在表格容器内部横滚；行高 33px（12px/11px 两行 + 上下 ≥1px 留白）；原 sessionStorage 会话凭据改为 localStorage「记住密码」持久化（默认不勾选，含明文安全提示）。`docs/design.md` §27.7/§27.8 与 `docs/tests.md` §24（UT-710～UT-722、B1–B10）同步更新。
- `docs/design.md` §27.9 与 `docs/tests.md` §26（需求 §24.6，UT-730～UT-741、C1–C12）已同步第三轮时间轴细化及 D-M9-25～D-M9-29 决策记录。

### Fixed

- M9 监控表格「请求模型 / 实例·上游」两行文字被 26px 行高纵向裁切：行高增至 33px，两行单元格改用 `line-height 1.3/1.2`、次行字号 11px 与 `overflow:visible`，不再用 `overflow:hidden` 掩盖；真实 Chrome 实测每行文字上下留白 2.11px、`td.scrollHeight=clientHeight=32`，行高 33px（≤34px），长名 `deepseek-v4-pro`/`DeepSeek/deepseek-v4-pro`、`claude-sonnet-5`/`MTI/mti-flash` 完整显示；空态卡片高度 177px（≤200px）；重试标记补充 `.retry` 类。
- M9 监控页认证断链：修复用 URL 内嵌凭据（`http://user:pass@host/admin`）打开时 `fetch('/api/config')` 抛 `TypeError`、监控反复 401 的问题（`api()` 捕获 credentials 类错误，`load()` 静默降级）。该页内认证栏与 localStorage 凭据机制已在 §24.7 被 Cookie 会话 + 全局 WS 取代。

## [0.1.1] - 2026-11-15

### Added

- M6 Anthropic 入口 `/v1/models` 对齐官方 schema：`id` 改为官方模型名（`claude-sonnet-5` / `claude-opus-5` / `claude-fable-5` / `claude-haiku-4-5`），新增 `line` / `lifecycle` / `max_input_tokens` / `max_tokens` / `capabilities` / `deprecated_at` / `retires_at` 字段。
- M6 查询参数与分页：`limit`（1–1000，默认 20）、`before_id` / `after_id` 游标、`lifecycle` 过滤（可重复/逗号分隔）；空页 `first_id` / `last_id` 为 `null`，非法参数返回 400 `invalid_request_error`。
- 新增实例配置 `role_names`（角色 → 对外模型 id）与 `model_capabilities`（上游模型名 → 窗口/输出上限/`image_input`/`pdf_input`/`thinking`/`structured_outputs`），多个实例声明时取配置顺序首个；`model_capabilities` 不影响路由与转发。
- Web 配置页支持编辑 `role_names`（每个角色行旁填写对外模型名，留空使用内置官方名）与 `model_capabilities`（按上游模型名的横向紧凑能力网格）；新增/删除/切换实例与保存回读均保留两项配置。
- `/api/probe/models` 在保留 `models` 数组的同时新增可选 `capabilities` 映射（形状同 `model_capabilities`）；配置页「查询模型」自动把上游声明的能力填入缺失字段，不覆盖用户已填值。
- `/api/probe/models` 能力解析兼容 OpenAI/DeepSeek 风格字段名：支持 `context_window` / `context_length` / `contextWindow` / `limit.context`、`max_output_tokens` / `maxTokens` / `maxOutputTokens` / `max_completion_tokens` / `top_provider.max_completion_tokens` / `limit.output`、`input_modalities` 与 `effort` / `reasoning`；Anthropic 官方字段与别名并存时官方字段优先，`input_modalities` 不含某模态时输出显式 `false`。
- M7 协议路径分离：新增 `POST /anthropic/v1/messages`（与 `/v1/messages` 同一 handler，认证/路由/回退/日志一致）与 `GET /anthropic/v1/models`（固定 Anthropic 格式）；`GET /v1/models` 固定 OpenAI 格式，不再按认证头猜测协议。两个模型列表入口均接受 `x-api-key` 或 Bearer。
- M7 OpenAI 入口能力字段：`/v1/models` 在基础四字段上按 `model_capabilities` 追加 `context_window` / `max_input_tokens`、`max_output_tokens` / `max_tokens`、`input_modalities`（恒含 `text`，按 `image_input`/`pdf_input` 追加）。同名模型去重后取第一个声明能力的实例；未声明能力的模型保持基础四字段。
- M7 配置页 OpenAI 实例新增「模型能力」紧凑表格（复用 Anthropic 侧组件），可编辑窗口/输出上限/四项能力；「查询模型」自动回填缺失字段。

### Changed

- 请求日志三档格式（§23）：从未路由到模型的请求（`GET /favicon.ico`、401 拒绝、未知路径）既不打印模型段也不打印 token 段；已路由但未拿到任何 usage 的请求保留 `-> <实例>/<上游模型> (<请求模型>)` 段、省略整段 `↑<in> ↓<out>`；有 usage 的请求维持完整格式（缺失项仍为 `n/a`，生成速率段规则不变）。上游候选全部失败时，`status=502 all candidates failed` 汇总行之前额外输出一行带最后候选实例名/上游模型名的无 token 段日志。
- 对外契约变更：Anthropic `/v1/models` 不再返回角色名；请求侧 `sonnet` / `claude-sonnet-5` 等仍按既有 R4c 角色规则路由到同一上游模型。OpenAI `/v1/models` 的基础四字段形状不变（M7 起可追加能力字段，并忽略 Anthropic 专用查询参数）。
- **破坏性变更（M7）**：`/v1/models` 不再按认证头返回 Anthropic 格式；依赖 Anthropic 模型列表的客户端需把 baseURL 指向 `.../anthropic`（即请求 `/anthropic/v1/models`）。`/v1/messages` 保留。
- `config.example.json` 补充 `role_names` / `model_capabilities` 示例；`docs/design.md`、`docs/tests.md`、`README.md` 同步 M6/M7 路径矩阵与字段表。

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
- 容器镜像名统一为 `wuzm219/model-router`（Docker Hub）：`docker-compose.yml`、README 示例、CI 推送目标同步；CI 的 `docker` job 改为登录 Docker Hub（`DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN`），未配置凭证时只构建不推送。

### Removed

- 三项原始需求确认取消并已在 `docs/requirements.md` 标注：多端口监听（FR-1）、自动查询后端可用模型（FR-2）、Anthropic「组内有序 / 跨组回退」（FR-2，已由 `roles` 角色映射 + 实例顺序候选链取代）。

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
