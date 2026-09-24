#!/usr/bin/env bash
# prism-2api 部署：本地源码 → 服务器现场 docker compose build → 重建容器 → 线上验收。
#
# 用法：
#   DEPLOY_HOST=YOUR_SERVER_IP DEPLOY_PORT=4344 DEPLOY_USER=root \
#     SSHPASS='...' DEPLOY_DIRTY=1 bash scripts/deploy.sh
#
# 认证：SSHPASS 非空走 sshpass，否则走 ssh 密钥/agent。
# 安全：默认只同步 HEAD 的已提交内容；DEPLOY_DIRTY=1 同步工作区（含未提交改动）。
#       服务器上的 .env / data*/ 永远保留（rsync exclude，且 bootstrap 幂等）。
# 计时：每个阶段结束打印「本阶段/累计」耗时，结尾打印总耗时（⏱ 行便于 grep）。
# 参数：NO_CACHE=1 让 compose build 带上 --no-cache（测冷构建 / 排除缓存干扰）。
#
# 注意：sshd 对新连接限流（Ubuntu 24.04 PerSourcePenalties），整场部署复用一条
#       ControlMaster 连接；逐条新建 ssh 会被拒（Permission denied）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_HOST="${DEPLOY_HOST:?请设置 DEPLOY_HOST}"
DEPLOY_USER="${DEPLOY_USER:-root}"
DEPLOY_PORT="${DEPLOY_PORT:-4344}"
DEPLOY_PATH="${DEPLOY_PATH:-/opt/prism-2api}"
APP_PORT="${APP_PORT:-8301}"
REV="${REV:-HEAD}"
TARGET="${DEPLOY_USER}@${DEPLOY_HOST}"

SSH_BIN=(ssh)
if [ -n "${SSHPASS:-}" ]; then
  command -v sshpass >/dev/null || { echo "需要 sshpass 才能用密码认证"; exit 1; }
  SSH_BIN=(sshpass -e ssh)
fi
CTL="/tmp/prism2api-deploy-%C"
SSH_OPTS=(
  -p "$DEPLOY_PORT"
  -o StrictHostKeyChecking=accept-new
  -o ServerAliveInterval=30
  -o ControlMaster=auto
  -o ControlPersist=300
  -o "ControlPath=$CTL"
)
SSH=("${SSH_BIN[@]}" "${SSH_OPTS[@]}")
RSYNC_SSH="${SSH_BIN[*]} ${SSH_OPTS[*]}"

# 计时：每阶段结束打印本阶段/累计耗时，结尾打印总耗时
T_START=$(date +%s); T_LAST=$T_START
mark() { local now; now=$(date +%s); printf '     ⏱ %-18s 本阶段 %2ss｜累计 %2ss\n' "$1" "$((now - T_LAST))" "$((now - T_START))"; T_LAST=$now; }

# NO_CACHE=1 强制全量重建（用于测冷构建耗时 / 排除缓存干扰）
BUILD_FLAGS=()
[ "${NO_CACHE:-0}" = "1" ] && BUILD_FLAGS=(--no-cache)

# 一条长连接复用到底
"${SSH[@]}" -o ControlMaster=yes -fN "$TARGET" || { echo "建立 SSH 主连接失败"; exit 1; }
mark "SSH 主连接"

if [ "${DEPLOY_DIRTY:-0}" = "1" ]; then
  REV_SHA="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo none)+dirty"
  echo "[1/5] DEPLOY_DIRTY=1：同步当前工作区（基线 ${REV_SHA}）"
  SYNC_SRC="$ROOT/"
  EXTRA_EXCLUDES=(--exclude '.git/')
else
  REV_SHA="$(git -C "$ROOT" rev-parse --short "$REV")"
  echo "[1/5] 导出提交 $REV_SHA 的干净源码"
  STAGE="$(mktemp -d "${TMPDIR:-/tmp}/prism2api-deploy.XXXXXX")"
  trap 'rm -rf "$STAGE"' EXIT
  git -C "$ROOT" archive "$REV" | tar -x -C "$STAGE"
  SYNC_SRC="$STAGE/"
  EXTRA_EXCLUDES=()
fi
mark "导出源码"

echo "[2/5] 预检服务器（目录 / docker / compose）"
"${SSH[@]}" "$TARGET" "mkdir -p '$DEPLOY_PATH' && command -v docker >/dev/null && docker compose version >/dev/null" \
  || { echo "预检失败：需要 docker + docker compose v2"; exit 1; }
mark "预检服务器"

echo "[3/5] rsync 源码（保留 .env / data*/，--delete 清理陈旧文件）"
rsync -az --delete \
  --exclude '.env' --exclude 'data/' --exclude 'data-pg/' --exclude 'data-mock/' --exclude 'data-login-state/' \
  --exclude '.DS_Store' --exclude 'web/node_modules/' --exclude 'web/dist/' \
  --exclude 'har/' --exclude 'cookies/' --exclude '*.log' \
  --exclude '.codegraph/' --exclude '.cursor/' \
  ${EXTRA_EXCLUDES[@]+"${EXTRA_EXCLUDES[@]}"} \
  -e "$RSYNC_SSH" "$SYNC_SRC" "$TARGET:$DEPLOY_PATH/"
mark "rsync 同步"

echo "[4/5] 引导 .env（幂等；缺失的密钥现场生成，已存在则保留）"
"${SSH[@]}" "$TARGET" "cd '$DEPLOY_PATH' && APP_PORT='$APP_PORT' bash -s" <<'BOOTSTRAP'
set -euo pipefail
f=.env
[ -f "$f" ] || : > "$f"
chmod 600 "$f"
get() { sed -n "s/^$1=//p" "$f" | head -1; }
put() { grep -q "^$1=" "$f" || printf '%s=%s\n' "$1" "$2" >> "$f"; }
put PRISM_HOST_PORT   "${APP_PORT:-8301}"
put PRISM_DB_USER     prism
put PRISM_DB_NAME     prism
put PRISM_DB_PASSWORD "$(openssl rand -hex 16)"
put PRISM_ENCRYPT_KEY "$(openssl rand -hex 32)"
put WEB2API_API_KEY   "$(openssl rand -hex 24)"
put TZ                Asia/Shanghai
put PRISM_IMAGE_REPO  "${IMAGE_REPO:-your-dockerhub-user/prism-2api}"
put WEB2API_DEFAULT_MODEL gpt-5.6-sol
echo "--- .env（密钥略）---"
sed -E 's/^(PRISM_DB_PASSWORD|PRISM_ENCRYPT_KEY|WEB2API_API_KEY)=.*/\1=***/' "$f"
BOOTSTRAP
mark "引导 .env"

echo "[5/5] 现场 build + 重建容器${BUILD_FLAGS:+（NO_CACHE=1，不用缓存）}"
"${SSH[@]}" "$TARGET" "cd '$DEPLOY_PATH' && docker compose build ${BUILD_FLAGS[@]+"${BUILD_FLAGS[@]}"} prism-2api prism-login && docker compose up -d && docker compose ps --format '{{.Name}} {{.Status}} {{.Ports}}'"
mark "build + 重建容器"

echo "--- 等健康 ---"
"${SSH[@]}" "$TARGET" "cd '$DEPLOY_PATH' && for i in \$(seq 1 40); do \
    docker inspect -f '{{.State.Health.Status}}' prism-2api 2>/dev/null | grep -q healthy && break; sleep 3; done; \
  docker inspect -f 'prism-2api health={{.State.Health.Status}}' prism-2api"
mark "等健康"

AUTH="$("${SSH[@]}" "$TARGET" "sed -n 's/^WEB2API_API_KEY=//p' '$DEPLOY_PATH/.env' | head -1")"
[ -n "$AUTH" ] || { echo "验收失败：服务器 .env 里没有 WEB2API_API_KEY"; exit 1; }

echo "--- 外部可达性 http://$DEPLOY_HOST:$APP_PORT ---"
echo "GET /healthz → $(curl -s --max-time 15 "http://$DEPLOY_HOST:$APP_PORT/healthz")"
echo "GET /admin/ → $(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "http://$DEPLOY_HOST:$APP_PORT/admin/")"
echo "GET /v1/models → $(curl -s --max-time 15 -H "Authorization: Bearer $AUTH" "http://$DEPLOY_HOST:$APP_PORT/v1/models" | head -c 200)"
mark "外部可达性"
echo
echo "本次部署总耗时：$(( $(date +%s) - T_START ))s"
echo "部署完成：$REV_SHA → $TARGET:$DEPLOY_PATH"
echo "网关地址：http://$DEPLOY_HOST:$APP_PORT/v1   （Authorization: Bearer \$WEB2API_API_KEY）"
echo "管理台：http://$DEPLOY_HOST:$APP_PORT/admin/   （账号 admin，密码见服务器 $DEPLOY_PATH/.env 之外的手工设置）"
