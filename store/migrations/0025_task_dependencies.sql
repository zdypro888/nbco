-- 任务依赖编排：depends_on 里的任务全部 accepted 之前，worker 领不到该任务。
-- 依赖只能指向已存在的任务（新任务 id 恒大于依赖），天然无环。
ALTER TABLE tasks ADD COLUMN depends_on BIGINT[] NOT NULL DEFAULT '{}';
