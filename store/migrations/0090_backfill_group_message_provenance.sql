-- Before message envelopes were persisted, group transcripts still used the
-- canonical speakerLine form: 【display name】message. Preserve that historical
-- label and only bind a stable user when the current directory has one exact,
-- unambiguous match. Unknown or renamed speakers deliberately remain unbound.
WITH legacy_group_messages AS (
    SELECT m.id,
           split_part(s.channel, ':group:', 1) AS provider,
           split_part(s.channel, ':group:', 2) AS external_chat_ref,
           substring(m.content FROM '^【([^】]+)】') AS actor_display_name
      FROM chat_messages m
      JOIN chat_sessions s ON s.id = m.session_id
     WHERE m.role = 'user'
       AND s.channel LIKE '%:group:%'
)
UPDATE chat_messages m
   SET provider = COALESCE(NULLIF(m.provider, ''), legacy.provider),
       external_chat_ref = COALESCE(NULLIF(m.external_chat_ref, ''), legacy.external_chat_ref),
       actor_display_name = COALESCE(NULLIF(m.actor_display_name, ''), legacy.actor_display_name, ''),
       source_created_at = COALESCE(m.source_created_at, m.created_at)
  FROM legacy_group_messages legacy
 WHERE m.id = legacy.id;

WITH unambiguous_users AS (
    SELECT name, min(id) AS user_id
      FROM users
     WHERE name <> ''
     GROUP BY name
    HAVING count(*) = 1
)
UPDATE chat_messages m
   SET actor_user_id = matched.user_id
  FROM unambiguous_users matched,
       chat_sessions session
 WHERE m.session_id = session.id
   AND session.channel LIKE '%:group:%'
   AND m.role = 'user'
   AND m.actor_user_id IS NULL
   AND m.actor_display_name = matched.name;

-- Project work facts use a dedicated projection. Backfill it from canonical
-- transcript rows once, retaining the source message as the durable identity.
INSERT INTO work_evidence (
    source_type, source_key, kind, status, title, content, actor_user_id,
    project_id, source_message_id, confidence, event_at, metadata, created_by,
    created_at, updated_at
)
SELECT COALESCE(NULLIF(m.provider, ''), split_part(session.channel, ':group:', 1)) || '_group_message',
       'legacy-chat-message:' || m.id::text,
       'communication', 'observed', m.actor_display_name,
       regexp_replace(m.content, '^【[^】]+】', ''), m.actor_user_id,
       group_project.project_id, m.id, 1,
       COALESCE(m.source_created_at, m.created_at),
       '{"historical_backfill":"chat_message_envelope_v1"}'::jsonb,
       COALESCE(m.actor_user_id, session.user_id), m.created_at, m.created_at
  FROM chat_messages m
  JOIN chat_sessions session ON session.id = m.session_id
  LEFT JOIN telegram_group_projects group_project
    ON group_project.chat_id = CASE
        WHEN m.provider = 'telegram' AND m.external_chat_ref ~ '^-?[0-9]+$'
        THEN m.external_chat_ref::bigint
       END
 WHERE m.role = 'user'
   AND session.channel LIKE '%:group:%'
   AND NOT EXISTS (
       SELECT 1 FROM work_evidence evidence
        WHERE evidence.source_message_id = m.id
          AND evidence.kind = 'communication'
   );
