package chat

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"

	"github.com/zdypro888/nbco/ai"
)

// toolTextRelevance is used only for action-audit hints. Tool availability and
// selection are handled by Eino's dynamic tool middleware, not this scorer.
func toolTextRelevance(text string, item ai.Tool) int {
	query := strings.ToLower(strings.TrimSpace(text))
	if query == "" {
		return 0
	}
	name := strings.ToLower(item.Name)
	description := strings.ToLower(item.Description)
	score := 0
	for _, part := range strings.FieldsFunc(name, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || unicode.IsSpace(r)
	}) {
		if len([]rune(part)) > 1 && strings.Contains(query, part) {
			score += 20
		}
	}
	runes := []rune(query)
	for i := 0; i+1 < len(runes); i++ {
		gram := string(runes[i : i+2])
		if strings.TrimSpace(gram) != "" &&
			(strings.Contains(description, gram) || strings.Contains(name, gram)) {
			score++
		}
	}
	return score
}

func containsAnyTerm(text string, terms []string) bool {
	for _, term := range terms {
		if term != "" && strings.Contains(text, term) {
			return true
		}
	}
	return false
}

func looksLikeFileReferenceRequest(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	hasExplicitRef := containsAnyTerm(s, fileExplicitReferenceTerms)
	hasDeicticRef := containsAnyTerm(s, fileDeicticReferenceTerms)
	hasAction := containsAnyTerm(s, fileReferenceActionTerms)
	hasStrongAction := containsAnyTerm(s, strongFileReferenceActionTerms)
	return (hasExplicitRef && hasAction) || (hasDeicticRef && hasStrongAction)
}

func routedToolNames(toolset []ai.Tool) []string {
	names := make([]string, 0, len(toolset))
	for _, item := range toolset {
		names = append(names, item.Name)
	}
	sort.Strings(names)
	return names
}

func toolSchemaChars(toolset []ai.Tool) int {
	total := 0
	for _, item := range toolset {
		total += len(item.Name) + len(item.Description)
		if raw, err := json.Marshal(item.InputSchema); err == nil {
			total += len(raw)
		}
	}
	return total
}

var fileExplicitReferenceTerms = []string{
	"文件", "附件", "上传", "pdf", "xlsx", "xls", "excel", "csv", "txt", "doc", "word",
	"图片", "照片", "资料", "表格", "文档", "file", "attachment", "document", "spreadsheet",
}

var fileDeicticReferenceTerms = []string{
	"这个", "这些", "这份", "这几", "两个", "几个", "刚才", "刚上传", "上面的", "前面的", "它", "它们",
	"this", "these", "those", "them", "it",
}

var fileReferenceActionTerms = []string{
	"看", "读", "打开", "分析", "整理", "处理", "总结", "提取", "检查", "识别", "转换", "对比", "发送", "给", "交给",
	"read", "open", "analyze", "analyse", "summarize", "extract", "process", "convert", "compare", "send",
}

var strongFileReferenceActionTerms = []string{
	"分析", "整理", "处理", "总结", "提取", "检查", "识别", "转换", "对比", "交给", "发给", "发送", "读取", "打开",
	"analyze", "analyse", "summarize", "extract", "process", "convert", "compare", "send", "read", "open",
}
