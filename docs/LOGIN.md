# 登录与凭据获取（设计 · 规划）

目标：Prism 账号凭据（`prism_oai_access_token`）**不再只能靠人工导 cookie**，支持三种来源：

| 方式 | 触发 | 产出 | 现状 |
|---|---|---|---|
| A. Cookie 导入 | 管理台导入 / 启动参数 / CLI | `prism_oai_access_token`（+ 可选 refresh） | ✅ 已上线 |
| B. **协议登录**（账号 + 密码 + TOTP） | CLI / 管理台任务 | 同上（服务器自己走 OpenAI 登录 + Prism OAuth 回调） | ⏳ 本设计 |
| C. **Camoufox 浏览器登录** | CLI / 管理台任务（有头/无头） | 同上（真浏览器兜底，抗风控） | ⏳ 本设计 |


---

## 1. 为什么需要 B/C

- `prism_oai_access_token` 只有 **10 天**寿命，到期就得重新导 cookie（人工、不可批量）。
- 有了账号密码 + TOTP，可以：批量导入、token 到期自动重登、失效自动恢复 → 账号寿命不再受 10 天限制。
- OpenAI 登录有反机器人（sentinel challenge），纯协议可能被风控 → 需要浏览器兜底。

---

## 2. 关键前置结论（2026-09-16 真机探测）

| 探测 | 结果 | 含义 |
|---|---|---|
| 普通 Go 客户端 → `prism.openai.com` | ✅ 200（Cloudflare 放行） | 对话链路不需要浏览器（已上线） |
| 普通 Go → `prism /api/auth/redirect` | ✅ 200，返回 authorize URL + `prism_openai_oauth_state_binding` | 登录入口可在 Go 侧发起 |
| 普通 Go → `auth.openai.com/api/accounts/authorize` | ❌ 403 `Just a moment...`（CF JS 挑战） | OpenAI 登录页在 Go 客户端下**过不去** |
| TLS 指纹伪装（tls-client, Chrome_131）+ 代理 → 同上 | ❌ 403 `Just a moment...` | 光靠 JA3 伪装不够 |
| 真浏览器拿到 `cf_clearance` 后，Go 复用该 cookie 再请求 | ❌ 403（含手搓 Cookie 头，排除 Go 丢弃 cookie 的干扰） | `cf_clearance` 绑定 **客户端 TLS 指纹 + IP**，不能移植给 Go |

**结论：OpenAI 侧（auth.openai.com）必须真浏览器。**
所以顺序反过来——**C（浏览器）是主路径，B（协议）是可选加速通道**：

```
主路径  C. Camoufox 浏览器登录   ← 真浏览器过 CF，全程在浏览器里完成 OpenAI 登录 + Prism 回跳
可选    B. 协议登录             ← 仅在「出口 IP + TLS 指纹能过 CF」的环境里启用（参考项目用住宅代理 + curl_cffi 就是这种环境）
兜底    A. Cookie 导入          ← 人工/外部系统产出 cookie（已上线）
```

协议登录仍值得实现（在合适的出口环境里它比浏览器快 5~10 倍、资源占用低），但**不作为唯一依赖**。

---

## 3. 协议登录（B）——纯 HTTP（可选通道）

### 3.1 端点（来自参考实现，`platforms/chatgpt/constants.py:85-97`）

```
OPENAI_AUTH = https://auth.openai.com
POST {OPENAI_AUTH}/api/accounts/authorize/continue   # 提交邮箱，返回下一页状态
POST {OPENAI_AUTH}/api/accounts/password/verify      # {"password": "..."}
POST {OPENAI_AUTH}/api/accounts/mfa/issue_challenge  # {"type":"totp","id":factor_id,"force_fresh_challenge":false}
POST {OPENAI_AUTH}/api/accounts/mfa/verify           # {"type":"totp","id":factor_id,"code":"123456"}
POST {OPENAI_AUTH}/api/accounts/workspace/select     # {"workspace_id":"..."}
POST {chatgpt.com}/backend-api/sentinel/req          # 取 sentinel challenge
```
参考实现里这条链路叫 `relogin_with_credentials()`（`platforms/chatgpt/protocol_register.py:996-1032`），
注释写明「移植自 toSub2 protocol-login」，是已验证可用的路径。

### 3.2 与 Prism 的差异（关键）

参考实现登的是 chatgpt.com，我们要的是 **Prism 的 OAuth 回调**。Prism 侧参数取自我们自己的登录抓包：

```
POST https://prism.openai.com/api/auth/redirect   {"action":"sign-in","provider":"keycloak"}
  → Set-Cookie: prism_openai_oauth_state_binding=…（HttpOnly, Max-Age=600）★ 必须留到回调
  → {"data":{"url":"https://auth.openai.com/api/accounts/authorize
        ?audience=https%3A%2F%2Fapi.openai.com%2Fv1
        &client_id=app_jqKb52JverFFcl5GP4axT8QY
        &redirect_uri=https%3A%2F%2Fprism.openai.com%2Fauth%2Fpopup-callback
        &response_type=code&scope=openid+email+profile+offline_access
        &state=prism_openai_oauth_state.v1.…"}}
```

完整序列：

```
① POST prism.openai.com/api/auth/redirect        → authorize URL + state cookie
② GET  {authorize URL}                            → 登录页（会下发 sentinel challenge）
③ POST auth.openai.com/api/accounts/authorize/continue {email}   ← 带 openai-sentinel-token
④ POST …/password/verify {password}
⑤ 若 page.type == "mfa_challenge"：
     POST …/mfa/issue_challenge {type:totp,id:factor_id}
     POST …/mfa/verify {type:totp,id:factor_id,code:<RFC6238>}
⑥ POST …/workspace/select {workspace_id}（有 workspaces 时才要）
⑦ 跟随 continue_url 链 → 最终 302 到
   https://prism.openai.com/auth/popup-callback?code=…&state=…
⑧ Prism 回调落 cookie：prism_oai_access_token / prism_oai_refresh_token（+ prism_session_token）
   → 抓 CookieJar 里这两个值 → 与「Cookie 导入」同一条入库路径
```

`factor_id` 从 ③ 的响应里取：
`payload["oai-client-auth-session"]["mfa_challenge_factors"]` 或 `["mfa_factors"]` 中
`factor_type == "totp"` 的 `id`（参考实现 `_complete_totp()`，`protocol_register.py:947-982`）。

### 3.3 sentinel token（协议路径的重依赖）

OpenAI 在这些端点校验 `openai-sentinel-token` 头，token 由**官方 JS SDK 在浏览器环境跑出来**。
参考实现的做法（`platforms/chatgpt/sentinel_runtime/`）：

- `sentinel_runtime.cjs`（440 行）：起一个 Node 常驻进程，用 jsdom 伪造 DOM 加载官方 `sentinel-sdk.js`；
- `sentinel_client.py`（168 行）：Python 侧用 stdin/stdout 的 JSON 行协议驱动它，拿到 token 字符串。

我们的方案（Go 为主进程）：

1. **复用同一套 Node 侧车**：把 `sentinel_runtime.cjs` + `sentinel-sdk.js` 搬到 `prism-2api/sidecar/sentinel/`，
   Go 用 `exec.Command` 起进程，按行 JSON 收发（接口同 `DynamicSentinelClient._call/token`）。
2. 退化路径：登录时不带 sentinel 直接打，若上游返回 challenge 校验失败 → 自动切 C（浏览器）。

> 侧车是 Node 依赖（`node >= 18`），Docker 镜像里加一层 `FROM node:20-alpine AS sentinel` 拷 `sentinel/`。

**许可证提醒**：参考项目 `freeagent-producer` 是 **AGPL-3.0**（`LICENSE` 首行）。
直接把它 `sentinel_runtime/` 的代码搬进 prism-2api 会让本项目受 AGPL 约束。三条路：

| 选项 | 做法 | 代价 |
|---|---|---|
| ① 自研（推荐） | 用 **OpenAI 官方公开的 `sentinel-sdk.js`**（参考项目里那份 30KB 压缩包也是官方产物）+ jsdom 自己写运行时（约 200 行） | 一次性开发量，无许可证牵连 |
| ② 直接复用 | 把 `sentinel_runtime/` 拷过来 | prism-2api 需按 AGPL 分发 |
| ③ 跨项目调用 | 部署时把参考项目的侧车当独立服务调用 | 运维耦合；AGPL 网络条款仍有争议 |

默认按 ① 设计，②/③ 作为「先跑通再说」的临时手段。

### 3.4 TOTP

RFC 6238，参考实现是零依赖标准库实现（`core/totp.py`）。Go 侧 `crypto/hmac` + `crypto/sha1` + `encoding/base32`
即可，约 40 行，无第三方依赖。要点：Base32 补齐 padding、大小写不敏感、30s 步长、6 位。

---

## 4. 浏览器登录（C）—— Camoufox 侧车（主路径）

为什么是主路径：`auth.openai.com` 对非浏览器客户端一律回 Cloudflare JS 挑战（§2 实测），
真浏览器是唯一稳定能过的路径（参考项目也是同一套 Camoufox 栈，只是它更多用在注册链路）。

### 4.1 技术选型（对齐参考项目）

| 组件 | 参考项目 | 我们的用法 |
|---|---|---|
| 浏览器 | `camoufox[geoip]>=0.4.0`（反检测 Firefox） | 同 |
| 驱动 | `playwright` / `patchright` | `playwright` + `camoufox` API |
| 后端抽象 | `platforms/_browser_backend.py`：`camoufox` / `bitbrowser` 可切换，窗口形态 `headed/hidden/headless` | 只做 `camoufox`，形态 `headed/headless` |
| 代理 | 每个上下文挂 HTTP/SOCKS 代理 | 复用内核 `proxypool` 的出口 |
| 指纹 | camoufox 自带（含 geoip 时区/locale 对齐） | 同；`storage_state` 持久化以便复登时保持同一设备 |
| 产出 | cookies + `page.context.storage_state()` | 我们只要 `prism_oai_access_token` / `prism_oai_refresh_token`；`storage_state` 一并存下做复登复用 |

**Go 不自己实现浏览器**：做成 **Python 侧车**（`sidecar/login/`），由 Go 用子进程 + JSON 行协议驱动：

```
→ {"action":"login","email":"…","password":"…","totp":"…","proxy":"http://…","headless":true,"timeout":180}
← {"event":"state","name":"open_login"}      # 进度（可映射到管理台任务日志）
← {"event":"state","name":"submit_password"}
← {"event":"state","name":"submit_totp"}
← {"event":"done","cookies":{"prism_oai_access_token":"…","prism_oai_refresh_token":"…"},"storage_state":{…}}
← {"event":"error","message":"…","stage":"password"}
```

侧车内部步骤（对应我们登录抓包的 UI 动作；参考实现的等价入口是
`browser_register.py:3563 run(email, password, totp_secret, relogin=True)`
→ `password_login=True`，产出 cookies + `storage_state`）：

```
① goto https://prism.openai.com/
② 点「使用 OpenAI 继续」 → 弹窗（或直接 goto authorize URL，带 state cookie）
③ 填邮箱 → Continue
④ 填密码 → Continue
⑤ 有 mfa 页 → 填 TOTP（从参数算）→ Continue
⑥ 等回跳 prism.openai.com（同源），读 context.cookies() 里的 prism_oai_* 两个值
⑦ 关上下文，输出 cookies + 可选 storage_state（便于下次复用设备指纹）
```

### 4.2 与协议路径的关系

```
默认:   camoufox（唯一能过 CF 的路径）
可选:   protocol → 失败降级 camoufox
判定切换: 403/JS 挑战页、sentinel 校验失败、连续 2 次密码态失败
```

---

## 5. 与 prism-2api 的接线

### 5.1 凭据存储复用现有字段（零内核改动）

内核 `auth.Token` 只有三个字段，且只在 `AccessToken` 上判过期/身份：

| 字段 | 存什么 |
|---|---|
| `AccessToken` | `prism_oai_access_token`（JWT，10 天 → 决定过期与自动刷新时机） |
| `RefreshToken` | **重登凭据束 JSON**：`{"prism_oai_refresh_token":"…","email":"…","password":"…","totp_secret":"…","proxy":"…"}` |
| `APIKey` | 同 `AccessToken`（让 `--api-key` 导入路径也能用） |

适配器的 `parseCredential()`（已有）扩展识别这个 JSON → 于是：

**内核的 `TokenManager.Refresh()`（token 将过期时自动调 `ExchangeHook(secret=RefreshToken)`）
天然变成「自动重登」**：`ExchangeCredential(secret)` 看到凭据束里有 password+totp
→ 走协议登录 → 拿新的 `prism_oai_access_token` → 返回新 Token → 内核落库。
→ 账号寿命从 10 天变成「密码/2FA 有效期内无限续」。

### 5.2 导入格式扩展

管理台导入框 / CLI 逐行支持（`----` 分隔，后段可缺省）：

```
email----password----totp_secret----proxy
email----password                 # 无 2FA
email----<prism_oai_access_token> # 现有 cookie/JWT 导入（保持兼容，靠内容自动判别）
```

判别规则：段里有 `@` → email；段是 JWT（`eyJ…`）→ token；段是 16~32 位 Base32 → totp；
`http(s)://` / `socks5://` → proxy。需在内核导入解析里加一个「站点专用凭据解析」钩子
（`adminapi.ParseImportLine` 目前只认 token/email）。

### 5.3 登录入口（三层，按成本递增）

| 层 | 形式 | 说明 |
|---|---|---|
| L1 | CLI：`prism-server login --email … --password … --totp … [--proxy …] [--browser]` | 最简单，可脚本批量（`xargs -P`），输出凭据或直接入库 |
| L2 | CLI 批量：`prism-server login --file accounts.txt --concurrency 5` | 读 `email----password----totp----proxy`，逐条登录入库，打印成功/失败表 |
| L3 | 管理台「账号 → 添加」新增「账号密码登录」标签页 + 任务中心进度 | 复用内核任务中心（SSE 进度、可取消） |

L1/L2 先做（不需要新管理台页面），L3 视需要再排。

### 5.4 失败分类（复用内核 failclass）

| 上游信号 | 分类 | 行为 |
|---|---|---|
| 密码错误 / 账号锁定 | `auth` | 不进冷却重试，写账号备注 `login_failed:password` |
| 需要 TOTP 但无 secret | `auth` | 标记 `need_totp`，任务日志提示 |
| TOTP 校验失败 | `auth` | 重试 1 次（等下一个 30s 窗口） |
| sentinel/风控页 | `unavailable` | 自动切浏览器路径 |
| 网络/5xx | `server` | 退避重试（同适配器现有策略） |

---

## 6. 阶段与验收

| 阶段 | 内容 | 验收 |
|---|---|---|
| **P5.0** | Go TOTP（RFC 6238）+ 单测 | ✅ `internal/adapter/prism/totp.go`，RFC 官方向量全绿 |
| **P5.1** | 浏览器侧车：状态事件流 + 错误页识别 + 自动重试 + storage_state 复用 | ✅ `sidecar/login/login.py`；真账号 3 次成功登录（含失败重试） |
| **P5.2** | 凭据束入库 + `ExchangeCredential` 重登 | ✅ 代码完成（`CredentialBundle` + `ReloginWithBundle`）；到期自动重登待长跑观察 |
| **P5.3** | CLI 单条 / 批量（CSV 或 `----` 行） | ✅ `cmd/login`；真机：登录 → 入库 → `/v1/chat/completions` 出文本 |
| **P5.4** | 可选：协议登录（authorize/continue → password → mfa → workspace → 回调）+ sentinel 侧车 | 在「能过 CF 的出口」上验证；过不去则明确报错并降级浏览器 |
| **P5.5** | 管理台入口（导入框支持新行格式 + 登录任务进度） | 页面上加一个账号 → 进度可见 → 账号出现在号池 |

## 7. 依赖与风险

| 项 | 说明 | 处置 |
|---|---|---|
| Node ≥18（sentinel 侧车） | Docker 加 node 层；本机开发需 node | 侧车缺失时只降级不崩 |
| Python + camoufox + playwright（浏览器侧车） | 镜像约 +400MB | 单独镜像层 / 可选 profile |
| 出口 IP | 登录比对话更敏感，数据中心 IP 易触发风控 | 走内核 `proxypool`/Resin，一账号一出口 |
| 凭据泄露面 | 密码/TOTP 落库（加密存储 `WEB2API_ENCRYPT_KEY`） | 沿用内核 secret 加密；导出接口默认不带密码 |
| 上游改版 | auth0 端点或 sentinel SDK 变更 | sentinel 侧车独立目录，改版只动它；协议层做「探测 + 降级浏览器」 |
| 合规 | 账号密码属高敏信息 | 只在自有账号/授权账号上使用；不做注册机 |

> 注册（新号创建）**不在本设计范围**：参考项目里的注册链路依赖邮箱池/接码平台/验证码服务，
> 属于另一个产品形态；本设计只做「已有账号 → 凭据」。
