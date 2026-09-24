# 登录侧车（浏览器）

`auth.openai.com` 对非浏览器客户端一律返回 Cloudflare JS 挑战（实测：Go 原生 HTTP、
TLS 指纹伪装、复用浏览器 `cf_clearance` 全部 403），所以登录由本侧车用**真浏览器**完成：
Prism 首页 → 取授权地址 → 邮箱 → 密码 → TOTP → 回跳 `prism.openai.com/auth/popup-callback`
→ 导出 `prism_oai_access_token` 等 cookie。

主进程（Go）用 stdin/stdout 的 JSON 行协议驱动它，见 `docs/LOGIN.md`。

## 依赖

| 后端 | 依赖 | 用途 |
|---|---|---|
| `camoufox`（默认，生产） | `pip install "camoufox[geoip]"` + `python -m camoufox fetch` | 反检测 Firefox，抗风控最好 |
| `chromium`（本地开发） | `pip install playwright` + `playwright install chromium` | 本机已有 playwright 浏览器时可直接用 |

> 实测：**headless 过不了 Cloudflare**，必须 `headless: false`（有头）。
> 服务器上用 xvfb / `xvfb-run` 提供虚拟显示。

## sentinel 资产（需自备一个文件）

协议登录（登录面）与网关对话面（`openai-sentinel-token`，上游 start 强制校验，
一次性、缺失即 403）都依赖 sentinel proof 铸造。铸造由两部分组成：

- `sentinel-runner.js`：本仓库已附带（自研 Node 宿主：参数解析 + vm 加载 SDK + 挑战协议）。
- `sentinel_assets/sdk.js`：来自上游站点的混淆代码，**因版权不随仓库分发，需自备**：

  1. 浏览器登录 prism.openai.com，打开 DevTools → Network；
  2. 找到 `backend-api/sentinel/sdk.js`（或 `sentinel/<sv>/sdk.js`）的响应，
     另存为 `sidecar/login/sentinel_assets/sdk.js`。

缺失时 `login.py` 报 `sentinel 资产缺失`；网关侧表现为 mint 失败、对话 403。
上游若更新 SDK 版本/协议（sv 变化），需重新抓取并适配 `login.py` 里的
`SENTINEL_SV` 等常量——本项目为逆向封装，上游变更需自行跟进。

## 用法

```bash
# 常驻，按行读请求
python login.py --backend camoufox

# 单条（一次性）
echo '{"action":"login","email":"a@b.com","password":"***","totp_secret":"BASE32",
       "headless":false,"proxy":"http://user:pass@host:port"}' | python login.py

# 排障：打开某页并打印表单结构（选择器变了先看这里）
echo '{"action":"inspect","fetch_authorize":true,"headless":false}' | python login.py
```

## 事件

```json
{"event":"state","name":"goto_authorize","url":"https://auth.openai.com/…"}
{"event":"state","name":"fill_password"}
{"event":"state","name":"totp_submitted"}
{"event":"state","name":"retry_after_error","detail":"上游错误页 [route error]: …"}
{"event":"done","cookies":{"prism_oai_access_token":"…","prism_oai_refresh_token":"…"},"storage_state":"/path"}
{"event":"error","stage":"upstream","message":"…"}
```

- 失败会**自动重试一次**（上游错误页 / Cloudflare 挑战超时算可重试）。
- 邮箱步容错：共享设备指纹后 OpenAI 认得这台设备，授权页会先给「选择账号」——
  侧车只点「使用另一个账号」之类的切换入口（绝不点具体账号条目），回到邮箱页再填；
  若页面直接是密码/验证码页则跳过邮箱步，交给后面的等待循环处理。
- `storage_state` 落盘后可复用设备指纹，降低复登时的风控概率（`storage_state_in`）。
