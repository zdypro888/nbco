-- Scheduler and event-bus turns used to share the user's interactive chat
-- session. Quarantine both the internal trigger and its immediate assistant
-- reply so old automation text cannot be replayed or semantically recalled as
-- a current user instruction. The messages remain available as internal audit
-- history rather than being destroyed.
CREATE TEMP TABLE legacy_automation_messages AS
WITH triggers AS (
    SELECT m.id, m.session_id
      FROM chat_messages m
      JOIN chat_sessions cs ON cs.id = m.session_id
     WHERE cs.channel NOT LIKE 'internal:%'
       AND m.role = 'user'
       AND (m.content LIKE '[系统定时触发·%' OR m.content LIKE '[系统事件·%')
), paired_replies AS (
    SELECT next_message.id, trigger.session_id
      FROM triggers trigger
      JOIN LATERAL (
          SELECT m.id, m.role
            FROM chat_messages m
           WHERE m.session_id = trigger.session_id AND m.id > trigger.id
           ORDER BY m.id
           LIMIT 1
      ) next_message ON next_message.role = 'assistant'
)
SELECT id, session_id FROM triggers
UNION
SELECT id, session_id FROM paired_replies;

INSERT INTO chat_sessions (user_id, channel, engine, active, created_at, updated_at)
SELECT source.user_id,
       'internal:legacy:automation:' || source.id,
       source.engine,
       false,
       min(message.created_at),
       now()
  FROM (SELECT DISTINCT session_id FROM legacy_automation_messages) legacy
  JOIN chat_sessions source ON source.id = legacy.session_id
  JOIN legacy_automation_messages moved ON moved.session_id = source.id
  JOIN chat_messages message ON message.id = moved.id
 WHERE NOT EXISTS (
       SELECT 1 FROM chat_sessions existing
        WHERE existing.user_id = source.user_id
          AND existing.channel = 'internal:legacy:automation:' || source.id
 )
 GROUP BY source.id, source.user_id, source.engine;

UPDATE chat_messages message
   SET session_id = target.id,
       embedding = NULL,
       embed_model = ''
  FROM legacy_automation_messages legacy
  JOIN chat_sessions source ON source.id = legacy.session_id
  JOIN chat_sessions target
    ON target.user_id = source.user_id
   AND target.channel = 'internal:legacy:automation:' || source.id
 WHERE message.id = legacy.id;

-- Existing summaries and managed Eino traces may already contain the moved
-- text. Force the next human turn to seed a fresh trace from clean history.
UPDATE chat_sessions source
   SET summary = '',
       summary_upto = 0,
       engine_ref = '',
       updated_at = now()
 WHERE source.id IN (SELECT DISTINCT session_id FROM legacy_automation_messages);

DROP TABLE legacy_automation_messages;
