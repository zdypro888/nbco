package workerproto

import "testing"

func TestExecutionContractValidation(t *testing.T) {
	for _, value := range []string{string(ExecutorAgent), string(ExecutorCommand)} {
		if got, ok := ParseExecutor(value); !ok || string(got) != value {
			t.Fatalf("ParseExecutor(%q) = %q, %v", value, got, ok)
		}
	}
	if _, ok := ParseExecutor("task"); ok {
		t.Fatal("business task kind must not be accepted as an executor")
	}
	for _, value := range []string{string(OutcomeSucceeded), string(OutcomeFailed)} {
		if got, ok := ParseOutcome(value); !ok || string(got) != value {
			t.Fatalf("ParseOutcome(%q) = %q, %v", value, got, ok)
		}
	}
}
