-- Preserve every historical message for audit while keeping retired control
-- text and known truncated fragments out of model replay and semantic recall.
-- This is metadata, not deletion: query_data and the control center can still
-- inspect the original row and its eligibility flag.
ALTER TABLE chat_messages
    ADD COLUMN context_eligible BOOLEAN NOT NULL DEFAULT TRUE;

CREATE INDEX idx_chat_messages_context_session
    ON chat_messages (session_id, id)
    WHERE context_eligible;

CREATE TEMP TABLE quarantined_chat_messages AS
SELECT id, session_id
  FROM chat_messages
 WHERE role = 'assistant'
   AND (
       char_length(btrim(content)) <= 3
       OR btrim(content) = '（历史异常短答复已清理）'
       OR content LIKE '%[nbco:tool_budget_exhausted]%'
       OR content LIKE '%工具调用预算%'
       OR content LIKE '%工具调用次数限制%'
       OR content LIKE '%工具调用次数上限%'
       OR content LIKE '%工具调用限制%'
       OR content LIKE '%本轮工具调用已达到上限%'
       OR btrim(content) LIKE '<tool_code>%'
       OR content LIKE '这轮没有成功执行任何写入/执行型系统工具%'
       OR content LIKE '这轮没有拿到能证明操作成功的工具结果%'
   );

UPDATE chat_messages message
   SET context_eligible = FALSE,
       embedding = NULL,
       embed_model = ''
  FROM quarantined_chat_messages quarantined
 WHERE message.id = quarantined.id;

-- Summaries and Eino-managed traces may already contain quarantined text.
-- Rotate only affected sessions so the next turn is seeded from clean rows.
UPDATE chat_sessions session
   SET summary = '',
       summary_upto = 0,
       engine_ref = '',
       updated_at = now()
 WHERE session.id IN (SELECT DISTINCT session_id FROM quarantined_chat_messages);

DROP TABLE quarantined_chat_messages;
