#!/usr/bin/env python3
"""Prism 登录侧车 —— 用真浏览器完成 OpenAI 登录，导出 prism 凭据。

为什么需要浏览器：auth.openai.com 对非浏览器客户端一律返回 Cloudflare JS 挑战
（实测：普通 HTTP 客户端 / TLS 指纹伪装 / 复用浏览器 cf_clearance 都是 403），
只有真浏览器能走完登录。

协议（stdin/stdout 每行一个 JSON 对象）：

  请求（一次一条）：
    {"action":"ping"}
    {"action":"inspect","url":"https://auth.openai.com/log-in"}          # 打印页面结构（排障用）
    {"action":"login",
     "email":"a@b.com","password":"...","totp_secret":"BASE32",
     "proxy":"http://user:pass@host:port" | null,
     "headless":true,"timeout":240,
     "storage_state_in":"/path/state.json" | null,
     "storage_state_out":"/path/state.json" | null}

  事件（多行）：
    {"event":"state","name":"open_prism"}
    {"event":"state","name":"openai_login_page","url":"..."}
    {"event":"state","name":"email_submitted"}
    {"event":"state","name":"password_submitted"}
    {"event":"state","name":"totp_submitted"}
    {"event":"state","name":"back_on_prism"}
    {"event":"done","cookies":{...},"storage_state":"/path"|null,"url":"..."}
    {"event":"error","stage":"...","message":"..."}

用法：
    python login.py                 # 常驻，按行读请求
    python login.py --backend chromium   # 用 playwright 自带 chromium（本地开发/无 camoufox 时）
    echo '{"action":"ping"}' | python login.py
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import re
import struct
import sys
import threading
from urllib.parse import quote, unquote, urlparse
import time
import asyncio
import uuid
import glob

PRISM = "https://prism.openai.com"
AUTH_HOST_RE = re.compile(r"auth\.openai\.com")
PRISM_HOST_RE = re.compile(r"prism\.openai\.com")
# 上游 origin 抖动时 CF 会把 5xx 顶到页面上（标题里就是「504: Gateway time-out」）。
UPSTREAM_5XX_RE = re.compile(r"\|\s*5(0[0-9]):|gateway time-?out|bad gateway|service unavailable", re.I)
CF_CHALLENGE = ("just a moment", "attention required", "enable javascript")
# 上游错误页（账号被风控、服务抖动都会落到这里）——重试一次，不行就报错退出。
ERROR_PAGE_MARKERS = (
    "oops, something went wrong",
    "something went wrong",
    "route error",
    "糟糕，出错了",
    "出错了",
    "invalid content type",
)


class Retryable(RuntimeError):
    """可重试的失败（错误页 / 挑战超时 / 导航超时）。"""

# 登录页在中文/英文下文案不同，选择器全部按「多个候选，先出现先用」处理。
EMAIL_SELECTORS = [
    'input[name="email"]',
    'input[type="email"]',
    'input[autocomplete="username"]',
    'input[id="email-input"]',
]
PASSWORD_SELECTORS = [
    'input[type="password"]',
    'input[name="password"]',
    'input[autocomplete="current-password"]',
]
CODE_SELECTORS = [
    'input[name="code"]',
    'input[id="code"]',
    'input[autocomplete="one-time-code"]',
    'input[inputmode="numeric"]',
]
SUBMIT_TEXTS = ["Continue", "Log in", "Next", "继续", "登录", "下一步", "Verify", "验证"]
# 设备上已有 OpenAI 会话时，授权页会先给「选择账号」：这里的入口回到邮箱登录页。
# 只认「切换/新增账号」入口——绝不点具体账号条目，否则会登成上一个账号。
SWITCH_ACCOUNT_TEXTS = (
    "使用另一个账号",
    "使用其他账号",
    "其他账号",
    "切换到其他账号",
    "重新登录",
    "Use another account",
    "Use a different account",
    "Log in with a different account",
    "Sign in with a different account",
    "Log in to another account",
    "Add another account",
    "Add account",
    "Switch account",
    "Not you",
)


# ---------------------------------------------------------------- TOTP

def totp_code(secret: str, at: float | None = None, period: int = 30, digits: int = 6) -> str:
    """RFC 6238（SHA1/30s/6 位），与 Go 侧 internal/adapter/prism/totp.go 等价。"""
    cleaned = "".join(str(secret).split()).upper().rstrip("=")
    if not cleaned:
        raise ValueError("empty totp secret")
    pad = (8 - len(cleaned) % 8) % 8
    key = base64.b32decode(cleaned + "=" * pad)
    counter = int((at if at is not None else time.time()) // period)
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    value = struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF
    return str(value % (10 ** digits)).zfill(digits)


# ---------------------------------------------------------------- 输出

# 事件落点：stdin/stdout 模式写 stdout；HTTP 模式由服务线程挂自己的 sink。
# 注意不能用 sys.stdout 重定向——那是进程级的，并发登录会互相踩（实测：并发 2 时
# 一条请求的 done 事件被写进了另一个线程/容器日志，调用方只看到"没有凭据"）。
_SINK = threading.local()


def emit(event: str, **fields) -> None:
    payload = {"event": event}
    payload.update({k: v for k, v in fields.items() if v is not None})
    line = json.dumps(payload, ensure_ascii=False)
    sink = getattr(_SINK, "write", None)
    if sink is not None:
        sink(line)
        return
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def log(msg: str) -> None:
    sys.stderr.write(f"[login-sidecar] {msg}\n")
    sys.stderr.flush()


# ---------------------------------------------------------------- 浏览器

def playwright_proxy(proxy: str | None) -> dict | None:
    """Playwright 要求认证拆成 username/password 字段：server 里内嵌 user:pass@ 会被
    chromium 拒（ERR_INVALID_AUTH_CREDENTIALS）。render_proxy 归一时对凭据做过 quote，
    这里拆开并 unquote 还原。"""
    if not str(proxy or "").strip():
        return None
    parsed = urlparse(str(proxy))
    if parsed.username is None:
        return {"server": str(proxy)}
    return {
        "server": f"{parsed.scheme}://{parsed.hostname}:{parsed.port}",
        "username": unquote(parsed.username),
        "password": unquote(parsed.password or ""),
    }


def launch_backend(backend: str, headless: bool, proxy: str | None):
    """返回 (context_manager, 说明)。camoufox 是生产路径，chromium 用于本地开发。"""
    if backend == "camoufox":
        from camoufox import Camoufox  # type: ignore

        opts: dict = {"headless": headless, "humanize": True}
        pp = playwright_proxy(proxy)
        if pp:
            opts["proxy"] = pp
            opts["geoip"] = True
        return Camoufox(**opts), "camoufox"

    from playwright.sync_api import sync_playwright  # type: ignore

    pw = sync_playwright().start()
    browser = pw.chromium.launch(
        headless=headless,
        args=["--disable-blink-features=AutomationControlled"],
        proxy=playwright_proxy(proxy),
    )
    try:
        browser._pw_instance = pw  # 交给 Session.close() 收尾
    except Exception:
        pw.stop()
    return browser, "chromium"


class Session:
    """一次登录会话：context + page + 通用等待/填写工具。"""

    def __init__(self, backend: str, headless: bool, proxy: str | None, storage_state_in: str | None, timeout: int):
        self.backend = backend
        self.timeout = timeout
        self._pw = None
        self._cm = launch_backend(backend, headless, proxy)
        browser = self._cm[0] if isinstance(self._cm, tuple) else self._cm
        self.browser = browser
        self._pw = None
        state = storage_state_in if storage_state_in and os.path.exists(storage_state_in) else None
        if backend != "camoufox":
            self._pw = getattr(browser, "_pw_instance", None)
        if backend == "camoufox":
            self.context = browser.new_context(storage_state=state)
        else:
            self.context = browser.new_context(storage_state=state)
        self.page = self.context.new_page()
        self.page.set_default_timeout(45_000)

    def goto(self, url: str) -> None:
        self.page.goto(url, wait_until="domcontentloaded", timeout=60_000)

    def first_visible(self, selectors: list[str], timeout_ms: int = 30_000):
        """等任一候选选择器出现（返回 locator），都没有则返回 None。"""
        deadline = time.time() + timeout_ms / 1000.0
        while time.time() < deadline:
            for sel in selectors:
                loc = self.page.locator(sel).first
                try:
                    if loc.count() > 0 and loc.is_visible():
                        return loc
                except Exception:
                    continue
            self.page.wait_for_timeout(400)
        return None

    def submit(self) -> bool:
        """点提交按钮：优先 submit 按钮，其次常见文案。"""
        for sel in ['button[type="submit"]', 'button:has-text("Continue")', 'button:has-text("继续")']:
            loc = self.page.locator(sel).first
            try:
                if loc.count() > 0 and loc.is_visible():
                    loc.click()
                    return True
            except Exception:
                continue
        for text in SUBMIT_TEXTS:
            loc = self.page.get_by_role("button", name=re.compile(text, re.I)).first
            try:
                if loc.count() > 0 and loc.is_visible():
                    loc.click()
                    return True
            except Exception:
                continue
        return False

    def page_title_and_text(self) -> tuple[str, str]:
        try:
            return self.page.title(), (self.page.inner_text("body") or "")[:400]
        except Exception:
            return "", ""

    def is_cf_challenge(self) -> bool:
        title, text = self.page_title_and_text()
        blob = (title + " " + text).lower()
        return any(marker in blob for marker in CF_CHALLENGE)

    def wait_cf_pass(self, timeout_ms: int = 25_000) -> None:
        """等 Cloudflare 自动挑战过掉（真浏览器几秒内会跳走）。"""
        deadline = time.time() + timeout_ms / 1000.0
        while time.time() < deadline:
            if not self.is_cf_challenge():
                return
            self.page.wait_for_timeout(1_000)
        title, text = self.page_title_and_text()
        raise Retryable(f"blocked by Cloudflare challenge (url={self.page.url} title={title!r})")

    def cookies(self) -> dict:
        out = {}
        for c in self.context.cookies():
            out[c["name"]] = c["value"]
        return out

    def dump_storage_state(self, path: str | None) -> str | None:
        if not path:
            return None
        state = self.context.storage_state()
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        with open(path, "w") as fh:
            json.dump(state, fh)
        return path

    def close(self) -> None:
        try:
            self.context.close()
        except Exception:
            pass
        try:
            if self.backend == "camoufox":
                self.browser.__exit__(None, None, None)
                close = getattr(self.browser, "close", None)
                if callable(close):
                    close()
            else:
                self.browser.close()
                if self._pw is not None:
                    self._pw.stop()
                    self._pw = None
        except Exception:
            pass


# ---------------------------------------------------------------- 流程

def fetch_authorize_url(sess: Session) -> str:
    """在页面里调 Prism 的登录入口，拿到 auth.openai.com 的授权地址。

    比点「使用 OpenAI 继续」稳：不受中英文文案/弹窗结构变化影响，
    且 state 绑定 cookie 会落在当前浏览器上下文里（回调校验要用）。
    """
    result = sess.page.evaluate(
        """async () => {
            const r = await fetch('/api/auth/redirect', {
                method: 'POST', credentials: 'include',
                headers: {'Content-Type': 'application/json'},
                body: JSON.stringify({action: 'sign-in', provider: 'keycloak'})
            });
            const text = await r.text();
            try { return {status: r.status, url: JSON.parse(text)?.data?.url || ''}; }
            catch (e) { return {status: r.status, url: '', body: text.slice(0, 200)}; }
        }"""
    )
    url = (result or {}).get("url") or ""
    if not url:
        raise RuntimeError(f"/api/auth/redirect 没返回授权地址: {result}")
    return url


def click_openai_continue(sess: Session) -> None:
    """在 Prism 首页点「使用 OpenAI 继续」（中英文都试）。"""
    patterns = [re.compile(t, re.I) for t in ("使用 OpenAI 继续", "Continue with OpenAI", "Sign in with OpenAI", "Log in")]
    for _ in range(20):
        for pat in patterns:
            loc = sess.page.get_by_role("button", name=pat).first
            try:
                if loc.count() > 0 and loc.is_visible():
                    loc.click()
                    return
            except Exception:
                continue
        # 有些版本是链接而不是按钮
        for pat in patterns:
            loc = sess.page.get_by_role("link", name=pat).first
            try:
                if loc.count() > 0 and loc.is_visible():
                    loc.click()
                    return
            except Exception:
                continue
        sess.page.wait_for_timeout(500)
    raise RuntimeError("找不到「使用 OpenAI 继续」按钮")


def click_switch_account(sess: Session) -> bool:
    """「选择账号」页 → 点「使用另一个账号」回到邮箱登录页（点不点得到都返回结果）。

    复用浏览器设备指纹后，OpenAI 认得这个设备，授权页会直接给账号选择列表；
    侧车只认「切换/新增账号」入口，绝不点具体账号条目。
    """
    pats = [re.compile(re.escape(t), re.I) for t in SWITCH_ACCOUNT_TEXTS]
    for _ in range(6):
        for pat in pats:
            for role in ("button", "link"):
                loc = sess.page.get_by_role(role, name=pat).first
                try:
                    if loc.count() > 0 and loc.is_visible():
                        loc.click()
                        log(f"switch account via {role} {pat.pattern!r}")
                        sess.page.wait_for_timeout(2_000)
                        return True
                except Exception:
                    continue
        sess.page.wait_for_timeout(500)
    return False


def wait_auth_page(sess: Session, timeout_ms: int) -> None:
    deadline = time.time() + timeout_ms / 1000.0
    while time.time() < deadline:
        if AUTH_HOST_RE.search(sess.page.url or ""):
            sess.wait_cf_pass()
            return
        sess.page.wait_for_timeout(500)
    raise RuntimeError(f"未跳到 auth.openai.com（当前 {sess.page.url}）")


def fill_and_submit(sess: Session, selectors: list[str], value: str, label: str) -> None:
    loc = sess.first_visible(selectors, timeout_ms=40_000)
    if loc is None:
        raise RuntimeError(f"找不到 {label} 输入框")
    loc.click()
    try:
        loc.fill("")
    except Exception:
        pass
    # 逐字输入更接近真人（部分风控看键入节奏）。
    loc.type(value, delay=35)
    if not sess.submit():
        try:
            loc.press("Enter")
        except Exception:
            raise RuntimeError(f"{label} 提交按钮未找到")


def run_login(req: dict) -> None:
    """最多尝试 2 次；只有可重试失败（错误页 / 挑战超时）才重试。"""
    last: Exception | None = None
    for attempt in range(1, 3):
        try:
            if _login_attempt(req, attempt):
                return
            return
        except Retryable as exc:
            last = exc
            emit("state", name=f"retry_after_error", detail=str(exc)[:160])
            log(f"attempt {attempt} retryable failure: {exc}")
            time.sleep(3)
        except Exception as exc:  # noqa: BLE001
            emit("error", stage="exception", message=f"{type(exc).__name__}: {exc}")
            return
    emit("error", stage="upstream", message=f"登录失败（重试后仍失败）: {last}")


def render_proxy(template: str, email: str) -> str:
    """渲染 {sid}/{sid8} 占位符：同一账号固定 sid ⇒ 固定出口 IP（复登不跳 IP）。
    host:port:user:pass 形式归一成标准 URL。"""
    text = str(template or "").strip()
    if not text:
        return ""
    if "{" in text:
        sid = hashlib.sha1(str(email or "").strip().lower().encode()).hexdigest()[:8]
        for token in ("{sid8}", "{sid}"):
            text = text.replace(token, sid)
    if "://" not in text:
        parts = text.split(":")
        if len(parts) == 4:
            host, port, user, password = parts
            text = f"http://{quote(user, safe='')}:{quote(password, safe='')}@{host}:{port}"
    return text


def _apply_default_proxy(req: dict) -> None:
    """请求没带代理时用环境默认（PRISM_LOGIN_PROXY，支持 {sid} 模板）。"""
    if str(req.get("proxy") or "").strip():
        return
    tmpl = str(os.environ.get("PRISM_LOGIN_PROXY") or "").strip()
    if not tmpl:
        return
    rendered = render_proxy(tmpl, str(req.get("email") or ""))
    if rendered:
        req["proxy"] = rendered


def _default_state_paths(req: dict) -> None:
    """容器里按账号固定 storage_state（复登复用设备指纹）。
    没显式给路径时，用 PRISM_LOGIN_STATE_DIR/<邮箱>.json 作为 in/out。"""
    state_dir = str(os.environ.get("PRISM_LOGIN_STATE_DIR") or "").strip()
    if not state_dir:
        return
    email = str(req.get("email") or "").strip()
    if not email:
        return
    slug = re.sub(r"[^A-Za-z0-9._-]", "_", email)[:120]
    path = os.path.join(state_dir, slug + ".json")
    try:
        os.makedirs(state_dir, exist_ok=True)
    except OSError:
        return
    req.setdefault("storage_state_out", path)
    if os.path.exists(path):
        req.setdefault("storage_state_in", path)


def _login_attempt(req: dict, attempt: int) -> None:
    email = str(req.get("email") or "").strip()
    password = str(req.get("password") or "")
    totp_secret = str(req.get("totp_secret") or "").strip()
    if not email or not password:
        emit("error", stage="input", message="email/password required")
        return

    timeout = int(req.get("timeout") or 240)
    sess = Session(
        backend=req.get("backend") or "camoufox",
        headless=bool(req.get("headless", True)),
        proxy=req.get("proxy"),
        storage_state_in=req.get("storage_state_in"),
        timeout=timeout,
    )
    try:
        emit("state", name="open_prism", url=PRISM)
        sess.goto(PRISM + "/")
        sess.wait_cf_pass()

        emit("state", name="fetch_authorize")
        try:
            authz = fetch_authorize_url(sess)
        except Exception as exc:  # noqa: BLE001 - 退回 UI 点击
            log(f"fetch_authorize failed ({exc}); fallback to UI click")
            emit("state", name="click_openai_continue")
            click_openai_continue(sess)
            emit("state", name="wait_auth")
            wait_auth_page(sess, timeout_ms=90_000)
        else:
            emit("state", name="goto_authorize")
            sess.goto(authz)
            emit("state", name="wait_auth")
            wait_auth_page(sess, timeout_ms=90_000)
        emit("state", name="openai_login_page", url=sess.page.url)

        # 邮箱：页面上没有邮箱框时，可能是「选择账号」页（共享设备指纹后 OpenAI 认得这台设备）
        # ——先点「使用另一个账号」回到邮箱登录；若已经是密码/验证码页，直接交给后面的循环处理。
        emit("state", name="fill_email")
        if sess.first_visible(EMAIL_SELECTORS, timeout_ms=8_000) is not None:
            fill_and_submit(sess, EMAIL_SELECTORS, email, "email")
            emit("state", name="email_submitted")
        elif click_switch_account(sess) and sess.first_visible(EMAIL_SELECTORS, timeout_ms=10_000) is not None:
            fill_and_submit(sess, EMAIL_SELECTORS, email, "email")
            emit("state", name="email_submitted")
        else:
            title, text = sess.page_title_and_text()
            log(
                f"no email box: url={sess.page.url[:140]} title={title[:60]!r} "
                f"text={' '.join(text.split())[:200]!r}"
            )

        deadline = time.time() + timeout
        totp_done = False
        while time.time() < deadline:
            current = sess.page.url or ""
            # 回调页本身就是成功信号（SPA 不会再跳走）；旧判断只等非回调页会一直等下去。
            if PRISM_HOST_RE.search(current) and (
                "popup-callback" in current or "code=" in current
            ):
                emit("state", name="back_on_prism", url=current.split("&")[0])
                break
            if sess.cookies().get("prism_oai_access_token"):
                emit("state", name="back_on_prism", url=current)
                break
            if sess.is_cf_challenge():
                sess.wait_cf_pass()

            code_loc = sess.first_visible(CODE_SELECTORS, timeout_ms=1_500)
            if code_loc is not None and totp_secret and not totp_done:
                code = totp_code(totp_secret)
                emit("state", name="fill_totp")
                code_loc.click()
                code_loc.type(code, delay=40)
                if not sess.submit():
                    code_loc.press("Enter")
                totp_done = True
                emit("state", name="totp_submitted")
                sess.page.wait_for_timeout(2_000)
                continue

            pass_loc = sess.first_visible(PASSWORD_SELECTORS, timeout_ms=1_500)
            if pass_loc is not None:
                emit("state", name="fill_password")
                pass_loc.click()
                pass_loc.type(password, delay=35)
                if not sess.submit():
                    pass_loc.press("Enter")
                emit("state", name="password_submitted")
                sess.page.wait_for_timeout(2_000)
                continue

            title, text = sess.page_title_and_text()
            low = (title + " " + text).lower()
            for marker in ERROR_PAGE_MARKERS:
                if marker in low:
                    raise Retryable(f"上游错误页 [{marker}]: {title} :: {text[:160]}")
            if any(m in low for m in ("verify your email", "check your inbox", "email verification", "验证码", "邮箱")):
                emit("error", stage="email_otp",
                     message="账号需要邮箱验证码（本侧车不接管邮箱）", url=sess.page.url)
                return

            log(f"waiting url={sess.page.url[:120]} title={sess.page_title_and_text()[0][:60]!r}")
            sess.page.wait_for_timeout(800)

        # 等 prism 落地（弹窗/整页都可能）。上游 origin 会偶发 5xx（CF 顶回来的
        # 「504: Gateway time-out」直接落在回调页上，token 就永远不落 cookie）——
        # 页面卡在 5xx 时重载同一个回调 URL，比干等 60s 强。
        reloads = 0
        for _ in range(60):
            cookies = sess.cookies()
            if cookies.get("prism_oai_access_token"):
                break
            title, _ = sess.page_title_and_text()
            if reloads < 5 and UPSTREAM_5XX_RE.search(title):
                reloads += 1
                log(f"callback 5xx, reload #{reloads}: {title[:60]!r}")
                try:
                    sess.page.reload(wait_until="domcontentloaded", timeout=60_000)
                except Exception:
                    pass
                sess.page.wait_for_timeout(1_000)
                continue
            sess.page.wait_for_timeout(1_000)

        cookies = sess.cookies()
        token = cookies.get("prism_oai_access_token")
        state_path = sess.dump_storage_state(req.get("storage_state_out"))
        if not token:
            emit("error", stage="no_credential",
                 message="登录流程结束了但没拿到 prism_oai_access_token",
                 url=sess.page.url)
            return
        emit("done",
             cookies={k: v for k, v in cookies.items() if k.startswith("prism_") or k in ("oai-sc", "oai-did")},
             storage_state=state_path, url=sess.page.url)
        return True
    except Retryable:
        raise
    except Exception as exc:  # noqa: BLE001 - 侧车要把任何异常回报给主进程
        emit("error", stage="exception", message=f"{type(exc).__name__}: {exc}")
    finally:
        try:
            sess.close()
        except Exception:
            pass
    return


def run_inspect(req: dict) -> None:
    """打开指定页面并打印表单结构，用于修选择器。"""
    sess = Session(
        backend=req.get("backend") or "camoufox",
        headless=bool(req.get("headless", True)),
        proxy=req.get("proxy"),
        storage_state_in=req.get("storage_state_in"),
        timeout=120,
    )
    try:
        url = req.get("url") or (PRISM + "/")
        sess.goto(url)
        sess.wait_cf_pass()
        sess.page.wait_for_timeout(2_000)
        if req.get("fetch_authorize"):
            authz = fetch_authorize_url(sess)
            emit("state", name="goto_authorize")
            sess.goto(authz)
            sess.wait_cf_pass()
            sess.page.wait_for_timeout(3_000)
        for text in req.get("click") or []:
            for role in ("button", "link"):
                loc = sess.page.get_by_role(role, name=re.compile(re.escape(text), re.I)).first
                try:
                    if loc.count() > 0 and loc.is_visible():
                        loc.click()
                        sess.page.wait_for_timeout(3_000)
                        break
                except Exception:
                    continue
        info = sess.page.evaluate(
            """() => ({
                url: location.href,
                title: document.title,
                inputs: [...document.querySelectorAll('input')].map(i => ({
                    name: i.name, type: i.type, id: i.id,
                    ph: i.placeholder, ac: i.autocomplete, vis: !!(i.offsetParent)
                })),
                buttons: [...document.querySelectorAll('button')].map(b => (b.innerText || '').trim()).filter(Boolean).slice(0, 12),
                text: (document.body.innerText || '').slice(0, 300)
            })"""
        )
        emit("done", inspect=info)
    except Exception as exc:  # noqa: BLE001
        emit("error", stage="inspect", message=f"{type(exc).__name__}: {exc}")
    finally:
        try:
            sess.close()
        except Exception:
            pass


def handle(req: dict) -> None:
    action = str(req.get("action") or "login")
    if action == "ping":
        emit("done", pong=True)
    elif action == "inspect":
        run_inspect(req)
    elif action == "sentinel_token":
        run_sentinel_token(req)
    elif action == "chat_material":
        run_chat_material(req)
    elif action == "chat_turn":
        _turn_dispatch(req)
    else:
        run_login(req)


# ---------------------------------------------------------------- sentinel token
#
# 上游对话面（/api/llm/response_with_tools_start）自 2026-09-19 起强制
# openai-sentinel-token header：缺失或复用一律应用层 403（真机实验矩阵实锤，
# 与 IP/账号/请求形状无关）。token 由 sentinel 官方 SDK 在本地 Node 进程里
# 铸造（sentinel_assets/sentinel-runner.js + sdk.js，参考 chatgpt-web 协议栈
# 的 SentinelClient 适配），流程：
#   1) 同一 TLS 会话按 HAR 顺序请求 loader/sdk/frame/frame-sdk（拿 sentinel 域 cookie）
#   2) node runner --challenge-stdin：SDK 本地算 proof（stdout 第一行）
#   3) proof POST /backend-api/sentinel/req 换 challenge
#   4) challenge 喂回 runner stdin → 最终 token（即 header 原文，含 p/t/c/id/flow）
# token 严格一次性、跨账号通用（同出口 IP）。flow=prism_inference 为对话面专属。

SENTINEL_SV = "20260219f9f6"
SENTINEL_ORIGIN = "https://sentinel.openai.com"
SENTINEL_DEFAULT_FLOW = "prism_inference"
SENTINEL_DEFAULT_PAGE = "https://prism.openai.com/"
SENTINEL_ASSETS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "sentinel_assets")

# sentinel 会话复用与并发闸：高并发 mint 时每次新建 Session 会把容器 fd/DNS 打爆
# （线上实测 curl:27 OOM + curl:6 DNS + Errno 24 Too many open files），这里
# 模块级复用同一个 chrome 指纹会话（cookie 在则跳过 assets 预热），并用
# 信号量限制并发（token 铸造 2、材料浏览器 1）。
_SENTINEL_TLS = threading.local()  # curl_cffi Session 非线程安全：必须线程本地，严禁跨线程共享
def _sentinel_conc():
    """sentinel 铸造并发闸（PRISM_SENTINEL_CONCURRENCY，默认 2，上限 16）。
    每路并发一个 node 进程（~100-150MB 内存）+ 两次网络往返；产线吞吐
    ≈ 并发÷单票时长（1~2s）。要冲高 RPM 时与材料工人一起放大。"""
    try:
        n = int(str(os.environ.get("PRISM_SENTINEL_CONCURRENCY") or "2"))
    except ValueError:
        n = 2
    return max(1, min(n, 16))
_SENTINEL_SEM = threading.Semaphore(_sentinel_conc())
_MATERIAL_SEM = None  # 延迟初始化（与 worker 池等宽，见 run_chat_material）


def _sentinel_session(creq, user_agent):
    """线程本地 sentinel 会话（cookie 常驻）；无会话时按 HAR 顺序拉一次 assets。
    曾经做成模块级共享 Session——两个线程并发用它发请求直接连接错乱挂死
    （线上 mint 全部空流的根因），curl_cffi Session 不是线程安全的。"""
    sess = getattr(_SENTINEL_TLS, "sess", None)
    if sess is None:
        sess = creq.Session(impersonate="chrome")
        for url, accept, referer in (
            (f"{SENTINEL_ORIGIN}/backend-api/sentinel/sdk.js", "*/*", SENTINEL_DEFAULT_PAGE),
            (f"{SENTINEL_ORIGIN}/sentinel/{SENTINEL_SV}/sdk.js", "*/*", SENTINEL_DEFAULT_PAGE),
            (f"{SENTINEL_ORIGIN}/backend-api/sentinel/frame.html?sv={SENTINEL_SV}",
             "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8", SENTINEL_DEFAULT_PAGE),
            (f"{SENTINEL_ORIGIN}/sentinel/{SENTINEL_SV}/sdk.js", "*/*",
             f"{SENTINEL_ORIGIN}/backend-api/sentinel/frame.html?sv={SENTINEL_SV}"),
        ):
            resp = sess.get(url, headers={"User-Agent": user_agent, "Accept": accept, "Referer": referer}, timeout=30)
            if resp.status_code not in (200, 304):
                raise RuntimeError(f"asset {url.rsplit('/', 1)[-1]} HTTP {resp.status_code}")
        _SENTINEL_TLS.sess = sess
    return sess


def _sentinel_session_reset():
    """会话疑似失效时丢弃重建（cookie 过期/连接错误时调用）。"""
    try:
        old = getattr(_SENTINEL_TLS, "sess", None)
        if old is not None:
            old.close()
    except Exception:
        pass
    _SENTINEL_TLS.sess = None


# ---------------------------------------------------------------- chat material
#
# 对话材料包供给（2026-09-19 实验矩阵结论）：上游把对话 start 与"页面上下文"
# 强绑定——自建会话+预热沙箱的 body 一律 400 / sandbox_reconnecting，唯一被
# 接受的 body 形态来自浏览器页面（metadata 里的 sandbox_token /
# codex_listen_snapshot / proxy_request_debug）。但材料可跨会话复用（换 conv、
# 换 input、甚至换账号 cookie 都 STARTED），所以一份材料供全池：网关低频
# （分钟级）调本 action 刷新，发送仍走网关自己的纯 HTTP 通道（高并发不经过
# 浏览器）。流程：登录态开项目页 → 输入框发一条消息 → route 拦截 start 偷走
# body（请求被 abort，票据不消费）→ 返回 metadata 全套。

CHAT_MATERIAL_TIMEOUT_MS = 90_000


def run_chat_material(req: dict) -> None:
    import glob as _glob

    state_dir = str(os.environ.get("PRISM_LOGIN_STATE_DIR") or "/data/login-state")
    states = sorted(f for f in _glob.glob(os.path.join(state_dir, "*.json"))
                    if "probe" not in f and "stolen" not in f)
    if not states:
        emit("error", stage="chat_material", message=f"无可用 storage_state: {state_dir}")
        return
    global _MATERIAL_SEM
    if _MATERIAL_SEM is None:
        with _MAT_WORKER_LOCK:
            if _MATERIAL_SEM is None:
                _MATERIAL_SEM = threading.Semaphore(_material_workers_count())
    with _MATERIAL_SEM:  # 与 worker 池等宽：不串行也不超订阅（fd/内存受控）
        # 材料池：state_index 指定用哪个账号的 storage_state（网关按槽位轮转分流）
        try:
            idx = int(req.get("state_index") or 0)
        except (TypeError, ValueError):
            idx = 0
        state_file = states[idx % len(states)]
        if not run_chat_material_playwright(state_file):
            emit("error", stage="chat_material", message=f"浏览器拦截 start 失败（state_index={idx}）")


# 常驻浏览器页（材料产线专用）：每次刷新材料都冷启 chromium 会把容器 CPU 打满
# （线上实测：TTL 收紧后刷新频率升高 → sidecar CPU 154% → mint 全超时雪崩）。
# playwright sync API 绑定创建线程且 with 退出即杀 driver，所以由专用 daemon
# 线程拥有 playwright/浏览器/页面的完整生命周期，请求线程经队列投递。
# 材料池扩容：K 个 worker 并行（PRISM_MATERIAL_WORKERS，默认 4），各自独立
# 浏览器（chromium 多实例各 ~300MB，context 切账号秒级），产线吞吐 = K/30s。
_MAT_QUEUE = None
_MAT_WORKER_STARTED = False
_MAT_WORKER_LOCK = threading.Lock()


def _material_workers_count() -> int:
    try:
        n = int(str(os.environ.get("PRISM_MATERIAL_WORKERS") or "4"))
    except ValueError:
        n = 4
    return max(1, min(n, 16))


def _material_worker(worker_id: int):
    """单线程拥有一个常驻浏览器：循环取队列里的 (state_file, reply_q)，拦材料回传。"""
    import queue as _queue
    from playwright.sync_api import sync_playwright

    pw = sync_playwright().start()
    browser = None
    ctx = None
    page = None
    cur_state = None

    def rebuild(state_file):
        """整浏览器重建（仅初始/坏死时）；换账号只走 switch_state。"""
        nonlocal browser, ctx, page, cur_state
        for obj in (page, ctx, browser):
            try:
                if obj is not None:
                    obj.close()
            except Exception:
                pass
        browser = pw.chromium.launch(headless=True, args=["--no-sandbox"])
        ctx = browser.new_context(locale="zh-CN", storage_state=state_file,
                                  viewport={"width": 1440, "height": 900})
        page = ctx.new_page()
        cur_state = state_file

    def switch_state(state_file):
        """换账号：只重建 context（storage_state 免登录，秒级），浏览器复用。"""
        nonlocal ctx, page, cur_state
        for obj in (page, ctx):
            try:
                if obj is not None:
                    obj.close()
            except Exception:
                pass
        ctx = browser.new_context(locale="zh-CN", storage_state=state_file,
                                  viewport={"width": 1440, "height": 900})
        page = ctx.new_page()
        cur_state = state_file

    while True:
        state_file, reply_q = _MAT_QUEUE.get()
        got: dict[str, str] = {}
        try:
            if browser is None:
                rebuild(state_file)
            elif cur_state != state_file:
                # 材料池轮转：每个槽位对应一个账号的 storage_state
                switch_state(state_file)
            else:
                try:
                    _ = page.title()  # 活性探测
                except Exception:
                    rebuild(state_file)

            def route_start(route):
                if "body" not in got:
                    got["body"] = route.request.post_data or ""
                route.abort()  # 票据不消费；材料里的 token 仍有效（跨请求复用已验证）

            page.route("**/api/llm/response_with_tools_start", route_start)
            page.goto("https://prism.openai.com", wait_until="networkidle",
                      timeout=CHAT_MATERIAL_TIMEOUT_MS)
            page.wait_for_timeout(3000)
            hrefs = page.eval_on_selector_all(
                'a[href*="?u="]', "els => els.map(e => e.getAttribute('href')).slice(0,3)")
            ok = False
            if hrefs:
                target = hrefs[0] if hrefs[0].startswith("http") else "https://prism.openai.com" + hrefs[0]
                page.goto(target, wait_until="networkidle", timeout=CHAT_MATERIAL_TIMEOUT_MS)
                page.wait_for_timeout(8000)
                el = page.query_selector("textarea")
                if el and el.is_visible():
                    el.click(timeout=4000)
                    page.keyboard.type("ping", delay=80)
                    page.keyboard.press("Enter")
                    page.wait_for_timeout(7000)
                    ok = True
            if not ok or "body" not in got:
                reply_q.put(None)
                continue
            try:
                body = json.loads(got["body"])
                meta = body.get("metadata") or {}
                cookie = "; ".join(f"{c['name']}={c['value']}"
                                   for c in ctx.cookies(["https://prism.openai.com"]))
                if not (isinstance(meta, dict) and meta.get("sandbox_token") and cookie):
                    reply_q.put(None)
                    continue
                reply_q.put({"metadata": meta, "conversation_id": body.get("conversationId", ""),
                             "cookie": cookie})
            except json.JSONDecodeError:
                reply_q.put(None)
        except Exception as exc:  # noqa: BLE001
            log(f"chat_material worker{worker_id}: {type(exc).__name__}: {exc}")
            try:
                rebuild(state_file)  # 页面可疑坏死：重建
            except Exception:
                pass
            reply_q.put(None)


def run_chat_material_playwright(state_file: str) -> bool:
    """向常驻浏览器 worker 池要一份新材料（metadata+全套 cookie），emit 给调用方。"""
    import queue as _queue

    global _MAT_QUEUE, _MAT_WORKER_STARTED
    with _MAT_WORKER_LOCK:
        if not _MAT_WORKER_STARTED:
            _MAT_QUEUE = _queue.Queue()
            for i in range(_material_workers_count()):
                threading.Thread(target=_material_worker, args=(i,),
                                 daemon=True, name=f"mat-worker{i}").start()
            _MAT_WORKER_STARTED = True
    reply = _queue.Queue()
    _MAT_QUEUE.put((state_file, reply))
    try:
        material = reply.get(timeout=150)
    except _queue.Empty:
        material = None
    if not material:
        return False
    emit("done", material=material)
    return True


def run_sentinel_token(req: dict) -> None:
    try:
        from curl_cffi import requests as creq  # 容器镜像预装；本地缺库时给出明确错误
    except ImportError as exc:
        emit("error", stage="sentinel_init", message=f"curl_cffi 不可用: {exc}")
        return
    import shutil
    import subprocess
    import uuid

    flow = str(req.get("flow") or SENTINEL_DEFAULT_FLOW)
    page_url = str(req.get("page_url") or SENTINEL_DEFAULT_PAGE)
    device_id = str(req.get("device_id") or uuid.uuid4())
    user_agent = str(req.get("user_agent") or
                     "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 "
                     "(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0")
    node = shutil.which("node")
    if not node:
        emit("error", stage="sentinel_init", message="node 不在 PATH（sentinel SDK 需要 Node ≥ 20.20）")
        return
    runner = os.path.join(SENTINEL_ASSETS, "sentinel-runner.js")
    sdk = os.path.join(SENTINEL_ASSETS, "sdk.js")
    if not (os.path.isfile(runner) and os.path.isfile(sdk)):
        emit("error", stage="sentinel_init", message=f"sentinel 资产缺失: {SENTINEL_ASSETS}")
        return

    with _SENTINEL_SEM:  # 并发闸：同时至多 2 个铸造（node 进程 + 网络往返）
        _run_sentinel_mint(req, flow, page_url, device_id, user_agent, node, runner, sdk, creq)


def _run_sentinel_mint(req, flow, page_url, device_id, user_agent, node, runner, sdk, creq):
    import subprocess

    try:
        sess = _sentinel_session(creq, user_agent)
        emit("state", name="sentinel_proof")
        proc = subprocess.Popen(
            [node, runner, "--challenge-stdin",
             "--flow", flow, "--device-id", device_id, "--page-url", page_url,
             "--user-agent", user_agent, "--sdk", sdk,
             "--script-src", f"{SENTINEL_ORIGIN}/sentinel/{SENTINEL_SV}/sdk.js",
             "--width", "1920", "--height", "1080", "--cores", "32",
             "--language", "zh-CN", "--languages", "zh-CN,zh,en-US,en", "--no-cookie"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            env=dict(os.environ, SENTINEL_CONFIG="__none__"))
        # 看门狗：runner 偶发挂起不吐 proof（线上实测堆积卡死、占死并发闸与 mint 全线
        # 空流），55s 强杀让阻塞的 readline/read 收到 EOF 走异常路径，闸门必被释放。
        threading.Timer(55, lambda: proc.poll() is None and proc.kill()).start()
        proof_line = proc.stdout.readline().decode()
        proof_msg = json.loads(proof_line)
        if proof_msg.get("type") != "proof" or not proof_msg.get("proof"):
            raise RuntimeError(f"runner proof 输出异常: {proof_line[:120]}")

        emit("state", name="sentinel_challenge")
        resp = sess.post(
            f"{SENTINEL_ORIGIN}/backend-api/sentinel/req",
            headers={"User-Agent": user_agent, "Accept": "*/*",
                     "Content-Type": "text/plain;charset=UTF-8",
                     "Origin": SENTINEL_ORIGIN,
                     "Referer": f"{SENTINEL_ORIGIN}/backend-api/sentinel/frame.html?sv={SENTINEL_SV}"},
            data=json.dumps({"p": proof_msg["proof"], "id": device_id, "flow": flow},
                            separators=(",", ":")),
            timeout=30)
        if resp.status_code != 200:
            _sentinel_session_reset()  # 会话疑似失效：丢弃重建（下次拉 assets）
            raise RuntimeError(f"sentinel/req HTTP {resp.status_code}: {resp.text[:120]}")
        challenge = resp.json()
        if not challenge.get("token"):
            raise RuntimeError(f"sentinel/req 缺 token: {str(challenge)[:120]}")

        proc.stdin.write((json.dumps({"type": "challenge", "challenge": challenge},
                                     separators=(",", ":")) + "\n").encode())
        proc.stdin.flush()
        proc.stdin.close()
        out = proc.stdout.read()
        errb = proc.stderr.read()
        proc.wait(timeout=60)  # 看门狗已兜底：runner 挂起时 kill 后 read 已 EOF 返回
        token = None
        for line in out.decode().splitlines():
            try:
                msg = json.loads(line)
            except json.JSONDecodeError:
                continue
            if msg.get("type") == "token" and msg.get("token"):
                token = msg["token"]
        if not token:
            raise RuntimeError(f"runner 未产出 token: {errb.decode()[:200]}")
        emit("done", token=token, device_id=device_id, flow=flow)
    except Exception as exc:  # noqa: BLE001
        emit("error", stage="sentinel_token", message=f"{type(exc).__name__}: {exc}")


# ---------------------------------------------------------------- HTTP 服务模式
#
# 线上（docker）里侧车与网关是两个容器：网关通过 HTTP 驱动它（PRISM_LOGIN_SIDECAR_URL）。
# 事件协议与 stdin/stdout 模式完全一致，只是按 NDJSON 逐行回。
#
#   GET  /healthz   → {"status":"ok","backend":…,"inflight":N}
#   POST /login     → 请求体同 stdin 模式的 login 请求；响应 application/x-ndjson，
#                     逐行输出同样的 state/done/error 事件


def _serve(port: int, backend: str, max_concurrent: int) -> int:
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    sem = threading.Semaphore(max(1, max_concurrent))
    state = {"inflight": 0}
    lock = threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, fmt, *args):  # 收进 stderr，别污染事件流
            log("http: " + fmt % args)

        def _json(self, code: int, payload: dict) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):  # noqa: N802
            if self.path.split("?")[0] in ("/healthz", "/"):
                with lock:
                    inflight = state["inflight"]
                self._json(200, {"status": "ok", "backend": backend, "inflight": inflight})
                return
            self._json(404, {"error": "not found"})

        def do_POST(self):  # noqa: N802
            if self.path.split("?")[0] not in ("/login", "/run"):
                self._json(404, {"error": "not found"})
                return
            length = int(self.headers.get("Content-Length") or 0)
            raw = self.rfile.read(length) if length else b""
            try:
                req = json.loads(raw or b"{}")
            except json.JSONDecodeError as exc:
                self._json(400, {"error": f"invalid json: {exc}"})
                return
            req.setdefault("backend", backend)
            _apply_default_proxy(req)
            _default_state_paths(req)
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Cache-Control", "no-cache")
            # 长度未知：用 chunked，边跑边发
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()

            def write_line(text: str) -> None:
                data = (text + "\n").encode()
                chunk = b"%x\r\n%s\r\n" % (len(data), data)
                try:
                    self.wfile.write(chunk)
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    pass

            # 轻量/独立浏览器 action 不占登录信号量：真登录一次占浏览器
            # 几分钟，max_concurrent=2 的槽被并发登录吃满时这些调用会排队
            # 饿死（线上实测：mint 60s 超时读不到 done.token）。
            if str(req.get("action") or "login") in ("sentinel_token", "chat_material", "chat_turn"):
                _SINK.write = write_line
                try:
                    handle(req)
                except Exception as exc:  # noqa: BLE001
                    write_line(json.dumps({"event": "error", "stage": "dispatch",
                                           "message": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
                finally:
                    _SINK.write = None
                    try:
                        self.wfile.write(b"0\r\n\r\n")  # chunked 结束
                        self.wfile.flush()
                    except (BrokenPipeError, ConnectionResetError):
                        pass
                return

            sem.acquire()
            with lock:
                state["inflight"] += 1
            _SINK.write = write_line
            try:
                handle(req)
            except Exception as exc:  # noqa: BLE001
                write_line(json.dumps({"event": "error", "stage": "dispatch",
                                       "message": f"{type(exc).__name__}: {exc}"}, ensure_ascii=False))
            finally:
                _SINK.write = None
                with lock:
                    state["inflight"] -= 1
                sem.release()
                try:
                    self.wfile.write(b"0\r\n\r\n")  # chunked 结束
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    pass

    srv = ThreadingHTTPServer(("0.0.0.0", port), Handler)
    log(f"serve on :{port} backend={backend} max_concurrent={max(1, max_concurrent)}")
    srv.serve_forever()
    return 0


def main() -> int:
    backend_default = str(os.environ.get("PRISM_LOGIN_BACKEND") or "camoufox").strip() or "camoufox"
    argv = sys.argv[1:]
    if "--backend" in argv:
        backend_default = argv[argv.index("--backend") + 1]
    if "--serve" in argv:
        port = int(argv[argv.index("--port") + 1]) if "--port" in argv else 8099
        if "--max-concurrent" in argv:
            maxc = int(argv[argv.index("--max-concurrent") + 1])
        else:
            try:
                maxc = int(str(os.environ.get("PRISM_LOGIN_MAX_CONCURRENT") or "2"))
            except ValueError:
                maxc = 2
        return _serve(port, backend_default, maxc)
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
        except json.JSONDecodeError as exc:
            emit("error", stage="parse", message=str(exc))
            continue
        req.setdefault("backend", backend_default)
        _apply_default_proxy(req)
        _default_state_paths(req)
        try:
            handle(req)
        except Exception as exc:  # noqa: BLE001
            emit("error", stage="dispatch", message=f"{type(exc).__name__}: {exc}")
    return 0


# ---------------------------------------------------------------- chat turn v2（async 常驻引擎）
#
# 500RPM 架构（2026-09-21 SDK 票破局后定稿）：
#   票 = 页面原生 SentinelSDK.token('prism_inference')（130ms/张，白嫖浏览器供票，
#        外部 node 铸票下岗——产能从 ~10张/s 到 50+张/s）
#   发 = ctx.request（浏览器网络栈，自动全套 cookie/指纹）
#   常驻 = N 部浏览器各进项目页待命（SDK 就绪），asyncio 并发多 turn/浏览器
#          （轮询全异步不占串行时间——旧版每 turn 冷启浏览器 + 串行轮询只有 10RPM）
# 流程：借引擎 → SDK票+ctx.request start → 异步自适应轮询 → payload.output 提取

TURN_WORKERS_DEFAULT = 6          # 常驻浏览器数（PRISM_TURN_WORKERS 可调）
TURN_TURN_TIMEOUT = 180           # 单轮对话总时限(秒)
TURN_STATUS_INTERVAL = 6          # 轮询间隔(秒)——生产最优档:家宽池+6s=157RPM@100%(4s=151@92%,密集轮询反触残余限流)

_TURN_ENGINE = None
_TURN_ENGINE_LOCK = threading.Lock()
_TURN_QUEUE = None  # asyncio.Queue 的线程代理由 run_coroutine_threadsafe 实现


def _turn_proxies():
    """家宽代理池（PRISM_TURN_PROXIES，逗号/空格分隔 host:port:user:pass）。
    每 worker 绑定一个出口 IP——上游 429 是按 IP 的总请求率限流（309RPM 档
    实锤 1201 次 429），出口打散后限流矩阵解除。空=直连（行为不变）。"""
    raw = str(os.environ.get("PRISM_TURN_PROXIES") or "").strip()
    out = []
    for item in raw.replace("\n", ",").replace(" ", ",").split(","):
        item = item.strip()
        if not item:
            continue
        parts = item.split(":")
        if len(parts) != 4:
            log(f"turn proxy 格式忽略(要 host:port:user:pass): {item[:30]}")
            continue
        out.append({"server": f"http://{parts[0]}:{parts[1]}", "username": parts[2], "password": parts[3]})
    return out


_LOCAL_EGRESS = None


def _local_egress() -> str:
    """本机出口 IP（自检基准，进程内取一次）。"""
    global _LOCAL_EGRESS
    if _LOCAL_EGRESS is None:
        try:
            import urllib.request as _ur
            _LOCAL_EGRESS = _ur.urlopen("https://api.ipify.org", timeout=10).read().decode().strip()
        except Exception:
            _LOCAL_EGRESS = "?"
    return _LOCAL_EGRESS


def _worker_proxy(worker_id):
    """取 worker 绑定的代理配置；模板含 {sid} 时替换成 turn{worker_id}——
    arxlabs 轮转家宽（不同 sid=不同出口 IP，同 sid 粘性），12 worker 即 12 个
    独立出口，上游按 IP 的 429 限流矩阵彻底打散（2026-09-21 实测 6 sid 6 IP）。"""
    proxies = _turn_proxies()
    if not proxies:
        return None
    cfg = dict(proxies[worker_id % len(proxies)])
    # sid 带容器标识（HOSTNAME 短哈希）：双 login 容器同 worker_id 也不撞出口
    import hashlib as _hl
    box = _hl.md5(os.environ.get("HOSTNAME", "x").encode()).hexdigest()[:4]
    sid = f"turn{box}{worker_id}"
    cfg["username"] = cfg.get("username", "").replace("{sid}", sid)
    cfg["password"] = cfg.get("password", "").replace("{sid}", sid)
    cfg["server"] = cfg.get("server", "").replace("{sid}", sid)
    return cfg


def _turn_workers_n() -> int:
    try:
        n = int(str(os.environ.get("PRISM_TURN_WORKERS") or TURN_WORKERS_DEFAULT))
    except ValueError:
        n = TURN_WORKERS_DEFAULT
    return max(1, min(n, 16))


def run_coro(coro, timeout=None):
    """把协程投递到常驻引擎的 event loop（线程安全）。"""
    return asyncio.run_coroutine_threadsafe(coro, _TURN_ENGINE["loop"]).result(timeout)


async def _sdk_token(page, flow="prism_inference"):
    """页面原生取票（130-190ms/张）。"""
    t = await page.evaluate(
        "async (f) => { const t = await window.SentinelSDK.token(f); return typeof t === 'string' ? t : JSON.stringify(t); }",
        flow)
    return str(t)


async def _enter_project_page(page, state_file_hint=None):
    """进项目页并等 SentinelSDK 就绪（冷启/重连后用）。"""
    for attempt in range(3):
        await page.goto("https://prism.openai.com", wait_until="networkidle", timeout=60000)
        await page.wait_for_timeout(4000 if attempt == 0 else 9000)
        hrefs = await page.eval_on_selector_all(
            'a[href*="?u="]', "els => els.map(e => e.getAttribute('href')).slice(0,3)")
        if hrefs:
            break
    else:
        raise RuntimeError("项目列表为空(重试3次)")
    target = hrefs[0] if hrefs[0].startswith("http") else "https://prism.openai.com" + hrefs[0]
    await page.goto(target, wait_until="networkidle", timeout=60000)
    for _ in range(20):  # 等 SDK（最长 ~20s）
        ready = await page.evaluate("() => typeof window.SentinelSDK === 'object' && typeof window.SentinelSDK.token === 'function'")
        if ready:
            return
        await page.wait_for_timeout(1000)
    raise RuntimeError("SentinelSDK 未就绪")


async def _worker_loop(worker_id, states):
    """一部常驻浏览器：循环领 turn 任务，asyncio 并发跑（每任务一个协程）。"""
    from playwright.async_api import async_playwright

    state_file = states[worker_id % len(states)]
    _local_egress()  # 预热本机出口基准
    proxy_cfg = _worker_proxy(worker_id)
    async with async_playwright() as pw:
        browser = await pw.chromium.launch(headless=True, args=["--no-sandbox"])
        ctx = None
        for attempt in range(3):
            try:
                ctx = await browser.new_context(locale="zh-CN", storage_state=state_file,
                                                viewport={"width": 1440, "height": 900},
                                                proxy=proxy_cfg)
                if proxy_cfg:
                    # 出口自检：出口不是本机 IP 即代理生效（sid 轮转模板下出口 IP
                    # 与 server 域名无关，不能拿 server 字符串比对——旧逻辑误杀全部轮转出口）
                    probe = await ctx.new_page()
                    egress = await probe.evaluate("() => fetch('https://api.ipify.org').then(r => r.text()).catch(() => '')")
                    await probe.close()
                    if egress and egress != _LOCAL_EGRESS:
                        break
                    log(f"turn worker{worker_id}: 代理出口异常(egress={egress[:20]}), "
                        f"{'第' + str(attempt + 1) + '次重试' if attempt < 2 else '回退直连'}")
                    await ctx.close()
                    ctx = None
                    if attempt == 2:
                        proxy_cfg = None
                        continue
                else:
                    break
            except Exception as exc:
                log(f"turn worker{worker_id}: 代理建context失败: {type(exc).__name__}")
                await asyncio.sleep(3)
                if attempt == 2:
                    proxy_cfg = None
                    ctx = await browser.new_context(locale="zh-CN", storage_state=state_file,
                                                    viewport={"width": 1440, "height": 900})
        if ctx is None:
            ctx = await browser.new_context(locale="zh-CN", storage_state=state_file,
                                            viewport={"width": 1440, "height": 900})
        page = await ctx.new_page()
        tpl = {}

        def _route(route):
            if "body" not in tpl:
                tpl["body"] = route.request.post_data or ""
            return route.abort()

        await page.route("**/api/llm/response_with_tools_start", _route)
        try:
            await _enter_project_page(page)
            # 拦一份模板（触发一次页面消息）；失败不致命——模板可由其他 worker 供
            for ping in range(2):
                try:
                    el = await page.wait_for_selector("textarea", state="visible", timeout=15000)
                    await el.click(timeout=8000)
                    await page.keyboard.type(f"init{ping}", delay=50)
                    await page.keyboard.press("Enter")
                    await page.wait_for_timeout(6000)
                except Exception as exc:
                    log(f"turn worker{worker_id}: 拦截尝试{ping}失败: {type(exc).__name__}")
                if tpl.get("body"):
                    break
                await _enter_project_page(page)
        except Exception as exc:
            log(f"turn worker{worker_id}: 启动异常(继续空模板待命): {type(exc).__name__}: {exc}")
        egress_tag = ''
        try:
            probe = await ctx.new_page()
            egress_tag = await probe.evaluate("() => fetch('https://api.ipify.org').then(r => r.text()).catch(() => '')")
            await probe.close()
        except Exception:
            egress_tag = '?'
        log(f"turn worker{worker_id}: 就绪 (state={state_file.rsplit('/', 1)[-1][:18]}, tpl={'Y' if tpl.get('body') else 'N'}, egress={str(egress_tag)[:20]})")

        # 无模板不自愈前不领活（部分账号项目被上游清、首页为空——重进直到拿到）
        for heal in range(60):
            if tpl.get("body"):
                break
            await asyncio.sleep(20)
            try:
                await _enter_project_page(page)
                el = await page.wait_for_selector("textarea", state="visible", timeout=15000)
                await el.click(timeout=8000)
                await page.keyboard.type(f"h{heal}", delay=50)
                await page.keyboard.press("Enter")
                await page.wait_for_timeout(6000)
                if tpl.get("body"):
                    log(f"turn worker{worker_id}: 自愈拿到模板")
            except Exception:
                pass
        if not tpl.get("body"):
            log(f"turn worker{worker_id}: 60次自愈失败, 退出(其余worker承担)")
            return

        while True:
            task = await _TURN_QUEUE.get()
            if task is None:
                break
            job, reply_q = task
            # 每 turn 独立协程并发：worker 串行领活，turn 并发跑
            asyncio.get_event_loop().create_task(_run_turn(page, ctx, tpl, job, reply_q, worker_id))
        await browser.close()


async def _run_turn(page, ctx, tpl, job, reply_q, worker_id):
    """单个 turn：SDK票 start → 自适应轮询 → 提取 → 回传。"""
    def done(**kw):
        try:
            reply_q.put({"ok": True, **kw})
        except Exception:
            pass

    def fail(msg):
        try:
            reply_q.put({"ok": False, "error": msg})
        except Exception:
            pass

    try:
        import uuid as _u
        body = json.loads(tpl["body"]) if tpl.get("body") else {}
        if not body:
            return fail("worker 无模板")
        body["conversationId"] = "cdx1_" + str(_u.uuid4())
        body.pop("previousResponseId", None)
        body["input"] = job.get("input") or []
        meta = dict(body.get("metadata") or {})
        meta["model"] = job.get("model") or "gpt-5.6-sol"
        meta["reasoning_effort"] = job.get("effort") or "medium"
        body["metadata"] = meta

        # start 带退避重试（上游 429 限流：等 2/4/6s 重试，共 3 次）
        r = None
        st = {}
        for attempt in range(3):
            tok = await _sdk_token(page)
            r = await ctx.request.post(
                "https://prism.openai.com/api/llm/response_with_tools_start",
                headers={"openai-sentinel-token": tok, "content-type": "application/json"},
                data=json.dumps(body), timeout=30000)
            try:
                st = await r.json()
            except Exception:
                st = {}
            if r.status == 200 and st.get("status") == "started":
                break
            soft_reject = r.status == 200 and st.get("status") == "completed"
            if r.status == 429 or soft_reject:
                await asyncio.sleep(2 + attempt * 2)
                continue
            # 200+completed=上游秒拒，完整 payload 里才有真实原因
            err_txt = (await r.text())[:400]
            if st.get("status") == "completed":
                p = ((st.get("response") or {}).get("payload") or {})
                err_txt = str(p.get("message") or err_txt)[:250]
            return fail(f"start HTTP {r.status}/{st.get('status')}: {err_txt}")
        if r is None or st.get("status") != "started":
            return fail(f"start 重试耗尽(429): HTTP {getattr(r, 'status', '?')}")
        rid = st.get("request_id", "")
        turn_state = st.get("turn_state") or {}

        deadline = time.time() + TURN_TURN_TIMEOUT
        while time.time() < deadline:
            await asyncio.sleep(TURN_STATUS_INTERVAL)
            ptok = await _sdk_token(page)
            pr = await ctx.request.post(
                "https://prism.openai.com/api/llm/response_with_tools_status",
                headers={"openai-sentinel-token": ptok, "content-type": "application/json"},
                data=json.dumps({"request_id": rid, "turn_state": turn_state}), timeout=30000)
            j = {}
            try:
                j = await pr.json()
            except Exception:
                continue
            if j.get("status") == "completed":
                resp = j.get("response") or {}
                if resp.get("status") == "error":
                    payload = (resp.get("payload") or {}).get("message", "") or json.dumps(resp)[:150]
                    return fail(str(payload)[:200])
                pobj = resp.get("payload") if isinstance(resp.get("payload"), dict) else {}
                outs = pobj.get("output") or resp.get("output") or []
                texts, reasoning = [], []
                for it in outs:
                    if not isinstance(it, dict):
                        continue
                    if it.get("type") == "reasoning":
                        for s in (it.get("summary") or []):
                            if isinstance(s, dict) and s.get("text"):
                                reasoning.append(s["text"])
                    if it.get("type") == "message":
                        for c in (it.get("content") or []):
                            if isinstance(c, dict) and c.get("type") in ("output_text", "text"):
                                texts.append(c.get("text", ""))
                return done(text="".join(texts), reasoning="".join(reasoning),
                            conversation_id=body["conversation_id"] if "conversation_id" in body else body["conversationId"])
        return fail(f"轮询超时({TURN_TURN_TIMEOUT}s)")
    except Exception as exc:  # noqa: BLE001
        return fail(f"{type(exc).__name__}: {exc}")


def _turn_engine_loop():
    """常驻引擎线程：own event loop，起 N 个 worker。"""
    global _TURN_QUEUE
    asyncio.set_event_loop(asyncio.new_event_loop())
    loop = asyncio.get_event_loop()
    _TURN_ENGINE["loop"] = loop
    _TURN_QUEUE = asyncio.Queue()
    states = sorted(f for f in glob.glob('/data/login-state/*.json')
                    if 'probe' not in f and 'stolen' not in f and 'pool_cred' not in f)
    async def _supervised(i):
        while True:  # worker 崩溃自动重启（单点异常不炸引擎）
            try:
                await _worker_loop(i, states)
            except Exception as exc:
                log(f"turn worker{i} 崩溃重启: {type(exc).__name__}: {exc}")
                await asyncio.sleep(10)
    loop.run_until_complete(asyncio.gather(*[_supervised(i) for i in range(_turn_workers_n())]))


def _ensure_turn_engine():
    with _TURN_ENGINE_LOCK:
        global _TURN_ENGINE
        if _TURN_ENGINE is not None:
            return
        _TURN_ENGINE = {"loop": None}
        threading.Thread(target=_turn_engine_loop, daemon=True, name="turn-engine").start()
        # 等 loop 就绪
        for _ in range(150):
            if _TURN_ENGINE.get("loop") is not None:
                break
            time.sleep(0.2)


def _turn_dispatch(req: dict) -> None:
    """chat_turn v2：投递到 async 常驻引擎，NDJSON 流式回报。"""
    import queue as _q
    _ensure_turn_engine()
    input_items = req.get("input") or []
    if not input_items:
        emit("error", stage="chat_turn", message="input 为空")
        return
    emit("state", name="turn_queued")
    reply = _q.Queue()

    async def _submit():
        await _TURN_QUEUE.put(({"input": input_items,
                                "model": req.get("model") or "gpt-5.6-sol",
                                "effort": req.get("effort") or "medium"}, reply))
    fut = asyncio.run_coroutine_threadsafe(_submit(), _TURN_ENGINE["loop"])
    fut.result(timeout=10)
    try:
        result = reply.get(timeout=TURN_TURN_TIMEOUT + 30)
    except _q.Empty:
        emit("error", stage="chat_turn", message="排队超时(引擎忙)")
        return
    if not result.get("ok"):
        emit("error", stage="chat_turn", message=str(result.get("error"))[:220])
        return
    emit("done", text=result.get("text", ""), reasoning=result.get("reasoning", ""),
         conversation_id=result.get("conversation_id", ""))


def creq_session_for_turn():
    from curl_cffi import requests as _creq
    return _creq.Session(impersonate="chrome")


if __name__ == "__main__":
    raise SystemExit(main())
