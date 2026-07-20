// Package workerproto defines the execution contract shared by nbco and its
// worker binary. These values describe mechanical execution only; business
// task review and acceptance remain owned by the task domain.
package workerproto

import "strings"

type Executor string

const (
	ExecutorAgent   Executor = "agent"
	ExecutorCommand Executor = "command"
)

func ParseExecutor(value string) (Executor, bool) {
	executor := Executor(strings.TrimSpace(value))
	if !executor.Valid() {
		return "", false
	}
	return executor, true
}

func (e Executor) Valid() bool {
	return e == ExecutorAgent || e == ExecutorCommand
}

// EvidenceScope states what a successful executor outcome can prove. It is
// deliberately separate from the business task lifecycle: a process can exit
// cleanly and an agent can submit a report without independently proving that
// the requester's objective was satisfied.
type EvidenceScope string

const (
	EvidenceUnknown          EvidenceScope = "unknown"
	EvidenceProcessExecution EvidenceScope = "process_execution"
	EvidenceAgentSubmission  EvidenceScope = "agent_submission"
)

func (e Executor) EvidenceScope() EvidenceScope {
	switch e {
	case ExecutorCommand:
		return EvidenceProcessExecution
	case ExecutorAgent:
		return EvidenceAgentSubmission
	default:
		return EvidenceUnknown
	}
}

type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
)

func ParseOutcome(value string) (Outcome, bool) {
	outcome := Outcome(strings.TrimSpace(value))
	if !outcome.Valid() {
		return "", false
	}
	return outcome, true
}

func (o Outcome) Valid() bool {
	return o == OutcomeSucceeded || o == OutcomeFailed
}
