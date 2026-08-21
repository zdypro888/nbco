package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Decision is the structured boundary between an AI notification judgment and
// deterministic delivery. It avoids interpreting natural-language sentinels.
type Decision struct {
	Notify  bool
	Message string
}

// ParseDecision validates the complete JSON response. A notify=true decision
// must carry deliverable text; notify=false never leaks an unused draft.
func ParseDecision(text string) (Decision, error) {
	var payload struct {
		Notify  *bool  `json:"notify"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &payload); err != nil {
		return Decision{}, fmt.Errorf("decode notification decision: %w", err)
	}
	if payload.Notify == nil {
		return Decision{}, fmt.Errorf("notification decision missing notify")
	}
	message := strings.TrimSpace(payload.Message)
	if *payload.Notify && message == "" {
		return Decision{}, fmt.Errorf("notification decision missing message")
	}
	if !*payload.Notify {
		message = ""
	}
	return Decision{Notify: *payload.Notify, Message: message}, nil
}
