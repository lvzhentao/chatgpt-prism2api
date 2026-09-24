#!/bin/sh
# 待登录账号的重试循环：本地副本，部署到服务器 /opt/prism-2api/data/login-loop.sh（data/ 不进 rsync）。
#
# v2：门控不靠 /auth/session 探针（它 5xx 时登录流程照样能过），
# 而是「先试登录 1 个随机待登录账号」——成功就紧接着跑整批，失败就退避 2 分钟。
# 每轮只花 1 个浏览器会话，被 Cloudflare 挡时不放大压力。
# 用法：setsid nohup sh /opt/prism-2api/data/login-loop.sh >/dev/null 2>&1 < /dev/null &
set -u

PG="docker exec prism-2api-pg psql -U prism -d prism -t -A -c"
LOG=/opt/prism-2api/data/login-loop.log
CONC=4        # CLI 并发（侧车实际同时只跑 2 个）
MAXPASS=200   # 上限（每轮最多 2 次 CLI 调用：试登录 + 整批）
IDLE=120      # 试登录失败后的退避秒数

stat() {
  $PG "SELECT count(*) FILTER (WHERE coalesce(payload->>'accessToken','')<>'') || '/' || count(*) FROM documents WHERE kind='account'"
}

# 随机取一个还没登录的账号的邮箱（保证每轮试的是不同账号，坏账号不会堵住整批）。
# 邮箱在凭据束里（顶层 payload 只有 accessToken/refreshToken/enabled/id 等键）；
# CLI 侧 --status pending 会把凭据束里的邮箱喂给 --only 匹配。
pick_pending() {
  $PG "SELECT ((payload->>'refreshToken')::jsonb)->>'email' FROM documents WHERE kind='account' AND coalesce(payload->>'accessToken','')='' ORDER BY random() LIMIT 1" | tr -d '[:space:]'
}

# 用线上唯一有效账号的 cookie 探上游会话接口（只记日志，不参与门控；2 次上限 30s）
probe() {
  tok=$($PG "SELECT payload->>'accessToken' FROM documents WHERE kind='account' AND coalesce(payload->>'accessToken','')<>'' LIMIT 1" | tr -d '[:space:]')
  [ -z "$tok" ] && { echo 0; return; }
  ok=0
  for i in 1 2; do
    code=$(curl -sS -o /dev/null -m 15 -w '%{http_code}' -X POST \
      -H "Cookie: prism_oai_access_token=$tok" https://prism.openai.com/auth/session 2>/dev/null)
    [ "$code" = "200" ] && ok=$((ok + 1))
    sleep 1
  done
  echo "$ok"
}

# 跑一次 CLI，等它结束，回显成功数
run_cli() { # $1=tag $2..=CLI 参数
  tag=$1
  shift
  f="/tmp/pass-$tag.log"
  docker exec prism-2api sh -c "rm -f $f"
  docker exec -d prism-2api sh -c "/app/prism-login $* > $f 2>&1"
  w=0
  while [ "$w" -lt 360 ]; do
    docker exec prism-2api grep -q "^成功 " "$f" 2>/dev/null && break
    docker exec prism-2api sh -c 'pgrep -f "[p]rism-login --" >/dev/null' 2>/dev/null || break
    sleep 15
    w=$((w + 1))
  done
  n=$(docker exec prism-2api sh -c "grep '^成功 ' $f 2>/dev/null | tail -1 | cut -d' ' -f2" | tr -d '\r')
  [ -z "$n" ] && n=0
  echo "$n"
}

p=0
while [ "$p" -lt "$MAXPASS" ]; do
  s=$(stat)
  logged=${s%%/*}
  total=${s##*/}
  if [ "$logged" = "$total" ]; then
    echo "[$(date '+%F %T')] 全部登录完成 logged=$s" >>"$LOG"
    break
  fi

  ok=$(probe)
  email=$(pick_pending)
  if [ -z "$email" ]; then
    echo "[$(date '+%F %T')] 取不到待登录账号（logged=$s ok=$ok/4），等 60s" >>"$LOG"
    sleep 60
    continue
  fi

  p=$((p + 1))
  n=$(run_cli "$p" "--status pending --only $email --limit 1 --backend chromium --concurrency 1")
  echo "[$(date '+%F %T')] 试登录 $email 成功 $n 个（探针 ok=$ok/4，剩余 $((total - logged))）" >>"$LOG"

  if [ "$n" -gt 0 ]; then
    p=$((p + 1))
    n2=$(run_cli "$p" "--status pending --limit 100 --backend chromium --concurrency $CONC")
    s2=$(stat)
    echo "[$(date '+%F %T')] 整批 pass=$p 成功 $n2 个，logged=$s2" >>"$LOG"
    if [ "$s2" = "$s" ]; then
      sleep "$IDLE"   # 整批零进展（其余账号都被上游挡），退避
    fi
  else
    sleep "$IDLE"
  fi
done
echo "[$(date '+%F %T')] 循环退出（最后一轮 logged=$(stat)，pass=$p）" >>"$LOG"
