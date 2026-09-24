#!/usr/bin/env python3
"""到期前自动续命（零依赖，跑在 prism-login 容器里，cron 每 2 小时一次）。

策略（login-loop v2 同款门控）：
- 只处理 expires_at 在未来 WINDOW_HOURS 内的账号（窗口外直接退出，零成本；
  expires_at=0 的「待首登」凭据束号不参与，也不毒化统计）；
- 先试登窗口里最急的候选（至多 3 个，个体失败换号、CF 全局风控才放弃）：
  成功才续其余（Cloudflare 挡时一轮只花 1 个浏览器会话）；
- FORCE=1 无视窗口（手工验证用：FORCE_BATCH=1 只续 1 个）。

cron（服务器 root，每 2 小时一轮、批 24，覆盖 216 账号/72h 窗口的产能）：
  17 */2 * * * docker exec -i -e ADMIN_PASS='xxx' -e BATCH=24 prism-login \
    python3 - < /opt/prism-2api/scripts/renew_loop.py >> /opt/prism-2api/data/renew-loop.log 2>&1
"""
import json
import os
import sys
import time
import urllib.request

BASE = os.environ.get("PRISM_URL", "http://prism-2api:8080").rstrip("/")
USER = os.environ.get("ADMIN_USER", "admin")
PASS = os.environ.get("ADMIN_PASS", "")
WINDOW_SEC = int(os.environ.get("WINDOW_HOURS", "72")) * 3600
BATCH = int(os.environ.get("FORCE_BATCH", "") or os.environ.get("BATCH", "8"))
FORCE = os.environ.get("FORCE") == "1"
TASK_TIMEOUT = 900  # 单批登录任务基线（浏览器 1~4 分钟/号，按批量伸缩，见 run_login_task）


def log(msg):
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}", flush=True)


def req(method, path, body=None, cookie=None):
    r = urllib.request.Request(BASE + path, method=method)
    if body is not None:
        r.data = json.dumps(body).encode()
        r.add_header("Content-Type", "application/json")
    if cookie:
        r.add_header("Cookie", cookie)
    with urllib.request.urlopen(r, timeout=30) as resp:
        return json.loads(resp.read()), resp.headers


def login_admin():
    _, headers = req("POST", "/api/admin/login", {"username": USER, "password": PASS})
    sc = headers.get_all("Set-Cookie") or []
    for c in sc:
        if c.startswith("c2a_session="):
            return c.split(";")[0]
    raise RuntimeError("admin 登录没拿到 c2a_session")


def run_login_task(cookie, names):
    """提交一批账号重登并等结果，返回 {name: ok}。"""
    # 批量越大等待越久：并发 2 浏览器 × 每号 1~4 分钟 → 每号 150s 上界足够宽。
    timeout = max(TASK_TIMEOUT, len(names) * 150)
    payload = {"names": names}
    d, _ = req("POST", "/api/admin/accounts/login", payload, cookie=cookie)
    tid = (d or {}).get("task_id") or (d or {}).get("id")
    if not tid:
        raise RuntimeError(f"任务提交失败: {d}")
    deadline = time.time() + timeout
    while time.time() < deadline:
        time.sleep(10)
        d, _ = req("GET", f"/api/admin/tasks/{tid}", cookie=cookie)
        st = (d or {}).get("status")
        if st in ("done", "failed", "canceled"):
            results = (d or {}).get("results") or {}
            return {n: bool(results.get(n)) for n in names}
    raise RuntimeError(f"任务 {tid} 超时")


def main():
    if not PASS:
        sys.exit("ADMIN_PASS 未设")
    cookie = login_admin()
    d, _ = req("GET", "/api/admin/accounts", cookie=cookie)
    now = time.time()
    horizon = now + WINDOW_SEC
    # 只认有效到期时间：池里有 expires_at=0 的「待首登」凭据束号（线上实测
    # 114 个），既不参与续命候选，也不能毒化 min() 统计（2026-09-20 前
    # renew-loop.log 连续多轮「最近到期还有 -20716 天」即此病）。
    def exp_of(a):
        e = a.get("expires_at") or 0
        return e if e > 0 else None
    expiring = sorted(
        (a for a in d.get("accounts", []) if (e := exp_of(a)) is not None and e < horizon),
        key=lambda a: exp_of(a))
    if not FORCE and not expiring:
        known = [e for a in d["accounts"] if (e := exp_of(a))]
        if known:
            log(f"窗口外（最近到期还有 {(min(known) - now) / 86400:.1f} 天），退出")
        else:
            log("窗口外（无有效 expires_at 记录），退出")
        return
    if FORCE:
        # FORCE：取最急的 1 个（不管窗口）
        expiring = sorted((a for a in d.get("accounts", []) if exp_of(a)),
                          key=lambda a: exp_of(a))
    if not expiring:
        log("没有带有效 expires_at 的账号，退出")
        return

    # probe 试登最多 3 个候选：首个失败可能是账号个体问题（凭据失效/风控残留），
    # 直接放弃会让整轮续命白跑；换号重试把「个体失败」与「CF 全局风控」分开。
    probe_ok, tried = None, []
    for probe in expiring[:3]:
        log(f"窗口内 {len(expiring)} 个账号（最早 {time.strftime('%m-%d %H:%M', time.localtime(exp_of(probe)))} 到期），"
            f"试登 {probe['name']}")
        res = run_login_task(cookie, [probe["name"]])
        if res.get(probe["name"]):
            log(f"试登成功 {probe['name']}")
            probe_ok = probe
            break
        tried.append(probe["name"])
        log(f"试登失败 {probe['name']}，换下一个候选")
    if probe_ok is None:
        log("连续 3 个候选试登失败（Cloudflare 风控期或凭据批量失效），本轮退出，不放大")
        return

    rest = [a["name"] for a in expiring if a["name"] not in tried][:BATCH]
    if not rest:
        log("本轮只续了探针这 1 个")
        return
    log(f"续登 {len(rest)} 个：{', '.join(rest)}")
    res = run_login_task(cookie, rest)
    ok = sum(1 for v in res.values() if v)
    log(f"本轮完成：续登 {ok}/{len(rest)}（+ 探针 1）")


if __name__ == "__main__":
    main()
