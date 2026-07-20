package taskflow

import "testing"

func TestNormalizeCompletionPolicy(t *testing.T) {
	tests := []struct {
		input string
		want  CompletionPolicy
		ok    bool
	}{
		{"", CompletionSelfAcceptOnSuccess, true},
		{" auto_accept_on_success ", CompletionAutoAcceptOnSuccess, true},
		{"review_required", CompletionReviewRequired, true},
		{"executor:command", "executor:command", false},
	}
	for _, tt := range tests {
		got, ok := NormalizeCompletionPolicy(tt.input)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("NormalizeCompletionPolicy(%q) = %q, %v; want %q, %v", tt.input, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseExecutionOutcome(t *testing.T) {
	for _, value := range []string{string(ExecutionSucceeded), string(ExecutionFailed)} {
		got, ok := ParseExecutionOutcome(value)
		if !ok || string(got) != value {
			t.Fatalf("ParseExecutionOutcome(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := ParseExecutionOutcome("command_exit_0"); ok {
		t.Fatal("executor-specific outcome must be rejected")
	}
}
