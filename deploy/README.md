# 部署辅助

- `worker-sandbox/`：AI worker 容器化模板——每个 worker 独立容器+卷+绑定码，宿主机密不可达（安全边界在部署侧的参考实现）。没装 claude CLI 也能跑（自动回退内置智能体）。
- `backup/`：PostgreSQL 定时备份脚本与 launchd 模板。全部公司业务事实只在这个库里，务必启用。
- `qdrant/`：Qdrant 本机回环部署与 systemd 模板。它只保存可重建语义索引，丢失不会丢业务事实。
- `telegram-bot-api/`：Telegram 官方本地 Bot API 的 systemd 模板；用于突破云端 `getFile` 20 MB 限制，凭据只放服务器环境文件。
