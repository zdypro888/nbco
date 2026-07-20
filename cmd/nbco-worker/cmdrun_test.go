package main

import (
	"strings"
	"testing"
)

func TestCommandSummaryPreservesResultSemanticsAndOutputPriority(t *testing.T) {
	summary := commandSummary("curl https://example.invalid", "pipe", commandResult{
		Output:   `{"status":401,"error":"unauthorized"}`,
		ExitCode: 0,
	}, nil)

	for _, want := range []string{"命令进程已结束", "退出码只描述命令进程", "401", "curl https://example.invalid"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
	if strings.Index(summary, "输出：") > strings.Index(summary, "命令：") {
		t.Fatalf("output must precede command so bounded notifications keep result evidence: %s", summary)
	}
}
