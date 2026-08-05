# 本地 embedding 服务

该可选服务只在回环地址运行 BGE-M3 embedding 模型。它不会替换 Eino 的聊天模型，
也不会把 Ollama 暴露为 nbco 的聊天后端。

1. 将已校验的 Ollama 版本安装到 `/usr/local/bin/ollama`。
2. 创建 `ollama` 系统用户，状态目录使用 `/var/lib/ollama`。
3. 安装并启动 `nbco-embedding.service`，再执行 `ollama pull bge-m3`。
4. 将 `nbco-embedding-dependency.conf` 安装为 `nbco.service.d` drop-in。

服务模板使用 `OLLAMA_KEEP_ALIVE=-1` 常驻已加载的 embedding 模型。语义检索位于
在线对话路径，按空闲时间卸载会让首轮规则与 Skill 召回承担完整模型加载延迟。
5. 配置 nbco：

```json
{
  "ai": {
    "embed_model": "bge-m3",
    "embed_base_url": "http://127.0.0.1:11434/v1",
    "embed_api_key": "ollama-local",
    "embed_revision": "ollama-0.31.2-bge-m3-daec91ff-ctx8192"
  }
}
```

该模型的 OpenAI 兼容接口必须返回 1024 维向量。Qdrant collection 身份还包含固定
探针的向量指纹，因此更换 runtime 或模型权重会创建一套独立、可重建的索引。
