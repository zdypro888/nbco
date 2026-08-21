UPDATE task_outcomes
SET task_kind = 'general'
WHERE task_kind NOT IN ('engineering','materials','review','research','operations','product_design','general');

ALTER TABLE task_outcomes ADD CONSTRAINT task_outcomes_kind_check
    CHECK (task_kind IN ('engineering','materials','review','research','operations','product_design','general'));
