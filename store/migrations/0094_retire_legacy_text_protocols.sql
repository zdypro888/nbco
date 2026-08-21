-- Retire the old natural-language replay marker from durable records. Runtime
-- code only understands the structured nbco_history_meta tag after this point,
-- so ordinary user-visible Chinese text is never treated as a control signal.
DO $$
BEGIN
    LOOP
        UPDATE chat_messages
           SET content = ltrim(substring(content FROM strpos(content, ']') + 1)),
               embedding = NULL,
               embed_model = ''
         WHERE role = 'assistant'
           AND ltrim(content) LIKE '[历史消息时间 %'
           AND strpos(content, ']') BETWEEN 2 AND 240;
        EXIT WHEN NOT FOUND;
    END LOOP;

    LOOP
        UPDATE chat_sessions
           SET summary = ltrim(substring(summary FROM strpos(summary, ']') + 1))
         WHERE ltrim(summary) LIKE '[历史消息时间 %'
           AND strpos(summary, ']') BETWEEN 2 AND 240;
        EXIT WHEN NOT FOUND;
    END LOOP;

    LOOP
        UPDATE conversation_turns
           SET result_text = ltrim(substring(result_text FROM strpos(result_text, ']') + 1))
         WHERE ltrim(result_text) LIKE '[历史消息时间 %'
           AND strpos(result_text, ']') BETWEEN 2 AND 240;
        EXIT WHEN NOT FOUND;
    END LOOP;

    LOOP
        UPDATE action_turns
           SET reply_excerpt = ltrim(substring(reply_excerpt FROM strpos(reply_excerpt, ']') + 1))
         WHERE ltrim(reply_excerpt) LIKE '[历史消息时间 %'
           AND strpos(reply_excerpt, ']') BETWEEN 2 AND 240;
        EXIT WHEN NOT FOUND;
    END LOOP;
END $$;
