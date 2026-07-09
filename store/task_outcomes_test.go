package store

import "testing"

func TestInferTaskKind(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "engineering", text: "升级 nbco，增加固定 tg 命令并部署", want: "engineering"},
		{name: "materials", text: "这两个 xlsx 是员工资料，帮我整理入库", want: "materials"},
		{name: "review", text: "审核当前项目所有代码并跑回归测试", want: "engineering"},
		{name: "research", text: "调研 hermes-agent 是怎么自我学习的", want: "research"},
		{name: "operations", text: "明天早上提醒全员完善个人档案", want: "operations"},
		{name: "design", text: "设计一个 Telegram mini app 控制中心页面", want: "product_design"},
		{name: "general", text: "看看这个问题", want: "general"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InferTaskKind(tt.text); got != tt.want {
				t.Fatalf("InferTaskKind(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
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
