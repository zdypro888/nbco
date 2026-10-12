package tools

import (
	"context"
	"github.com/zdypro888/nbco/store"
	"testing"
)

func TestBuiltInApprovalRegistryIsPublishedToNativeRuntime(t *testing.T) {
	user := &store.User{ID: 1, Status: store.UserActive, IsSuperadmin: true}
	seen := 0
	for _, tool := range ForUserContext(context.Background(), Deps{}, user, nil) {
		if approvalRequired[tool.Name] {
			seen++
			if !tool.ApprovalRequired {
				t.Fatalf("native runtime cannot gate %s", tool.Name)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no built-in approval metadata checked")
	}
}
