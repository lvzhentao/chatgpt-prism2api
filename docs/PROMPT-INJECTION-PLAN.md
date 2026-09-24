# 提示词注入方案（prism-2api）

参考项目：`../navos2api`（TypeScript 兄弟站点，同一套 web2api 思路的 TS 实现）。
**优先级：`/v1/chat/completions` 与 `/v1/responses` 第一（本服务面向 GPT 客户端）；`/v1/messages` 已有拦截层，本次不动。**

## 0. 结论

1. 基线已上线：`c2a4106` 在 adapter 层注入自有系统指令，`PRISM_SYSTEM_PROMPT` 可一键 `off`。实测结论：**注入能改默认口吻，改不了 tools 路径下的身份自述**。
2. 两个 OpenAI 入口共用同一收口函数 `handleChat`（`responsesToChatRequest` 先把 Responses 请求转成 chat 请求再委托），所以**一处埋点覆盖 chat + responses（含流式/非流式）**，不需要在两个入口各写一遍。
3. 身份控制分三层，谁都不能单独全包：

   | 层 | 作用域 | chat/responses 现状 | 动作 |
   |---|---|---|---|
   | ① 提示词注入 | 全部对话的默认口吻 | 有，但只认 env，改文案要重部署 | **热配置（P0）** |
   | ② 请求前本地作答 | 逐字锚定的裸身份问句 | **完全没有** | **新增（P1-b）** |
   | ③ 回答后清洗 | 漏网的底座自述（Codex / GPT-5 / OpenAI） | **完全没有** | **新增（P1-a）** |

4. 明确不做：全局改写普通回答；tools 路径硬改身份（实测压不住，见 §1.1）；不动 `/v1/messages` 已有逻辑；不改 pool / scheduler / failclass。

## 1. 现状与证据

### 1.1 通道语义（真机实测，见 `docs/VERIFY.md` §12–13）

prism 上游一轮对话 = `start` 提交 + `status` 轮询，**正文一次性到达**：`internal/adapter/prism/stream.go:289` 只 `emit` 一次 `Event{Text: 全部正文}`（之后才是 tool call 与 Ended）。所以「拿到完整正文再决定写什么」在本站零成本，不是 navos 那种"流式改写要缓冲"的负担。

| 场景 | 放置方式 | 实测结果 |
|---|---|---|
| 无 tools | 注入单独成 `input[0]` 的 **system** item，调用方 system 随后 | 带「最高优先级…以本指令为准」声明 → 覆盖成功（"我是小助。"，8.4s）；无声明 → 压不过，仍答 "我是 Codex…" |
| 无 tools | 覆盖指令写进 **user 消息** | 不覆盖（"我是基于 GPT-5 的 Codex。"） |
| 带 tools | 上游**整体忽略 system item** | 注入并进唯一 user 消息首块 `[系统指令]`（调用方 system 改名 `[客户端指令]`）；只有披露策略类规则可靠（"我是本服务的通用助手。" ✅ / 硬改身份"我是小助" ❌） |

### 1.2 OpenAI 两个入口的收口关系（本次核实）

```
POST /v1/chat/completions ──────────────────────► handleChat   (server.go:428)
POST /v1/responses ─ responsesToChatRequest ────► handleChat   (responses.go:270)
                     └ 流式走 io.Pipe + pipeResponseWriter      (responses.go:309)
                     └ 非流式走 httptest.NewRecorder            (responses.go:283)
```

- 请求侧：`handleChat` → `MapChat(&req)`（`chat_map.go:44`）→ `toAdapterRequest` → `adapter.MapChat` → `prism.mapChat`。**这里加一个字段，三个入口全部拿到注入**（`/v1/messages` 也走 `anthropic_handler.go:211` 的同一个 `MapChat`）。
- 响应侧：非流式唯一写在 `nonStreamChat`（`server.go:793` 构造 `ChatMessage{Role:"assistant"}`）；流式在 `streamChat` 的 `ev.Text` 分支（`server.go:581`）。**这两个点就是清洗的埋点**，responses 因为委托而自动继承。
- 附带：`batches.go` / `gemini.go` 也调 `handleChat`，同样白拿。

### 1.3 chat/responses 入口当前没有任何身份保护

`grep -rn "isIdentityProbeQuery\|CheckRefusal\|HasIdentityLeak\|SanitizeIdentityText" internal` 的命中**全部**落在 `internal/api/anthropic_handler.go`：

| 能力 | 位置 | 覆盖 |
|---|---|---|
| 裸身份问句本地作答（答 `claudeIdentityText`） | `anthropic_handler.go:104 / :118 / :236` | 仅 `/v1/messages` |
| 拒绝词 / 身份泄漏清洗 | `anthropic_handler.go:365` → `emulation.HasIdentityLeak` / `SanitizeIdentityText` | 仅 `/v1/messages` |
| 5 类探针本地作答 | `anthropic_probes.go:handleLocalProbes` | 仅 `/v1/messages` |

**且现有泄漏词表不适用于 GPT 入口**：`internal/emulation/probes.go:29 identityLeakRes` 只认 `cursor|prism-2api|vendor-brand` 与 "I'm not Claude Code" 这类**否认 Claude / 自称 Cursor** 的话术。而我们在 GPT 入口实测到的泄漏是**反方向**的——上游自称 "我是 Codex，基于 GPT-5"、"AI 编程与协作代理，能使用终端和文件工具"。这些**一条都匹配不到**，照搬现有词表等于没做。

### 1.4 与 navos2api 的取舍

| navos 机制 | 位置 | 结论 |
|---|---|---|
| 运行时可配 + 控制台可改 + 持久化 + env 播种默认 | `runtime-config-schema.ts`、`runtime-config-service.ts`、`RuntimeConfigPage.tsx` | prism 已有等价物：`admin.RuntimeConfig` + `/api/admin/config` + `web/src/pages/config-page.tsx`，直接复用，**不新建第二套配置规范** |
| 结构化输出指令追加（system 数组 push，字符串 join） | `model-proxy.ts:532` | 语义已对齐：注入是**追加**，不替换调用方 system |
| 回答改写计划 | `detector-probes.ts:planDetectorResponse` / `rewriteDetectorText` | **照抄结构与门控**：只在"本轮就是身份话题 + 问句长度上限"时改写；prism 的 `emulation/probes.go:299-330` 已有同名函数可复用 |
| 回答前拦截（整串相等规则，本地出官方形状响应） | `probe-interception.ts` + `anthropic-official-shape.ts` | 只抄"裸探针本地作答"这一条；**不抄**可编辑规则列表（YAGNI） |
| 全面伪造 Claude 身份（`CLAUDE_IDENTITY_TEXT`） | `detector-probes.ts` | **不照搬**：那是 Anthropic 协议合规层，GPT 入口该给 prism 自己的身份，两者不混 |
| navos 的 `anthropicTools(body).length > 0 → 不做改写` | `planDetectorResponse` | **有意偏离**：GPT 客户端（Codex 类）几乎每条请求都声明 tools，按他们的条件等于在最需要的场景关闭。见 §2.3 |

## 2. 落地设计

### 2.1 P0-① 注入文案热化（去掉"改文案要重部署"）

沿用现有惯例，不新增配置规范：

1. `internal/admin/config.go`
   - `RuntimeConfig` 增 `SystemPrompt string` + `SystemPromptMode string`（`inject` | `off`，默认 `inject`）。
   - `Default()` 播种：env `PRISM_SYSTEM_PROMPT`（`off`/`none`/`-` → `mode=off`）→ 否则 `PRISM_SYSTEM_PROMPT_FILE` 文件内容 → 否则 `prism.DefaultSystemPrompt`。`Open()` 沿用 fill-empty 惯例（老库缺字段 → 填默认），风格对照 `applyAllowRemoteFromEnv`。
   - 访问器 `SystemPromptText() (string, bool)`（`mode=off` 返回 `"", false`）；`PUT` 增 `getStr("system_prompt")` / `getStr("system_prompt_mode")`；`GET` 回显同名键 —— 三处照 `history_compress` 的现成改法（config.go:363 / :431 / :740）。
2. `internal/admin/schema.go`：加两条 `FieldSchema`，`system_prompt` 用新 `Type: "text"`。
3. `web/src/pages/config-page.tsx`：`groups.runtime` 加两个字段名；`renderField` 增 `type === "text"` → 多行 `Textarea`（目前只有 `map` 走 textarea，长文案会被塞进单行 `Input`）。
4. `internal/adapter/prism/prompt.go`：删 env 解析，只留 `DefaultSystemPrompt` 常量；env 真源唯一化到 `admin` 播种处。

### 2.2 P0-② 注入通道（每请求热读，不重启）

- `internal/adapter/types.go`：`ChatRequest` 增 `SystemPrompt string`（kernel-neutral，已解析好的最终文本，空 = 不注入）。
- `internal/api/chat_map.go`：`MapChat` / `toAdapterRequest` 增 `systemPrompt string` 形参，调用方传 `s.runtime.SystemPromptText()`；四处调用点同步（`server.go:446`、`responses.go:270` 经 `handleChat`、`anthropic_handler.go:211`、`anthropic_websearch.go:161`）。
- `internal/adapter/prism/stream.go:mapChat` 带进 `NativeRequest`；`buildInput` 的两条放置路径不变（§1.1 实测过）。
- 效果：改文案 → 保存 → 下一条请求即生效；`off` 一键回上游默认。

### 2.3 P1-a 回答后清洗（GPT 入口的底座自述闸）

新增 `internal/api/identity_guard.go`：

```go
// identityProbeGate 判定本轮是否按身份问句处理（整串锚定 + 长度上限，普通问答打不中）。
func identityProbeGate(req *ChatCompletionRequest) (question string, ok bool)

// guardIdentityAnswer 仅在 gate 命中时改写：泄漏/拒答 → 清洗 → 仍脏则用配置的身份答句。
func (s *Server) guardIdentityAnswer(question, text string) string
```

规则：

1. **门控**：最后一条 user 消息的文本整串匹配身份正则（`anthropic_handler.go:104 identityProbeRe` 已覆盖中英「你是谁 / 你是什么模型 / hi / 你好 …」，抽出来复用），**或**长度 ≤ 上限且命中身份话题（navos 的 `IDENTITY_TOPIC_MAX_CHARS` 口径）。不命中 → 原样返回，**绝不触碰普通问答**（问"Codex 和 Claude 有什么区别"必须原样透传）。
2. **不因 tools 而跳过**（有意偏离 navos）：GPT 客户端常驻声明 tools，而"你是谁"永远用不到工具；跳过等于在最需要的场景关闸。
3. **清洗词表按本站实测补**：新增 `codex`、`openai`、`gpt-5`/`gpt-4` 等底座自述模式，与现有 `identityLeakRes`（Cursor/否认 Claude）合并成两套：**协议层**（Anthropic 入口，Claude 口径）与**产品层**（GPT 入口，禁底座自述）。只删不加，删空则回退到配置的身份答句（默认「我是本服务的通用助手。」，与 `DefaultSystemPrompt` 的身份规则同口径）。
4. **埋点**：
   - 非流式：`server.go:793` 构造 `msg` 前对 `text.String()` 处理（`usage` 估算随之用清洗后文本）。
   - 流式：`streamChat` 的 `ev.Text` 分支。prism 只有一次 `Text` 事件（`adapter/prism/stream.go:289`），直接对 `ev.Text` 处理即可；为兼容将来多片事件的适配器，累积 `strings.Builder`、在 `Ended` 时统一处理并在 gate 命中时**推迟到末尾一次性发出**（等于 navos 的 `buffered: true`）。
5. 开关 `IdentityGuardEnabled`（默认 on）；`/v1/responses` 两条路径因委托自动继承，无需单独埋点。

### 2.4 P1-b 裸身份问句本地作答（chat/responses，对齐 Anthropic 入口）

`/v1/messages` 已经在 `isIdentityProbeQuery` 命中时**不发上游、本地出答**。GPT 入口同样处理，收益是省一次租号与 7–11s 往返，且答案 100% 可控：

- 抽出 `identityProbeText(text string) bool`（复用同一正则），两个入口共用。
- chat 入口：命中 → 直接写一条 `chat.completion`（流式则一个 delta + `finish_reason: stop`），不经 `MapChat`；responses 入口：命中 → 写 `response.completed` + 一个 `output_text` item（`responses.go` 已有非流式转换函数可复用形状）。
- 答句来自配置 `IdentityAnswerText`（默认同 §2.3 的回退句），与 Anthropic 入口的 `claudeIdentityText` **各自独立**。
- 与注入的关系：裸探针不再经过上游，注入对这条路径无影响；其余对话仍靠注入定调。

### 2.5 P1-c 观测

- 注入：`prism: prompt source=config|env|default len=%d placement=system|user-block`（`PRISM_LOG_INPUT=1` 仍可打全文，`inputShape` 输出 `system(1538)/user(30)` 形态）。
- 身份闸：命中时在请求日志上记一行 `identity guard: gate=probe|topic action=passthrough|sanitized|local reason=leak|refusal` —— 便于线上判断是注入生效还是清洗兜底。

## 3. 验收矩阵

全部走 GPT 协议（chat + responses），Anthropic 入口只做回归。

| # | 用例 | 入口 | 期望 | 判据 |
|---|---|---|---|---|
| 1 | 只发 user 问身份 | chat 非流式 | "我是本服务的通用助手…"，不出现 Codex/GPT 自述 | 文案命中 |
| 2 | 同上但声明 tools | chat 流式 | 同上（§2.3 保证） | 文案不含底座自述 |
| 3 | `input: "你是谁"` | responses 非流式 + 流式 | 与 #1 同答案，事件形状合法（`output_text`/`response.completed`） | 两种流式都过 |
| 4 | 管理端改文案 → 立即再问 | chat | 新文案生效，**无需重启** | A/B 前后不同 |
| 5 | `mode=off` / `PRISM_SYSTEM_PROMPT=off` | chat | 回上游默认（"我是基于 GPT-5 的 Codex…"） | 一键回退可用 |
| 6 | 误伤检查：问"Codex 和 Claude 有什么区别" | chat + responses | 原样返回，闸**不**触发 | 日志无 guard 行 |
| 7 | 普通问答回归（牛顿第二定律 + `$F=ma$`） | chat | 与基线一致 | 无回归 |
| 8 | 工具链路回归 | chat | `/tmp/t5.sh`：E2 历史工具结果 25℃、T4 双结果对比正确 | 16s 级完成 |
| 9 | Anthropic 回归 | `/v1/messages` | 仍是 `claudeIdentityText`，行为不变 | 无回归 |
| 10 | 单测 + 全量 | — | `go test ./...` 全绿 | — |

改前先跑基线对照：`go test ./...`、`bash /tmp/t5.sh`、`docs/VERIFY.md` §13 的 M1–M4 探针。

## 4. 风险与边界

- **成本**：注入让每轮上游 input 多 ~1.5k 字符（≈400–500 token）；实测时延 7.1–34.5s，与未注入同量级。缓存模拟按 token 总数估算（`emulation.Cache.Preview`），注入后 emulated `cache_creation`/`cache_read` 数字同步变化，属预期。注入文本固定 → 前缀稳定，利于上游缓存。
- **改写误伤**：只删不加 + 严格门控（整串锚定 / 短问句），普通问答零改动；这是把闸门做窄、宁可漏也不误伤。
- **已知边界**：tools 路径下上游仍可能换话术自称；清洗只覆盖词表命中的表达，不追求 100%。
- **与调用方 system 冲突**：注入声明最高优先级，调用方 system 让位；若要"调用方优先"，加 `SystemPromptMode=append|off` 即可。per-key / per-model 覆盖本期不做。
- **`/v1/responses` 委托结构**：`streamResponses` 用 `io.Pipe` 复用 `handleChat`，所有改动落在 chat 层；一旦将来 responses 不走 `handleChat`，需补埋点（§1.2 的收口关系会变）。

## 5. 分期

| 期 | 内容 | 风险 | 判据 |
|---|---|---|---|
| P0 | §2.1 + §2.2：热配置 + 单一真源 + 通道打通（chat/responses/messages 全覆盖） | 低（纯配置管线） | #1 #4 #5 #7 #10 |
| P1 | §2.3 清洗 + §2.4 本地作答 + §2.5 观测 | 中（会改写回答） | #2 #3 #6 #8 #9 |
| P2（按需） | 可编辑探针规则、per-key/per-model 覆盖、模板变量、注入预览接口 | — | 用户提需求再做 |
