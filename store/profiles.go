package store

import (
	"context"
	"time"
)

// Profile 一条画像条目：author 写在 subject 身上的一段认知。
// subject == author 时是自我介绍。所有权归 author，可见性由被动权限控制。
type Profile struct {
	ID        int64
	SubjectID int64
	AuthorID  int64
	Position  int
	Content   string
	UpdatedAt time.Time
}

// ReplaceProfiles 整体替换 author 写在 subject 身上的画像列表（一个事务）。
func (s *Store) ReplaceProfiles(ctx context.Context, subjectID, authorID int64, contents []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`DELETE FROM profiles WHERE subject_id = $1 AND author_id = $2`, subjectID, authorID); err != nil {
		return err
	}
	for i, c := range contents {
		if _, err := tx.Exec(ctx,
			`INSERT INTO profiles (subject_id, author_id, position, content) VALUES ($1, $2, $3, $4)`,
			subjectID, authorID, i, c); err != nil {
			return wrapErr(err)
		}
	}
	return tx.Commit(ctx)
}

// ProfilesBy 取 author 写在 subject 身上的画像。
func (s *Store) ProfilesBy(ctx context.Context, subjectID, authorID int64) ([]Profile, error) {
	return s.queryProfiles(ctx,
		`SELECT id, subject_id, author_id, position, content, updated_at
		 FROM profiles WHERE subject_id = $1 AND author_id = $2 ORDER BY position`, subjectID, authorID)
}

// ProfilesOn 取 subject 身上全部作者的画像（可见性由调用方按被动权限过滤）。
func (s *Store) ProfilesOn(ctx context.Context, subjectID int64) ([]Profile, error) {
	return s.queryProfiles(ctx,
		`SELECT id, subject_id, author_id, position, content, updated_at
		 FROM profiles WHERE subject_id = $1 ORDER BY author_id, position`, subjectID)
}

func (s *Store) queryProfiles(ctx context.Context, sql string, args ...any) ([]Profile, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ps []Profile
	for rows.Next() {
		var p Profile
		if err := rows.Scan(&p.ID, &p.SubjectID, &p.AuthorID, &p.Position, &p.Content, &p.UpdatedAt); err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, rows.Err()
}
