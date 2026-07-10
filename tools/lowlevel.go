package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

const (
	lowLevelQueryDefaultRows = 50
	lowLevelQueryMaxRows     = 200
	lowLevelExecDefaultRows  = 10
	lowLevelExecMaxRows      = 500
)

var (
	lowLevelForbiddenReadRe = regexp.MustCompile(`(?i)\b(insert|update|delete|merge|drop|alter|create|truncate|grant|revoke|copy|vacuum|do|call|execute)\b`)
	lowLevelForbiddenExecRe = regexp.MustCompile(`(?i)\b(drop|alter|create|truncate|grant|revoke|copy|vacuum|do|call|execute|refresh|reindex|cluster)\b`)
	lowLevelBlockedTableRe  = regexp.MustCompile(`(?i)\b(api_tokens|bind_keys|worker_bind_codes|schema_migrations)\b`)
	lowLevelWhereRe         = regexp.MustCompile(`(?i)\bwhere\b`)
)

// lowLevelTools is the superadmin-only last-resort layer. Prefer domain tools:
// they encode business intent, target-level permissions, and cleaner results.
// These tools exist so the AI still has a controlled escape hatch when a domain
// tool is missing or temporarily misrouted.
func lowLevelTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("low_level_db_query",
			"超管兜底读库工具。仅在业务工具缺失/失效、需要核实事实时使用；优先使用 list_users/get_assigned_tasks/list_action_turns 等领域工具。只允许 SELECT/WITH/SHOW/EXPLAIN，禁止读凭据表，结果会限行并脱敏。",
			obj(map[string]any{
				"sql":   p("string", "只读 SQL；允许 SELECT/WITH/SHOW/EXPLAIN；可使用 $1 参数占位"),
				"args":  arr("string", "SQL 参数，按 $1/$2 顺序传入；需要数字时建议 SQL 里写 $1::bigint"),
				"limit": p("integer", "最多返回行数，默认 50，最大 200"),
			}, "sql"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if msg := checkLowLevelToolReady(d, u); msg != "" {
					return msg, nil
				}
				var args struct {
					SQL   string   `json:"sql"`
					Args  []string `json:"args"`
					Limit int      `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				sql, err := validateLowLevelQuerySQL(args.SQL)
				if err != nil {
					return err.Error(), nil
				}
				limit := clampLowLevelLimit(args.Limit, lowLevelQueryDefaultRows, lowLevelQueryMaxRows)
				qargs := stringsToAny(args.Args)
				qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				tx, err := d.Store.Pool().BeginTx(qctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
				if err != nil {
					return "", err
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				rows, err := tx.Query(qctx, sql, qargs...)
				if err != nil {
					return "", err
				}
				out, err := renderLowLevelRows(rows, limit)
				if err != nil {
					return "", err
				}
				if err := tx.Commit(qctx); err != nil {
					return "", err
				}
				return out, nil
			}),

		tool("low_level_db_exec",
			"超管兜底写库工具。仅在明确没有合适领域工具且用户要求强制底层处理时使用；优先使用 update_user_info/delete_assigned_task/delete_file/cancel_schedule 等业务工具。只允许 INSERT/UPDATE/DELETE 单语句，UPDATE/DELETE 必须带 WHERE，禁止 DDL/凭据表；默认最多影响 10 行，超出自动回滚。",
			obj(map[string]any{
				"sql":        p("string", "写 SQL；只允许 INSERT/UPDATE/DELETE；可使用 $1 参数占位"),
				"args":       arr("string", "SQL 参数，按 $1/$2 顺序传入；需要数字时建议 SQL 里写 $1::bigint"),
				"max_rows":   p("integer", "允许影响的最大行数，默认 10，最大 500；超出会回滚"),
				"reason":     p("string", "执行原因，必须说明为什么不能使用领域工具"),
				"table_hint": p("string", "可选：本次预计修改的主表名，便于审计阅读"),
			}, "sql", "reason"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if msg := checkLowLevelToolReady(d, u); msg != "" {
					return msg, nil
				}
				var args struct {
					SQL       string   `json:"sql"`
					Args      []string `json:"args"`
					MaxRows   int64    `json:"max_rows"`
					Reason    string   `json:"reason"`
					TableHint string   `json:"table_hint"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				sql, verb, err := validateLowLevelExecSQL(args.SQL, args.Reason)
				if err != nil {
					return err.Error(), nil
				}
				maxRows := int64(clampLowLevelLimit(int(args.MaxRows), lowLevelExecDefaultRows, lowLevelExecMaxRows))
				qctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				defer cancel()
				tx, err := d.Store.Pool().Begin(qctx)
				if err != nil {
					return "", err
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				tag, err := tx.Exec(qctx, sql, stringsToAny(args.Args)...)
				if err != nil {
					return "", err
				}
				affected := tag.RowsAffected()
				if affected > maxRows {
					return fmt.Sprintf("已回滚：本次会影响 %d 行，超过 max_rows=%d。请缩小 WHERE 或提高上限后再确认。", affected, maxRows), nil
				}
				if err := tx.Commit(qctx); err != nil {
					return "", err
				}
				return fmt.Sprintf("底层写库已执行：verb=%s affected_rows=%d table_hint=%s reason=%s", verb, affected, strings.TrimSpace(args.TableHint), textfmt.RedactSecrets(strings.TrimSpace(args.Reason))), nil
			}),
	}
}

func checkLowLevelToolReady(d Deps, u *store.User) string {
	if u == nil || !u.IsSuperadmin {
		return "只有超级管理员能使用底层兜底工具。"
	}
	if d.Store == nil || d.Store.Pool() == nil {
		return "当前入口没有数据库连接，无法使用底层兜底工具。"
	}
	return ""
}

func validateLowLevelQuerySQL(in string) (string, error) {
	sql, verb, err := normalizeLowLevelSQL(in)
	if err != nil {
		return "", err
	}
	switch verb {
	case "select", "with", "show", "explain":
	default:
		return "", fmt.Errorf("low_level_db_query 只允许 SELECT/WITH/SHOW/EXPLAIN，当前是 %s", verb)
	}
	if lowLevelForbiddenReadRe.MatchString(sql) {
		return "", fmt.Errorf("low_level_db_query 是只读工具，SQL 中不能包含写入/DDL 关键字")
	}
	if lowLevelBlockedTableRe.MatchString(sql) {
		return "", fmt.Errorf("底层工具禁止直接读取凭据/迁移控制表；请使用专用管理工具或改查业务表")
	}
	return sql, nil
}

func validateLowLevelExecSQL(in, reason string) (sql, verb string, err error) {
	if len([]rune(strings.TrimSpace(reason))) < 8 {
		return "", "", fmt.Errorf("low_level_db_exec 必须提供明确 reason，说明为什么不能使用领域工具")
	}
	sql, verb, err = normalizeLowLevelSQL(in)
	if err != nil {
		return "", "", err
	}
	switch verb {
	case "insert", "update", "delete":
	default:
		return "", "", fmt.Errorf("low_level_db_exec 只允许 INSERT/UPDATE/DELETE，当前是 %s", verb)
	}
	if lowLevelForbiddenExecRe.MatchString(sql) {
		return "", "", fmt.Errorf("low_level_db_exec 禁止 DDL、维护命令和过程执行")
	}
	if lowLevelBlockedTableRe.MatchString(sql) {
		return "", "", fmt.Errorf("底层写库禁止修改凭据/迁移控制表")
	}
	if (verb == "update" || verb == "delete") && !lowLevelWhereRe.MatchString(sql) {
		return "", "", fmt.Errorf("UPDATE/DELETE 必须带 WHERE；不要做无界写库")
	}
	return sql, verb, nil
}

func normalizeLowLevelSQL(in string) (sql, verb string, err error) {
	sql = strings.TrimSpace(in)
	if sql == "" {
		return "", "", fmt.Errorf("SQL 不能为空")
	}
	if strings.Contains(sql, "\x00") {
		return "", "", fmt.Errorf("SQL 不能包含 NUL 字符")
	}
	if strings.Contains(sql, "--") || strings.Contains(sql, "/*") || strings.Contains(sql, "*/") {
		return "", "", fmt.Errorf("底层 SQL 不允许注释；请传单条明确语句")
	}
	sql = strings.TrimSpace(strings.TrimSuffix(sql, ";"))
	if strings.Contains(sql, ";") {
		return "", "", fmt.Errorf("底层工具只允许单条 SQL，禁止多语句")
	}
	fields := strings.Fields(sql)
	if len(fields) == 0 {
		return "", "", fmt.Errorf("SQL 不能为空")
	}
	verb = strings.ToLower(fields[0])
	return sql, verb, nil
}

func clampLowLevelLimit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func stringsToAny(in []string) []any {
	if len(in) == 0 {
		return nil
	}
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

func renderLowLevelRows(rows pgx.Rows, limit int) (string, error) {
	defer rows.Close()
	fields := rows.FieldDescriptions()
	cols := make([]string, len(fields))
	for i, f := range fields {
		cols[i] = string(f.Name)
	}
	var b strings.Builder
	if len(cols) == 0 {
		b.WriteString("查询完成，无返回列。")
		return b.String(), rows.Err()
	}
	b.WriteString("底层查询结果")
	b.WriteString(fmt.Sprintf("（最多 %d 行）：\n", limit))
	b.WriteString(strings.Join(cols, " | "))
	b.WriteString("\n")
	count := 0
	for rows.Next() {
		if count >= limit {
			b.WriteString(fmt.Sprintf("…（已截断，仅展示前 %d 行）\n", limit))
			break
		}
		vals, err := rows.Values()
		if err != nil {
			return "", err
		}
		cells := make([]string, len(vals))
		for i, v := range vals {
			cells[i] = formatLowLevelValue(v)
		}
		b.WriteString(strings.Join(cells, " | "))
		b.WriteString("\n")
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count == 0 {
		b.WriteString("（无数据）\n")
	}
	return b.String(), nil
}

func formatLowLevelValue(v any) string {
	if v == nil {
		return "NULL"
	}
	var s string
	switch x := v.(type) {
	case time.Time:
		s = x.Format(time.RFC3339)
	case []byte:
		if utf8.Valid(x) {
			s = string(x)
		} else {
			s = fmt.Sprintf("<%d bytes>", len(x))
		}
	default:
		s = fmt.Sprint(x)
	}
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = textfmt.RedactSecrets(s)
	return textfmt.TruncateRunes(s, 180)
}
