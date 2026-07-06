# 部署辅助

- `worker-sandbox/`：AI worker 容器化模板——每个 worker 独立容器+卷+绑定码，宿主机密不可达（安全边界在部署侧的参考实现）。没装 claude CLI 也能跑（自动回退内置智能体）。
- `backup/`：PostgreSQL 定时备份脚本与 launchd 模板。全部公司状态只在这一个库里，务必启用。
