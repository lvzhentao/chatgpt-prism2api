#!/bin/sh
set -e

# 以 root 启动时：修正数据目录权限后降权到 app 用户运行
if [ "$(id -u)" = "0" ]; then
  mkdir -p /data
  chown -R app:app /data 2>/dev/null || true
  exec su-exec app /app/web2api "$@"
fi

exec /app/web2api "$@"
