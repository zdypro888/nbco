package store

import (
	"context"
	"encoding/json"
	"time"
)

type ScriptTool struct {
	ID             int64
	Name           string
	Description    string
	Runtime        string
	InputSchema    []byte
	Source         string
	Enabled        bool
	RequiredAction string
	CreatedBy      int64
	LastTestResult string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

const scriptToolCols = `id, name, description, runtime, input_schema, source, enabled, required_action, created_by, last_test_result, created_at, updated_at`

func scanScriptTool(row interface{ Scan(...any) error }) (*ScriptTool, error) {
	var st ScriptTool
	if err := row.Scan(&st.ID, &st.Name, &st.Description, &st.Runtime, &st.InputSchema, &st.Source,
		&st.Enabled, &st.RequiredAction, &st.CreatedBy, &st.LastTestResult, &st.CreatedAt, &st.UpdatedAt); err != nil {
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
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING `+scriptToolCols,
		t.Name, t.Description, t.Runtime, t.InputSchema, t.Source, t.Enabled, t.RequiredAction, t.CreatedBy, t.LastTestResult))
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
		   updated_at = now()
		 WHERE id = $1
		 RETURNING `+scriptToolCols,
		id, t.Name, t.Description, t.Runtime, nullableJSON(t.InputSchema), t.Source, &t.RequiredAction))
}

func (s *Store) SetScriptToolEnabled(ctx context.Context, id int64, enabled bool) error {
	return s.execOne(ctx, `UPDATE script_tools SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
}

func (s *Store) SetScriptToolTestResult(ctx context.Context, id int64, result string) error {
	return s.execOne(ctx, `UPDATE script_tools SET last_test_result = $2, updated_at = now() WHERE id = $1`, id, result)
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
