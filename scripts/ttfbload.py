#!/usr/bin/env python3
"""TTFB 并发压测（对应 docs/TTFB-PLAN.md §6）。

M 并发突发流式请求，测：首帧/首内容帧 P50/P95、429+Retry-After 是否干净、
完成率。日志侧对照 sandbox_hit 率与预热后台速率。

用法：
    WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=xxx \
      python3 scripts/ttfbload.py --concurrency 50 --total 100 --model gpt-5.6-sol

退出码：0 = 完成率 100% 且无 5xx；1 = 有失败/5xx。
"""
from __future__ import annotations

import argparse
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ.get("WEB2API_URL", "http://127.0.0.1:8080").rstrip("/")
KEY = os.environ.get("WEB2API_API_KEY", "")


def one(idx, model, prompt, timeout):
    """单次流式请求。返回 (首帧, 首内容, 完成, status, retry_after, note)。"""
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": 32,
        "stream": bool(model != "__nostream__"),
    }
    data = json.dumps(payload).encode()
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=data,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    # 每线程独立 opener（绕代理，与 smoke.py 同策略）
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    t0 = time.monotonic()
    ff = fc = dn = None
    status = retry_after = note = None
    try:
        with opener.open(req, timeout=timeout) as r:
            status = r.status
            buf = ""
            first_line_done = False
            while True:
                # 首帧计时粒度：role 帧未读出前逐字节读（read(4096) 会把它憋到
                # 缓冲满才返回，虚高首帧）；读出后切回大块读。
                chunk = r.read(1 if not first_line_done else 4096)
                if not chunk:
                    break
                now = time.monotonic() - t0
                buf += chunk.decode("utf-8", "replace")
                while "\n" in buf:
                    line, buf = buf.split("\n", 1)
                    line = line.strip()
                    if not line.startswith("data:"):
                        continue
                    d = line[5:].strip()
                    if ff is None:
                        ff = now
                        first_line_done = True
                    if d == "[DONE]":
                        dn = now
                    try:
                        j = json.loads(d)
                        delta = (j.get("choices") or [{}])[0].get("delta") or {}
                    except Exception:
                        continue
                    if fc is None and (delta.get("content") or delta.get("reasoning_content")):
                        fc = now
                if dn is not None:
                    break
    except urllib.error.HTTPError as e:
        status = e.code
        try:
            retry_after = e.headers.get("Retry-After")
        except Exception:
            pass
        try:
            body = e.read().decode("utf-8", "replace")[:160]
        except Exception:
            body = ""
        note = f"HTTP {e.code} retry_after={retry_after} {body}"
    except Exception as e:
        note = f"{type(e).__name__}: {e}"
    return ff, fc, dn, status, retry_after, note


def pct(vals, p):
    s = sorted(vals)
    if not s:
        return None
    k = (len(s) - 1) * p / 100
    f, c = int(k), min(int(k) + 1, len(s) - 1)
    return s[f] + (s[c] - s[f]) * (k - f)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="gpt-5.6-sol")
    ap.add_argument("--concurrency", type=int, default=20)
    ap.add_argument("--total", type=int, default=60)
    ap.add_argument("--prompt", default="只回复：可用")
    ap.add_argument("--timeout", type=int, default=300)
    a = ap.parse_args()

    print(f"BASE={BASE} model={a.model} concurrency={a.concurrency} total={a.total}", flush=True)
    t0 = time.monotonic()
    with ThreadPoolExecutor(max_workers=a.concurrency) as ex:
        rows = list(ex.map(lambda i: one(i, a.model, a.prompt, a.timeout), range(a.total)))
    wall = time.monotonic() - t0

    ok = sum(1 for r in rows if r[2] is not None)
    statuses: dict = {}
    no_retry_429 = 0
    for _, _, dn, st, ra, note in rows:
        statuses[st] = statuses.get(st, 0) + 1
        if st == 429 and not ra:
            no_retry_429 += 1
        if dn is None:
            print(f"  FAIL status={st} {note or ''}", flush=True)

    for name, idx in (("首帧", 0), ("首内容", 1), ("完成", 2)):
        vals = [r[idx] for r in rows if r[idx] is not None]
        if vals:
            print(f"{name} n={len(vals)} p50={pct(vals,50):.3f}s p95={pct(vals,95):.3f}s max={max(vals):.3f}s")
        else:
            print(f"{name} 无数据")

    print(f"完成率 {ok}/{a.total} wall={wall:.1f}s 状态分布={statuses} 429无Retry-After={no_retry_429}")
    bad = a.total - ok
    for _, _, dn, st, ra, note in rows:
        if st is not None and st >= 500:
            bad += 1
            break
    return 1 if (bad > 0 or no_retry_429 > 0) else 0


if __name__ == "__main__":
    sys.exit(main())
