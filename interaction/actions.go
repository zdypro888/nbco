// Package interaction defines structured client actions returned by an Agent
// turn. Channels render these actions using their native controls.
package interaction

import (
	"net/url"
	"strings"
)

type ActionKind string

const (
	ActionOpenURL    ActionKind = "open_url"
	ActionOpenWebApp ActionKind = "open_web_app"
)

// Action is a transport-neutral user action. It is persisted with the
// canonical conversation turn so retries preserve the same controls.
type Action struct {
	Kind  ActionKind `json:"kind"`
	Label string     `json:"label"`
	URL   string     `json:"url"`
}

// Normalize removes invalid and duplicate actions and applies a bounded label
// size before actions cross a transport boundary.
func Normalize(actions []Action, limit int) []Action {
	if limit <= 0 || len(actions) == 0 {
		return nil
	}
	out := make([]Action, 0, min(len(actions), limit))
	seen := make(map[string]struct{}, len(actions))
	for _, action := range actions {
		action.Kind = ActionKind(strings.TrimSpace(string(action.Kind)))
		action.Label = truncate(strings.TrimSpace(action.Label), 64)
		action.URL = strings.TrimSpace(action.URL)
		if action.Kind != ActionOpenURL && action.Kind != ActionOpenWebApp {
			continue
		}
		parsed, err := url.Parse(action.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
			continue
		}
		if action.Label == "" {
			action.Label = "打开"
		}
		key := string(action.Kind) + "\x00" + action.URL
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, action)
		if len(out) == limit {
			break
		}
	}
	return out
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
