# Qdrant

Qdrant 是 nbco 的可重建语义索引，不是业务事实源。服务只监听回环地址；PostgreSQL 备份仍是完整恢复的必要数据，Qdrant 丢失后 nbco 会自动重新嵌入和回填。

生产部署：

1. 安装与 `go-client` 同一 minor 版本的 Qdrant 二进制到 `/usr/local/bin/qdrant`。
2. 将 `qdrant.yaml.example` 安装为 `/root/nbco/qdrant/qdrant.yaml`。
3. 创建权限为 `0600` 的 `/root/nbco/qdrant/qdrant.env`：`QDRANT__SERVICE__API_KEY=<随机密钥>`。
4. 安装 `nbco-qdrant.service` 到 `/etc/systemd/system/`，执行 `systemctl daemon-reload && systemctl enable --now nbco-qdrant`。
5. 在 `nbco.json` 配置相同密钥和 `http://127.0.0.1:6334`，重启 nbco。

验证：

```bash
systemctl is-active nbco-qdrant
. /root/nbco/qdrant/qdrant.env
curl -fsS -H "api-key: $QDRANT__SERVICE__API_KEY" http://127.0.0.1:6333/healthz
curl -fsS https://nbco.example.com/healthz
```

不要将 Qdrant 的 `6333/6334` 端口暴露到公网。管理员运维接口会显示 Qdrant 可用性、当前 embedding 模型与维度、最近一次结构化同步时间和记录数。
