package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"

	"github.com/zdypro888/nbco/semantic"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/vectorstore"
)

func TestLexicalRowsAcrossSourcesDoesNotTurnDatabaseFailureIntoEmptyResult(t *testing.T) {
	s := openToolsTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := lexicalRowsAcrossSources(ctx, Deps{Store: s}, &store.User{ID: 1, IsSuperadmin: true},
		[]string{"任务"}, []string{"tasks", "projects"}, 10, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestLexicalRowsAcrossSourcesPreservesPartialSuccess(t *testing.T) {
	s := openToolsTestStore(t)
	rows, err := lexicalRowsAcrossSources(context.Background(), Deps{Store: s}, &store.User{ID: 1, IsSuperadmin: true},
		[]string{"不存在的检索词"}, []string{"tasks", "not_a_source"}, 10, false)
	if err != nil {
		t.Fatalf("one unavailable source must not discard successful sources: %v", err)
	}
	if rows == nil {
		t.Fatal("successful empty result should remain distinguishable from total query failure")
	}
}

func TestDataSemanticFilterAppliesCoarsePrivateScopes(t *testing.T) {
	user := &store.User{ID: 42}
	chat := dataSemanticFilter(semantic.SourceChatMessage, user)
	if chat.Must[vectorstore.PayloadSessionUser] != int64(42) ||
		chat.Must[vectorstore.PayloadConversationScope] != "private" {
		t.Fatalf("chat filter = %+v", chat)
	}
	knowledge := dataSemanticFilter(semantic.SourceKnowledge, user)
	if knowledge.MustNot[vectorstore.PayloadKind] != store.KnowledgeKindPolicy {
		t.Fatalf("knowledge filter = %+v", knowledge)
	}
	admin := dataSemanticFilter(semantic.SourceChatMessage, &store.User{ID: 1, IsSuperadmin: true})
	if len(admin.Must) != 1 || len(admin.MustNot) != 0 {
		t.Fatalf("admin filter = %+v", admin)
	}
}

func TestMergeRankedDataRowsUsesRRF(t *testing.T) {
	row := func(id int) json.RawMessage {
		return json.RawMessage([]byte(`{"task_id":` + strconv.Itoa(id) + `}`))
	}
	primary := []json.RawMessage{row(1), row(2)}
	secondary := []json.RawMessage{row(2), row(3)}
	out := mergeRankedDataRows("tasks", primary, secondary, 3)
	if len(out) != 3 || string(out[0]) != string(row(2)) {
		t.Fatalf("RRF 应优先双路命中: %s", out)
	}
}

func TestInterleaveLexicalRowsDoesNotStarveLaterSources(t *testing.T) {
	sources := []string{"tasks", "projects", "files"}
	rows := map[string][]json.RawMessage{
		"tasks":    {json.RawMessage(`{"task_id":1}`), json.RawMessage(`{"task_id":2}`)},
		"projects": {json.RawMessage(`{"project_id":1}`)},
		"files":    {json.RawMessage(`{"file_id":1}`)},
	}
	out := interleaveLexicalRows(sources, rows, 3)
	if len(out) != 3 || out[0].Source != "tasks" || out[1].Source != "projects" || out[2].Source != "files" {
		t.Fatalf("interleaved rows = %+v", out)
	}
}

func TestMergeCrossRankedDataRowsUsesSourceAndID(t *testing.T) {
	task1 := rankedDataRow{Source: "tasks", Row: json.RawMessage(`{"task_id":1}`)}
	task2 := rankedDataRow{Source: "tasks", Row: json.RawMessage(`{"task_id":2}`)}
	project1 := rankedDataRow{Source: "projects", Row: json.RawMessage(`{"project_id":1}`)}
	out := mergeCrossRankedDataRows(
		[]rankedDataRow{task1, task2},
		[]rankedDataRow{task2, project1}, 3,
	)
	if len(out) != 3 || string(out[0].Row) != string(task2.Row) || out[0].Source != "tasks" {
		t.Fatalf("跨源 RRF 应按 source+稳定ID 去重并优先双路命中: %+v", out)
	}
}

func TestMergeCrossRankedDataRowsPreservesEqualTextAcrossDistinctFacts(t *testing.T) {
	fact := "已通知全体员工完善手机号职位和组别资料信息"
	chat := rankedDataRow{Source: "chat_messages", Row: json.RawMessage(`{"chat_message_id":1,"content":"` + fact + `"}`)}
	action := rankedDataRow{Source: "action_turns", Row: json.RawMessage(`{"turn_id":2,"reply":"` + fact + `"}`)}
	out := mergeCrossRankedDataRows([]rankedDataRow{chat}, []rankedDataRow{action}, 10)
	if len(out) != 2 {
		t.Fatalf("equal prose does not prove equal business identity: %+v", out)
	}
}

func TestMergeCrossRankedDataRowsCollapsesDeclaredProjection(t *testing.T) {
	chat := rankedDataRow{Source: "chat_messages", Row: json.RawMessage(`{"chat_message_id":1,"content":"项目有风险"}`)}
	projection := rankedDataRow{Source: "work_evidence", Row: json.RawMessage(`{"evidence_id":9,"kind":"communication","source_message_id":1,"content":"项目有风险"}`)}
	derived := rankedDataRow{Source: "work_evidence", Row: json.RawMessage(`{"evidence_id":10,"kind":"risk","source_message_id":1,"content":"项目有风险"}`)}
	out := mergeCrossRankedDataRows([]rankedDataRow{chat}, []rankedDataRow{projection, derived}, 10)
	if len(out) != 2 {
		t.Fatalf("only the declared raw-message projection should collapse: %+v", out)
	}
	if out[0].Source != "chat_messages" {
		t.Fatalf("two retrieval paths should reinforce the canonical chat row: %+v", out)
	}
	out = mergeCrossRankedDataRows(nil, []rankedDataRow{projection, chat, derived}, 10)
	if len(out) != 2 || out[0].Source != "chat_messages" {
		t.Fatalf("canonical source must win even when its projection is retrieved first: %+v", out)
	}
}

func TestMergeCrossRankedDataRowsPreservesSourceDiversity(t *testing.T) {
	var tasks []rankedDataRow
	for id := 1; id <= 5; id++ {
		tasks = append(tasks, rankedDataRow{Source: "tasks", Row: json.RawMessage(`{"task_id":` + strconv.Itoa(id) + `}`)})
	}
	project := rankedDataRow{Source: "projects", Row: json.RawMessage(`{"project_id":9}`)}
	out := mergeCrossRankedDataRows(append(tasks, project), nil, 3)
	if len(out) != 3 || out[2].Source != "projects" {
		t.Fatalf("source diversity = %+v", out)
	}
}
