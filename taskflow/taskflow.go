// Package taskflow defines the execution-neutral contract shared by task
// creators, workers, gateways, and the task state machine.
package taskflow

import "strings"

// CompletionPolicy declares how a successful submission leaves the review
// stage. Explicit reviewers always take precedence over these policies.
type CompletionPolicy string

const (
	CompletionReviewRequired      CompletionPolicy = "review_required"
	CompletionAutoAcceptOnSuccess CompletionPolicy = "auto_accept_on_success"
	CompletionSelfAcceptOnSuccess CompletionPolicy = "self_accept_on_success"
)

// NormalizeCompletionPolicy validates a stored or requested policy. Empty
// values use the ordinary task policy, which preserves self-assigned tasks
// without granting automatic acceptance to delegated work.
func NormalizeCompletionPolicy(value string) (CompletionPolicy, bool) {
	policy := CompletionPolicy(strings.TrimSpace(value))
	if policy == "" {
		policy = CompletionSelfAcceptOnSuccess
	}
	return policy, policy.Valid()
}

func (p CompletionPolicy) Valid() bool {
	switch p {
	case CompletionReviewRequired, CompletionAutoAcceptOnSuccess, CompletionSelfAcceptOnSuccess:
		return true
	default:
		return false
	}
}

// ExecutionOutcome is the executor's structured result. Infrastructure
// failures use the separate retry/fail path and do not submit an outcome.
type ExecutionOutcome string

const (
	ExecutionSucceeded ExecutionOutcome = "succeeded"
	ExecutionFailed    ExecutionOutcome = "failed"
)

func ParseExecutionOutcome(value string) (ExecutionOutcome, bool) {
	outcome := ExecutionOutcome(strings.TrimSpace(value))
	if !outcome.Valid() {
		return "", false
	}
	return outcome, true
}

func (o ExecutionOutcome) Valid() bool {
	return o == ExecutionSucceeded || o == ExecutionFailed
}
