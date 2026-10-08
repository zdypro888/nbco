package sched

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/zdypro888/nbco/chat"
	"github.com/zdypro888/nbco/store"
)

// Run serially with NBCO_TEST_PG_DSN; all fixtures and fault injection stay in
// a private schema, leaving shared integration-test tables untouched.
func TestProfileRefreshReclaimedRunPreservesActionState(t *testing.T) {
	dsn := os.Getenv("NBCO_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("NBCO_TEST_PG_DSN is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	schema := fmt.Sprintf("sched_profile_refresh_%d", time.Now().UnixNano())
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	})
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		dsn = u.String()
	} else {
		dsn += " search_path=" + schema
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	admin, err := db.CreateUser(ctx, "profile-admin", true, store.Identity{
		Provider: "test", ExternalID: "profile-admin", ChatRef: "profile-admin",
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name          string
		actionStarted bool
		result        string
		outcome       string
		failFacts     bool
	}{
		{name: "started_read_failure", actionStarted: true, failFacts: true},
		{name: "cached_result_read_failure", result: "saved profile report", outcome: store.AutomationOutcomeSucceeded, failFacts: true},
		{name: "started_cached_result_read_failure", actionStarted: true, result: "saved profile report", outcome: store.AutomationOutcomeSucceeded, failFacts: true},
		{name: "fresh_read_failure", failFacts: true},
		{name: "started_empty_facts", actionStarted: true},
		{name: "cached_result_empty_facts", result: "saved profile report", outcome: store.AutomationOutcomeSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Pool().Exec(ctx, "DELETE FROM automation_runs"); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			occurrence := now.Format("2006-01") + ":b01"
			_, expires := monthlyAutomationWindow(now, 0, 1)
			run, err := db.ClaimAutomationRunUntil(ctx, kvProfileRefresh, occurrence, admin.ID, now, expires)
			if err != nil {
				t.Fatal(err)
			}
			if tc.actionStarted {
				if err := db.BeginAutomationAction(ctx, run); err != nil {
					t.Fatal(err)
				}
			}
			if tc.result != "" {
				if err := db.PrepareAutomationResult(ctx, run, tc.result, tc.outcome, ""); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Pool().Exec(ctx, "UPDATE automation_runs SET claimed_at=$1", now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			if tc.failFacts {
				if _, err := db.Pool().Exec(ctx, "ALTER TABLE tasks RENAME TO profile_refresh_unavailable_tasks"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if _, err := db.Pool().Exec(ctx, "ALTER TABLE profile_refresh_unavailable_tasks RENAME TO tasks"); err != nil {
						t.Errorf("restore facts table: %v", err)
					}
				})
			}
			s := New(db, nil, &chat.Orchestrator{}, nil, "test", time.UTC, 0, 1)
			// Keep the dispatch synchronous and avoid invoking an AI backend.
			s.aiPool.sem <- struct{}{}
			s.maybeProfileRefresh(ctx)
			got, err := db.AutomationRunByKey(ctx, kvProfileRefresh, occurrence, admin.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "pending" || got.ClaimedAt != nil || got.Attempts != run.Attempts+1 {
				t.Fatalf("expected reclaimed run to be retried, got %+v", got)
			}
			if got.ActionStarted != tc.actionStarted || got.ResultText != tc.result || got.Outcome != tc.outcome {
				t.Fatalf("retry changed durable action state: %+v", got)
			}
			if tc.failFacts && !strings.HasPrefix(got.LastError, "构建画像盘点事实失败:") {
				t.Fatalf("expected facts read failure, got %q", got.LastError)
			}
			if !tc.failFacts && got.LastError != "AI 执行池满载" {
				t.Fatalf("expected recovery dispatch, got %q", got.LastError)
			}
		})
	}
}
