#!/usr/bin/env python3
"""prism-2api 容量探针：一个账号到底能吃多少 RPM（对应 docs/TESTPLAN.md §6）。

零依赖，只用标准库。用法：

    # 1) 先干跑，只看阶梯与请求预算，不发任何请求（**先跑这个**）
    python3 scripts/rpmprobe.py --plan

    # 2) 真跑：并发阶梯 1/2/4，每档 60 秒
    WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=xxx \
      python3 scripts/rpmprobe.py --admin-user admin --admin-pass xxx \
      --steps 1,2,4 --duration 60

设计要点（为什么不是一个"并发 50 猛冲"的压测）：

0. **本地不限流**：`prism-2api` 不做任何自设的并发/RPM 闸门
   （`account_concurrency=0`、`account_concurrency_429=0`、`max_in_flight=0`，全部 = 不限制），
   所以阶梯上测到的失败**都是上游的**，不存在"把自己的限流误读成上游天花板"。
1. **唯一的账号只有 1 个**，一旦触发上游 429，该号会被冷却
   `60s × 1.5^(n-1)`（封顶 5 分钟，见 internal/failclass/class.go），
   粗暴压测会把整个测试窗口烧掉。所以走**阶梯 + 安全阀**。
2. 每档前先读管理台 `/api/admin/dashboard`：若账号在冷却中，**拒绝开跑并等待**。
3. 每档后比对 `by_fail_class` 增量与 `cooldowns`，把"是哪一类失败先出现"记下来——
   这比一个孤立的 RPM 数字有用得多。
4. 每个客户端请求在上游是 **1 次 start + N 次 status 轮询**（N ≈ 单轮耗时 / 1.5s），
   所以"客户端 RPM"与"上游 RPM"是两个数，脚本给的是前者，后者见 TESTPLAN §6。
5. 直连网关：本机常带 HTTP_PROXY，并发时它吐 502，会污染结论（同 scripts/smoke.py）。

退出码：0 = 全部档位跑完且未触发冷却；1 = 中途触发冷却/错误率超阈值而中止。
"""

from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

BASE = os.environ.get("WEB2API_URL", "http://127.0.0.1:8080").rstrip("/")
KEY = os.environ.get("WEB2API_API_KEY", "")

client = urllib.request.build_opener(urllib.request.ProxyHandler({}))
admin = urllib.request.build_opener(
    urllib.request.ProxyHandler({}),
    urllib.request.HTTPCookieProcessor(),
)

# 默认短答提示词：把单轮生成时间压到最低，让测出来的数字逼近"调度/上游"上限，
# 而不是被模型生成速度掩盖。--prompt long 跑对照，看生成时长如何稀释 RPM。
PROMPT_SHORT = "只回复 OK 两个字，不要解释，不要标点。"
PROMPT_LONG = "用中文写一段 800 字左右的短文，主题是海水为什么是咸的，要分段。"

# 安全阀
ERR_ABORT_RATE = 0.25  # 单档错误率超过此值即中止后续档位
ERR_ABORT_MIN = 8  # 且至少这么多条样本（避免 1/2 就中止）
COOLDOWN_WAIT_MAX = 360  # 等冷却解除的上限（秒）


def post(path, payload, timeout=300):
    """返回 (status, body_text, elapsed)。HTTP 错误也返回 body。"""
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        BASE + path,
        data=data,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    t0 = time.time()
    try:
        with client.open(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace"), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), time.time() - t0
    except Exception as e:  # 连接被上游直接关掉等
        return 0, f"<{type(e).__name__}: {e}>", time.time() - t0


def admin_login(user, password):
    if not user:
        return False
    # 生产验证过的是 JSON 形态（200 + Set-Cookie: c2a_session）；失败再退回表单。
    for payload, ctype in (
        (json.dumps({"username": user, "password": password}).encode(), "application/json"),
        (urllib.parse.urlencode({"username": user, "password": password}).encode(), "application/x-www-form-urlencoded"),
    ):
        req = urllib.request.Request(
            BASE + "/api/admin/login", data=payload, headers={"Content-Type": ctype}
        )
        try:
            with admin.open(req, timeout=30) as r:
                if r.status == 200:
                    return True
        except urllib.error.HTTPError:
            continue
        except Exception as e:
            print(f"  [warn] 管理台登录异常：{e}", flush=True)
            return False
    print("  [warn] 管理台登录被拒（账号密码不对？）", flush=True)
    return False


def dashboard():
    """读管理台总览；拿不到时返回 None（脚本仍可跑，只是少了归因）。"""
    try:
        with admin.open(BASE + "/api/admin/dashboard", timeout=30) as r:
            return json.loads(r.read())
    except Exception:
        return None


def cooling_names(dash):
    if not dash:
        return []
    return [c.get("name", "?") for c in (dash.get("cooldowns") or [])]


def fail_counts(dash):
    if not dash:
        return {}
    return dict((dash.get("stats") or {}).get("by_fail_class") or {})


def wait_for_cooldown_clear(limit=COOLDOWN_WAIT_MAX):
    deadline = time.time() + limit
    while time.time() < deadline:
        names = cooling_names(dashboard())
        if not names:
            return True
        rest = int(deadline - time.time())
        print(f"  [wait] 账号仍在冷却 {names}，{rest}s 后重试…", flush=True)
        time.sleep(15)
    return False


def one_request(prompt, model, timeout):
    """发一次非流式 chat 请求。返回 (ok, status, elapsed, err_kind)。"""
    st, body, el = post(
        "/v1/chat/completions",
        {"model": model, "messages": [{"role": "user", "content": prompt}]},
        timeout=timeout,
    )
    if st == 200:
        return True, st, el, ""
    kind = ""
    try:
        j = json.loads(body)
        kind = str(((j.get("error") or {}).get("type")) or "")[:40]
    except Exception:
        kind = body.strip()[:40]
    return False, st, el, kind


def run_step(conc, duration, prompt, model, timeout):
    """固定并发跑 duration 秒，返回统计。"""
    stop = time.time() + duration
    lock = threading.Lock()
    rows = []  # (ok, status, elapsed, kind)

    def worker():
        while time.time() < stop:
            ok, st, el, kind = one_request(prompt, model, timeout)
            with lock:
                rows.append((ok, st, el, kind))

    threads = [threading.Thread(target=worker, daemon=True) for _ in range(conc)]
    t0 = time.time()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    wall = time.time() - t0

    with lock:
        done = list(rows)
    ok = [r for r in done if r[0]]
    lat = sorted(r[2] for r in ok)

    def pct(q):
        if not lat:
            return 0.0
        i = min(len(lat) - 1, max(0, int(round(q * (len(lat) - 1)))))
        return lat[i]

    p50, p95 = pct(0.5), pct(0.95)
    kinds = {}
    for r in done:
        if not r[0]:
            k = f"{r[1]}:{r[3]}" if r[3] else str(r[1])
            kinds[k] = kinds.get(k, 0) + 1
    return {
        "conc": conc,
        "wall": wall,
        "total": len(done),
        "ok": len(ok),
        "rpm_ok": len(ok) / wall * 60 if wall > 0 else 0,
        "rpm_all": len(done) / wall * 60 if wall > 0 else 0,
        "err_rate": (len(done) - len(ok)) / len(done) if done else 1.0,
        "p50": p50,
        "p95": p95,
        "kinds": kinds,
    }


def fmt_step(r):
    return (
        f"  并发 {r['conc']:>2} │ 成功 {r['ok']:>3}/{r['total']:<3} │ "
        f"**{r['rpm_ok']:6.1f} RPM**（含失败 {r['rpm_all']:6.1f}） │ "
        f"成功率 {100 * (1 - r['err_rate']):5.1f}% │ p50 {r['p50']:4.1f}s p95 {r['p95']:4.1f}s"
        + (f" │ {r['kinds']}" if r["kinds"] else "")
    )


def main(argv=None):
    ap = argparse.ArgumentParser(description="prism-2api 容量探针（见 docs/TESTPLAN.md §6）")
    ap.add_argument("--steps", default="1,2,4,8,16", help="并发阶梯，逗号分隔（默认 1,2,4,8,16；本地不限流，上限只由上游决定）")
    ap.add_argument("--duration", type=int, default=60, help="每档持续秒数（默认 60）")
    ap.add_argument("--repeat", type=int, default=1, help="每档重复轮数（默认 1，取最好一轮）")
    ap.add_argument("--model", default="gpt-6-astra")
    ap.add_argument("--prompt", default="short", choices=("short", "long"))
    ap.add_argument("--timeout", type=int, default=300, help="单请求超时秒数")
    ap.add_argument("--admin-user", default=os.environ.get("PRISM_ADMIN_USER", ""))
    ap.add_argument("--admin-pass", default=os.environ.get("PRISM_ADMIN_PASS", ""))
    ap.add_argument("--plan", action="store_true", help="只打印阶梯与请求预算，不发任何请求")
    ap.add_argument("--yes", action="store_true", help="跳过确认（CI/无人值守用）")
    args = ap.parse_args(argv)

    steps = [int(s) for s in args.steps.split(",") if s.strip()]
    budget = sum(c * args.duration for c in steps) * args.repeat
    prompt = PROMPT_SHORT if args.prompt == "short" else PROMPT_LONG

    print(f"目标 {BASE}")
    print(f"阶梯 {steps}，每档 {args.duration}s ×{args.repeat}，模型 {args.model}，提示词 {args.prompt}")
    print(f"请求预算上限 ≈ {budget} 次（并发 × 时长；实际按单轮耗时封顶）")
    print("副作用：每次请求都会在上游 project 里**新建一个会话**（无清理路径），见 TESTPLAN §6.1")
    if args.plan:
        est = 1 + 8 / 1.5  # 1 次 start + 轮询，按单轮 8s 估
        print(f"\n[plan] 上游请求放大 ≈ {est:.1f}×（1 次 start + 单轮耗时/1.5s 次 status 轮询）")
        print(f"[plan] 预计上游请求 ≈ {int(budget * est)} 次；RPM 上限初估 = 并发 / 单轮耗时 × 60")
        for c in steps:
            for lat in (6, 9, 15):
                print(f"        并发 {c:>2} × 单轮 {lat:>2}s → 理论 {c / lat * 60:5.1f} RPM")
        return 0

    if not KEY:
        print("缺 WEB2API_API_KEY（环境变量）", file=sys.stderr)
        return 2
    if not args.yes:
        print("\n这会消耗生产账号额度并写数据。确认继续？[y/N] ", end="", flush=True)
        if (sys.stdin.readline() or "").strip().lower() not in ("y", "yes"):
            print("已取消。")
            return 0

    have_admin = admin_login(args.admin_user, args.admin_pass)
    if not have_admin:
        print("  [warn] 未登录管理台：跑完就没有失败分类/冷却归因，仍继续。", flush=True)
    else:
        print("  [ok] 管理台已登录，可做冷却与失败归因。", flush=True)
        if cooling_names(dashboard()):
            print("  账号当前已在冷却中，先等它解除…", flush=True)
            if not wait_for_cooldown_clear():
                print("  [abort] 冷却未在窗口内解除，先别压。", file=sys.stderr)
                return 1

    results = []
    aborted = None
    for conc in steps:
        if not wait_for_cooldown_clear():
            aborted = f"并发 {conc} 档前账号仍在冷却"
            break
        before_dash = dashboard()
        before = fail_counts(before_dash)

        best = None
        for i in range(args.repeat):
            r = run_step(conc, args.duration, prompt, args.model, args.timeout)
            if best is None or r["rpm_ok"] > best["rpm_ok"]:
                best = r
            if args.repeat > 1:
                print(f"  (第 {i + 1} 轮) " + fmt_step(r), flush=True)
        results.append(best)
        print(fmt_step(best), flush=True)

        after_dash = dashboard()
        after = fail_counts(after_dash)
        delta = {
            k: after.get(k, 0) - before.get(k, 0)
            for k in set(after) | set(before)
            if after.get(k, 0) - before.get(k, 0)
        }
        if delta:
            print(f"         失败分类增量 {delta}", flush=True)
        if after_dash:
            names = cooling_names(after_dash)
            if names:
                aborted = f"并发 {conc} 触发账号冷却（{names}）——这就是天花板，别再往上加"
                print(f"  [stop] {aborted}", flush=True)
                break
        if best["err_rate"] > ERR_ABORT_RATE and best["total"] >= ERR_ABORT_MIN:
            aborted = f"并发 {conc} 错误率 {best['err_rate'] * 100:.0f}% 超阈值，停止加梯"
            print(f"  [stop] {aborted}", flush=True)
            break

    print("\n──── 汇总 ────")
    for r in results:
        print(fmt_step(r))
    if results:
        top = max(results, key=lambda r: r["rpm_ok"])
        print(f"\n可用吞吐（本号、本提示词）：**{top['rpm_ok']:.0f} RPM** @ 并发 {top['conc']}")
        print("注意：这是**稳定速率**上限；突发（短时间成批）另跑 burst 对照，见 TESTPLAN §6.3。")
    if aborted:
        print(f"中止原因：{aborted}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
