# 登录侧车镜像：真浏览器 + node（sentinel）+ xvfb。
#
# 为什么单独一个容器：网关镜像只装 Go 二进制（40MB），浏览器运行时（chromium ~400MB）
# 塞进去会拖慢每次部署；而且登录是重体力活（一个浏览器一份内存），
# 独立容器可以单独重启/限流，不牵连 API 进程。
#
# 用法（compose 里由 prism-login 服务拉起）：
#   python login.py --serve --port 8099 --backend chromium --max-concurrent 2
#
# headful 过 Cloudflare 是硬要求（实测 headless 必挂），容器里用 xvfb 提供显示。
FROM python:3.12-slim-bookworm

ENV DEBIAN_FRONTEND=noninteractive \
    PYTHONUNBUFFERED=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright

# node（sentinel 运行时）+ xvfb（虚拟显示）+ playwright 系统依赖
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl gnupg xvfb xauth fonts-liberation tini \
    && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# playwright + chromium（含系统依赖）；curl_cffi 供协议登录路径用
RUN pip install --no-cache-dir "playwright==1.60.0" "curl_cffi>=0.6.0" \
    && playwright install --with-deps chromium \
    && rm -rf /var/lib/apt/lists/*

# sentinel 运行时（协议登录用）：node + jsdom
WORKDIR /app/sentinel
RUN npm init -y >/dev/null 2>&1 && npm install --no-audit --no-fund jsdom

WORKDIR /app
COPY sidecar/login/ /app/

# 账号的 storage_state（设备指纹）与截图落盘目录，compose 挂卷持久化
ENV PRISM_LOGIN_STATE_DIR=/data/login-state \
    DISPLAY=:99
RUN mkdir -p /data/login-state

EXPOSE 8099
ENTRYPOINT ["/usr/bin/tini", "--", "xvfb-run", "-a", "-s", "-screen 0 1440x900x24"]
CMD ["python", "/app/login.py", "--serve", "--port", "8099", "--backend", "chromium", "--max-concurrent", "2"]
