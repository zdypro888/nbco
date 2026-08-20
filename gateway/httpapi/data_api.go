package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/store"
)

type dataSourceView struct {
	store.DataSource
	StableIDField string `json:"stable_id_field,omitempty"`
}

func (s *Server) handleDataSources(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": visibleDataSourceViews(u.IsSuperadmin)})
}

func (s *Server) handleReadData(w http.ResponseWriter, r *http.Request) {
	u := s.requireUser(w, r)
	if u == nil {
		return
	}
	query, err := dataReadQueryFromRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	source, visible := visibleDataSource(query.Source, u.IsSuperadmin)
	if !visible {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "数据源不存在或当前身份无权读取"})
		return
	}
	for field := range query.Filters {
		if !slices.Contains(source.Fields, field) {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("数据源 %s 不支持字段 %s；可用字段：%s", source.Name, field, strings.Join(source.Fields, ", ")),
			})
			return
		}
	}
	if len(query.EntityIDs) > 0 {
		if _, supported := store.DataSourceIDField(query.Source); !supported {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "该数据源不支持稳定实体 ID 回读"})
			return
		}
	}
	rows, err := s.store.ReadData(r.Context(), u.ID, u.IsSuperadmin, query)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "数据源不存在或当前身份无权读取"})
		return
	}
	if err != nil {
		slog.Error("权限感知数据 API 读取失败", "user", u.ID, "source", query.Source, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "读取数据失败"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"source": query.Source, "rows": rows, "count": len(rows), "limit": query.Limit,
		"offset": query.Offset, "next_offset": query.Offset + len(rows), "page_full": len(rows) == query.Limit,
	})
}

func visibleDataSourceViews(isSuperadmin bool) []dataSourceView {
	sources := store.DataSources(isSuperadmin)
	views := make([]dataSourceView, 0, len(sources))
	for _, source := range sources {
		idField, _ := store.DataSourceIDField(source.Name)
		views = append(views, dataSourceView{DataSource: source, StableIDField: idField})
	}
	return views
}

func visibleDataSource(name string, isSuperadmin bool) (store.DataSource, bool) {
	for _, source := range store.DataSources(isSuperadmin) {
		if source.Name == name {
			return source, true
		}
	}
	return store.DataSource{}, false
}

func dataReadQueryFromRequest(r *http.Request) (store.DataReadQuery, error) {
	if r == nil {
		return store.DataReadQuery{}, errors.New("请求不能为空")
	}
	source := strings.TrimSpace(r.PathValue("source"))
	if source == "" {
		return store.DataReadQuery{}, errors.New("source 不能为空")
	}
	values := r.URL.Query()
	for key := range values {
		if key == "q" || key == "id" || key == "limit" || key == "offset" || strings.HasPrefix(key, "filter.") {
			continue
		}
		return store.DataReadQuery{}, fmt.Errorf("不支持查询参数 %s", key)
	}
	if len(values["q"]) > 8 {
		return store.DataReadQuery{}, errors.New("q 最多提供 8 次")
	}
	if len(values["id"]) > 500 {
		return store.DataReadQuery{}, errors.New("id 最多提供 500 次")
	}
	limit, err := boundedQueryInt(values.Get("limit"), 30, 1, 100, "limit")
	if err != nil {
		return store.DataReadQuery{}, err
	}
	offset, err := boundedQueryInt(values.Get("offset"), 0, 0, 10_000, "offset")
	if err != nil {
		return store.DataReadQuery{}, err
	}
	filters := make(map[string]string)
	for key, entries := range values {
		field, ok := strings.CutPrefix(key, "filter.")
		if !ok {
			continue
		}
		field = strings.TrimSpace(field)
		if field == "" {
			return store.DataReadQuery{}, errors.New("filter. 后必须提供字段名")
		}
		if len(entries) != 1 {
			return store.DataReadQuery{}, fmt.Errorf("filter.%s 只能提供一个值", field)
		}
		filters[field] = strings.TrimSpace(entries[0])
	}
	if len(filters) > 20 {
		return store.DataReadQuery{}, errors.New("单次最多使用 20 个精确过滤字段")
	}
	return store.DataReadQuery{
		Source: source, Terms: values["q"], EntityIDs: values["id"], Filters: filters,
		Limit: limit, Offset: offset,
	}, nil
}

func boundedQueryInt(raw string, fallback, minValue, maxValue int, name string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minValue || value > maxValue {
		return 0, fmt.Errorf("%s 必须是 %d 到 %d 的整数", name, minValue, maxValue)
	}
	return value, nil
}
