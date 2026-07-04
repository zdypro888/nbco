package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestWrapErr(t *testing.T) {
	if wrapErr(nil) != nil {
		t.Error("nil 应原样返回")
	}
	if !errors.Is(wrapErr(pgx.ErrNoRows), ErrNotFound) {
		t.Error("ErrNoRows 应译为 ErrNotFound")
	}
	if !errors.Is(wrapErr(fmt.Errorf("查询: %w", pgx.ErrNoRows)), ErrNotFound) {
		t.Error("包装过的 ErrNoRows 也应译为 ErrNotFound")
	}
	if !errors.Is(wrapErr(&pgconn.PgError{Code: "23505"}), ErrConflict) {
		t.Error("unique_violation 应译为 ErrConflict")
	}
	if !errors.Is(wrapErr(fmt.Errorf("写入: %w", &pgconn.PgError{Code: "23505"})), ErrConflict) {
		t.Error("包装过的 unique_violation 也应译为 ErrConflict")
	}
	if errors.Is(wrapErr(&pgconn.PgError{Code: "23503"}), ErrConflict) {
		t.Error("非 23505 的 pg 错误不应映射为 ErrConflict")
	}
	other := errors.New("boom")
	if wrapErr(other) != other {
		t.Error("其余错误应透传")
	}
}
