package store

import (
	"testing"
)

func TestLifecycleRollbackDoesNotReportReclaimedRows(t *testing.T) {
	s := openTestStore(t)
	ctx := t.Context()
	if err := s.SetKV(ctx, "maintenance-test", "keep"); err != nil {
		t.Fatal(err)
	}
	result, err := s.runLifecycleQueries(ctx, false, []lifecycleQuery{
		{name: "first", count: `SELECT count(*) FROM kv_state WHERE key='maintenance-test'`, apply: `DELETE FROM kv_state WHERE key='maintenance-test'`},
		{name: "second", count: `SELECT 1/0`, apply: `SELECT 1`},
	})
	if err == nil {
		t.Fatal("expected transaction failure")
	}
	if value, err := s.GetKV(ctx, "maintenance-test"); err != nil || value != "keep" {
		t.Fatalf("rollback failed: value=%q err=%v", value, err)
	}
	if result.Reclaimed != 0 || result.Details["first"] != 0 {
		t.Fatalf("reported uncommitted reclamation: %+v", result)
	}
}
