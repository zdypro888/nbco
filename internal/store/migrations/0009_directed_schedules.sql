-- 定时任务通用化：定向（给某人/全体设）、每日 HH:MM 语义（可选工作日）、
-- AI 模式（到点跑一轮 AI 现场生成个性化内容再推送）。
-- 运营节奏（作息问候、例会提醒等）由 AI 按对话动态落成这些行，代码不含任何具体政策。
ALTER TABLE schedules ADD COLUMN target TEXT NOT NULL DEFAULT 'self';       -- self | _all | <用户ID>
ALTER TABLE schedules ADD COLUMN mode TEXT NOT NULL DEFAULT 'message';      -- message=原文投递 | ai=按指令跑轮次
ALTER TABLE schedules ADD COLUMN daily_at TEXT NOT NULL DEFAULT '';         -- "HH:MM"（公司时区），kind=daily 用
ALTER TABLE schedules ADD COLUMN weekdays TEXT NOT NULL DEFAULT '';         -- "1,2,3,4,5"（1=周一…7=周日），空=每天
ALTER TABLE schedules ADD COLUMN created_by BIGINT NOT NULL DEFAULT 0;      -- 创建者（定向时与 user_id 不同）
UPDATE schedules SET created_by = user_id WHERE created_by = 0;
