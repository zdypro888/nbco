# nbco-worker 沙箱镜像：worker 以低权限用户跑在容器里，宿主机密（其他 worker
# 的 token、SSH 私钥等）不在其可达范围——这是 README 所说「安全边界在部署侧」的
# 参考实现。模型 CLI（claude/codex）带完整 shell，边界必须靠隔离，不靠提示词。
#
# 构建（worker 二进制从中枢下载，SERVER 传你的 public_base_url）：
#   docker build --build-arg SERVER=https://your-nbco-host:8443 -t nbco-worker .
FROM node:24.17.0-trixie-slim

ARG SERVER
ARG WORKER_BIN=nbco-worker-linux-amd64
ARG CODEX_VERSION=0.144.1
ARG CLAUDE_CODE_VERSION=2.1.207

# 常用工具：worker 干活要 git/构建链；按需增删。
RUN apt-get update && apt-get install -y --no-install-recommends \
    git curl ca-certificates build-essential python3 \
    && rm -rf /var/lib/apt/lists/*

# CLI agent：固定已验证版本保证镜像可复现。运行时某个 CLI 不可用时，
# nbco-worker 仍会按配置选择另一 CLI 或回退内置智能体。
RUN npm install -g \
    "@openai/codex@${CODEX_VERSION}" \
    "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}"

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
