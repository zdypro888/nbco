package store

import "testing"

func TestNormalizeTaskKind(t *testing.T) {
	for _, kind := range []string{
		TaskKindEngineering, TaskKindMaterials, TaskKindReview, TaskKindResearch,
		TaskKindOperations, TaskKindProductDesign, TaskKindGeneral,
	} {
		if got := NormalizeTaskKind(kind); got != kind {
			t.Fatalf("NormalizeTaskKind(%q) = %q", kind, got)
		}
	}
	if got := NormalizeTaskKind("unknown"); got != TaskKindGeneral {
		t.Fatalf("unknown kind = %q", got)
	}
}

func TestTaskOutcomeStatsRate(t *testing.T) {
	if got := (TaskOutcomeStats{}).PassRate(); got != 0 {
		t.Fatalf("empty PassRate = %v", got)
	}
	st := TaskOutcomeStats{Accepted: 3, Rejected: 1}
	if got := st.Total(); got != 4 {
		t.Fatalf("Total = %d", got)
	}
	if got := st.PassRate(); got != 0.75 {
		t.Fatalf("PassRate = %v", got)
	}
}
