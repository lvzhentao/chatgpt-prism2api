# prism-2api 项目计划

目标：把 **prism.openai.com**（OpenAI 的 AI LaTeX 编辑器）封装成 OpenAI / Anthropic 兼容 API，基于 `web2api` 技能的 Go 内核脚手架。

分析工作区（`prism-gpt/`）里的抓包与文档**已全部迁入本仓库**，仓库即唯一来源：

```
prism-2api/                    交付仓库
├── har/                       抓包（sop-capture / 抓包2 / 登录-全流程）+ 站点 system prompt
├── cookies/                   账号 cookie 导出
├── docs/PRISM_API.md          ★ 接口梳理（证据文档，含真机验证结论）
├── docs/VERIFY.md             ★ 验收记录（真机矩阵）
├── docs/LOGIN.md              登录能力设计
├── docs/PLAN.md               ★ 本文件
└── internal/adapter/prism/    适配器
```

---

## 交付物

| # | 交付物 | 位置 | 状态 |
|---|---|---|---|
| D1 | 接口 API 文档（含请求/响应/exact 字段、真机结论） | `docs/PRISM_API.md`（分析工作区的 `API.md` 已并入，无残留内容） | ✅ |
| D2 | 适配器 5 文件（含状态机 client.go） | `prism-2api/internal/adapter/prism/` | ✅ |
| D3 | 可运行的 `prism-server`（`/v1` + `/admin`） | `prism-2api/` | ✅ |
| D4 | 真机验收记录（打真实上游出文本） | `docs/VERIFY.md` | ✅ |
| D5 | 账号导入格式说明（cookie 导入） | `prism-2api/README.md` + `docs/PRISM_API.md` | ✅ |
| D6 | 多账号轮询实测（2 个真实账号） | `docs/VERIFY.md` §5 | ✅ |
| D7 | **登录能力设计（协议登录 + Camoufox）** | `prism-2api/docs/LOGIN.md` | ✅ |
| D8 | **账号密码+MFA 登录（浏览器侧车 + CLI）** | `prism-2api/sidecar/login/` + `cmd/login` | ✅ 真机验证 |

无 DESIGN.md → 管理台用模板默认样式（已确认）。

---

## 已完成的关键验证（真机，2026-09-16）

1. **Go net/http 可直连**：curl 被 Cloudflare 403（TLS 指纹），Go 客户端 200。→ 适配器用 Go 原生 HTTP，不需要浏览器。
2. **最小 cookie 集 = 2 个**：`prism_oai_access_token` + `prism_session_token`（少任一个 `/api/*` 报 401 `{"error":{"code":"token_expired"}}`）。
3. **完整预热链**（缺一不可，缺 → start 请求无限挂起）：
   ```
   POST /api/backend/1/new                          → sandbox_token（每次调用都是新沙箱会话）
   POST /api/projects/{id}/sandbox/resources-token  → access_token(JWT 1h)
   POST /s/sandboxes/proxy/resources-token          → {token, resourceBaseUrl, projectId}
   POST /api/y {docId}                              → Y-Sweet ws 令牌
   POST /s/sandboxes/proxy/token                    → 令牌交棒（X-Crixet-Sandbox-Token）
   GET  /s/sandboxes/proxy/wait-for-sync            → status: synced（关键判据）
   ```
4. **对话 id 由站点 server action 铸造**：`POST /?u={projectId}&pg=1` + `next-action: 60f6ef46…`（"新聊天"动作）→ RSC 响应 `1:"cdx1_…"`。
5. **端到端成功**：start（11.6s）→ status 首次轮询即 `completed`，取出 `payload.output[0].content[0].text` = `收到`。

---

## 阶段

### P0 分析与文档 ✅
- 三份 HAR 全量解析：端点表、认证 cookie、项目、沙箱、对话、遥测
- 导出站点 system prompt（`har/prism-system-prompt.txt`，7476 字符）
- **D1 `docs/PRISM_API.md`**

### P1 脚手架 ✅
- `git clone web2api-kit` → `new_site.py --quick prism` → `prism-2api/`
- 内核 verbatim，只改 `internal/adapter/prism/`

### P2 适配器 ✅
| 文件 | 内容 |
|---|---|
| `endpoints.go` | 全部路径常量；`ChatStream=false`（轮询模型，非 SSE） |
| `models.go` | 静态目录：`gpt-6-astra`（含别名、thinking=reasoning_effort） |
| `session.go` | `ExchangeCredential`=校验 cookie 凭据并探活；`FetchUsage`=entitlements+session；`LoginURL/PollLogin`=ErrNotImplemented（账号页导入） |
| `stream.go` | `buildChatBody` OpenAI→Prism `input`/`metadata`；`Stream`=预热链+铸造会话+start+轮询→`adapter.Event` |
| `client.go`（新增） | 账号级状态机：session 自举、project 解析、sandbox 会话预热、失败重试 |

**凭据格式**（管理台导入，JSON）：
```json
{"name":"acct1","email":"x@y.com",
 "access_token":"<prism_oai_access_token JWT>",
 "refresh_token":"<prism_session_token JWT>",
 "api_key":"<可选：prism_session_token>"}
```
> 内核只在 `AccessToken` 上做 JWT 过期/身份判定，故 `access_token` 放 10 天寿命的 `prism_oai_access_token`；会话令牌由适配器自行自举/续期（`POST /auth/session` 只需 oai cookie 即可换新 session cookie，已验证）。

### P3 验证 ✅
- `go build` → `--mock`（`/v1/models` 有目录、mock 对话有文本）
- `--listen` 真机：用真实 cookie 走 `/v1/chat/completions` 与 `/v1/messages`，比对文本

### P4 收尾 ✅
- `VERIFY.md` 验收记录、TODO 清零、README（导入格式 + 启动方式）

---

## 关键决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 对话上下文 | **单轮**：每次请求铸新会话，整段 messages 作为 input | 已确认；避免 previousResponseId 状态耦合 |
| 模型 | 静态目录 `gpt-6-astra` | 上游无模型列表接口 |
| 认证 | Cookie 导入，不做 OAuth 自动登录 | 登录需 OpenAI 账号密码 + 风控，性价比低 |
| 沙箱预热 | 每账号一份（token+synced 状态），失效自动重来 | 缺预热会挂起；每次新铸代价大 |
| 出口 | 内核 egress/proxypool | Cloudflare 需稳定出口 IP |

## 交付后新增发现（已处理）

| 发现 | 处理 |
|---|---|
| 内核 `nonStreamChat`/`streamChat` 在 `iter.Next()` 失败分支对 nil 账号调 `Release()` → 进程 panic | 修复 2 处（A/B 复现验证：修复前 curl Empty reply + panic，修复后干净错误） |
| `internal/api` 两个测试文件引用 Cursor 范式残留符号，整包不编译 | 删除 4 个无效用例，保留其余；`go test ./...` 全绿 |
| `internal/auth` 保活用例依赖未绑定的 `ExchangeHook` 占位实现 | 用例内绑定最小换票桩（测的是保活语义） |
| `owned_by: "cursor"` 模板残留 | 改为 `adapter.Name()` |
| `ExchangeCredential` 遇上游偶发 503 直接失败 | 3 次重试（抓包实证上游会 503） |

### P5 登录能力（账号密码 + MFA + 浏览器兜底）📐 已设计，待实现

> 详细设计：`prism-2api/docs/LOGIN.md`；参考实现：`gpt注册机/freeagent-producer`。

现状：账号只能人工导 cookie（`prism_oai_access_token`，10 天寿命）。
新增两种服务器侧登录（产出同一种凭据，走同一条入库路径）：

**前置探测结论（决定主次）**：`auth.openai.com` 对普通 Go 客户端回 CF JS 挑战，
带上浏览器 `cf_clearance` 也不行（清关绑定 TLS 指纹 + IP；TLS 伪装同样 403）→ **浏览器是主路径**。

| 方式 | 机制 | 关键依赖 |
|---|---|---|
| **Camoufox 浏览器登录（主）** | Python 侧车驱动真浏览器：Prism 首页 →「使用 OpenAI 继续」→ 邮箱 → 密码 → TOTP → 回跳后导 `prism_oai_*` cookie | Python + camoufox + playwright（镜像层）；与对话同一出口 IP |
| **协议登录（可选加速）** | `prism /api/auth/redirect` → auth.openai.com `authorize/continue` → `password/verify` → `mfa/issue_challenge`+`mfa/verify`（TOTP）→ `workspace/select` → 跟随 `continue_url` 到 `prism/auth/popup-callback` → 抓 cookie | sentinel Node 侧车、Go TOTP（✅ 已实现）、能过 CF 的出口 |

接线要点：
1. **不新增内核算法**：重登凭据束放进 `auth.Token.RefreshToken`（加密落库），
   内核 keepalive 到点自动调 `ExchangeHook` → 适配器用密码+TOTP 重登 → **账号寿命突破 10 天**。
2. 导入格式扩展为 `email----password----totp----proxy`，与现有 cookie/JWT 行按内容自动判别。
3. 入口分三层：CLI 单条 → CLI 批量 → 管理台任务（SSE 进度）。
4. 失败分级沿用 `failclass`；风控/sentinel 失败自动降级到浏览器路径。

进度：P5.0 TOTP ✅ → P5.1 浏览器侧车 ✅ → P5.2 凭据束+自动重登 ✅（代码） → P5.3 CLI 单条/批量 ✅ → P5.4 可选协议通道（待评估） → P5.5 管理台入口（待排）

**真机证据**（`docs/VERIFY.md` §7）：`account3@example.com` 全流程登录成功；
`prism-login` CLI 把 `account5@example.com` 登录入库后，`/v1/chat/completions` 正常出文本。

**不做**：新账号注册（依赖邮箱池/接码，属另一产品形态）。

### P6 工具调用仿真 ✅（2026-09-17）
- 证伪上游原生工具通道（`response_with_tools_start` 请求体无 `tools` 键、不产出 `function_call` item）
- 提示词协议 + 文本解析：单次 / 并行 / **连续多轮**、OpenAI + Anthropic、流式全部真机通过
- 上游语义修正：带 `tools` 时上游不采信 `system` → 工具链路上下文改走本轮 user 消息
- 顺带修掉 Anthropic 入口的模型名等级后缀 400（`gpt-5.6-sol-high`）
- **D9 工具调用仿真** → `internal/adapter/prism/tools.go` + `docs/VERIFY.md` §11

## 风险与未决

1. **`next-action` 是构建期常量**：站点重新部署后失效 → 需回退策略（首轮失败时用自造 `cdx1_<uuid4>` 兜底，见 `docs/PRISM_API.md` §8 待验证项）。
2. **`prism_oai_access_token` 10 天寿命**：→ 由 P5（协议登录 + 自动重登）解决；在 P5 落地前账号寿命仍是 10 天。
3. **start 可能挂起**：已确认未预热会无限挂；适配器必须给 start 设置超时并触发重新预热 + 重试。
4. **`wait-for-sync` 轮询**：正常 1~2 次即 `synced`，异常时会一直 `syncing`（本次观测到沙箱会话过期后不再恢复）。
