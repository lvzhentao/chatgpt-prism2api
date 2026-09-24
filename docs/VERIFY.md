# 验收记录 · prism-2api

本文件随仓库交付：接口结论见 `docs/PRISM_API.md`，计划与交付物见 `docs/PLAN.md`。
日期：2026-09-16 ~ 2026-09-17 ｜ 上游：`https://prism.openai.com`（真实账号 `account1@example.com`，free 套餐）
构建：`go build -o /tmp/prism-server ./cmd/server`；PG：本地 `postgres://prism@127.0.0.1:5434/prism`

## 1. 编译与测试

```
$ go vet ./...            → 无输出
$ go test ./...           → 18 个包全 ok（含新增 internal/adapter/prism 用例）
```

新增用例（`internal/adapter/prism/stream_test.go`）：
- `TestBuildInputFoldsHistoryIntoContext` — 历史必须折叠进 Context，末条 user 只出现一次
- `TestBuildInputSingleTurnHasNoContextBlock`
- `TestBuildInputKeepsToolResults` — 工具结果不丢
- `TestParseCredential` — 裸 JWT / Cookie 头 / Cookie-Editor 导出 / JSON 字段
- `TestExtractText` — completed payload → 正文，跳过非 message 项

## 2. Mock 模式

```
$ ./prism-server --mock --listen 127.0.0.1:18080
GET /v1/models → {"data":[{"id":"gpt-6-astra", ...}]}
```

## 3. 真机（真实上游）

| # | 用例 | 命令 | 结果 |
|---|---|---|---|
| 1 | 模型目录 | `GET /v1/models` | `{"id":"gpt-6-astra","owned_by":"prism"}` |
| 2 | 非流式对话 | `POST /v1/chat/completions` `{"messages":[{"role":"user","content":"回复：验收OK"}]}` | `"content":"验收OK"` |
| 3 | 流式对话 | 同上 + `"stream":true` | SSE：role 帧 → `"content":"流式OK"` → `finish_reason:"stop"` |
| 4 | Anthropic | `POST /v1/messages` + `anthropic-version: 2023-06-01`，`回复：anthropic OK` | `"content":[{"type":"text","text":"anthropic OK"}]` |
| 5 | 管理台 | `GET /admin/` | 200；登录 → 强制改密 → 工作台；账号页显示 `account1@example.com`（`FetchUsage` 真机取到） |
| 6 | 多轮上下文 | `system + user(中国首都？) + assistant(北京) + user(它有多少人口？)` | `"截至2025年末，北京市常住人口为2180万人。"`（折叠生效） |
| 7 | 预热复用 | 连续两轮同账号 | start 2.7s / 2.5s，整轮 ~9s（冷启动首轮 11.6s + 预热 20s） |

反例（证明折叠必要）：把 `assistant` 消息按 role 直接透传 → 上游丢弃，模型答 `哪里？`（丢失上下文）。

## 4. 失败路径（真机 + 替身）

**内核 panic 修复 A/B**（`internal/api/server.go`，`iter.Next()` 失败分支的 `acc.Release()`）：

```
上游替身：POST /auth/session → 200 + cookie；其余一律 500
请求：POST /v1/chat/completions

修复前：curl: (52) Empty reply from server    log: panic: nil pointer dereference（3 次）
        pool.(*Account).Release(...) ← api.(*Server).nonStreamChat
修复后：{"error":{"message":"All credentials for model gpt-6-astra are cooling down","type":"rate_limit_error"}}
        log: chat attempt 1 failed on account "default" class=server (retrying)  → panics=0
```

**无效凭据**：`--api-key BAD_TOKEN_XYZ`（空库）→ `no available accounts`，无 panic；账号未入库。
**上游 503**：`ExchangeCredential` 现在重试 3 次（抓包实证上游会偶发 `upstream connect error`）。

## 5. 多账号与轮询（真机，2 个真实账号）

账号：`account1@example.com`（account1）+ `account2@example.com`（account2，凭据从登录抓包的 Cookie 头提取）。
启动：`--api-keys "<tok1>,<tok2>"` → 两个账号均 `logged_in=True`。

```
req1 → account=api-key-2   "多号额度1"
req2 → account=api-key-1   "多号额度2"
req3 → account=api-key-2   "多号额度3"
req4 → account=api-key-1   "多号额度4"
汇总：account=api-key-1 ×4 / account=api-key-2 ×6（含重试占号）
```

- 轮询成立：连续请求在两个号之间交替（默认 `round-robin`，无会话标记时不粘号）。
- 账号隔离：两个号各自创建了自己的 `prism-2api` 项目（不同 uuid / owner），session / 沙箱 / 会话互不干扰。
- 每次请求新铸 `cdx1_<uuid4>` 会话并登记；同沙箱可连续承载多轮。

**上游劣化观测（重要）**：测试期间 Prism 后端整段抽风，`start` 会以
`200 + status=completed + response.status=error` 立刻返回失败：

```json
{"status":"completed","request_id":"…","conversation_id":"cdx1_…",
 "response":{"status":"error","payload":{"reason":"unknown",
   "message":"Error while processing conversation, please submit prompt again.",
   "rootCause":"Project conversation lookup failed (503)"}}}
```

处理：适配器识别「无 turn_state 的 start」→ 换沙箱 + 换会话重试（最多 3 次）；
前置调用（session/项目/预热链）各有 2~3 次瞬时重试；仍失败才交给内核冷却。
最初版本这种响应会被当成正常 start，导致轮询拿到 `400 turn_state is required`（已修）。

## 6. 边界与结论

- Cloudflare：`curl` 403（TLS 指纹），Go net/http 200 —— 适配器用 Go 原生客户端，不需要浏览器。
- 最小 cookie：`prism_oai_access_token` + `prism_session_token`；后者可自举（`POST /auth/session` 只需 oai）。
- 缺沙箱预热链（尤其 Y-Sweet 交棒）时 `start` **无限挂起**：已用 90s 超时 + 自动重来兜住。
- 单账号吞吐：整轮 9~20s（上游是 start+轮询模型，非流式增量）；并发靠内核号池，单号并发受上游沙箱限制未测。

## 7. 登录能力（账号密码 + MFA，真机，2026-09-16）


**前置探测（决定架构）**

| 探测 | 结果 |
|---|---|
| 普通 Go → `prism.openai.com` | 200 ✅ |
| 普通 Go → `prism /api/auth/redirect` | 200，返回 authorize URL + `prism_openai_oauth_state_binding` ✅ |
| 普通 Go / TLS 指纹伪装（Chrome_131）→ `auth.openai.com/api/accounts/authorize` | 403 `Just a moment...`（Cloudflare JS 挑战）❌ |
| 真浏览器取到 `cf_clearance` 后，Go 复用（含手搓 Cookie 头） | 仍 403 ❌ → clearance 绑定 TLS 指纹 + IP，不能移植 |
| playwright chromium **headless** 打开 authorize | 挑战不过 ❌ |
| playwright chromium **有头** 打开 authorize | ✅ 落到 `auth.openai.com/log-in`（"欢迎回来 - OpenAI"） |

结论：**登录必须真浏览器 + 有头**。

**侧车实现**：`sidecar/login/login.py`（Camoufox 默认 / chromium 可切换）
流程：Prism 首页 → 页内调 `/api/auth/redirect` 取授权地址 → goto authorize →
邮箱 → 密码 → TOTP → 回跳 `auth/popup-callback?code=…` → 导出 `prism_oai_*` cookie。

**真机结果**

| 账号 | 结果 |
|---|---|
| `account3@example.com` | ✅ 全流程成功；导出 `prism_oai_access_token`(1757B) / `prism_oai_refresh_token` / `prism_session_token` / `prism_oai_earliest_refresh_at` + storage_state |
| 该 token 直接打网关 | ✅ `POST /v1/chat/completions` → "登录账号可用" |
| `account4@example.com` | ❌ 上游错误页（"糟糕，出错了"）→ 已据此加「错误页识别 + 自动重试」 |
| `account5@example.com`（走 CLI） | ✅ `prism-login` 登录 → 入库为 `cli-account5` → `/v1/chat/completions` → "CLI登录的账号可用" |

**新增代码**：`internal/adapter/prism/totp.go`(+测试) / `login.go`(侧车驱动 + 凭据束) /
`cmd/login`(CLI 单条+批量) / `sidecar/login/`(浏览器侧车 + README)。

### 7.1 Cookie 导入（2026-09-16，空库重跑）

输入：`cookies/prism.openai.com_16-09-2026.json`（Cookie-Editor 导出，15 条 cookie）原样粘贴。

| 形态 | 修复前 | 修复后 |
|---|---|---|
| 整份导出 JSON | ❌ 被当成 **15 个账号**，全部导入失败 | ✅ 折叠成 1 个账号（`account1`，名字取自 JWT 里的邮箱） |
| Cookie 头（`a=…; b=…`） | ❌ 选中排在后面的 **session 令牌**（exp 09-17，12h） | ✅ 选中 access 令牌（exp 09-26，10 天） |
| 裸 JWT | ✅ | ✅（回归不变） |

导入后在**空库**实例上 `POST /v1/chat/completions` → `"可用"`（18.3s），管理台显示
`logged_in=true`、`expires_at=2026-09-26 17:33`。

修复点：`adminapi.parseCookieJar`（识别 cookie 导出形状，按 JWT 有效期区分 access/session）、
`adminapi.pickTokens`（一段文本多令牌时按有效期挑，不再取末位）、`auth.ExtractTokens`、`auth.JWTEmail`。

## 8. 遗留

见 `docs/PRISM_API.md` §8：sandbox_token 精确 TTL、免费号隐性额度。
登录侧：camoufox 正式后端待在有 GitHub 配额的环境执行 `python -m camoufox fetch`；
到期自动重登（凭据束 → 侧车重登）已在代码里，待长跑观察。

## 9. 生产部署（YOUR_SERVER_IP:8301，2026-09-16）

Ubuntu 24.04 / x86_64 / Docker 29.5.3 + Compose v5.1.4；`/opt/prism-2api`，走仓库自带
多阶段 Dockerfile（node 打前端 + go 编译 + alpine 运行），服务器无需 Go/Node。

| 项 | 结果 |
|---|---|
| `docker compose ps` | `prism-2api` healthy / `prism-2api-pg` healthy |
| 公网 `GET /healthz` | `{"status":"ok"}` |
| 公网 `GET /admin/` | 200（`WEB2API_ALLOW_REMOTE_ADMIN=true`） |
| 公网 `GET /v1/models`（带 Bearer） | 200 → `gpt-6-astra`；无 Bearer → 401 |
| 整份 cookie 导出导入 | 1 个账号 `account1`，`logged_in=true`，到期 09-26 |
| 公网 `POST /v1/chat/completions` | `"可用"`，13.7s |
| 公网 `POST /v1/messages` | `content:[{type:text,text:"ok"}]`，`stop_reason=end_turn`，7.8s |
| 公网 `stream:true` | SSE `delta` 分块正常 |
| `docker compose restart prism-2api` | 回到 healthy，账号仍在，对话仍通（状态在 `./data-pg`，restart=unless-stopped） |

部署中修掉的两个配置缺陷（都会让首次 `docker compose up` 直接不可用）：

1. **compose 没给上游地址** → 容器日志 `listening … (endpoint=https://api.example.com)`，
   首次对话 `mint session: Post "https://api.example.com/auth/session": no such host`，
   账号被计入 2 分钟冷却。修复 = compose 设 `VENDOR_API_BASE_URL=https://prism.openai.com`。
2. **Dockerfile 残留脚手架环境变量**（`CURSOR2API_ADMIN_STATIC` / `AGENT_CLI_CREDENTIAL_STORE_DIR`）
   → 管理台静态目录与远程管理开关都不会生效。修复 = 改成 `WEB2API_ADMIN_STATIC` /
   `WEB2API_ALLOW_REMOTE_ADMIN`。

运维坑：Ubuntu 24.04 sshd 的 `PerSourcePenalties`。一次 scp 超时 + 之后连续新建 ssh 会让本机 IP 被罚，
表现是**密码正确也 `Permission denied`**，每次重试都在延长罚期。必须停手等约 10 分钟，之后全程复用一条
ControlMaster（`scripts/deploy.sh` 已这么做）。

## 10. 模型目录与思考档位（YOUR_SERVER_IP:8301，2026-09-16）

前提：模型清单以**账号 entitlement** 为准 —— 登录流程里 Statsig gate `62892348` 的 value：
`gpt-6-astra` / `gpt-5.6-sol` / `gpt-5.6-terra`（`free_model=gpt-5.6-terra`，`free_reasoning_effort=high`）。

### 10.1 模型名（`POST /v1/chat/completions`，串行打，无并发）

| 请求 model | HTTP | 耗时 | 响应 |
|---|---|---|---|
| `gpt-6-astra` | 200 | 22.1s | `"可用"` |
| `gpt-5.6-sol` | 200 | 7.7s | `"可用"` |
| `gpt-5.6-terra` | 200 | 7.7s | `"可用"` |
| `gpt-5.5` | 400 | 1.0s | `invalid_request: … (400 Bad Request)` |
| `gpt-5.4` | 400 | 1.1s | 同上 |
| `gpt-5.6-luna` | 400 | 0.6s | 同上 |

**修复前**同样打 `gpt-5.4` 的形态：20.1s 才返回 + 上游内联 400 被当成 server 类 →
账号整账号冷却 2 分钟 → 后续 4 个模型名全部 0.0s 拿 `All credentials for model … are cooling down`
（那次探测里 sol/astra 并没有真被上游拒绝，是冷却被误导）。

修复（`internal/adapter/prism/client.go`）：`startFailedError` 从内联文案 `(400 Bad Request)` 抽状态码 →
`failclass` 判 `BadRequest`（`Cool=false, Switch=false`，HTTP 400）→ 客户端**立刻**拿到 400，
账号不冷却。`retryable()` 同时让 4xx 不再换沙箱重试（少等 ~20s）。单测：`internal/adapter/prism/client_test.go`。

判定：失败那三条之后立刻再打 `gpt-6-astra` 仍 200（账号没进冷却）。

### 10.2 思考档位（上游 `metadata.reasoning_effort`）

容器日志每轮一行，直接显示真正发给上游的参数：

```text
prism: start model="gpt-5.6-sol" effort="low"    conv=cdx1_7bf49a03-…
prism: start model="gpt-5.6-sol" effort="medium" conv=cdx1_b35bf5c0-…
prism: start model="gpt-5.6-sol" effort="high"   conv=cdx1_66fa9558-…
```

| 入口 | 请求 | 上游实收 effort | 耗时 |
|---|---|---|---|
| OpenAI | `reasoning_effort:"low"` | `low` | 6.8s |
| OpenAI | `reasoning_effort:"medium"` | `medium` | 7.7s |
| OpenAI | `reasoning_effort:"high"` | `high` | 9.7s |
| OpenAI | model=`gpt-5.6-sol-high`（后缀变体） | `high`，model 还原成 `gpt-5.6-sol` | 6.5s |
| OpenAI | model=`gpt-5.6-terra-low` | `low`，model=`gpt-5.6-terra` | 7.2s |
| OpenAI | 不传 effort | `medium`（默认） | 7.7s |
| Anthropic | `thinking{enabled,budget_tokens:16000}` | `high` | 返回 `thinking` 块 + 正文 |
| Anthropic | `output_config{effort:"low"}` | `low` | `391` |

三道题的答案都对（鸡23兔12 / 391 / 2），即档位只影响思考深度，不影响正确性；耗时随档位单调上升。

两个入口的差别：Anthropic 入口过 `capEffort` → 被 `WEB2API_MAX_EFFORT` 压上限（代码默认 `low`，
本部署 compose/`.env` 设成 `high`，容器内 `printenv WEB2API_MAX_EFFORT` = `high`）；
OpenAI 入口的 `reasoning_effort` 不经这个上限，原样透传。

`GET /v1/models` 现返回 `gpt-6-astra` / `gpt-5.6-sol` / `gpt-5.6-terra`。
请求里的模型名本身 100% 透传（无白名单），所以换套餐/加付费账号后那些名字会**自动可用**，无需改代码。

---

## 11. 工具调用（function call）真机验证（YOUR_SERVER_IP:8301，2026-09-17）

### 11.1 结论

对话正常（三个模型均 200）。工具调用**没有原生通道**，由适配器仿真：
**单次 / 并行 / 连续多轮全部真机通过**，OpenAI 与 Anthropic 两个协议都成立，流式也成立。
证据：容器日志 `prism: turn tools_sent=1 tool_calls=1 text_len=0 item_types=message x1`（上游 output 里
永远只有 `message`，没有 `function_call` → 调用来自适配器解析）。

### 11.2 上游没有工具通道（先证伪）

| 探测 | 结果 |
|---|---|
| HAR 里 `response_with_tools_start` 请求体键 | `['conversationId','input','metadata','previousResponseId']`——**无 `tools` 键** |
| 平铺 `tools:[{type,name,parameters}]`（真机） | `200`，`tools_sent=1 tool_calls=0 item_types=message x1`，模型凭记忆答天气 |
| Chat-Completions 嵌套 `[{type:"function",function:{…}}]` | 同上，模型完全无视 |

⇒ 走提示词协议 + 文本解析仿真（`internal/adapter/prism/tools.go`）。

### 11.3 回归矩阵（全绿）

| 编号 | 场景 | 观测 | 耗时 |
|---|---|---|---|
| T1 | OpenAI 单次调用 | `finish_reason:"tool_calls"`、`id=call_…`、`arguments={"city":"北京"}`、`content:""` | 11.9s |
| T2 | 连续两轮（回灌结果 → 又要上海） | 第二轮再出 `tool_calls`（上海），ID 与首轮不同 | 7.6s |
| T3 | 并行（一次要两地） | 一条消息里两个 `tool_calls`（北京 / 上海，ID 不同） | 6.8s |
| T4 | 两条结果回灌 → 收尾 | `finish_reason:"stop"`，正文「北京晴，25℃；上海多云，28℃…」 | 6.8s |
| T5 | `stream:true` | `delta.tool_calls[0]` 带完整 id/name/arguments，`finish_reason:"tool_calls"` | — |
| T6 | Anthropic 两轮 | `content:[{type:"tool_use",id:"toolu_…",name:"get_weather",input:{city:"北京"}}]` + `stop_reason:"tool_use"`；回灌 `tool_result` 后第二轮再出 `tool_use`（上海） | 13.8s / 20.5s |
| T7 | Anthropic `stream:true` | `content_block_start(tool_use)` → `content_block_delta(input_json_delta,"{\"city\":\"北京\"}")` → `content_block_stop` → `stop_reason:"tool_use"` | — |
| T8 | Anthropic 第三轮收尾 | 文本块「北京当前天气：晴，25℃。」+ `stop_reason:"end_turn"` | — |
| E1 | 无 tools 多轮历史折叠（回归） | 历史里 `4173` → 答 `4173` | 6.4s |
| E2 | 带 tools + 历史里的工具结果（修复前答"没看到结果"） | 修复后答「北京温度是 25℃。」 | — |

### 11.4 关键上游语义（本轮单变量测定，决定了实现形态）

| 探测 | 观测 |
|---|---|
| 不带 tools，system 里放「回答必须以 ZZZ 开头」 | ✅ 生效 → `ZZZ 2` |
| **带 tools**，同一句 system 指令 | ❌ 被忽略 → `2` |
| 不带 tools，system 里放对话历史 / 工具结果 | ✅ 采信（`4173` / `25℃`） |
| **带 tools**，system 里放工具结果 | ❌ 模型答"当前对话里没有显示…返回结果" |
| **带 tools**，同样的历史+结果放进**最后一条 user 消息** | ✅ 稳定生效，并正确产出 `<tool_call>` |

⇒ **`tools` 在场时上游不采信 `system` 内容**。工具链路的上下文（客户端 system 指令、对话历史、
已执行结果、工具协议）全部并入本轮 user 消息，`system` item 一个都不发；不带 `tools` 的请求保持
原 system 通道（E1/E2 回归确认未受影响）。

### 11.5 顺带修掉：Anthropic 入口的等级后缀 400

内核只在客户端没给 `reasoning_effort` 时还原裸模型名，Anthropic 入口完全没走这一步 →
`model:"gpt-5.6-sol-high"` 原样发给上游 → `400`（1.4s）。适配器现自行剥后缀
（`splitModelSuffix`）：名字一定剥干净，档位仅在客户端没给时按后缀补。

| 请求 | 上游实收（`prism: start` 日志） | 结果 |
|---|---|---|
| Anthropic `gpt-5.6-sol-high`（修复前） | `gpt-5.6-sol-high` | ❌ 400 / 1.4s |
| Anthropic `gpt-5.6-sol-high`（修复后） | `model="gpt-5.6-sol" effort="high"` | ✅ 200 |
| OpenAI `gpt-5.6-sol-high` + `reasoning_effort:"low"` | `model="gpt-5.6-sol" effort="low"` | ✅ 200 |
| OpenAI `gpt-5.6-terra-high`（无显式 effort） | `model="gpt-5.6-terra" effort="high"` | ✅ 200 |

### 11.6 已知限制

- `tools` 字段仍随请求发给上游（上游忽略），仅为向前兼容保留。
- 工具协议只在该轮声明了 `tools` 时注入，不污染普通对话。
- 模型可能在调用块外写一句说明（如"我先查天气"）——正文照常返回，属正常表现。
- 解析失败的块原样保留在正文里，不吞内容；此时该轮退化为普通文本回答。

---

## 12. input 提示词通道复核（YOUR_SERVER_IP:8301，2026-09-17）

问题：`POST /api/llm/response_with_tools_start` 的 `input[0]`/`input[1]`（站点 persona 7476 字 + 编辑器状态 266 字）
能不能拿掉，只送用户的问题？

做法：适配器加 `prism: start … items=N shape=role(chars)/…` 日志（`PRISM_LOG_INPUT=1` 还能打整段 input），
再用网关跑矩阵，逐条核对上游到底收到了什么。

| 探测 | 上游实收 `input` | HTTP | 耗时 | 观测 |
|---|---|---|---|---|
| 只有 user 消息 | `user(30)` | 200 | 10.7s | 正常作答 |
| system(ZZZ 标记) + user | `system(29)/user(30)` | 200 | 7.2s | 答「ZZZ …」→ system 项确实生效 |
| system(站点 persona) + user | `system(7496)/user(18)` | 200 | 7.3s | 口吻转为 Prism 编辑器助手 |
| system(persona) + system(编辑器状态) + user（站点原形） | `system(7496)/system(266)/user(18)` | 200 | 10.6s | 无差异、无 400 |
| 无 system、多轮历史 | `system(64)/user(39)` | 200 | 7.4s | 答对 `4173` |
| 只给 system、不给 user | `system(27)` | 200 | 8.9s | 上游不强制 user 项 |
| 带 tools（工具链路） | `user(1261)` | 200 | 7.7s | 单条 user |

**结论 1：站点那两个提示词可以拿掉。** 适配器从不注入它们（`buildInput` 只摊平客户端消息），
只发一条 user 就是上游收到的全部。

**结论 2：上游自己还有一套内置提示词，拿不掉。** user-only 请求问身份 → 模型自述
"我是 Codex…能使用终端和文件工具"；要求原样贴系统消息 → 拒绝披露；而站点 persona 的两条硬规则
（Overleaf 禁令、≤2 句）在 user-only 请求里完全不生效（给了长篇 Overleaf 方案与长篇详解）——
说明生效的是上游服务端注入的那套，与 `input` 无关。

**副作用**：`items=`/`shape=` 成为常驻日志（每次 start 一行），`PRISM_LOG_INPUT=1` 时额外打印整段 input，
用于核对「上游到底收到了哪些提示词」。

---

## 13. 注入自己的系统指令：覆盖强度实测（YOUR_SERVER_IP:8301，2026-09-17）

目的：上游内置提示词删不掉（§12），那就抢先注入自己的指令，实测能覆盖到什么程度。
默认指令在 `internal/adapter/prism/prompt.go`；`PRISM_SYSTEM_PROMPT` 可覆盖/关闭。

| # | 注入位置与内容 | HTTP / 耗时 | 观测 |
|---|---|---|---|
| I1 | `system(1194)` 通用默认指令 + user「你是什么模型？」 | 200 / 14.2s | "我是 Codex…编程协作代理" → ❌ 压不过 |
| I2 | 同上 + 「解释 LaTeX 编译，越详细越好」 | 200 / 35.1s | 长篇（用户明确要详细，风格规则让位于明确要求） |
| I3 | 注入 + 调用方自带 `system("回答必须以 ZZZ 结尾")` | 200 / 8.6s | 两个 system 项并存，答尾缀 `ZZZ` ✅ |
| I5 | `system(显式"最高优先级…冲突时以本指令为准"+"自称小助")` | 200 / 8.4s | "我是小助。" ✅ 覆盖成功 |
| I6 | 同一覆盖指令写进 user 消息 | 200 / 10.0s | "我是基于 GPT-5 的 Codex。" ❌ user 消息不覆盖 |
| I8 | 声明 tools + `system(覆盖指令)` | 200 / 6.6s | "我是 Codex，基于 GPT-5。" ❌ **tools 模式忽略 system 项** |
| I9 | 声明 tools + `system(覆盖指令)` + 工具问题 | 200 / 9.4s | 工具链路本身正常（答北京天气） |
| L4 | 声明 tools，覆盖指令并入 user 块（适配器做法） | 200 / 8.1s | "我是本服务的通用助手。" ✅（措辞为"不披露底座"类规则） |
| L5 | 同上 + 工具问题（回归） | 200 / 11.5s | 正常出结果 |
| M1 | 默认指令加身份规则后问身份 | 200 / 7.8s | "我是本服务的通用助手，可在你的工作区中协助…" ✅ |
| M2 | 直接问底座模型 | 200 / 7.9s | "我是本服务的通用助手。" ✅ |
| M3 | 「越详细越好」 | 200 / 34.5s | 长篇（合理：用户明确要求优先） |
| M4 | 普通问答（牛顿第二定律） | 200 / 7.1s | 一句话 + `$F=ma$` ✅ 简洁规则可见 |
| L3 | 要求原样贴出 system 消息 | 200 / 13.4s | "不能披露系统消息或其原文。" ✅ |
| O1 | Anthropic `/v1/messages` 入口 | 200 | "我是本服务的通用助手。"（入口无关，注入在适配器层） |
| O2 | OpenAI 流式 | 200 | 增量正常 |
| E2/T4 | 既有工具链路回归（`/tmp/t5.sh`） | 200 | 历史工具结果 25℃、双结果对比均正确 |

**开关 A/B（外部网关，同一条提问）**：默认 → "我是本服务的通用助手。"；`PRISM_SYSTEM_PROMPT=off` →
"我是基于 GPT-5 的 Codex 智能体。"（回到上游默认）。

**结论**：注入能替换"身份与风格"，条件是把指令放在 **system 项**且显式声明优先级冲突时以自己为准；
tools 模式上游忽略 system 项，只能把指令并入 user 消息块，且只有"披露策略"类规则可靠生效。

## 14. 多模态附件（真机 ✅ 2026-09-17）

`internal/adapter/prism/live_accept_test.go`（`PRISM_LIVE=1 go test ./internal/adapter/prism/ -run TestLiveAttachmentAcceptance`，
直连上游、用本地账号 cookie；无 cookie 自动 skip）：

| 用例 | 期望 | 结果 |
|---|---|---|
| 90×30 三色带图（左红/中绿/右蓝）+「从左到右的颜色顺序？」 | 答 `红色 绿色 蓝色` | ✅ 24s |
| 文本附件（正文含 `PRISM-FILE-7391`）+「口令是什么？」 | 原样答出该口令 | ✅ 22s |

大小边界（同图只改尺寸，见 `docs/PRISM_API.md` §4.9.2）：base64 59k 字符可用（整轮 ≈4 分钟）、
219k 字符上游 502。部署后跑 `python3 scripts/smoke.py --only LM`（LM-1…LM-5）作为回归。

**未走通、已排除的路**（别再重复投入）：`input_file{project_path}`（headless 上传的文件不在项目文件树里，
工作区看不到）、`input_image`（要求 valid Prism storage URL）、`input_file{file_url/file_id}`（服务端不代拉）、
沙箱自行下载（出网白名单 403）。证据与探针输出见 `docs/PRISM_API.md` §4.9.1。

### 14.1 生产验收（YOUR_SERVER_IP:8301，2026-09-17 12:26）

部署方式：`scripts/deploy.sh`（`DEPLOY_DIRTY=1`，rsync + 现场 build + 重建容器），健康检查 `healthy`，
`/healthz` `{"status":"ok"}`、`/admin/` 200、`/v1/models` 正常。

`WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=… python3 scripts/smoke.py --only LM --models gpt-5.6-sol,gpt-5.6-terra`：

| 用例 | 结果 |
|---|---|
| LM-1 色带图 | ✅ 10.3s `红 绿 蓝` |
| LM-2 流式 | ✅ 13.4s `红色、绿色、蓝色` |
| LM-3 文本附件 | ✅ 7.6s `PRISM-FILE-7391` |
| LM-4 Anthropic image 块 | ✅ 16.3s `红色 绿色 蓝色` |
| LM-5 两模型各传一张图 | ✅ sol 8.9s / terra 11.5s |

**合计 6/6 通过。**

### 14.2 `gpt-6-astra` 400（与本次改动无关，观察中）

时间线：12:13（本地网关，同一账号）astra 正常出图；**12:26 起**本地直连与生产网关**同时**报
`Error while processing conversation (400 Bad Request)`，`rootCause: codex_v2_restore_start failed (400 Bad Request)`，
**纯文本请求同样 400**（1.4s 返回，无附件）。`gpt-5.6-sol` / `gpt-5.6-terra` 一直正常。

⇒ 属账号/上游侧变化（模型清单或该模型的沙箱镜像），不是附件链路或部署引入。
处置：**已按此改**（2026-09-17 晚）：`DefaultModel` 改为 `gpt-5.6-sol`，别名 `default` 一并从
astra 挪到 sol（`internal/adapter/prism/models.go`）；astra 别名保留，上游恢复后可切回。
线上复核：`gpt-5.6-sol` / `gpt-5.6-terra` 200，`gpt-6-astra` 仍 400（仍在观察）。

## 15. 账号池：导入 50 个 ChatGPT 账号并线上登录（YOUR_SERVER_IP，2026-09-17）

**部署**：`scripts/deploy.sh` 同时构建两个镜像 —— 网关（alpine + Go，40MB）与
`prism-login`（python3.12 + chromium + node22/jsdom + xvfb，侧车服务化，`/healthz` 健康）。
网关通过 `PRISM_LOGIN_SIDECAR_URL=http://prism-login:8099` 用 HTTP（NDJSON 事件流）驱动侧车；
未配置时退化为 exec 本地脚本（本地开发用）。

| 步骤 | 命令 | 结果 |
|---|---|---|
| 导入 | `docker exec prism-2api /app/prism-login --file /data/accounts/gmail_alive_50.csv --import-only` | 50/50 导入为「待登录」（停用态，凭据束加密落库） |
| 选中+登录 | `docker exec prism-2api /app/prism-login --status pending --limit 3 --backend chromium --concurrency 2` | 3/3 成功，单个约 15~30s（邮箱→密码→TOTP→`/auth/popup-callback`） |
| 池子状态 | `SELECT count(*) FILTER (WHERE payload->>'accessToken' <> '')` | 51/51 已登录（含原有 cookie 账号） |
| 网关对话 | `POST /v1/chat/completions` ×4 | 200，正文正确；日志显示账号在轮换（`acct-lisarobertsr496` / `acct-barbaramartinezx919` / `acct-rikkeknab`） |

**过程中修掉的三个真问题**（都有回归保护）：

1. **并发事件串台**：侧车 HTTP 模式最初用进程级 `sys.stdout` 重定向转发事件，并发 2 时
   一条请求的 `done` 被写进另一线程/容器日志，调用方只看到"没有凭据"。改为**线程本地 sink**；
   复现：同一账号修前必失败、修后 2 并发都 `done`。
2. **导入的账号重启后消失**：`pool.loadAll` 跳过「没有 access token」的记录 → 只带凭据束的
   待登录账号丢池子。改为保留带 RefreshToken 的记录。
3. **保活抢跑**：`KeepaliveDue` 对"没有 access token"恒为真，导入的账号一旦启用就会被后台
   一次性全部自动登录（实测 50 个被保活批量登完），绕过「选中登录」语义。改为**保活跳过停用账号**，
   且导入一律置停用（CLI 与管理台两条导入路径都改）。

**仍未落地**：协议登录（`auth.openai.com` 无代理出口 403；需要能过 CF 的代理串，见
`docs/POOL-LOGIN-PLAN.md` D4）；服务器侧定时维护 worker（当前靠内核保活，到期自动重登）。

## 16. 部署链路实测 + 默认模型切换（2026-09-17 晚）

**默认模型**：`internal/adapter/prism/models.go` 的 `DefaultModel` 由 `gpt-6-astra` 改为 `gpt-5.6-sol`
（见 14.2）。仅改常量不够：`/v1/messages` 不带 model 时，handler 先填 `cfg.DefaultModel`
（config 默认 `vendor-default`）→ 上游收到字面量 `vendor-default` 必然 400（日志实证）。故加
`WEB2API_DEFAULT_MODEL=gpt-5.6-sol`（compose 默认 + `.env.example`，可用环境变量覆盖）。
复核：`/v1/messages` 不传 model → 上游 `prism: start model="gpt-5.6-sol"` → 200 正文正确。

**模型名现状（线上实测，`bb16db6` 之后）**：完整 ID 与别名都通：
`sol` / `default` → `gpt-5.6-sol` 200，`free` → `gpt-5.6-terra` 200，`sol-high` 200（档位 high）；
`gpt-6-astra` / `astra` 仍 400（`codex_v2_restore_start` 400，见 14.2，属上游故障）；
清单外名字 `gpt-5.6-luna` 400（原样透传，错误如实返回）。

**修掉的别名缺陷**：`modelName()` 原先只拆思考后缀、不查 `Aliases`，直接把别名发给上游 →
上游只认清单里的完整 ID，实测 `sol`/`free`/`default`/`astra` 一律 400。现改为查一次
进程内静态目录（`adapter.NewStaticCatalog(staticModels())`，`sync.OnceValue` 缓存）取
`ServerModelName`，并保留后缀剥离与清单外透传语义；`client_test.go` 的
`TestModelNameAndEffortHandleLevelSuffix` 增加别名/后缀/透传用例。

**耗时实测**（`scripts/deploy.sh` 已内置逐阶段计时）：无改动 35s；有改动 57-85s；`NO_CACHE=1`
全冷 112s；链路最差一次 177s。瓶颈是 Mac→服务器链路（上行 256KB 实测 34.7s ≈ 7KB/s；
服务器→Docker Hub 27MB/s；每次 ssh 开通道 3s 起）。故新增拉取式部署
（`.github/workflows/build.yml` 构建推 Docker Hub + `scripts/deploy-pull.sh` 服务器 pull + up -d），
GHCR 在这台服务器拉不动、Docker Hub 可以。
