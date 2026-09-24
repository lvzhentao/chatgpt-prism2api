#!/usr/bin/env bash
# prism-2api 拉取式部署：镜像由 GitHub Actions 构建推到 Docker Hub，服务器只做 pull + up -d。
# 与 scripts/deploy.sh（本地 rsync 源码 → 服务器现场 build）是并列的两条路，互不影响。
#
# 为什么用它：本机到服务器只有 ~7KB/s（实测），源码同步/现场编译都被这条链路拖住；
# 服务器从 registry 拉镜像有 ~27MB/s。构建放到 CI（amd64 runner，与服务器同架构）后，
# 服务器侧只剩拉变更层 + 重建容器，通常 10-15s。
#
# 用法：
#   DEPLOY_HOST=YOUR_SERVER_IP DEPLOY_PORT=4344 DEPLOY_USER=root \
#     SSHPASS='...' bash scripts/deploy-pull.sh                  # 拉 app-latest / login-latest
#   TAG=536b84e bash scripts/deploy-pull.sh                      # 上线/回滚某个 sha 的镜像
#   SKIP_CHAT=1 bash scripts/deploy-pull.sh                      # 跳过对话冒烟（不消耗上游额度）
#   REGISTRY_USER=... REGISTRY_PASS=... [REGISTRY_HOST=ghcr.io] bash scripts/deploy-pull.sh
#     ↑ 服务器没存 docker 凭证时由调用方注入（CI 部署用）：pull 前临时 login，拉完即 logout，
#       不在服务器落盘。REGISTRY_HOST 默认 docker.io。两个 USER/PASS 都不设则保持旧行为。
#
# 只同步 docker-compose.yml（几 KB），不再传源码；.env / data*/ 一律不动。
# 要固化某个版本（回滚）：把 .env 里的 PRISM_APP_TAG / PRISM_LOGIN_TAG 改成 app-<sha> / login-<sha>。
#
# 注意：sshd 对新连接限流（Ubuntu 24.04 PerSourcePenalties），全程复用一条 ControlMaster。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_HOST="${DEPLOY_HOST:?请设置 DEPLOY_HOST}"
DEPLOY_USER="${DEPLOY_USER:-root}"
DEPLOY_PORT="${DEPLOY_PORT:-4344}"
DEPLOY_PATH="${DEPLOY_PATH:-/opt/prism-2api}"
APP_PORT="${APP_PORT:-8301}"
TAG="${TAG:-latest}"
TARGET="${DEPLOY_USER}@${DEPLOY_HOST}"

# TAG=latest 时用滚动 tag；给了 sha 就拉那一版（app-<sha> / login-<sha>）
APP_TAG="app-${TAG}"
LOGIN_TAG="login-${TAG}"

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

echo "[1/4] 建立 SSH 主连接（复用一条，绕开 sshd 限流）"
"${SSH[@]}" -o ControlMaster=yes -fN "$TARGET" || { echo "建立 SSH 主连接失败"; exit 1; }
mark "SSH 主连接"

echo "[2/4] 同步 compose 文件（只传 docker-compose.yml）"
rsync -az -e "$RSYNC_SSH" "$ROOT/docker-compose.yml" "$TARGET:$DEPLOY_PATH/docker-compose.yml"
mark "同步 compose"

echo "[3/4] pull 镜像 + 重建容器（${APP_TAG} / ${LOGIN_TAG}）"
# 凭证由调用方注入（CI）时临时 login，pull 完即 logout，不在服务器留 docker 凭证
REGISTRY_HOST="${REGISTRY_HOST:-docker.io}"
LOGIN_CMD=""
LOGOUT_CMD=""
if [ -n "${REGISTRY_USER:-}" ] && [ -n "${REGISTRY_PASS:-}" ]; then
  LOGIN_CMD="printf '%s' '$REGISTRY_PASS' | docker login '$REGISTRY_HOST' -u '$REGISTRY_USER' --password-stdin && "
  LOGOUT_CMD="docker logout '$REGISTRY_HOST' >/dev/null 2>&1; "
fi
# 同一条远程 shell 里 export，保证 pull 与 up 解析到同一个镜像引用
"${SSH[@]}" "$TARGET" "cd '$DEPLOY_PATH' && export PRISM_APP_TAG='$APP_TAG' PRISM_LOGIN_TAG='$LOGIN_TAG' && \
  ${LOGIN_CMD}docker compose pull -q prism-2api prism-login && ${LOGOUT_CMD}\
  docker compose up -d && \
  docker compose ps --format '{{.Name}} {{.Image}} {{.Status}}'"
mark "pull + 重建容器"

echo "--- 等健康 ---"
"${SSH[@]}" "$TARGET" "cd '$DEPLOY_PATH' && for i in \$(seq 1 40); do \
    docker inspect -f '{{.State.Health.Status}}' prism-2api 2>/dev/null | grep -q healthy && break; sleep 3; done; \
  docker inspect -f 'prism-2api health={{.State.Health.Status}}' prism-2api"
mark "等健康"

AUTH="$("${SSH[@]}" "$TARGET" "sed -n 's/^WEB2API_API_KEY=//p' '$DEPLOY_PATH/.env' | head -1")"
[ -n "$AUTH" ] || { echo "验收失败：服务器 .env 里没有 WEB2API_API_KEY"; exit 1; }

echo "--- 实际运行镜像（含 registry digest）---"
"${SSH[@]}" "$TARGET" "cd '$DEPLOY_PATH' && docker compose images prism-2api prism-login && \
  docker image inspect \$(docker inspect -f '{{.Image}}' prism-2api) -f 'prism-2api digest={{index .RepoDigests 0}}'"

echo "--- 外部可达性 http://$DEPLOY_HOST:$APP_PORT ---"
echo "GET /healthz → $(curl -s --max-time 15 "http://$DEPLOY_HOST:$APP_PORT/healthz")"
echo "GET /admin/ → $(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "http://$DEPLOY_HOST:$APP_PORT/admin/")"
echo "GET /v1/models → $(curl -s --max-time 15 -H "Authorization: Bearer $AUTH" "http://$DEPLOY_HOST:$APP_PORT/v1/models" | head -c 200)"
if [ "${SKIP_CHAT:-0}" != "1" ]; then
  echo "POST /v1/chat/completions（model 不传 → 走默认模型）→"
  curl -s --max-time 180 -H "Authorization: Bearer $AUTH" -H 'Content-Type: application/json' \
    -d '{"model":"","messages":[{"role":"user","content":"只回两个字：正常"}],"max_tokens":16,"stream":false}' \
    "http://$DEPLOY_HOST:$APP_PORT/v1/chat/completions" | head -c 300
fi
mark "外部可达性"
echo
echo "本次部署总耗时：$(( $(date +%s) - T_START ))s"
echo "网关地址：http://$DEPLOY_HOST:$APP_PORT/v1   （Authorization: Bearer \$WEB2API_API_KEY）"
echo "回滚：TAG=<旧 sha> 重跑本脚本，或把 $DEPLOY_PATH/.env 的 PRISM_APP_TAG/PRISM_LOGIN_TAG 换成旧 tag。"
