package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/textfmt"
)

func workFactTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("record_fact", "记录当前用户明确提供的工作事实，不要求它已经属于正式任务。适用于进展、决策、风险、产物或摘要；evidence 必须逐字来自当前用户消息。无法确定任务/项目关联时保持未关联，不能为补齐关联而臆造对象。",
			obj(map[string]any{
				"kind":       enumP("事实类型", store.WorkEvidenceUpdate, store.WorkEvidenceDecision, store.WorkEvidenceRisk, store.WorkEvidenceDeliverable, store.WorkEvidenceSummary),
				"title":      p("string", "简短、可检索的事实标题"),
				"evidence":   p("string", "当前用户消息中的逐字事实原文，不得改写"),
				"task_id":    p("integer", "可选；当前用户可见的正式任务ID"),
				"project_id": p("integer", "可选；当前用户可见的项目ID"),
			}, "kind", "title", "evidence"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				if d.Store == nil || u == nil {
					return "当前入口没有可用的事实存储。", nil
				}
				var args struct {
					Kind      string `json:"kind"`
					Title     string `json:"title"`
					Evidence  string `json:"evidence"`
					TaskID    int64  `json:"task_id"`
					ProjectID int64  `json:"project_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				args.Kind = strings.TrimSpace(args.Kind)
				args.Title = strings.TrimSpace(args.Title)
				args.Evidence = strings.TrimSpace(args.Evidence)
				if !validWorkFactKind(args.Kind) {
					return "kind 必须是 update、decision、risk、deliverable 或 summary。", nil
				}
				if args.Title == "" || args.Evidence == "" {
					return "title 和 evidence 不能为空。", nil
				}
				if textfmt.RedactSecrets(args.Title) != args.Title || textfmt.RedactSecrets(args.Evidence) != args.Evidence {
					return "事实内容包含凭据或密钥，不能写入工作事实。", nil
				}
				if args.TaskID < 0 || args.ProjectID < 0 {
					return "task_id 和 project_id 必须是正整数。", nil
				}
				var linkedTask *store.Task
				if args.TaskID > 0 {
					visible, err := visibleDataEntity(ctx, d, u, "tasks", "task_id", args.TaskID)
					if err != nil {
						return "", err
					}
					if !visible {
						return "任务不存在或当前用户无权关联。可以不填 task_id，先把事实保存为未关联记录。", nil
					}
					linkedTask, err = d.Store.TaskByID(ctx, args.TaskID)
					if err != nil {
						return "任务不存在或当前用户无权关联。", nil
					}
				}
				if args.ProjectID > 0 {
					visible, err := visibleDataEntity(ctx, d, u, "projects", "project_id", args.ProjectID)
					if err != nil {
						return "", err
					}
					if !visible {
						return "项目不存在或当前用户无权关联。可以不填 project_id，先把事实保存为未关联记录。", nil
					}
				}
				if linkedTask != nil {
					if args.ProjectID > 0 && linkedTask.ProjectID != args.ProjectID {
						return "task_id 不属于指定 project_id，请修正关联或只保留 task_id。", nil
					}
					if args.ProjectID == 0 {
						args.ProjectID = linkedTask.ProjectID
					}
				}

				messageID := sourceMessageID(ctx)
				var sourceID int64
				if messageID != nil {
					sourceID = *messageID
				}
				if sourceID <= 0 {
					return "record_fact 需要当前对话消息作为证据；此入口没有可验证的用户消息。", nil
				}
				sourceMessage, err := d.Store.ChatMessageByID(ctx, sourceID)
				if err != nil {
					return "", err
				}
				turn, ok := approvalTurnFromContext(ctx)
				if !ok || turn.MessageID != sourceMessage.ID || turn.SessionID != sourceMessage.SessionID {
					return "record_fact 只能引用当前对话轮次的用户消息。", nil
				}
				session, err := d.Store.SessionByID(ctx, sourceMessage.SessionID)
				if err != nil {
					return "", err
				}
				channel := interactionChannel(ctx)
				if session.Channel != channel || (!store.IsGroupChannel(channel) && session.UserID != u.ID) ||
					(sourceMessage.ActorUserID != nil && *sourceMessage.ActorUserID != u.ID) {
					return "当前消息不属于调用者或当前会话，不能作为事实证据。", nil
				}
				if sourceMessage.Role != string(ai.RoleUser) || !containsNormalizedEvidence(sourceMessage.Content, args.Evidence, 4) {
					return "evidence 必须逐字来自当前用户消息，且包含实质事实内容。", nil
				}
				actorID := u.ID
				metadata, _ := json.Marshal(map[string]any{"channel": channel, "origin": "agent_tool", "evidence": args.Evidence})
				input := store.WorkEvidenceInput{
					SourceType: store.WorkEvidenceSourceConversationFact,
					SourceKey:  store.ConversationFactSourceKey(sourceID, actorID, args.Evidence),
					Kind:       args.Kind, Status: store.WorkEvidenceObserved,
					Title: textfmt.TruncateRunes(args.Title, 200), Content: textfmt.TruncateRunes(args.Evidence, 4000),
					ActorUserID: &actorID, SourceMessageID: messageID, Confidence: 1,
					EventAt: sourceMessage.EventAt().UTC(), Metadata: metadata, CreatedBy: &actorID,
				}
				if args.TaskID > 0 {
					input.TaskID = &args.TaskID
				}
				if args.ProjectID > 0 {
					input.ProjectID = &args.ProjectID
				}
				evidence, err := d.Store.UpsertWorkEvidence(ctx, input)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("工作事实已记录：evidence_id=%d，kind=%s，status=%s。", evidence.ID, evidence.Kind, evidence.Status), nil
			}),
	}
}

func validWorkFactKind(kind string) bool {
	switch kind {
	case store.WorkEvidenceUpdate, store.WorkEvidenceDecision, store.WorkEvidenceRisk,
		store.WorkEvidenceDeliverable, store.WorkEvidenceSummary:
		return true
	default:
		return false
	}
}

func visibleDataEntity(ctx context.Context, d Deps, u *store.User, source, field string, id int64) (bool, error) {
	rows, err := d.Store.ReadData(ctx, u.ID, u.IsSuperadmin, store.DataReadQuery{
		Source:  source,
		Filters: map[string]string{field: strconv.FormatInt(id, 10)},
		Limit:   1,
	})
	return len(rows) > 0, err
}

func containsNormalizedEvidence(source, evidence string, minRunes int) bool {
	source = strings.Join(strings.Fields(source), " ")
	evidence = strings.Join(strings.Fields(evidence), " ")
	return len([]rune(evidence)) >= minRunes && strings.Contains(source, evidence)
}
