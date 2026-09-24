#!/usr/bin/env python3
"""池子自动恢复守护（2026-09-20 上游平台故障后部署）。

背景：上游 backend/1/new RBAC 403 波把全池 token 处决（旧代码 403 即杀）。
token 备份在 /opt/prism-2api/data/backups/accounts-full-20260919-125112.jsonl
（184 个带 token）。真浏览器链路也被拒 = 平台级故障，只能等恢复。

行为：每 PROBE_SEC 探测一次上游（sidecar 真浏览器 chat_material——它走完整
登录态链路，是最灵敏的恢复信号）；连续 2 次成功视为恢复，随即两遍法批量
回灌 token（pass1 按备份名导入拿 duplicate 映射；pass2 按池内现名 overwrite
覆写 token），然后退出。日志写 stdout（nohup 重定向到 data/restore-pool.log）。
"""
import json
import time
import urllib.request
import http.cookiejar

BACKUP = "/opt/prism-2api/data/backups/accounts-full-20260919-125112.jsonl"
BASE = "http://127.0.0.1:8301"
SIDECAR = "http://127.0.0.1:8099"
ADMIN_USER = "admin"
ADMIN_PASS = "JJBEE1rzr0OHIzXiSKn7"
PROBE_SEC = 300


def log(msg):
    print(f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] {msg}", flush=True)


class Admin:
    def __init__(self):
        cj = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
        self.op.open(urllib.request.Request(
            BASE + "/api/admin/login",
            data=json.dumps({"username": ADMIN_USER, "password": ADMIN_PASS}).encode(),
            headers={"Content-Type": "application/json"}))

    def import_batch(self, items, overwrite=True):
        req = urllib.request.Request(
            BASE + "/api/admin/accounts/import",
            data=json.dumps({"accounts": items, "overwrite": overwrite}).encode(),
            headers={"Content-Type": "application/json"})
        r = json.loads(self.op.open(req).read())
        tid = (r.get("task") or {}).get("id")
        for _ in range(120):
            time.sleep(1)
            t = json.loads(self.op.open(BASE + "/api/admin/tasks/" + str(tid)).read())
            tk = t.get("task") or t
            if tk.get("status") in ("done", "failed", "canceled"):
                return tk.get("result") or {}
        return {"timeout": True}

    def logged_in_count(self):
        d = json.loads(self.op.open(BASE + "/api/admin/accounts").read())
        return sum(1 for a in d.get("accounts", []) if a.get("logged_in"))


def probe_upstream():
    """sidecar 真浏览器抓一次材料：done 即上游对话链路恢复。"""
    try:
        req = urllib.request.Request(
            SIDECAR + "/login",
            data=json.dumps({"action": "chat_material", "flow": "prism_inference",
                             "page_url": "https://prism.openai.com/", "state_index": 0}).encode(),
            headers={"Content-Type": "application/json"})
        r = urllib.request.urlopen(req, timeout=110)
        for line in r:
            ev = json.loads(line)
            if ev.get("event") == "done":
                return True
            if ev.get("event") == "error":
                return False
    except Exception as e:
        log(f"probe exc: {type(e).__name__}")
    return False


def load_backup():
    rows = []
    for line in open(BACKUP):
        line = line.strip()
        if not line:
            continue
        p = json.loads(line)
        p = p.get("payload") or p
        if p.get("accessToken"):
            rows.append(p)
    return rows


def to_item(p, name):
    item = {"name": name, "access_token": p["accessToken"]}
    if p.get("refreshToken"):
        item["refresh_token"] = p["refreshToken"]
    if p.get("api_key"):
        item["api_key"] = p["api_key"]
    return item


def restore():
    rows = load_backup()
    log(f"备份载入 {len(rows)} 个带 token 账号，开始两遍法回灌")
    mapping = {}  # 备份名 -> 池内现名
    pass2 = []
    for i in range(0, len(rows), 50):
        batch = rows[i:i + 50]
        items = [to_item(p, (p.get("id") or p.get("name") or "")[:32]) for p in batch]
        res = Admin().import_batch(items)
        for it, r in zip(items, (res.get("items") or [])):
            err = r.get("error") or ""
            if r.get("status") == "error" and err.startswith("duplicate of "):
                real = err[len("duplicate of \""):-1]
                mapping[it["name"]] = real
            elif r.get("status") == "error":
                log(f"pass1 {it['name']}: {err[:80]}")
        log(f"pass1 批 {i//50+1}: duplicate 命中 {len(mapping)} 累计")
    for backup_name, real_name in mapping.items():
        p = next((x for x in rows if (x.get("id") or x.get("name") or "")[:32] == backup_name), None)
        if p:
            pass2.append(to_item(p, real_name))
    if pass2:
        for i in range(0, len(pass2), 50):
            res = Admin().import_batch(pass2[i:i + 50])
            ok = sum(1 for r in (res.get("items") or []) if r.get("status") != "error")
            log(f"pass2 批 {i//50+1}: 恢复 {ok}/{len(pass2[i:i+50])}")
    a = Admin()
    log(f"回灌完成，当前 logged_in={a.logged_in_count()}")


def main():
    log("恢复守护启动：每 300s 探测（sidecar 真浏览器材料链路），连续 2 次成功即回灌")
    streak = 0
    while True:
        ok = probe_upstream()
        streak = streak + 1 if ok else 0
        log(f"probe={'OK' if ok else 'fail'} streak={streak}")
        if streak >= 2:
            log("上游恢复实锤（连续 2 次），开始回灌 token 池")
            try:
                restore()
            except Exception as e:
                log(f"restore exc: {type(e).__name__}: {e}")
                streak = 0  # 失败回到探测循环，下轮再试
                continue
            log("守护退出")
            return
        time.sleep(PROBE_SEC)


if __name__ == "__main__":
    main()
