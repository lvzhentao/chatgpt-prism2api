# 测试计划（TESTPLAN）

目标：用可重跑的最小集合回答三个问题——**网关活着吗**、**核心对话链路对不对**、
**参数面与工具面有没有静默降级**。

配套脚本：`scripts/smoke.py`（零依赖，`python3 scripts/smoke.py --help`）。
本文记录的是 **2026-09-17 在 YOUR_SERVER_IP:8301 实跑的基线**，脚本可原样复现。

---

## 0. 环境与前置

| 项 | 值 |
|---|---|
| 目标 | `http://YOUR_SERVER_IP:8301`（容器 `prism-2api` + `prism-2api-pg`） |
| 鉴权 | `Authorization: Bearer $WEB2API_API_KEY`（本站 key，非上游 cookie） |
| 上游 | `VENDOR_API_BASE_URL=https://prism.openai.com` |
| 账号 | 1 个（`account1`，free 套餐，cookie 形态，`expires_at`≈2026-09-26） |
| 观测 | `docker logs prism-2api \| grep 'prism: start\|prism: turn'` |
| 客户端超时 | ≥ 300s（冷启动建项目 + 预热链可达 20~60s） |

**日志断言用的两个锚点**（每轮各一行，是判断"参数真的到了上游"的唯一证据，不看这两行一切都是猜）：

```
prism: start model="gpt-6-astra" effort="high" conv=cdx1_… items=2 shape=system(1538)/user(21)
prism: turn  tools_sent=1 tool_calls=1 text_len=57 item_types=message x1
```

---

## 1. 分层用例

编号 `L<层>-<序>`。判定标准写在 §4，实跑结果写在 §3。

### L0 存活与鉴权（必过，30 秒内）

| 编号 | 请求 | 期望 |
|---|---|---|
| L0-1 | `GET /healthz` | 200；PG ping 通过 |
| L0-2 | `GET /admin/` | 200（管理台静态资源在） |
| L0-3 | `GET /v1/models` | 200，含 `gpt-6-astra` / `gpt-5.6-sol` / `gpt-5.6-terra` |
| L0-4 | `GET /v1/models` 不带 key | 401 `invalid_api_key` |
| L0-5 | `POST /v1/chat/completions` 不带 key | 401（且**不**消耗上游额度） |

### L1 核心对话链路（必过）

| 编号 | 请求 | 期望 |
|---|---|---|
| L1-1 | 非流式「只回复四个字：生产可用」 | 200，正文精确等于 `生产可用` |
| L1-2 | 同账号第二轮「1+1」 | 200，正文含 `2`；耗时 ≤ L1-1（沙箱复用） |
| L1-3 | `stream:true`「数到三」 | 200，SSE 帧以 `data: [DONE]` 结束；`finish_reason=stop`；拼出的正文含 `1` `2` `3` |
| L1-4 | 多轮历史折叠：system 里埋 `4173`，追问该值 | 200，答 `4173`（证明 Context 折叠生效） |
| L1-5 | `POST /v1/messages`（`anthropic-version: 2023-06-01`） | 200，`content[].type=="text"`，`stop_reason=end_turn` |
| L1-6 | `POST /v1/messages` + `thinking.budget_tokens=2048` | 200，`content` 至少含一个 `thinking` 块 + 一个 `text` 块 |
| L1-7 | `POST /v1/responses`（OpenAI Responses 形状） | 200，`status=completed`，`output_text` 非空 |
| L1-8 | 问身份「你是什么模型？」/「由谁提供？」 | 答**本服务通用助手**，不出现 `Codex` / `OpenAI` / `GPT-5` |
| L1-9 | `system`「必须用法语」+ 1 轮历史 + 提问 | 答法语（`system` 指令在多轮下仍生效，**P0-1 回归用例**） |

### L2 参数面（易静默降级，重点）

| 编号 | 请求 | 期望 |
|---|---|---|
| L2-1 | `reasoning_effort` = low / medium / high | 200，三个值日志**原样**出现 |
| L2-2 | `reasoning_effort` = `xhigh` / `max` | 200；日志收值见 §3（当前被夹到 `high`，是**待修**项，不是通过） |
| L2-3 | 模型名后缀 `-low/-medium/-high/-xhigh/-max` | 200，**上游收到的模型名必须是裸名**（日志无后缀），档位按后缀补 |
| L2-4 | 显式 `reasoning_effort` 与名字后缀同时给 | 显式参数优先，后缀不覆盖 |
| L2-5 | 未知模型 `gpt-5.4` | 400 `invalid_request`，≤5s，**不冷却账号**（紧接着的正常请求仍 200） |
| L2-6 | `-xhigh` 后缀 | 200，且上游收到的模型名是裸名（不得把 `-xhigh` 整串发上游） |

### L3 工具调用（仿真协议，成功率依赖措辞）

| 编号 | 请求 | 期望 |
|---|---|---|
| L3-1 | 强提示（含"必须以工具调用形式回复"）+ `tools` | 200，`finish_reason=tool_calls`，`tool_calls[0].function.name=get_weather`，`arguments` 是合法 JSON 含 `city` |
| L3-2 | 同上 `stream:true` | `delta.tool_calls[0]` 带 `id`/`name`/`arguments`；`finish_reason=tool_calls` |
| L3-3 | 回灌 `role:"tool"` + `tool_call_id` | 200，模型据此继续或收尾 |
| L3-4 | 弱提示（"查一下北京天气"） | 允许退化成文本；**记录退化率**（见 §3，这是当前的主要缺陷面） |
| L3-5 | 三个模型各跑同一强提示 5 次 | 记录触发率 `n/5`，作为模型维度的回归基线 |

### LM 多模态（图片 / 文件附件）

| 编号 | 请求 | 期望 |
|---|---|---|
| LM-1 | 90×30 **三色带图**（左红/中绿/右蓝，`image_url` data URL）+「从左到右是什么颜色？」 | 200，答含 `红` `绿` `蓝`（颜色顺序模型猜不中 → 证明图真的到了模型眼前） |
| LM-2 | 同 LM-1 且 `stream:true` | 同上（流式路径也走同一套附件块） |
| LM-3 | 文本附件（`file` 块，内含口令 `PRISM-FILE-7391`）+「附件里的口令是什么？」 | 200，答含该口令（文档类贴正文） |
| LM-4 | Anthropic `{"type":"image","source":{"type":"base64",…}}` 同一张色带图 | 200，`content[].text` 含三个颜色词 |
| LM-5 | 三个模型各传同一张色带图 | 三个都对（模型维度回归；适配器对三个模型走同一条路） |

服务器侧对应日志：`prism: start … shape=user(<字符数>)`——内联图片时字符数会明显变大（base64）。
判别信号刻意用**模型猜不中的事实**（颜色顺序、随机口令），否则"没看到图也能蒙对"。

**边界（实测记录，见 `docs/PRISM_API.md` §4.9.2）**：内联 base64 59k 字符可用（整轮 ≈4 分钟）；
219k 字符上游 `502`。实现取单图 ≤48k、单请求 ≤96k，超预算先缩图再退化成"未载入"说明。

### L4 并发与容量

| 编号 | 请求 | 期望 |
|---|---|---|
| L4-1 | 4 并发同模型 | 全 200；wall 时间 ≈ 单请求耗时（不串行化） |
| L4-2 | 8 并发 | 全 200；无 429/5xx；账号 `fail_count` 不涨 |
| L4-3 | 并发后紧接 1 个请求 | 200（账号未被冷却） |

### L5 运维与寿命（人查，不进脚本）

| 编号 | 检查 | 期望 / 动作 |
|---|---|---|
| L5-1 | `GET /api/admin/accounts` | `logged_in=true`；记录 `expires_at` 剩余天数 |
| L5-2 | `has_api_key` | `false` ⇒ **到期无法自动重登**，须提前换 `prism-login` 导入的凭据束 |
| L5-3 | `GET /api/admin/stats` | `accounts` 数 ≥ 2（单账号是硬单点）；`by_fail_class` 无持续增长 |
| L5-4 | `docker compose ps` | 两个容器 `healthy` |

### L6 容量与 RPM（人驱动，阶梯式）

不能用"并发 50 猛冲"——唯一的号一旦吃 429 会被冷却 `60s × 1.5^(n-1)`（封顶 5 分钟），
测试窗口直接被烧掉。协议、遥测与安全阀见 **§6**，工具 `scripts/rpmprobe.py`。

| 编号 | 检查 | 期望 |
|---|---|---|
| L6-0 | `python3 scripts/rpmprobe.py --plan` | 打印阶梯与请求预算；不发请求 |
| L6-1 | 并发 1 / 2 / 4 各 60s（短提示词） | 全 200；RPM 随并发线性增长（= 并发 / 单轮耗时 × 60） |
| L6-2 | 上探到 8 / 10 | 找到**首次出现失败或冷却**的并发 = 天花板 |
| L6-3 | 长提示词对照 | 生成时长拉长 → RPM 按比例下降（证明瓶颈是生成而非调度） |
| L6-4 | 突发对照（短时间成批打满并发） | 与稳定速率结论分开记录，不混用 |

---

## 2. 判定标准

- **放行**：L0 全过 + L1 全过 + L2-1/L2-3/L2-4/L2-5 过 + L4-1 过。
- **阻塞**：L0 任一失败，或 L1-1/L1-5/**L1-9** 失败，或 L2-5 失败（模型名后缀没剥干净会直接把 `-high` 发上游 → 400）。
  L1-9 当前为红——这是 **P0-1 的已知缺陷信号**，修掉后转绿（见 `docs/ENHANCE-PLAN.md`）。
- **已知不通过但可用**（记录在案，不算阻塞）：L1-8 身份长尾泄漏、L3-4 弱提示退化、L2-2 档位夹取。
- **抖动用例**：L3-2 / L3-5 有固有抖动（见 §3），单次失败不构成回归；以 5 次重复的触发率为准。

---

## 3. 实跑结果（2026-09-17，YOUR_SERVER_IP:8301）

### L0

| 编号 | 结果 |
|---|---|
| L0-1 | 200 |
| L0-2 | 200 |
| L0-3 | 200，三个模型均在 |
| L0-4 | 401 `invalid_api_key`（3.0s） |
| L0-5 | 401（3.1s） |

### L1

| 编号 | 结果 | 耗时 | 正文 |
|---|---|---|---|
| L1-1 | ✅ 200 | 5.5–11.4s | `生产可用` |
| L1-2 | ✅ 200 | 5.4–8.8s | `2` |
| L1-3 | ✅ 200 | 5.8–8.1s | 6 帧，`finish_reason=stop`，文本 `1 2 3` |
| L1-4 | ❌ **FAIL** | 7.8s | 期望 `4173`，实答 **`未提供数字。`** —— 见下方根因 |
| L1-5 | ✅ 200 | 6.6–8.2s | `content:[{type:text,…}]`，`stop_reason=end_turn` |
| L1-6 | ✅ 200 | 9.5–11.4s | `content:[{type:thinking,…},{type:text,…}]` |
| L1-7 | ✅ 200 | 7.3–8.2s | `status=completed`，`output_text="responses 通道通"`，usage 齐全 |
| L1-8 | ❌ **FAIL** | 8.3–10.5s | 「你是什么模型？由谁提供？」→ `我是 OpenAI 的 AI 助手…`。身份闸已上线且**挡住**了「你是谁？」「你是什么模型？」，但 provider 类问法整体漏网（详见下） |
| L1-9 | ❌ **FAIL** | 6~8s | system「必须用法语」+ 1 轮历史 → 答中文，**system 指令被整条丢弃** |

### L1-4 / L1-9 的根因（隔离实验，各 3 次，`gpt-6-astra`）

| 实验 | 布局（除注明外前面还有注入的 DefaultSystemPrompt） | 命中 |
|---|---|---|
| V1 | `system:"数字是4173"` + user，**无历史** | ✅ 3/3 |
| V2 | `system:"数字是4173"` + `system:"[对话历史]…"` + user | ❌ **0/3** |
| V3 | 信息放进历史里的 user 轮 | ✅ 3/3 |
| M1/M2 | 前文**合并成一条** system | ✅ 3/3 |
| M3 | 仍两条，但把事实放在 `[对话历史]` **之后** | ✅ 3/3 |
| C1 | `system:"必须用法语"` + user（无历史） | ✅ 3/3 法语 |
| C2 | 同 system + 1 轮历史 | ❌ **0/3** 法语 |
| C2m | C2 的 system 与 `[对话历史]` 合并 | ✅ 3/3 法语 |

⇒ **最后一条 system item 会盖掉之前的 system item**，而 `[对话历史]` 恒为最后一条。
即"多轮对话 + 人设/格式约束"的调用方会静默失效。tools 分支无此问题（它早就合并成一条 user 消息）。
定位与改法见 `docs/ENHANCE-PLAN.md` P0-1；L1-9 即该缺陷的回归用例。

### L1-8 的身份闸覆盖（2026-09-17 10:2x，打生产）

生产 `identity_guard_enabled=True` 已生效，四个问法逐一复核：

| 问句 | 结果 |
|---|---|
| 「你是谁？」 | ✅ 闸命中（`我是本服务的通用助手，可以在你的工作区中读写代码与文档…`） |
| 「你是什么模型？」 | ✅ 闸命中（`我是本服务的通用助手，当前对话未提供可确认的具体模型名称。`） |
| 「你是什么模型？**由谁提供**？」 | ❌ 泄漏：`我是 OpenAI 的 AI 助手…` |
| 「你**由哪家公司**提供？」 | ❌ 泄漏：`我是由 OpenAI 提供的 AI 助手。` |

⇒ 闸的 `identityTopicRe` 是**整串锚定**且词表里没有 provider 类问法，后两句整体失配。
修法见 `docs/ENHANCE-PLAN.md` P1-1。

### L2

| 编号 | 结果 |
|---|---|
| L2-1 | ✅ 日志实收 `low` / `medium` / `high`，与请求一致 |
| L2-2 | ⚠️ `xhigh` 与 `max` 收到的都是 `high` —— 夹取在 `internal/adapter/prism/stream.go` `reasoningEffort`（`case "high","xhigh","max": return "high"`），上游是否真支持 `xhigh` **未知**（`har/` 三份抓包里站点只发过 `medium` 与 `high`）。见 `docs/ENHANCE-PLAN.md` P2-1 |
| L2-3 | ✅ `gpt-6-astra-xhigh` → 日志 `model="gpt-6-astra" effort="high"`，未 400 |
| L2-4 | ✅ 显式 `reasoning_effort` 优先 |
| L2-5 | ✅ 400 / 3.1s，紧接的 `gpt-6-astra` 请求 200（**未冷却**） |
| L2-6 | ✅ |

### L3（`get_weather` 单工具，同一句提示词）

触发率（同一强提示「必须以工具调用形式回复」，每模型 5 次，跑了两轮）：

| 模型 | 第一轮 | 第二轮 |
|---|---|---|
| `gpt-6-astra` | **5/5** | 4/5 |
| `gpt-5.6-sol` | **5/5** | 5/5 |
| `gpt-5.6-terra` | **0/5** | 1/5 |

⇒ 触发率 = f(提示词强度, 模型)，**且本身有抖动**（astra 5/5→4/5）。
当前强提示下 astra/sol 可用、terra 基本不可用。

提示词强度阶梯（`astra` / `terra`）：

| 提示词 | astra | terra |
|---|---|---|
| V1「北京天气怎么样？」 | 文本 | 文本 |
| V2「用 get_weather 工具查北京天气」 | 文本 | 文本 |
| V3「…必须以工具调用形式回复」 | **工具调用** | 文本 |
| V4「必须以下面格式回复，不要输出任何其他内容：`<tool_call>{…}</tool_call>`」 | **工具调用** | **工具调用** |

其余：

| 编号 | 结果 |
|---|---|
| L3-2 流式 | ✅ 单独复测 **5/5** `finish=tool_calls`，`delta.tool_calls` 拼出完整 `{"city":"北京"}` + `id`/`name`；耗时 10~19s（比非流式慢 1 倍）。**但冒烟脚本里曾出现 1 次退化成 `stop`** → 与 L3-5 的抖动同源 |
| L3-3 回灌 | ✅ 200（8.9~11.0s） |
| L3-4 弱提示 | 退化成文本（凭记忆答"北京当前多云，22°C"）—— 记录项，是 P0-2 要改善的产品指标 |

### L4

| 编号 | 结果 |
|---|---|
| L4-1 | ✅ 4 并发 **4/4 200**，wall 10.1s |
| L4-2 | ✅ 8 并发 **8/8 200**，wall 11.6s；`by_fail_class` 未新增 |
| L4-3 | ✅ 并发后紧接 1 个 → 200（6.2~6.5s） |

⇒ 延迟与并发基本线性，8 路下无排队（`concurrency_limit=10`，未触顶）。

> ⚠️ **测试环境陷阱（已修）**：本机 `HTTP_PROXY=127.0.0.1:7897`，Python `urllib` 默认走代理，
> 4 路并发时代理回 **502**（wall 0.6s），一度被误判成"服务端并发故障"；同一批请求用 `curl` 直连则 4/4 200。
> `scripts/smoke.py` 现在显式 `ProxyHandler({})` 直连。跑并发用例前务必确认没被本机代理污染。

### L5

| 编号 | 结果 |
|---|---|
| L5-1 | ⚠️ 仅 1 个账号，`logged_in=true`，`expires_at=1790415217` ≈ **9.3 天后到期** |
| L5-2 | ⚠️ `has_api_key=false` + 无密码/TOTP ⇒ **到期无法自动重登**，服务会直接断 |
| L5-3 | ⚠️ `accounts=1`；`by_model`：`gpt-6-astra` 173、`gpt-5.6-terra` 132（含大量重试）；`by_fail_class={bad_request:7, canceled:1, server:2}` |
| L5-4 | ✅ 两容器 `healthy` |

`by_fail_class` 的两次采样（跑测前 / 跑测后）：`{6, 2}` → `{7, 1, 2}`——
`bad_request` +1 正是 L2-5 的未知模型（预期行为），多出的 `canceled:1` 对应下一条。

**偶发传输层断连**：约 40 次请求中出现 1 次 `RemoteDisconnected`（服务端直接关连接、无响应），
被管理台记为 `canceled`，`server` 类计数未涨 ⇒ 不是上游报错，是连接在中途断掉。
客户端侧表现为硬失败而非 5xx，长跑场景需注意（未定位到确定性触发条件）。

---

## 4. 未覆盖（本计划待办）

1. **上游整段抖动**（`503 Project conversation lookup failed`）：只在长跑里出现，需要拉长观察窗 / 造错注入。
2. **沙箱闲置 3 分钟后的重预热**：需故意等待。
3. **上游对 `reasoning_effort: "xhigh"` 的接受度**：需临时放开夹取后再打（L2-2 的根因）。
4. ~~**图片 / 附件输入**：站点走项目文件管理，适配器未实现（`docs/ENHANCE-PLAN.md` P2-2）。~~
   → 已实现 + 已测（LM 层，走"base64 内联 + view_image"）。仍未验：大图缩图后的识别质量、
   webp/avif 等无解码器格式的端到端行为、扫描件 PDF 的抽取效果。
5. **Anthropic 入口 `WEB2API_MAX_EFFORT` 上限**的实际生效点（compose 默认 `high`，未做压限对比）。
6. **多账号轮询**：当前只有 1 个号，调度器行为无法验证。
7. **长上下文**（>100k）与历史折叠的截断行为。
8. **`RemoteDisconnected` 的触发条件**：需要服务端日志 + 长跑采样（当前只有 1 例）。
9. **并发 / RPM 天花板**：只验到 8 路"能过"，不知道第一次失败出现在哪里 —— 见 §6。
   （本地已不限流，见 §6.2，所以阶梯上出现的失败即可直接归因上游。）

---

## 5. 复现

```bash
# 全量
WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=... python3 scripts/smoke.py

# 只看某一层
python3 scripts/smoke.py --only L0,L1
python3 scripts/smoke.py --only L2 --models gpt-6-astra
python3 scripts/smoke.py --only L3 --repeat 5     # 触发率统计

# 容量阶梯（先干跑看预算，再真跑）
python3 scripts/rpmprobe.py --plan
python3 scripts/rpmprobe.py --steps 1,2,4 --duration 60

# 观测侧（在服务器上）
docker logs prism-2api --since 15m | grep 'prism: start\|prism: turn'
```

---

## 6. 容量测试方案（一个号能吃多少 RPM）

### 6.1 机制前提（先读清楚，否则 RPM 数字没有意义）

**每个客户端请求都会在上游新建一个会话。** `internal/adapter/prism/client.go` `startTurn()` 内：

```go
conv := "cdx1_" + uuid4()   // 每次调用都新生成，随后 registerConversation 登记
```

**没有会话复用、也没有清理路径**（全仓无 conversation 删除/裁剪逻辑）。相应地：

| 资源 | 生命周期 | 证据 |
|---|---|---|
| **会话（conversation）** | **一请求一个**，`cdx1_<uuid4>` | `client.go` `startTurn()` |
| 项目（project） | 进程级复用，标题固定 `prism-2api`，重启后按标题找回 | `client.go:41`、`ensureProject`（`projectID` 缓存） |
| 沙箱令牌 | 复用，**闲置 3 分钟**过期后重走预热链 | `client.go:43` `sandboxIdleTTL`、`sandboxToken()` |

推论（与容量测试直接相关）：

1. **上游每客户端请求 ≈ 1 次 `start` + N 次 `status` 轮询**，`N ≈ 单轮耗时 / 1.5s`（`pollTurn` 间隔 1.5s）。
   单轮 9s ⇒ 约 6.3 倍放大 ⇒ **"客户端 RPM" 与 "上游 RPM" 是两个数**。
2. 轮询是廉价请求，`start` 才是重活（要拉起一轮沙箱生成）⇒ **瓶颈大概率是 `start` 的并发能力**，不是 HTTP QPS。
3. 多轮上下文**不是上游记忆**：适配器把历史折叠成文本塞进本轮 `input`，每轮都是"全新会话 + 重放历史"。
   这既解释了 L1-4（历史折叠失效），也意味着**上下文越长，每个请求越贵**，RPM 会随上下文长度下降。
4. **会话无界堆积在一个项目里**，且删不掉。跑 1000 次请求 = 项目里多 1000 个会话。
   这与"偶发 `503 Project conversation lookup failed`"是否相关，是容量测试顺带要回答的问题。

### 6.2 要测的两个数

| 指标 | 定义 | 用途 |
|---|---|---|
| **客户端 RPM** | 每 60 秒成功返回的 `/v1/chat/completions` 数 | 对外承诺的吞吐 |
| **上游 RPM** | 账号真正承受的上游请求数（≈ 客户端 RPM × (1 + 单轮耗时/1.5)） | 区分先撞我们的限流还是上游的 |

**本地不限流（2026-09-17 起）**：本项目上游没有速率限制，本地任何自设闸门都只是白丢吞吐，
因此代码默认值已全部改为"不限制"，测到的失败一律归因上游：

| 闸门 | 默认 | 现状 |
|---|---|---|
| `account_concurrency` | **0 = 不限制** | 单号并发不再有上限（原默认 10，会让单号场景在 11 路时直接失败） |
| `account_concurrency_429` | **0 = 不降级** | 收到 429 不再把并发降到 5 |
| `max_in_flight`（对话并发） | **已移除** | 对话路径不再有本地闸门，配置面里连这个字段都没有 |

> 代码侧依据：`internal/pool/account.go` `Acquire`/`fullLocked`（上限 `<=0` 即"永不满"）、
> `internal/admin/config.go`（默认值与归一化不再回退到 10/5）、`internal/admin/schema.go`；
> 对话闸门 `internal/api/ops.go` 的 `inflightGate`/`Server.admit` 已整体删除，7 条路由不再包裹。
> **改完必须核对线上值**：`curl -s --noproxy '*' -b <cookie> .../api/admin/config | grep -E 'account_concurrency'`
> —— 老库升级历史里可能残留 10/5。

上游失败对应的**冷却**（`internal/failclass/class.go`）：
`rate_limit` 60s×1.5^(n-1)、`server` 120s、`unavailable` 300s，封顶 5 分钟；`auth` 1h、`quota` 24h。

### 6.3 协议：阶梯 + 安全阀，不猛冲

```bash
python3 scripts/rpmprobe.py --plan                       # 0) 先干跑，看预算，不发请求
python3 scripts/rpmprobe.py --steps 1,2,4 --duration 60   # 1) 稳定速率阶梯
python3 scripts/rpmprobe.py --steps 8,16 --duration 60    # 2) 上探（只在上一档全绿时做；本地已不限流）
python3 scripts/rpmprobe.py --steps 4 --duration 60 --prompt long  # 3) 生成时长对照
```

- **每档固定并发、持续 `--duration` 秒**，worker 串行发请求 ⇒ 测的是"稳定吞吐"，不是峰值。
- **每档开跑前**读 `/api/admin/dashboard`：账号在冷却中就等（最多 6 分钟），不在冷却中才开跑。
- **每档结束后**比对 `stats.by_fail_class` 增量与 `cooldowns` 列表，记录**第一次出现的是哪一类失败**。
- **安全阀**（脚本内置）：出现冷却 → 立即停止加梯，记录"并发 C 就是天花板"；
  单档错误率 > 25% 且样本 ≥ 8 → 停止加梯。
- **突发与稳定分开记**：L6-4 的"短时间成批"结果不得与稳定速率混进同一个数字。

### 6.4 观测与归因

| 要看什么 | 怎么看 |
|---|---|
| 客户端成功率 / RPM / 分位 | `scripts/rpmprobe.py` 每档一行 |
| 失败分类增量 | `/api/admin/dashboard` → `stats.by_fail_class` |
| 是否触发冷却 | 同上 → `cooldowns[]` / `accounts.cooling` |
| 参数真到上游 | `docker logs prism-2api --since 15m \| grep 'prism: start\|prism: turn'` |
| 上游 start 次数 | 上面 `grep -c 'prism: start'`，与成功数应大致相等（差值 = 重试次数） |
| 上游轮询次数 | **当前无法直接计数**（代码未打点）。要么加一行计数日志，要么按 `1 + 耗时/1.5` 估算 |

### 6.5 风险与副作用（**动手前必须认账**）

1. **只有 1 个号，且约 9 天后到期**（`expires_at≈2026-09-26`、`has_api_key=false`）。
   压测消耗的是唯一账号；一旦被冷却 5 分钟，期间服务对外不可用（无兜底）。
2. **会话污染不可逆**：每次请求在上游项目里留下一个会话，无删除接口。
   请求预算必须设上限（脚本会打印预算），别跑到四位数。
3. **可能把上游推向降级**：若 `503 Project conversation lookup failed` 确与会话堆积相关，
   压测本身会让之后的正常请求更容易失败。**跑完必须回归**：`python3 scripts/smoke.py --only L1`。
4. 建议顺序：**先把 P1-2 的第二账号补上，再压容量**。单号压测的结论只是"这 1 个号的上限"，且没有退路。

### 6.6 交付物

一张阶梯表（并发 × 稳定 RPM × 成功率 × p50/p95 × 首次失败类型）+ 一句结论：

> 该账号在**并发 N** 时达到 **X RPM**，首先出现的是 **<失败类型>**；
> 再往上受**上游**限制（本地已无限流，见 §6.2）。

### 6.7 理论上限初估（实测前的标尺）

单轮耗时是唯一的除数：`RPM ≈ 并发 / 单轮耗时(s) × 60`。已观测：短提示词单轮仍 ~9–11s
（**生成不是瓶颈，沙箱 turn 开销才是**），长提示词 15–35s。

| 并发 | 单轮 9s | 单轮 15s | 单轮 30s |
|---|---|---|---|
| 1 | 6.7 | 4.0 | 2.0 |
| 4 | 26.7 | 16.0 | 8.0 |
| 8 | 53.3 | 32.0 | 16.0 |
| 10 | 66.7 | 40.0 | 20.0 |
| 16 | 106.7 | 64.0 | 32.0 |

⇒ 单号合理预期是**几十 RPM 量级**，不是几百。第一次失败出现在哪一档，才是要测的东西。

**已做的仪器验证**：`--steps 1 --duration 12` 实跑一次——2 个请求全 200、管理台登录成功、
RPM/分位/汇总正常输出，确认脚本链路可用。完整阶梯尚未跑，**等 §6.5 认账后再跑**。
