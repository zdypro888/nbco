package store

import "testing"

func TestLearningTextSimilarity(t *testing.T) {
	similar := LearningTextSimilarity(
		"Worker Token 不外发", "不要把 worker token 发给用户或发到群里。",
		"禁止外发 Worker Token", "worker token 不得发送给用户，也不能出现在群消息中。",
	)
	unrelated := LearningTextSimilarity(
		"Worker Token 不外发", "不要把 worker token 发给用户或发到群里。",
		"每周财务对账", "每周五核对发票与银行流水。",
	)
	if similar <= unrelated || similar < LearningDuplicateThreshold {
		t.Fatalf("similar=%.3f unrelated=%.3f", similar, unrelated)
	}
	if unrelated != 0 {
		t.Fatalf("unrelated similarity = %.3f", unrelated)
	}
}

func TestLearningConflictIsGenericAndNotDuplicate(t *testing.T) {
	if !LearningTextsConflict(LearningKindRule,
		"推理过程展示", "以后不要展示模型推理过程。",
		"推理过程展示", "以后默认开启模型推理过程。") {
		t.Fatal("same subject with opposite direction must be treated as a conflict")
	}
	if LearningTextsConflict(LearningKindKnowledge,
		"推理过程展示", "不要展示模型推理过程。",
		"推理过程展示", "默认开启模型推理过程。") {
		t.Fatal("plain knowledge facts do not use policy polarity governance")
	}
}
