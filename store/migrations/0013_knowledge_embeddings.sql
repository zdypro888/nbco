-- 知识库语义检索：存每条知识的 embedding 向量（float4[]，无需 pgvector 扩展；
-- nbco 规模下在应用层暴力 cosine 排序足够）。embed_model 记录产出向量的模型，
-- 模型变更时据此识别需重嵌入的旧行。未配 embedder 时这两列留空，检索回退词法。
ALTER TABLE knowledge ADD COLUMN embedding real[];
ALTER TABLE knowledge ADD COLUMN embed_model TEXT NOT NULL DEFAULT '';
