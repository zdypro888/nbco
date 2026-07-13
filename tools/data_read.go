package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/vectorstore"
	"golang.org/x/sync/errgroup"
)

// dataReadTools exposes one broad read surface instead of requiring the model
// to memorize a growing set of narrow list tools. The store owns row/field
// visibility; the model owns source selection and query planning.
func dataReadTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("query_data", "通用权限感知数据读取。source 为空时列可用数据源；source='*' 对全部可见数据源做统一语义检索；指定 source 时融合Qdrant语义召回、AI规划的词法候选和精确filters。所有语义命中都按稳定ID回PostgreSQL执行当前用户的行级与字段级权限复核。适合跨对象调查、历史事实和领域工具未覆盖的查询；只读。",
			obj(map[string]any{
				"source": p("string", "数据源名；空值返回目录；'*' 跨全部可见数据源语义检索"),
				"search": p("string", "自然语言名称/内容查询；语义向量处理同义表达，AI子调用同时规划词法候选"),
				"filters": map[string]any{
					"type": "object", "description": "字段精确匹配，可选；字段必须来自数据目录",
				},
				"limit":  p("integer", "最多返回条数，默认30，最大100"),
				"offset": p("integer", "分页偏移，默认0，最大10000"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if d.Store == nil {
					return "当前入口没有数据库连接，无法读取数据。", nil
				}
				var args struct {
					Source  string         `json:"source"`
					Search  string         `json:"search"`
					Filters map[string]any `json:"filters"`
					Limit   int            `json:"limit"`
					Offset  int            `json:"offset"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if strings.TrimSpace(args.Source) == "" {
					return renderDataSources(store.DataSources(u.IsSuperadmin)), nil
				}
				args.Source = strings.TrimSpace(args.Source)
				filters, msg := normalizeDataReadFilters(args.Filters)
				if msg != "" {
					return msg, nil
				}
				if args.Source == "*" && len(filters) > 0 {
					return "跨数据源检索不能使用 filters；请指定 source 后按该数据源字段精确过滤。", nil
				}
				if args.Source == "*" && strings.TrimSpace(args.Search) == "" {
					return "source='*' 时 search 必填，避免无条件读取全部数据。", nil
				}
				plan := semanticSearchPlan{}
				if strings.TrimSpace(args.Search) != "" {
					var allowedSources []string
					if args.Source == "*" {
						allowedSources = semanticSourcesForUser(u.IsSuperadmin)
					}
					plan = planSemanticSearch(ctx, d, u, args.Search, allowedSources)
				}
				limit := args.Limit
				if limit <= 0 || limit > 100 {
					limit = 30
				}
				offset := min(max(0, args.Offset), 10_000)
				if args.Source == "*" {
					rows, err := queryAllVisibleData(ctx, d, u, strings.TrimSpace(args.Search), plan, limit, offset)
					if err != nil {
						return "", err
					}
					return renderCrossSourceRows(plan, rows), nil
				}
				readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				lexical, err := d.Store.ReadData(readCtx, u.ID, u.IsSuperadmin, store.DataReadQuery{
					Source: args.Source, Terms: plan.Terms, Filters: filters,
					Limit: limit, Offset: offset,
				})
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "数据源不存在或当前身份无权读取。请先把 source 留空查看可用目录。", nil
					}
					if strings.Contains(err.Error(), "不支持字段") {
						return err.Error(), nil
					}
					return "", err
				}
				semanticRows := semanticRowsForSource(ctx, d, u, args.Source, strings.TrimSpace(args.Search), filters, limit, offset)
				rows := mergeRankedDataRows(args.Source, semanticRows, lexical, limit)
				return renderDataRows(args.Source, plan, filters, offset, rows), nil
			}),
	}
}

type rankedDataRow struct {
	Source string
	Row    json.RawMessage
}

func semanticRowsForSource(ctx context.Context, d Deps, u *store.User, source, query string, filters map[string]string, limit, offset int) []json.RawMessage {
	if d.Semantic == nil || !d.Semantic.Enabled() || query == "" {
		return nil
	}
	if offset > 400 {
		return nil
	}
	if _, ok := store.DataSourceIDField(source); !ok {
		return nil
	}
	searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	wanted := min(limit+offset, 500)
	overfetch := min(max(wanted*10, 100), 500)
	hits, err := d.Semantic.Search(searchCtx, query, dataSemanticFilter(source, u), overfetch, 0)
	if err != nil {
		slog.Warn("Qdrant 数据检索失败，保留 PostgreSQL 词法结果", "source", source, "err", err)
		return nil
	}
	ids := make([]string, 0, len(hits))
	for _, hit := range hits {
		if hit.Source == source {
			ids = append(ids, hit.EntityID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := d.Store.ReadData(searchCtx, u.ID, u.IsSuperadmin, store.DataReadQuery{
		Source: source, EntityIDs: ids, Filters: filters, Limit: min(len(ids), 500),
	})
	if err != nil {
		slog.Warn("语义候选 PostgreSQL 权限复核失败", "source", source, "err", err)
		return nil
	}
	byID := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		if id, ok := store.DataRowEntityID(source, row); ok {
			byID[id] = row
		}
	}
	ranked := make([]json.RawMessage, 0, min(wanted, len(rows)))
	for _, id := range ids {
		if row := byID[id]; row != nil {
			ranked = append(ranked, row)
			delete(byID, id)
			if len(ranked) == wanted {
				break
			}
		}
	}
	if offset >= len(ranked) {
		return nil
	}
	return ranked[offset:]
}

func queryAllVisibleData(ctx context.Context, d Deps, u *store.User, query string, plan semanticSearchPlan, limit, offset int) ([]rankedDataRow, error) {
	sources := semanticSourcesForUser(u.IsSuperadmin)
	if len(plan.Kinds) > 0 {
		sources = slices.DeleteFunc(sources, func(source string) bool { return !slices.Contains(plan.Kinds, source) })
	}
	if len(sources) == 0 {
		return nil, nil
	}
	// Cross-source ranking is intentionally bounded to the top 500 candidates;
	// reject deeper pages before arithmetic or database work.
	if offset >= 500 {
		return nil, nil
	}
	wanted := min(limit+offset, 500)
	var semanticCandidates []rankedDataRow
	if d.Semantic != nil && d.Semantic.Enabled() {
		searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		overfetch := min(max(wanted*12, 120), 500)
		hits, err := searchVisibleSemanticSources(searchCtx, d, u, query, sources, overfetch)
		if err == nil {
			semanticCandidates = visibleRowsForHits(searchCtx, d, u, hits, min(overfetch, 500))
		} else {
			slog.Warn("Qdrant 跨数据源检索失败，回退词法", "err", err)
		}
	}
	// Lexical RRF remains useful for exact wording and must cover every source
	// selected by the planner. Qdrant hits cannot safely prune lexical recall.
	lexicalCandidates, lexicalErr := lexicalRowsAcrossSources(ctx, d, u, plan.Terms, sources, wanted, plan.Recent)
	if lexicalErr != nil {
		return nil, lexicalErr
	}
	combined := mergeCrossRankedDataRows(semanticCandidates, lexicalCandidates, wanted)
	if offset >= len(combined) {
		return nil, nil
	}
	return combined[offset:min(offset+limit, len(combined))], nil
}

func searchVisibleSemanticSources(ctx context.Context, d Deps, u *store.User, query string, sources []string, limit int) ([]vectorstore.Hit, error) {
	queryVector, err := d.Semantic.QueryVector(ctx, query)
	if err != nil {
		return nil, err
	}
	perSource := min(max(limit/max(1, len(sources))*4, 24), 160)
	// Sources with row-level ACLs cannot express their full permission graph in
	// Qdrant. Non-admin searches therefore overfetch each source, reauthorize in
	// PostgreSQL, and only then apply the global result limit.
	if !u.IsSuperadmin {
		perSource = max(perSource, 80)
	}
	var hits []vectorstore.Hit
	var searchErrors []error
	var successful int
	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(6)
	for _, source := range sources {
		source := source
		group.Go(func() error {
			items, searchErr := d.Semantic.SearchVector(groupCtx, queryVector, dataSemanticFilter(source, u), perSource, 0)
			if searchErr != nil {
				mu.Lock()
				searchErrors = append(searchErrors, fmt.Errorf("%s: %w", source, searchErr))
				mu.Unlock()
				return nil
			}
			mu.Lock()
			successful++
			hits = append(hits, items...)
			mu.Unlock()
			return nil
		})
	}
	_ = group.Wait()
	if successful == 0 && len(searchErrors) > 0 {
		return nil, errors.Join(searchErrors...)
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits, nil
}

func dataSemanticFilter(source string, user *store.User) vectorstore.Filter {
	filter := vectorstore.Filter{Must: map[string]any{vectorstore.PayloadSource: source}}
	if user == nil || user.IsSuperadmin {
		return filter
	}
	if source == semantic.SourceChatMessage {
		filter.Must[vectorstore.PayloadSessionUser] = user.ID
		filter.Must[vectorstore.PayloadConversationScope] = "private"
	}
	if source == semantic.SourceKnowledge {
		filter.MustNot = map[string]any{vectorstore.PayloadKind: store.KnowledgeKindPolicy}
	}
	return filter
}

func lexicalRowsAcrossSources(ctx context.Context, d Deps, u *store.User, terms, sources []string, wanted int, recent bool) ([]rankedDataRow, error) {
	if (len(terms) == 0 && !recent) || len(sources) == 0 {
		return nil, nil
	}
	perSource := min(max(wanted, 12), 60)
	rowsBySource := make(map[string][]json.RawMessage, len(sources))
	var readErrors []error
	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(6)
	for _, source := range sources {
		source := source
		group.Go(func() error {
			rows, err := d.Store.ReadData(groupCtx, u.ID, u.IsSuperadmin, store.DataReadQuery{
				Source: source, Terms: terms, Limit: perSource,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				readErrors = append(readErrors, fmt.Errorf("%s: %w", source, err))
			} else {
				rowsBySource[source] = rows
			}
			return nil
		})
	}
	_ = group.Wait()
	if len(readErrors) > 0 {
		return nil, errors.Join(readErrors...)
	}
	capRows := min(max(wanted*6, 60), 500)
	return interleaveLexicalRows(sources, rowsBySource, capRows), nil
}

func interleaveLexicalRows(sources []string, rowsBySource map[string][]json.RawMessage, limit int) []rankedDataRow {
	if limit <= 0 {
		return nil
	}
	out := make([]rankedDataRow, 0, limit)
	for rank := 0; ; rank++ {
		added := false
		for _, source := range sources {
			rows := rowsBySource[source]
			if rank >= len(rows) {
				continue
			}
			out = append(out, rankedDataRow{Source: source, Row: rows[rank]})
			added = true
			if len(out) == limit {
				return out
			}
		}
		if !added {
			return out
		}
	}
}

func semanticSourcesForUser(isSuperadmin bool) []string {
	var out []string
	for _, source := range store.DataSources(isSuperadmin) {
		if _, ok := store.DataSourceIDField(source.Name); ok {
			out = append(out, source.Name)
		}
	}
	return out
}

func visibleRowsForHits(ctx context.Context, d Deps, u *store.User, hits []vectorstore.Hit, limit int) []rankedDataRow {
	bySource := make(map[string][]string)
	for _, hit := range hits {
		bySource[hit.Source] = append(bySource[hit.Source], hit.EntityID)
	}
	visible := make(map[string]json.RawMessage)
	var mu sync.Mutex
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(6)
	for source, ids := range bySource {
		source, ids := source, ids
		if _, ok := store.DataSourceIDField(source); !ok {
			continue
		}
		group.Go(func() error {
			rows, err := d.Store.ReadData(groupCtx, u.ID, u.IsSuperadmin, store.DataReadQuery{
				Source: source, EntityIDs: ids, Limit: min(len(ids), 500),
			})
			if err != nil {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			for _, row := range rows {
				if id, ok := store.DataRowEntityID(source, row); ok {
					visible[source+"\x00"+id] = row
				}
			}
			return nil
		})
	}
	_ = group.Wait()
	out := make([]rankedDataRow, 0, min(limit, len(visible)))
	for _, hit := range hits {
		key := hit.Source + "\x00" + hit.EntityID
		if row := visible[key]; row != nil {
			out = append(out, rankedDataRow{Source: hit.Source, Row: row})
			delete(visible, key)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func mergeRankedDataRows(source string, primary, secondary []json.RawMessage, limit int) []json.RawMessage {
	type fused struct {
		row      json.RawMessage
		key      string
		score    float64
		bestRank int
	}
	items := make(map[string]*fused, len(primary)+len(secondary))
	for _, rows := range [][]json.RawMessage{primary, secondary} {
		for rank, row := range rows {
			key := string(row)
			if id, ok := store.DataRowEntityID(source, row); ok {
				key = id
			}
			item := items[key]
			if item == nil {
				item = &fused{row: row, key: key, bestRank: rank}
				items[key] = item
			}
			item.score += 1 / float64(60+rank+1)
			item.bestRank = min(item.bestRank, rank)
		}
	}
	ranked := make([]*fused, 0, len(items))
	for _, item := range items {
		ranked = append(ranked, item)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].bestRank != ranked[j].bestRank {
			return ranked[i].bestRank < ranked[j].bestRank
		}
		return ranked[i].key < ranked[j].key
	})
	if limit > len(ranked) {
		limit = len(ranked)
	}
	out := make([]json.RawMessage, 0, max(0, limit))
	for _, item := range ranked[:max(0, limit)] {
		out = append(out, item.row)
	}
	return out
}

func mergeCrossRankedDataRows(primary, secondary []rankedDataRow, limit int) []rankedDataRow {
	type fused struct {
		item     rankedDataRow
		key      string
		score    float64
		bestRank int
	}
	items := make(map[string]*fused, len(primary)+len(secondary))
	facts := make(map[string]string)
	for _, rows := range [][]rankedDataRow{primary, secondary} {
		for rank, row := range rows {
			id, ok := store.DataRowEntityID(row.Source, row.Row)
			key := row.Source + "\x00" + id
			if !ok {
				key = row.Source + "\x00" + string(row.Row)
			}
			factKeys := crossSourceFactKeys(row)
			for _, factKey := range factKeys {
				if existingKey := facts[factKey]; existingKey != "" {
					if existing := items[existingKey]; existing != nil && existing.item.Source != row.Source {
						key = existingKey
						break
					}
				}
			}
			item := items[key]
			if item == nil {
				item = &fused{item: row, key: key, bestRank: rank}
				items[key] = item
			}
			for _, factKey := range factKeys {
				if facts[factKey] == "" {
					facts[factKey] = key
				}
			}
			item.score += 1 / float64(60+rank+1)
			item.bestRank = min(item.bestRank, rank)
		}
	}
	ranked := make([]*fused, 0, len(items))
	for _, item := range items {
		ranked = append(ranked, item)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		if ranked[i].bestRank != ranked[j].bestRank {
			return ranked[i].bestRank < ranked[j].bestRank
		}
		return ranked[i].key < ranked[j].key
	})
	if limit > len(ranked) {
		limit = len(ranked)
	}
	out := make([]rankedDataRow, 0, max(0, limit))
	selected := make(map[*fused]bool, max(0, limit))
	perSource := make(map[string]int)
	sourceQuota := max(2, (limit+2)/3)
	for _, item := range ranked {
		if len(out) == limit {
			break
		}
		if perSource[item.item.Source] >= sourceQuota {
			continue
		}
		selected[item] = true
		perSource[item.item.Source]++
		out = append(out, item.item)
	}
	// Diversity is a ranking preference, not a hard result cap. If the query
	// genuinely matches only one source, fill the remaining slots from it.
	for _, item := range ranked {
		if len(out) == limit {
			break
		}
		if !selected[item] {
			out = append(out, item.item)
		}
	}
	return out
}

func crossSourceFactKeys(row rankedDataRow) []string {
	switch row.Source {
	case "chat_messages", "action_turns", "audit_activity", "events", "deliveries":
	default:
		return nil
	}
	var value any
	if json.Unmarshal(row.Row, &value) != nil {
		return nil
	}
	var candidates []string
	var collect func(any)
	collect = func(value any) {
		switch typed := value.(type) {
		case string:
			normalized := strings.ToLower(strings.Join(strings.Fields(typed), " "))
			if len([]rune(normalized)) >= 20 {
				candidates = append(candidates, normalized)
			}
		case []any:
			for _, item := range typed {
				collect(item)
			}
		case map[string]any:
			for _, item := range typed {
				collect(item)
			}
		}
	}
	collect(value)
	sort.SliceStable(candidates, func(i, j int) bool { return len([]rune(candidates[i])) > len([]rune(candidates[j])) })
	seen := make(map[string]bool)
	out := make([]string, 0, min(4, len(candidates)))
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
		if len(out) == 4 {
			break
		}
	}
	return out
}

func normalizeDataReadFilters(in map[string]any) (map[string]string, string) {
	if len(in) == 0 {
		return nil, ""
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, "filters 不能包含空字段名。"
		}
		switch value := value.(type) {
		case string:
			out[key] = value
		case float64:
			out[key] = strconv.FormatFloat(value, 'f', -1, 64)
		case bool:
			out[key] = strconv.FormatBool(value)
		case nil:
			out[key] = ""
		default:
			return nil, fmt.Sprintf("filters.%s 只能是字符串、数字、布尔值或 null。", key)
		}
	}
	return out, ""
}

func renderDataSources(sources []store.DataSource) string {
	if len(sources) == 0 {
		return "当前身份没有可用数据源。"
	}
	var b strings.Builder
	b.WriteString("当前身份可用的数据源（查询时 source 使用英文名）：\n")
	for _, source := range sources {
		fmt.Fprintf(&b, "- %s：%s\n  fields=%s\n", source.Name, source.Description, strings.Join(source.Fields, ","))
	}
	b.WriteString("filters 是字段精确匹配；search 会融合 Qdrant 语义召回与 AI 规划的词法候选；source='*' 可跨源搜索。结果中的内部ID可继续传给领域写工具，但最终回复不要无故暴露外部渠道标识。")
	return strings.TrimSpace(b.String())
}

func renderDataRows(source string, plan semanticSearchPlan, filters map[string]string, offset int, rows []json.RawMessage) string {
	if len(rows) == 0 {
		return fmt.Sprintf("数据源 %s 没有符合当前查询的可见记录。AI查询计划 terms=%q filters=%v。", source, plan.Terms, filters)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "数据源=%s；AI查询计划 terms=%q recent=%t；filters=%v；offset=%d。\n", source, plan.Terms, plan.Recent, filters, max(0, offset))
	for _, row := range rows {
		b.Write(row)
		b.WriteByte('\n')
	}
	b.WriteString("以上记录已经按当前调用者权限裁剪；内部ID可作为后续工具参数，不能把未返回的字段或记录推断为存在。")
	return strings.TrimSpace(b.String())
}

func renderCrossSourceRows(plan semanticSearchPlan, rows []rankedDataRow) string {
	if len(rows) == 0 {
		return fmt.Sprintf("全部可见数据源中没有找到相关记录。词法候选 terms=%q；语义索引不可用时已自动降级。", plan.Terms)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "跨数据源权限感知检索结果；词法候选 terms=%q。每条语义候选均已按稳定ID回 PostgreSQL 复核：\n", plan.Terms)
	for _, item := range rows {
		fmt.Fprintf(&b, "source=%s %s\n", item.Source, item.Row)
	}
	b.WriteString("以上只包含当前调用者可见的行和字段；请根据 source 与稳定ID调用领域工具继续核实或执行。")
	return strings.TrimSpace(b.String())
}
