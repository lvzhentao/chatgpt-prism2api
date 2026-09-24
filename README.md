# prism-2api

把 **prism.openai.com**（OpenAI 的 AI LaTeX 编辑器）封装成 OpenAI / Anthropic 兼容 API。
内核来自 `web2api-starter/kernel`（号池 / 调度 / 假缓存 / 管理台），站点协议在 `internal/adapter/prism/`。

文档：`docs/PRISM_API.md`（接口逆向结论 + 全部抓包证据）、`docs/VERIFY.md`（真机验收记录）、
`docs/LOGIN.md`（登录能力设计）、`docs/PLAN.md`（计划与交付物）、
`docs/PROMPT-INJECTION-PLAN.md`（提示词注入 + 身份控制方案）、
`docs/TESTPLAN.md`（分层测试计划 + 实跑基线）、`docs/ENHANCE-PLAN.md`（增强提升计划）。

## 能力

| 端点 | 状态 |
|---|---|
| `POST /v1/chat/completions`（含 `stream:true`） | ✅ 真机验证 |
| `POST /v1/messages`（Anthropic） | ✅ 真机验证 |
| `GET /v1/models` | ✅ 静态目录：`gpt-6-astra` / `gpt-5.6-sol` / `gpt-5.6-terra`（见「模型与思考档位」） |
| 管理台 `/admin/` | ✅ 账号 / 用量 / 日志 / 配置 |
| function call | ⚠️ 无原生通道（见「工具调用」），适配器用提示词协议**仿真**。触发率取决于提示词措辞与模型：强提示（"必须调用工具"）+ `gpt-6-astra`/`gpt-5.6-sol` 实测 5/5，`gpt-5.6-terra` 需给出**填好参数的格式示例**才稳定触发；弱提示（"北京天气怎么样"）下模型倾向于凭记忆直接作答。基线与复现见 `docs/TESTPLAN.md` §3 L3 |
| 图片 / 文件附件（多模态） | ✅ 图片转 base64 内联（模型自己还原成工作区文件后用 `view_image` 看图）、文本/PDF 贴正文。三个模型与四个入口（chat / responses / messages / gemini）都支持，见「多模态输入」 |
| 账号登录（密码 + TOTP，真浏览器） | ✅ `prism-login` CLI（见下） |

一轮对话在上游是 **start + status 轮询**（不是 SSE）：适配器先做一次「沙箱预热」，再提交本轮，
轮询到 `completed` 后把正文一次性吐给内核。历史对话会折叠进 `Context` 文本（上游只认最后一条 user 消息）。

### 提示词通道与系统指令注入

站点自己在 `input[0]`/`input[1]` 里带两段 system（LaTeX 编辑器 persona 7476 字 + 编辑器状态 JSON），
**适配器一条都不发**：只摊平客户端给的消息（真机：`items=1 shape=user(30)`，200/10.7s）。

上游服务端还有一套**内置提示词**（只发一条 user 时模型自述"我是 Codex…能使用终端和文件工具"），
客户端删不掉，只能抢先注入自己的指令覆盖它（默认开启，`internal/adapter/prism/prompt.go`）：

- 不带 tools：注入指令作为 `input[0]` 的 system 项，并在开头声明"冲突时以本指令为准"才压得住
  （实测：默认指令 → 仍答 Codex；加优先级声明 + 身份规则 → "我是本服务的通用助手"）。
- 声明了 tools：上游忽略 system 项（实测 system 覆盖失败），故并入本轮 user 消息的第一个块
  `[系统指令]`，调用方自己的 system 另起 `[客户端指令]` 块。
- 开关：`PRISM_SYSTEM_PROMPT=off` 关闭（回到上游默认），`=<文本>` 整体替换（`\n` 为换行），
  `PRISM_SYSTEM_PROMPT_FILE=<路径>` 从文件读。详见 `docs/VERIFY.md` §12–13、`docs/PRISM_API.md` §4.7–4.8。

### 工具调用（仿真）

上游 `response_with_tools_start` **不接受 `tools` 字段**（请求体只有 `conversationId/input/metadata/previousResponseId`），
也不产出 `function_call` item，所以不存在原生 function call 通道。适配器改为**提示词协议 + 文本解析**：

- 请求带 `tools` 时，适配器在**本轮 user 消息**里注入工具协议（每把工具的名字 / 描述 / JSON Schema + 输出格式
  `<tool_call>{"name":…,"arguments":{…}}</tool_call>` 与规则），再把正文里的调用块解析成标准事件。
- 客户端拿到的仍是标准形状：OpenAI `tool_calls` + `finish_reason:"tool_calls"`；Anthropic `tool_use` + `stop_reason:"tool_use"`。
- 客户端回灌的 `role:"tool"` / `tool_result` 会被收集成「[已执行工具的结果]」块，模型据此继续下一轮或直接收尾
  （`finish_reason:"stop"` / `stop_reason:"end_turn"`）。
- 解析失败的块**原样保留在正文里**，不吞内容；协议不合法时模型退化成普通文本回答。

**上游通道语义（真机实证，2026-09-17）**：声明了 `tools` 的请求里，上游**不采信 `system` 内容**——同一句
「回答必须以 ZZZ 开头」的 system 指令，不带 tools 时生效（答 `ZZZ 2`），带 tools 时被忽略（答 `2`）；
对话历史/工具结果放进 system 同样读不到，放进**最后一条 user 消息**则稳定生效。因此工具链路的上下文
一律走本轮 user 消息，且**一个 system item 都不发**；不带 tools 的请求仍走 system 通道（行为不变）。

**延迟预期**（客户端超时建议 ≥ 300s）：

| 场景 | 耗时 |
|---|---|
| 冷启动首个请求（建项目 + 预热链 + start） | 20~60s；上游抖动时可达 1~4 分钟（适配器内部重试，不会挂死） |
| 带图片（小图，base64 几百字符） | 8~31s（实测 7 例）：模型要多走「写文件 → `view_image`」两步 |
| 带图片（base64 ≈ 60k 字符） | +2~4 分钟（长提示词 + 缩图后仍很大）；219k 字符上游直接 502 |
| 同账号请求（沙箱复用，low/medium/high 档） | 7~25s（实测 low 6.8s / medium 7.7s / high 9.7s） |
| 沙箱闲置 > 3 分钟后 | 重新预热一次 |

## 多模态输入（图片 / 文件附件）

上游**没有任何多模态/二进制入参**（真机实测 2026-09-17，过程见 `docs/PRISM_API.md` §4.9）。
适配器按「内容怎么才能真的到模型眼前」分两类处理：

| 附件 | 送法 | 为什么 |
|---|---|---|
| 图片 | base64 内联进提示词 + **一条还原命令**，让模型自己写进工作区再用 `view_image` 看图 | 模型有 `view_image` 工具但只能看工作区文件；工作区只物化项目文件树里已有的文件，headless 上传进不去 |
| 文本 / PDF | api 层抽成正文后直接贴（PDF 抽字面量，上限 8000 rune） | 上游没有二进制通道，贴正文最省事也最稳 |
| 其他二进制 | 一句说明（文件名/MIME/大小）+ 提示用户可提供文本版 | 不静默丢，也不假装能看到 |

图片块的形状（`internal/adapter/prism/attachments.go`）：

```
[图片附件 1] shot.png（image/png）
图片内容只在这段 base64 里，直接读 base64 看不到图像。请先在工作区还原成文件，再查看它：
mkdir -p prism-uploads && printf '%s' '<base64>' | base64 -d > prism-uploads/shot.png
然后用 view_image 查看 prism-uploads/shot.png（该工具不可用时，用你手头能看图的工具），再回答用户。
不要把 base64 复述进回答。
```

| 入口 | 附件怎么给 | 支持度 |
|---|---|---|
| `POST /v1/chat/completions` | `{"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}` 或 `{"type":"file","file":{"filename":"a.pdf","file_data":"…"}}` | ✅ |
| `POST /v1/responses` | `input[].content[].type="input_image"` / `"input_file"`（`file_data` / `file_url`） | ✅（`file_id` 不支持：本服务没有文件存储） |
| `POST /v1/messages` | `{"type":"image","source":{"type":"base64",…}}` / `{"type":"document",…}` | ✅ |
| `POST /v1beta/models/{m}:generateContent` | `parts[].inlineData{mimeType,data}` | ✅（`fileData` 远端引用不支持） |

载荷四种形态都认：`data:` URL、裸 base64、`http(s)` 地址（服务端代拉，20s 超时、20MB 上限、3 次重定向）、
各协议自己的块形状。**三个模型走同一条路**，不按模型区分能力（`/v1/models` 的 `supports_images` 全为 true）。

**行为与约束**

- **预算是硬约束**：单图 base64 ≤ 48k 字符（≈12k token），单请求内联总量 ≤ 96k 字符。
  真机：59k 字符可用（但整轮约 4 分钟）、219k 字符上游直接 `502`。超预算的图会先缩（PNG/JPEG/GIF 转 JPEG，
  盒式平均缩到 1400px 内，质量 82/70/55 逐级降），缩完仍放不下就退化成一句"本轮未载入"的说明（不静默丢）。
- 非 PNG/JPEG/GIF（如 webp/avif）没有内置解码器：小图原样内联，大图直接说明无法处理（提示改用 PNG/JPEG）。
- 多轮历史里的附件按**新消息优先**分配预算：本轮图片先内联，预算用尽后历史里的图只留文件名标记，不重复烧 token。
- 纯图片（无文本）请求同样按「本轮请求」处理（早期实现按"文本非空"认本轮，会把图当历史丢掉）。
- 文件名会被清掉路径/引号等字符再进命令（`it's.png` → `it_s.png`），避免注入 shell。
- 日志：`prism: start … shape=user(713)`（内联图片时字符数会明显变大）；`PRISM_LOG_INPUT=1` 打全量 input，
  `PRISM_LOG_PROMPT=1` 打上游编好的 prompt（`[project file: …]` 之类的展开都在里面）。
- 真机验收：`python3 scripts/smoke.py --only LM`（LM-1 色带图 / LM-2 流式 / LM-3 文本附件 / LM-4 Anthropic 图块 / LM-5 三个模型）。

**故意不做的两条路**（都试过、都不通，别再走）：

1. **上传成项目文件 + `input_file{project_path}`**（站点自己的做法）：`POST /api/project-files/upload` 无鉴权问题、
   响应也正常（`{"id","fileUuid","sedimentFileId"}`），但会话工作区只物化**项目文件树**（由站点编辑器的
   协作文档维护）里已有的文件——headless 上传的文件不在树里，模型 `ls prism-uploads` 得到 No such file。
   站点 UI 上传的文件能被读到（实测同一账号另一个项目里确实有 `prism-uploads/…`），所以这条路的成败取决于文档写入。
2. **`input_image` / `file_url` / `file_id`**：`input_image` 只接受 "valid Prism storage URL"（data:/公网 URL/file_id 全被拒），
   `input_file{file_url}`/`{file_id}` 语法上收下但模型仍去工作区找文件。沙箱出网被代理拦（`Domain forbidden`），
   模型也无法自己下载外部图片。

## 账号池：导入 ChatGPT 账号 → 选中 → 登录（线上可跑）

号池里每条账号有两种来源：**Cookie 导入**（已登录凭据）与 **账号密码 + 2FA 导入**（待登录，
由本服务自己登上去换 Prism 凭据）。后者是主流路径，全流程在服务器上完成。

```
CSV（email,password,totp_secret[,proxy]）──导入──►  号池（凭据束加密落库，账号停用=待登录）
                                                        │  在管理台勾选 / CLI 选中
                                                        ▼
                                  prism-login 容器（真浏览器 + xvfb，可挂代理）
                                   邮箱 → 密码 → TOTP → prism 回调 → prism_oai_* 凭据
                                                        │
                                                        ▼
                                   写回号池（SetToken + 启用）→ 参与调度；到期由保活自动重登
```

**导入**（两种导出格式都认，表头自动映射，列顺序无关）：

| 文件 | 列 |
|---|---|
| `gmail_alive_*_email_password_2fa.csv` | `email,password,totp_secret` |
| `accounts_*.csv`（19 列） | `ID,Email,Password,…,TOTP Secret,…` |
| 老格式 | `email----password----totp----proxy` 或 JSON 数组 |

- 管理台「账号 → 导入」：直接粘贴 CSV / 上传文件（`POST /api/admin/accounts/import`）。
- CLI（容器内 / 本地）：
  ```bash
  prism-login --file accounts.csv --import-only        # 只导入（待登录，停用态）
  prism-login --status pending --limit 3               # 从池子里选 3 个未登录的去登
  prism-login --status pending --only a@b.com,c@d.com  # 按邮箱选中
  prism-login --file accounts.csv --limit 5            # 从文件里只登前 5 个
  ```

**登录**（在 `prism-login` 容器里跑：chromium + xvfb + node/jsdom）：

- 管理台勾选账号 → 「登录」（`POST /api/admin/accounts/actions` `{"action":"login","names":[…]}`），
  进度走任务中心（SSE，逐账号日志：`open_prism → fill_email → fill_password → fill_totp → back_on_prism`）。
- 语义：**导入 = 停用（待登录）**；登录成功才 `SetToken` + 启用 → 进入调度。
  停用账号不参与保活（否则刚导入的账号会被后台自动登掉，"选中"就没意义了）。
- `headful` 是硬要求（headless 过不了 Cloudflare）；容器里用 xvfb 提供虚拟显示。
- 每账号固定 `storage_state`（`/data/login-state/<邮箱>.json`）→ 复登复用设备指纹、降低风控。

**相关环境变量**（compose 已接线，`.env` 可覆盖）：

| 变量 | 作用 | 默认 |
|---|---|---|
| `PRISM_LOGIN_SIDECAR_URL` | 侧车地址；**留空则 exec 本地脚本**（本地开发） | `http://prism-login:8099` |
| `PRISM_LOGIN_CONCURRENCY` | 登录并发（一个浏览器约 500MB） | 2 |
| `PRISM_LOGIN_PROXY` | 登录出口代理，支持 `{sid}` 粘性会话模板（同账号固定 IP） | 空 |
| `PRISM_LOGIN_STATE_DIR` | storage_state 落盘目录 | `/data/login-state` |
| `PRISM_LOGIN_BACKEND` | 侧车浏览器后端 | `chromium` |

**协议登录**（纯 HTTP，无浏览器；`curl_cffi` + sentinel + 代理）已在计划中但**未落地**：
`auth.openai.com` 对无代理出口一律 403（本机与服务器实测），必须先有能过 CF 的代理
（见 `docs/POOL-LOGIN-PLAN.md` P0/P2 与 D4）。当前线上的登录全部走浏览器路径。

## 模型与思考档位

## 模型与思考档位

模型目录是**静态的**：上游没有 `/models` 端点，唯一的模型清单来自登录流程里 Statsig gate
`62892348` 的 value —— 它给出**该账号**可用模型：

```json
{"free_model":"gpt-5.6-terra","free_reasoning_effort":"high",
 "models":[{"id":"gpt-6-astra","label":"6 Astra"},
           {"id":"gpt-5.6-sol","label":"5.6 Sol"},
           {"id":"gpt-5.6-terra","label":"5.6 Terra"}]}
```

真机验证：`gpt-5.6-sol` / `gpt-5.6-terra` 稳定可用；**`gpt-6-astra` 于 2026-09-17 12:26 起在本账号上
回 400**（`codex_v2_restore_start failed`，纯文本也一样，与附件无关——账号模型清单变化或该模型沙箱异常，
处置见 `docs/VERIFY.md` §14.2）。**不在清单里的名字**（`gpt-5.4` / `gpt-5.5` /
`gpt-5.6-luna`）上游同样回 `Error while processing conversation (400 Bad Request)`，
适配器原样转成 `400 invalid_request`（**不冷却账号**，1 秒内返回）。
换套餐/加付费账号后清单会变，此时**不用改代码**：请求里的模型名 100% 透传到
`metadata.model`，`/v1/models` 只是广告位，改 `internal/adapter/prism/models.go` 即可同步广告位。

思考档位走上游的 `metadata.reasoning_effort`（HAR 实测值 `medium`，账号清单默认 `high`），
内核统一成 `low` / `medium` / `high` 三档：

```bash
# OpenAI 入口：显式参数（原样透传，不受上限约束）
-d '{"model":"gpt-5.6-sol","messages":[...],"reasoning_effort":"high"}'
# OpenAI 入口：模型名后缀变体（内核解析后还原裸名 + 注入档位）
-d '{"model":"gpt-5.6-sol-high","messages":[...]}'    # -low / -medium / -high / -xhigh / -max
# Anthropic 入口：budget_tokens → 档位（≥8000 high / ≥2000 medium / 其余 low）
-d '{"model":"gpt-5.6-sol","max_tokens":256,"thinking":{"type":"enabled","budget_tokens":16000},"messages":[...]}'
# Anthropic 入口：显式 effort
-d '{"model":"gpt-6-astra","output_config":{"effort":"low"},"messages":[...]}'
```

验证用哪一档看日志（每轮一行，显示真正发给上游的参数）：

```bash
docker logs prism-2api | grep 'prism: start model='
# prism: start model="gpt-5.6-sol" effort="high" conv=cdx1_...
```

Anthropic 入口会被 `WEB2API_MAX_EFFORT` 压上限（默认 `low`，防止 max 档 40~181s 纯 thinking；
本部署设成 `high`）。OpenAI 入口的 `reasoning_effort` 不经过这个上限。

## 起服务

```bash
# 1) 依赖：PostgreSQL（唯一持久化后端）+ 一份 Prism 凭据
export WEB2API_DATABASE_URL='postgres://user:pass@127.0.0.1:5432/prism?sslmode=disable'
export WEB2API_ADMIN_STATIC=$PWD/web/dist      # 可选：管理台静态资源目录（默认 web/dist）

# 2) 构建
cd web && npm i && npm run build && cd ..
go build -o prism-server ./cmd/server

# 3) 用一份 cookie 引导启动（第一账号）
./prism-server --listen 127.0.0.1:8080 \
  --endpoint https://prism.openai.com \
  --api-key "<prism_oai_access_token>"
```

管理台默认 `admin` / `admin123`，**首次登录强制改密**（未改密时除登录/改密外的管理接口全部 403）。

## 部署（Docker Compose）

`Dockerfile` 是多阶段构建（node 打前端 → go 编译 → alpine 运行），服务器不需要装 Go/Node。

```bash
# 服务器上：源码 + docker compose 即可
cd /opt/prism-2api && cp .env.example .env    # 或让 scripts/deploy.sh 现场生成
docker compose up -d --build                  # API + 管理台在 8080，PG 只在 compose 网络内
```

一键部署（本地 rsync 源码 → 服务器现场 build → 验收），适配 YOUR_SERVER_IP:8301：

```bash
DEPLOY_HOST=YOUR_SERVER_IP DEPLOY_PORT=4344 DEPLOY_USER=root SSHPASS='...' \
  DEPLOY_DIRTY=1 bash scripts/deploy.sh
```

**两条部署路线**（`docker-compose.yml` 的镜像名由变量决定，两者共用同一份 compose 和 `.env`）：

| 路线 | 命令 | 实测耗时 | 用途 |
|---|---|---|---|
| 现场构建 | `scripts/deploy.sh` | 35s（无改动）／57-85s（有改动）／112s（`NO_CACHE=1` 全冷）／177s（链路最差时） | 首次部署、临时改配置、CI 不可用时 |
| 拉取镜像 | `scripts/deploy-pull.sh` | 链路固定开销约 20-30s（实测：SSH 1s + 同步 compose 7s），再加镜像层拉取与容器重建 | 日常上线、回滚 |

拉取路线省掉的是**源码同步和服务器编译**（改动多时 rsync 要 20-40s、冷编译 112s），
不是全部耗时——本机到这台服务器的链路本身很慢，每条 ssh/rsync 都要秒级，这部分省不掉。
真正的收益：改动多时不必再传几十 MB 源码、服务器不必装 Go/Node、按 tag 秒级回滚。

拉取路线的前提是 CI 已把镜像推到 Docker Hub：`.github/workflows/build.yml`，push master 触发，
需要仓库 secrets `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN`（未配置时该工作流跳过，不会把 push 变红）。
构建跑在 GitHub runner（amd64，与服务器同架构）上，服务器只做 `docker compose pull && up -d`。
实测原因：本机到这台服务器只有 ~7KB/s，服务器从 registry 拉有 ~27MB/s；且 GHCR 在这台服务器拉不动，
所以用 Docker Hub（两个镜像共用一个私有仓库，用 `app-*` / `login-*` tag 区分）。

```bash
TAG=<CI 打印的 short sha> bash scripts/deploy-pull.sh   # 上线/回滚指定版本
bash scripts/deploy-pull.sh                             # 拉 app-latest / login-latest
```

要点：

- **必须给上游地址**，否则退回脚手架占位符 `api.example.com`（所有请求 `no such host`）：
  `VENDOR_API_BASE_URL=https://prism.openai.com`（compose 已默认）。
- `.env` 里放 `PRISM_HOST_PORT` / `PRISM_DB_PASSWORD` / `PRISM_DB_USER` / `PRISM_DB_NAME` /
  `WEB2API_API_KEY`（本站鉴权）/ `PRISM_ENCRYPT_KEY`（凭据加密，**换了旧令牌就解不开**）。
  部署脚本 bootstrap 幂等生成，rsync 排除 `.env` 与 `data*/`，升级不会覆盖。
- 拉取路线再用到 `PRISM_IMAGE_REPO`（默认 `your-dockerhub-user/prism-2api`）与
  `PRISM_APP_TAG` / `PRISM_LOGIN_TAG`（默认 `app-latest` / `login-latest`，回滚改这两个）。
- 状态全在 PostgreSQL（`./data-pg`）；`./data` 仅用于一次性迁入旧 JSON。
- 管理台远程访问靠 `WEB2API_ALLOW_REMOTE_ADMIN=true`（compose 已默认）——**等于把管理面暴露到公网**，
  只靠密码保护；怕被打就把端口换成反代 + 白名单，或只绑内网。
- 单条 ssh 连不上（`Permission denied`）多半是 Ubuntu 24.04 sshd 的 `PerSourcePenalties`：
  连续新建连接/超时被罚，等 10 分钟，或像 `scripts/deploy.sh` 那样全程复用一条 ControlMaster。

## 凭据（账号导入）

凭据是浏览器里的一条 cookie：**`prism_oai_access_token`**（RS256 JWT，约 10 天）。

管理台「账号 → 导入」三种形态都能直接粘（整份导出文件也行，不用自己挑值）：

```text
# 形态 1：裸 JWT
eyJhbGciOiJSUzI1NiIsImtpZCI6...

# 形态 2：Cookie 头（DevTools 里整行复制）
prism_oai_access_token=eyJ...; prism_session_token=eyJ...

# 形态 3：Cookie-Editor / Playwright 导出的 JSON（整份粘贴，带不带 url 外壳都行）
{"url":"https://prism.openai.com","cookies":[{"name":"prism_oai_access_token","value":"eyJ..."}, ...]}
```

- 一份导出文件 = **一个账号**；账号名自动取令牌 JWT 里的邮箱（`.../profile.email`），不用手填。
- 一段文本里同时有访问令牌和会话令牌时，按 JWT 有效期自动区分（活得久的是 access），
  不会因为会话令牌排在后面就拿错。
- `prism_session_token`（12 小时）**不需要**导入：适配器会用 oai token 调 `POST /auth/session` 自举并自动续期。
- `prism_oai_access_token` 约 **10 天**过期，cookie 导入**没有密码可供重登**，到期需重新导出；
  想要到期自动续命，改用 `prism-login`（账号密码 + TOTP，见下）。

管理台「账号 → 导入」也支持 JSON 行：

```json
{"name":"acct1","email":"x@y.com","access_token":"eyJ...","api_key":"eyJ..."}
```

## 多账号与负载轮询

一个账号 = 一份 `prism_oai_access_token`。号池由内核调度器管（`internal/pool` + `scheduler`），
适配器对每个账号持有独立的 session / 项目 / 沙箱状态（互不干扰，各建自己的 `prism-2api` 项目）。

导入三种方式：

```bash
# 1) 启动引导（每个 token 一个账号）
./prism-server --api-keys "<token1>,<token2>,<token3>"

# 2) 管理台「账号 → 导入」，每行一个 token（或 JSON 行）
eyJhbGciOiJSUzI1NiIs...token1
eyJhbGciOiJSUzI1NiIs...token2

# 3) JSON 数组
[{"name":"a1","access_token":"eyJ..."},{"name":"a2","access_token":"eyJ..."}]
```

调度行为（默认 `round-robin`，`配置中心` 可改）：

| 配置项 | 默认 | 说明 |
|---|---|---|
| `routing_strategy` | `round-robin` | 同优先级桶内轮询；另有 `weighted-round-robin`、`fill-first` |
| `session_affinity` | 开（1h） | 只有请求带 `session_id` / `conversation` / `prompt_cache_key` 等会话标记时才粘号；普通 `/v1/chat/completions` 体里没有这些字段 → **纯轮询** |
| `request_retry` | 3 | 上游失败后换号重试次数 |
| `max_retry_credentials` | 0 = 全部 | 单请求最多试几个号 |
| `disable_cooling` | 关 | 关掉冷却后，5xx 不再把账号摘 2 分钟（上游整段抽风时有用） |
| `account_concurrency` | 0 = 不限制 | 单号并发上限。**本项目默认不设**：上游没有速率限制，本地闸门只损失吞吐 |
| `account_concurrency_429` | 0 = 不降级 | 账号收到 429 后的并发上限；默认不降级 |

> **不设本地限流**：`prism-2api` 侧不存在任何自设的 RPM/并发速率限制（单号并发与对话并发都没有闸门），
> 并发与 RPM 上限完全由上游决定；唯一的 429 出口是「所有账号都在冷却 / 无可用账号」，与限速无关。

**新建对话**：每个 API 请求都会新铸一个 `cdx1_<uuid4>` 会话（并在上游登记），互不串台；
同一沙箱会话可连续承载多轮/多会话。所以多个账号 × 多请求是天然隔离的。

上游偶发整段 503（`Project conversation lookup failed (503)` / `Please submit prompt again`）时：
适配器先自己重试（换沙箱 + 换会话，最多 3 次，前置调用另有 2~3 次瞬时重试），
仍失败才会把账号交给内核冷却——账号越多越能扛住这种波段。

## 登录（账号密码 + MFA）

除了导 cookie，还可以直接拿账号密码 + TOTP 让服务器自己登录（真浏览器侧车）：

```bash
go build -o prism-login ./cmd/login

# 单条：登录并入库
PRISM_LOGIN_PYTHON=python3 \
./prism-login --email a@b.com --password '***' --totp BASE32 \
  --backend chromium --proxy http://user:pass@host:port \
  --database-url 'postgres://user:pass@127.0.0.1:5432/prism?sslmode=disable'

# 批量（CSV：email,password,totp_secret[,proxy]）
./prism-login --file accounts.csv --concurrency 2 --database-url '…'

# 只验证不入库
./prism-login --email … --password … --totp … --dry-run
```

- `--backend camoufox`（生产，反检测 Firefox）｜`chromium`（本地开发）。
- **必须开有头浏览器**（`--headful=true` 默认）：headless 过不了 auth.openai.com 的 Cloudflare 挑战。
- 侧车细节见 `sidecar/login/README.md`，设计见 `docs/LOGIN.md`。
- 入库时凭据束（含邮箱/密码/TOTP）会一起加密存下：**token 快过期时内核会自动重登**，账号不再受 10 天限制。

## 适配器结构

| 文件 | 职责 |
|---|---|
| `internal/adapter/prism/client.go` | 账号级状态机：session 自举、项目解析、沙箱预热链、start/轮询 |
| `internal/adapter/prism/stream.go` | OpenAI 消息 → Prism `input`；completed payload → `adapter.Event`；`FetchUsage` |
| `internal/adapter/prism/endpoints.go` | 全部上游路径 |
| `internal/adapter/prism/models.go` | 静态模型目录 |
| `internal/adapter/prism/session.go` | 凭据校验（`ExchangeCredential`）；无浏览器登录 |

沙箱预热链（缺一步 `start` 会无限挂起）：`backend/1/new` → `projects/{id}/sandbox/resources-token`
→ `s/sandboxes/proxy/resources-token` → `api/y` → `s/sandboxes/proxy/token` → `wait-for-sync=synced`。
结果按账号缓存（闲置 3 分钟失效），`start` 超时会作废并重来一次。

## 与上游模板的差异

| 位置 | 改动 | 原因 |
|---|---|---|
| `internal/api/server.go` | 删除 `iter.Next()` 失败分支里的 `acc.Release()`（2 处） | 该分支 `acc == nil`，会 nil 解引用 panic（已复现：错误上游 → 进程 panic） |
| `internal/api/server.go` | `OwnedBy: "cursor"` → `adapter.Name()` | 模板残留 |
| `internal/auth/keepalive_test.go` | `refreshServer` 里绑定 `ExchangeHook` 桩 | 模板默认 hook 是占位实现，用例必挂 |
| `internal/api/schema_test.go`、`toolmap_test.go` | 删除引用 `truncateToolResult` / `buildHistoryText` / `agentproto` 的用例 | Cursor 范式残留符号，内核里不存在，包都不编译 |

## 测试

```bash
go test ./...        # 单元测试，全绿

# 端到端冒烟（打真实网关，分层用例见 docs/TESTPLAN.md）
WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=<本站 key> \
  python3 scripts/smoke.py --only L0,L1
python3 scripts/smoke.py --only L3 --repeat 5    # 工具触发率统计

# 容量阶梯（一个号能吃多少 RPM；协议与风险见 docs/TESTPLAN.md §6）
python3 scripts/rpmprobe.py --plan                          # 先干跑看预算，不发请求
python3 scripts/rpmprobe.py --steps 1,2,4 --duration 60      # 真跑
```

> `scripts/smoke.py` / `scripts/rpmprobe.py` 直接连 IP，会绕过 `HTTP_PROXY`
> （本机代理在并发下会吐 502，曾被误判成服务端故障）。
> 档位、工具触发等结论必须配合容器日志 `docker logs prism-2api | grep 'prism: start\|prism: turn'` 才能定案。
> 容量压测会消耗唯一账号且**不可逆地**在上游项目里堆会话——**动手前先读 `docs/TESTPLAN.md` §6.5**。

**一个请求 = 一个上游会话**：`startTurn()` 每次都新建 `cdx1_<uuid4>`，项目与沙箱复用但会话不复用，
且没有清理路径（`docs/ENHANCE-PLAN.md` P1-3）。所以多轮上下文靠适配器折叠重放，而非上游记忆。
