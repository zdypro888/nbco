# nbco-worker 沙箱镜像：worker 以低权限用户跑在容器里，宿主机密（其他 worker
# 的 token、SSH 私钥等）不在其可达范围——这是 README 所说「安全边界在部署侧」的
# 参考实现。模型 CLI（claude/codex）带完整 shell，边界必须靠隔离，不靠提示词。
#
# 构建（worker 二进制从中枢下载，SERVER 传你的 public_base_url）：
#   docker build --build-arg SERVER=https://your-nbco-host:8443 -t nbco-worker .
FROM node:22-bookworm-slim

ARG SERVER
ARG WORKER_BIN=nbco-worker-linux-amd64

# 常用工具：worker 干活要 git/构建链；按需增删。
RUN apt-get update && apt-get install -y --no-install-recommends \
    git curl ca-certificates build-essential python3 \
    && rm -rf /var/lib/apt/lists/*

# claude CLI（可选）：不装也能干活——nbco-worker 会自动回退内置智能体。
RUN npm install -g @anthropic-ai/claude-code || true

RUN useradd -m -u 10001 worker

RUN curl -fsSL -o /usr/local/bin/nbco-worker "$SERVER/downloads/worker/$WORKER_BIN" \
    && chmod +x /usr/local/bin/nbco-worker

# 绑定码经环境变量传入，容器首次启动时兑换（token 只存在容器卷里）。
# docker run -e NBCO_SERVER=... -e NBCO_BIND_CODE=wbc_xxx -v worker-config:/home/worker/.config/nbco -v worker-work:/home/worker/nbco-work
COPY entrypoint.sh /usr/local/bin/nbco-worker-entrypoint
RUN chmod +x /usr/local/bin/nbco-worker-entrypoint

USER worker
WORKDIR /home/worker
ENTRYPOINT ["/usr/local/bin/nbco-worker-entrypoint"]
