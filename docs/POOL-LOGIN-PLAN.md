# ChatGPT 账号池 → Prism 凭据：导入 / 选中 / 登录 / 维护（计划）

目标：把「ChatGPT 账号（email + password + TOTP）」批量导入本服务号池，按需**选中**其中若干，
用**协议登录**或**浏览器登录**（都过代理、都支持 sid 轮换）拿到 Prism 凭据并入库，
之后能自动维护（到期重登）。

---

## 0. 现状盘点（已核实的代码与能力）

| 能力 | 位置 | 现状 |
|---|---|---|
| 号池（PG）：账号 / 凭据 / 代理 / 状态 / 分组 / 优先级 | `internal/pool/` | ✅ 已上线，含 `proxy_url`、`proxy_id`、`logged_in`、`enabled`、`disable_reason` |
| 凭据束（email+password+totp+proxy+backend+storage_state）存 `RefreshToken` | `internal/adapter/prism/login.go`（`CredentialBundle`） | ✅ 已有；`ExchangeCredential` → `ReloginWithBundle` 即「到期自动重登」钩子 |
| 浏览器登录侧车（JSON 行协议） | `sidecar/login/login.py`（camoufox / chromium 两后端） | ✅ 已有；实测：headless 过不了 CF，必须 headful |
| 批量登录 CLI（单条/文件/并发/dry-run/storage_state） | `cmd/login/main.go` | ✅ 已有；**只认位置列**，无表头映射、无选择器、代理是全局单值 |
| 管理台导入（JSON / `----` 行 / 纯 token / cookie jar） | `internal/adminapi/import_parse.go` | ✅ 已有；**不认 CSV 表头、不认 password/totp 字段** |
| 管理台账号页 + 任务中心 | `web/src/pages/accounts-page.tsx`、`tasks-page.tsx` | ✅ 已有列表/编辑；无「待登录」筛选与多选 |
| 部署镜像 | `Dockerfile` | ⚠️ **只有 Go 二进制（alpine）**，无 Python / 无浏览器 → 网关侧无法自己跑登录 |
| 协议登录（纯 HTTP，无浏览器） | — | ❌ 未实现（`docs/LOGIN.md` §3 设计：需 sentinel token + 能过 CF 的出口） |

`platforms/chatgpt/protocol_register.py::relogin_with_credentials()`（已跑通的协议登录）、
`platforms/chatgpt/sentinel_runtime/`（node + jsdom + 官方 `sentinel-sdk.js` 生成 token）、
`core/proxy_template.py`（`{sid}` 粘性会话模板 + URL 归一化）。
本机已有：python3.13 + `curl_cffi` 0.14、`.venv` 里的 camoufox 0.5.4 / playwright 1.60、node v22；**缺 jsdom**（`npm i jsdom` 即可）。

**输入数据**（已核实列结构）：

| 文件 | 列 | 行数 |
|---|---|---|
| `~/csv/gmail_alive_50_5_email_password_2fa.csv`（测试用） | `email,password,totp_secret` | 50 |
| `~/csv/gmail_alive_7_email_password_2fa.csv` | `email,password,totp_secret` | 7 |
| `~/Downloads/accounts_20260917_043858.csv` | `ID,Email,Password,Client ID,Account ID,Workspace ID,Access Token,Refresh Token,ID Token,Session Token,Cookies,TOTP Secret,TOTP Recovery Codes,MFA Enabled,Email Service,Status,Registered At,Last Refresh,Expires At` | 3 |

**代理**（用户给的模板）：`http://<user>-region-US-sid-{sid}-t-5:<pass>@us.arxlabs.io:3010`（凭据见 `.env`/私密渠道，不入库）
（`host:port:user:pass` 形式，需归一成 `http://user:pass@host:port`；`{sid}` 每次渲染成新的 8 位串 = 新出口 IP）。
**实测（2026-09-17 本机）**：DNS/TCP 通，但 CONNECT 返回 `403 Forbidden`（`Proxy-Authenticate: Basic`）——
凭据当前不可用（过期 / 额度用尽 / 按源 IP 白名单）。**这是 P0 必须先解决的前置**。

---

## 1. 总体设计

```
                  ┌── 导入（CSV / ---- / JSON）────────────────┐
  CSV 文件 ──────► │ adminapi 解析 → 池子建条目（待登录）      │
                  └───────────────────────────────────────────┘
                                    │
  选择（list + 多选）───────────────┤  管理台：状态筛选 + 多选 + 复制登录命令
                                    │  CLI   ：list / run --select …
                                    ▼
                  ┌── 登录执行（本地 worker，带代理）─────────┐
                  │ 协议：curl_cffi + sentinel（无浏览器）    │  ← 优先
                  │ 浏览器：camoufox / chromium（真浏览器）   │  ← 协议失败降级
                  └───────────────────────────────────────────┘
                                    │ 产出 prism_oai_* + 凭据束
                                    ▼
                  池子写回（SetToken + enabled=true + 登录方式/时间/代理 sid 落账）
                                    │
                                    ▼
                  维护：maintain worker 轮询 → 临期重登 → 回写（= 账号长期可用）
```

要点：
1. **单一池子**（prism-2api 的 PG），两种登录方式产出**同一形状**凭据 → 下游（调度/对话）不区分来源。
2. **登录在本地跑**（P1–P3）：网关镜像里没有浏览器，且本地是住宅出口、我已验证可用；侧车沿用 `sidecar/login/`。
3. **服务器侧车**（P4 可选）：想要无人值守自动重登，就在 compose 里加一个 python+browser+node 的 sidecar 容器，
   把 `PRISM_LOGIN_SIDECAR` 指过去，网关内重登钩子即可自动工作（代价：镜像 +~500MB、服务器出口必须走代理）。
4. **协议优先、浏览器兜底**：`--method auto`（默认）先协议，遇 CF 挑战 / sentinel 失败 / 风控页 → 降级浏览器。

---

## 2. 分阶段实施（状态：P1/P3 已落地并线上验收，P2 卡在代理）

### P0 前置与可行性 spike（0.5 天）——**已先跑，结果如下**

| 项 | 内容 | 状态 / 结果 |
|---|---|---|
| P0-1 | **代理可用性** | ❌ **阻塞**：`us.arxlabs.io:3010` DNS/TCP 通，但 CONNECT 返回 `403 Forbidden`（`Proxy-Authenticate: Basic`），换 sid / 换用户名格式都一样 → 凭据过期 / 额度用尽 / 按源 IP 白名单。**需要你提供可用串** |
| P0-2 | **协议登录 spike**（参考项目 `.venv` + `npm i jsdom`） | ⚠️ 无代理时 `初始化会话` 阶段 60s 超时（`curl(28) 0 bytes`）；本机 `curl https://auth.openai.com/` → `403`（CF），`chatgpt.com` → `403`，`prism.openai.com` → `200` ⇒ **协议路径必须走能过 CF 的代理**，属预期 |
| P0-3 | **浏览器路径 spike**（本服务自己的侧车） | ✅ **成功**：`sidecar/login/login.py --backend chromium`（有头，本机住宅出口，无代理）对 `gmail_alive_50` 第 1 个账号 31s 走完 邮箱→密码→TOTP→`/auth/popup-callback`，拿到 `prism_oai_access_token` / `prism_oai_refresh_token` / `prism_session_token` / `oai-did` / `oai-sc`，并落 `storage_state` |
| P0-4 | 结论 | 浏览器路径**今天就能用**（P1/P3 可直接对接）；协议路径待代理就绪后按 P2 实施 |

### P1 导入 + 选中（1 天）— ✅ 已落地

交付：`internal/accountfile`（两种 CSV 表头映射 + `----`/JSON 兼容，含真实文件回归测试）、
管理台导入走同一套解析、`prism-login --import-only/--status pending/--only/--limit`、
管理台批量动作 `{"action":"login"}`（任务中心 SSE 进度）。线上：50 账号导入 + 选中登录 3/3 通过。

### （原始设计，保留）
| 项 | 内容 |
|---|---|
| P1-1 | `adminapi` 导入解析扩展：**CSV 表头映射**（`email`/`Email`、`password`/`Password`、`totp_secret`/`TOTP Secret`/`totp`/`2fa`、可选 `proxy`），兼容现有 `----` 行与 JSON；大小写/空格/下划线归一 |
| P1-2 | 「待登录」态：导入即建账号条目（`enabled=false`，`disable_reason=待登录`），凭据束写入 `RefreshToken`（沿用 `WEB2API_ENCRYPT_KEY` 加密），登录成功再 `SetToken` + `enabled=true` |
| P1-3 | 导入入口：管理台「账号 → 导入」接受**粘贴 CSV / 上传文件**；CLI `--file` 同一套解析（同一个 Go 包，避免两份逻辑） |
| P1-4 | **选择器**：`prism-login list --status pending|logged|all [--grep]`；`prism-login run --select name:a,b / email:a@b,c@d / file:sel.txt / status:pending [--limit N]` |
| P1-5 | 管理台：账号页加「待登录 / 已登录 / 全部」筛选、复选框多选、显示「已选 N 个」+「复制登录命令」按钮（登录在本地执行，UI 只负责选与看） |
| 验收 | 50 行 CSV 全量导入 → `list` 显示 50 条待登录 → 选中 3 条打印出可直接执行的 `prism-login run --select …`；重复导入幂等（同邮箱不重复建） |

### P2 协议登录（1.5–2 天）
| 项 | 内容 |
|---|---|
| P2-1 | 自研 sentinel runtime：`sidecar/login/sentinel_runtime.cjs`（jsdom 加载**官方** `sentinel-sdk.js`，≈200 行）+ `sentinel_client.py` 桥。**不复制参考项目代码**（其 LICENSE 首行是 AGPL-3.0，复制会让本项目受 AGPL 约束） |
| P2-2 | `sidecar/login/protocol_login.py`：curl_cffi（`impersonate=chrome142`）跑 **Prism OAuth 全链**：`/api/auth/redirect` → `authorize/continue`（带 `openai-sentinel-token`）→ `password/verify` → `mfa/issue_challenge` + `mfa/verify` → `workspace/select` → 跟随 `continue` 到 `/auth/popup-callback?code=…&state=…` → 从 cookie jar 取 `prism_oai_access_token` / `prism_oai_refresh_token` / `prism_session_token` |
| P2-3 | 代理：`{sid}` 模板渲染 + URL 归一化（移植 `core/proxy_template.py` 的**行为**，Go/Python 各一份：`internal/proxypool` 侧供 worker，Python 侧供侧车）；**每账号固定 sid**（`sha1(email)[:8]`，落库 `proxy_url`），复登保持同 IP |
| P2-4 | 侧车统一协议：新增 `backend=protocol`，与既有 camoufox/chromium 共用同一套 JSON 行协议与事件（`state`/`done`/`error`）→ `RunSidecarLogin` 零改动即多一条路 |
| P2-5 | 失败分类（复用 `internal/failclass`）：密码错/锁定 → `auth`（标记，不重试）；缺 TOTP → `need_totp`；CF 挑战/sentinel 失败 → 降级浏览器；5xx/网络 → 退避重试 |
| 验收 | 10 个账号协议登录，成功率与失败分类列表；成功账号当场可跑 `/v1/chat/completions` 出文本；同一账号复登 2 次都成功（sid 固定） |

### P3 浏览器登录接线（0.5 天）— ✅ 已落地（服务化）

`Dockerfile.login` + compose `prism-login` 服务（chromium + xvfb + node/jsdom），
网关 HTTP 驱动（NDJSON），`PRISM_LOGIN_STATE_DIR` 每账号持久化 storage_state，
导入语义 = 停用待登录、登录成功才启用、保活跳过停用。

### （原始设计，保留）
| 项 | 内容 |
|---|---|
| P3-1 | 侧车依赖固化：`sidecar/login/requirements.txt`（playwright/camoufox/curl_cffi）+ README 补 `npm i jsdom`、`python -m camoufox fetch` |
| P3-2 | 代理注入 + `storage_state` 每账号持久化（复登复用设备指纹） |
| P3-3 | `--method browser|auto` 接线；协议失败账号自动落到浏览器 |
| 验收 | 协议失败的 5 个账号里，浏览器补登成功率 ≥ 60%；两者产物在池子里形状一致 |

### P4 维护（1 天，可选但强烈建议）
| 项 | 内容 |
|---|---|
| P4-1 | `prism-login maintain --interval 30m --method auto --before 48h`：挑 `expires_at < now+48h` 的账号重登 → 回写；失败按 `failclass` 冷却并记日志 |
| P4-2 | 本地常驻（launchd/cron，本机）**或**服务器侧车容器（compose 加 `sidecar`：python+playwright+node+xvfb，`PRISM_LOGIN_SIDECAR` 指向它） |
| 验收 | 手动把某账号 token 改到 1 小时后过期 → worker 自动重登 → 网关继续可用；日志可见完整链路 |

### P5 验收与文档（0.5 天）
- 50 账号跑批：成功/失败/分类统计表；池子状态快照；3 个账号的真实对话结果。
- 更新 `README.md`（能力表 + 用法）、`docs/LOGIN.md`（协议/浏览器两条路的现状）、`docs/VERIFY.md`（真机记录）。

---

## 3. 关键决策（需要你拍板）

| # | 决策 | 选项 | 我的建议 |
|---|---|---|---|
| D1 | 登录产物 | (a) 只要 Prism 凭据（本服务用）(b) 同时保一份 ChatGPT 会话（access/session token + cookies，落池子备注/字段） | (b)：rich CSV 里本来就有这些列，多存一份几乎零成本 |
| D2 | 登录跑在哪 | (a) 本地 Mac worker（现在就能跑）(b) 服务器侧车容器（无人值守，镜像 +500MB，服务器出口必须走代理） | 先 (a)，P4 再上 (b) |
| D3 | sentinel 运行时 | (a) 自研（jsdom + 官方 SDK，规避 AGPL）(b) 直接拷参考项目（本项目需按 AGPL 分发） | (a) |
| D4 | 代理 | 现有串 403，需要可用凭据（或确认是否需要按源 IP 白名单） | 你确认 |

---

## 4. 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| `auth.openai.com` Cloudflare 挑战 | 协议路径直接 403 | 协议走 curl_cffi chrome142 + 美国住宅/机房代理；失败即降级浏览器（浏览器是已验证路径） |
| 代理 403 / 额度耗尽 | 全部路径不可用 | P0 先验；worker 启动前自检代理（`api.ipify.org` 出口校验），失败即停不空跑 |
| 账号被锁（密码错/频繁登录） | 账号损失 | 密码错不重试；同账号 24h 内失败 ≥3 次标记 `locked?` 并暂停；并发默认 1~3（浏览器）/5（协议） |
| TOTP 时间漂移 | 验证失败 | 失败等下一个 30s 窗口重试 1 次；仍失败归类 `auth` |
| 服务器无浏览器 | 网关不能自维护 | P4 的 sidecar 容器；或本地 worker 常驻 |
| AGPL 传染 | 许可证风险 | 自研 sentinel runtime（D3-a），参考项目只作行为参考 |
| 50 账号批量登录触发风控 | 部分失败 | 阶梯放量：先 3 → 10 → 50，观察失败分类；每账号固定 sid 不跳 IP |

---

## 5. 交付物清单

- 代码：`adminapi` CSV 解析 + `cmd/login` 的 list/run/select + `sidecar/login/protocol_login.py` + `sentinel_runtime.cjs` + 代理模板渲染 + 池子状态回写。
- 文档：本计划 → 落地后并入 `docs/LOGIN.md`；真机数据进 `docs/VERIFY.md`；README 用法一节。
- 验收产物：50 账号跑批表 + 池子快照 + 3 个账号的对话证据。

**工作量**：P0 0.5 天 → P1 1 天 → P2 1.5~2 天 → P3 0.5 天 → P4 1 天 → P5 0.5 天（串行约 5 天；P1 与 P2 可并行）。
