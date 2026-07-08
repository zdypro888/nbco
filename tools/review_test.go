package tools

import (
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestBuildReviewBrief(t *testing.T) {
	task := &store.Task{ID: 12, Title: "写登录页", Goal: "让用户能登录",
		Description: "实现表单", Acceptance: "能提交且有错误态"}
	progress := []store.Progress{
		{Content: "开工了", CreatedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)},
		{Content: "🤖 完成汇报：做完了", CreatedAt: time.Date(2026, 7, 4, 11, 0, 0, 0, time.UTC)},
	}
	brief := buildReviewBrief(task, "小码", progress, "重点看安全", time.UTC)
	for _, want := range []string{
		"任务内部编号 12", "写登录页", "让用户能登录", "实现表单", "能提交且有错误态",
		"小码", "重点看安全", "完成汇报：做完了", "nbco-work/task-12",
		"建议通过", "建议打回", "不要轻信汇报文字",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("简报缺 %q", want)
		}
	}
}

func TestBuildReviewBriefCapsProgress(t *testing.T) {
	task := &store.Task{ID: 1, Title: "T"}
	var progress []store.Progress
	for i := 0; i < reviewBriefMaxProgress+10; i++ {
		progress = append(progress, store.Progress{Content: strings.Repeat("x", 5), CreatedAt: time.Now()})
	}
	progress[len(progress)-1].Content = "最后一条"
	progress[0].Content = "第一条"
	brief := buildReviewBrief(task, "谁", progress, "", time.UTC)
	if !strings.Contains(brief, "最后一条") || strings.Contains(brief, "第一条") {
		t.Error("应保留最近的记录、截断最早的记录")
	}
	if !strings.Contains(brief, "仅列最近") {
		t.Error("截断时应注明")
	}
}
