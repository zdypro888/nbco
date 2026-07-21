package chat

import (
	"encoding/json"
	"sort"

	"github.com/zdypro888/nbco/ai"
)

func catalogToolNames(toolset []ai.Tool) []string {
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
