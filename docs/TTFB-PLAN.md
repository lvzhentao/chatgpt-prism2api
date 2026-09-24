# 首字与高并发容量计划（多账号目标）

> 目标形态：**账号数十~数百个，外部高并发涌入**。首字慢只是表象，真正的设计目标是：
> ① 首字（首帧/首内容帧）快且与并发数解耦；② 预热成本与账号总数 N 解耦；
> ③ 慢 PG / 慢上游不把全池拖下水；④ 过载时对客户端是干净的 429+Retry-After 而不是挂死。
>
> v3 修订：预热从「一个任务项」升级为常驻子系统（warm-set 限容 + 触发点 + 退避 + 抖动）；
> 落盘异步化随 `p.mu` 全池锁证据上调优先级；新增容量模型与压测方案。
> 不碰模型效果、计费语义、failclass 冷却语义。

## 0. 容量模型（先算清，再动手）

- 单请求占账号槽位 ≈ 整轮耗时（热 ~6-10s）。**池吞吐上限 = 热账号数 × 单号并发上限 × 60 / 整轮秒**。
  例：K=12 热账号 × C=4 并发 × 60 / 8s ≈ 360 RPM。
- 账号数 N 不直接提供吞吐，**只有热账号提供吞吐**。冷账号首请求付冷链（~5-12s），高并发下等于不可用。
- 预热成本：每次刷新 = 5 RTT + ≤4 次 wait-for-sync（服务端 hold，各 ≤10s）。
  **naive 全量预热成本 = N × 9 / TTL**（N=100、TTL=3m → ~5 req/s 后台流量，会惊动上游/Cloudflare）。
  → 必须限容：**只保 K 个热账号（warm-set），成本 = K × 9 / (TTL/2)，与 N 解耦**（K=16、TTL=3m → ≤0.8 req/s）。
- K  sizing：K ≥ 目标并发 / C + 25% headroom。目标 50 并发、C=4 → K ≥ 16。
- 并发上限现状：`account_concurrency` 默认 0 不限（`admin/config.go:50`），429 后单向降级（`account.go:304-308`）。
  同沙箱并发上限是上游开放问题（`PRISM_API.md` §开放问题 1）→ C 建议先设 4，T5.3 实测修正。
- 过载出口已有：全池满/冷却时 `scheduler.UnavailableError` → `writePickError` 带 `Retry-After`（`pick.go:138`，`ops.go:151`）。保持，不新建队列。

## 1. 单请求时间线（代码证据）

`handleChat`（`internal/api/server.go:426`）→ `streamChat`（`:491`，`:510` `WriteHeader(200)` 但 Go 不 flush；role chunk `:516` 等 `sentAny` 才发，`:551-556`）→ `prism.Stream`（`internal/adapter/prism/stream.go:302`）：

| 阶段 | 热 / 冷开销 | 代码位置 |
|---|---|---|
| 选号 `iter.Next()` | **O(N) 逐账号 `Tokens.Token()` 解 JWT**（base64+JSON+锁）；N=100 时每请求 100 次解码 | `internal/api/pick.go:63`，`internal/pool/account.go:378`，`internal/auth/login.go:231` |
| `ensureSession` | 热 0 RTT；冷 1 RTT | `internal/adapter/prism/client.go:372` |
| `ensureProject` | 热 0 RTT；冷 1~2 RTT | `client.go:421` |
| `ensureSandbox` | 热 0 RTT；冷 5 RTT + ≤4 次 wait-for-sync（各 ≤10s 服务端 hold） | `client.go:494/502/582`，`endpoints.go:32`，`syncPollTries=4` |
| `registerConversation` | 每次 1 个阻塞 RTT | `stream.go:351`，`client.go:628` |
| `start` | 提交快 | `client.go:661`，`startTimeout=75s` |
| `pollTurn` | 首次立即；之后睡 1.5s→+0.3s→3s 封顶 | `client.go:870-928` |
| 一次性 `emit(Text)` | 全文到达才发首内容帧 | `stream.go:337` |
| 完成态落盘 | `MarkSuccess→save()` 持 **`p.mu` 全池锁** 同步 PG 写 | `account.go:475`，`pool.go:517-533` |

上游无 SSE，pending 无部分正文（HAR 已核实）→ 首内容帧 = 整轮耗时，网关只能优化首帧、零头、冷链、完成检测。

实测：`har/抓包2.har` status 被服务端 hold 730/3517/867/3474ms（长轮询特征）；`PRISM_API.md §7` 冷启动 11.6s、预热后 2.5~2.7s、整轮 9~10s、沙箱闲置约 7 分钟失效。

## 2. 高并发下的放大点（按影响排）

0. **首帧懒发送（横跨四协议）**：首字节等整轮 9~10s，客户端/代理 first-byte-timeout（常见 10s）直接断。并发越高断得越多。
1. **完成态 save 持全池锁同步 PG 写**：完成速率 = 并发×60/整轮秒，50 并发时 ~6 次/s 的「`p.mu` + 同步 SaveDoc（8s 超时）」。一次慢 PG 写，全池 pick 排队 → 所有在飞请求首字被拖。**高并发下这是首要瓶颈**。
2. **Transport 切碎 ×N**：`resin.go:211` 零值每 host 2 空闲连接；`ApplyEgress` 每账号新建 Transport（`account.go:1108`）。N=100 → 100 个碎池；并发 >2/账号 反复 TLS 握手过 Cloudflare。
3. **pick O(N) JWT 解码**：每请求 × 每账号一次（§1）。N=100、50 req/s → 5000 次/s 解码，真实 CPU。
4. **无预热 → 冷账号级联**：请求落冷账号付 9 步冷链；`prepareSandbox` 失败还会换号重试全链（`streamChat` 重试逻辑），高并发突发等于全池冷链风暴。
5. **预热成本若 naive 则随 N 线性**：见 §0。必须 warm-set 限容。
6. **过期惊群**：`Refresh`（`login.go:250`）、`ensureSession`、`prepareSandbox` 无 singleflight；keepalive 路径已有 per-account flight 去重（`auth/keepalive.go:161-168`），请求路径没有。
7. **单沙箱槽位 + 3m TTL**：`client.go:92-111`。同账号并发请求共用同一 sandbox token 同时 `start`，上游并发上限未知 → Phase 0 观测、Phase 5 决定是否沙箱池化。
8. **每请求 CPU**：`ExtractSessionIDs` 最多 7 次全量 `json.Unmarshal`（`scheduler/session.go:19-68`），大历史（120k）×高并发 = 可观 CPU。
9. **固定小开销**：register 阻塞 1 RTT；轮询 1.5s 起步；三入口缺 `X-Accel-Buffering`；远端附件串行代拉（`chat_files.go:99-134`）。

## 3. 目标 / 非目标

目标（外部可观测）：

- **首帧时间**（`curl -w time_starttransfer`）：热/冷都降到 ms 级。
- **首内容帧时间**（§6 探针）：热账号整轮 -0.5~1.5s；冷账号 P95 回落到热水平；**50 并发突发下 P95 劣化 ≤ 单请求的 1.5 倍**。
- 预热后台流量 ≤ K×9/(TTL/2)，与 N 无关；预热不打冷却、不计 failclass、不动 RPM/用量计数。
- 过载时客户端收到干净 429 + `Retry-After`，无挂死连接。
- 网关自身 CPU/锁开销占首字 <50ms（P99）。

非目标：

- 不伪造增量内容。早发 role chunk / message_start 不属于伪造（官方流首帧本就是无内容角色帧）。
- 不改 failclass 冷却语义、不改调度亲和语义（性能等价改写）。warm-set 只影响「谁被预热」，不影响「谁被选中」。
- 不提前建沙箱池/不放宽 TTL，先看数（Phase 5）。

成功指标：Phase 0 先存档基线（§8）；上线后日志可见冷链占比 ~0、`register=0ms`、`conn_reused`>90%、`sandbox_hit` 率、逐次 `poll_hold_ms` 分布；`rpmprobe.py` + `smoke.py` 全绿。

## 4. 分阶段实施

### Phase 0 — 分段计时 + 基线（无行为变化，独立上线）

- T0.1 `Stream` 阶段计时（`credential/session/project/sandbox/register/start/pollN/completed`），含 `sandbox_hit/session_hit/project_hit`；**poll 逐次打 `hold_ms`**（Phase 5 决策依据）。
- T0.2 `streamChat` 记 `t_pick`；`pick.go:63` 打 `sched_pick`；**`start` 日志加 `inflight=N`**（该账号当时在飞数，T5.3 同沙箱并发相关性分析用）。
- T0.3 `request` 包 `httptrace.ClientTrace`，日志带 `conn_reused bool`（顺手解决 T1.1 验收手段）。
- T0.4 **基线存档**：热/冷各 ≥50 次，首帧 + 首内容帧 P50/P95，回填 §8。无基线不进 Phase 1。
- T0.5 保留 `inputShape` 日志。

### Phase 1 — 首帧与传输层快赢（小改、独立可回滚）

- T1.0 **首帧提前（四协议）**：OpenAI role chunk（`server.go:516`）在 `WriteHeader` 后立即发 + Flush，重试守卫从「发过任何帧」改为「发过**内容**帧才禁换号」（role chunk 与账号无关，换号对客户端不可见；全耗尽的 error-as-content 路径 `:529-536` 已存在）。Anthropic `message_start`（`anthropic_handler.go:509`）同样提前；Gemini/Responses 对齐。断言「首帧必含内容」的测试属钉实现，允许适配。
  验收：首帧 <200ms；首内容帧不变（对基线）；10s first-byte-timeout 故障消失。
- T1.1 **Transport 调优 + 同出口共享**：`MaxIdleConns=100`、`MaxIdleConnsPerHost=20~50`、`IdleConnTimeout=90s`、`TLSHandshakeTimeout=10s`、`ResponseHeaderTimeout=30s`；同出口共享底层 Transport（包级 `sync.Map`），Resin `Account` 头保持 per-request（包外层）。**N 账号时这是 fd 与 TLS 握手的硬约束**。
  验收：10 并发 `conn_reused`>90%；`resin_test.go` 绿 + 共享复用单测。
- T1.2 **register 异步（热）+ 与 prepareSandbox 并行（冷）**：热 fire-and-forget（5s 超时 `context.WithoutCancel`）；冷路径 register 不依赖沙箱，与冷链并行起。丢 401/403 提前发现，由 `ensureSession` 覆盖，接受。
  验收：`register=0ms`；register hang 不拖 start 的单测。
- T1.3 **轮询起步调陡（保守版）**：sleep `0.4→0.8→1.2→…→2s 封顶`。终局策略 Phase 5 用 `hold_ms` 分布定。
- T1.4 **`X-Accel-Buffering: no` 补齐**（`server.go:507`、`gemini.go`、`responses.go`）。
- T1.5 **远端附件并行拉取**：worker≤4，单文件 20s 与 `maxAttachments=16` 不变，失败丢该附件。

### Phase 2 — 预热子系统（核心：常驻、限容、自愈）

新增 `internal/adapter/prism/prewarm.go` + `internal/api/prewarm_boot.go`，配置入 `admin/config.go` / `schema.go`。

- T2.1 **`Prewarm(ctx) error`**：包内组装 `ensureSession→ensureProject→ensureSandbox`，不暴露内部字段。`Pool` 侧用类型断言取 `Prewarmer` 小接口。
- T2.2 **singleflight 统一请求与预热**（前提，先做）：`golang.org/x/sync` 转直接依赖；`ensureSession/prepareSandbox/TokenManager.Refresh` 各包内 `singleflight.Group`，key=`账号ID+kind`。**请求路径与预热共享同一 flight**：请求到达时预热在飞 → 直接等结果，不重复冷链。验收：「10 goroutine 同时过期只出 1 次网络」×3 处。
- T2.3 **warm-set 调度器**：
  - 集合：Enabled 账号按 `lastUsedAt` MRU 取前 K（K=`prewarm_accounts`，默认 16，0=关）。冷却/停用中跳过。
  - ticker 90s（`prewarm_interval_sec`）巡检：warm-set 内 `sandboxAt` 超过 TTL/2 才重预热（TTL=3m → 每账号实际刷新 ~3-4.5min 一次）。
  - **抖动**：每账号刷新时刻加 `hash(accountID)%30s` 偏移，防 N 账号同龄对齐突发。
  - workers=10（对齐 `auth/keepalive.go:14`），启动 stagger。
- T2.4 **触发点（不只 boot+ticker）**：boot；`ApplyAdminPatch(Enabled:true)`（`account.go:877`）；登录完成置 Enabled 的调用点；`RememberUsage` 配额恢复（`:786`）；**请求落到冷账号**（该请求仍付冷链，但 MRU 提升使其进 warm-set，自愈）。
- T2.5 **失败退避**：预热失败指数退避 1m→2m→5m 封顶，成功复位；只记日志，不打冷却、不计 failclass、不动 RPM/用量（`noteUpstream` 只属真实请求）。防抖动：账号反复启停不重复预热（per-account 1min 冷却）。
- T2.6 **观测**：`Account.Snapshot`（`account.go:654-736`）加 `sandbox_warm bool`/`sandbox_age_sec`；admin 列表显示热账号数 `warm/total`；日志已有 `sandbox_hit` 率。
- 验收：重启后 1min 内 warm-set 全 `sandbox_hit=true`；3min 闲置仍热；运行时新启用账号 1min 内转热；N=100 时后台预热流量 ≤0.8 req/s（日志计数验证）；压测中冷账号被请求命中后下一tick进 warm-set。

### Phase 3 — 锁与落盘（高并发硬瓶颈）

- T3.1 **账号 save 合并节流**（`pool.go:517-533`，`account.go:475`）：脏标记 + 1s 合并写 + 退出刷盘，显式 `Flush()` 给测试。**把同步 SaveDoc 移出 `p.mu`**。**风险修正：崩溃丢的不止 ≤1s 计数——同 doc 含 `cooldownUntil/failClass` 冷却状态**，重启后本该冷却的账号可能立刻被重用再失败一次。接受（冷却内存态优先），注释写明。`TestPersistSchedulingFieldsAndCooldown` 等用例改调 `Flush()`，不删断言。
- T3.2 **`RecordUsage` 内存累计 + 周期落盘**（`store.go:382-397`）：对齐 `VerifyAndTouch` 的 `TotalCalls++` 不落盘策略，5s 或 N 条合并写。
- T3.3 **prepareSandbox 失败就地退避，不级联换号**：冷链失败在 `Stream` 内指数退避重试 ≤2 次（1s/2s），不抛给 `streamChat` 换号（换号对冷链零收益，只放大全池预热）。验收：「wait-for-sync 两次 syncing 后 synced」不换号成功。

### Phase 4 — 热路径 CPU（等价改写）

- T4.1 **`ExtractSessionIDs` 一次解析**（`session.go`）：小 struct 一次解，`TestExtractSessionIDs` 原样过 + 新旧等价单测。
- T4.2 **JWT `exp` 缓存**（`login.go:231`）：token 串不变不重复解码，O(N) pick 解码消除。`keepalive_test.go:169` 语义不变。
- T4.3 **`promptText()` 读一次**（`server.go:444` + `prompt.go:72`）：清理非收益，顺手做。

### Phase 5 — 数据驱动终局（看 Phase 0/压测的数再动）

- T5.1 **轮询终局**：`hold_ms` 分布若普遍 ≥3s → 改「快速重 poll + 服务端 hold」，完成检测 ≈1 RTT；先把 `pollCallTimeout=30s` 调到 ≥60s（53s→503 案例）。
- T5.2 **TTL 3m→5m**：看 `sandbox_hit` 率与 start 超时率。TTL 翻倍 = 预热流量减半。
- T5.3 **同沙箱并发实测 + 沙箱池化决策**：用 T0.2 的 `inflight` 与 start 失败率做相关性；证实碰撞再建每账号 M 槽沙箱池（checkout/checkin），**不提前建**。`account_concurrency` 默认值据此修正。
- T5.4 **SessionAffinity 再评估**：预热覆盖后 affinity 只剩沙箱 warmth 价值，看数决定默认关/缩短 TTL。
- T5.5 **图片 CPU**：`attachments.go:250` 盒式缩放，T1.5 后仍高则加信号量（上限=CPU 数）。

## 5. 任务清单（文件级）

| # | 文件 | 改动 | 验收 |
|---|---|---|---|
| 0 | `stream.go`，`client.go`，`server.go`，`pick.go` | 分段计时 + poll hold + httptrace + inflight | 全阶段可还原；基线存档 |
| 1 | `server.go`，`anthropic_handler.go`，`gemini.go`，`responses.go` | 首帧提前 + Flush | 首帧 <200ms；首内容帧不变 |
| 2 | `internal/egress/resin.go`，`internal/pool/account.go` | Transport 调优 + 同出口共享 | `conn_reused`>90%；测试绿 |
| 3 | `stream.go`，`client.go` | register 异步 + 冷路径并行 | hang 不阻塞 start |
| 4 | `client.go:870` | 轮询 0.4s 起步 | 短轮检测提前 |
| 5 | 三入口 | `X-Accel-Buffering: no` | 响应头可见 |
| 6 | `internal/api/chat_files.go` | 附件并行 | 现有测试绿 + 并行单测 |
| 7 | `internal/auth/login.go`，`client.go` | singleflight ×3（请求↔预热共享） | 10 并发只 1 次网络 |
| 8 | 新增 `prewarm.go`、`prewarm_boot.go`，`admin/config.go`，`schema.go` | warm-set 预热子系统 | §Phase 2 验收全项 |
| 9 | `internal/pool/`，`internal/clientkeys/` | save 移出 p.mu + 合并写 + Flush | 慢 PG 不堵 pick 的压测 |
| 10 | `client.go` | prepare 退避不换号 | 级联换号消失 |
| 11 | `internal/scheduler/session.go` | 一次解析 | 等价单测 |
| 12 | `internal/auth/login.go` | JWT exp 缓存 | `keepalive_test.go` 绿 |
| 13 | 线上配置 | 轮询终局/TTL/并发上限/亲和 | 首内容帧 P95 回落 |

顺序：0 → 1~6（可并行）→ 7 → 8 → 9~10 → 11~12 → 13（看数）。

## 6. 验证矩阵

- 单测：`go test ./internal/...` 相关包全绿。T3.1/T3.2 涉及用例允许加 `Flush()`，不允许删断言。
- 首内容帧探针：`curl -N -w '\nstarttransfer=%{time_starttransfer}\n' ... | while read line; do echo "$(date +%s.%N) $line"; done`，取首个含非空 `"content"` 的 `data:` 行时间。
- **压测（方案核心）**：≥20 账号池，50 并发突发流式请求，观测：首帧/首内容帧 P50/P95（对基线 ≤1.5×）、429+Retry-After 是否干净、`sandbox_hit` 率、预热后台请求速率（≤预算）、慢 PG 注入（测试库 sleep 200ms）时 pick 是否被堵（T3.1 验收）。
- 回放：`har/抓包2.har` 时间线核对轮询改动。
- 线上：`scripts/deploy.sh`（`DEPLOY_HOST=YOUR_SERVER_IP DEPLOY_PORT=4344`，ControlMaster 复用，`.env/data*` 保留）→ `/healthz` → `/admin/`（看 warm/total）→ `/v1/models` → 探针对照基线。

## 7. 风险与回滚

- 首帧提前：全账号失败时客户端收到 200+role+错误内容帧——与现行 `:529-536` 路径一致，非新行为。回滚一行。
- Transport 共享：Resin `X-Resin-Account` 必须 per-request，否则粘性错乱。回滚恢复每账号 Transport。
- register 异步：丢 401/403 提前发现，`ensureSession` 覆盖。回滚改同步。
- 预热子系统：预算超支（K 误配/刷新过频）→ `prewarm_accounts=0` 整体关闭即回滚；warm-set MRU 策略可能让长尾账号永远冷（其请求付冷链）——这是设计取舍，容量规划靠 K 覆盖目标并发。
- 异步落盘：崩溃丢 ≤1s 计数/用量/**冷却状态**（同 doc）。回滚改回同步 save。
- prepare 退避：单请求占账号槽位时间最多 +3s。回滚改回直接抛错换号。
- 长轮询终局：hold 撞 `pollCallTimeout` 计 failure，先调超时再上。

## 8. 基线数据（Phase 0 上线后回填）

| 场景 | 首帧 P50/P95 | 首内容帧 P50/P95 | 样本 |
|---|---|---|---|
| 热（terra，全链命中） | — | 5.5s（turn日志，N=1） | _2026-09-17_ |
| 冷（sol，全链未命中） | =首内容（首帧提前已生效） | P50 18.4s / P95 23.0s（N=5） | _2026-09-17_ |
| 冷（astra） | =首内容 | P50 12.3s / P95 26.0s（N=5，全400内联失败） | _2026-09-17_ |
| 20并发×40（sol，2026-09-17，Phase1+2已上线） | =首内容 | 首内容 P50 91s / P95 167s / max 228s，完成率 40/40 全 200 | _N=40，wall=281s_ |
| Phase3 后单请求（sol，预热覆盖） | 0.9~1.1s（直连 curl time_starttransfer） | 34~108s（整轮，上游侧排队） | _N=3_ |
| Phase3 后 20并发×40（sol） | 待 Phase3 日志对照 | 首内容 P50 21s / P95 56s / max 69s，完成率 40/40 全 200，wall=97s | _N=40_ |
| L1+T5.1 验收 20并发×40（sol，f94209f，warm_preference=on） | P50 0.82s / P95 5.0s | 首内容 P50 9.6s / P95 24.9s / max 30.8s，完成率 40/40 全 200，429无Retry-After=0，wall=42.1s | _N=40，2026-09-17_ |

首轮实测要点（2026-09-17，cb1c685）：

- 首帧提前已生效：探针逐帧时间戳里首帧=首内容，role 帧不再等整轮。
- round-robin 下每次命中不同账号，每个都是冷的（`sandbox_hit=false`，
  session/project/sandbox 全链 4~13s）——正是预热子系统要消灭的。
- status 长保持证实：`hold_ms` 两簇——快速 pending（200~600ms）与 hold 到完成
  （2.4~6.1s）；首 poll 直接 completed 的也存在（hold 6080ms）。
- `conn_reused=true` 全绿：T1.1 Transport 共享生效。
- astra 全灭：上游 `codex_v2_restore_start` 400（见 §9），非本计划问题。

## 9. 线上异常备忘（非本计划问题，记录以免误归因）

- 2026-09-17：`gpt-6-astra` 全账号 start 内联 400
  （`POST .../codex_v2_restore_start failed (400 Bad Request)`），sol/terra 正常。
  属上游侧模型/账号问题，基线采集改用 sol/terra。
- 2026-09-17 17:10~17:40（UTC）：prism.openai.com 整段 5xx（`auth-session-policy-unavailable`
  / Cloudflare 504 / `backend/1/new` Internal error），真实请求连带失败：换号重试把几十个
  账号逐个送进 server 类 2~3 分钟冷却。预热子系统行为符合设计：失败只记日志+退避
  （fails=1→4，backoff 1m→5m），不打冷却；上游恢复后 warm-set 自然回暖
  （`warmed=4`，`sandbox_hit=true` 的 `total_ms=0` 条目出现）。教训：boot stagger
  抖动窗口 30s 在 16 账号 × workers=10 下仍显密，观察 N=100 时是否需要按 K/workers
  动态拉长。
- 2026-09-17 20并发×40 压测（Phase1+2 已上线，sol）：
  首内容 P50 91s / P95 167s，完成率 100% 全 200，但绝对值比单请求冷启动（18s）慢 5 倍。
  turn 日志拆解：冷链各段被**上游侧排队**放大——session 9~22s、project 3~38s、
  sandbox 20~51s、start 3~32s、poll 9~29s，而网关自身开销（t_pick 1~2ms、
  register 50~600ms、conn 全 reused）占比 <1%。结论：**并发下的首字瓶颈已不在网关，
  在上游 prism.openai.com 对突发 20 并发的限流/排队**（单账号上游 RTT 从 ~1s 膨胀到 ~10s）。
  网关侧剩余杠杆只剩：warm-set 覆盖（本轮 warm-set=16 但请求分散到 40+ 不同账号，
  多数仍付冷链——突发并发 > K 时必然穿透）、单号并发上限（T5.3）、上游恢复后的回暖速度。

## 9.5 Phase 3 后复核（2026-09-17，重排优先级）

证据：turn 日志里网关自身开销 <1%（t_pick 1~2ms @ N=40+、conn 全 reused）；
首帧直连实测 0.5~1.1s（T1.0 达成）；20 并发压测 P50 91s→21s。
**剩余首内容帧大头 = 冷链穿透 + 上游突发排队**。

### T5.3 实验结论（已实测，闸门已开）

- 第一次实验（18:24 前后）撞上上游二次劣化：8 并发全部挂在 `backend/1/new`
  http2 超时上。**新发现：预热与突发并发同时打冷账号时，并发冷准备叠加上游
  排队形成正反馈**（prepare 就地退避拦住了级联换号，T3.3 有效，但账号照样废在
  上游侧）。教训：warm-set 必须在流量低峰保持热度，不能等突发。
- 第二次实验（19:27，上游健康，fill-first + account_concurrency=4，8 并发×8）：
  两账号各 inflight=4，**同沙箱 4 路并发 start 全部一次成功，无重试无冲突**；
  全部 `sandbox_hit=true`、`register_ms=0`，整轮 4.7~6.1s（start 1.6~3.0s +
  poll 2.3~3.8s）。结论：**同沙箱并发安全（至少 4 路），不需要沙箱池化**；
  `account_concurrency=4` 作为稳态推荐值成立。
- 实验后配置已还原 round-robin / 0。

### 最终方案（按序执行）

1. **L1 温热优先选号**：`pickStrategy` 内按 `SandboxWarm()` 探针把 tier 分温/冷
   两组，温组内 RR，温组耗尽/满槽（inflight≥4）才落冷组；冷却/限额/分组/优先级/
   tried 语义全保留。`pool` 侧加一个轻量导出访问器（读探针注册表，不走完整
   Snapshot）。配置 `warm_preference`（admin 热改，默认关，灰度后开）。
   配套稳态配置：`account_concurrency=4`、`account_concurrency_429=2`。
2. **T5.1 轮询急启**：前 3 次 pending 用 100ms 间隔，之后回 0.4→0.8→…→2s；
   `pollCallTimeout=30s` 不动（观测最大 hold 6.1s）。每轮省 1~2s。
3. **验收（已完成，f94209f）**：20 并发×40（sol，warm_preference=on）首帧 P50 0.82s /
   P95 5.0s，首内容 P50 9.6s / P95 24.9s（对照 Phase3 后 21s/56s，目标 P50≤12s 达成），
   wall 97s→42.1s，40/40 全 200 无脏 429。turn 日志：温账号 `sandbox_hit=true`、
   `register_ms=0`、整轮 6~10s；少量冷穿透为 warm-set 外账号（符合预期，K=16 < 突发 20）。
   稳态配置已落线上：`warm_preference=true`、`account_concurrency=4`、
   `account_concurrency_429=2`、`prewarm_accounts=16`/90s（100 账号 0 冷却、16 温热、
   sandbox_age ~125s）。
4. **不做**：Phase 4 CPU 改写（<1%）；沙箱池化（T5.3 已证伪需求）；T5.2 TTL
   （预热已覆盖）；T5.5 图片 CPU（未见热点）；T5.4 亲和保持默认（L1 后钉住=保热）。

## 9.6 探针修正备忘

`ttfbload.py` 首帧读粒度修正：role 帧未读出前逐字节读（`read(4096)` 缓冲会把
首帧憋到与首内容同时返回，虚高 ~20s）。修正后实测：单请求首帧 0.5s；
19:27 实验 8 并发首帧 P50 0.81s / P95 4.2s。
