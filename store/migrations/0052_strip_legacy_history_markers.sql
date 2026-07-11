-- Historical replay timestamps used to be prepended as natural-language text.
-- Some models copied that internal marker into assistant replies, which then
-- reinforced itself through later replay and semantic search. Remove every
-- leading legacy layer and invalidate affected embeddings for clean backfill.
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
END $$;
