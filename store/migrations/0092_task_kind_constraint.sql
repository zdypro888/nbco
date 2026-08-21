ALTER TABLE tasks ADD CONSTRAINT tasks_kind_check
    CHECK (kind IN ('engineering','materials','review','research','operations','product_design','general'));
