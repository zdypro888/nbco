package store

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestDataRowEntityIDPreservesLargeAndNegativeIDs(t *testing.T) {
	row := json.RawMessage(`{"chat_id":-1009876543210123,"title":"项目群"}`)
	id, ok := DataRowEntityID("telegram_groups", row)
	if !ok || id != "-1009876543210123" {
		t.Fatalf("chat_id = %q, %t", id, ok)
	}
}

func TestSemanticDataSourcesOnlyIncludeCuratedTextModels(t *testing.T) {
	sources := SemanticDataSources()
	for _, required := range []string{
		"users", "worker_sessions", "worker_capabilities", "roles", "org_groups",
		"projects", "tasks", "task_updates", "files", "file_intakes",
		"schedules", "deliveries", "learning_candidates", "events", "material_entities",
		"script_tools", "eval_cases",
	} {
		if !slices.Contains(sources, required) {
			t.Errorf("缺少语义数据源 %s", required)
		}
	}
	for _, exactOnly := range []string{"identities", "permissions", "knowledge"} {
		if slices.Contains(sources, exactOnly) {
			t.Errorf("%s 应由专用索引或精确SQL处理", exactOnly)
		}
	}
	if slices.Contains(sources, "chat_messages") {
		t.Fatal("chat_messages 由实时消息管道索引，不应被结构化对账覆盖作用域 payload")
	}
	if slices.Contains(sources, "file_chunks") {
		t.Fatal("file_chunks 由持久文件向量队列索引，不应被通用对账重复覆盖")
	}
	if field, ok := DataSourceIDField("file_chunks"); !ok || field != "chunk_id" {
		t.Fatalf("file_chunks 稳定 ID = %q, %t", field, ok)
	}
	if field, ok := DataSourceIDField("chat_messages"); !ok || field != "chat_message_id" {
		t.Fatalf("chat_messages 稳定 ID = %q, %t", field, ok)
	}
	for _, source := range sources {
		def := dataSourceDefs[source]
		joined := strings.Join(def.semanticFields, ",")
		for _, forbidden := range []string{
			"api_key", "token", "storage_path", "postgres_dsn", "embed_model",
			"engine_session_ref", "workdir", "hostname", "sha256", "tested_source_hash", "source_code",
		} {
			if strings.Contains(joined, forbidden) {
				t.Errorf("数据源 %s 语义字段包含敏感字段 %s", source, forbidden)
			}
		}
	}
	if fields := strings.Join(dataSourceDefs["users"].semanticFields, ","); strings.Contains(fields, "info") {
		t.Fatalf("字段级受限的 users.info 不得进入全局语义索引: %s", fields)
	}
}

func TestSemanticDocumentTextUsesOnlySelectedFields(t *testing.T) {
	text, err := semanticDocumentText(json.RawMessage(`{
		"task_id":7,"title":"天气预报","description":"增加天气查询","secret":"never"
	}`), []string{"title", "description"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "天气预报") || !strings.Contains(text, "增加天气查询") || strings.Contains(text, "never") {
		t.Fatalf("semantic text = %q", text)
	}
}
