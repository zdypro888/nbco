-- 会话情景记忆（episodic memory）：给消息存 embedding，支持跨会话语义检索
-- 「我们之前聊过什么」。列结构与 knowledge 的向量方案一致（real[]，无 pgvector；
-- embed_model 记「模型:维度」标签，模型/维度变更自愈）。
ALTER TABLE chat_messages ADD COLUMN embedding real[];
ALTER TABLE chat_messages ADD COLUMN embed_model TEXT NOT NULL DEFAULT '';
