# 本地 embedding 服务

该可选服务只在回环地址运行 BGE-M3 embedding 模型。它不会替换 Eino 的聊天模型，
也不会把 Ollama 暴露为 nbco 的聊天后端。

1. 将已校验的 Ollama 版本安装到 `/usr/local/bin/ollama`。
2. 创建 `ollama` 系统用户，状态目录使用 `/var/lib/ollama`。
3. 安装并启动 `nbco-embedding.service`，再执行 `ollama pull bge-m3`。
4. 将 `nbco-embedding-dependency.conf` 安装为 `nbco.service.d` drop-in。
5. 配置 nbco：

```json
{
  "ai": {
    "embed_model": "bge-m3",
    "embed_base_url": "http://127.0.0.1:11434/v1",
    "embed_api_key": "ollama-local"
  }
}
```

该模型的 OpenAI 兼容接口必须返回 1024 维向量。Qdrant collection 身份还包含固定
探针的向量指纹，因此更换 runtime 或模型权重会创建一套独立、可重建的索引。
