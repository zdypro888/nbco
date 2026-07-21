package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zdypro888/nbco/ai"
)

func TestApprovalToolContractForbidsPreconfirmation(t *testing.T) {
	wrapped := withApproval(nil, 1, ai.Tool{
		Name: "low_level_db_exec",
		Handler: func(context.Context, json.RawMessage) (string, error) {
			return "executed", nil
		},
	})
	for _, expected := range []string{"不要在首次调用前口头询问确认", "先用最终参数调用一次", "系统只登记待确认动作、不会执行"} {
		if !strings.Contains(wrapped.Description, expected) {
			t.Fatalf("approval tool description is missing %q: %s", expected, wrapped.Description)
		}
	}
}
