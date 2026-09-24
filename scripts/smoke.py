#!/usr/bin/env python3
"""prism-2api 冒烟测试（对应 docs/TESTPLAN.md）。

零依赖，只用标准库。默认打本地网关，用环境变量切到生产：

    WEB2API_URL=http://YOUR_SERVER_IP:8301 WEB2API_API_KEY=xxx python3 scripts/smoke.py
    python3 scripts/smoke.py --only L0,L1 --models gpt-6-astra --repeat 5
    python3 scripts/smoke.py --only LM            # 只跑多模态（图片 / 文件附件）

退出码：0 = 无阻塞失败；1 = 有阻塞失败（判定见 TESTPLAN.md §2）。

注意：L2 的档位夹取、L3 的触发率结论必须配合容器日志才成立——
脚本只能证明"没 400"，证明"参数真到了上游"请看：

    docker logs prism-2api | grep 'prism: start\\|prism: turn'
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ.get("WEB2API_URL", "http://127.0.0.1:8080").rstrip("/")
KEY = os.environ.get("WEB2API_API_KEY", "")
STREAM_MODEL = os.environ.get("WEB2API_STREAM_MODEL", "gpt-5.6-sol")
DEFAULT_MODELS = ["gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"]

WEATHER_TOOL = {
    "type": "function",
    "function": {
        "name": "get_weather",
        "description": "查询指定城市的实时天气",
        "parameters": {
            "type": "object",
            "properties": {"city": {"type": "string", "description": "城市名"}},
            "required": ["city"],
        },
    },
}
READ_TOOL = {
    "type": "function",
    "function": {
        "name": "ReadLocal_README_MARKER_7X9",
        "description": "读取客户本地工作目录中的文件，返回文件内容",
        "parameters": {
            "type": "object",
            "properties": {"path": {"type": "string", "description": "文件路径"}},
            "required": ["path"],
        },
    },
}

READ_STRONG_PROMPT = ("调用 ReadLocal_README_MARKER_7X9 工具读取 README.md 的前两行，"
                      "必须以工具调用形式回复，不要直接回答")
STRONG_TOOL_PROMPT = "调用 get_weather 工具查询北京天气，必须以工具调用形式回复"
WEAK_TOOL_PROMPT = "北京天气怎么样？"

# 判定为阻塞的用例（TESTPLAN.md §2）。
# L1-9 是 P0-1 的回归用例：客户端 system 指令在多轮场景被丢弃，属正确性缺陷，当前会红——
# 这是预期信号，修掉 docs/ENHANCE-PLAN.md P0-1 后转绿。
BLOCKING = {"L0-1", "L0-3", "L0-4", "L1-1", "L1-5", "L1-9", "L2-3", "L2-4", "L2-5", "LM-1", "LM-3", "LM-5", "L4-1", "L3-6", "L3-7", "L3-8", "L3-9"}

RESULTS: list[tuple[str, bool, str]] = []

# 直连网关：本机环境常带 HTTP_PROXY（如 127.0.0.1:7897），并发时它会吐 502，
# 会被误判成服务端故障。冒烟测试打的是 IP+端口，一律绕过代理。
_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


def record(cid, ok, detail):
    RESULTS.append((cid, ok, detail))
    mark = "PASS" if ok else "FAIL"
    print(f"  [{mark}] {cid:6} {detail}", flush=True)


def call(path, payload=None, headers=None, timeout=300):
    """返回 (http_status, body_text, elapsed_seconds)。HTTP 错误也返回 body。"""
    hdr = {"Content-Type": "application/json"}
    hdr.update(headers or {})
    if hdr.get("Authorization") is None:
        hdr["Authorization"] = f"Bearer {KEY}"
    data = json.dumps(payload).encode() if payload is not None else None
    req = urllib.request.Request(BASE + path, data=data, headers=hdr)
    t0 = time.time()
    try:
        with _OPENER.open(req, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace"), time.time() - t0
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace"), time.time() - t0
    except Exception as e:  # 连接被拒 / 超时
        return 0, f"<{type(e).__name__}: {e}>", time.time() - t0


def jbody(text):
    try:
        return json.loads(text)
    except Exception:
        return None


def chat(model, content, **kw):
    payload = {"model": model, "messages": [{"role": "user", "content": content}], "max_tokens": 256}
    payload.update(kw)
    return call("/v1/chat/completions", payload)


def content_of(text):
    j = jbody(text)
    try:
        return j["choices"][0]["message"].get("content") or ""
    except Exception:
        return ""


# ---------------------------------------------------------------- L0

def layer_l0():
    print("L0 存活与鉴权")
    st, _, t = call("/healthz", headers={"Authorization": ""})
    record("L0-1", st == 200, f"GET /healthz → {st} ({t:.1f}s)")

    st, body, t = call("/v1/models")
    j = jbody(body) or {}
    ids = [m.get("id") for m in j.get("data", [])]
    record("L0-3", st == 200 and {"gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra"} <= set(ids),
           f"GET /v1/models → {st} {ids}")

    st, body, t = call("/v1/models", headers={"Authorization": ""})
    record("L0-4", st == 401, f"无 key GET /v1/models → {st} ({t:.1f}s)")

    st, _, t = call("/v1/chat/completions",
                    {"model": STREAM_MODEL, "messages": [{"role": "user", "content": "hi"}]},
                    headers={"Authorization": ""})
    record("L0-5", st == 401, f"无 key POST chat → {st} ({t:.1f}s)")


# ---------------------------------------------------------------- L1

def layer_l1(models):
    print("L1 核心对话链路")
    m = STREAM_MODEL if STREAM_MODEL in models else models[0]

    st, body, t = chat(m, "只回复四个字：生产可用", max_tokens=64)
    txt = content_of(body).strip()
    record("L1-1", st == 200 and "生产可用" in txt, f"非流式 → {st} ({t:.1f}s) {txt[:24]!r}")

    st, body, t = chat(m, "1+1 等于几？只回复数字", max_tokens=64)
    txt = content_of(body).strip()
    record("L1-2", st == 200 and "2" in txt, f"第二轮 → {st} ({t:.1f}s) {txt[:24]!r}")

    st, body, t = chat(m, "数到三，用空格分隔", stream=True, max_tokens=64)
    frames = [l[6:] for l in body.splitlines() if l.startswith("data: ")]
    done = any(f.strip() == "[DONE]" for f in frames)
    agg, finish = "", None
    for f in frames:
        j = jbody(f)
        if not j:
            continue
        for ch in j.get("choices", []):
            agg += ch.get("delta", {}).get("content") or ""
            finish = ch.get("finish_reason") or finish
    nums_ok = all(c in agg for c in "123") or all(c in agg for c in "一二三")
    record("L1-3", st == 200 and done and finish == "stop" and nums_ok,
           f"流式 → {st} ({t:.1f}s) frames={len(frames)} finish={finish} text={agg.strip()[:20]!r}")
    msgs = [{"role": "system", "content": "用户偏好的数字是 4173。"},
            {"role": "user", "content": "记住这个数字。"},
            {"role": "assistant", "content": "好的。"},
            {"role": "user", "content": "那个数字是多少？只回复数字。"}]
    st, body, t = call("/v1/chat/completions", {"model": m, "messages": msgs, "max_tokens": 64})
    txt = content_of(body).strip()
    record("L1-4", st == 200 and "4173" in txt, f"历史折叠 → {st} ({t:.1f}s) {txt[:24]!r}")

    st, body, t = call("/v1/messages", {"model": m, "max_tokens": 64,
                                        "messages": [{"role": "user", "content": "只回复：anthropic 通"}]},
                       headers={"anthropic-version": "2023-06-01"})
    j = jbody(body) or {}
    blocks = [b.get("type") for b in j.get("content", [])]
    record("L1-5", st == 200 and "text" in blocks and j.get("stop_reason") == "end_turn",
           f"/v1/messages → {st} ({t:.1f}s) blocks={blocks} stop={j.get('stop_reason')}")

    st, body, t = call("/v1/messages", {"model": m, "max_tokens": 256,
                                        "thinking": {"type": "enabled", "budget_tokens": 2048},
                                        "messages": [{"role": "user", "content": "只回复：thinking 通"}]},
                       headers={"anthropic-version": "2023-06-01"})
    j = jbody(body) or {}
    blocks = [b.get("type") for b in j.get("content", [])]
    record("L1-6", st == 200 and "text" in blocks, f"+thinking → {st} ({t:.1f}s) blocks={blocks}")

    st, body, t = call("/v1/responses", {"model": m, "input": "只回复：responses 通道通",
                                         "max_output_tokens": 64})
    j = jbody(body) or {}
    record("L1-7", st == 200 and j.get("status") == "completed" and (j.get("output_text") or "").strip(),
           f"/v1/responses → {st} ({t:.1f}s) status={j.get('status')} text={(j.get('output_text') or '')[:16]!r}")

    st, body, t = chat(m, "你是什么模型？由谁提供？", max_tokens=128)
    txt = content_of(body)
    leaked = any(k in txt for k in ("Codex", "GPT-5", "OpenAI", "prism"))
    record("L1-8", st == 200 and not leaked, f"身份（含『由谁提供』）→ {st} ({t:.1f}s) {txt.strip()[:48]!r}")

    # L1-9：客户端 system 指令在「有多轮历史」时是否还生效。
    # 这是 P0-1 的回归用例（见 docs/ENHANCE-PLAN.md）：当前实现把每个来源发成独立 system item，
    # 最后一条 [对话历史] 会盖掉前面的客户端 system → 约束整体失效。修复后应恒为法语。
    fr = [{"role": "system", "content": "回答必须全部使用法语（Français），不得使用中文。"},
          {"role": "user", "content": "你好"},
          {"role": "assistant", "content": "Bonjour !"},
          {"role": "user", "content": "今天天气怎么样？"}]
    st, body, t = call("/v1/chat/completions", {"model": m, "messages": fr, "max_tokens": 64})
    txt = content_of(body).strip()
    cjk = re.search(r"[\u4e00-\u9fff]", txt) is not None
    record("L1-9", st == 200 and not cjk,
           f"system 指令 + 多轮历史 → {st} ({t:.1f}s) {'中文（约束被丢弃）' if cjk else '法语（约束生效）'} {txt[:28]!r}")


# ---------------------------------------------------------------- L2

def layer_l2(models):
    print("L2 参数面")
    m = STREAM_MODEL if STREAM_MODEL in models else models[0]

    ok = []
    for eff in ("low", "medium", "high"):
        st, _, t = chat(m, "只回复 OK", reasoning_effort=eff, max_tokens=32)
        ok.append((eff, st, t))
    record("L2-1", all(s == 200 for _, s, _ in ok),
           "reasoning_effort low/medium/high → " + ", ".join(f"{e}={s}" for e, s, _ in ok))

    ok = []
    for eff in ("xhigh", "max"):
        st, _, t = chat(m, "只回复 OK", reasoning_effort=eff, max_tokens=32)
        ok.append((eff, st, t))
    # 通过标准只是"不报错"；实际收值必须看容器日志（TESTPLAN.md §3 L2-2 记录当前被夹到 high）
    record("L2-2", all(s == 200 for _, s, _ in ok),
           "reasoning_effort xhigh/max → " + ", ".join(f"{e}={s}" for e, s, _ in ok)
           + "  ⚠ 收值需查日志（当前夹到 high）")

    ok = []
    for sfx in ("-low", "-medium", "-high", "-xhigh", "-max"):
        # 后缀没剥干净 → 上游 400；这是本用例的判别信号
        st, body, t = chat(m + sfx, "只回复 OK", max_tokens=32)
        ok.append((sfx, st))
    record("L2-3", all(s == 200 for _, s in ok),
           "模型名后缀 → " + ", ".join(f"{k}={v}" for k, v in ok))

    st, _, t = chat(m + "-low", "只回复 OK", reasoning_effort="high", max_tokens=32)
    record("L2-4", st == 200, f"显式 effort 覆盖后缀 → {st} ({t:.1f}s)")

    st, body, t = chat("gpt-5.4", "hi", max_tokens=16)
    record("L2-5", st == 400 and t < 10, f"未知模型 gpt-5.4 → {st} ({t:.1f}s)")

    st, _, t = chat(m, "只回复 OK", max_tokens=32)
    record("L2-5b", st == 200, f"未知模型之后（未冷却）→ {st} ({t:.1f}s)")

    st, _, t = chat(m + "-xhigh", "只回复 OK", max_tokens=32)
    record("L2-6", st == 200, f"-xhigh 名字剥离 → {st} ({t:.1f}s)")


# ---------------------------------------------------------------- L3

def layer_l3(models, repeat):
    print("L3 工具调用（仿真协议）")

    st, body, t = call("/v1/chat/completions",
                       {"model": STREAM_MODEL, "messages": [{"role": "user", "content": STRONG_TOOL_PROMPT}],
                        "tools": [WEATHER_TOOL], "max_tokens": 256})
    j = jbody(body) or {}
    msg = (j.get("choices") or [{}])[0].get("message", {})
    tc = (msg.get("tool_calls") or [None])[0]
    ok = st == 200 and tc and tc["function"]["name"] == "get_weather"
    try:
        args_ok = json.loads(tc["function"]["arguments"]).get("city") == "北京" if tc else False
    except Exception:
        args_ok = False
    record("L3-1", bool(ok and args_ok),
           f"强提示 → {st} ({t:.1f}s) finish={(j.get('choices') or [{}])[0].get('finish_reason')} "
           f"tool_calls={json.dumps(tc, ensure_ascii=False) if tc else None}")

    st, body, t = call("/v1/chat/completions",
                       {"model": STREAM_MODEL, "stream": True,
                        "messages": [{"role": "user", "content": STRONG_TOOL_PROMPT}],
                        "tools": [WEATHER_TOOL], "max_tokens": 256})
    acc, finish = {}, None
    for f in (l[6:] for l in body.splitlines() if l.startswith("data: ")):
        j = jbody(f)
        if not j:
            continue
        for ch in j.get("choices", []):
            for d in ch.get("delta", {}).get("tool_calls") or []:
                e = acc.setdefault(d.get("index", 0), {"id": "", "name": "", "arguments": ""})
                if d.get("id"):
                    e["id"] = d["id"]
                fn = d.get("function") or {}
                if fn.get("name"):
                    e["name"] = fn["name"]
                e["arguments"] += fn.get("arguments") or ""
            finish = ch.get("finish_reason") or finish
    tc_stream = next(iter(acc.values()), None)
    try:
        args_ok = bool(tc_stream) and json.loads(tc_stream["arguments"]).get("city") == "北京"
    except Exception:
        args_ok = False
    record("L3-2", st == 200 and bool(tc_stream) and tc_stream["name"] == "get_weather"
           and args_ok and finish == "tool_calls",
           f"流式 → {st} ({t:.1f}s) finish={finish} "
           f"delta={json.dumps(tc_stream, ensure_ascii=False) if tc_stream else None}")

    if tc:
        msgs = [{"role": "user", "content": STRONG_TOOL_PROMPT},
                {"role": "assistant", "content": msg.get("content") or "", "tool_calls": msg["tool_calls"]},
                {"role": "tool", "tool_call_id": tc["id"], "content": "北京 25℃ 晴"}]
        st, body, t = call("/v1/chat/completions",
                           {"model": STREAM_MODEL, "messages": msgs, "tools": [WEATHER_TOOL], "max_tokens": 256})
        record("L3-3", st == 200, f"回灌 tool 结果 → {st} ({t:.1f}s)")
    else:
        record("L3-3", False, "跳过：L3-1 未拿到 tool_call（回灌无 id 可用）")

    st, body, t = call("/v1/chat/completions",
                       {"model": STREAM_MODEL, "messages": [{"role": "user", "content": WEAK_TOOL_PROMPT}],
                        "tools": [WEATHER_TOOL], "max_tokens": 256})
    got = bool(((jbody(body) or {}).get("choices") or [{}])[0].get("message", {}).get("tool_calls"))
    record("L3-4", True,
           f"弱提示「{WEAK_TOOL_PROMPT}」→ {'工具调用' if got else '退化成文本'}（允许退化，仅记录）")

    print(f"  触发率（同一强提示各 {repeat} 次）：")
    for model in models:
        n = 0
        for _ in range(repeat):
            st, body, t = call("/v1/chat/completions",
                               {"model": model, "messages": [{"role": "user", "content": STRONG_TOOL_PROMPT}],
                                "tools": [WEATHER_TOOL], "max_tokens": 256})
            if ((jbody(body) or {}).get("choices") or [{}])[0].get("message", {}).get("tool_calls"):
                n += 1
        record("L3-5", n > 0, f"  {model:16} {n}/{repeat}")
# ---------------------------------------------------------------- LM

    # L3-6：本地文件读取归属——仿真工具名必须原样 echo 回来。
    # 若返回的是上游沙箱内置工具名（shell/read/ls…），说明模型在读它自己的
    # 空工作区而非客户端工具；若直接文本作答不调工具，说明仿真协议对该
    # 工具名未触发（需修 toolContract 措辞）。
    st, body, t = call("/v1/chat/completions",
                       {"model": STREAM_MODEL, "messages": [{"role": "user", "content": READ_STRONG_PROMPT}],
                        "tools": [READ_TOOL], "max_tokens": 256})
    j = jbody(body) or {}
    msg = (j.get("choices") or [{}])[0].get("message", {})
    tc = (msg.get("tool_calls") or [None])[0]
    got_name = (tc or {}).get("function", {}).get("name", "") if tc else ""
    txt = msg.get("content") or ""
    if st == 200 and got_name == "ReadLocal_README_MARKER_7X9":
        record("L3-6", True,
               f"归属正确 → 客户端工具被调用 {t:.1f}s args={(tc.get('function') or {}).get('arguments', '')[:60]}")
    elif st == 200 and got_name:
        record("L3-6", False,
               f"归属错误 → 调的是上游沙箱工具 {got_name!r}（读的是它自己的空工作区）{t:.1f}s")
    else:
        leak = [k for k in ("工作区", "AGENTS", "不存在", "无法读取") if k in txt]
        record("L3-6", False,
               f"未触发工具调用 → {st} ({t:.1f}s) 文本={txt[:60]!r} 泄漏词={leak}")

    # L3-7/8/9：bash/read/write 三件套归属。仿真协议下模型必须原样输出
    # 客户端声明的工具名（具名示例钉死），不能改成它沙箱里的习惯名。
    for cid, tname, prompt, argkey, argval in (
        ("L3-7", "Bash", "调用 Bash 工具执行命令 ls -la 列出当前目录，必须以工具调用形式回复", "command", "ls -la"),
        ("L3-8", "Read", "调用 Read 工具读取 README.md 的前两行，必须以工具调用形式回复", "path", "README.md"),
        ("L3-9", "Write", "调用 Write 工具把字符串 hello 写入 hello.txt，必须以工具调用形式回复", "file_path", "hello.txt"),
    ):
        tool = {"type": "function", "function": {
            "name": tname,
            "description": f"客户端本地执行的 {tname} 工具",
            "parameters": {"type": "object",
                           "properties": {argkey: {"type": "string"}},
                           "required": [argkey]}}}
        st, body, t = call("/v1/chat/completions",
                           {"model": STREAM_MODEL, "messages": [{"role": "user", "content": prompt}],
                            "tools": [tool], "max_tokens": 256})
        j = jbody(body) or {}
        msg = (j.get("choices") or [{}])[0].get("message", {})
        tc = (msg.get("tool_calls") or [None])[0]
        got = (tc or {}).get("function", {}).get("name", "") if tc else ""
        args = (tc or {}).get("function", {}).get("arguments", "") if tc else ""
        detail = f"{tname} → name={got!r} args={args[:80]!r} ({t:.1f}s)"
        if st == 200 and got == tname and argval in args:
            record(cid, True, detail)
        elif st == 200 and got:
            record(cid, False, f"改名/归属错误 {detail}")
        else:
            record(cid, False, f"未触发工具调用 {detail} 文本={(msg.get('content') or '')[:60]!r}")

    # L3-10：三工具齐声明时的首轮触发率探针（Bash/Read/Write 同发，5 次，记录分布）。
    # 真机基线（2026-09-18，sol）：裸 ls 0/5 必败、`pwd && ls` 5/5 必过——这是模型习惯问题，
    # 不是网关能修的；本用例只记录分布，不阻塞。
    for tname, prompt in (
        ("Bash", "用 Bash 工具执行命令 pwd && ls，必须以工具调用形式回复"),
        ("Read", "调用 Read 工具读取 README.md 的前两行，必须以工具调用形式回复"),
        ("Write", "调用 Write 工具把字符串 hello 写入 hello.txt，必须以工具调用形式回复"),
    ):
        ok = 0
        for _ in range(5):
            st, body, _ = call("/v1/chat/completions",
                               {"model": STREAM_MODEL, "messages": [{"role": "user", "content": prompt}],
                                "tools": [WEATHER_TOOL, READ_TOOL,
                                          {"type": "function", "function": {"name": tname, "description": f"客户端本地执行的 {tname} 工具",
                                           "parameters": {"type": "object", "properties": {"p": {"type": "string"}}, "required": ["p"]}}}],
                                "max_tokens": 256})
            j = jbody(body) or {}
            msg = (j.get("choices") or [{}])[0].get("message", {})
            tc = (msg.get("tool_calls") or [None])[0]
            if tc and (tc.get("function", {}).get("name", "")) == tname:
                ok += 1
        record("L3-10", True, f"三工具齐声明 {tname} 首轮触发 {ok}/5（仅记录分布，不阻塞）")



def bands_png(w, h, colors):
    """生成竖色带 PNG（标准库实现）：颜色顺序 = 内容指纹，模型猜不中。"""
    import struct
    import zlib

    row = b""
    for x in range(w):
        row += bytes(colors[min(len(colors) - 1, x * len(colors) // w)])
    raw = b"".join(b"\x00" + row for _ in range(h))

    def chunk(tag, data):
        body = tag + data
        return struct.pack(">I", len(data)) + body + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF)

    ihdr = struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", ihdr)
            + chunk(b"IDAT", zlib.compress(raw, 9)) + chunk(b"IEND", b""))


def as_data_url(mime, blob):
    return f"data:{mime};base64,{base64.b64encode(blob).decode()}"


def modal_chat(model, text, parts, **kw):
    return chat(model, [{"type": "text", "text": text}] + parts, **kw)


def layer_lm(models):
    """多模态：图片/文档附件必须真的到模型眼前。

    上游没有二进制通道，适配器把图片转成 base64 + "还原到工作区再 view_image"的指令，
    文档类附件贴正文（见 internal/adapter/prism/attachments.go）。
    判别信号用色带顺序/随机口令这种模型猜不中的事实——否则"没看到图也能蒙对"。"""
    print("LM 多模态（图片 / 文档附件）")
    m = STREAM_MODEL if STREAM_MODEL in models else models[0]
    # 左红 / 中绿 / 右蓝
    bands = as_data_url("image/png", bands_png(90, 30, [(220, 20, 20), (20, 200, 20), (20, 20, 220)]))
    want = ("红", "绿", "蓝")

    st, body, t = modal_chat(m, "图片里从左到右的颜色顺序是什么？只回复三个颜色词。",
                             [{"type": "image_url", "image_url": {"url": bands}}], max_tokens=64)
    txt = content_of(body).strip()
    record("LM-1", st == 200 and all(w in txt for w in want),
           f"三色带图 → {st} ({t:.1f}s) {txt[:32]!r}")

    st, body, t = modal_chat(m, "图片里从左到右的颜色顺序是什么？只回复三个颜色词。",
                             [{"type": "image_url", "image_url": {"url": bands}}], stream=True, max_tokens=64)
    agg = ""
    for f in (l[6:] for l in body.splitlines() if l.startswith("data: ")):
        j = jbody(f)
        if not j:
            continue
        for ch in j.get("choices", []):
            agg += ch.get("delta", {}).get("content") or ""
    record("LM-2", st == 200 and all(w in agg for w in want),
           f"stream:true → {st} ({t:.1f}s) {agg.strip()[:32]!r}")

    token = "PRISM-FILE-7391"
    notes = as_data_url("text/plain", f"验证口令：{token}\n".encode())
    st, body, t = modal_chat(m, "附件里的验证口令是什么？只回复口令。",
                             [{"type": "file", "file": {"filename": "notes.txt", "file_data": notes}}], max_tokens=64)
    txt = content_of(body).strip()
    record("LM-3", st == 200 and token in txt, f"文本附件 → {st} ({t:.1f}s) {txt[:40]!r}")

    st, body, t = call("/v1/messages", {
        "model": m, "max_tokens": 64,
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": "图片里从左到右的颜色顺序是什么？只回复三个颜色词。"},
            {"type": "image", "source": {"type": "base64", "media_type": "image/png",
                                         "data": base64.b64encode(bands_png(90, 30, [(220, 20, 20), (20, 200, 20), (20, 20, 220)])).decode()}},
        ]}]}, headers={"anthropic-version": "2023-06-01"})
    j = jbody(body) or {}
    txt = "".join(b.get("text", "") for b in j.get("content", []) if b.get("type") == "text")
    record("LM-4", st == 200 and all(w in txt for w in want),
           f"Anthropic image 块 → {st} ({t:.1f}s) {txt.strip()[:32]!r}")

    print("  三个模型各传同一张色带图：")
    for model in models:
        st, body, t = modal_chat(model, "图片里从左到右的颜色顺序是什么？只回复三个颜色词。",
                                 [{"type": "image_url", "image_url": {"url": bands}}], max_tokens=64)
        txt = content_of(body).strip()
        record("LM-5", st == 200 and all(w in txt for w in want),
               f"  {model:16} → {st} ({t:.1f}s) {txt[:28]!r}")


# ---------------------------------------------------------------- L4

def layer_l4(n):
    print(f"L4 并发（{n} 路）")

    def one(i):
        st, body, t = chat(STREAM_MODEL, f"只回复数字 {i}", max_tokens=32)
        return st, t, content_of(body).strip()[:8]

    t0 = time.time()
    with ThreadPoolExecutor(n) as ex:
        res = list(ex.map(one, range(n)))
    wall = time.time() - t0
    allok = all(s == 200 for s, _, _ in res)
    slowest = max(t for _, t, _ in res)
    detail = f"{n} 并发 → {sum(1 for s, _, _ in res if s == 200)}/{n} 200, wall={wall:.1f}s, 最慢={slowest:.1f}s"
    record(f"L4-{'1' if n == 4 else '2'}", allok and wall < slowest * 2.5, detail)
    st, _, t = chat(STREAM_MODEL, "只回复 OK", max_tokens=32)
    record("L4-3", st == 200, f"并发后紧接 1 个 → {st} ({t:.1f}s)")


# ---------------------------------------------------------------- main

def main():
    ap = argparse.ArgumentParser(description="prism-2api 冒烟测试（见 docs/TESTPLAN.md）")
    ap.add_argument("--only", default="L0,L1,L2,L3,LM,L4", help="要跑的层，逗号分隔（默认全部）")
    ap.add_argument("--models", default=",".join(DEFAULT_MODELS), help="工具触发率要覆盖的模型")
    ap.add_argument("--repeat", type=int, default=5, help="L3 每个模型的重复次数（默认 5）")
    ap.add_argument("--concurrency", type=int, default=8, help="L4 并发路数（默认 8；会另跑一组 4 路对照）")
    args = ap.parse_args()

    if not KEY:
        print("缺少 WEB2API_API_KEY 环境变量（本站鉴权 key，非上游 cookie）", file=sys.stderr)
        return 2

    layers = [s.strip().upper() for s in args.only.split(",") if s.strip()]
    models = [s.strip() for s in args.models.split(",") if s.strip()]

    print(f"target {BASE}\n")

    if "L0" in layers:
        layer_l0()
    if "L1" in layers:
        layer_l1(models)
    if "L2" in layers:
        layer_l2(models)
    if "L3" in layers:
        layer_l3(models, args.repeat)
    if "LM" in layers:
        layer_lm(models)
    if "L4" in layers:
        layer_l4(4)
        if args.concurrency != 4:
            layer_l4(args.concurrency)

    print("\nL5 运维与寿命（需管理台凭据，手工执行）：")
    print("  GET /api/admin/accounts → logged_in / expires_at / has_api_key")
    print("  has_api_key=false ⇒ 到期无法自动重登，见 docs/ENHANCE-PLAN.md P1")

    failed = [(c, d) for c, ok, d in RESULTS if not ok]
    blocking = [c for c, _ in failed if c in BLOCKING]
    print(f"\n合计 {len(RESULTS)} 项，失败 {len(failed)} 项，其中阻塞 {len(blocking)} 项")
    for c, d in failed:
        print(f"  {'[阻塞]' if c in BLOCKING else '[告警]'} {c} {d}")
    return 1 if blocking else 0


if __name__ == "__main__":
    sys.exit(main())
