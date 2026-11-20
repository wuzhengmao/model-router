# 需求 §24：admin 实时请求监控（M9）

> 版本：0.1.3（dev）｜状态：待实现｜作者：主代理（产品经理）

## 24.1 背景与目标

管理页 `/admin` 目前只有配置功能。用户希望增加一个**实时请求监控视图**，形态对标浏览器 F12 的「网络」面板：能看到当前活跃的 AI 请求，并把每个 SSE trunk 的到达情况画出来。

用户原话（2026-11-15）：

> 「我想给admin增加一个监控功能，能显示当前活跃的ai请求，可以展示的类似浏览器F12的网络视图，开启后通过ws连接和后端连接，接收后端推送的消息来显示，刷新后就重新开始不用保存记录。展示的信息除了日志里的那些元素以外，主要是请求每个trunk到达通知前端画出来每个trunk到达的起始-结束，含多少token等等。」

关键约束（用户明确）：
1. 通过 **WebSocket** 连接，后端主动推送消息，前端被动渲染。
2. **不持久化**：刷新页面 = 重新开始，打开面板之前的历史请求不补发。
3. 展示的信息 = 现有日志里的元素 **+** 每个 trunk 的到达时段与 token 数。

## 24.2 功能需求

### F1 开关

- 监控默认**关闭**。管理页有「实时监控」开关，打开时建立 WS 连接并开始接收；关闭时断开 WS、清空面板。
- 只有在**至少一个 WS 客户端连接期间**，后端才为请求采集逐 trunk 明细；没有客户端时不做额外采集（不增加常态开销）。

### F2 数据范围

- 只推送**AI 请求**：即命中模型路由的请求（`POST /v1/chat/completions`、`POST /v1/messages`、`POST /anthropic/v1/messages`）。
- 非 AI 请求（favicon、401、未知路径、`GET /v1/models`）**不推送**。

### F3 事件模型

后端向每个已连接的 WS 客户端推送 JSON 文本帧，事件类型：

| type | 触发时机 | 关键字段 |
|---|---|---|
| `start` | 请求进入路由链、确定候选后 | `id`、`ts`、`method`、`path`、`protocol`(openai/anthropic)、`reqModel`、`clientIP`、`userAgent`、`instName`、`upstreamModel` |
| `chunk` | 每个上游 trunk 到达 | `id`、`seq`、`tsStart`、`tsEnd`（毫秒时间戳）、`bytes`、`deltaTokens`（该 trunk 解析出的新增 token 数，无则 0）、`cumTokens` |
| `retry` | 当前候选失败、切换到下一个候选 | `id`、`instName`、`upstreamModel`、`reason`（`error`/`first_token_timeout`/`idle_timeout`/`slow`） |
| `end` | 请求结束（成功/失败/客户端断开） | `id`、`status`、`durationMs`、`inTokens`、`outTokens`、`chunks`、`ttftMs`、`genMs`、`tps`、`instName`、`upstreamModel`、`error`（失败时） |

- 事件必须按到达顺序推送；`start` 在 `chunk` 之前，`end` 在最后。
- 单请求的 chunk 事件数量不设上限（真实流量下不会爆），但单个 WS 连接**积压超过 1024 条**未发出时丢弃该客户端连接（防止慢客户端拖垮代理）。

### F4 认证

- WS 端点复用现有 admin 认证（HTTP Basic，`/api/` 前缀已受保护）。浏览器 `new WebSocket()` 无法自定义请求头，**允许**从 query 参数 `?user=&password=` 取 Basic 凭据作为回退，但优先使用 cookie。
- 认证失败返回 HTTP 401，不升级 WS。

### F5 前端视图（对标 F12 网络面板）

- **请求列表**（左侧/主区）：每行一个请求，列：状态（进行中/成功/失败）、方法+路径、请求模型 → 实例/上游模型、入 token、出 token、总耗时、TTFT、速率（t/s）。
  - 进行中的请求实时更新耗时与已收 chunk 数。
- **选中详情**（右侧/下方）：
  - 顶部信息条：方法、路径、协议、客户端 IP、User-Agent、开始时间、状态码、in/out token、生成速率。
  - **Trunk 时间轴**（核心）：横轴时间（相对请求开始的毫秒），每个 chunk 画成一段色块 `[tsStart, tsEnd]`，块内/悬停显示 `bytes` 与 `deltaTokens`。连续 trunk 应能看出「间隙」（TTFT 空隙、卡顿空隙、重试切换点）。
  - 重试切换点在时间轴上用竖线标注，并显示新的实例/上游模型。
- 视觉要求：**紧凑、信息密度高**，不要大卡片、不要过高行高；与现有管理页风格一致（现有 CSS 变量/配色）。
- 列表最多保留最近 500 个请求（内存，DOM 同步裁剪），超出后丢弃最旧的已完成请求。

## 24.3 验收标准（可机械验证）

| # | 标准 |
|---|---|
| A1 | `WS /api/monitor` 在无 admin 密码时返回 404；认证失败返回 401 |
| A2 | 无 WS 客户端连接时，发起 AI 请求不影响响应体与日志（现有全部测试仍绿） |
| A3 | 客户端连接后发起一次流式请求，能收到 `start` → 若干 `chunk` → `end` 序列，且 `chunk` 数等于上游实际 deltas 数 |
| A4 | 非 AI 请求（`GET /favicon.ico`、401 的 AI 请求）不产生任何事件 |
| A5 | 接口全部候选失败时，`retry` 事件按候选顺序出现，`end` 的 `status`=502 |
| A6 | 刷新页面后重新连接，不会收到连接前的历史事件 |
| A7 | 静态检查：`go build ./...`、`go vet ./...` 退出码 0；`gofmt -l` 无输出；`go test ./... -race -count=1` 全绿 |
| A8 | 新增单元测试至少覆盖：事件不泄露给未连接客户端、chunk 序列计数、慢客户端断开、认证拒绝 |
| A9 | 现有 130+ 用例全部保持通过，不改动既有断言 |

## 24.4 非目标

- 不保存历史（无数据库、无文件）。
- 不回放、不导出 HAR。
- 不做 token 级粒度（只到 trunk 粒度）。
- 不改动 `attemptMetrics` 的现有语义与日志格式（§23 三档规则必须原样保留）。

---

## 24.5 第二轮调整（用户 2026-11-16 验收反馈）

用户原话：

> 「右边不应该只显示选中行，应该每行都显示一条，横轴是时间轴，但不是用绝对的时间，而是从请求开始0 - 请求结束的时间。随着请求变多，横轴的时间长度可以动态的变化，取耗时最长的请求。再增加一个清空的按钮点击可以清空已经完成的请求。左侧和右侧的条应该一一对齐，最好就合成一个表格，表格左边是统计项，右边是横条。另外使用用户名/密码登录的方式，增加一个记住密码的功能，之后连接ws也就不存在鉴权的问题了。」

### W1 合并为单表格（替换 F5 的左右分栏布局）

- 原「左请求列表 + 右选中详情」两栏布局**整体删除**，合并为**一张表格**。
- 表格列顺序固定：`#` ｜ 状态 ｜ 方法/路径 ｜ 请求模型 / 实例·上游 ｜ 入 ｜ 出 ｜ 块 ｜ 耗时 ｜ TTFT ｜ t/s ｜ **trunk 时间轴**。
- 左侧统计列与右侧时间轴**在同一行内一一对齐**（同一 `<tr>` 内两个单元格，行高一致，滚动时绝不脱节）。
- 时间轴列宽度随视口自适应，占据剩余宽度（`table-layout: fixed` + colgroup，避免行间列宽抖动）。
- 行高保持紧凑（≤28px）；时间轴单元格内不换行。

### W2 每行独立时间轴（相对 0 → 请求结束/当前位置）

- **每一行**都画自己的 trunk 条，不再只有选中行有。
- 横轴为**相对时间**：`0` = 该请求的 `start.ts`（请求开始），终点 = 该请求的 `durationMs`（`end` 到达后的总耗时）；**未结束的请求**终点 = `Date.now() - start.ts`，并随 250ms 定时器**实时增长**（块随时间往右推进）。
- 每个 chunk 仍画成一段色块 `[tsStart - start.ts, tsEnd - start.ts]`；含新 token 的块用不同颜色。
- 重试切换点在该行时间轴上画竖虚线（保留原 tooltip 语义）。
- 进行中的行：时间轴右端可有一条"当前时刻"游标（竖向细线）。

### W3 横轴统一量程（取耗时最长的请求）

- **所有行共用同一条横轴量程**：`axisSpan = max(已结束请求的 durationMs, 进行中请求的累计耗时, 100ms)`。
- 量程**随请求变化动态更新**：出现更长耗时的请求时，所有行的块按新量程**整体缩放**（百分比布局，DOM 不重建也算得对）。
- 行内块宽度用百分比：`left = start / axisSpan * 100%`，`width = (end - start) / axisSpan * 100%`。
- 量程变化后，表格下方/表头的横轴刻度（0、1/4、1/2、3/4、总长）同步重算。
- 量程不因清空或行被裁剪而停在旧值（清空后重置为最小值）。

### W4 清空按钮

- 监控工具栏新增「清空」按钮（id `monitor-clear`）。
- 点击后**只清空已完成（ended）的请求**；**进行中的请求保留**，WS 连接保持不断开。
- 若清空后无任何行，则恢复空状态提示。
- 该按钮与「断开」不同：断开 = 关 WS + 清空全部 + 清凭据；清空 = 仅清已完成行，不动连接与凭据。

### W5 记住密码（替换 F4 的 sessionStorage 行为）

- 登录栏增加「记住密码」复选框（id `monitor-remember`，默认**不勾选**）。
- 勾选后点「连接」成功 → 凭据写入 **localStorage**（键沿用 `model-router.monitor.cred`）。
- 未勾选 → 凭据不落盘（沿用当前行为，仅在内存中用于本次连接）。
- 页面加载时若 localStorage 有凭据：自动填入用户名/密码输入框、勾选「记住密码」，并且**进入监控页时自动连接**（无需再点连接）。
- **取消勾选**或**认证失败**时，立即从 localStorage 删除凭据并清空密码框。
- 「断开」按钮：关 WS，但**不删** localStorage 中已记住的凭据（下次进入自动重连）；只有取消勾选/认证失败才删。
- 安全提示文案：明确写着"密码以明文保存在本机浏览器 localStorage，仅建议在可信设备使用"。

### W6 兼容与不变量（必须保持）

- 后端 `monitor.go` **零改动**（事件字段已足够：`ts`、`tsStart`、`tsEnd`、`durationMs`、`retry`）；如需新字段必须在 criteria_map 说明为什么。
- WS 认证仍走 `?user=&password=` 握手回退（浏览器限制不变）。
- 上一轮已验收的 V1–V8（长模型名不截断、数字列不截断、窄屏无横向滚动等）在新表格中**继续成立**。
- 刷新页面仍不补发历史（A6）。

### 第二轮验收标准

| # | 标准 |
|---|---|
| B1 | 页面上不存在左右分栏（`.monitor-wrap` / `.monitor-card-list` / `.monitor-card-detail` 被删除或不再使用）；只有一个表格，统计列与时间轴在同一 `<tr>` |
| B2 | 真实数据下每一行都有 trunk 条（不是只有选中行）；同一行内统计单元格与时间轴单元格 `getBoundingClientRect().top` 差值 ≤ 1px |
| B3 | 横轴量程 = 最长耗时请求；构造 2 条耗时相差 4 倍的请求，短请求的块右端约为长请求的 25%（±3%） |
| B4 | 进行中的请求时间轴终点随时间增长（间隔 1s 采样两次，终点像素值增大） |
| B5 | 点「清空」后：已完成行消失、进行中行仍在、`monitor.ws.readyState === 1`（连接未断） |
| B6 | 「记住密码」勾选连接成功后 localStorage 有 `model-router.monitor.cred`；刷新页面后凭据自动填入且自动连接成功；取消勾选后 localStorage 该项被删除 |
| B7 | 不勾选「记住密码」时连接成功后 localStorage 无该项 |
| B8 | 1600×1019 真实 Chrome：模型名列、数字列、时间轴列均无截断（`scrollWidth <= clientWidth`）；表格无横向滚动条 |
| B9 | 1000×900 窄屏下页面无横向滚动（表格容器内部可横向滚动） |
| B10 | `go build ./...`、`go vet ./...` 退出码 0；`gofmt -l` 无输出；`go test ./... -race -count=1` 全绿 |

---

## 24.6 时间轴第三轮细化（用户 2026-11-16 反馈）

### 用户原话

> 「另外条前面空的一块是表示等待响应么？是的话也需要加上颜色，这样才能看到等待了多久。另外时间轴的标题栏要显示时间，时间轴几个字可以不要，最左边是0s，最右边应该是最长的响应时间，补齐到整数秒，中间画几个刻度。」

### X 组决策（🔒 必须逐条实现）

**X1 等待段着色（TTFT 可视化）**

- 每行的 trunk 时间轴分两段渲染：
  - **等待段**：从 `0`（请求开始）到该行**首个 trunk 到达时刻**。首块时刻取 `req.chunksList[0].s`（相对坐标，单位 ms，后端事件 `tsStart` 已提供）。
  - **传输段**：从首块时刻到该行终点（已结束用 `durationMs`，进行中用当前时刻）。
- 等待段必须有可见颜色（新建 `.tl-wait`，建议使用 `--warn` 或低饱和度的强调色，与蓝/绿块明显区分），并加 `title` 提示「等待首包 xx ms」。
- 等待段宽度 = `首块时刻 / axisSpan * 100%`；**已结束但零 trunk 的请求**（首包超时/直接失败）等待段覆盖整段 `durationMs`。
- 进行中且**尚未收到任何 trunk** 时，等待段从 0 延伸到当前时刻（随 250ms 刷新增长）。
- 等待段与 trunk 块不重叠：trunk 块仍按原坐标绘制，等待段绘制在其下层（`z-index` 低于 `.tl-block`）。
- 图例文案更新为「等待首包 · trunk 蓝 · 含新 token 绿 · 虚线 重试」（原「trunk 蓝 · 含新 token 绿 · 虚线 重试」，位置不变）。

**X2 时间轴标题栏改显示量程（去掉「时间轴」字样）**

- 表头第 11 列（`internal/web/index.html` 第 480 行 `<th>trunk 时间轴</th>`）改为只显示量程：`<th class="req-axis-head"><span id="monitor-axis-title">0s</span></th>`，**不再出现「时间轴」/「trunk」字样**（用户原话「时间轴几个字可以不要」）。
- `#monitor-axis-title` 内容为 `0s – <整数秒>s`，随量程实时更新；量程 < 1s 时写 `0s – 1s`。
- 该文本仅作说明，不参与像素定位。

**X3 量程取整（右端补齐到整数秒，左端必须是 0s）**

- `axisSpan` = 所有行最长耗时（原 W3 逻辑不变），再**向上取整到整数秒**：`displaySpan = Math.max(1000, Math.ceil(axisSpan / 1000) * 1000)`。
- 刻度与块定位一律用 `displaySpan` 计算百分比，保证最右刻度文字恰好是「Ns」。
- 最左端刻度文字**必须是 `0s`**（不是 `0ms`）；其余刻度文案统一用秒口径，如 `1s`、`2s`（整数秒刻度不带小数）。
- 量程变化导致 `displaySpan` 变化时，所有行同步重定位（沿用 W3 的整体缩放逻辑）。

**X4 中间刻度**

- 中间至少 3 个、最多 5 个刻度（含两端），取「整秒」步长：设整数秒总量 `S = displaySpan / 1000`，选择步长 `step ∈ {1,2,5,10,15,20,30,60,120,300}` 中使 `S/step ≤ 4` 的最小值，刻度序列 `0, step, 2*step, … 直到 ≤ S`，最后一格强制落在 `S`。
- 例：`S=4` → 刻度 `0s,1s,2s,3s,4s`；`S=7` → step=2 → `0s,2s,4s,6s,7s`；`S=23` → step=10 → `0s,10s,20s,23s`。
- 首尾刻度沿用现有 CSS（`.row-axis span:first-child` 不偏移、`:last-child` 右对齐），中间刻度居中偏移，禁止互相重叠（若相邻刻度文字宽度会重叠，则按上述 step 规则自动升档）。
- 刻度不再随 `axisSpan` 亚像素级变化抖动：只有 `displaySpan` 真正变化时才重绘刻度。

**X5 与既有不变量**

- W6 中「后端 `monitor.go` 零改动」条款**由 24.6 的 X6 取代**；其余 W1–W5 行为不变。
- W1（单表格）、W2（每行一条、相对起点）、W4（清空只清已完成）、W5（记住密码）行为不变。
- V1–V8、B1–B10 继续成立；时间轴列不得出现横向滚动或裁切。

### 第三轮验收标准

| # | 标准 |
|---|---|
| C1 | 某行已收 3 个 trunk 时，该行时间轴内存在 `.tl-wait` 元素，其宽度 ≈ `首块时刻 / 量程 × 100%`（±2%），且 `title` 含「等待」字样 |
| C2 | 等待段颜色与 trunk 块颜色不同（计算样式的 `background-color` 不相等） |
| C3 | 已结束且零 trunk 的失败行，`.tl-wait` 覆盖整行宽度（±2%） |
| C4 | 表头第 11 列显示 `0s – <N>s`（`N` 为整数且 ≥ 表中最大耗时秒数），且该列不出现「时间轴」「trunk」字样 |
| C5 | 最左刻度文字严格等于 `0s`；最右刻度文字等于 `<N>s`，且位置与表头标题的 N 一致 |
| C6 | 构造最大耗时 3.4s 的请求：量程显示 `4s`，刻度数 ≥3 且 ≤5，刻度全部落在整数秒 |
| C7 | 量程在小数位变化（如 3.02s→3.08s）时，刻度文本不变（不抖动）；跨过整数秒（3.99s→4.01s）时量程变为 5s 并重绘 |
| C8 | 1600×1019 与 1000×900 下，时间轴列无裁切，刻度文字之间不重叠（相邻刻度边界框不相交） |
| C9 | `go build ./...`、`go vet ./...` 退出码 0；`gofmt -l` 无输出；`go test ./... -race -count=1` 全绿 |

### 24.6 补充决策（X6–X9，主代理规格化）

**X6 每块 token 数（量化 trunk 块高度）**

- 后端 `internal/proxy/monitor.go` 把「每块 token 数」改为真实增量，替代当前无意义的 `delta = cumTokens - 上块 cum`：
  - 现有 `sseState.tokens` 是**累计 token 数**（流式里从文本长度估算），不是本块增量。
  - 改为在 `copySSE` 逐块处理时取 `before = outcome.state.tokens` 作为块起点，`outcome.state.feed(buf[:n])` 之后再读 `after = outcome.state.tokens`，本块增量 `deltaTokens = max(0, after-before)`；`cumTokens` 仍为 `after`。
  - `hasTokens=false`（非解析/非流式）时 `deltaTokens = 0`，前端据此按「未知」渲染。
  - 后端内部保留「本块是否已知 token」（可用 `deltaTokens` 之外的新布尔字段，如 `tokensKnown`，由后端决定），前端在**从未见过非零 delta 的行**上统一按最小高度绘制，避免同列内出现无意义的高度差。
- 事件字段名、类型不变（`deltaTokens` / `cumTokens`），**前端 W6 的「后端零改动」条款由本决策取代**。
- 语义澄清：这是**按字符估算的 token 数**（与 §15 日志里 `↑↓` 的 usage 口径不同、量级也不同），只用于在时间轴上表达「这块带来了多少内容」，不参与计费与速率判定。

**X7 块高度随 token 数缩放 —— 已被第五轮 Y5 取代，本条作废**

- ~~`.tl-block` 高度按 `deltaTokens` 在两档之间连续缩放~~ → 第五轮 Y5 改为**逐行等高**：一行内所有块（含等待段）高度完全一致，不再随 `deltaTokens` 变化。
- `deltaTokens` 仍然保留在事件与 tooltip 里（X6/X8 不受影响），只是不再驱动高度。
- 下列实测样本仅作 X6 的 `deltaTokens` 量级参考：`Δ0:4  0:7  0:8  0:8  0:8  0:8  0:2  0:3  0:7  0:2  0:3  0:3  0:10  1:1  1:10  1:2  1:2  1:0  1:3  1:1  1:1  1:1  1:2  1:2  1:0  1:2  1:0  1:4  1:4  4:1  4:5  32:65`（格式为 `deltaTokens:字节数/16`）。

**X8 气泡提示里的等待时长**

- `.tl-block` 的 `title` 增加「等待」一栏：`#seq · bytes B · Δn tokens · 累计 c · 距请求开始 x ms`（`x` = 本块 `tsStart - req.ts` 的整数毫秒），便于核对首包等待。

**X9 验收补充**

| # | 标准 |
|---|---|
| C10 | 真实流式请求中，`chunk` 事件的 `deltaTokens` 不再出现「除首块外恒为 0」；同一请求内 `Σ deltaTokens == end 事件的 cumTokens/输出 token 估算值`（±0） |
| C11 | ~~同一行内不同块的高度不同~~ **已被第五轮 Y5 取代**：同一行内所有块与等待段高度必须相等（最大差 ≤ 0.5px） |
| C12 | `.tl-block` 的 `title` 含「距请求开始」字样 |

---

## 24.7 第四轮：管理员界面重构（用户 2026-11-16 反馈）

### 用户原话（逐条对应 D1–D10）

> 「1、时间轴条的黄、兰、绿三种颜色的高度都没有对齐，应该和蓝色的高度一直」
> 「2、表格最下面表尾的时间轴的0s .... 移到最上面标题栏这里，这里把左边的图例移过来，另外原本图例的色块太小了，就一个小点根本看不清，应该和实际的高度一致，宽度和高度也一样一个正方形的样子」
> 「3、坐标的表格要显示发起的客户端和来源IP，可以两层合并到一列里，然后客户端显示的时候版本号去掉这样可以短一点，鼠标移上去才显示完整的，把模型这列的宽度让一点出来，如果还是不够，可以从时间轴这里挤一点空间出来」
> 「4、网站需要登录的功能，而不是使用http协议的认证功能，登录页面输入用户名密码，可以选择记住，记忆后30天内可以自动登录进入首页」
> 「5、这样监控页面的登录功能可以去掉，连接应该进入页面自动连接，也不存在需要连接断开的功能」
> 「6、每个页面应该独立路由，这样不至于每次刷新页面都回到首页」
> 「7、首页应该放一个统计页面，实时显示统计数，统计由后端提供，变化时通过ws告诉前端更新，至于可以做那些统计值可以你可以根据经验先设计一下做出来再看」
> 「8、监控页面放在首页后面，配置和JSON修改页面放在最后，每个页面都应该有独立的路由」
> 「9、ws连接应该登录完就连接上，然后断线可以自动重连，接收统计数据和实时监控数据，以便于切到实时监控画面不至于每次都是空的」
> 「10、为了避免存量实时监控数据太多，就保留最近100条的，切到实时监控时显示的不限制100条，觉得太多了有清空按钮可以清理掉已经完成的请求」

### D 组决策（🔒 必须逐条实现）

**D1 时间轴三种颜色高度统一（修复对齐 bug）**

- 现状 bug：`.tl-wait` 是 `top:0;bottom:0`（铺满 16px 轨道），`.tl-block` 是 `top:50%;height:<40%~100>%;translateY(-50%)`（居中、可变高）。
- 目标：**等待段、普通 trunk 块、含新 token 的绿色块，三者使用同一高度基准，且都与块的实际高度一致**。
- 实现约定：所有段统一用「居中 + 百分比高度」定位（`top:50%; transform:translateY(-50%)`），高度由同一个计算函数产出；等待段的 `opacity` 不得导致视觉高度变小（即高度按同一百分比，不额外 inset）。
- 块高**不再随 Δ tokens 缩放**（第五轮 Y5 取代 §24.6 X7）：等待段、蓝块、绿块，以及**任何行内所有块**高度一律相等，取时间轴轨道的基础高度 `--tl-track-h × 40%`（16px × 40% = 6.4px）；行与行之间可以不同（实际由同一常量决定，故也相同）。

**D2 表尾刻度与图例移入表头，图例色块改为真色块**

- 删除 `<tfoot>` 整行（含 `#monitor-axis` 与 `.axis-hint` 文本图例）。
- 第 11 列表头改为两行结构：
  - 第一行：`<span id="monitor-axis-title">0s – Ns</span>`
  - 第二行：**刻度尺**（复用原 `.row-axis` 的百分比定位与首尾对齐规则，容器 id 改为 `#monitor-axis`），紧贴第一行下方。
- 图例移到**表头左侧区域**（第 11 列表头内刻度的左边，或表头上方一行），元素为**三个真实色块 + 说明文字**：
  - 色块尺寸 = **正方形**，边长等于时间轴上块的**实际渲染高度**（用 CSS 变量或内联值保证一致，不得写死成小圆点）。
  - 三个色块分别对应：等待首包（琥珀）、trunk（蓝）、含新 token（绿）；重试用竖直虚线示例块。
- 表头总高度增加不得超过 12px（原 tfoot 高度被释放，视觉总量应持平或更紧凑）。

**D3 新增「客户端」列（来源 IP + 客户端/UA 两层合并）**

- 表格新增一列，位置在「方法/路径」之后、「请求模型 / 实例·上游」之前：
  - 上层（12px）：**来源 IP**（如 `172.18.0.1`）
  - 下层（11px，灰）：**客户端标识**——从 `User-Agent` 解析出 `名称/主.次`，**去掉 patch 与之后的版本号**。
- UA 解析规则（🔒 按下表，首个匹配生效）：

  | UA 特征 | 显示 |
  |---|---|
  | 含 `deepseek-harness/` | `deepseek-harness/0.2.2`（取前两段） |
  | 含 `claude-code/` | `claude-code/2.1` |
  | 含 `Go-http-client/` | `Go-http-client/1.1`（原样，仅两段） |
  | 以 `curl/` 开头 | `curl/8.4` |
  | 含 `Mozilla/5.0` 且含 `Chrome/` | `Chrome/131` |
  | 含 `Mozilla/5.0` 且含 `Firefox/` | `Firefox/132` |
  | 含 `Mozilla/5.0` 且含 `Safari/` 且无 Chrome | `Safari/604` |
  | 含 `python-requests/` | `python-requests/2.31` |
  | 含 `python-httpx/` | `python-httpx/0.27` |
  | 以上都不匹配 | 取 UA 第一个 token，截断到 24 字符；UA 为空显示 `–` |

- **截断保底规则**：按上表产出后若长度 > 18 字符，再截到 18 字符并加 `…`。
- 下层单元格的 `title` 属性 = **完整原始 User-Agent**；上层单元格 `title` = `来源 IP`。整列单元格 `title` 都不得为空。
- 列宽不从模型列硬抢：新列宽度 138px，**来源 = 模型列 210px → 184px（让出 26px）+ 时间轴列自适应吸收剩余**（时间轴是 `1fr`，不受影响）。
- 若 1600px 宽度下模型列首个模型名出现截断，则继续从时间轴列 `padding` 里回收（`td.req-timeline { padding: 0 6px }` → `0 4px`），**不得让时间轴列出现横向滚动**。

**D4 基于 Cookie/Session 的登录（替换 HTTP Basic）**

- **登录页** `GET /admin/login`：独立 HTML 页面（可用同一个 index.html 内的路由，也可独立资源），含用户名、密码、「记住我」复选框、登录按钮、错误提示区。
- **登录接口** `POST /api/login`：body `{"user":"…","password":"…","remember":true|false}`。
  - 凭据校验仍用 `adminCredentialsMatch`（常量时间比较 `cfg.AdminUser`/`cfg.AdminPassword`）。
  - 成功 → `Set-Cookie` 下发会话 cookie，返回 200 + 跳转地址（`{"redirect":"/admin"}`）。
  - 失败 → 401，**不设 cookie**，响应体不含任何凭据信息。
- **会话 cookie 规格**：
  - 名称 `mr_session`；值 = 服务端生成的**不透明随机 token**（≥ 32 字节 CSPRNG，base64url 编码），**不得是用户名/密码或其哈希**。
  - `HttpOnly`、`SameSite=Lax`、`Path=/`；**不加 `Secure`**（本地 HTTP 部署需可用），但要在响应头注释与文档里说明。
  - **勾选「记住我」** → `Max-Age=2592000`（30 天），服务端会话表也记 30 天；
  - **未勾选** → 无 `Max-Age`（会话 cookie，关浏览器即失效），服务端会话表记 12 小时。
- **会话存储**：进程内存 `map[token]session{user, expiresAt}` + `sync.RWMutex`；不落盘（重启即全部失效，符合「不持久化」的既有原则）。
- **过期清理**：每次鉴权顺手清理过期项；另可低频（如每分钟）批量清理，避免无限增长。
- **登出** `POST /api/logout`：删除服务端会话、下发清除 cookie（`Max-Age=0`），返回 200。
- **鉴权优先级**（`ServeHTTP` 里按序）：
  1. 路径是 `PathLogin` / `PathLoginPage` / 静态登录所需资源 → 放行（无需登录）；
  2. 有效 `mr_session` cookie → 放行；
  3. **保留** `Authorization: Basic` 与 `?user=&password=` 两种旧方式（供脚本/自动化与既有测试使用）→ 放行；
  4. 页面请求（`Accept` 含 `text/html` 或路径是 `/admin*`）→ **302 重定向到 `/admin/login`**；
  5. API 请求（`/api/*`、WS 升级）→ 401（`WWW-Authenticate` 头可保留 Basic 以免破坏既有测试，但**浏览器页面不再触发 Basic 弹窗**）。
- `admin_password` 为空 → 整个 admin 面仍 404（保持既有语义）。

**D5 监控页去掉登录栏，进入页面自动连接**

- 删除 `internal/web/index.html` 的全部监控认证 UI：`#monitor-auth-user`、`#monitor-auth-password`、`#monitor-auth-connect`、`#monitor-auth-disconnect`、`#monitor-auth-msg`、「记住密码」复选框 `#monitor-remember`、`localStorage['model-router.monitor.cred']` 相关逻辑、`startMonitor`/`stopMonitor` 的凭据解析（`resolveMonitorCredential`）、`handleMonitorHandshakeFailure` 的认证失败分支。
- 连接生命周期由**全局 WS 管理器**（见 D9）负责：登录进入任一 admin 页面即建立，切到监控页只是订阅渲染，不再另行连接。

**D6/D8 独立路由与页面顺序**

- 路由表（🔒 固定）：

  | 路径 | 页面 | 说明 |
  |---|---|---|
  | `/admin` | 302 → `/admin/` | 兼容旧书签 |
  | `/admin/` | **统计首页**（默认页） | 第 1 个标签 |
  | `/admin/monitor` | 实时监控 | 第 2 个标签 |
  | `/admin/config` | 可视化配置 | 第 3 个标签 |
  | `/admin/json` | JSON 高级 | 第 4 个标签 |
  | `/admin/login` | 登录页 | 无导航栏 |

- 顶部导航顺序**固定为**：`统计`、`实时监控`、`配置`、`JSON 高级`；当前页高亮。
- **刷新任意页面必须停留在该页**（服务端路由，不是 `#hash`，不是 SPA 前端 `history.pushState`）。
- 未登录访问 `/admin/*` → 302 `/admin/login`；登录成功 → 回到 `?next=` 指定的页面（缺省 `/admin/`），`next` 必须校验为以 `/admin` 开头的站内路径，**拒绝 `//` 与外部 URL**（防开放重定向）。
- 静态资源（若有）走 `/admin/static/*`，同样无需登录。

**D7 统计首页（指标由主代理设计，用户授权「你可以根据经验先设计一下」）**

首页展示下列指标，**全部由后端计算**，分三块布局（紧凑卡片网格，非大卡片）：

- **A. 运行与连接**
  | 指标 | 口径 |
  |---|---|
  | 运行时长 | 进程启动至今（`time.Since(startedAt)`），格式 `2d 3h 14m` |
  | 版本 | `internal/version.Get()` |
  | 活跃 WS 客户端数 | 当前 `/api/ws` 连接数（原子计数） |
  | 活跃请求数 | 生成时未结束的 AI 请求数 |
  | 当前 QPS | 最近 60 秒内 AI 请求数 / 60，保留 1 位小数 |

- **B. 请求统计**
  | 指标 | 口径 |
  |---|---|
  | 累计请求数 / 成功 / 失败 | `end.status` 2xx/3xx 记成功，其余失败；失败含 `end.error != ""` |
  | 成功率 | 成功 / 累计，1 位小数百分比 |
  | 按协议 | `openai` / `anthropic` 各自累计数 |
  | 首包延迟 P50 / P95 | 已结束请求的 `ttftMs` 分位数，单位 ms（样本 < 10 时显示 `–`） |
  | 总耗时 P50 / P95 | `durationMs` 分位数 |
  | 输入 / 输出 tokens 累计 | 全量累加 |
  | 平均生成速率 | 全部有 `genMs>0` 请求的 `Σ outTokens / Σ genMs × 1000`，1 位小数 |
  | 重试次数 | `retry` 事件累计数 |

- **C. 实例统计**（按 `instName` 分组，一行一个实例）
  | 指标 | 口径 |
  |---|---|
  | 请求数 / 占比 | 该实例承担的请求数与占比 |
  | 成功 / 失败 | 按该实例 `end.status` 归类 |
  | 平均 TTFT | 该实例 `ttftMs` 均值 |
  | 平均 t/s | 该实例 `Σ outTokens / Σ genMs × 1000` |
  | 降级状态 | 当前 `isDegraded(now)` → `正常` / `降级`（红色） |

- **D. 近期错误**（表格，最多 20 行，倒序）
  | 时间 | 状态码 | 实例/上游 | 模型 | 错误摘要（截断 80 字） |

- **统计口径与保活规则**（🔒）：
  - **累计值**：进程启动起累积，**并落盘持久化**（口径见「D11 统计持久化」，重启后恢复）。
  - **分位数**：保留最近 **1000** 个样本的滑动窗口（环形缓冲），超出丢最旧。
  - **推送时机**：数据变化后**至少每 2 秒推送一次**（节流合并），且变化时尽快推送；无变化时**每 10 秒**推一次心跳式快照，保证新连上来的客户端有值。
  - WS 消息格式：`{"type":"stats","ts":<毫秒>,"data":{...}}`，`data` 结构与 `GET /api/stats` 返回体**完全一致**（同一序列化函数）。
  - 另提供 `GET /api/stats`（需登录）返回同一快照，供刷新首屏与服务端自测使用。

**D9 全局 WS 连接（登录即连、断线自动重连、双流复用）**

- 新端点 `/api/ws`（替代 `/api/monitor`，**旧端点保留一个版本周期**，行为等同，避免破坏既有测试与脚本）。
- **浏览器端全局单连接**：
  - admin 任一页面加载后**立即建立**（不依赖当前在哪个标签页）。
  - 断线后**自动重连**：退避 1s → 2s → 4s → 8s → 最大 15s，重连成功后重置为 1s；页面 `hidden` 时不主动断开。
  - 连接状态在顶栏 `#status` 区域显示：`已连接` / `重连中(第 N 次)`。
- **服务端保留最近 100 条**（D10）：
  - 环形缓冲保存**最近 100 条已结束请求**（`end` 事件快照，含 start/chunk 摘要所需的最小字段）。
  - **新客户端连接时**：立即推送一帧 `{"type":"snapshot","requests":[...],"stats":{...}}`（requests ≤ 100 条，按时间倒序），随后推 `stats` 与实时事件。
  - 切到监控页时**不再空白**——历史来自该快照，实时来自既有事件流。
  - 前端对 snapshot 与实时事件**去重合并**（按 `id`）；snapshot 里 `ended=true` 的请求不允许被后续事件覆盖成进行中。
- **采集开关语义变更（🔒 重要）**：
  - 原「无 WS 客户端则完全不采集」（`newRequest` 里 `if !h.hasClients() { return nil }`）**改为**：无客户端时**仍维护计数/统计累加与最近 100 条环形缓冲**，但**不广播**逐 trunk 事件。
  - 逐 trunk 的 `chunk` 事件才受「有客户端」开关控制；`start`/`end` 事件必须始终记录到环形缓冲（这是统计与快照的数据源）。
  - 这项变更**必须**保留「无客户端时零广播开销」的特性（无客户端时不调用任何 WS 相关写操作）。

**D10 前端行数上限与会话内行为**

- 前端内存上限仍是 **500 行**（既有 `MONITOR_MAX_REQUESTS`），但首次加载时从 snapshot 拿到**至多 100 条**历史，之后实时累加。
- 「清空」按钮语义不变（只清已完成行，不动 WS 连接）。**同时清空后端的最近 100 条环形缓冲**（新增 `POST /api/monitor/clear`，需登录），否则刷新页面历史会回来。
- 切走标签页**不清空**任何数据；只有点击「清空」或刷新页面才重新开始（刷新后从后端环形缓冲恢复最近 100 条）。

**D11 统计持久化（统计落盘，用户 2026-11-16 补充需求）**

用户原话：「统计值我希望能落盘，另外我希望后端能缓存最近100条ai请求的数据（这个不用落盘），这样每次前端连接上之后能立刻有显示数据。」

- **落盘内容**：只有**累计统计**落盘，即 D7 的 B/C 组累计值（累计请求数/成功/失败、按协议计数、输入/输出 tokens 累计、重试次数、各实例的请求数/成功/失败/ΣoutTokens/ΣgenMs/ΣttftMs）+ **近期错误表**（最多 20 条）。
- **不落盘**：D9 的最近 100 条已结束请求（用户明确「这个不用落盘」）、P50/P95 分位数的 1000 样本环形窗口、活跃连接数、活跃请求数、当前 QPS、运行时长（运行时长改为「服务启动起的时长」，不从统计文件推算）。
- **落盘位置**：与配置文件同目录的 `stats.json`（`filepath.Dir(configPath)`）。配置文件路径为 `/home/nonroot/config/config.json` 时即 `/home/nonroot/config/stats.json`，落在既有挂载目录内，**不需要新增 Docker 卷**；写入用 `os.CreateTemp` 同目录临时文件 + `Rename` 原子替换，失败只记日志、不影响请求转发。
- **写入时机**：节流写盘，距上次落盘 ≥ 5 秒且有变化才写；进程正常收到 SIGINT/SIGTERM 时补写一次；不强求崩溃安全（最多丢 5 秒数据）。
- **加载时机**：`NewServer` 时读取；文件不存在视为全零正常启动；JSON 解析失败或字段非法 → 记一条 warning 日志、以全零启动，并把旧文件重命名为 `stats.json.bad` 保留现场，**不得阻止进程启动**。
- **字段版本**：文件带 `"schema": 1`；读到不认识的 schema 版本按「解析失败」处理（备份为 `.bad` 后全零启动）。
- **清空接口**：`POST /api/stats/clear`（需登录）把累计值与近期错误清零并立即落盘；**不**影响最近 100 条环形缓冲（那是 D10 的 `/api/monitor/clear`）。两个清空互不调用。
- **实例统计的键**：以实例 `name` 为键（实例改名后新旧两条并存，不做迁移）；已从配置中删除的实例，其历史计数在 D7 的 C 组中仍列出并标注 `已移除`。

**D12 最近 100 条历史快照（内存，不落盘）**

用户原话：「我希望后端能缓存最近100条ai请求的数据（这个不用落盘），这样每次前端连接上之后能立刻有显示数据。」

- 即 D9 已定的环形缓冲，本次补充明确：**只存已结束（`end`）的请求，容量固定 100，不落盘、重启清零**；放在内存 `[]monitorEndEvent` + `sync.RWMutex` 环形写。
- 快照里的每条记录必须带足以直接渲染表格行的字段：`id`、`path`、`method`、`clientIP`、`ua`、`reqModel`、`instName`、`upstreamModel`、`statusCode`、`inTokens`/`hasIn`、`outTokens`/`hasOut`、`chunks`、`durationMs`、`ttftMs`、`genMs`、`tps`、`startTs`、`ended=true`、`error`。
- 快照帧在 WS 握手成功后**立即**发送（先于任何实时事件），保证「连上就有数据」而非等到下一条请求才有画面。

### 第四轮验收标准（可机械验证）

| # | 标准 |
|---|---|
| E1 | `.tl-wait`、`.tl-block`、`.tl-block.tok` 三者在**每一行内**的 `getBoundingClientRect().height` 与 `top` 各自全部相等（最大差 ≤ 0.5px）；至少有一行同时含等待段、蓝块、绿块。依据第五轮 **Y5**：块高不再随 Δ tokens 缩放（§24.6 X7/C11 的块高差异部分已作废），`deltaTokens` 只保留在 tooltip 中 |
| E2 | 页面中不存在 `<tfoot>`；`#monitor-axis` 位于第 11 列表头内部（`closest('thead') !== null`）；表头文本含 `0s – Ns` |
| E3 | 表头图例含 3 个 `.tl-swatch` 色块元素，每个 `getBoundingClientRect()` 的 `width === height`（误差 ≤ 0.5px），且边长等于时间轴块的基础渲染高度（误差 ≤ 1px，即 16px 轨道 × 40% ≈ 6.4px）；三色块背景色互不相同 |
| E4 | 表格存在「客户端」列，位于「方法/路径」与「请求模型 / 实例·上游」之间；每行上层显示 IP、下层显示截断后的客户端标识（`claude-code/2.1.323` → `claude-code/2.1`、`deepseek-harness/0.2.2-rc.1` → `deepseek-harness/0.2.2`、`curl/8.4.0` → `curl/8.4`、`Go-http-client/1.1` → `Go-http-client/1.1`） |
| E5 | 客户端列上下两层的 `title` 分别等于「来源 IP」与「完整原始 User-Agent」；1600px 下模型列零截断（`scrollWidth <= clientWidth`），时间轴列零横向滚动 |
| E6 | `GET /admin/login` 返回 200 且是登录页（含用户名/密码/记住我/登录按钮）；未登录访问 `/admin/`、`/admin/monitor`、`/admin/config`、`/admin/json` 均返回 302 且 `Location` 以 `/admin/login` 开头 |
| E7 | `POST /api/login` 正确凭据 + `remember:true` → 200 且 `Set-Cookie` 含 `Max-Age=2592000` 与 `HttpOnly`；`remember:false` → `Set-Cookie` 不含 `Max-Age`；错误凭据 → 401 且无 `Set-Cookie` |
| E8 | 携带有效 `mr_session` cookie 访问 `/admin/monitor` → 200；会话 token 在任何响应体与日志中都不出现；`POST /api/logout` 后该 cookie 失效（再用返回 302/401） |
| E9 | 旧的 `Authorization: Basic` 与 `?user=&password=` 仍可用（既有 §24 的 WS 认证测试必须继续通过） |
| E10 | 四个页面路径各自返回 200 且页面主体标识互不相同；刷新 `/admin/config` 后仍停留在配置页（页面标识不变，不跳 `/admin/`） |
| E11 | `/admin/` 是统计首页：含运行时长、版本、活跃连接、累计请求/成功/失败、成功率、P50/P95 TTFT、输入/输出 tokens、平均 t/s、实例统计表、近期错误表 |
| E12 | `GET /api/stats` 返回 JSON，字段结构与 WS `stats` 事件的 `data` 完全一致（键集合相同） |
| E13 | 登录后**无需任何点击**，3 秒内 WS 建立；人为断开（服务端重启或断网模拟）后前端在 ≤15 秒内自动重连并恢复接收 |
| E14 | 新客户端连上 `/api/ws` 后立即收到 `snapshot` 帧：有 120 条历史时 `requests.length === 100`；切到监控页立刻能看到这 100 行（不空白） |
| E15 | 无 WS 客户端时仍能累计统计：断连状态下发 3 个请求，重连后 `stats` 里的累计请求数比断连前 +3（说明计数未被客户端开关关掉） |
| E16 | `POST /api/monitor/clear` 后重连，`snapshot.requests` 长度为 0；前端「清空」按钮同时清空后端缓冲 |
| E17 | 监控页不存在 `#monitor-auth-user`/`#monitor-auth-password`/`#monitor-remember` 元素；`localStorage['model-router.monitor.cred']` 不再被读写 |
| E18 | `go build ./...`、`go vet ./...` 退出码 0；`gofmt -l` 无输出；`go test ./... -race -count=1` 全绿 |
| E19 | 1600×1019 与 1000×900 两档视口：页面无横向滚动条、表头刻度文字不重叠、统计首页卡片不溢出 |
| E20 | 发若干请求后 `stats.json` 存在且含 `"schema": 1` 与累计字段；**重启进程**后 `/api/stats` 的累计请求数、in/out tokens、重试次数与重启前一致（允许 ±1 的当前在途请求误差） |
| E21 | `stats.json` 内容被写成非法 JSON 后启动：进程正常启动、日志有一条 warning、旧文件被重命名为 `stats.json.bad`、`/api/stats` 返回全零 |
| E22 | `POST /api/stats/clear` 后 `/api/stats` 累计值全零，且 `stats.json` 立即被改写为零值；此操作**不**清空 snapshot 的最近 100 条 |
| E23 | 重启后 `snapshot.requests` 为 0（历史不落盘），但发 1 个请求后立刻重连即可拿到 1 条 |
| E24 | 距上次落盘不足 5 秒内连续 100 个请求不会产生 100 次写盘（用文件 mtime 或写入计数验证） |

## 24.8 第五轮：逐行等高 + 块内容类型着色（用户 2026-11-16 拍板）

### 用户原话

> 「另外绿色块的含新token到底是啥意思？图上基本看不到绿色块」

对 E1 的选择（提问卡片）：**「逐行等高：一行内所有条（含等待段和绿块）完全一样高，块高不再随 token 缩放」**。

### Y 组决策（🔒 必须逐条实现）

**Y5 逐行等高（取代 §24.6 X7、C11 与 §24.7 E1 的冲突口径）**

- 一行时间轴内 `.tl-wait`、`.tl-block`、`.tl-block.tok` **全部等高**：高度一律为 `--tl-track-h × 40%`（16px × 40% = 6.4px），`top: 50%` + `translateY(-50%)` 居中。
- 删除「按 `deltaTokens` 在 40%–100% 之间缩放」的全部代码路径（前端 `blockHeightPercent` / `refreshRowTimelineHeights` / `timelineTokenThreshold` / `rowBaseHeightPercent` 一类用于计算的辅助函数一并清理），避免留下死代码与再次漂移的口子。
- `deltaTokens` / `cumTokens` / `tokensKnown` 仍保留在后端事件与前端 `title` 里（X6/X8 不变），只是**不再驱动任何几何尺寸**。
- 等待段宽度规则不变（X1）；块宽（横向时间长度）规则不变（X2/W2/Z2）。

**Y6 「含新 token」重新定义：从 token 数改为 SSE 内容类型（回答「绿色块到底是啥意思、为什么看不到」）**

- **问题根因**：当前绿色的判据是 `deltaTokens > 0`，而 `sseState.tokens` 只从上游 `usage.output_tokens` / `completion_tokens` 取值，MTI 与 DeepSeek 都**只在流末 `message_delta` 上报**，所以除最后一个块外 `deltaTokens` 恒为 0 —— 绿块几乎永远不出现，且即使出现也是"末块"而不是"有新内容的块"。
- **新判据**：一个 chunk 内只要**包含 `content_block_delta` 事件**（即携带真实增量内容：`text_delta` / `thinking_delta` / `input_json_delta` / `signature_delta`），该块标为绿（`.tl-block.tok`）；否则为蓝。
- 后端在 `sseState` 里新增**按块计数**的 `contentDeltas`（每条 `content_block_delta` 行 +1），`copySSE` 仍用 feed 前后差分取本块增量，输出为 `chunk` 事件的新字段 `contentDeltas`（int，未知时为 0）。
- 该字段同时用于 tooltip：`#seq · bytes B · Δn tokens · 内容块 k · 累计 c · 距请求开始 x ms`。
- **兼容性**：`deltaTokens` 字段保留不动（§23 日志的 usage 口径不受影响）；新增字段只增不减。
- 预期效果：真实流式请求里，思考/正文输出阶段的块应**大面积变绿**，只有心跳、`message_start`、`content_block_start/stop`、`message_delta`、`[DONE]` 之类的块保持蓝色。

### 第五轮验收标准（可机械验证）

| # | 标准 |
|---|---|
| Y1 | 真实流式请求下，同一行内 `.tl-wait`、`.tl-block`、`.tl-block.tok` 的 `height` 与 `top` 全部相等（差 ≤ 0.5px），且值 ≈ 6.4px（1600px 视口、默认缩放） |
| Y2 | 一行内**至少出现 1 个绿块**且绿块数量占该行块数的比例 > 50%（真实 DeepSeek 流式，输出 100+ tokens 的请求） |
| Y3 | 绿块的 `title` 含「内容块」字样且该数字 > 0；蓝块对应数字为 0 |
| Y4 | 后端 `chunk` 事件带 `contentDeltas` 字段；同一请求内 `Σ contentDeltas` == 该请求 `content_block_delta` 事件的实际条数 |
| Y5a | 源码中不再存在由 `deltaTokens` 驱动高度计算的路径（`grep` 校验：`docs/design.md` 记录被删除的函数名，代码中不应再出现） |
| Y6 | 既有 §24/§24.5/§24.6/§24.7 测试全部通过（断言未被放宽），`go build`/`go vet`/`gofmt`/`go test -race` 全绿 |

---

---

## 24.9 第六轮：时间轴 tooltip、自适应高度、快照完整化、OpenAI 等价判据（用户 2026-11-16 反馈）

### 用户原话

> 「1、鼠标移到时间轴的条上，应该可以显示请求开始的时间；
> 2、页面的高度没有自适应到浏览器的高度，表格下面还空出很多，应该能自适应高度；
> 3、进入监控页面时，缓存的统计数据排序也要最新的在最上面，也就是序号是倒排的；
> 4、绿色块似乎只有停留在监控页面才会画出来，切换再回来就变成蓝色了，这个快照也要记录下来；
> 5、openai协议也增加等价判据；」

### Z 组决策（🔒 必须逐条实现）

**Z1 条上 tooltip 显示请求开始时间**

- 每一行**所有时间轴元素**（`.tl-wait`、`.tl-block`，以及存在时的 `.tl-cursor` / `.tl-retry`）的 `title` 属性**末尾追加** ` · 开始 HH:MM:SS.mmm`，取该请求的 `start.ts`（毫秒精度、本地时区、24 小时制、毫秒固定 3 位）。
- 元素原有 title 内容全部保留，只在后面追加，不替换。
- 快照恢复的行同样遵守（`start.ts` 来自 snapshot 的 `ts` 字段）。

**Z2 页面高度自适应视口**

- 监控页在视口高度 ≥ 600px 时，**整页不出现纵向滚动条**：`document.documentElement.scrollHeight <= window.innerHeight + 1`。
- 达成方式：`.req-list` 的高度改为**弹性填充**而不是固定 `calc(100vh - 330px)`——卡片用 flex 列布局填满 `header` 以下的剩余高度，`.req-list { flex: 1 1 auto; min-height: 200px; overflow: auto; }`，表格区随窗口拉伸/收缩。
- 视口高度 < 600px 时允许页面纵向滚动（优先保证可用性，不强塞进一屏）。
- 1600×1019 与 1000×900 下都不得出现横向整页滚动（沿用 E19）。

**Z3 快照里的时间倒排**

- 进入监控页后，列表顺序必须**最新在最上面**：第一行是 `ts` 最大的请求，序号列按该行在列表中的位置显示。
- 后端 `recentRequests()` 已经是新→旧；前端 `applyMonitorSnapshot` 当前逐条 `append` 到列表**末尾**，导致旧的在上面——必须改为按顺序插入到顶部（或整体反转后插入），与实时新请求插到最上面的行为一致。
- 实时到达的新请求仍然插到**最上面**。

**Z4 快照记录逐块明细（让绿块在切页/刷新后仍能显示）**

- **问题**：`monitorRequestSnapshot` 只存了 `chunks` 计数、没有逐块数据；前端拿不到块内容，只能画一个全蓝摘要块。用户「切走再回来绿块变蓝」就是它。
- **改法**：后端环形缓冲每条记录新增 `chunksList []monitorChunkEvent`（JSON 字段名 `chunksList`，数组按 `seq` 升序），内容是逐块事件的精简副本，字段：`seq`、`tsStart`、`tsEnd`、`bytes`、`deltaTokens`、`contentDeltas`、`cumTokens`。
- 容量与内存边界：**每条最多保留 400 个块**；超过时**保留前 200 个与后 200 个**，中间用**一个合成块**表示（`seq` 取被跳过区间首尾之和的一半取整、`bytes` 求和、`contentDeltas` 求和、`deltaTokens` 求和、`tsStart` 取区间起点、`tsEnd` 取区间终点）——保证时间轴长度与真实耗时一致且内存可控。
- 100 条 × 400 块的量级（约 4 万个小结构）可接受，且**依然不落盘**（D12 不变）。
- 前端 `ingestSnapshotRequest` 优先用 `chunksList` 还原每块（绿色判据 `contentDeltas > 0`）；仅当字段缺失时才回退到现有「单个摘要块」路径（保留兼容）。

**Z5 OpenAI 协议的等价判据**

- OpenAI `/v1/chat/completions` 流（`choices[].delta`）没有 `content_block_delta`，补等价语义：
  - 一个 chunk 内只要出现 **`choices[].delta.content` 非空**，或 **`choices[].delta.reasoning_content` 非空**（DeepSeek 思考流），或 **`choices[].delta.tool_calls[].function.arguments` 非空**，该块 `contentDeltas` **+1**（三条同时出现也只 +1，按「本块含内容」计）。
  - 纯 `role`、纯 `finish_reason`、空 `delta`、`[DONE]`、`: keep-alive` 注释行一律不计。
- Anthropic 侧判据不变（每条 `content_block_delta` +1）。
- 该判据只用于**视觉着色与 tooltip**，不影响 `deltaTokens` 与 §23 日志口径。

### 第六轮验收标准

| # | 标准 |
|---|---|
| Z1a | 任意时间轴元素的 `title` 末尾匹配 `· 开始 \d{2}:\d{2}:\d{2}\.\d{3}$`；原有内容（如「内容块 k」「距请求开始」）仍在 |
| Z1b | 同一请求的等待段与所有块的「开始」时间戳一致（都是该请求的 `start.ts`） |
| Z2a | 1600×1019 与 1000×900 下 `document.documentElement.scrollHeight <= innerHeight + 1` |
| Z2b | 视口高度 1400 与 900 两档下 `.req-list` 的 `clientHeight` 差值 > 100px（证明是弹性而非固定高度） |
| Z3a | 刷新监控页后所有行按 `ts` 严格降序（第一行 `ts` 最大），序号列从上到下递增 |
| Z3b | 新请求到达时仍插在最上方，序号随之重排 |
| Z4a | 真实流式请求结束后切到统计页再切回监控页（不刷新），该行绿块数量与切走前一致 |
| Z4b | 完整刷新页面后，从快照恢复的行仍能看到绿块（`chunksList` 非空且至少一个 `contentDeltas > 0`） |
| Z4c | snapshot 里每条 `chunksList` 长度 ≤ 400；合成块的 `bytes`/`contentDeltas` 等于被跳过区间之和 |
| Z5a | 真实 OpenAI 协议流式请求（`POST /v1/chat/completions`）产生 `contentDeltas > 0` 的块，且绿块占比 > 50% |
| Z5b | OpenAI 侧 `role` 首块、`finish_reason` 块、`[DONE]` 的 `contentDeltas` 均为 0 |
| Z6 | 既有全部测试通过（断言不放宽），`go build`/`go vet`/`gofmt`/`go test -race` 全绿 |

---

## 24.10 第七轮：OpenAI 模型语义纠错、统计落盘与镜像卷、数值可读化（用户 2026-11-16 反馈）

### 用户原话

> 「1、openai并没有 claude-xxxx 这些模型，这个和anthropic是不同的，openai的模型是根据每个实例选择模型的并集；
> 2、D11统计需要落盘，另外镜像要定义volume保证容器即使没有把统计数据映射出来重建容器也不会丢；
> 3、D12还有什么没做的？
> 4、统计页面显示汇总值得时候应该更合理一些，比如时间超过1000毫秒以秒为单位，token超过1000后以K为单位....」

### 主代理的现状核实（写规格前做的）

- `/v1/models` 实测返回 6 个上游模型名（`mti-pro`、`mti-flash`、`deepseek-v4-pro`、`deepseek-v4.1-flash`、`qwen3.8-flash`、`deepseek-flash`），**没有** `claude-*`。用户判断正确。
- `POST /v1/chat/completions` 带 `"model":"claude-sonnet-5"` → 上游返回 `503 model_not_found ... No available channel for model claude-sonnet-5 under group ...`（有 mti 味的新接口错误）。即：**OpenAI 通道不做角色名映射，直接透传请求里的 model**。
- `docker-compose.yml` 现有 `volumes: - ./.docker:/home/nonroot/config:rw`；`Dockerfile` 无 `VOLUME` 声明；`stats.json` 规划落 `filepath.Dir(configPath)` = `/home/nonroot/config`。
- 统计页数字目前全部原样输出：`formatStatInteger` 直接 `String(Math.round(v))`，`formatPercentilePair` 直接拼 `ms`，token 可能是 `62147` 这种七位数。

---

### A 组：OpenAI 协议的模型语义（修正 §24.9 Z5 的验收环境错误）

**A1 协议语义澄清（写进文档，不改行为）**

- **Anthropic 入口**（`/v1/messages`、`/anthropic/v1/messages`）：`model` 字段是**角色名**（`claude-sonnet-5` 等内部角色），由 `roles` 映射到实例的具体上游模型。
- **OpenAI 入口**（`/v1/chat/completions`）：`model` 字段就是**上游模型名本身**，直通实例的模型列表。可用的模型 = **各实例模型列表的并集**，与 Anthropic 的角色名互不相干。
- 因此传入 `claude-*` 到 OpenAI 入口是**用户错误**，上游返回 4xx/5xx 属正常透传。路由层不需要为它做特殊映射或拦截（保持纯透传原则）。

**A2 `/v1/models` 的返回必须与 A1 一致**

- 返回集合 = 所有实例模型列表的**并集**，按「实例顺序 + 实例内模型顺序」去重；不得混入任何 `claude-*` 角色名。
- `owned_by` 建议为提供该模型的实例名（而不是统一 `model-router`），便于用户看出这个模型来自哪个上游。若同一模型名出现在多个实例，取**第一个**声明它的实例。

**A3 第七轮验收改用真实存在的上游模型名**

- 后续所有 OpenAI 协议验收（含 §24.9 Z5a/Z5b）一律使用 `/v1/models` 里真实存在的模型名（当前环境用 `mti-pro`），**不得**再用 `claude-sonnet-5` 作为 OpenAI 验收样本。
- 在 `docs/tests.md` 里为 Z5a/Z5b 补一行说明：样本模型必须取自 `/v1/models`，原样本 `claude-sonnet-5` 无效（上游 503）。

---

### B 组：统计持久化落地（D11，此前只写了规格未实现）

**B1 落盘实现（口径沿用 §24.7 D11，不重复定义）**

- 位置 `filepath.Dir(configPath)/stats.json`；只落盘累计统计 + 近期错误表 ≤20 条；不落盘分位数窗口、活跃连接、QPS、运行时长、100 条请求快照。
- 原子写：`os.CreateTemp` + `Rename`；节流 ≥5 秒且有变化才写；`SIGINT`/`SIGTERM` 补写一次。
- 启动加载：文件缺失 → 零值；解析失败 → warning + 重命名 `stats.json.bad` + 零值启动，**绝不能因它阻止启动**。
- 文件带 `"schema": 1`；未知 schema 同样走 `.bad` 分支。
- 新增 `POST /api/stats/clear`（需登录）：清零累计值并立刻落盘，不动 100 条请求快照。

**B2 镜像与 compose 必须自带卷，避免重建容器丢统计**

- `Dockerfile` 增加 `VOLUME ["/home/nonroot/config"]`，声明该目录为持久卷。
- `docker-compose.yml` 增加**具名卷**映射统计目录，且与配置目录分开，例如：
  ```yaml
  volumes:
    - ./.docker:/home/nonroot/config:rw
    - model-router-data:/home/nonroot/config   # 冲突时以数据卷为准会覆盖配置，禁止
  ```
  **注意**：配置文件和 `stats.json` 同目录，一旦分别用不同的卷映射同一路径会互相覆盖。所以本轮采用：
  - `stats.json` 落在 `/home/nonroot/config`（与配置同目录，沿用既有挂载）；
  - `Dockerfile` 声明 `VOLUME ["/home/nonroot/config"]`；
  - `docker-compose.yml` 保留现有 `./.docker:/home/nonroot/config:rw`，并在注释里说明「统计与配置同目录，二者一起持久化」；
  - 额外提供 `docker-compose.named.yml` 示例，用具名卷 `model-router-config:/home/nonroot/config`，让不映射宿主目录的用户也能保住统计。
- README 增一节说明：不映射该目录时统计会在容器重建后丢失，`VOLUME` 声明保证 `docker rm` 前的容器数据在匿名卷里。

**B3 验收**

| # | 标准 |
|---|---|
| B3a | 发 3 个 AI 请求后等待 ≥6 秒，`/home/nonroot/config/stats.json` 存在且 `schema=1`，`requests.total` ≥3 |
| B3b | `docker compose restart` 后 `/api/stats` 的 `requests.total` 与重启前一致（允许 ±1） |
| B3c | 手工把 `stats.json` 写成 `{`，重启容器：服务正常启动、日志有 warning、生成 `stats.json.bad`、`/api/stats` 返回零值 |
| B3d | `POST /api/stats/clear` → 累计值归零、`stats.json` 立即被改写、100 条请求快照不变 |
| B3e | 5 秒内发 100 个请求，`stats.json` 的 mtime 变化次数 ≤ 3（节流生效） |
| B3f | `docker inspect` 显示镜像有 `VOLUME /home/nonroot/config`；`docker-compose.named.yml` 可正常 `up -d` |

---

### C 组：统计页数值可读化

**C1 时间类**（TTFT P50/P95、总耗时 P50/P95、运行时长）

- `< 1000 ms`：整数毫秒 + `ms`，如 `842ms`
- `≥ 1000 ms`：秒，保留 **2 位小数**，如 `1.23s`、`12.36s`、`132.00s`
- `≥ 3600 s`：小时，`1 位小数` + `h`，如 `2.5h`
- 运行时长沿用现有 `2d 3h 14m` 格式不变

**C2 token 与计数类**（累计/成功/失败、按协议计数、输入/输出 tokens、重试次数）

- `< 1000`：原样整数
- `≥ 1000`：`K`，保留 **1 位小数**，如 `1.2K`、`62.1K`
- `≥ 1_000_000`：`M`，保留 **2 位小数**，如 `1.23M`
- 舍入规则：先按阈值选单位，再四舍五入；`999.96K` 进位为 `1.00M`（进位后跨到下一单位要重新取单位）
- 鼠标悬停 `title` 显示**精确值**（如 `62,147`），不得丢失原始信息

**C3 速率与百分比**

- t/s 保持 1 位小数不变；成功率保持 1 位小数 + `%`；QPS 保持 1 位小数不变
- 实例统计表里的平均 TTFT / 平均 t/s / 请求数同样套用 C1、C2

**C4 验收**

| # | 标准 |
|---|---|
| C4a | 构造 `ttft.p50=999`、`p95=1234` → 显示 `999ms / 1.23s` |
| C4b | 构造 `outTokens=62147` → 显示 `62.1K`，`title` 为 `62,147` |
| C4c | 构造 `inTokens=1234567` → 显示 `1.23M`，`title` 为 `1,234,567` |
| C4d | 构造 `duration.p50=3600000` → 显示 `1.0h` |
| C4e | 构造 `requests.total=999` → 显示 `999`（不缩写） |
| C4f | 构造 `979999` → 显示 `980.0K`（不是 `979.9K` 的错误舍入），`title` 精确 |
| C4g | 实例表里的请求数/tokens/TTFT 同样遵守 C1/C2 |

---

### 第七轮统一验收

| # | 标准 |
|---|---|
| G1 | `GET /v1/models` 不包含任何 `claude-*`；`owned_by` 为实例名；模型集合等于各实例列表并集去重 |
| G2 | `POST /v1/chat/completions` 用 `/v1/models` 里的模型名可正常返回；用 `claude-sonnet-5` 时错误原样透传（不做隐藏拦截） |
| G3 | Anthropic 入口的角色名映射行为完全不变（既有测试全绿） |
| G4 | B3a–B3f 全过 |
| G5 | C4a–C4g 全过 |
| G6 | `go build`/`go vet`/`gofmt`/`go test -race` 全绿，既有测试断言不放宽 |

---

## 24.11 第八轮：D12 收尾（进行中请求也进快照 + 删除旧路径）

### 用户原话

> 「D12都收掉吧，进行中的也要快照，旧路径都删了」

### H 组决策

**H1 进行中的请求也进快照**

- `snapshot` 帧的 `requests` 数组里，**除了已结束请求，必须同时包含当前仍在进行中的请求**。每条进行中记录带 `ended: false`。
- 进行中记录至少包含渲染表格行所需的全部字段：`id`/`ts`/`method`/`path`/`protocol`/`reqModel`/`clientIP`/`userAgent`/`instName`/`upstreamModel`/`status`(0 或已定)/`durationMs`(到取快照那一刻)/`ttftMs`/`genMs`/`inTokens`/`outTokens`/`chunks`，以及**已产生的逐块 `chunksList`**（同样 400 块上限 + 合成块规则）。
- 排序：整个 `requests` 数组统一按 `ts` 降序（进行中与已结束混排，最新的在最前）。
- 容量：已结束 100 条不变；进行中的**不占**这 100 条配额，但进行中条目数硬上限 **200**（超过则丢弃最早的进行中记录，避免异常长连接撑爆内存）。
- 前端收到 `ended: false` 的快照行后：按进行中渲染（耗时随 250ms 定时器增长、时间轴右端有当前游标）；后续同一 `id` 的 `chunk`/`end` 事件必须能**无缝续接**（不能因为该行来自快照就忽略它的实时事件）。
- 已结束请求仍然由 `end` 事件移出「进行中」集合。

**H2 删除旧 WebSocket 路径 `/api/monitor`**

- 服务端删除 `PathMonitor = "/api/monitor"` 的路由与处理，只保留 `PathWS = "/api/ws"`。
- 请求 `/api/monitor` 返回 **404**（不是 401、不是 101）。
- 文档里所有「保留一个版本周期」的说法改为「已删除」。
- 既有测试 `TestMonitorNoHistoryReplay` 等若依赖旧路径，改为针对 `/api/ws` 重写；**不得**简单删除测试，拦截能力（认证、没客户端时不采集广播等）必须仍被覆盖。

**H3 验收**

| # | 标准 |
|---|---|
| H3a | 连接 `/api/ws` 时若有一条请求正在进行，`snapshot.requests` 里能找到它，`ended === false`，且 `chunksList` 长度等于已收到的块数 |
| H3b | 收到快照后该请求继续流式输出，前端该行的块数继续增长（不因来自快照而断流） |
| H3c | 该请求结束后再刷新页面，同一 `id` 出现在快照里且 `ended === true`，块的着色（绿/蓝）与刷新前一致 |
| H3d | 快照数组整体按 `ts` 降序（进行中与已结束混排） |
| H3e | 进行中条目超过 200 时，最早的被丢弃，内存不无界增长 |
| H3f | `GET /api/monitor`（含正确凭据）返回 404；`/api/ws` 正常 101 |
| H3g | 既有 monitor 相关测试要么重写为 `/api/ws` 且能力等效，要么保留原断言意义，全部通过 |
| H3h | `go build`/`go vet`/`gofmt`/`go test -race` 全绿 |

---

## 24.12 第九轮：JSON 高级页编辑框高度自适应

### 用户原话

> 「还有json高级页面的编辑框高度也没有自适应到浏览器的高度」

### J 组决策

**J1 编辑框填满视口剩余高度**

- `#json-editor` 当前是全局 `textarea { min-height: 480px; resize: vertical; }`，高度写死。第 2 条（§24.9 Z2）的「页面高度自适应」只做到了监控页，JSON 页要一并补齐。
- 做法：`#json-pane` 用纵向 flex，`.card` 撑满 `header` 以下的剩余空间，`#json-editor { flex: 1 1 auto; min-height: 240px; resize: none; }`——编辑框随浏览器窗口拉伸/收缩。
- 与监控页一致：视口高度 ≥600px 时整页**不出现纵向滚动条**；<600px 时允许滚动，`min-height: 240px` 兜底。
- `resize` 从 `vertical` 改为 `none`（高度已由弹性布局决定，手动拖拽会与自适应打架）。
- 只影响 `#json-editor`，全局 `textarea` 规则保持不变（配置页的 textarea 不受影响）。

**J2 验收**

| # | 标准 |
|---|---|
| J2a | `/admin/json` 在 1600×1019 与 1000×900 下 `documentElement.scrollHeight <= innerHeight + 1` |
| J2b | 视口高 1400 与 900 两档下 `#json-editor` 的 `clientHeight` 差值 > 150px（证明是弹性而非固定） |
| J2c | `#json-editor` 的 computed `resize` 为 `none` |
| J2d | 配置页（`/admin/config`）的 textarea 高度行为不受影响（仍为原样） |
| J2e | 编辑框内容滚动正常（`scrollHeight` 大于 `clientHeight` 时可滚动），不影响校验/保存/重载按钮可见性与功能 |

---

## 24.13 第十轮：项目品牌化（中文展示名 + Logo + favicon）

### 用户决策（2026-11-16）

> 「选B，中文名只是用于展示名称，英文标识保留不改，然后开始执行修订吧。」

- **展示名（中文）**：`模型智能路由`
- **英文标识保持原样**：`model-router` 用于 Go module 路径、二进制名、镜像 tag（`wuzm219/model-router`）、配置文件名、环境变量前缀、Docker 服务名。**一个都不改**。
- **Logo**：选定方案 B（深色圆角底板 + 双弧环 + 三线汇聚箭头），原始图已存 `docs/assets/logo-source-256.png`。

### K 组决策

**K1 展示名替换范围（只换"给人看"的地方）**

需要改成「模型智能路由」的位置：
- `internal/web/index.html`：`<title>`、页面内 `PAGE_TITLES`（`model-router 统计` → `模型智能路由 统计` 等四条）、`document.title` 兜底字符串
- `internal/web/login.html`：`<title>`、左上角标题
- `README.md`：一级标题处补中文名（保留英文标识并列，如 `# 模型智能路由（model-router）`），正文描述里第一次提及处用中文名
- `docs/` 下各文档：项目描述性语句用中文名；但涉及文件名、路径、代码标识符、镜像名、URL 的地方**保持 `model-router` 不变**
- `CHANGELOG.md`：新版本条目标题可用中文名

**K2 菜单/正文出现过的项目名一律不动**

- 日志行、HTTP 头、`Set-Cookie`、配置 JSON、镜像名、服务名、测试名、包名、二进制名全部保持 `model-router`。

**K3 Logo 资源**

- 新增 `internal/web/favicon.ico`：**多尺寸 ICO**，至少包含 16×16、32×32、48×48 三个尺寸（`go:embed` 打包）。
- 新增 `internal/web/logo.svg`：矢量版，用于页面左上角（保证高分屏清晰）。
- 新增路由：`GET /favicon.ico` 返回 ICO（`Content-Type: image/x-icon`，带 `Cache-Control: public, max-age=86400`）；Logo 的 SVG 路由见 §24.14 L（**已由 0.1.3 修订为匿名 `GET /logo.svg`，原 `/admin/logo.svg` 已删除**）。
- **`/favicon.ico` 必须允许匿名访问**（浏览器拉 favicon 不带 Cookie），不能因为鉴权被 302 到登录页——否则标签栏图标永远不显示。这是本轮最容易出错的地方。
- 若 favicon 无法用纯手写二进制可靠生成，允许用 Go 代码在构建期或运行期由内嵌的 PNG 转出 ICO；优先选择「直接把生成的 .ico 文件提交进仓库」的简单方案。

**K4 左上角布局**

- `<header>` 中 `<h1>` 前插入 `<img src="/logo.svg" alt="" width="24" height="24">`（§24.14 L 修订后的路径），与标题垂直居中对齐（`display:flex; align-items:center; gap:8px` 的容器包住图标与文字）。
- 登录页同样处理。
- 图标尺寸 24×24，不影响现有导航按钮位置；窄屏（1000px）下不产生额外换行。

**K5 不动的东西**

- 不改任何接口契约、不改统计/监控行为、不改配置结构。
- 不改 `go.mod` module 名、不改 `internal/version/version.txt`。

### K6 验收

| # | 标准 |
|---|---|
| K6a | `/admin/`、`/admin/monitor`、`/admin/config`、`/admin/json` 的 `<title>` 均为「模型智能路由 …」形式，左上角文字为「模型智能路由」 |
| K6b | 四个页面左上角都显示 24×24 Logo，与文字垂直居中，`gap` 8px；1000×900 下 header 不换行、无溢出 |
| K6c | 登录页 `/admin/login` 同样有 Logo + 中文名 |
| K6d | **匿名** `GET /favicon.ico`（不带任何凭据）返回 **200** 且 `Content-Type` 为 `image/x-icon`，响应体以 `00 00 01 00` 开头（ICO 魔数），尺寸至少含 16/32/48 |
| K6e | ~~登录后 `GET /admin/logo.svg` 返回 200~~ → 已由 §24.14 L3a 取代：匿名 `GET /logo.svg` 返回 200、`Content-Type: image/svg+xml`，内容为合法 SVG |
| K6f | 页面里不存在残留的「model-router」可见文字（用 `grep -c "model-router"` 校验：只应出现在 `<script>` 内的 localStorage/凭据键、注释、以及不影响展示的地方；可见文本节点中为 0） |
| K6g | `grep -rn "model-router" go.mod Dockerfile docker-compose.yml` 仍为原值（英文标识未被误改）；镜像 tag、服务名、module 路径不变 |
| K6h | `go build`/`go vet`/`gofmt`/`go test ./... -race` 全绿；新增测试覆盖 `/favicon.ico` 匿名 200 与 Logo 资源行为（鉴权要求已由 §24.14 L3a 改为匿名 200） |

## 24.14 第十一轮修订：Logo 匿名可访问（0.1.3）

> 用户原话（2026-11-16）：

> 「bump dev版本，没有登录的时候，/admin/logo.svg 会被重定向到登录页，这导致了的登录页的logo始终显示不出来；logo同时加到README.md中；」

### L1 Logo 改为匿名静态资源

- 矢量 Logo 改由 **`GET /logo.svg`** 提供，与 `/favicon.ico` 一样在鉴权之前处理，**匿名 200**，`Content-Type: image/svg+xml`，`Cache-Control: public, max-age=86400`。
- 删除旧路由 `/admin/logo.svg`；`internal/web/index.html`（四个管理页）与 `internal/web/login.html` 的 `<img src>` 统一改为 `/logo.svg`。
- 登录页与已登录页面拿到的资源完全一致，不再有「登录前空白、登录后出现」的差异。

### L2 README 展示 Logo

- `README.md` 标题下方展示 Logo：使用**相对路径 `docs/assets/logo.svg`** 的 `<img width="96">`，保证在 GitHub 与本地编辑器预览都能显示，不依赖运行中的服务。

### L3 版本

- `internal/version/version.txt` 由 `0.1.2` 升为 `0.1.3`（dev，构建版本 `0.1.3-rc`）；`CHANGELOG.md` 新增 `[0.1.3]` 条目。

### L 验收

| # | 标准 |
|---|---|
| L3a | 匿名 `GET /logo.svg` 返回 200、`Content-Type: image/svg+xml`、`Cache-Control` 含 `max-age=86400`，body 为合法 SVG（含 `viewBox="0 0 256 256"`） |
| L3b | 匿名 `GET /admin/logo.svg` 不再返回 200 |
| L3c | `admin_password` 为空时 `GET /logo.svg` 仍为 200 |
| L3d | 登录后 `GET /logo.svg` 仍为 200 |
| L3e | `internal/web/login.html` 与 `internal/web/index.html` 中不存在 `/admin/logo.svg` 引用 |
| L3f | `README.md` 中出现指向 `docs/assets/logo.svg` 的图片引用，且该文件存在 |
| L3g | `go build` / `go vet` / `gofmt` / `go test ./... -race` 全绿 |

## 24.15 第十二轮：服务熔断能力优化（M11，0.1.4）

> 用户原话（2026-11-16）：

> 「这个迭代想继续优化下服务熔断的能力：1、统计页面的实例统计按实例的优先级排序，另外当前选择的实例要有一个明显的标识去可识别；2、增加切换按钮，可以强制切换当前选择的实例，手动切换到非第一个实例的视同与主动降级，重新开始计算回退时长；3、另外half-open的概念是怎样的，我们先讨论下这个，确认方案再实现；4、还有什么需要优化的点，你也可以给我点建议；」

> 追加拍板（2026-11-16）：

> 「1、openai为啥要用/v1/models探测，而不用对话接口探测呢？如果使用对话接口探测那和真实探测也没区别了，没有必要再甲兜底了吧？ 2、就是链头的语义； 3、不需要恢复默认按钮，也不需要落盘，强制切换就是一次性的，你可以理解为强行把这个实例前面的示例都置成open，把这个实例开始及后面的都置成closed； 4、不用指数退避功能，探测失败就重置回退时间就行，另外回退时间这样其实就是探测周期了，默认改成1分钟吧探测一次吧； 5、实例列表除了你能看到你说的open/closed/half-open的状态外，还要显示失败降级开始的时间； 6、half-open的探测从第一个实例开始，而不是当前实例的上一个，第一个实例探测失败，继续探测下一个，直到当前实例（当前实例不用再探测了），如果探测成功就回退到探测成功的实例； 7、探测请求也应当在监控页面上显示，而且应该和其他请求能明显的区分出来； 8、如果所有实例都失败了，那么新的请求依然发到最后一个实例，然后同样的去探测前面的实例就行；」

> 再拍板（2026-11-16）：

> 「1、你理解的对； 2、就按后者做； 3、统一探测tick； 4、补充建议 1后和5做，2不要剩余时间、lastProbeAt + lastProbeResult，3 链头不要加星了有颜色区分已经很明显了； 5、强制切换功能不做了，改成一个锁定/解锁的功能，可以用个锁的图形按钮，锁定之后就熔断策略就暂停，所有请求都使用这个实例，锁定不用落盘，重启后就恢复，锁定的实例状态就叫已锁定，其他实例的状态变成"-"，同样也像open一样变灰；」

### 24.15.1 现状与问题（实现前）

现有熔断状态机（`internal/proxy/proxy.go` 的 `instanceRuntime.isDegraded` / `recordSuccess` / `recordFailure` / `Handler.serveChain`）：每个实例只有 `consecutiveFailures` 与 `degradedUntil` 两个字段；连续失败 ≥ `failure_threshold` 进入降级；降级期内 `serveChain` 直接跳过；降级期结束后的**第一个真实请求**充当探测（成功清零、失败立即重新计时）；全部候选都在降级期时强制打链头一次。

问题：half-open 是**隐式**的（没有显式状态、没有并发放行控制），探测成本由用户请求承担，失败类型不区分，状态在统计页只有 `degraded: true/false` 一个布尔，看不到降级开始时间、探测进度，也无法人工干预。

### 24.15.2 M 组决策

| # | 决策 |
|---|---|
| M1 | 熔断状态显式化为四态：`closed`（正常）、`open`（降级）、`probe`（探测中）、`locked`（已锁定）；被锁定时其余实例展示态为 `-` |
| M2 | **只做对话探测**：半开探测使用各协议的真实对话接口（OpenAI `POST {base_url}/v1/chat/completions`，Anthropic `POST {base_url}/v1/messages`），`max_tokens: 1`、非流式；不做 `/v1/models` 探测、不需要「真实请求兜底」 |
| M3 | 探测请求携带该实例的真实模型映射：OpenAI 取 `models[0]`；Anthropic 按 `roles['*'] → roles['haiku'] → roles['sonnet'] → roles['opus'] → roles['fable']` 取第一个存在的；全部缺失则跳过该实例的探测（视为探测失败） |
| M4 | 探测超时使用**该实例自己的 `timeout`**（默认 15s），不套用 `first_token_timeout` / `idle_timeout` / `min_tokens_per_second` |
| M5 | 探测判定：上游返回 2xx 即成功；连接失败、超时、5xx 计为失败；4xx（400/401/403/404/422）按「实例可达」计为**成功**（与 §10 测试连接语义一致：认证/参数问题不代表实例挂了） |
| M6 | **统一探测 tick**：每 `degrade_duration`（默认改为 `1m`）触发一轮，一轮内对每个协议从索引 0 开始逐个探测 `[0, currentIdx)` 区间内处于 `open` 的实例；`current` 自身不探测；区间外的实例（`current` 之后）不探测 |
| M7 | 探测成功 → 该实例 `closed`、`consecutiveFailures = 0`、`degradedSince` 清空，并**把 `current` 回退到该实例**，本轮该协议探测结束；探测失败 → 该实例 `degradedUntil = now + degrade_duration`（不加退避、不做指数增长），本轮继续探测下一个 |
| M7b | **单实例协议的链头不自锁**（0.1.4 补充，实测发现）：`advanceCurrent` 在「current 指向的实例转 open、但其后再无非 open 实例」时**停在 current 不动**，请求由全 open 分支打链尾处理；该实例的恢复靠 M31 的全链探测，不靠 current 顺延 |
| M8 | **全部实例 `open`**：新请求发往**最后一个实例**（链尾）；该实例成功后**就地成为 `current`**；此后每轮 tick 照常探测 `[0, currentIdx)` 内的 `open` 实例，一旦有实例探测成功，`current` 回退到它 |
| M9 | `current` 指针按协议维护（openai / anthropic 各一个），默认索引 0，**只存内存**，重启清零 |
| M10 | **锁定 / 解锁**（取代原「强制切换」）：对一个实例加锁后，熔断策略**暂停**——不计数、不探测、不回退，该协议的所有请求都发往该实例；解锁后以该实例为 `current` 恢复，其余实例状态清零、重新计数 |
| M11 | 锁定状态**不落盘**，重启后恢复（与 §24.10 D11「当下型状态不落盘」一致） |
| M12 | 锁定为**按协议**的：`{"protocol":"openai","name":"x"}`；同一实例不会同时出现在两个协议分组之外 |
| M13 | 探测请求必须进入监控页展示，且与真实请求**明显区分**；探测**不计入累计统计**（不增加请求数、不参与成功率和分位数） |
| M14 | 统计页实例行按「协议顺序（openai 在前）→ 实例在配置中的优先级」排序；`current` 行以视觉方式标识（**不加星号/文字标记**），锁按钮在当前行显示 |
| M15 | 统计页实例行展示 `degradedSince`（降级开始时间）；**不展示剩余时间**，也不展示 `lastProbeAt` / `lastProbeResult` |
| M16 | `fallback.degrade_duration` 的**默认值**由 `5m` 改为 `1m`（它同时是探测周期）；`config.example.json`、README 中示例值同步改为 `1m`。用户线上的 `.docker/config.json` 改由主代理在重建容器时同步改为 `1m`（已获用户授权，见 M17 下方说明） |
| M17 | **实例级 token 累计**：统计页实例行新增「输入 token」「输出 token」两列，位置在「平均 t/s」列**之前**。口径采用 **A——全部请求累加**，含失败/中断/被截断的请求；输入取上游返回的 `usage` 中输入端全量（含缓存命中的输入，若上游分字段则求和），输出取**全量输出 token（含 thinking/reasoning）**，**不得**使用只统计吞吐样本的 `genOut`。历史数据不可追溯：`stats.json` 从未存过实例级 token，升级后从零累加 |

### 24.15.3 接口

| 路径 | 方法 | 请求体 | 行为 |
|---|---|---|---|
| `/api/instances/lock` | POST | `{"protocol":"openai","name":"MTI"}` | 锁定该实例；该协议其余实例状态置为「被遮蔽」。已锁定时重复调用幂等返回 200 |
| `/api/instances/unlock` | POST | `{"protocol":"openai"}` | 解除该协议的锁定；`current` 设为被解锁的实例，其余实例清零 |
| `/api/stats` | — | — | `instances[]` 每项新增 `protocol`、`priority`、`state`、`locked`、`degradedSince`（毫秒时间戳，未降级为 0）、`inTokens`、`outTokens`（均 `int64`，累计值，缺省为 0）、`reason` 字段；保留原有 `degraded` 布尔以兼容。`instances[]` 顺序为：按 `protocol` 分组（`openai` 在前、`anthropic` 在后），**组内按 `priority` 升序、同级按 `name` 升序** |

### 24.15.4 监控页探测请求

- `monitorRequestSnapshot` 与 WS 事件新增布尔字段 `probe`，另加 `probeResult`（成功/失败文本）可选字段。
- 探测行在「方法/路径」列前显示 `探测` 徽标；整行左侧 3px 琥珀色竖条 + 淡琥珀背景，与真实请求区分；状态列显示 `探测成功` / `探测失败`。
- 探测行同样进最近 100 条环形缓冲（可被「清空」移除），但**不进**统计累计。

### 24.15.5 M 组验收

| # | 标准 |
|---|---|
| M15a | 统计页实例行按 openai 组在前、组内按配置优先级升序排列；同一实例名在两个协议时各占一行 |
| M15b | `current` 行有明显的视觉区分（左侧色条 / 加粗 / 行背景之一或组合），且**不出现星号或「链头」文字** |
| M15c | `open` 行变灰并显示降级开始时间（`HH:MM:SS` 形式）；`probe` 行显示「探测中」；`locked` 行显示「已锁定」，同协议其余行状态列显示 `-` 且同样变灰 |
| M15d | 实例行「状态」列旁有锁图标按钮；点击锁定后 `POST /api/instances/lock` 被调用且返回 200，随后 WS 推送的 stats 中该实例为 `locked` |
| M15e | 锁定后一切请求都发往被锁定实例（构造第二个实例必然失败的场景验证）；解锁后恢复原顺序 |
| M15f | 连续失败达到阈值后实例转 `open`；不早于 `degrade_duration` 不做探测 |
| M15g | 探测 tick 到期后，按索引从 0 开始对 `[0, currentIdx)` 内 `open` 实例逐个发出**对话接口**探测请求（`max_tokens:1`、非流式、携带真实模型）；探测成功的实例转 `closed` 且 `current` 回退到它 |
| M15h | 探测失败时该实例 `degradedUntil` 重置为 `now + degrade_duration`，不发生指数增长（连续多次失败的间隔恒等于 `degrade_duration`） |
| M15i | 全部实例 `open` 时新请求发往最后一个实例；该实例成功后 `current` 变为它；下一轮 tick 仍探测前面的 `open` 实例 |
| M15j | 探测请求在监控页可见且带 `探测` 徽标与琥珀色标识；`GET /api/stats` 的请求总数在有探测发生时**不增加** |
| M15k | `fallback.degrade_duration` 缺省值解析为 `1m`；`config.example.json` 与 README 示例为 `1m` |
| M15l | 锁定状态不落盘：重启后（或新建 Handler）所有实例为 `closed`、`current` 为索引 0 |
| M15m | `go build` / `go vet` / `gofmt` / `go test ./... -race` 全绿；新增测试覆盖四态迁移、tick 探测顺序、`current` 回退、全 open 时的链尾兜底、锁定/解锁、探测不进统计 |
| M15n | 统计页实例行在「平均 t/s」列**之前**出现「输入 token」「输出 token」两列；表头分别为 `输入 token` / `输出 token`，数值千分位展示，缺省显示 `0` |
| M15o | 一次成功的对话请求后，该实例的 `inTokens` / `outTokens` 增量等于上游 `usage` 的输入/输出全量；**失败的请求同样累加**（发送前已拿到 usage 或流中途失败的已产生部分计入）；探测请求**不计入**这两列 |
| M15p | 输出 token 列统计的是全量输出（含 thinking），与「平均 t/s」的分母 `genOut` 口径不同；构造一个只有 thinking 增量、`genOut` 不增长的场景，输出 token 列仍增长 |
| M15q | 实例统计按协议拆成**上下两张表**：上表 `OpenAI 实例`、下表 `Anthropic 实例`，表头与列宽完全一致（各 11 列，同 UT-808 列顺序）；两张表各自有独立的空状态文案（`暂无 OpenAI 实例。` / `暂无 Anthropic 实例。`），某协议无实例时只隐藏该表 |
| M15r | 每张表**组内按配置优先级升序**排列（同优先级再按实例名升序），不再按协议名字典序；`current`、`open`、`locked` 三种行的视觉标识与锁按钮行为在两张表中保持一致，锁按钮仍调用 `/api/instances/lock` 与 `/api/instances/unlock` |
| M15s | 实例表「输入 token」「输出 token」两列按量级缩写：`< 1000` 显示整数原值；`≥ 1000` 且 `< 1000000` 显示 `K` 保留 1 位小数（如 `1.2K`、`10.0K`、`441.4K`）；`≥ 1000000` 显示 `M` 保留 2 位小数（如 `1.05M`）；舍入跨单位时重新选单位（`999.96K → 1.00M`）；鼠标悬停 `title` 显示精确千分位原值；缺省/非法值为 `0`。**阈值 1000 与 §24.10 C2 的顶部磁贴一致**（用户 m00668 拍板：全局统一，不出现各自标准） |
| M15t | **数值与单位的展示逻辑全局唯一**：`internal/web/index.html` 中所有 K/M 计数类（请求数、成功/失败、token、重试次数、WS 连接数、活跃请求数）与百分比，以及所有时长类（ms/s/h，含监控页首包/耗时、统计页平均 TTFT、P50/P95）都必须走同一套共享函数，页面内**不得存在第二套阈值或小数位规则**。原 `formatMs` / `formatInstanceTokens` / `formatInstanceTokenCount` 一律删除或改为对共享函数的薄封装，不得保留独立实现 |

## 24.16 第十三轮：周期性探测与监控页探测展示修正（M12，0.1.4）

> 用户原话（2026-11-16，实测反馈）：

> 「1、状态列，探测也不要显示探测... 而是显示探测中，探测结束显示成功或失败；2、方法/路径列，探测的tag不要放在单独的一行，这样就出现了3行了，把它放在 POST 后面就好；3、探测的计时有问题，两分多钟过去了还没探测」

> 追加反馈（2026-11-16）：

> 「好像刚才那句话我输入完就开始探测了，但是那句话的请求没出来，一直等探测完才显示请求，是需要有新的请求才会触发探测？我期望的应该是周期性的，只要有降级即使空闲的时候也会探测恢复。」

### 24.16.1 现状与问题（实现前）

§24.15 的探测 tick 只在 `serveChain` 开头调用 `maybeProbe`，**没有后台定时器**：只有真实请求进来才可能触发探测，空闲时降级实例永远不恢复。

实测证据（2026-11-16，`degrade_duration = 1m`、实例 `timeout = 15s`）：

- `anthropic MTI/deepseek` 于 `15:42:13` 转 `open`，`MTI/aliyun` 于 `15:42:29` 转 `open`，两条均为 `reason = probe_failed`；`current` 指向健康的 `anthropic DeepSeek`。
- 此后的两次探测都发生在**真实请求到达之后**：`15:45:41` 与 `15:45:57`（并非 1m 周期到了就发）。`15:45:41` 那次请求被探测阻塞了约 15s 才写出响应，用户看到「请求一直等探测完才显示」。
- 两次探测都 `context deadline exceeded`（`status 0`、`durationMs 15000`），而被探测的实例上**真实流式请求是 200**（`/anthropic/v1/messages`）。原因是探测请求只带 `max_tokens: 1`、且该实例 `timeout = 15s` 被上游排队时间吃满。
- 探测报文协议不一致：`probeChatPath(protocolAnthropic)` 返回 `/v1/messages`，但 Anthropic 请求进入本服务的路径是 `/anthropic/v1/messages`（实例 `base_url = http://10.168.4.230:30000`），导致探测走的是 OpenAI 协议路径，同一实例上「探测失败、真实请求成功」互相矛盾。

### 24.16.2 M 组决策（承 §24.15 编号）

| # | 决策 |
|---|---|
| M18 | **周期性探测**：探测由服务端**独立后台定时器**驱动，与真实请求完全解耦。只要任一协议存在 `open` 实例，后台就按探测周期发起探测；没有任何 `open` 实例时定时器空转（不占 CPU、不写内存、不发请求） |
| M19 | **探测移到请求路径之外**：`serveChain` 不再同步执行探测，也不再等待探测结果，真实请求的时延不受探测影响。删除 `serveChain` 开头的 `maybeProbe` 同步调用 |
| M20 | **探测节奏**：后台定时器每 1s 检查一次；每轮探测把「每个有 `open` 实例的协议」全部探测完；判定是否该起新一轮时，取**所有协议中最晚的 `nextProbe`**，避免一个协议把另一个协议的周期拖短或拖长。探测需满足：距上次本轮全部结束 ≥ 探测周期（`degrade_duration`，默认 `1m`），且同时进行的轮次不超过 1 |
| M21 | **探测报文路径修正**：Anthropic 协议的探测路径改为 `/anthropic/v1/messages`（与真实 Anthropic 请求进入本服务后的转发路径一致），OpenAI 协议保持 `/v1/chat/completions`。裸 `/v1/messages` 属非法路径 |
| M22 | **探测报文内容**：在 §24.15 M2 的 `max_tokens: 1` 基础上，`messages[0].content` 用 `"ping"`，非流式；用户未要求放宽，保持最小 |
| M23 | **首轮探测不等一个周期**：某协议从「无 open 实例」变为「有 open 实例」时（新降级发生或已有 open 实例），后台定时器最迟在**下一个 1s 检查点**发起第一轮探测，不做额外等待；此后按 M20 的周期推进 |
| M24 | **探测不阻塞真实请求**，但**两个探测之间仍互斥**：同一实例同一时刻最多一个探测在飞（沿用 `inst.probing`）；不同实例可并发探测，避免逐个 15s 串成一条长链 |
| M25 | **监控页状态列文案**：探测行未结束时显示 `探测中`；结束后只显示 `成功` / `失败`（与真实请求一致），**不再**出现 `探测成功` / `探测失败` |
| M26 | **监控页「探测」徽标不进「方法/路径」列**：该列与真实请求完全同构，固定两行（第一行 `POST`、第二行路径），不含任何探测相关节点 |
| M27 | **「探测」徽标放进「客户端」列**（用户 m00881 拍板：「徽标不要放在请求路径这边了，直接放在客户端这列，本地探测的时候，IP/客户端就不要显示了」）：探测行的客户端列**只显示徽标**，IP 行与 UA 行都不显示内容（不是 `-`、不显示 `model-router-probe`）。真实请求的客户端列保持 IP + UA 两行、无徽标。后端 `userAgent` 字段保持不变（仍是 `model-router-probe`，供外部脚本识别） |
| M28 | 探测仍**不计入**累计统计（沿用 §24.15 M13）；探测行仍进最近 100 条环形缓冲 |
| M29 | 探测超时仍取该实例自己的 `timeout`（沿用 §24.15 M4），本次不改 |
| M30 | 探测成功/失败后的状态迁移与 `current` 回退规则**完全沿用** §24.15 M7 / M8，本次不改语义 |
| M31 | **探测范围 = 整条链的 open 实例**（修订 §24.15 M7 的 `[0, current)` 区间）：`probeProtocol` 遍历该协议**全部**实例，对每个 `open && due && !probing` 的发探测；某个成功后该实例 `recordSuccess` 并就地 `setCurrent(i)`，随即结束该协议本轮（沿用 M7 的「一次成功即停」）。理由：current 之后（乃至整条链只有它自己）的 open 实例在原区间外，永远进不了探测，导致永久降级——这是实测「两分多钟不探测」的第二个根因 |
| M32 | **单实例协议不参与熔断**（用户 m01157 拍板「按建议改」）：该协议只有 1 个实例时，实例永不进入 open 状态——无论连续失败多少次都保持 closed，`degradedUntil` 恒为零，不产生探测，监控页状态恒为「正常」。失败仍照常计入统计（`requests`/`failed`/`recentErrors` 等），只是**不触发熔断状态迁移**。理由：熔断的前提是「有别的实例可切」，单实例时 `serveChain` 的全 open 分支本来就把请求发给它自己，熔断既拦不住流量，又让状态灯长期亮红、`degradedSince` 反复刷新，与事实不符。**推论**：①单实例无需探测恢复（原 M15ad 场景不再成立）；②`advanceCurrent`/`allInstancesOpen` 在单实例协议下不再被触发；③多实例协议的链尾探测（M15ae）不受影响，仍需保留 |

### 24.16.3 接口

无新增接口。`POST /api/instances/lock` / `unlock`、`GET /api/stats`、WS `probe` / `probeResult` 字段均不变。

### 24.16.4 监控页探测请求（修订 §24.15.4）

- 「方法/路径」列与真实请求**完全同构**：两行（`POST` + 路径），不含任何探测相关节点。
- 「探测」徽标显示在**「客户端」列**，该列在探测行里**只显示徽标**——IP 与 UA 都不显示（不是 `-`）。
- 状态列：未结束显示 `探测中`，结束显示 `成功` / `失败`（沿用探测行的琥珀色与左侧竖条区分）。
- 整行左侧 3px 琥珀色竖条 + 淡琥珀背景保持不变；探测行同样进环形缓冲、不进统计累计。

### 24.16.5 M 组验收

| # | 标准 |
|---|---|
| M15u | 实例转为 `open` 后**没有任何真实请求**，后台定时器仍会在 ≤ 1s（M23）内发起第一轮探测；此后每轮间隔不小于 `degrade_duration`。用假时钟断言：不加任何真实请求，推进时钟即可观测到探测请求 |
| M15v | 真实请求的响应时延不受探测影响：构造一个探测必然阻塞到超时的场景，同时发一个能立刻成功到其他实例的请求，断言该请求在远小于实例 `timeout` 的时间内返回（例如 < 1s，而 `timeout = 15s`）。同时断言 `serveChain` 不再调用 `maybeProbe` |
| M15w | Anthropic 协议的探测请求打到 `/anthropic/v1/messages`（不是 `/v1/messages`），OpenAI 协议打到 `/v1/chat/completions`；两者都用该实例的 `timeout`、`max_tokens: 1`、非流式 |
| M15x | 多个 `open` 实例在同一轮内**并发**探测：构造两个都会阻塞到超时的实例，断言一轮探测的总耗时远小于「2 × timeout」（例如 < timeout + 2s） |
| M15y | 无 `open` 实例时后台定时器不发任何探测请求（计数为 0）；全部探测成功后停止探测，直到再次出现 `open` |
| M15z | 监控页探测行状态列：未结束为 `探测中`、结束为 `成功` / `失败`，页面源码中**不存在** `探测成功` / `探测失败` 字符串拼接 |
| M15aa | 监控页探测行的「方法/路径」列 DOM 与真实请求**完全同构**（两行：`POST` + 路径），不含任何探测徽标节点；路径列不出现三行 |
| M15ab | 监控页探测行的「客户端」列**只渲染 `探测` 徽标**，IP 与 UA 两个文本节点内容为空（不是 `-`、不含 `model-router-probe`）；真实请求的客户端列仍是 IP + UA 两行、不带徽标 |
| M15ac | `go build` / `go vet` / `gofmt` / `go test ./... -race` 全绿；新增测试覆盖周期性探测、请求不被阻塞、探测路径、并发探测、无 open 时不探测 |
| M15ad | 单实例协议连续失败超过 `failure_threshold` 后，实例状态恒为 `closed`、`degradedSince` 恒为 0、后台 tick 不产生任何探测请求；同时该实例的 `requests`/`failed` 计数正常累加，`recentErrors` 照常记录 |
| M15ae | 多实例协议中，链尾实例降级且 `current` 位于它之前的健康实例时，链尾的 open 实例同样会被后台 tick 探测；探测成功后退回链尾继续承接流量（原 M15ad 描述的「`current` 停在 0 的单实例」场景已由 M32 取代） |
| M15af | 非流式响应的请求统计在客户端读取完成前已完成计数（`copyBody`/`copyBodyTee` 不再逐块 Flush）；SSE 仍逐块 flush |

---

## 24.17 取消主动探测，改为真实请求驱动的半开熔断（M33–M40）

### 24.17.1 为什么要改

§24.15 / §24.16 的主动探测方案在生产中失效，实测证据：

1. **探测量级差四个数量级**。真实请求平均输入 12–18 万 token（`/api/stats` 实测：MTI/deepseek 513 请求 / 70,417,916 输入 token ≈ 137,266 token/条；MTI/aliyun ≈ 183,673；DeepSeek ≈ 124,620），而探测报文是 `{"model":"mti-flash","max_tokens":1,"messages":[{"role":"user","content":"ping"}]}`，仅 11 token。探测成功只证明「端口能接 POST 并回 200」，对「能否扛住一条真实请求」没有预测力。
2. **判定只读 HTTP 状态码**（`probeSucceeded`），连「纯 thinking、无正文、`stop_reason: max_tokens` 的残废响应」都判为健康。
3. **后果**：上游过载时探测照样秒回成功（截图实测 90–147ms），`current` 被拉回故障实例，真实请求继续超时。用户体感「探测都是成功的，但实际请求又超时」，探测失去意义。
4. 附带缺陷：`degradedUntil` 到期后 `isOpen()` 自动变 false，实例「静默恢复」，但 `current` 没有回退逻辑，导致健康的早序实例被永久跳过（`buildChainFrom` 的 `base = start` 截断）。

**用户决策（m01343 原话）**：「改成真实请求去探测，还是half-open的语义，先从第一个实例开始试，哪个实例通就变成closed，后续的请求都往这里走，不通就重置回退时间。这样也就不存在专门探测的说法了，空闲的时候回退时间到了实例就回到closed，每次请求都发往第一个closed，如果都open就用第一个实例。」

### 24.17.2 M 组决策（承 §24.16 编号）

| # | 决策 |
|---|---|
| M33 | **取消主动探测**：删除后台探测定时器（`startProber`/`stopProber`/`probeCheckInterval`）、`probeTick`、`probeProtocol`、`sendProbe`、`newProbeChatBody`、`probeChatPath`、`probeModel`、`probeSucceeded` 及其专用状态字段（`probing`/`probeAttempted`/`probeOpen`/`nextProbe`）。`serveChain` 不再做任何探测。**恢复完全由真实请求驱动** |
| M34 | **实例只剩两态可见**：`closed`（正常）与 `open`（降级），外加 `locked`（已锁定）与 `-`（被锁定时的其余实例）。删除 `probe` 态与监控页「探测中」 |
| M35 | **半开判定只看向「回退时间」**：`isOpen()` 即 `!degradedUntil.IsZero()`。`degradedUntil` 到期后实例**自动回到 `closed`**——不需要任何探测、不需要请求，空闲时靠时间自然恢复（用户原话「空闲的时候回退时间到了实例就回到closed」） |
| M36 | **选路规则：第一个 `closed` 优先，全 `open` 用第一个**（取代 M8/M9/M31/M32 的 `current` 指针语义）。每次请求按配置顺序遍历该协议的候选链，**选择链上第一个 `closed` 的实例**承接；若链上全部实例都是 `open`，则发往**链上第一个**实例。请求成功后该实例保持 `closed`；因此「哪个实例通，后续请求就往这里走」由状态本身自然达成，**无需维护 `current` 指针** |
| M37 | **删除 `current` 指针**：`protocolRuntime.current`、`setCurrent`、`advanceCurrent`、`noteRealSuccess`、`buildChainFrom` 的 `base = start` 全部移除。这样不再出现「`current` 停在后面、前面的实例已恢复健康却永远不被选中」的缺陷。锁定时 `locked` 字段语义保持（锁定实例取代选路结果） |
| M38 | **失败即重置回退时间**：链上某个实例的真实请求失败，该实例 `degradedUntil = now + degrade_duration`（达到 `failure_threshold` 才真正转 `open`，未达阈值仅累计计数）。已在 `open` 的实例若再次失败，`degradedUntil` 直接重置为 `now + degrade_duration`（不加退避、不指数增长），即用户原话「不通就重置回退时间」 |
| M39 | **失败后继续尝试链上的下一个 `closed` 实例**：沿用既有回退语义——一个实例失败即重试下一个候选，全部失败才 502。区别仅在于候选顺序由 M36 决定，`open` 实例在遍历中被跳过（除非全部 `open`，此时按 M36 只打第一个） |
| M40 | **监控页/统计页清理**：删除「探测」徽标、探测行琥珀色标识、「探测中」状态与 `probe`/`probeResult` 字段的渲染；`/api/stats` 的 `state` 只可能取 `closed`/`open`/`locked`/`-`；后台不再产生 `model-router-probe` 用户代理的请求。`degradedSince`/`reason` 展示不变 |

### 24.17.3 接口

无新增、无删除接口。`/api/stats` 的 `instances[]` 字段保持，但 `state` 的取值域收窄（删除 `probe`）；`probe`/`probeResult` 若仍存在于 WS 事件结构中可以保留字段但**永不置真**，或直接删除。

### 24.17.4 验收

| # | 标准 |
|---|---|
| M15ag | 「按第一个 `closed` 选路」：三个实例 [A, B, C]，A 为 `open`、B/C 为 `closed` 时，请求打到 **B**；A 的 `degradedUntil` 到期后再发请求，打到 **A** |
| M15ah | 「全 `open` 打第一个」：三个实例全为 `open` 时，请求打到 **A**（链上第一个），不是链尾 |
| M15ai | 「失败重置回退时间」：实例 A 连续失败达到阈值转 `open`，推进时钟至 `degradedUntil` 前 1s 再令其失败一次，断言 `degradedUntil` 被重置为 `now + degrade_duration`，不发生指数增长 |
| M15aj | 「空闲自动恢复」：实例 A 转 `open` 后**不发任何请求**，仅推进时钟越过 `degradedUntil`，断言 A 的 `state` 变为 `closed`，且下一条请求打到 A |
| M15ak | 「不存在的 `current`」：源码中不再出现 `protocolRuntime.current`、`setCurrent`、`advanceCurrent`、`noteRealSuccess`、`base = start`；`/api/stats` 不再输出 `current` 字段（或其恒为 false） |
| M15al | 「无主动探测」：源码中不再存在后台探测定时器与 `sendProbe` 等探测函数；构造实例全为 `open` 且推进时钟 5 分钟、不发任何请求，断言上游收到的请求数**为 0**；`/api/stats` 的 `requests.total` 不因内部行为增加 |
| M15am | 「监控页无探测」：页面源码中不存在 `probe-badge`、「探测中」、「探测成功」/「探测失败」字符串；实例统计表状态列只可能显示 `正常` / `降级 HH:MM:SS` / `已锁定` / `-` |
| M15an | 「请求不被探测阻塞」：所有真实请求的时延只取决于上游本身，不存在被内部探测串行阻塞的路径 |
| M15ao | 「锁定仍然生效」：锁定某实例后所有请求只发往它；解锁后按 M36 重新选路（第一个 `closed`）；锁定状态不落盘 |
| M15ap | 单实例协议：始终 `closed`（沿用 M32 的结论），请求全部发往该实例，不出现 `open` 状态 |
| M15aq | `go build` / `go vet` / `gofmt` / `go test ./... -race` 全绿；测试覆盖 M15ag–M15ap 全部场景 |
---

## 24.18 回退时间默认值改回 5 分钟（M41–M44）

### 24.18.1 背景

§24.15 M16 把 `fallback.degrade_duration` 的默认值由 `5m` 改为 `1m`，理由是「它同时是探测周期」——探测要足够勤才能及时恢复。

§24.17 取消了主动探测，改为真实请求驱动的半开熔断后，这个理由不复存在。`degrade_duration` 现在的唯一语义是「实例失败后，多久之内不再把流量交给它」。

用户原话（m01462）：

> 「以前改为1m是因为有探测，现在探测取消了，回退时间默认还是改成5分钟吧」

1m 在无探测模型下过短：一次上游抖动就会让实例被摘除 1 分钟，而重试恢复完全依赖真实流量，频繁摘除会让正常请求反复承受一次失败再重试的代价。

### 24.18.2 M 组决策

| 编号 | 决策 |
| --- | --- |
| M41 | `fallback.degrade_duration` 的**代码默认值**由 `1m` 改回 `5m`（`internal/config/config.go` 的 `DefaultDegradeDuration`） |
| M42 | `config.example.json` 的 `degrade_duration` 示例值同步改为 `5m` |
| M43 | `README.md` 中三处表述同步：参数总览、示例配置片段、配置字段表中「默认」一栏；同时删除「同时是半开探测周期」「同时是统一半开探测周期」等已失效的描述（§24.17 已无探测） |
| M44 | 配置**显式填写的值**不受影响：用户配多少就按多少生效。本次只改默认值与文档，不改任何运行时语义 |

### 24.18.3 接口

无新增、无删除。`fallback.degrade_duration` 字段、类型、校验规则（Duration 字符串 > 0）全部不变，仅缺省值变化。

`GET /api/config` 返回的 `fallback.degrade_duration` 在配置未显式填写时，应为 `5m`。

### 24.18.4 验收

| 编号 | 验收标准 |
| --- | --- |
| M15ar | `config.example.json` 中 `degrade_duration` 为 `"5m"` |
| M15as | 配置未显式填写 `degrade_duration` 时，解析结果为 `5m`（代码默认值 = `5m`） |
| M15at | README 中不再出现「同时是半开探测周期」「同时是统一半开探测周期」等探测相关表述；参数默认值表述为 `5m` |
| M15au | `go build ./...`、`go vet ./...`、`gofmt -l .`（无输出）、`go test ./... -race` 全绿 |

## 24.19 全 open 时逐实例向后重试

### 24.19.1 现状与问题

§24.17 定下「全部实例都 open 时，请求发往链上的**第一个**实例」。但当前实现（`internal/proxy/proxy.go` 的 `serveChain`）在构造尝试列表时，全 open 分支只把链上第一个候选塞进 `attempts`：

```go
attempts := make([]*candidate, 0, len(chain))
for _, cand := range chain {
    if !cand.inst.isOpen() {
        attempts = append(attempts, cand)
    }
}
if len(attempts) == 0 && len(chain) > 0 {
    attempts = append(attempts, chain[0])   // 只塞第一个
}
```

`attempts` 只有 1 个元素时，重试循环也只跑一次：第一个实例失败就返回 502，**不会继续尝试链上后续实例**。在全部实例都被摘除（例如上游整体抖动）的场景下，这等于整个协议只能打一个实例，可用性显著劣化。

用户原话：

> 「我希望的是 all open 时请求超时依然一个个实例往后重试」

### 24.19.2 M 组决策

| 编号 | 决策 |
| --- | --- |
| M45 | 全部候选都 open（或没有 closed 候选）时，尝试列表为**整条链按顺序的全部候选**，而不是只有链上第一个 |
| M46 | 全 open 时的顺序与 closed 时一致：从链上索引 0 开始依次向后，第一个成功即返回，全部失败才 502 |
| M47 | 撤订 M36 中「全 open 只打第一个」的限制：§24.17 的「发往链上第一个」仅用于表述**起点**，不再是**唯一尝试对象** |
| M48 | 混合场景（部分 closed、部分 open）行为不变：closed 候选按链序全部尝试，open 候选依旧跳过 |

### 24.19.3 接口

无新增、无删除。`/api/stats` 的实例 `state` 取值（closed / open / locked / -）与语义均不变。

### 24.19.4 验收

| 编号 | 验收标准 |
| --- | --- |
| M15av | 三实例全部 open 时，第一个实例失败后请求继续尝试第二个；第二个失败后继续尝试第三个 |
| M15av-b | 三实例全部 open 且第一个实例成功时，只调用第一个实例（不预发后续实例的请求） |
| M15aw | 三实例全部 open 且全部失败时，返回 502，且三个实例各被调用恰好 1 次 |
| M15ax | 两实例 closed + 一实例 open 时，只尝试两个 closed 实例，open 实例调用次数为 0 |
| M15ay | 全部 open 时每个失败实例仍按规则记录失败并重置 `degradedUntil`（不因重试而跳过失败记账） |
| M15az | `go build ./...`、`go vet ./...`、`gofmt -l .`（无输出）、`go test ./... -race` 全绿 |

### 24.19.5 版本

本次改动 bump dev 版本：`0.1.4` → `0.1.5`（`internal/version/version.txt`），CHANGELOG 新增 `[0.1.5]` 段落。

## 24.20 上游非 2xx 响应按状态码分类处置（0.1.6）

### 24.20.1 现状与问题

`internal/proxy/proxy.go` 的 `tryCandidate` 拿到上游响应后，**只读状态码、不做分类**：只要响应头到达（`cand.inst.client.Do` 未报错），无论状态码是 200 还是 400/500，一律继续把响应体转给客户端，并在结束时调用 `h.recordSuccess(cand.inst)`。后果三条：

1. **熔断失效。** 上游持续返回 5xx 错误时，实例连续失败计数不增加，熔断器永远不打开，请求会一直砸向坏实例。
2. **不会换实例。** 上游返回 4xx/5xx 时，客户端（如 Claude Code）看到的是错误 JSON，而不是被转发到链上下一个健康实例。
3. **错误不可观测。** `recentErrors` 里只记录网关自己产生的 502（`error: "upstream request failed"`），上游返回的错误体不落任何日志；生产环境排查「为什么思考块 missing」这类问题时，网关日志里一条线索都没有。

一次真实事故（2026-11-18，生产 10.168.4.160）暴露了这一点：客户端报 `The content[].thinking in the thinking mode must be passed back to the API`，但网关侧 `grep -c thinking` 结果为 **0**，`recentErrors` 里只有 `status: 400 / error: ""` 的空白记录，无法判断是哪个上游、因为什么拒收。

用户原话：

> 「我在 10.168.4.160 上跑的容器，前面在降级的时候导致claude code报错，这是分析的报告，你让claude code确认下是实现层面有什么问题么？」

### 24.20.2 M 组决策

| 编号 | 决策 |
| --- | --- |
| M49 | 上游状态码 ≥ 400 且 < 500 时：**响应头尚未提交给客户端**，该候选判为失败，按既有链路换下一个候选；同时记录该实例失败（与连接错误同等对待） |
| M50 | 上游状态码 ≥ 500 或 < 200 时：同上，判为失败并换实例；记录实例失败 |
| M51 | 上游 3xx：Go 客户端默认跟随重定向；最终仍未落在 2xx 的按 M49/M50 处理 |
| M52 | 不可换实例的例外：响应头已经提交（流式已写出首块、非流式已 `WriteHeader`），或候选列表已耗尽。此时原样转发上游响应体，并记 `finalStatus = 上游状态码`，不得改写状态码 |
| M53 | 实时监控与 `recentErrors` 必须记下上游非 2xx 的**状态码 + 实例名 + 上游模型 + 响应体前 N 字节**（用于定位错误原因），且 `error` 字段不得为空串 |
| M54 | 换实例时重试原因新增取值 `upstream_status`；该值出现在实时监控的重试标记与统计中 |
| M55 | 该分类**只作用于真实转发路径**（`tryCandidate`）。本地生成响应（models 列表、管理面、鉴权失败等）不受影响 |

### 24.20.3 接口

- `/api/stats` 的 `recentErrors[]` 追加字段 `upstreamBody`（字符串，截断），既有字段语义不变。
- 实时监控的重试原因枚举增加 `upstream_status`。

### 24.20.4 验收

| 编号 | 验收标准 |
| --- | --- |
| M15ba | 上游返回 400 且响应头未提交时，请求换到链上下一个候选；该实例失败计数 +1 |
| M15bb | 上游返回 500 且响应头未提交时，同上 |
| M15bc | 上游返回 400 且候选列表已耗尽时，客户端收到上游原始的 400 与原始响应体，`recentErrors` 中该条 `status=400` 且 `error`/`upstreamBody` 非空 |
| M15bd | 流式请求已写出首个数据块后上游再返回错误（不应发生，但需防回归）：客户端收到已提交的响应，不再换实例 |
| M15be | 上游连续返回 5xx 达到 `failure_threshold` 次后，实例进入 open |
| M15bf | 本地生成的错误响应（如 401 未带凭据）不触发实例失败计数 |
| M15bg | `go build ./...`、`go vet ./...`、`gofmt -l .`（无输出）、`go test ./... -race` 全绿 |

### 24.20.5 版本

本次改动 bump dev 版本：`0.1.5` → `0.1.6`（`internal/version/version.txt`），CHANGELOG 新增 `[0.1.6]` 段落。

## 24.21 全候选失败时的补漏尝试与错误透传（0.1.6）

### 24.21.1 现状与问题

§24.15 M8 与 §24.19 M45–M48 都只描述「尝试哪些候选」，没有规定**尝试完之后的兜底动作**。当前实现的兜底是统一的 `502 upstream request failed`：

```go
log.Printf("%s %s -> (%s) total=%ss status=502 all candidates failed", ...)
finalStatus = http.StatusBadGateway
finalErr = errors.New("upstream request failed")
writeProtocolError(w, proto, http.StatusBadGateway, "upstream request failed")
```

这带来两个问题：

1. **上游的真实拒绝原因丢失。** 上游用 4xx 明确说明了原因（例如参数不合法、思考块缺失、内容审核），网关却换成一句 `upstream request failed`。客户端拿不到可行动的线索，运维也无法从客户端侧判断。
2. **有实例一次都没被打过。** 候选链由模型匹配决定，某些实例可能因为模型不匹配（或被 `open` 跳过）完全不进链。链上候选全部失败后，这些实例**一次都没试过**，请求直接 502。

用户原话（2026-11-18）：

> 「all open 的时候，都向后重试一次，如果你向后重试之后，依然都没有效果呢？那是不是应该用原模型，用所有实例再向后重试一次？」

随后用户两次明确收窄口径：

> 「不要这样改，BC失败了可以再试下A，但是依然用映射名。」
> 「不需要额外的用原始名重试的逻辑」

即：**补漏仍然使用各实例自己的映射模型名，不引入「客户端原始模型名重跑」这一层。**

### 24.21.2 M 组决策

| 编号 | 决策 |
| --- | --- |
| M56 | 补漏实例集 = 该协议全部实例中**本次请求尚未被调用过**的实例，按配置顺序；与实例的闭包/开断状态无关 |
| M57 | 第 1 段：候选链按 §24.17–§24.19 的既有规则走完（closed 按链序、全 open 时整条链）。链上候选全部失败后进入补漏段 |
| M58 | 补漏段对每个漏掉实例发送的请求，模型名一律使用**该实例自己的映射模型**（`roles` 映射或 `models` 直连），与第 1 段完全一致；**不得**回退到客户端原始模型名 |
| M59 | 全部失败后返回最后一个候选的真实上游状态码与响应体；若所有候选都停在「未拿到响应头」阶段（连接失败/超时），才返回 502 |
| M60 | 补漏尝试的每一次失败与成功都照常记账（`recordFailure` / `recordSuccess`），实时监控中其重试原因为 `fallback_retry` |
| M61 | 每个实例在一次请求中最多被真实调用一次（第 1 段已调用过的实例不再进入补漏段） |
| M62 | 候选链已覆盖全部实例时不产生补漏尝试；锁定协议仍只打被锁定实例 |

### 24.21.3 接口

- 实时监控的重试原因枚举增加 `fallback_retry`。
- 无新增 HTTP 接口。

### 24.21.4 验收

| 编号 | 验收标准 |
| --- | --- |
| M15bh | 三实例闭包 + 二实例开断，闭包三实例全失败后，两个开断实例被依次调用 |
| M15bi | 全部实例失败且最后一个实例返回 400 时，客户端收到该 400 与其原始响应体，而不是 502 |
| M15bj | 全部实例都返回连接失败/超时时，客户端收到 502 `upstream request failed`（保持现有行为） |
| M15bk | 补漏段发出的请求体 `model` 字段等于**该实例的映射模型名**，不是客户端原始模型名 |
| M15bl | 五实例协议下每个实例的真实调用次数恰好 ≤ 1 次（不重复打同一实例）；链已覆盖全部实例时补漏不发生 |
| M15bm | 补漏尝试中的失败仍会让对应实例累计失败次数 |
| M15bn | `go build ./...`、`go vet ./...`、`gofmt -l .`（无输出）、`go test ./... -race` 全绿 |

### 24.21.5 版本

与 §24.20 合并到同一次 dev bump：`0.1.5` → `0.1.6`。

## 24.22 失败请求的客户端请求体取证（并入 0.1.6）

### 24.22.1 现状与问题

§24.20 只把**上游返回了什么**记进了 `recentErrors.upstreamBody`，**没有记客户端发来了什么**。这导致一个关键判据缺失。

2026-11-18 的 400 事故（`The content[].thinking in the thinking mode must be passed back to the API`）已经查明触发条件，但这些结论全部来自**主代理手工构造**的请求：

- 助手消息**不含 `tool_use`** 时，丢掉 `thinking` 块、签名置空、签名改成 bogus、`thinking` 文本置空、`thinking` 开↔关任意组合，DeepSeek 官方**一律 200，不校验**。
- 助手消息**含 `tool_use`** 时，丢掉 `thinking` 块**必定 400**，且**与 `thinking` 开关无关**（关掉也 400）；保留 `thinking` 块原样回传则 200。

而生产上真实失败的请求，其历史里究竟**有没有** `tool_use`、**有没有** `thinking` 块，网关**一条都没记**。于是无法区分三种可能：

1. 客户端发来的历史里，含 `tool_use` 的助手消息确实缺 `thinking` 块 —— 问题在客户端侧，网关可以补救；
2. 客户端发来的历史是完整的，是网关转发时弄丢了 —— 网关有 bug；
3. 客户端历史完整、网关也没丢，但官方仍 400 —— 还有未发现的触发条件。

**在拿到这条证据之前动手修，等于在没验证的情况下再猜一次。** 用户拍板先取证：

> 「第一步先做，确定了问题才能修对」

### 24.22.2 M 组决策

| 编号 | 决策 |
| --- | --- |
| M63 | 当一个请求**最终以失败结束**（`status >= 400` 或为空）时，网关必须记录该请求**客户端原始请求体**的结构摘要 `reqShape` |
| M64 | `reqShape` 必须是**结构摘要**，不得记录请求体原文或任何消息正文。仅允许包含：消息条数、每条消息的 role、每条消息 content 的**块类型序列**、以及每条助手消息是否**同时**含 `tool_use` 与 `thinking` 块 |
| M65 | `reqShape` 总长受限（默认 1024 字符，超出截断并加省略标记），且**无论请求体多大**都不允许超过该上限 |
| M66 | `reqShape` 必须能在 `/api/stats` 的 `recentErrors[]` 中读取（字段名 `reqShape`），并随实时监控终态快照一并下发 |
| M67 | 请求体已解析失败（非 JSON）时，`reqShape` 记录为能表达的尽力摘要（如 `unparsable: <前 64 字符>`），不得使请求本身失败 |
| M68 | 取证**只记录、不改变任何转发行为**：不得丢弃、改写、补充任何消息或块，不得因此改变状态码、候选链或熔断计数 |
| M69 | `reqShape` 只在请求失败时采集；成功请求不新增该字段，避免常态开销与信息冗余 |

`reqShape` 的文本格式（可机械校验）：

```
msgs=4 | 0:user[text] | 1:assistant[thinking,text,tool_use] | 2:user[tool_result] | 3:user[text]
```

当某条助手消息同时含 `tool_use` 而**不含** `thinking` 时，该条必须追加标记 `!!tool_use_without_thinking`：

```
msgs=2 | 0:user[text] | 1:assistant[text,tool_use]!!tool_use_without_thinking
```

### 24.22.3 接口

- `/api/stats` 的 `recentErrors[]` 追加字段 `reqShape`（字符串，omitempty）。
- WS `end` 事件与 `snapshot` 事件中的请求记录追加 `reqShape`（字符串，omitempty）。
- 无新增 HTTP 路由。

### 24.22.4 验收

| 编号 | 验收标准 |
| --- | --- |
| M15bo | 失败请求的 `recentErrors[]` 条目含 `reqShape`，且其中列出的消息条数与请求体一致 |
| M15bp | 助手消息含 `tool_use` 但不含 `thinking` 块时，`reqShape` 该条带 `!!tool_use_without_thinking` 标记 |
| M15bq | 助手消息同时含 `tool_use` 与 `thinking` 块时，**不**带该标记 |
| M15br | 请求体为 1 MB、20 条消息时，`reqShape` 长度仍 ≤ 1024 字符 |
| M15bs | 请求体不是合法 JSON 时，请求的转发行为不受影响，`reqShape` 为 `unparsable:` 摘要 |
| M15bt | 成功请求的 `recentErrors` 中不出现 `reqShape` |
| M15bu | 同一请求转发到上游的请求体与不开启取证时**逐字节相同**（取证不修改请求体） |
| M15bv | `go build ./...`、`go vet ./...`、`gofmt -l .`（无输出）、`go test ./... -race` 全绿 |

### 24.22.5 版本

用户拍板不单独 bump：本次改动并入 `0.1.6`（`internal/version/version.txt` 保持 `0.1.6`），CHANGELOG 条目并入 `[0.1.6]` 段落。

### 24.22.6 后续（不在本次范围）

拿到真实 `reqShape` 后再定修复方案，三种病因对应三种修法，**本次不实现任何修复逻辑**。

## 24.23 流式「无内容」超时与流阶段可观测性（并入 0.1.6）

### 24.23.1 现状与问题

2026-11-18 19:38 生产 10.168.4.160 实测到一次**卡死**：一条 `/anthropic/v1/messages` 请求耗时 **316.519s** 才结束，日志片段为

```
19:33:53  candidate MTI/deepseek failed: ... timeout awaiting response headers
19:38:54  candidate DeepSeek     failed: Post "https://api.deepseek.com/anthropic/v1/messages?beta=true": context canceled
19:38:54  candidate MTI/qwen      failed: ... context canceled
19:38:54  -> MTI/qwen/qwen3.8-flash (claude-sonnet-5) in 316.519s
19:38:54  -> (anthropic) total=316.519s status=502 all candidates failed
```

`DeepSeek` 那一行不是「等待响应头超时」，而是 **`context canceled`** —— 说明**响应头早已收到、连接已建立**，之后一直等不到可用的数据，直到 HTTP/2 客户端放弃。

根因三条，全部在 `internal/proxy/proxy.go`：

- **R1｜流式请求的实例 `timeout` 只管到响应头。** `newClient` 只设 `ResponseHeaderTimeout: timeout`；上游一旦返回 200 响应头，该超时即失效（§24.23 实测：`deepseek-chat` 流式 **0.12s** 就返回响应头）。此后整条流没有任何与实例 `timeout` 挂钩的保护。
- **R2｜`idle_timeout` 被任意数据重置。** `copySSE` 在 `proxy.go:2161` 每读到**一块数据**就 `arm(idle, true)`。上游在思考阶段持续发送 `event: ping` 心跳时，`idle_timeout`（生产 30s）被无限顺延，**永远触发不了**。
- **R3｜`first_token_timeout` 在 committed 之后失效。** `proxy.go:2167` 的判定是 `if firstHit.Load() && outcome.chunks == 0`；只要收到过哪怕一个字节，`chunks > 0`，此后首块超时不再生效。

三者叠加出一个**真空**：上游在发数据、但发的是空数据（心跳/无 delta 的块），`idle_timeout` 看不见、`min_tokens_per_second` 只能统计内容块也看不见、实例 `timeout` 已失效 —— 请求无限期挂着。

生产同一时段还存在另一种形态：`total=152.987s`、`total=259.862s` 同样返回 400，说明卡顿不是孤例。

另：日志当前缺流阶段信息 —— 只有一个总耗时 `in X.XXXs`，无法区分「连接慢」「出字慢」「thinking 阶段空转」，也无法判断流里到底有多少心跳。**下次再卡住仍然只能靠猜**。

### 24.23.2 M 组决策（承 §24.22 编号）

| # | 决策 |
| --- | --- |
| M70 | **新增「无内容超时」（content idle timeout）**，语义是「距离上一个**内容块**多久没到」，而不是「距离上一块数据多久」。只有**内容块**到达才重置该定时器；`event: ping`、空 delta、`message_start` / `content_block_start` / `content_block_stop` 等**非内容事件一律不重置**。 |
| M71 | **内容块的判定**沿用现有 token 统计口径：Anthropic 协议为 `content_block_delta` 且 `delta.type` 属于 `text_delta` / `thinking_delta` / `input_json_delta`；OpenAI 协议为流式 chunk 中 `choices[].delta` 含非空 `content` 或 `reasoning_content`（即现有 `sseState` 已经在统计的那些块）。**复用现有解析结果，不另起一套判定**。 |
| M72 | **超时时长 = 该实例的 `timeout`（默认 15s）**，不新增配置项、不扩展现有 `fallback` 字段。理由：用户要求「实例超时对 committed 后的流也生效」，直接复用 `timeout` 语义最贴近直觉；新增配置项会让 §24.23 变成又一个可调旋钮，超出本次范围。 |
| M73 | **无内容超时触发后的处置与 `idle_timeout` 完全一致**（复用 `outcome.idle` 分支，`proxy.go:1656-1668`）：Anthropic 且未 `finished` 时写 `pause_turn` 尾巴、OpenAI 时 `truncateClientConnection`、计一次实例失败、日志打印、`return true`（已 committed，**不再降级**）。**不新增分支、不改变既有语义**。 |
| M74 | **原有 `idle_timeout` 保留不变**：它仍是「块间隔」超时，仍然每块重置。两个定时器**同时存在、先到先触发**。`idle_timeout` 管「上游彻底不吭声」，无内容超时管「上游一直在吭声但没内容」。 |
| M75 | **`min_tokens_per_second` 的低速判定逻辑不改**（§19 已定义），仅在其判定基于内容块这一点上与本方案天然一致。本次**不调整任何吞吐阈值**。 |
| M76 | **流阶段可观测性**（只加日志，**不改任何行为**）。每个流式请求结束时（无论成功/失败/超时）打**一行**阶段摘要，字段固定为：`hdr=<到响应头耗时>`、`first_block=<响应头→第一块数据>`、`first_content=<响应头→第一个内容块>`、`blocks=<数据块数>`、`pings=<心跳/非内容事件数>`、`content=<内容块数>`、`last_gap=<最后一块数据距今>`。任一值为「未到达」时打印 `-`。 |
| M77 | 阶段摘要**只在流式请求上打印**，非流式不打印。**不进入 `/api/stats`、不进 WS 事件**（用户明确「统计字段先不加」，本轮只做日志）。 |
| M78 | **超时触发时额外补打一行原因行**，写明触发了哪个定时器（`content-idle` / `idle` / `first-token` / `slow`）、该实例名、及其对应阈值。内容与 M76 的阶段摘要**分开打印**，便于 grep。 |
| M79 | **转发行为零改变**：M76/M77/M78 的日志不得影响写入客户端的数据、不得改变 timing、不得改变任何状态机迁移。**转发字节必须与改动前逐字节相同**（复用 §24.22 M15bu 的验收方式）。 |
| M80 | 本方案**不修**「拿到确定 4xx 后仍继续降级、白付 15s×N」的问题（用户 m02891 明确「1不做」），§24.21 的补漏语义**保持不变**。 |

### 24.23.3 接口

无新增 HTTP 接口、无新增配置字段、无新增 `/api/stats` 字段。

新增内部字段（`internal/proxy/proxy.go` 的 `sseOutcome`）：

```go
type sseOutcome struct {
    // ... 既有字段 ...
    contentIdle      bool          // M70：无内容超时是否触发
    firstBlockDelay  time.Duration // M76：响应头→第一块数据
    firstContentDelay time.Duration // M76：响应头→第一个内容块
    blockCount       int           // M76：数据块数
    pingCount        int           // M76：心跳/非内容事件数
    contentCount     int           // M76：内容块数
    lastGap          time.Duration // M76：最后一块数据距结束
}
```

日志样例（M76，固定字段顺序、`=` 分隔、空格分隔键值对）：

```
stream MTI/qwen/qwen3.8-flash hdr=0.15s first_block=0.42s first_content=- blocks=37 pings=36 content=0 last_gap=30.0s
```

日志样例（M78）：

```
instance MTI/qwen timeout: content-idle for 15s (no content block); stream terminated
```

### 24.23.4 验收

| 编号 | 验收标准 |
| --- | --- |
| M15bw | 上游只发心跳（`event: ping`）不发任何内容块时，到达实例 `timeout`（默认 15s）后流被终止，客户端收到 `pause_turn` 尾巴（Anthropic）或连接被截断（OpenAI），该实例计一次失败；**不得**出现「无限期挂起」 |
| M15bx | 上游心跳正常且持续发送内容块时，流**不被**无内容超时打断（正常长回答不受影响） |
| M15by | 上游彻底不吭声（一个字节都不发）时，仍由既有 `first_token_timeout` / `idle_timeout` 生效，行为与改动前一致 |
| M15bz | 上游发 `message_start` / `content_block_start` / `content_block_stop` 等非内容事件但不发任何 delta 时，按 M70 不重置无内容定时器，超时正常触发 |
| M15ca | 无内容超时触发后的对外表现与 `idle_timeout` 完全一致（Anthropic 写 `pause_turn`、OpenAI 截断连接、计一次失败），断言两者产生相同的客户端可见字节 |
| M15cb | 每个流式请求结束打**恰好一行** M76 阶段摘要，字段名与顺序固定；未到达的字段打印 `-` |
| M15cc | 非流式请求**不**打印 M76 阶段摘要 |
| M15cd | 超时时额外打印 M78 原因行，写明触发的定时器名与实例名 |
| M15ce | 开启日志前后，转发到上游的请求体与返回给客户端的响应体**逐字节相同**（M79） |
| M15cf | 既有全部测试不改断言通过；`go build ./...`、`go vet ./...`、`gofmt -l .`（无输出）、`go test ./... -race` 全绿 |

### 24.23.5 版本

用户拍板不单独 bump：本次改动并入 `0.1.6`（`internal/version/version.txt` 保持 `0.1.6`），CHANGELOG 条目并入 `[0.1.6]` 段落。

### 24.23.6 不在本次范围

- 「拿到确定 4xx 后短路降级」（用户 m02891 明确「1不做」）
- 实例 `timeout` 与 `first_token_timeout` / `idle_timeout` 的职责重新划分（§24.2 定义保持）
- 流阶段统计进入 `/api/stats` 或 WS 事件（M77 明确不做）

## 24.24 流式响应中空 signature 的归一化（并入 0.1.6）

### 24.24.1 背景与问题

MTI（`10.168.4.230:30000`）在 Anthropic 协议下返回 thinking 块时，`signature` 有相当一部分是**空串 `""`**：

- `mti-flash` / `mti-pro`：返回 UUID 形态签名；
- `deepseek-v4.1-flash` / `qwen3.8-flash`：返回**空串**。

SDK 与上层客户端普遍把 `signature` 当作 thinking 块的有效性凭据。空签名可能导致两种后果之一：客户端**不把该 thinking 块写入历史**，或写入后在回传时被丢弃。无论哪种，都会让接下来的请求在「要求回传 thinking 块」的上游（DeepSeek 官方在思考开启时）上 400。

**注意**：本条**不声称**已证实「空签名是 400 的根因」。§24.23 已记录 dsh 走 MTI 后仍能正常降级的反例，因果链尚未闭合。本条目只做一件**独立成立**的改进：网关不应向下游输出语义不完整的字段。

### 24.24.2 M 组决策（承 §24.23 编号）

| # | 决策 |
| --- | --- |
| M81 | 只处理 **Anthropic 协议**的 **SSE 流式响应**。OpenAI 协议、非流式响应、请求侧一律不动。 |
| M82 | 识别对象：SSE 事件 `content_block_start` 中 `content_block.type == "thinking"` 且 `content_block.signature` **存在且为空串**（`""`），或 `signature` **字段缺失**。 |
| M83 | 命中时把 `signature` 改写为固定占位值 **`"model-router-placeholder"`**（常量 `placeholderThinkingSignature`）。**不生成随机值**，避免同一 history 每轮签名不同。 |
| M84 | 只改 `content_block_start` 事件。`content_block_delta` 的 `signature_delta` **不改**；`message_start` / `message_stop` / `ping` / `content_block_stop` **不改**。 |
| M85 | 改写**只发生在转发给客户端的字节上**；上游响应体本身、token 统计、usage 解析、吞吐计算一律使用**原始**字节，不得受改写影响。 |
| M86 | 每条 SSE 事件（以空行分隔的完整事件）**单独判定**：事件原文若能解析为 JSON 且命中 M82，则重写该事件的 `data:` 行；否则**逐字节原样转发该事件**。**不得**因解析失败而丢弃、截断或重排事件。 |
| M87 | 改写后的 JSON 必须**保留原字段顺序**且不引入额外字段；值为字符串时需正确转义。允许键顺序与上游不同，但同一个 thinking 块内的字段集合必须与原 JSON 相同。 |
| M88 | 解析开销与内存：单事件上限沿用 §20.2 的流式缓冲策略，**不得**为改写而把整条流读入内存；事件超长或解析失败一律走 M86 的透传分支。 |
| M89 | 日志：**每个请求最多一行**，仅当本次请求至少改写了一个 thinking 块时打印，形如 `thinking signature normalized: blocks=3 inst=<实例名>`。不改写则不打日志。 |
| M90 | 统计：不改 `/api/stats`、不进 WS 事件、不计入 `recentErrors`。 |
| M91 | 版本：**用户拍板不 bump**，继续用 `0.1.6`，CHANGELOG 并入 `[0.1.6]`。 |

### 24.24.3 验收（M15cg–M15cp）

| # | 验收点 |
| --- | --- |
| M15cg | `content_block_start`（thinking、signature 为空串）→ 客户端收到的 `signature` 为 `model-router-placeholder`。 |
| M15cp | `content_block_start`（thinking、signature 缺失）→ 客户端收到的 `signature` 为 `model-router-placeholder`。 |
| M15ch | `content_block_start`（thinking、signature 非空）→ **逐字节原样**转发，不改写。 |
| M15ci | `content_block_start`（`type != "thinking"`）→ 逐字节原样转发。 |
| M15cj | 非 thinking 的其它事件（`message_start`、`content_block_delta`、`ping`、`content_block_stop`、`message_stop`）→ 逐字节原样转发。 |
| M15ck | 无法解析的事件（半行、非 JSON、多行 data）→ **逐字节原样**转发，不丢字节、不改顺序。 |
| M15cl | usage / token 统计 / 吞吐不受改写影响：改写前后 `inTokens`、`outTokens`、`tps` 必须一致。 |
| M15cm | 日志：一次请求内改写 2 个 thinking 块 → 只打印 **1 行** `thinking signature normalized: blocks=2 inst=...`；未改写 → 不打印。 |
| M15co | OpenAI 协议流式响应、Anthropic 非流式响应 → 完全不受影响（与改动前逐字节一致）。 |

---

## §24.25 锁定实例的超时豁免与降级前模型映射修正（并入 0.1.6）

### 24.25.1 背景

用户报了三个叠加的现象（2026-11-18）：

1. **锁定后仍被 15s 强行掐断**：锁定语义（§24.15 M10）写明「熔断策略暂停——不计数、不探测、不回退」，但实例自己的 `timeout`（生产全为 15s）依然生效，`newClient` 把它同时用作 `DialContext` 连接超时、`TLSHandshakeTimeout`、`ResponseHeaderTimeout`。用户原话：「**锁定以后不应该熔断了，现在锁定了15s超时还是会强行掐断。一并改掉再部署**」。
2. **锁定后无兜底**：§24.15 M10 说「该协议的所有请求都发往该实例」，于是锁定实例一旦慢到 60s 以上，客户端只能干等，没有任何回退。
3. **降级请求打的是未映射的模型名**：`lockedCandidate`（`internal/proxy/circuit.go:228`）里 `buildChainFrom` 找不到该实例时走兜底分支，只对 OpenAI 写 `cand.model`，**Anthropic 分支只对 `fable`/`opus` 角色做映射，其余（含 `sonnet`、`haiku`、`*`）直接返回 `cand` 且 `rewrite=false`**——于是锁定场景下 `claude-sonnet-5` 被**原样**发往实例，违反 §21.2 模型名映射。同一类漏映射也存在于 §24.21 补漏轮（`fallbackCandidate`，`internal/proxy/proxy.go:1176`）。

三者叠加的实际后果：锁定 DeepSeek 时，官方模型可能因收到不认识的 `claude-sonnet-5` 而报错，而 15s 掐断又让用户没有别的路可走。

### 24.25.2 M 组决策（承 §24.24 编号）

| 编号 | 决策 |
|---|---|
| M92 | **锁定实例不计熔断**：沿用 §24.15 M10 的「不计数、不探测、不回退」。锁定实例上的任何失败（超时、非 2xx、连接错误）**一律不调用 `recordFailure`**，`consecutiveFailures` 保持原值、`degradedUntil` 不被设置。`recordSuccess` 行为不变。 |
| M93 | **锁定实例走「先锁定兜底链」**：请求到来时若某实例被锁定，尝试顺序改为 **`[锁定实例, 其余实例按配置顺序]**，锁定实例**恒排第一**。 |
| M94 | **锁定期间非锁定实例的流只在「拿到内容前」容忍**（用户 m03147「如果锁定的时候，未完成的请求如果不是锁定实例的，并且没有token响应的直接中断直接发往锁定的实例，不管有没有超时」）：当协议存在锁定实例时，任何**非锁定实例**的流式尝试，只要在**拿到第一个内容块之前**没有产出内容，就**立即中断**该上游并改发锁定实例；不设时间预算、不等超时。已产出过至少一个内容块的非锁定实例流**不中断**，继续跑到自然结束。 |
| M94a | **「有 token 响应」的口径沿用 §24.23 M71**：必须是真实内容块（`text_delta`/`thinking_delta`/`input_json_delta`），`ping`、`message_start`、`content_block_start`/`_stop`、空 delta 一律不算。**注意**：`signature_delta` 不算内容块（与 §24.24 M84 的改写范围无关，仅用于此处判定）。 |
| M94b | **触发时机 = 每次判定点**，不引入新定时器：在每次有 SSE 事件到达时判定一次（即现有 `copySSE` 的事件处理循环内），一旦判定「锁定存在 + 非锁定实例 + 尚未产出内容块」，立即关上游、本次尝试计失败（按 M92 只对非锁定实例计数）、把请求转交锁定实例。 |
| M94c | **转交语义**：改发锁定实例时，客户端尚未收到任何字节（因为内容块未到达，§24.20/§24.23 的响应头延迟提交保证了这点），因此转交对客户端**不可见**，不能出现重复响应头或半截响应；body 按 M99 使用锁定实例的映射模型名。 |
| M94d | **转交只发生一次**：一旦请求已经发往锁定实例，即使锁定实例自己也没内容，也**不再**回到非锁定实例，也不再中断，符合 M97「提交后不降级不掐断」。 |
| M95 | **锁定实例彻底豁免超时**（用户 m03136 拍板「M95 也豁免，锁定的时候所有熔断特性都不要，这种情况下就等客户端自己控制超时」）：锁定实例的 `DialContext` 连接超时、`TLSHandshakeTimeout`、`ResponseHeaderTimeout` **全部设为 0（不设超时）**。该实例的 `timeout` 对锁定请求**完全不生效**。代价：上游卡死时网关会一直挂住，由客户端自己的超时控制兜底。 |
| M96 | **非连接类失败仍可降级**：锁定实例返回**非 2xx 状态码**时（响应头已到达，不属于超时范畴），按 M93 的兜底链继续尝试其余实例；终态按 §24.20 语义处置。连接层面的失败既然没有超时，就只剩「客户端主动断开」和「TCP 层报错」两种，前者结束请求，后者按 M93 降级。 |
| M97 | **锁定实例提交响应头后不降级、不掐断**（锁定实例自身豁免全部时间预算；M94 的中断规则只针对非锁定实例）：一旦响应头提交（`outcome.committed == true`），即使后续 `content-idle` 想触发也不再关流；客户端拿到的是上游真实流，直到自然结束。若上游真的永久静默，客户端只能等到自己断开——这是 M94 的必然代价，接受。 |
| M98 | **解锁行为不变**：解锁后全部实例状态清零、重新计数（沿用 §24.15 M10），锁定期间累积的失败**不追溯计入**。 |
| M99 | **锁定场景必须使用实例自己的映射模型名**：`lockedCandidate` 的 Anthropic 分支必须与 `fallbackCandidate` 一致——按 `*` / `sonnet` / `fable` / `opus` / `haiku` 角色 + 能力申明解析出该实例应使用的模型，**解析不出就保持原模型名且 `rewrite=false`**（不猜）。 |
| M100 | **抽公共函数**：`lockedCandidate` 与 `fallbackCandidate` 必须走**同一个**「解析实例映射模型」函数，禁止各写一份；该函数签名与语义由实现方定，但两处行为必须一致（见验收 M15cy）。 |
| M101 | **日志**：锁定实例豁免超时后，原本会打印的 `instance <name> timeout: content-idle for <d>; stream terminated` 在锁定实例上**不再打印**；改为在流结束时打一行 `instance <name> locked; stream not interrupted`（每请求最多一行，仅在确实因锁定而豁免时有内容才打）。 |
| M102 | **不 bump 版本**：并入 `[0.1.6]`。 |

### 24.25.3 验收（M15cq–M15cz）

| 编号 | 验收点 |
|---|---|
| M15cq | 锁定实例流式响应只发 `ping`、无内容块，时长超过该实例 `timeout` 3 倍 → 客户端**收到完整上游字节**，无 `pause_turn` 尾巴，连接正常关闭。 |
| M15cr | 同上场景，断言日志**不含** `timeout: content-idle`，含 `locked; stream not interrupted`。 |
| M15cs | 锁定实例返回非 2xx（如 400）→ **不**记失败（`consecutiveFailures` 保持 0，`degradedUntil` 零值），且请求**继续**走其余实例；其余实例成功则客户端拿到 200。 |
| M15ct | 锁定实例**连接层彻底不设超时**：上游接受 TCP 连接后长时间不返回响应头（远超该实例 `timeout`）→ 网关**不主动断开**，客户端连接保持；断言网关日志无 `timeout awaiting response headers`、无 `instance <name> timeout`。（TCP 层直接报错时按 M93 降级到其余实例，不再断言超时相关行为。） |
| M15cu | 锁定时请求**第一步就发往锁定实例**（断言上游收到的第一个请求落在锁定实例）。 |
| M15cv | 锁定实例成功 → 其余实例一次都不被调用。 |
| M15cw | 解锁后：所有实例状态清零、`consecutiveFailures == 0`；锁定期间的失败不追溯。 |
| M15cx | 锁定 + 客户端发 `claude-sonnet-5`，该实例 `sonnet` 角色映射到 `deepseek-flash` → 上游收到的 body 中 `"model"` 为 `deepseek-flash`。 |
| M15cy | `lockedCandidate` 与 `fallbackCandidate` 对同一组 (实例, 请求体) 解析出的模型名**完全相同**（表驱动，覆盖 `*`/`sonnet`/`fable`/`opus`/`haiku` 五种角色 × 有无能力申明）。 |
| M15cz | 未锁定路径完全不受影响：所有既有锁定/熔断/降级测试保持通过，`§24.19`/`§24.20`/`§24.21` 语义不变。 |
| M15da | 锁定实例上游返回非 2xx（如 400）：**不记失败**（`consecutiveFailures` 保持原值，`degradedUntil` 零值），且请求**继续**走其余实例；其余实例成功则客户端拿到 200，其余全失败则透传最后一次真实状态码与响应体。 |
| M15db | 锁定存在 + 非锁定实例已回响应头但**一个内容块都没产出**（只发 `ping`）→ 网关**立即**中断该上游并改发锁定实例，客户端最终拿到锁定实例的完整响应；断言非锁定实例的上游请求被提前关闭。 |
| M15dc | 同上场景，断言**不依赖超时**：把该非锁定实例的 `timeout` 设为很大（如 10 分钟），仍必须立刻中断（测试总耗时远小于该 `timeout`）。 |
| M15dd | 非锁定实例**已产出至少一个内容块**后再静默 → **不中断**，流继续（与 M94c 的「已产出不中断」一致），客户端拿到该实例的完整响应。 |
| M15de | 锁定存在 + 非锁定实例首字节阶段就失败（连接错误/非 2xx）→ 改发锁定实例；客户端只看到锁定实例的响应，无重复响应头、无半截字节。 |
| M15df | 未锁定协议完全不受影响：没有锁定实例时，非锁定实例的流行为与改动前逐字节一致（回归断言）。 |
| M15dg | `ping`/`message_start`/`content_block_start`/空 delta 不算「有 token」：只发这些事件的非锁定实例必须被判为「无内容」并中断（M94a）。 |

---

## §24.26 上游响应缺 thinking 块时的自动补位与「响应头前」总预算（并入 0.1.6）

### §24.26.0 背景（已由实验证实的结论，供实现方参考）

DeepSeek 官方 Anthropic 兼容端点对 `content[].thinking` 的校验规则（实测）：

- **只校验 thinking 块是否存在，完全不校验 `signature` 内容**：`placeholder`、空串、`bogus`、UUID 在块存在时全部 200。
- 触发 400 的**充分条件**：思考模式开启（`adaptive` 被接受）、请求最后一条消息是 `tool_result`、且历史中**任意一条含 `tool_use` 的 assistant 消息没有 thinking 块**。
- 同一 assistant 消息里多个 `tool_use` **只配一个 thinking** 即足够。

Claude Code 客户端行为（本地 mock 实测 2.1.323）：

- **完全忽略 `content_block_start.content_block.signature`，只把 `signature_delta` 写入历史**。
- 无论 signature 是 placeholder / 空 / bogus，**都会保留 thinking 块并原样带回**，不会因格式丢弃。
- **如果上游某轮 tool_use 响应完全没有 thinking 块，客户端会忠实保存并回传 `assistant[tool_use]`（无 thinking）**——这正是污染源。

因此 §24.24 的 signature 归一化对 Claude Code 是无效操作（改的是它不读的字段）。**本条不删除 §24.24**（对其它可能读该字段的客户端仍有意义），但本条的补位才是针对该 400 的正面处置。

Go `net/http` 的 `ResponseHeaderTimeout` 契约（实测 + 源码核对）：

- 计时**从请求 body 完全写完之后才开始**，不覆盖连接获取、HTTP/2 流控/槽位等待、body 上传。
- 16 MiB body + 上游延迟读取时，200ms 的 `ResponseHeaderTimeout` **完全不触发**，`Do` 在 2.03s 后成功返回（httptrace `upload=2.03s`、`server_wait=0s`）。
- 故 `hdr=63–344s` 且 `first_block=0.00s` 的组合，只能是「body 写完前卡了几百秒，上游一拿到完整请求就立刻响应」。

### §24.26.1 决策

| 编号 | 决策 |
|---|---|
| M103 | **上游响应 thinking 补位（A2）**：Anthropic 协议 SSE 流式响应中，若**同一次尝试**的响应里出现了 `tool_use` 内容块但**从未出现任何 `thinking` 块**，在该流的末尾、`message_stop` 之前，向客户端注入一个空 thinking 块（`content_block_start` + `content_block_stop`）。 |
| M104 | 注入的 thinking 块内容：`{"type":"thinking","thinking":"","signature":""}`；`index` 取该流中出现过的最大 `index` 再加 1（避免与既有块冲突）；`content_block_start` 与 `content_block_stop` 成对出现。 |
| M105 | **只在流式且 parse 成功时注入**；非流式 JSON 响应不注入（非流式不产生会话历史，无污染风险）。`parse=false`（不可解码编码）不注入。 |
| M106 | 注入位置：`message_delta` 之前（若存在），`message_stop` 之前；`message_delta` 里的 `usage` 与 `stop_reason` **一个字节都不改**。 |
| M107 | 注入**不得改变已有的任何字节**：只允许在指定位置**插入**新事件，不重排、不改写、不删除既有事件。既有事件必须逐字节原样转发。 |
| M108 | 每个发生注入的尝试打印**一行**日志：`thinking block injected: inst=<实例名> tool_use=<n> index=<i>`。未注入不打印。 |
| M109 | 注入只作用于转发给客户端的字节；`sseState` 的 token/usage/吞吐统计仍只看原始字节（沿用 §24.24 M85 的分层原则）。 |
| M110 | **响应头前总预算**：对每一次上游尝试，从 `attemptStart` 起加一个独立 deadline，到 `client.Do()` 返回（拿到响应头）即取消。该预算 = 该实例的 `timeout`（默认 15s）。**不使用 `http.Client.Timeout`**（会杀 SSE 流）。 |
| M111 | 该预算**覆盖**连接获取、HTTP/2 流控/槽位等待、body 上传全过程；超时后按既有「候选失败」路径处理（`recordFailure`、换下一个候选），错误文本以 `response header budget exceeded` 标识。 |
| M112 | 锁定实例（`lockedClient`，三项超时为 0）**不施加该预算**——§24.25 M95 语义不变，锁定期间完全由客户端控制。 |
| M113 | 注入与预算是**两个独立开关**，互不依赖；任一单独生效都不得改变另一方所在的代码路径行为。 |

### §24.26.2 诊断埋点（用户明确要求：下次异常能直接抓到原因）

| 编号 | 决策 |
|---|---|
| M114 | **`reqShape` 统计段改名更精确**：`tool_use=N paired=M` 改为 `tool_use_msgs=N paired_msgs=M`，并新增 `unpaired_idx=<逗号分隔的消息下标，最多 10 个，超出加 +>`。语义：`tool_use_msgs` = 含至少一个 `tool_use` 块的 assistant 消息条数；`paired_msgs` = 其中同时含 thinking 块（`thinking` 或 `redacted_thinking` 都算）的条数。 |
| M115 | `reqShape` 的采集条件从「仅失败请求」放宽为「失败请求 **或** 请求体含 tool_use 的请求」。目的：可在同一会话内跨轮比较 `paired_msgs` 是否掉数。成功请求的 `reqShape` **不进 `recentErrors`**，只进一条新日志行（见 M116）。 |
| M116 | 每轮请求打印一行**请求形态摘要**（仅当请求体含 `tool_use` 时）：`reqshape msgs=<n> tool_use_msgs=<n> paired_msgs=<n> unpaired_idx=<...> thinking=<declared> last=<消息类型> session=<sha256 前 8 位>`。`session` 取自请求体 `metadata.user_id` 中的 `session_id`（JSON 字符串内嵌），取不到时打 `-`。**绝不打印消息正文。** |
| M117 | **上游响应侧形态摘要**（每个流式尝试一行，与 §24.23 M76 的阶段摘要合并输出或紧随其后）：`resp_shape thinking_blocks=<n> tool_use_blocks=<n> signature_empty=<n> injected=<0|1>`。`signature_empty` 统计 `content_block_start` 中 `signature` 为空串或缺失的 thinking 块数（含 `signature_delta` 全程为空的情况）。 |
| M118 | **响应头前耗时拆解**：新增一行日志 `hdrprof inst=<名> connect=<d> tls=<d> upload=<d> server_wait=<d> body_bytes=<n> reused=<0|1> remote=<addr> budget=<d>`。各阶段由 `httptrace.ClientTrace` 采集：`connect`=`ConnectDone`−开始，`tls`=`TLSHandshakeDone`−`ConnectDone`，`upload`=`WroteRequest`−`WroteHeaders`，`server_wait`=`GotFirstResponseByte`−`WroteRequest`；`reused` 取 `GotConnInfo.Reused`；未到达的阶段打 `-`。**仅当 `hdr` 超过该实例 `timeout` 的一半时才打印**，避免刷屏。 |
| M119 | 上述所有日志**只写 stderr/日志文件**，不进 `/api/stats`、不进 WS 事件、不改变任何转发字节。 |
| M120 | `session` 哈希固定 8 位小写十六进制，全局一致；同一会话的多轮请求必须打到同一个值（用于跨轮关联）。 |

### §24.26.3 验收

| 编号 | 用例 |
|---|---|
| M15dh | 流式响应只有 `tool_use` 没有 `thinking` → 客户端收到的字节**以注入的 thinking 块结尾**（在 `message_stop` 之前），且原响应的其余事件逐字节不变。 |
| M15di | 流式响应既有 `thinking` 又有 `tool_use` → **不注入**，客户端字节与上游**完全一致**。 |
| M15dj | 流式响应只有 `thinking` 没有 `tool_use` → **不注入**。 |
| M15dk | 注入的 thinking 块 `index` 不与任何既有块冲突（取最大 index+1），`content_block_start` 与 `content_block_stop` 成对。 |
| M15dl | 注入不影响 `message_delta.usage`：注入前后客户端收到的 usage 数值一致（`sseState` 统计仍按原始字节）。 |
| M15dm | 非流式 JSON 响应（无 thinking 有 tool_use）→ **不注入**，响应体逐字节不变。 |
| M15dn | `parse=false` 时**不注入**，转发字节逐字节不变。 |
| M15do | 注入日志每个尝试**最多一行**，字段为 `thinking block injected: inst=<名> tool_use=<n> index=<i>`。 |
| M15dp | 小 body 正常请求 → 响应头预算不触发，无额外延迟（回归）。 |
| M15dq | 上游接受 TCP 后**迟迟不读** body（模拟大 body 上传阻塞）→ 在实例 `timeout` 到点后候选失败，错误含 `response header budget exceeded`，客户端不会看到响应头。 |
| M15dr | 同上场景但实例处于**锁定**状态 → **不施加预算**，长时间等待不被网关掐断（§24.25 M95 语义）。 |
| M15ds | 预算触发后照常 `recordFailure` 并按链路换下一个候选；后续候选成功则客户端拿到后续候选的正常响应。 |
| M15dt | `reqShape` 统计段输出 `tool_use_msgs`/`paired_msgs`/`unpaired_idx`，且 `redacted_thinking` 计入 paired。 |
| M15du | 请求含 tool_use 但成功 → 打印 M116 的请求形态摘要行，且**不进** `recentErrors`。 |
| M15dv | 响应含 thinking 块且 `signature` 全程为空 → M117 的 `signature_empty` 计数正确；含 `signature_delta` 非空则不计。 |
| M15dw | `hdr` 超过实例 `timeout` 一半时打印 M118 的 `hdrprof` 行，各阶段数值可解析；`hdr` 正常时不打印。 |
| M15dx | `metadata.user_id` 含 `session_id` 时 M116 的 `session` 为 8 位小写十六进制，同一 session 多轮请求取值一致；`user_id` 缺失或非 JSON 时打 `-`。 |
| M15dy | **回归**：无 tool_use 的普通请求，转发字节与改动前逐字节一致；`/api/stats` 与 WS 事件结构不发生任何变化。 |

### §24.26.4 版本

- 并入 `0.1.6`，**不单独 bump**（用户 m02596「版本不用bump ，继续用0.1.6就行」）。

---

## §24.27 响应头预算触发时补打 `hdrprof`（并入 0.1.6）

### §24.27.0 背景

§24.26 上线后生产实测（2026-11-19，容器 uptime 约 11 小时）：

- `response header budget exceeded` **触发 8 次**（`MTI/deepseek` 5 次、`MTI/aliyun` 3 次），说明上游卡「响应头之前」的问题真实存在，M110 的总预算确实在拦截。
- 但 `hdrprof` 日志 **0 次**。

原因：`internal/proxy/proxy.go` 里 `client.Do(outReq)` 返回 `err` 时，代码在 `if err != nil` 分支里直接 `return`，而 `trace.log(...)`（M118 的唯一输出点）位于该分支之后。**预算一触发就恰好跳过了打印 `hdrprof` 的那行**，导致最需要诊断信息的时刻反而没有信息。

这正是「偶发 200–344s 超长请求」排查中最缺的那一块：无法区分卡在 `upload`（我们发不出去）还是 `server_wait`（上游不搭理）。

### §24.27.1 决策

| 编号 | 决策 |
| --- | --- |
| M121 | **预算触发（`budgetHit == true` 且 `client.Do` 返回错误）时，必须补打一行 `hdrprof`**，字段与 M118 完全一致，不新增字段。 |
| M122 | 补打时的时间基准取**超时触发时刻**（`time.Now()`），使 `hdr` 约为该实例 `timeout`，从而必然通过 M118 的「超过 `timeout` 一半」门槛。 |
| M123 | 除上述入口外，M118 的既有行为不变：正常拿到响应头时仍按 `headersAt` 判定，未超过 `timeout` 一半则不打印。 |
| M124 | 补打只写日志，**不得改变返回值、错误文本、熔断计数、重试链与转发字节**。错误文本 `response header budget exceeded after <d>: <err>` 保持原样。 |
| M125 | 锁定实例（§24.25 M95，`lockedExempt`）没有预算，因此不存在 M121 的补打场景；此处行为不变。 |
| M126 | 并入 `0.1.6`，**不单独 bump 版本**。 |

### §24.27.2 验收

| 编号 | 验收 |
| --- | --- |
| M15dz | 上游接受 TCP 但不返回响应头、且超过实例 `timeout` → 日志同时出现 `response header budget exceeded` 与**一行** `hdrprof inst=<该实例名>`；`hdrprof` 含 `connect=` `tls=` `upload=` `server_wait=` `body_bytes=` `reused=` `remote=` `budget=` 全部字段。 |
| M15ea | 上述场景中 `hdr` 值 ≥ 该实例 `timeout`（即补打不受「超过 `timeout` 一半」门槛影响）。 |
| M15eb | **回归**：响应头快速返回的请求不打印 `hdrprof`（沿用 M15dw 既有断言）。 |
| M15ec | **回归**：预算触发时的错误文本仍为 `response header budget exceeded after <d>: <err>`，候选失败计数 +1，链条继续按 §24.21 换下一个候选；客户端最终状态码与改动前一致。 |
| M15ed | **回归**：锁定实例（`lockedExempt`）路径不产生 `hdrprof`，且不施加预算。 |

## §24.28 连接标识观测（并入 `0.1.6`）

### §24.28.1 背景

生产在 2026-11-19 12:16–12:18 出现三次 `total≈60s status=502`：请求体 1473982 字节（730 条消息、约 42 万 token），四个上游逐个撞满 15s 响应头预算。

`hdrprof` 显示两种截然不同的卡法：

- `MTI/deepseek`、`MTI/aliyun`、`MTI/qwen`：`connect=20~41ms upload=2~24ms server_wait=- reused=0`——请求体已发完，上游不返回响应头。
- `DeepSeek`（官方）：`connect=- tls=- upload=- server_wait=- reused=1`——请求体未写完。

现有字段无法回答一个关键问题：**卡住的请求是否集中在少数几条 TCP 连接上**。`reused=1` 只能说明「复用了连接」，不能说明「是否同一条」。若卡住的全在一条连接、而该连接上其他请求正常，则与连接无关；若某条连接上的请求普遍偏慢，才指向连接层（例如 HTTP/2 单连接多路复用的队头阻塞）。

本条的观测结果用于**决定是否值得改动传输层**，本身不改变任何转发行为。

### §24.28.2 决策

| 编号 | 决策 |
| --- | --- |
| M127 | 观测目标：用连接标识判断同一实例上「响应头慢」的请求是否集中在少数几条连接上。 |
| M128 | `hdrprof` 行新增字段 `conn=<port>`，取值来自 `httptrace.GotConnInfo.Conn.LocalAddr()` 的端口数字；`GotConn` 回调未触发（连接尚未取得）时打 `conn=-`。 |
| M129 | 字段位置固定在 `remote=` 与 `budget=` 之间：`... reused=<0\|1> remote=<addr> conn=<port\|-> budget=<d>`。其余字段的名称、顺序、取值口径一律不变。 |
| M130 | `stream` 阶段摘要行（§24.23 M76）在 `last_gap=` 之后追加 `conn=<port\|->`，取值口径与 M128 相同，取自同一次尝试的连接。 |
| M131 | `conn` **只输出端口数字**，不输出 IP、不输出连接对象的指针或任何可反推进程内存布局的信息。 |
| M132 | 上述两条日志**只写日志**：不得改变转发字节、超时判定、熔断计数、重试链、候选顺序、统计字段，也不得因为取连接信息而增加额外阻塞或加锁范围。 |
| M133 | 不新增配置项，不改 `/api/stats`、WS 事件与 `recentErrors`。 |
| M134 | 并入 `0.1.6`，**不单独 bump 版本**。 |

### §24.28.3 验收

| 编号 | 验收 |
| --- | --- |
| M15ee | 正常情况下，`hdrprof` 行同时包含 `remote=<host:port>` 与 `conn=<数字端口>`，且两者属于同一条连接。 |
| M15ef | 连接尚未取得（`GotConn` 未触发）时，`conn` 打印为 `-`，该行其余字段格式不变。 |
| M15eg | `stream` 行在 `last_gap=` 之后带 `conn=<端口\|->`；其余字段名称与顺序不变。 |
| M15eh | 同一连接上连续发起的多个请求，日志中的 `conn` 值相同；不同连接的值不同（含端口复用后重新分配的情况）。 |
| M15ei | **回归**：新增字段不改变客户端收到的字节、状态码、超时、熔断计数与 `/api/stats` 结构。 |

## §24.29 监控页强行取消活跃请求（并入 `0.1.6`）

### §24.29.1 背景

生产出现过「上游卡在响应头之前、请求挂十几分钟不返回」的现场（§24.26/§24.27 记录）。排查期间只能在服务器上重启容器或等客户端自己断开，运维侧缺少一个「针对单个请求」的止损手段。

本条在监控页的活跃请求行尾加一个 `×`，点一下让**该请求**立刻失败。**取消的是请求，不是 TCP 连接**：HTTP/2 下多条请求共享一条连接，掐连接会连带杀掉同一连接上的其他请求，因此实现必须走请求级取消（取消该请求的 context），只影响目标行。

### §24.29.2 决策

| 编号 | 决策 |
| --- | --- |
| M135 | 取消通道复用现有 `/ws` WebSocket，方向为浏览器 → 服务端，载荷为 UTF-8 **文本帧**，内容 `{"type":"cancel","id":"<请求id>"}`。 |
| M136 | `id` 取自该请求在快照与 `start` 事件里已有的 `id` 字段，格式不变；服务端按 `id` 精确定位，未知 `id` 直接忽略（不报错、不断开连接、不回包）。 |
| M137 | 服务端为每个请求保留一个取消入口：请求进入候选链之前派生可取消 context，取消入口随该请求的生命周期注销。取消能力只对**尚未结束**的请求存在。 |
| M138 | 取消作用于该请求所有在途进度：正在等待的上游响应头、正在传输的响应体一律中断；**不区分当前处于哪个候选**。 |
| M139 | 取消后**不再尝试后续候选**（不再按 §24.21 降级、不做补漏），该请求直接终止。 |
| M140 | 响应头尚未提交时，向客户端返回状态码 **502**，正文按该协议的既有错误格式（Anthropic / OpenAI 各自格式），错误文本固定为 `request cancelled by operator`。 |
| M141 | 响应头已提交（流式已开始写）时无法改状态码，直接关闭客户端连接，行为与现有 `truncateClientConnection` 一致；不追加 `pause_turn` 尾巴。 |
| M142 | 取消**不计入实例失败**（不计熔断计数、不改变实例状态、不影响 `degradedUntil`），因为是运维动作而非上游故障。 |
| M143 | 取消计入该请求的结束统计，`status` 为该请求实际写出的状态码（M140 的 502 或 M141 时客户端连接已断），`error` 字段记录 `request cancelled by operator`。 |
| M144 | 每次取消打印一行日志：`request <id> cancelled by operator inst=<实例名\|-> phase=<pre-header\|streaming>`。 |
| M145 | 前端：活跃请求表格在**最右**新增一列（时间线列之后），列宽固定且不挤压时间线；仅「进行中」的行在该列显示 `×` 按钮，已结束的行该列留空。 |
| M146 | 前端点击 `×` **不做二次确认**；点击后该按钮立即禁用并变为「已取消」态，避免重复发送。该行随后按既有 `end` 事件流程结束。 |
| M147 | 只有已通过管理面鉴权的连接能发取消命令（`/ws` 本身就在鉴权之后），不新增鉴权入口、不新增 HTTP 路由。 |
| M148 | 不新增配置项，不改 `/api/stats` 字段与结构，不改现有 WS 事件格式（`start` / `end` / `snapshot` / `stats` 字段一律不变）。 |
| M149 | 并入 `0.1.6`，**不单独 bump 版本**。 |

### §24.29.3 验收

| 编号 | 验收 |
| --- | --- |
| M15ej | 活跃请求行尾出现 `×` 按钮；已结束的行不出现按钮。 |
| M15ek | 点击 `×` 后，该请求在 `end` 事件前就被终止，客户端收到 502 且错误文本为 `request cancelled by operator`（响应头尚未提交时）。 |
| M15el | 流式响应已提交后取消 → 客户端连接被关闭，不追加 `pause_turn`，该请求以失败结束。 |
| M15em | 取消后不再发起任何新候选：日志中不出现该请求后续的 `candidate ... failed` 或补漏尝试。 |
| M15en | 取消的请求**不增加**目标实例的失败计数，实例 `state` 与 `degradedUntil` 不变。 |
| M15eo | 同一连接上的其他活跃请求不受影响（各自正常结束或继续传输）。 |
| M15ep | 发送未知 `id` 的取消命令 → 服务端忽略，不产生错误响应，不中断任何请求，WS 连接保持可用。 |
| M15eq | 页面刷新后用快照恢复的「进行中」行，其 `×` 同样可用（`id` 与快照一致）。 |
| M15er | 日志出现 `request <id> cancelled by operator inst=<名\|-> phase=<pre-header\|streaming>`，且每个请求最多一条。 |
| M15es | **回归**：不点 `×` 时，`/api/stats`、WS 事件、转发字节、超时与熔断行为与改动前完全一致。 |

## §24.30 MiniMax 国内供应商预设（并入 0.1.7）

### §24.30.1 背景

用户 m04668 原话：「供应商增加MiniMax 国内的支持，可参考www.minimax.cn的官网，支持openai/anthropic的」。

现状：`internal/providers/providers.go` 内置 11 个纯数据预设，无 MiniMax。用户手工添加 MiniMax 实例需要自己填 base_url（`https://api.minimax.cn/anthropic` 或 `https://api.minimax.cn/v1`）。

实测结论（2026-11-20，本机直连 `api.minimax.cn`）：
- `GET https://api.minimax.cn/anthropic/v1/models` → 200，返回 Anthropic 格式模型列表（`MiniMax-M3.1-Flash` / `MiniMax-M3` / `MiniMax-M2.7` …）。
- `GET https://api.minimax.cn/v1/models` → 200，返回 OpenAI 格式模型列表（`owned_by: "minimax"`）。
- 因此 MiniMax **不需要** `models_override`（与 DeepSeek 不同：DeepSeek 的 anthropic 入口没有 `/v1/models`，才需要覆盖到 OpenAI 端点）。
- 官方文档（`platform.minimax.cn/docs/guides/text-generation`）给出的两个 base_url：Anthropic 兼容 `https://api.minimax.cn/anthropic`（官方推荐）、OpenAI 兼容 `https://api.minimax.cn/v1`。国际站为 `api.minimax.io` / `www.minimaxi.com`，本期不内置。

### §24.30.2 决策

| 编号 | 决策 |
| --- | --- |
| M150 | `internal/providers` 的 `presets` 新增一条：`ID: "minimax"`、`Name: "MiniMax"`、`OpenAIBaseURL: "https://api.minimax.cn/v1"`、`AnthropicBaseURL: "https://api.minimax.cn/anthropic"`。`ModelsOverride` 留空（两个协议都自带模型列表端点）。 |
| M151 | 预设编号只允许小写字母、数字与连字符；`minimax` 不得与既有 11 个 id 冲突（`GET /api/providers` 返回 12 条）。国际站不内置，按自定义实例处理（`provider` 留空）。 |
| M152 | 并入 `0.1.7`，不额外 bump 版本。 |

### §24.30.3 验收

| 编号 | 验收 |
| --- | --- |
| M15et | `GET /api/providers` 返回 12 条预设，其中一条 `id == "minimax"`，其 `openai_base_url == "https://api.minimax.cn/v1"`、`anthropic_base_url == "https://api.minimax.cn/anthropic"`，且无 `models_override`。 |
| M15eu | 在「添加提供商」界面选中 MiniMax 后，OpenAI 页签自动填入 `https://api.minimax.cn/v1`、Anthropic 页签自动填入 `https://api.minimax.cn/anthropic`（沿用既有「选择供应商即自动填 base_url」行为，不新增交互）。 |
| M15ev | **回归**：`GET /api/providers` 既有 11 条预设的 id、base_url、`models_override` 一字未改；用既有预设新建实例的行为与改动前完全一致。 |

## §24.31 余额与配额查询、余额列与配额耗尽自动禁用（并入 0.1.7）

### §24.31.1 背景

用户 m04668 原话：「另外支持MPlan订阅配额的查询，并在实例的状态前增加一栏余额显示 5h/7d的剩余配额，配额用完后，节点自动设置为不可用，并自动跳过，直到查询到有配额再恢复；同样的deepseek的实例也能查询余额，用完后节点自动设置为不可用；支持余额查询的实例的余额列点击可以刷新余额显示，同样也会触发节点状态的切换；」

接口均已实测可用（2026-11-20）：

- **MiniMax M Plan 额度**：`GET https://api.minimax.cn/v1/api/openplatform/coding_plan/remains`，`Authorization: Bearer <订阅 Key>` → 200。响应核心字段：`model_remains[]` 中 `model_name == "general"` 的一项，含 `current_interval_remaining_percent`（**5 小时窗口剩余百分比**，实测 90）、`current_weekly_remaining_percent`（**7 天窗口剩余百分比**，实测 97）、`current_interval_status` / `current_weekly_status`（1 = 正常）、`remains_time` / `weekly_remains_time`（毫秒）、`start_time` / `end_time` / `weekly_start_time` / `weekly_end_time`。**注意 `current_interval_usage_count` 实为「剩余量」不是「已用量」**（已用量 = `current_interval_total_count - current_interval_usage_count`），因此判定必须用 percent 字段，不能用 count 字段。
- **DeepSeek 余额**：`GET https://api.deepseek.com/user/balance`，`Authorization: Bearer <API Key>` → 200。响应：`is_available`（布尔）+ `balance_infos[]`（每项含 `currency`（CNY/USD）、`total_balance`、`granted_balance`、`topped_up_balance`）。实测 `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"51.01",...}]}`。

M Plan 额度机制（官方文档 `platform.minimax.cn/docs/m-plan/usage-rules`）：**5 小时窗口**（图片/视频模型除外，首次使用起算 5 小时）与**周窗口**（7 天，所有模型）**各自独立刷新**；非视频模型需要**两个窗口都有剩余额度**才能使用；未用完的额度不累积、不结转。

### §24.31.2 决策（配置与查询）

| 编号 | 决策 |
| --- | --- |
| M153 | `config.Instance` 新增**可选**字段 `balance`（对象）。缺省整个对象 = 该实例不参与余额检查，行为与改动前完全一致。对象字段：`kind`（必填，只接受 `"minimax_plan"` 与 `"deepseek"`，其它值一律报配置错误）、`url`（可选，覆盖默认端点）、`interval`（可选，轮询周期，默认 `5m`，`0` 表示关闭周期轮询、只保留手动刷新）。未知字段报错（沿用既有 `DisallowUnknownFields` 严格风格）。**不按 provider 自动推断**——用户可能用按量计费 Key（不是订阅 Key），M Plan 接口不适用，必须显式声明。 |
| M154 | 默认端点：`kind == "minimax_plan"` → `https://api.minimax.cn/v1/api/openplatform/coding_plan/remains`；`kind == "deepseek"` → `https://api.deepseek.com/user/balance`。认证统一 `Authorization: Bearer <实例的 api_key>`。查询超时固定 **10s**（与实例 `timeout` 无关，余额查询是轻量请求，不参与熔断计数、不触发 `recordFailure`/`recordSuccess`）。 |
| M155 | 查询结果在内部统一为 `balanceInfo` 结构：`kind`、`interval5hPercent`（5 小时窗口剩余百分比，-1 = 未知）、`weekly7dPercent`（7 天窗口剩余百分比，-1 = 未知）、`amount`（金额字符串，如 `"51.01"`，minimax_plan 为空串）、`currency`（如 `"CNY"`）、`exhausted`（是否耗尽）、`checkedAt`（本次查询时刻）、`err`（查询失败原因，成功时为空串）。多币种时取 `balance_infos` 中 `currency == "CNY"` 的一项；没有 CNY 则取数组第一项，并在 `currency` 中如实写明实际币种。 |
| M156 | **耗尽判定**：`minimax_plan` → `interval5hPercent <= 0` **或** `weekly7dPercent <= 0` 即耗尽（官方规则：非视频模型需两个窗口都有余额，故任一为 0 即不可用）；`deepseek` → `is_available == false` **或** `total_balance <= 0` 即耗尽。字段缺失导致无法判定时按「未耗尽」处理并在 `err` 留说明，不得因解析不确定就禁用节点。 |
| M157 | **周期轮询**：Server 启动后为每个声明了 `balance` 的实例按其 `interval` 周期查询（默认 5m，`interval == 0` 不轮询）。**启动后立即执行一次**首查，避免上线后数分钟内状态未知。轮询在后台 goroutine 中进行，查询失败不影响服务、不阻塞其它实例。 |
| M158 | **查询失败不改变状态**：网络错误、非 2xx、JSON 解析失败一律**不改变**该实例的 `exhausted` 状态（不能因为查不到就禁用节点），保留上一次成功的余额值与时间戳，在 `err` 中记原因，并打印一行日志 `balance check failed: inst=<名> err=<原因>`。 |
| M159 | **手动刷新接口**：新增 `POST /api/instances/balance`，请求体 `{"protocol":"<openai|anthropic>","name":"<实例名>"}`，`name` 可省略（表示该协议下全部声明了 balance 的实例）。鉴权与 `/api/instances/lock`、`/api/instances/unlock` 完全一致（Basic `admin:<admin_password>` 或管理面会话 Cookie）。响应 `{"instances":[{"name","protocol","kind","state","balance":{...}}]}`。协议非法 → 400；`name` 指定的实例不存在或未声明 balance → 404。**接口同步执行查询并返回最新结果**，不等待周期轮询。 |

### §24.31.3 决策（状态机与路由）

| 编号 | 决策 |
| --- | --- |
| M160 | 实例状态新增第 5 个取值 `exhausted`（配额/余额耗尽），与既有 `closed` / `open` / `locked` / `-` 并列。触发：一次余额查询判定耗尽即置为 `exhausted`（记录 `degradedSince` 与 `reason == "quota_exhausted"`）；恢复：查询判定有余额即清除 `exhausted` 回到 `closed`。**`exhausted` 与熔断计数完全独立**：余额查询不调用 `recordFailure`/`recordSuccess`；余额恢复**不**强行清除熔断的 `open` 状态（熔断仍按 `degrade_duration` 到期自动回 closed）。 |
| M161 | **路由跳过**：候选链构建时 `exhausted` 实例与 `open` 实例一样不进入 `attempts`。区别：`exhausted` 实例**不参与**「全部候选都不可用时整条链都试一遍」的兜底（该兜底只服务于熔断的 `open`），因为配额耗尽时上游必然快速失败，试它没有意义。 |
| M162 | 若过滤掉 `exhausted` 后 `attempts` 为空 → 向客户端返回 **502**，错误文本固定 `all instance quotas exhausted`（并在正文中列出耗尽的实例名），同时写入 `recentErrors`（`inst` 字段为耗尽实例名之一、`error` 为上述文本）。 |
| M163 | **锁定优先**：实例同时被锁定且配额耗尽时，`locked` 优先（运维显式锁定仍生效，该实例照旧排第一）。余额列仍如实显示耗尽状态，鼠标悬停给出提示。这是既有的「锁定 = 运维强制」语义的延续。 |
| M164 | 日志两行：状态切换时 `instance <名> quota exhausted` / `instance <名> quota restored`；余额值发生变化时 `balance changed: inst=<名> 5h=<p>% 7d=<p>% amount=<金额> exhausted=<bool>`（未变化不打印）。查询失败按 M158 打印。 |

### §24.31.4 决策（统计与前端）

| 编号 | 决策 |
| --- | --- |
| M165 | `/api/stats` 的 `instances[]` 每项新增**可选**字段 `balance`（`omitempty`）：不支持余额查询的实例不带该字段，字段结构 = M155 的 `balanceInfo`（`checkedAt` 用毫秒时间戳，与 `degradedSince` 同口径）。`state` 字段新增取值 `exhausted`，`reason` 新增取值 `quota_exhausted`。既有字段一个不改。 |
| M166 | **余额信息不落盘**：余额值与 `exhausted` 状态属「当下型」状态，**不写入 `stats.json`**，重启后由首查重建（与「降级状态不持久化」同理——带着陈旧的耗尽标记启动会导致节点被误跳过）。 |
| M167 | **前端余额列**：OpenAI 与 Anthropic 两张实例表都在**「状态」列之前**新增一列「余额」（表头 + `colgroup` + `statCell` 三处同步），列宽约 10%。展示规则：`minimax_plan` → `5h 90% · 7d 97%`；`deepseek` → `51.01 CNY`；不支持余额查询 → `-`；最近一次查询失败 → 保留上次成功的展示值并在单元格 `title` 中追加错误原因。 |
| M168 | **点击刷新**：余额单元格可点击（`cursor: pointer`，`title` 含「点击刷新余额」）。点击 → `POST /api/instances/balance`（带该行 protocol 与 name）→ 用响应就地更新该行的余额文本与状态列文本。请求进行中禁用重复点击（显示「刷新中…」），完成后恢复。刷新同样会触发 M160 的状态切换（查到有额度则立刻恢复 `exhausted`）。 |
| M169 | 状态列新增 `exhausted` 的展示：文案「配额耗尽」，配色与熔断的 `open`（红）**不同**（用橙/黄系），`title` 写明原因（`quota_exhausted` 或上游返回的耗尽原因）。 |
| M170 | **不新增 WS 事件**：既有 `stats` 帧已经携带 `instances[]`，余额与 `exhausted` 状态随该帧自动下发，不新增事件类型、不改既有字段结构。 |
| M171 | 本期**只支持在 JSON 配置文件里写 `balance` 字段**；可视化配置页（新增实例表单 / 实例编辑）暂不提供 balance 的可视化编辑项（记为已知限制，见 §24.31.6）。 |
| M172 | 并入 `0.1.7`，不额外 bump 版本。 |

### §24.31.5 验收

| 编号 | 验收 |
| --- | --- |
| M15fw | `config.example.json` 与 README 给出 `balance` 字段的完整示例（两种 kind）；配置里写了未知 `kind` 或未知子字段时启动报错并指明字段名。 |
| M15fx | 未声明 `balance` 的实例：`/api/stats` 的该行**不带** `balance` 字段，余额列显示 `-`，路由行为与改动前完全一致。 |
| M15fy | 声明 `kind: "minimax_plan"` 的实例：启动后立即完成一次查询，`/api/stats` 的 `balance` 含 `interval5hPercent` / `weekly7dPercent`（取自 `current_interval_remaining_percent` / `current_weekly_remaining_percent`），余额列显示 `5h <p>% · 7d <p>%`。 |
| M15fz | 声明 `kind: "deepseek"` 的实例：`balance.amount` / `balance.currency` 取自 `balance_infos`（多币种时取 CNY，无 CNY 取第一项并如实写明币种），余额列显示 `<金额> <币种>`。 |
| M15ga | 查询判定耗尽 → 该实例 `state` 变为 `exhausted`、`reason` 为 `quota_exhausted`，路由不再选它（发请求时不再出现该实例的转发日志）。 |
| M15gb | 查询判定有余额 → `state` 回到 `closed`，日志出现 `instance <名> quota restored`。 |
| M15gc | 全部候选都 `exhausted` → 客户端收到 502 且错误文本为 `all instance quotas exhausted`（含耗尽实例名），`recentErrors` 有对应记录。 |
| M15gd | 查询失败（网络错误 / 非 2xx / 非法 JSON）→ 该实例**状态不变**（仍为耗尽或正常），余额值保留上一次成功结果，`balance.err` 有原因，日志有 `balance check failed`。 |
| M15ge | `POST /api/instances/balance` 带合法 protocol+name → 200 且返回该实例**刚查询**的余额与状态；状态随之切换（耗尽↔可用）。 |
| M15gf | `POST /api/instances/balance` 带非法 protocol → 400；带不存在的 name 或未声明 balance 的实例 → 404；未通过管理面鉴权 → 401/403（与 lock/unlock 同一套鉴权）。 |
| M15gg | 前端：两张实例表都有「余额」列且排在「状态」列之前；点击余额单元格触发刷新并就地更新余额与状态文本；刷新中按钮/单元格禁用，不可重复点击。 |
| M15gh | 余额列展示值与 `/api/stats` 的 `balance` 字段一致；统计页与监控页的其它列、格式（K/M、ms/s）不受影响。 |
| M15gi | **回归**：未声明 `balance` 的实例，其 `state` 取值集合、路由顺序、熔断行为、`/api/stats` 既有字段与改动前完全一致；`stats.json` 里**不出现**任何余额字段（余额不落盘）。 |

### §24.31.6 已知限制

- 可视化配置页暂不支持编辑 `balance` 字段，只能编辑 JSON（§24.31.4 M171）。
- 只内置 MiniMax 国内站与国际站 DeepSeek 两种检查方式；其它供应商的余额/配额查询需自行扩展 `kind`。
- `exhausted` 节点不做主动恢复探测：只能等下一次余额查询（周期轮询或手动点击刷新）判定有余额后恢复。
- M Plan 官方说明「额度充足仍可能因速率限制被限流」，因此余额充足不代表调用一定成功；本功能只覆盖配额/余额维度。
