package tools

import (
	"strings"
	"testing"
)

func TestValidateLowLevelQuerySQL(t *testing.T) {
	for _, sql := range []string{
		"select id, title from tasks where id = $1",
		"WITH recent AS (select id from tasks) select * from recent",
		"show timezone",
		"explain select * from users where id = $1",
	} {
		if _, err := validateLowLevelQuerySQL(sql); err != nil {
			t.Fatalf("query SQL should pass %q: %v", sql, err)
		}
	}
	for _, sql := range []string{
		"update tasks set status='done' where id=1",
		"select * from api_tokens",
		"select 1; select 2",
		"select * from users -- comment",
		"WITH bad AS (delete from tasks where id=1 returning *) select * from bad",
	} {
		if _, err := validateLowLevelQuerySQL(sql); err == nil {
			t.Fatalf("query SQL should fail %q", sql)
		}
	}
}

func TestValidateLowLevelExecSQL(t *testing.T) {
	for _, sql := range []string{
		"update tasks set status='archived' where id = $1::bigint",
		"delete from schedules where id = $1::bigint",
		"insert into task_progress(task_id, author_id, content) values ($1::bigint, $2::bigint, $3)",
	} {
		if _, _, err := validateLowLevelExecSQL(sql, "领域工具缺失时由超管兜底处理"); err != nil {
			t.Fatalf("exec SQL should pass %q: %v", sql, err)
		}
	}
	for _, sql := range []string{
		"select * from tasks",
		"update tasks set status='archived'",
		"delete from schedules",
		"drop table tasks",
		"update api_tokens set revoked_at=now() where token_hash=$1",
		"update tasks set status='done' where id=1; update tasks set status='done' where id=2",
	} {
		if _, _, err := validateLowLevelExecSQL(sql, "领域工具缺失时由超管兜底处理"); err == nil {
			t.Fatalf("exec SQL should fail %q", sql)
		}
	}
	if _, _, err := validateLowLevelExecSQL("update tasks set status='done' where id=1", "太短"); err == nil {
		t.Fatal("exec SQL should require meaningful reason")
	}
}

func TestClampLowLevelLimit(t *testing.T) {
	if got := clampLowLevelLimit(0, 10, 100); got != 10 {
		t.Fatalf("default = %d", got)
	}
	if got := clampLowLevelLimit(150, 10, 100); got != 100 {
		t.Fatalf("max = %d", got)
	}
	if got := clampLowLevelLimit(50, 10, 100); got != 50 {
		t.Fatalf("value = %d", got)
	}
}

func TestFormatLowLevelValuePreservesAuthorizedCanonicalValue(t *testing.T) {
	secret := "token=" + strings.Repeat("a", 48)
	if got := formatLowLevelValue(secret); got != secret {
		t.Fatalf("authorized row was destructively redacted: %q", got)
	}
}
