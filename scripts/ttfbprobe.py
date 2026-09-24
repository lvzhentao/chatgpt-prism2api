#!/usr/bin/env python3
"""TTFB Phase 0 基线探针（对应 docs/TTFB-PLAN.md §0/§6/§8）。

逐帧打时间戳，分离「首帧时间」（连接层：time_starttransfer 类）与
「首内容帧时间」（体验层：首个 delta.content 非空）。

用法（密码放环境变量，不要写进命令行）：
    export SSHPASS='...'
    WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=xxx \
      python3 scripts/ttfbprobe.py --model gpt-6-astra --repeat 5 --prompt "只回复：可用"

退出码 0 = 全部完成（含无内容帧但 [DONE] 的情况会标记 EMPTY）。
"""
from __future__ import annotations

import argparse
import json
import os
import statistics
import subprocess
import sys
import time
import urllib.request

BASE = os.environ.get("WEB2API_URL", "http://127.0.0.1:8080").rstrip("/")
KEY = os.environ.get("WEB2API_API_KEY", "")
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def one(model, prompt, timeout):
    """单次流式请求。返回 dict(first_frame, first_content, done, status, note)。"""
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": 64,
        "stream": True,
    }
    req = urllib.request.Request(
        BASE + "/v1/chat/completions",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {KEY}"},
    )
    t0 = time.monotonic()
    first_frame = first_content = done_at = None
    status = note = None
    try:
        with _OPENER.open(req, timeout=timeout) as r:
            status = r.status
            buf = ""
            while True:
                chunk = r.read(4096)
                if not chunk:
                    break
                now = time.monotonic() - t0
                buf += chunk.decode("utf-8", "replace")
                while "\n" in buf:
                    line, buf = buf.split("\n", 1)
                    line = line.strip()
                    if not line.startswith("data:"):
                        continue
                    data = line[5:].strip()
                    if first_frame is None:
                        first_frame = now
                    if data == "[DONE]":
                        done_at = now
                        break
                    try:
                        j = json.loads(data)
                    except Exception:
                        continue
                    try:
                        delta = j["choices"][0].get("delta") or {}
                    except Exception:
                        continue
                    if first_content is None and (delta.get("content") or delta.get("reasoning_content")):
                        first_content = now
                if done_at is not None:
                    break
    except Exception as e:
        note = f"{type(e).__name__}: {e}"
    return {
        "first_frame": first_frame,
        "first_content": first_content,
        "done": done_at,
        "status": status,
        "note": note,
    }


def fetch_logs(since_min=20):
    """经同一条 SSH 长连接取网关日志 ttFB 相关行。只读，不写。"""
    host = os.environ.get("DEPLOY_HOST", "YOUR_SERVER_IP")
    port = os.environ.get("DEPLOY_PORT", "4344")
    user = os.environ.get("DEPLOY_USER", "root")
    sshpass = os.environ.get("SSHPASS", "")
    if not sshpass:
        return "<SSHPASS 未设置，跳过日志>"
    cmd = [
        "sshpass", "-p", sshpass, "ssh", "-p", port,
        "-o", "StrictHostKeyChecking=no", "-o", "ControlMaster=no",
        f"{user}@{host}",
        f"docker logs --since {since_min}m prism-2api 2>&1 | "
        "grep -E 'prism: (start|turn|t_pick|sched_pick|status poll)' | tail -120",
    ]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=60)
        lines = (out.stdout or "").strip().splitlines()
        # 密码绝不打印：只确认返回了行数
        return f"{len(lines)} 行\n" + "\n".join(lines[-60:])
    except Exception as e:
        return f"<取日志失败: {e}>"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", default="gpt-6-astra")
    ap.add_argument("--repeat", type=int, default=5)
    ap.add_argument("--prompt", default="只回复：可用")
    ap.add_argument("--timeout", type=int, default=300)
    ap.add_argument("--sleep", type=float, default=2.0, help="两次请求间隔秒数")
    ap.add_argument("--skip-logs", action="store_true")
    a = ap.parse_args()

    print(f"BASE={BASE} model={a.model} repeat={a.repeat}", flush=True)
    rows = []
    for i in range(a.repeat):
        r = one(a.model, a.prompt, a.timeout)
        rows.append(r)
        ff = f"{r['first_frame']:.3f}s" if r["first_frame"] is not None else "-"
        fc = f"{r['first_content']:.3f}s" if r["first_content"] is not None else ("EMPTY" if r["done"] else "-")
        dn = f"{r['done']:.3f}s" if r["done"] is not None else "-"
        print(f"  [{i+1}/{a.repeat}] status={r['status']} 首帧={ff} 首内容={fc} 完成={dn} {r['note'] or ''}", flush=True)
        if i + 1 < a.repeat:
            time.sleep(a.sleep)

    def pct(vals, p):
        if not vals:
            return None
        s = sorted(vals)
        k = (len(s) - 1) * p / 100
        f, c = int(k), min(int(k) + 1, len(s) - 1)
        return s[f] + (s[c] - s[f]) * (k - f)

    for name in ("first_frame", "first_content", "done"):
        vals = [r[name] for r in rows if r[name] is not None]
        if vals:
            print(f"{name:13} n={len(vals)} p50={pct(vals,50):.3f}s p95={pct(vals,95):.3f}s min={min(vals):.3f}s max={max(vals):.3f}s")
        else:
            print(f"{name:13} 无数据")

    if not a.skip_logs:
        print("--- 网关日志（ttfb 相关）---")
        print(fetch_logs())


if __name__ == "__main__":
    sys.exit(main())
