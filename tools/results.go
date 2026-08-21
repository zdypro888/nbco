package tools

import (
	"encoding/json"
	"strings"

	"github.com/zdypro888/nbco/ai"
)

// ToolResultEnvelope is the machine-readable lifecycle returned by generic
// tool boundaries. The message remains suitable for model and UI display; the
// status fields let audit code reason about effects without parsing prose.
type ToolResultEnvelope struct {
	Status     string            `json:"status"`
	ErrorType  string            `json:"error_type,omitempty"`
	Completion ai.ToolCompletion `json:"completion,omitempty"`
	Message    string            `json:"message"`
}

const toolResultStatusPendingApproval = "pending_approval"

func rejectedToolResult(errorType, message string) string {
	return encodeToolResult(ToolResultEnvelope{
		Status:    "rejected",
		ErrorType: strings.TrimSpace(errorType),
		Message:   strings.TrimSpace(message),
	})
}

func asynchronousAcceptedResult(message string) string {
	return encodeToolResult(ToolResultEnvelope{
		Status:     "accepted",
		Completion: ai.ToolCompletionAsynchronous,
		Message:    strings.TrimSpace(message),
	})
}

func invalidToolArgumentsResult(err error) string {
	message := "工具参数不符合输入 schema。"
	if err != nil {
		message = strings.TrimSpace(err.Error())
		if runes := []rune(message); len(runes) > 600 {
			message = string(runes[:600]) + "…"
		}
	}
	return rejectedToolResult("invalid_arguments", message)
}

func pendingApprovalResult(message string) string {
	return encodeToolResult(ToolResultEnvelope{
		Status:  toolResultStatusPendingApproval,
		Message: strings.TrimSpace(message),
	})
}

func encodeToolResult(result ToolResultEnvelope) string {
	raw, _ := json.Marshal(result)
	return string(raw)
}

// ParseToolResult decodes lifecycle-aware results. Plain-text results remain
// valid tool output and return ok=false.
func ParseToolResult(result string) (envelope ToolResultEnvelope, ok bool) {
	if json.Unmarshal([]byte(strings.TrimSpace(result)), &envelope) != nil || envelope.Status == "" {
		return ToolResultEnvelope{}, false
	}
	return envelope, true
}

func ToolResultAccepted(result string) bool {
	envelope, ok := ParseToolResult(result)
	return ok && envelope.Status == "accepted" && envelope.Completion == ai.ToolCompletionAsynchronous
}

// ToolResultRejected reports machine-readable boundary rejections.
func ToolResultRejected(result string) bool {
	envelope, ok := ParseToolResult(result)
	return ok && envelope.Status == "rejected"
}

// ToolResultPendingApproval reports a durable two-turn approval boundary.
// Callers classify the structured lifecycle instead of parsing human wording.
func ToolResultPendingApproval(result string) bool {
	envelope, ok := ParseToolResult(result)
	return ok && envelope.Status == toolResultStatusPendingApproval
}
