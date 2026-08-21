package tools

import (
	"strings"
	"testing"

	"github.com/zdypro888/nbco/workerproto"
)

func TestMaterialLearningUsesWorkerResultContract(t *testing.T) {
	prompt := materialAnalysisPrompt("nbco", "整理公司信息")
	for _, forbidden := range []string{"NBCO_LEARNING_CANDIDATES_JSON", "汇报末尾必须输出"} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("material prompt still contains text protocol %q: %s", forbidden, prompt)
		}
	}
	valid := []byte(`{"knowledge":[],"entities":[],"rules":[],"skills":[],"questions":[]}`)
	if _, err := workerproto.ValidateStructuredResult(true, materialLearningResultSchema(), valid); err != nil {
		t.Fatalf("material result schema rejected valid empty payload: %v", err)
	}
	invalid := []byte(`{"knowledge":[]}`)
	if _, err := workerproto.ValidateStructuredResult(true, materialLearningResultSchema(), invalid); err == nil {
		t.Fatal("material result schema accepted incomplete payload")
	}
}
