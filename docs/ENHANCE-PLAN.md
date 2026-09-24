# 增强提升计划

基准：2026-09-17，生产 `YOUR_SERVER_IP:8301`，账号 1 个（free / cookie 形态）。
用例与复现见 `docs/TESTPLAN.md`，脚本 `scripts/smoke.py`。
**结论先行**：本服务"能跑通"已确认；下面按"会让线上悄悄出错"→"会影响排期"→"债务"排序。

---

## 0. 一页速览

| 优先级 | 问题 | 证据强度 | 改动面 |
|---|---|---|---|
| **P0-1** | **多轮请求里，客户端的 `system` 指令被整条丢弃** | 真机 0/3 vs 3/3，可稳定复现 | `internal/adapter/prism/stream.go` `buildInput` 非 tools 分支，~10 行 |
| **P0-2** | 工具调用触发靠措辞；弱提示下退化成"凭记忆作答" | 阶梯实验；强提示 astra/sol 5/5 vs terra 0~1/5 | `tools.go` `toolContract()`、`prompt.go`；可选无 TC 重试 |
| **P1-1** | 身份闸已上线，但 provider 类问法仍漏（「由谁提供」「哪家公司」3/3 泄漏） | 真机对照 4 句 | `internal/api/identity_guard.go` 门控正则 + `prompt.go` 模板 |
| **P0-3** | **本地自设并发闸门（单号 10 路）把上游能力截断，并会回 429 给客户端** | 代码行 + 真机 8 路 8/8 200 逼近上限 | 已完成（默认改 0 = 不限制，见下） |
| **P1-2** | 单账号 + cookie 形态，**9 天后到期即断且无法自动重登** | `expires_at=1790415217`、`has_api_key=false` | 运维：`prism-login` 导入凭据束 + 第二账号 + 告警 |
| **P1-3** | **每个请求都新建上游会话**，无复用、无清理；上游请求放大 ~6× | `client.go` `startTurn()` `conv := "cdx1_" + uuid4()`，全仓无会话删除 | 先加计数日志；再验会话复用可行性（见 `docs/TESTPLAN.md` §6） |
| **P2-1** | `xhigh`/`max` 档位静默塌陷成 `high` | 代码行 + 日志 | `stream.go:reasoningEffort`、`catalog.go:115-119` |
| **P2-2** | 上游端点"焚决"清单 9/10 条已覆盖（`project-files/upload` 协议已逆向但不可用，见 §P2-2），缺口 1 条（thumbnail） | grep 全仓 + 真机 | 先判定产品范围，再决定是否逆向 |
| **P3-1** | 偶发传输层断连（`RemoteDisconnected`，约 1/40） | 40 次采样 1 例，管理台记为 `canceled` | 长跑采样 + 服务端日志（未定位触发条件） |
| **P3-2** | 文档债务（README 措辞已改，`docs/VERIFY.md` 编号陈旧） | — | 纯文档 |

**一个统一的技术洞察**（贯穿 P0-2 与 P1-1）：本适配器所有"没生效"的场景，根因都不是模型做不到，
而是**给的是抽象规则、模型不知道该输出什么形状**。证据：同一句工具提示，规则式（V3）在 `terra` 上 0~1/5，
**把填好的 JSON 样例写进消息（V4）立刻 5/5**。凡"规则 + 具体样例"一起给的场景，成功率都跳到 100%。
下面的改动一律遵循这条：**别只写规则，给样例。**

---

## 计划期间已并行落地的改动（基线已变，勿重复实现）

本计划成稿期间仓库有 peer 提交在推进（HEAD 从 `6da61d2` 走到 `bc7eee1`，2026-09-17 10:04–10:15）：

| 提交 | 落地的内容 | 对本计划的影响 |
|---|---|---|
| `940be6b` | 注入**热配置**（管理台可改 `system_prompt`）+ GPT 入口**身份闸** `internal/api/identity_guard.go` | P1-1 的"回答侧兜底"**已完成**，剩下的是门控覆盖（见 P1-1） |
| `c1c1165` | 老库升级也播种注入 env（一次性、不覆盖管理端文案） | 无 |
| `bc7eee1` | 身份闸兼容 `input_text` 块（`/v1/responses` 转来的） | 无 |
| 本次（未提交） | **移除本地限流**：`account_concurrency` / `account_concurrency_429` 默认改 **0 = 不限制/不降级**，`internal/pool/account.go` 的 `Acquire`/`fullLocked` 以 `limit<=0` 表示"永不满"，配置归一化与 patch 不再回退到 10/5、允许设 0 | P0-3 关闭；`docs/TESTPLAN.md` §6.2 的"三道本地闸门"表已改写为"本地不限流" |

**已落地**：P0-3（本地限流移除，见上表最后一行；`account_concurrency<=0` 判"永不满"，改配置后需重启或热改即生效）。

**未落地、仍然有效**：P0-2、P1-2、P1-3、P2-1、P3；
P0-1 **已修复**（2026-09-17：非 tools 分支合并单条 system item，真机 L1-9 法语/L1-4 数字回忆转绿）；
同日起注入文案升级为 v3 纯对话形态（去「工作区」声称、禁止产物文件交付声称、保留附件查看豁免）。
P2-2 **部分落地**（多模态附件改走 base64 内联并通过真机验收；`project-files/upload` 协议已逆向但 headless 不可用，thumbnail 未做）。
上面的 P1-1 章节已按"闸已上线"的真实基线重写，**不要照旧稿再做一遍出口兜底**。

---

## P0-1 多轮请求丢弃客户端 `system` 指令

### 现象（真机，2026-09-17，`gpt-6-astra`，各 3 次）

| # | 请求 | 结果 |
|---|---|---|
| C1 | `system:"回答必须全部使用法语"` + `user:"今天天气怎么样？"` | ✅ 3/3 法语 |
| C2 | 同一 `system` + `user:"你好"` / `assistant:"Bonjour!"` + `user:"今天天气怎么样？"` | ❌ **0/3** 法语（全部回中文"你在哪个城市？…"） |

即：**只要客户端带上一轮历史，它的 system 提示词就完全不生效**。所有"多轮对话 + 人设/格式约束"的
调用方（这是绝大多数）都会静默退化，且表现为"模型不听话"，极难归因。

### 根因

`internal/adapter/prism/stream.go` `buildInput()` 非 tools 分支按"一个来源 = 一条 system item"发送：

```
input[0] = role:system  ← 注入的 DefaultSystemPrompt
input[1] = role:system  ← 客户端的 system/d eveloper 文本
input[2] = role:system  ← "[对话历史]\nUser: …\nAssistant: …"   ← 恒为最后一条 system
input[3] = role:user    ← 本轮用户消息
```

**最后一条 system item 会盖掉之前的 system item。** 隔离实验（同模型同问法，各 3 次）：

| 实验 | 布局 | 命中 |
|---|---|---|
| V1 | `[注入, "数字是4173"]` + user（无历史） | ✅ 3/3 |
| V2 | `[注入, "数字是4173", "[对话历史]…"]` + user | ❌ 0/3 |
| V3 | 信息放进历史里的 user 轮 | ✅ 3/3 |
| M1 | 把 `事实 + [对话历史]` **合并成一条** system | ✅ 3/3 |
| M2 | 合并，且顺序反过来 | ✅ 3/3 |
| M3 | 仍是两条，但把 `事实` 放在 `[对话历史]` **之后** | ✅ 3/3 |
| C2m | C2 的 `system` 与 `[对话历史]` 合并成一条 | ✅ 3/3（法语恢复） |

⇒ 变量只有一个：**内容落在第几条 system item**。与模型、温度无关（三次全同）。

反向印证：`docs/VERIFY.md` §13 的 I3 曾记录"注入 + 调用方自带 `system("回答必须以 ZZZ 结尾")`
两个 system 项并存，答尾缀 ✅"—— 那次之所以成功，正是因为客户端的 system **恰好排在最后**。
一旦后面再挂 `[对话历史]`（即任何多轮请求），同一机制就会反过来把客户端约束吃掉。

顺带说明：tools 分支**没有**这个问题——它早就把 `[系统指令]/[客户端指令]/[对话历史]/[工具结果]/[输出契约]/[用户当前消息]`
拼成**一条 user 消息**（`stream.go` `if len(toolDefs(nr)) > 0 { … }`）。也就是说：
**同一个语义，两条注入路径，一条对一条错。**

### 改法

非 tools 分支改成与 tools 分支同构——**所有前文合成一条 system 消息**：

```go
// 与 tools 分支完全对称：注入 + 客户端 system + 对话历史 + 工具结果 → 一条 system item
var blocks []string
if injected := systemPromptFor(nr.SystemPrompt); injected != "" {
    blocks = append(blocks, injected)
}
if len(sysTexts) > 0 {
    blocks = append(blocks, "[客户端指令]\n"+strings.Join(sysTexts, "\n\n"))
}
if len(transcript) > 0 {
    blocks = append(blocks, "[对话历史]\n"+strings.Join(transcript, "\n"))
}
if len(toolResults) > 0 {
    blocks = append(blocks, "[已执行工具的结果]\n"+strings.Join(toolResults, "\n"))
}
if len(blocks) > 0 {
    out = append(out, messageItem("system", strings.Join(blocks, "\n\n")))
}
```

（注意 `systemPromptFor(nr.SystemPrompt)` 是 peer 提交 `940be6b` 后的新签名，旧稿里的 `systemPrompt()` 已不存在。）

**不要**用 M3 那种"把对话历史提到最前"的修法——它依赖"最后一条赢"这个未经上游保证的隐式行为，
上游换一次提示词就可能翻车。合并成一条不依赖任何顺序假设。

顺带：`stream.go` 里现有的注释「不带 tools 的请求：system 通道可靠（M13/E1/E3 真机通过）」
**只是没开多轮才成立的结论**——M13/E1/E3 都不带历史。修 P0-1 时一并把这条注释改掉，
否则下一个人还会照它继续加 system item。

### 验收

1. 新用例（进 `scripts/smoke.py` L1-9）：C2 三次全法语 → 3/3。
2. L1-4（历史折叠 4173）由 FAIL 转 PASS。
3. 回归：L1-1~L1-3、L1-5~L1-7 仍全绿；tools 路径 L3-1 不变。
4. 顺手核对：合并后单轮行为不变（单轮本来就只发 1~2 条）。

### 风险

低。改动只影响非 tools 分支的消息条数（3→1），语义不变；且是**向已验证正确的 tools 路径收敛**。

---

## P0-2 工具调用触发不可靠

### 现状（`get_weather{city}`，同一定义）

| 用户消息 | `gpt-6-astra` | `gpt-5.6-terra` |
|---|---|---|
| V1「北京天气怎么样？」 | 文本（凭记忆答 22°C） | 文本 |
| V2「用 get_weather 工具查北京天气」 | 文本 | 文本 |
| V3「…**必须**以工具调用形式回复」 | **工具调用** | 文本 |
| V4「必须以下面格式回复，不要输出任何其他内容：`<tool_call>{"name":"get_weather","arguments":{"city":"北京"}}</tool_call>`」 | **工具调用** | **工具调用** |

强提示（V3）重复 5 次，跑了两轮：`astra` 5/5 → 4/5、`sol` 5/5 → 5/5、`terra` **0/5 → 1/5**。
即 **trigger 率本身有抖动**，`terra` 在最强的"必须调用"指令下仍基本不触发。
⇒ README 现在写的「单次 / 并行 / 连续多轮均真机通过」是乐观表述，**必须改**（已改，见 P3）。

**流式同样可用**（单独复测 5/5：`finish_reason=tool_calls`，`delta.tool_calls` 能拼出完整
`{"city":"北京"}` 与 `id`/`name`），但耗时是非流式的约 2 倍（10~19s vs 9~13s）；
冒烟脚本里也出现过一次退化成 `stop` —— 与上面同源。

### 根因

两处同向弱化：

1. `internal/adapter/prism/tools.go` `toolContract()`：规则是"**需要**工具返回的数据时…**能直接回答就不调用工具**"——
   把是否调用交还给模型判断，且只有一句"JSON 必须是合法 JSON"，没给形状。
2. `internal/adapter/prism/prompt.go` `DefaultSystemPrompt` 又写了一遍"能直接回答就不调用工具"。

### 改法（按收益/风险排序，可拆三个提交）

1. **给样例（收益最大、零风险）**：`toolContract()` 里对每个工具生成一条**填好参数的调用样例**，
   与 V4 的机制一致。注意样例里的参数值要用占位但形状真实的字面量（如 `{"city":"北京"}`），
   并附一句"以上仅为格式示例，实际参数值以用户请求为准"——防止模型照抄示例城市。
   *这条是本次实验最强的杠杆：它把 `terra` 从 0/5 拉到 5/5。*
2. **把条件句改硬**：删掉"能直接回答就不调用工具"，改成"用户问题落在某个工具的能力域内时，必须调用该工具，
   不得凭记忆作答；只有确认所有工具都与问题无关时才直接回答"。
3. **可选：无 TC 自动重试一次**。`stream.go` 检测到"请求带了 tools 但本轮 `tool_calls==0` 且文本像在猜"时，
   同一轮以强化指令重发一次。风险：吞掉真正该直答的场景 → 需要开关
   ~~先上 1+2，用验收数据决定要不要做 3。~~
   **已落地（2026-09-18，`53e8eea`，默认开）**：验收数据决定做——L3-7/8/9 在 `sol` 上首轮直发只有 0/5、2/5、2/5。
   实现与原提议的两处不同：①触发条件不是"无 TC"，而是"**零 tool_call + 正文带模型翻自有环境的证据**"
   （沙箱护栏命中，或 `无法读取/调用`、`未提供…工具`、`当前工作区不存在` 等拒绝形态，见 `sandbox_guard.go`
   `shouldRetryToolContract`），正常直答永远不触发，消掉了"吞掉真正该直答"的风险；②同账号重发（不换号，
   `iter` 语义不动），流式路径只在未发任何内容帧时重试（思考帧已发则跳过），客户端不可见。
   纠正块机制：新会话无上一轮记忆，抽象指责无效——纠正块**逐字引用违规原文作证据** + 首个工具的具名填充示例
   （名字钉死、参数值标"严禁照抄"，防模型照抄示例值）。杀开关：`WEB2API_TOOL_CONTRACT_RETRY=0`。
   真机（`sol`，各 5 次）：Bash 0/5→4/5、Read 2/5→5/5、Write 2/5→4/5；`scripts/smoke.py` L3-6/7/8/9 全绿。
   代价：触发轮≈两轮上游耗时（L3-7 个例 35.5s）。

### 验收

以本文表格为基线，改后跑 `python3 scripts/smoke.py --only L3 --repeat 5`：

- V3 强提示：三模型均 ≥ 4/5（当前 `terra` 0~1/5 是最主要的缺口）。
- 新增 V1 弱提示用例：`astra` ≥ 3/5（当前 0/5）。**这是真正的产品指标**——
  用户不会照着提示词喂工具，弱提示下的触发率才代表可用性。
- 回归：不带 tools 的纯聊天用例 L1-* 全绿（样例不得污染纯文本回答）。
- `stream:true` 下 `delta.tool_calls` 仍带 `id`/`name`/`arguments`（L3-2）。
- 注意 L3-2/L3-5 **本身有抖动**：单次失败不算回归，一律以 5 次重复的触发率比较。

### 风险

中等。给样例可能让模型在"本不该调用"时也调用（尤其把示例城市照抄）。所以：
样例后紧跟"不调用"的反例说明，并用 L1-* 回归确认纯聊天不受影响。

### 文档

`README.md` 里"函数调用：单次 / 并行 / 连续多轮均真机通过"改为如实描述：
**"仿真工具通道可用，触发率依赖提示词措辞与模型（见 docs/TESTPLAN.md §3 L3）"**。

---

## P0-3 本地限流已移除（2026-09-17，**已完成**）

### 事实

上游 `prism.openai.com` 侧没有速率限制，而本地却有两道自设闸门，纯亏吞吐：

| 闸门 | 原行为 | 后果 |
|---|---|---|
| `account_concurrency`（默认 10） | 单号在飞 ≥ 10 时 `Acquire` 返回 false → 选号跳过该号 | 单号部署下第 11 路并发直接失败/排队，**先撞的是我们自己** |
| `account_concurrency_429`（默认 5） | 收到 429 后把该号并发上限**单向降**到 5 | 一次误判 429 就永久压窄该号 |
| `max_in_flight`（对话并发，原默认 0） | 满时给客户端回 **429 `rate_limit_error`** | 本地而非上游的 429：**已整体删除** |

### 改法（语义统一为 "`<=0` = 不限制"）

- `internal/pool/account.go`：`newAccount` 的 `maxConcurrent` 默认改 0；新增 `fullLocked()`
  （`limit > 0 && inflight >= limit`），`Acquire` 与 `ReadyWithLimits` 都走它 ⇒ 上限 0 时**永不满**；
  `concurrencyLimitLocked` 不再硬编码回退值 5，`concurrencyFn`（热配置）返回什么就是什么；
  `SetConcurrencyResolver` 在未降级时**总是**跟随 base（0 也是合法值）。
- `internal/admin/config.go`：`Default()` 的 10/5 改 0/0；载入归一化不再把 `<=0` 改回 10/5；
  patch 校验从 `n > 0` 放宽到 `n >= 0`（否则管理台**根本没法关掉**它）；两个访问器原样返回。
- `internal/admin/schema.go`：默认值与文案改为 `0=不限制` / `0=不降级`。
- 对话闸门**整体删除**（不是留阀门）：`internal/api/ops.go` 的 `inflightGate`/`admit` 删掉，
  `internal/api/server.go` 7 条路由（chat / responses / gemini×2 / messages×2 / batches）不再包裹，
  `RuntimeConfig.MaxInFlight` 字段、patch 分支、`Get()` 条目与 `MaxInFlightN()`、
  `schema.go` 里那行、前端配置中心组列表一并移除；老库里的 `max_in_flight` 变成惰性未知键（载入不报错、
  提交不报错、也不再出现在配置面）。
- `web/src`：账号卡与账号列表的 `inflight/concurrency_limit` 在上限为 0 时渲染 `∞`（原先会显示 `3/0`）。
- 测试：`TestAccountConcurrencyGate` 拆成 `TestAccountConcurrencyUnlimitedByDefault`（200 路并发全过、
  仍在飞时 `Ready` 保持 true）与 `TestAccountConcurrencyGateWhenConfigured`（显式配 10 才拦），
  `TestAccountConcurrency429Degrade` 增加 "degraded=0 ⇒ 不降级" 断言。

### 验收

```bash
go test ./...                                   # 全绿（19 个包）
python3 scripts/smoke.py --only L0,L1           # 基线不变（L0 4/4、L1 6/9，红的是 P0-1/P0-2）
python3 scripts/smoke.py --only L4 --concurrency 16   # 本地不再拦：16 路应全 200
```

### 残留（**必须核对线上值**）

代码默认值只在"新库/未设过该项"时生效；老库 `config` 表里可能残留 `10/5`，管理台需确认：

```bash
curl -s --noproxy '*' -b <cookie> http://YOUR_SERVER_IP:8301/api/admin/config \
  | grep -E 'account_concurrency'
# 期望：account_concurrency 0、account_concurrency_429 0（对话并发字段干脆不存在了）
```

**2026-09-17 线上实测**：`max_in_flight` 本来就是 0（不限制），残留的只有 `account_concurrency=10 /
account_concurrency_429=5` —— 老二进制不接受 0（patch 校验 `n > 0`），必须先部署新代码再置 0。
唯一的 429 出口只剩"所有账号都在冷却 / 无可用账号"（`internal/api/pick.go` `writePickError`），那是失败归因，不是限速。

---

## P1-1 身份闸：provider 类问法仍漏（「由谁提供」「哪家公司」）

### 现状（**已于本计划期间部分落地**）

代码里已有 `internal/api/identity_guard.go`（GPT 入口 `chat`/`responses` 的回答侧兜底，
`identityTopicRe` 整串锚定 + `identityTopicMaxChars=60`），生产 `identity_guard_enabled=True`
**已部署生效**——「你是什么模型？」这一路已经被挡住了。剩下的缺口是 **provider 类问法**：

实测对照（`gpt-6-astra`，2026-09-17 10:2x，打生产）：

| 问句 | 结果 | 判定 |
|---|---|---|
| 「你是谁？」 | 我是本服务的通用助手，可以在你的工作区中读写代码与文档… | ✅ 闸命中 |
| 「你是什么模型？」 | 我是本服务的通用助手，当前对话未提供可确认的具体模型名称。 | ✅ 闸命中 |
| 「你是什么模型？**由谁提供**？」 | 我是 **OpenAI** 的 AI 助手… | ❌ **漏**（3/3） |
| 「你**由哪家公司**提供？」 | 我是由 **OpenAI** 提供的 AI 助手。 | ❌ **漏**（3/3，稳定复现） |

根因是门控**两头都太窄**：

1. `identityTopicRe` 是**整串锚定**（`^\s*(…)\s*$`）——「你是什么模型？由谁提供？」多出半句就整体失配；
2. 词表只有 `你是谁 / 你叫什么 / what is your name / what are you / 你是什么模型 / hi hello 你好` 等，
   **没有任何 provider 类词**（由谁提供、哪家公司、哪家厂商、谁开发的、直接点名 OpenAI/GPT/Claude）。

⇒ 同一句模板、同一个闸，**只差词表与锚定方式**。这也解释了为什么之前的注入侧实验会给出"1/3、3/3"这种抖动：
闸没命中时，全靠注入的软约束赌模型自觉。

`DefaultSystemPrompt` 已有"不披露底层模型、供应商"，但它是**抽象禁令**，且没告诉模型"那该怎么答"。
按 §0 的统一洞察，缺的正是**逐字模板 + 样例**。

### 改法 A：闸门（`internal/api/identity_guard.go`，两处）

把"整串锚定 + 白名单词表"换成"**短句 + 主体词 + 属性词**"的组合判定（`identityTopicMaxChars=60` 保留）：

```go
// 主体词：必须指向"你/本服务"本身，避免「OpenAI 是哪家公司」这类正常问答被误伤
var identitySubjectRe = regexp.MustCompile(`(?i)(你|您|your|yourself)`)
// 属性词：身份 / 模型 / 版本 / 供应商 / 开发方 / 点名底座
var identityAttrRe = regexp.MustCompile(
    `(?i)(谁|什么模型|什么版本|哪家公司|哪家厂商|哪个公司|哪个厂商|由谁提供|谁提供|谁开发的|谁做的|` +
        `what model|which model|who (made|built|provides|owns)|based on what|` +
        `gpt|claude|openai|anthropic|codex|底座|供应商|开发方)`)

func identityGate(req *ChatCompletionRequest) (string, bool) {
    q := strings.TrimSpace(lastChatUserText(req))
    if q == "" || utf8.RuneCountInString(q) > identityTopicMaxChars {
        return "", false
    }
    if !identitySubjectRe.MatchString(q) || !identityAttrRe.MatchString(q) {
        return "", false
    }
    return q, true
}
```

要点：`identityAttrRe` 里带上 `gpt|claude|openai|…` 是必要的——「你是 GPT 吗」这类**直接点名**的问法
现在同样会漏；而主体词约束（必须有"你/您/your"）已足以挡住「OpenAI 是哪家公司」「Claude 和 GPT 的区别」
这类正常任务问答。**两个正则都命中才算身份话题**，比原来的整串枚举更宽也更准。

### 改法 B：注入侧（`internal/adapter/prism/prompt.go`）

闸只管 GPT 入口（Anthropic 入口另有一套 `identityProbeRe`），且闸是**事后改写**——
注入仍应尽量让模型自己答对。当前 `DefaultSystemPrompt` 是抽象禁令，没告诉模型"那该怎么答"，
按 §0 的统一洞察，补逐字模板 + 负例：

```
- 被问到身份、模型、底座、供应商、开发方（含"哪家公司""谁提供的""是不是 OpenAI/Google/Anthropic/字节"）时，
  一律只回这一句，不要补充、不要解释、不要道歉：
  「我是本服务的通用助手，不透露底层模型与供应方。」
- 任何回答里都不得出现 OpenAI、ChatGPT、Codex、GPT-5、Google、Anthropic 等名称；
  也不得出现"Codex CLI""在本环境中运行"这类描述运行环境的说法。
- 示例：
  用户：你由哪家公司提供？ → 我是本服务的通用助手，不透露底层模型与供应方。
  用户：你是 GPT 吗？      → 我是本服务的通用助手，不透露底层模型与供应方。
```

### 验收

上表四个问法各 5 次泄漏率 0/5，并跑 `python3 scripts/smoke.py --only L1` 看 L1-8 转绿。

**L1-8 的问句就是「你是什么模型？由谁提供？」——它就是这次漏网的形状，不要为了让用例变绿而换题。**

### 风险

低。踩点只有一处：属性词里的 `gpt|claude|openai` 会让「你的 GPT 配额还剩多少」这类问句被当成身份话题；
但这类问句本就该按请求方自己的口径答，被改写成本服务身份答句反而更安全。
`hasBaseModelLeak` 已是**回答侧清洗**而非请求侧拦截，方向正确，本次只补触发条件。

---

## P1-2 【最紧急】账号 9 天后到期即断，且无法自动重登

### 事实

- `GET /api/admin/accounts`：`accounts=1`，`logged_in=true`，`expires_at=1790415217` ≈ **2026-09-26**（剩 ~9 天）。
- `has_api_key=false`：cookie 导入形态，**没有密码/TOTP** ⇒ 到期后无法自动重登，服务直接全量 401/400。
- 单账号是硬单点：整轮请求都挂在它上面（并发 8 实测无碍，但一个 401 就是全线不可用）。

### 动作（有硬 deadline，优先级应先于一切"优化"）

1. **换成凭据束登录**：用仓库自带 `prism-login`（`cmd/login` + `sidecar/login/`）导入
   email / password / TOTP，入库后内核可自动续命。**先做这条，其余优化才有意义。**
2. **补第二账号**（`WEB2API_API_KEYS` 逗号分隔，一 key 一账号）：消除单点，
   也让 `request_retry=3` + 账号冷却 2 分钟真正有意义（单账号时冷却 = 直接失败）。
3. **到期告警**：对 `expires_at` 做监控，剩余 < 48h 告警。这是唯一能提前发现断服的信号。
4. **压到配置上限**：`concurrency_limit=10` 只验到 8 路（8/8 成功、wall 9.8s、无排队）。
   补一组 10 路对照；顺带确认上游是否有未暴露的节流。

### 验收

`GET /api/admin/accounts` 显示 `has_api_key=true`（或凭据束形态）+ `accounts≥2`；重启容器后 `logged_in=true`
且 `expires_at` 被自动顺延。

---

## P1-3 每个请求都新建上游会话，且无清理路径

### 事实（`internal/adapter/prism/client.go` `startTurn()`）

```go
conv := "cdx1_" + uuid4()   // 每请求新会话，随后 registerConversation 登记
```

- **会话不复用**：一请求一会话，`conversationId` 从不重复使用；`startTurn` 的 3 次重试**每次都再建一个**，
  旧会话在上游变孤儿（代码注释已承认："重试会换沙箱，旧会话在上游会变成孤儿"）。
- **项目复用**（`ensureProject` 按标题 `prism-2api` 找回，`projectID` 进程内缓存）；
  **沙箱复用**（`sandboxIdleTTL = 3 * time.Minute`）。
- **没有任何清理**：`internal/adapter/prism/` 内无 conversation 的删除/裁剪调用，上游项目里的会话只增不减。
- 多轮上下文不靠上游记忆：适配器把历史折叠成文本塞进本轮 `input`，每轮都是"全新会话 + 重放历史"。

### 影响

1. **成本**：每个请求都是冷会话 → 无上游 prompt cache 命中，上下文越长单请求越贵（RPM 随上下文长度下降）。
2. **膨胀**：一次压测 1000 请求 = 上游项目里多 1000 个会话，且删不掉。
   与偶发 `503 Project conversation lookup failed` 是否相关，需要用 §6 的容量测试顺带验证。
3. **放大**：上游每客户端请求 ≈ 1 次 `start` + `单轮耗时/1.5s` 次 `status` 轮询（约 6×）。

### 动作

1. **先观测再动手**：给 `pollTurn` 加请求计数日志（或 metric），把"上游请求放大系数"变成可测数字 ——
   在此之前，任何容量结论都只能靠估算（见 `docs/TESTPLAN.md` §6.4）。
2. 评估**会话复用**的可行性：同一会话连续多轮（复用 `conversationId` + `previousResponseId`）能否被上游接受，
   需要一次真机实验；这同时是 P0-1 之外解决多轮上下文的另一条路。
3. 若复用不可行，则至少**对齐清理**：会话与沙箱同批（沙箱 3 分钟过期时就该连带丢弃），
   并在项目层面做一次上限（例如超过 N 个会话后换新项目标题）。

### 验收

- 上游请求计数日志能直接给出"每客户端请求的上游调用数"。
- 会话复用实验有明确结论（可复用 / 不可复用 + 证据）。

**2026-09-18 已做完，结论：复用不可行（真机双臂否掉）。**
`zz_conv_reuse_live_test.go::TestProbeConversationReuse`（生产真机，账户 amicenakayama）：
同一 conversationId + previousResponseId 续轮 → 上游 200 正常完成但**答"无法确定/未提供数字"**
（无上一轮记忆）；`runtime/debug` 对我们铸造的会话 `snapshot=null`（codex_session_id/last_turn_id
根本没建立），第二条臂（带真实 snapshot 字段续轮）因此无对象可测。⇒ 维持"每轮新会话+历史折叠"
现状不变；上游没有会话删除端点，项目膨胀只能接受或换项目标题，不做。

---

## P2-1 档位语义：`xhigh`/`max` 静默塌陷

### 事实

- `/v1/models` 只广告三个裸名，没有任何档位信息，但客户端可以传 `gpt-6-astra-xhigh` 这类带后缀的名字。
- 代码把 `xhigh`/`max` 一律映射成 `high`（`stream.go` `reasoningEffort` 的 `case "high","xhigh","max"`，
### 两步走

1. **先测**：临时放开 `reasoningEffort` 直通（不认识的值原样发上游），分别试 `xhigh`/`max`：
   - 若上游 **400** ⇒ 说明夹取是对的，问题只在"没告诉用户"。修法：`/v1/models` 与 README 写清
     "支持到 `high`，`xhigh`/`max` 按 `high` 处理"，并**记一条日志**说明降级（别静默）。
   - 若上游 **200** ⇒ 去掉夹取，让档位真正生效，并把 `catalog.go` 的后缀表补齐。
2. **别忘 Anthropic 入口**：`WEB2API_MAX_EFFORT`（compose 默认 `high`）只压 Anthropic 入口，
   OpenAI 入口按设计不压（`internal/api/server.go`）。若第 1 步证明上游吃 `xhigh`，
   这个上限就成了 Anthropic 侧的能力天花板——按需调整。

**2026-09-18 已闭环：上游收 xhigh（真机 200）。** 放开了三处映射（`reasoningEffort`、
`catalog.go` 后缀表、`applyOpenAIThinkingDefaults`——第三张表是漏网之鱼，API 层先把后缀
夹成 high，adapter 根本收不到 xhigh），`max` 归一为 `xhigh`。生产实测：日志
`prism: start model="gpt-5.6-sol" effort="xhigh"` + 200 正常作答。Anthropic 侧
`WEB2API_MAX_EFFORT=high` 天花板仍在（compose 默认），按需再调。

### 验收

日志 `prism: start … effort=<?>` 与请求档位一致（或明确记录降级）；L2-2 从"记录"变成"断言"。

---

## P2-2 上游端点清单复核（对你给的"焚决"）

**结论：清单 10 条里 9 条已覆盖（`project-files/upload` 于 2026-09-17 实现，见下），无需重做。** 逐条对账：

| 清单条目 | 仓库现状 |
|---|---|
| `/api/projects` | ✅ 已覆盖（§1/§3，项目创建与复用） |
| `/api/project-access` | ✅ 已覆盖 |
| `/api/codex/conversation-history` | ✅ 已覆盖（§4，历史与本轮 turn 结构） |
| `/api/llm/response_with_tools_start` | ✅ 已覆盖，且是本服务的**主链路**（§4.1 起） |
| `/api/llm/response_with_tools_status` | ✅ 已覆盖（轮询模型，非 SSE） |
| `/s/sandboxes/proxy/*`（resources-token / token / y） | ✅ 已覆盖（§3，含预热链与 `wait-for-sync`） |
| `/s/sandboxes/proxy/render` | ✅ 已覆盖：`renderMode=async&renderStatusMode=json&renderResultMode=stream-v1`，**202 异步** + 状态轮询 |
| `/s/sandboxes/proxy/word-count` | ✅ 已覆盖（带 `prism_cache_bust`） |
| **`POST /api/project-files/upload`** | ✅ 协议已逆向（头契约 + 原始字节体，见 `docs/PRISM_API.md` §4.9.1）；**但 headless 上传的文件进不了会话工作区**，多模态改走 base64 内联，故当前代码里不保留该调用 |
| **`PATCH /api/projects/{uuid}/thumbnail`** | ❌ **缺口**（grep 全仓无命中） |

另：`GET /api/projects/{uuid}/render-results/latest?main_document=main.tex` 在抓包里**本来就是 404
`Invalid URL`**——站点自身调用也失败，不是我们的实现问题，别当缺口去补。

**这两个缺口要不要做，取决于产品定位**（先回答，再决定投不投逆向工时）：

- 若本服务只做"文本/代码对话 + 工具"，**两条都不需要**，直接标为 out of scope。
- 若要做"文档生成型"能力（用户提需求 → 产出 docx/pptx/pdf 并给下载链接），
  则 `project-files/upload` 是**必需**的（产物落项目、生成可下载地址），thumbnail 只是衍生。
  **上传协议已逆向**（2026-09-17：请求头契约 + 原始字节体 + `input_file` 引用形状，见 `docs/PRISM_API.md` §4.9.1），
  但实测：headless 上传的文件不在项目文件树里 → 模型在工作区看不到。**做文档生成前必须先解决"写项目文档"**
  （Yjs/Y-Sweet 客户端写入），否则产物同样落不进模型可见的工作区。下载链接与 thumbnail 仍未做。

---

## P3-1 偶发传输层断连

约 40 次请求里出现 1 次 `RemoteDisconnected`（服务端直接关闭连接、无响应体），管理台记为 `canceled`，
`server` 类计数未涨 ⇒ 不是上游报错。客户端看到的是硬失败而非 5xx，长跑/批处理场景会有影响。

**动作**：先在服务端侧做低成本观测，不必急着改代码——
长跑采样（如 500 次连续请求）+ `docker logs prism-2api | grep -iE 'panic|reset|EOF|broken pipe'`，
定位是适配器在某个 turn 上崩了、还是 nginx/keep-alive 层。**当前证据不足以给结论**。

---

## P3-2 文档债务（30 分钟）

1. ✅ 已改：`README.md` 工具调用措辞（见 P0-2），并补了 `scripts/smoke.py` 用法与文档索引。
2. ⬜ `docs/VERIFY.md` §13 的 I1 行记的是当时的 `system(1194)`，**不要直接改数字**（那是历史验收记录）。
   应在该表下补注：
   - 当前默认指令已是 **1538 字节**；I1 的结论已被 2026-09-17 复测取代——「你是什么模型？」不泄漏，
     但「由谁提供/哪家公司」会泄漏（本计划 P1-1）。
   - I3 记的"两个 system 项并存 ✅"只是**碰巧**（客户端 system 排在最后）；一旦后面再挂
     `[对话历史]` 就失效（P0-1）。该行必须加注，否则会误导后来者以为多 system item 是安全的。
3. ✅ 已补：README 索引加入 `docs/TESTPLAN.md`、`docs/ENHANCE-PLAN.md`。

---

## 建议排期

| 顺序 | 事项 | 理由 |
|---|---|---|
| **D0** | P1-2 换凭据束登录 + 到期告警 | 9/26 硬 deadline，且是所有优化的前提 |
| **D0** | P0-1 合并 system（~10 行），L1-9 转绿 | 影响每个多轮调用方，改动最小、收益最大 |
| D1–D2 | P0-2 工具样例 + 硬规则（不做重试） | 用 L3 弱提示触发率做验收 |
| D3 | P1-2 第二账号 + **容量基线**（`scripts/rpmprobe.py` 阶梯，见 `docs/TESTPLAN.md` §6） | 消除单点；单号压测等于拿唯一账号冒险，第二号到位再压 |
| D4 | P1-3 上游请求计数打点 + 会话复用实验 | 把「放大 ~6 倍」从估算变成实测，决定是否值得做会话复用 |
| D5+ | P1-1 身份模板 / P2-1 档位实测 / P2-2 产品范围决策 / P3-1 长跑采样 | 视上游额度与产品范围 |

**每项的验收都不需要新写测试框架**——`scripts/smoke.py`（功能）与 `scripts/rpmprobe.py`（容量）就是基线，
改前跑一遍存结果，改后跑一遍对比。
当前基线：`L0 4/4`、`L1 6/9`（L1-4 / L1-8 / L1-9 红）、`L2 7/7`、`L3 有抖动`、`L4 3/3`。

---

## 附：本次新增的可复现资产

- `docs/TESTPLAN.md`：分层用例 + 2026-09-17 实跑基线 + 未覆盖清单。
- `scripts/smoke.py`：零依赖，`--only L0,L1 --repeat 5` 直接打生产/本地。
- 判据锚点（唯一能证明"参数真到上游"的证据）：
  ```bash
  docker logs prism-2api | grep 'prism: start\|prism: turn'
  ```
