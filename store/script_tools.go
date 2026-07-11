package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"
)

type ScriptTool struct {
	ID               int64
	Name             string
	Description      string
	Runtime          string
	InputSchema      []byte
	Source           string
	Enabled          bool
	RequiredAction   string
	CreatedBy        int64
	LastTestResult   string
	TestedSourceHash string
	LastTestOK       bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

const scriptToolCols = `id, name, description, runtime, input_schema, source, enabled, required_action, created_by, last_test_result, tested_source_hash, last_test_ok, created_at, updated_at`

func scanScriptTool(row interface{ Scan(...any) error }) (*ScriptTool, error) {
	var st ScriptTool
	if err := row.Scan(&st.ID, &st.Name, &st.Description, &st.Runtime, &st.InputSchema, &st.Source,
		&st.Enabled, &st.RequiredAction, &st.CreatedBy, &st.LastTestResult, &st.TestedSourceHash,
		&st.LastTestOK, &st.CreatedAt, &st.UpdatedAt); err != nil {
		return nil, wrapErr(err)
	}
	return &st, nil
}

func (s *Store) CreateScriptTool(ctx context.Context, t ScriptTool) (*ScriptTool, error) {
	if len(t.InputSchema) == 0 {
		t.InputSchema = json.RawMessage(`{}`)
	}
	return scanScriptTool(s.pool.QueryRow(ctx,
		`INSERT INTO script_tools (name, description, runtime, input_schema, source, enabled, required_action, created_by, last_test_result)
		 VALUES ($1, $2, $3, $4, $5, false, $6, $7, '')
		 RETURNING `+scriptToolCols,
		t.Name, t.Description, t.Runtime, t.InputSchema, t.Source, t.RequiredAction, t.CreatedBy))
}

func (s *Store) UpdateScriptTool(ctx context.Context, id int64, t ScriptTool) (*ScriptTool, error) {
	return scanScriptTool(s.pool.QueryRow(ctx,
		`UPDATE script_tools SET
		   name = COALESCE(NULLIF($2, ''), name),
		   description = COALESCE(NULLIF($3, ''), description),
		   runtime = COALESCE(NULLIF($4, ''), runtime),
		   input_schema = COALESCE($5, input_schema),
			   source = COALESCE(NULLIF($6, ''), source),
			   required_action = COALESCE($7, required_action),
			   enabled = false,
			   last_test_result = '',
			   tested_source_hash = '',
			   last_test_ok = false,
			   updated_at = now()
		 WHERE id = $1
		 RETURNING `+scriptToolCols,
		id, t.Name, t.Description, t.Runtime, nullableJSON(t.InputSchema), t.Source, &t.RequiredAction))
}

func (s *Store) SetScriptToolEnabled(ctx context.Context, id int64, enabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var source, testedHash string
	var testOK bool
	if err := tx.QueryRow(ctx,
		`SELECT source, tested_source_hash, last_test_ok FROM script_tools WHERE id = $1 FOR UPDATE`, id).
		Scan(&source, &testedHash, &testOK); err != nil {
		return wrapErr(err)
	}
	if enabled && (!testOK || testedHash == "" || testedHash != ScriptToolSourceHash(source)) {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx,
		`UPDATE script_tools SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled); err != nil {
		return wrapErr(err)
	}
	return tx.Commit(ctx)
}

// RecordScriptToolTest records a result only if the exact tested source is still
// current. A concurrent update cannot accidentally certify different code.
func (s *Store) RecordScriptToolTest(ctx context.Context, id int64, source, result string, ok bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE script_tools
		    SET last_test_result = $3, tested_source_hash = $4, last_test_ok = $5, updated_at = now()
		  WHERE id = $1 AND source = $2`,
		id, source, result, ScriptToolSourceHash(source), ok)
	if err != nil {
		return wrapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	return nil
}

func ScriptToolSourceHash(source string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
}

func (s *Store) ScriptToolByID(ctx context.Context, id int64) (*ScriptTool, error) {
	return scanScriptTool(s.pool.QueryRow(ctx, `SELECT `+scriptToolCols+` FROM script_tools WHERE id = $1`, id))
}

func (s *Store) ScriptToolByName(ctx context.Context, name string) (*ScriptTool, error) {
	return scanScriptTool(s.pool.QueryRow(ctx, `SELECT `+scriptToolCols+` FROM script_tools WHERE name = $1`, name))
}

func (s *Store) ListScriptTools(ctx context.Context, enabledOnly bool, limit int) ([]*ScriptTool, error) {
	if limit <= 0 {
		limit = 100
	}
	where := ""
	if enabledOnly {
		where = "WHERE enabled"
	}
	rows, err := s.pool.Query(ctx, `SELECT `+scriptToolCols+` FROM script_tools `+where+` ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ScriptTool
	for rows.Next() {
		t, err := scanScriptTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullableJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}
