package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

func TestSearchContextKeepsExactGroupBoundary(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	user := mkToolsUser(t, s, "context-user", false)
	current := "telegram:group:-10001"
	other := "telegram:group:-10002"
	sourceAt := time.Date(2024, 3, 5, 6, 7, 0, 0, time.UTC)
	for channel, content := range map[string]string{
		current: "【Alice】北极星发布风险已经解除",
		other:   "【Bob】北极星发布仍有严重阻塞",
	} {
		session, err := s.StartGroupSession(ctx, user.ID, channel, "test")
		if err != nil {
			t.Fatal(err)
		}
		envelope := store.MessageEnvelope{}
		if channel == current {
			envelope = store.MessageEnvelope{Provider: "telegram", ExternalChatRef: "-10001", ExternalMessageRef: "41", SourceCreatedAt: &sourceAt, ActorDisplayName: "Alice"}
		}
		if _, err := s.AppendMessageWithEnvelope(ctx, session.ID, "user", content, envelope); err != nil {
			t.Fatal(err)
		}
	}
	tool := contextTools(Deps{Store: s, TZ: time.UTC}, user)[0]
	out, err := tool.Handler(WithInteractionChannel(ctx, current), json.RawMessage(`{"query":"北极星发布"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "风险已经解除") || !strings.Contains(out, "2024-03-05 06:07") || strings.Contains(out, "严重阻塞") {
		t.Fatalf("group context crossed channel boundary: %s", out)
	}
}

func TestSearchContextRechecksPrivateRowVisibility(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	user := mkToolsUser(t, s, "private-context-user", false)
	other := mkToolsUser(t, s, "private-context-other", false)
	for actor, content := range map[*store.User]string{
		user:  "海蓝计划由当前用户负责",
		other: "海蓝计划的隐藏预算是一百万元",
	} {
		session, err := s.StartSession(ctx, actor.ID, "api", "test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendMessage(ctx, session.ID, "user", content); err != nil {
			t.Fatal(err)
		}
	}
	tool := contextTools(Deps{Store: s, TZ: time.UTC}, user)[0]
	out, err := tool.Handler(WithInteractionChannel(ctx, "api"), json.RawMessage(`{"query":"海蓝计划"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "当前用户负责") || strings.Contains(out, "隐藏预算") {
		t.Fatalf("private context crossed user boundary: %s", out)
	}
}

func TestRecordFactRequiresCurrentUserEvidenceAndIsIdempotent(t *testing.T) {
	s := openToolsTestStore(t)
	ctx := context.Background()
	user := mkToolsUser(t, s, "fact-user", false)
	session, err := s.StartSession(ctx, user.ID, "api", "test")
	if err != nil {
		t.Fatal(err)
	}
	sourceAt := time.Date(2025, 2, 3, 4, 5, 0, 0, time.UTC)
	messageID, err := s.AppendMessageWithEnvelope(ctx, session.ID, "user", "今天继续完成品牌迁移，阻塞已经解除",
		store.MessageEnvelope{Provider: "telegram", ExternalChatRef: "fact-chat", ExternalMessageRef: "42", SourceCreatedAt: &sourceAt})
	if err != nil {
		t.Fatal(err)
	}
	handler := workFactTools(Deps{Store: s, TZ: time.UTC}, user)[0].Handler
	turnCtx := WithInteractionChannel(WithApprovalTurn(ctx, session.ID, messageID), "api")
	args := json.RawMessage(`{"kind":"update","title":"品牌迁移进展","evidence":"今天继续完成品牌迁移，阻塞已经解除"}`)
	first, err := handler(turnCtx, args)
	if err != nil || !strings.Contains(first, "工作事实已记录") {
		t.Fatalf("first record = %q err=%v", first, err)
	}
	second, err := handler(turnCtx, json.RawMessage(`{"kind":"risk","title":"迁移状态","evidence":"今天继续完成品牌迁移，阻塞已经解除"}`))
	if err != nil || !strings.Contains(second, "工作事实已记录") {
		t.Fatalf("idempotent record = %q err=%v", second, err)
	}
	rows, err := s.ReadData(ctx, user.ID, false, store.DataReadQuery{
		Source: "work_evidence", Filters: map[string]string{"source_type": store.WorkEvidenceSourceConversationFact}, Limit: 10,
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("facts = %d err=%v rows=%s", len(rows), err, rows)
	}
	rejected, err := handler(turnCtx, json.RawMessage(`{"kind":"risk","title":"虚构风险","evidence":"用户并没有说过这句话"}`))
	if err != nil || !strings.Contains(rejected, "必须逐字来自") {
		t.Fatalf("ungrounded evidence = %q err=%v", rejected, err)
	}
	other := mkToolsUser(t, s, "fact-other", false)
	otherMessageID, err := s.AppendMessageWithEnvelope(ctx, session.ID, "user", "其他人的进展不能冒充当前用户证据",
		store.MessageEnvelope{Provider: "telegram", ExternalChatRef: "fact-chat", ExternalMessageRef: "foreign", ActorUserID: &other.ID})
	if err != nil {
		t.Fatal(err)
	}
	foreignCtx := WithInteractionChannel(WithApprovalTurn(ctx, session.ID, otherMessageID), "api")
	foreign, err := handler(foreignCtx, json.RawMessage(`{"kind":"update","title":"越权事实","evidence":"其他人的进展不能冒充当前用户证据"}`))
	if err != nil || !strings.Contains(foreign, "不属于调用者") {
		t.Fatalf("foreign evidence = %q err=%v", foreign, err)
	}
	firstProject, err := s.CreateProject(ctx, "事实所属项目", "", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherProject, err := s.CreateProject(ctx, "错误项目", "", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.CreateTask(ctx, &store.Task{
		ProjectID: firstProject.ID, AssignerID: user.ID, AssigneeID: user.ID,
		Title: "品牌迁移", Description: "完成迁移",
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := handler(turnCtx, json.RawMessage(fmt.Sprintf(
		`{"kind":"update","title":"品牌迁移进展","evidence":"今天继续完成品牌迁移，阻塞已经解除","task_id":%d,"project_id":%d}`,
		task.ID, otherProject.ID)))
	if err != nil || !strings.Contains(mismatch, "不属于") {
		t.Fatalf("mismatched links = %q err=%v", mismatch, err)
	}
	linked, err := handler(turnCtx, json.RawMessage(fmt.Sprintf(
		`{"kind":"update","title":"品牌迁移进展","evidence":"今天继续完成品牌迁移，阻塞已经解除","task_id":%d}`,
		task.ID)))
	if err != nil || !strings.Contains(linked, "工作事实已记录") {
		t.Fatalf("task-linked fact = %q err=%v", linked, err)
	}
	rows, err = s.ReadData(ctx, user.ID, false, store.DataReadQuery{
		Source: "work_evidence", Filters: map[string]string{"source_type": store.WorkEvidenceSourceConversationFact}, Limit: 10,
	})
	var stored map[string]any
	if err == nil && len(rows) == 1 {
		err = json.Unmarshal(rows[0], &stored)
	}
	projectID, projectOK := stored["project_id"].(float64)
	taskID, taskOK := stored["task_id"].(float64)
	eventAt, eventOK := stored["event_at"].(string)
	if err != nil || len(rows) != 1 || !projectOK || !taskOK || int64(projectID) != firstProject.ID || int64(taskID) != task.ID {
		t.Fatalf("linked fact did not inherit task project: rows=%s err=%v", rows, err)
	}
	parsedEventAt, parseErr := time.Parse(time.RFC3339Nano, eventAt)
	if !eventOK || parseErr != nil || !parsedEventAt.Equal(sourceAt) {
		t.Fatalf("fact dropped source event time: %#v", stored["event_at"])
	}
}
