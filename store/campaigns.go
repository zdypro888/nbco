package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const (
	DataCampaignActive    = "active"
	DataCampaignClosed    = "closed"
	DataCampaignCancelled = "cancelled"

	DataCampaignTargetPending   = "pending"
	DataCampaignTargetCompleted = "completed"
)

type DataCollectionCampaign struct {
	ID             int64
	Title          string
	Instruction    string
	RequiredFields []string
	Status         string
	CreatedBy      int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type DataCollectionCampaignView struct {
	DataCollectionCampaign
	CreatorName string
	Total       int64
	Completed   int64
	Pending     int64
}

type DataCollectionCampaignTarget struct {
	CampaignID     int64
	UserID         int64
	UserName       string
	Status         string
	MissingFields  []string
	LastNotifiedAt *time.Time
	CompletedAt    *time.Time
	UpdatedAt      time.Time
}

const dataCampaignCols = `id, title, instruction, required_fields, status, created_by, created_at, updated_at`

func scanDataCollectionCampaign(row interface{ Scan(...any) error }) (*DataCollectionCampaign, error) {
	var c DataCollectionCampaign
	err := row.Scan(&c.ID, &c.Title, &c.Instruction, &c.RequiredFields, &c.Status, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt)
	return &c, wrapErr(err)
}

func (s *Store) CreateDataCollectionCampaign(ctx context.Context, title, instruction string, requiredFields []string, createdBy int64, targetUserIDs []int64) (*DataCollectionCampaign, error) {
	requiredFields = normalizeStringSet(requiredFields)
	targetUserIDs = normalizeIntSet(targetUserIDs)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	c, err := scanDataCollectionCampaign(tx.QueryRow(ctx,
		`INSERT INTO data_collection_campaigns (title, instruction, required_fields, created_by)
		 VALUES ($1, $2, $3, $4) RETURNING `+dataCampaignCols,
		strings.TrimSpace(title), strings.TrimSpace(instruction), requiredFields, createdBy))
	if err != nil {
		return nil, err
	}
	for _, id := range targetUserIDs {
		if _, err := tx.Exec(ctx,
			`INSERT INTO data_collection_campaign_targets (campaign_id, user_id)
			 VALUES ($1, $2) ON CONFLICT DO NOTHING`, c.ID, id); err != nil {
			return nil, wrapErr(err)
		}
	}
	return c, tx.Commit(ctx)
}

func (s *Store) DataCollectionCampaignByID(ctx context.Context, id int64) (*DataCollectionCampaign, error) {
	return scanDataCollectionCampaign(s.pool.QueryRow(ctx,
		`SELECT `+dataCampaignCols+` FROM data_collection_campaigns WHERE id = $1`, id))
}

func (s *Store) ListDataCollectionCampaigns(ctx context.Context, viewerID int64, superadmin bool, status string, limit int) ([]DataCollectionCampaignView, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	status = strings.TrimSpace(status)
	where := []string{"true"}
	args := []any{limit}
	if !superadmin {
		args = append(args, viewerID)
		where = append(where, "(c.created_by = $2 OR EXISTS (SELECT 1 FROM data_collection_campaign_targets t WHERE t.campaign_id = c.id AND t.user_id = $2))")
	}
	switch status {
	case "", DataCampaignActive:
		args = append(args, DataCampaignActive)
		where = append(where, "c.status = $"+strconv.Itoa(len(args)))
	case "all":
	default:
		args = append(args, status)
		where = append(where, "c.status = $"+strconv.Itoa(len(args)))
	}
	rows, err := s.pool.Query(ctx,
		`SELECT c.id, c.title, c.instruction, c.required_fields, c.status, c.created_by, c.created_at, c.updated_at,
		        coalesce(u.name, ''),
		        count(t.user_id),
		        count(t.user_id) FILTER (WHERE t.status = 'completed'),
		        count(t.user_id) FILTER (WHERE t.status = 'pending')
		   FROM data_collection_campaigns c
		   LEFT JOIN users u ON u.id = c.created_by
		   LEFT JOIN data_collection_campaign_targets t ON t.campaign_id = c.id
		  WHERE `+strings.Join(where, " AND ")+`
		  GROUP BY c.id, u.name
		  ORDER BY CASE c.status WHEN 'active' THEN 0 ELSE 1 END, c.id DESC
		  LIMIT $1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DataCollectionCampaignView
	for rows.Next() {
		var v DataCollectionCampaignView
		if err := rows.Scan(&v.ID, &v.Title, &v.Instruction, &v.RequiredFields, &v.Status, &v.CreatedBy, &v.CreatedAt, &v.UpdatedAt,
			&v.CreatorName, &v.Total, &v.Completed, &v.Pending); err != nil {
			return nil, wrapErr(err)
		}
		out = append(out, v)
	}
	return out, wrapErr(rows.Err())
}

func (s *Store) DataCollectionCampaignTargets(ctx context.Context, campaignID int64) ([]DataCollectionCampaignTarget, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT t.campaign_id, t.user_id, coalesce(u.name, ''), t.status, t.missing_fields,
		        t.last_notified_at, t.completed_at, t.updated_at
		   FROM data_collection_campaign_targets t
		   JOIN users u ON u.id = t.user_id
		  WHERE t.campaign_id = $1
		  ORDER BY CASE t.status WHEN 'pending' THEN 0 ELSE 1 END, t.user_id`, campaignID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DataCollectionCampaignTarget
	for rows.Next() {
		var t DataCollectionCampaignTarget
		if err := rows.Scan(&t.CampaignID, &t.UserID, &t.UserName, &t.Status, &t.MissingFields,
			&t.LastNotifiedAt, &t.CompletedAt, &t.UpdatedAt); err != nil {
			return nil, wrapErr(err)
		}
		out = append(out, t)
	}
	return out, wrapErr(rows.Err())
}

func (s *Store) RefreshDataCollectionCampaign(ctx context.Context, campaignID int64) (int, error) {
	c, err := s.DataCollectionCampaignByID(ctx, campaignID)
	if err != nil {
		return 0, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT t.user_id, u.info
		   FROM data_collection_campaign_targets t
		   JOIN users u ON u.id = t.user_id
		  WHERE t.campaign_id = $1`, campaignID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	type row struct {
		userID int64
		info   map[string]string
	}
	var items []row
	for rows.Next() {
		var it row
		var raw []byte
		if err := rows.Scan(&it.userID, &raw); err != nil {
			return 0, wrapErr(err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &it.info); err != nil {
				return 0, err
			}
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return 0, wrapErr(err)
	}
	updated := 0
	for _, it := range items {
		missing := missingRequiredFields(c.RequiredFields, it.info)
		status := DataCampaignTargetPending
		var completedAt any = nil
		if len(missing) == 0 {
			status = DataCampaignTargetCompleted
			completedAt = time.Now()
		}
		if _, err := s.pool.Exec(ctx,
			`UPDATE data_collection_campaign_targets
			    SET status = $3,
			        missing_fields = $4,
			        completed_at = CASE WHEN $3 = 'completed' THEN COALESCE(completed_at, $5) ELSE NULL END,
			        updated_at = now()
			  WHERE campaign_id = $1 AND user_id = $2`,
			campaignID, it.userID, status, missing, completedAt); err != nil {
			return updated, wrapErr(err)
		}
		updated++
	}
	_, err = s.pool.Exec(ctx, `UPDATE data_collection_campaigns SET updated_at = now() WHERE id = $1`, campaignID)
	return updated, wrapErr(err)
}

func (s *Store) RefreshDataCollectionCampaignsForUser(ctx context.Context, userID int64) (int, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT campaign_id FROM data_collection_campaign_targets t
		  JOIN data_collection_campaigns c ON c.id = t.campaign_id
		 WHERE t.user_id = $1 AND c.status = 'active'`, userID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, wrapErr(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, wrapErr(err)
	}
	total := 0
	for _, id := range ids {
		n, err := s.RefreshDataCollectionCampaign(ctx, id)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *Store) MarkDataCollectionCampaignTargetsNotified(ctx context.Context, campaignID int64, userIDs []int64) error {
	userIDs = normalizeIntSet(userIDs)
	if len(userIDs) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx,
		`UPDATE data_collection_campaign_targets
		    SET last_notified_at = now(), updated_at = now()
		  WHERE campaign_id = $1 AND user_id = ANY($2)`, campaignID, userIDs)
	return wrapErr(err)
}

func (s *Store) SetDataCollectionCampaignStatus(ctx context.Context, campaignID int64, status string) error {
	return s.execOne(ctx,
		`UPDATE data_collection_campaigns SET status = $2, updated_at = now()
		  WHERE id = $1`, campaignID, strings.TrimSpace(status))
}

func missingRequiredFields(required []string, info map[string]string) []string {
	var missing []string
	for _, f := range normalizeStringSet(required) {
		if strings.TrimSpace(info[f]) == "" {
			missing = append(missing, f)
		}
	}
	return missing
}

func normalizeStringSet(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func normalizeIntSet(in []int64) []int64 {
	seen := map[int64]bool{}
	var out []int64
	for _, n := range in {
		if n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}
