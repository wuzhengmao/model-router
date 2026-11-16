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
