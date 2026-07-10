package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
)

// dataReadTools exposes one broad read surface instead of requiring the model
// to memorize a growing set of narrow list tools. The store owns row/field
// visibility; the model owns source selection and query planning.
func dataReadTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("query_data", "通用权限感知数据读取。source 为空时先列可用数据源和字段；指定 source 后可用自然语言 search、精确 filters、limit/offset 查询。由 AI 选择数据源与条件，服务端按当前用户强制行级和字段级权限。适合领域工具没有覆盖的查询、跨对象调查和核实底层事实；只读，不执行任何修改。",
			obj(map[string]any{
				"source": p("string", "数据源名；省略或空字符串时返回当前身份可用的数据目录"),
				"search": p("string", "自然语言名称/内容查询，可选；AI 子调用会规划字面候选词并纠正明显错别字"),
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
				filters, msg := normalizeDataReadFilters(args.Filters)
				if msg != "" {
					return msg, nil
				}
				plan := semanticSearchPlan{}
				if strings.TrimSpace(args.Search) != "" {
					plan = planSemanticSearch(ctx, d, u, args.Search, nil)
				}
				readCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				rows, err := d.Store.ReadData(readCtx, u.ID, u.IsSuperadmin, store.DataReadQuery{
					Source: args.Source, Terms: plan.Terms, Filters: filters,
					Limit: args.Limit, Offset: args.Offset,
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
				return renderDataRows(strings.TrimSpace(args.Source), plan, filters, args.Offset, rows), nil
			}),
	}
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
	b.WriteString("filters 是字段精确匹配；search 由 AI 规划为候选词。结果中的内部ID可继续传给领域写工具，但最终回复不要无故暴露外部渠道标识。")
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
