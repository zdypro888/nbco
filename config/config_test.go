package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nbco.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"telegram_token": "tok",
		"superadmins": [1],
		"postgres_dsn": "postgres://x",
		"mcp_servers": [{"name":"ops","url":"https://ops.example/mcp"}],
		"ai": {"api_key": "k", "model": "m"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8900" {
		t.Errorf("Listen 默认值 = %q", cfg.Listen)
	}
	if cfg.Timezone != "Asia/Shanghai" {
		t.Errorf("Timezone 默认值 = %q", cfg.Timezone)
	}
	if cfg.DailySummaryHour == nil || *cfg.DailySummaryHour != 9 {
		t.Errorf("DailySummaryHour 默认值 = %v", cfg.DailySummaryHour)
	}
	if cfg.AI.Engine != EngineEino || cfg.AI.Provider != ProviderClaude {
		t.Errorf("引擎默认值 = %q/%q", cfg.AI.Engine, cfg.AI.Provider)
	}
	if cfg.AI.MaxTokens != 4096 || cfg.AI.MaxTurns != 64 || cfg.AI.TimeoutMS != 300000 || cfg.AI.TurnTimeoutMS != 600000 {
		t.Errorf("MaxTokens/MaxTurns/TimeoutMS/TurnTimeoutMS 默认值 = %d/%d/%d/%d",
			cfg.AI.MaxTokens, cfg.AI.MaxTurns, cfg.AI.TimeoutMS, cfg.AI.TurnTimeoutMS)
	}
	if cfg.AI.SummarizeAfterTokens != 24000 || cfg.AI.SummarizeAfterMessages != 80 {
		t.Errorf("Eino 摘要阈值默认值 = %d tokens/%d messages",
			cfg.AI.SummarizeAfterTokens, cfg.AI.SummarizeAfterMessages)
	}
	if cfg.AI.MaxCompletionTokens != 0 || cfg.AI.ReasoningEffort != "" {
		t.Errorf("reasoning 默认配置 = max_completion_tokens:%d reasoning_effort:%q", cfg.AI.MaxCompletionTokens, cfg.AI.ReasoningEffort)
	}
	if cfg.AI.StreamReasoning {
		t.Error("stream_reasoning 默认不应展示推理过程")
	}
	if cfg.FileStorePath != "files" {
		t.Errorf("FileStorePath 默认值 = %q", cfg.FileStorePath)
	}
	if cfg.WorkerDownloadPath != "downloads" {
		t.Errorf("WorkerDownloadPath 默认值 = %q", cfg.WorkerDownloadPath)
	}
	if got := cfg.MCPServers[0].RequiredAction; got != "superadmin" {
		t.Errorf("MCP required_action 默认值 = %q", got)
	}
	if cfg.Qdrant.Enabled() {
		t.Error("未配置 qdrant.url 时不应启用 Qdrant")
	}
}

func TestLoadQdrantConfig(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"postgres_dsn":"postgres://x",
		"qdrant":{"url":"http://127.0.0.1:6334/"},
		"ai":{"provider":"openai","api_key":"k","base_url":"https://ai.example/v1","model":"m","embed_model":"bge-m3"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Qdrant.URL != "http://127.0.0.1:6334" || cfg.Qdrant.CollectionPrefix != "nbco_semantic" ||
		cfg.Qdrant.SyncIntervalSeconds != 120 || cfg.Qdrant.SyncTimeoutSeconds != 3600 {
		t.Fatalf("Qdrant 默认配置异常: %+v", cfg.Qdrant)
	}
}

func TestLoadDailySummaryOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"telegram_token": "tok",
		"superadmins": [1],
		"postgres_dsn": "postgres://x",
		"daily_summary_hour": -1,
		"ai": {"api_key": "k", "model": "m"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if *cfg.DailySummaryHour != -1 {
		t.Errorf("显式 -1 不应被默认值覆盖，got %d", *cfg.DailySummaryHour)
	}
}

func TestLoadReasoningConfig(t *testing.T) {
	for _, effort := range []string{"none", "LOW", "medium", "high", "xhigh", "max"} {
		cfg, err := Load(writeConfig(t, `{
			"telegram_token": "tok",
			"superadmins": [1],
			"postgres_dsn": "postgres://x",
			"ai": {"provider":"openai","api_key": "k", "model": "m", "max_completion_tokens": 8192, "reasoning_effort": "`+effort+`"}
		}`))
		if err != nil {
			t.Fatalf("reasoning_effort=%q: %v", effort, err)
		}
		if cfg.AI.MaxCompletionTokens != 8192 || cfg.AI.ReasoningEffort != strings.ToLower(effort) {
			t.Fatalf("reasoning config = max_completion_tokens:%d reasoning_effort:%q", cfg.AI.MaxCompletionTokens, cfg.AI.ReasoningEffort)
		}
	}
}

func TestLoadValidation(t *testing.T) {
	cases := []struct {
		name, body, wantErr string
	}{
		{"缺 postgres_dsn",
			`{"telegram_token":"t","superadmins":[1],"ai":{"api_key":"k","model":"m"}}`,
			"postgres_dsn"},
		{"eino 缺 api_key",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"model":"m"}}`,
			"ai.api_key"},
		{"eino 缺 model",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"api_key":"k"}}`,
			"ai.model"},
		{"eino max_turns 过小",
			`{"postgres_dsn":"d","ai":{"api_key":"k","model":"m","max_turns":1}}`,
			"ai.max_turns"},
		{"turn timeout 过大",
			`{"postgres_dsn":"d","ai":{"api_key":"k","model":"m","turn_timeout_ms":1800001}}`,
			"ai.turn_timeout_ms"},
		{"未知引擎",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"engine":"gpt"}}`,
			"ai.engine 不支持"},
		{"未知 provider",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"provider":"gemini","api_key":"k","model":"m"}}`,
			"ai.provider 不支持"},
		{"未知 reasoning_effort",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"provider":"openai","api_key":"k","model":"m","reasoning_effort":"extreme"}}`,
			"ai.reasoning_effort"},
		{"mcp server 缺 url",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","mcp_servers":[{"name":"x"}],"ai":{"api_key":"k","model":"m"}}`,
			"mcp_servers[0]"},
		{"mcp server 名称重复",
			`{"postgres_dsn":"d","mcp_servers":[{"name":"ops","url":"https://a.example/mcp"},{"name":"OPS","url":"https://b.example/mcp"}],"ai":{"api_key":"k","model":"m"}}`,
			"重复"},
		{"mcp server url 非 HTTP",
			`{"postgres_dsn":"d","mcp_servers":[{"name":"ops","url":"file:///tmp/mcp"}],"ai":{"api_key":"k","model":"m"}}`,
			"http/https"},
		{"mcp server 名称非法",
			`{"postgres_dsn":"d","mcp_servers":[{"name":"运营 工具","url":"https://a.example/mcp"}],"ai":{"api_key":"k","model":"m"}}`,
			"name 只能"},
		{"mcp required_action 非法",
			`{"postgres_dsn":"d","mcp_servers":[{"name":"ops","url":"https://a.example/mcp","required_action":"Manage Worker"}],"ai":{"api_key":"k","model":"m"}}`,
			"required_action"},
		{"TLS 证书缺 key",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","tls_cert_file":"/x/cert.pem","ai":{"api_key":"k","model":"m"}}`,
			"tls_cert_file"},
		{"STT 缺 base_url",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"api_key":"k","model":"m","stt_model":"whisper"}}`,
			"ai.stt_base_url"},
		{"Claude provider 下 embed 必须显式 base_url",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"provider":"claude","api_key":"k","base_url":"https://anthropic.example","model":"m","embed_model":"bge"}}`,
			"ai.embed_base_url"},
		{"Claude provider 下 stt 必须显式 base_url",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"provider":"claude","api_key":"k","base_url":"https://anthropic.example","model":"m","stt_model":"whisper"}}`,
			"ai.stt_base_url"},
		{"Qdrant 缺 embedding",
			`{"postgres_dsn":"d","qdrant":{"url":"http://127.0.0.1:6334"},"ai":{"api_key":"k","model":"m"}}`,
			"ai.embed_model"},
		{"Qdrant URL 含路径",
			`{"postgres_dsn":"d","qdrant":{"url":"http://127.0.0.1:6334/path"},"ai":{"provider":"openai","api_key":"k","base_url":"https://ai.example/v1","model":"m","embed_model":"bge"}}`,
			"qdrant.url"},
		{"Qdrant 同步间隔过短",
			`{"postgres_dsn":"d","qdrant":{"url":"http://127.0.0.1:6334","sync_interval_seconds":5},"ai":{"provider":"openai","api_key":"k","base_url":"https://ai.example/v1","model":"m","embed_model":"bge"}}`,
			"qdrant.sync_interval_seconds"},
		{"Qdrant 单轮同步超时过短",
			`{"postgres_dsn":"d","qdrant":{"url":"http://127.0.0.1:6334","sync_timeout_seconds":300},"ai":{"provider":"openai","api_key":"k","base_url":"https://ai.example/v1","model":"m","embed_model":"bge"}}`,
			"qdrant.sync_timeout_seconds"},
		{"embedding 运行策略版本非法",
			`{"postgres_dsn":"d","ai":{"provider":"openai","api_key":"k","base_url":"https://ai.example/v1","model":"m","embed_model":"bge","embed_revision":"ctx 8192"}}`,
			"ai.embed_revision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.body))
			if err == nil {
				t.Fatal("应返回错误")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("错误 %q 应包含 %q", err, tc.wantErr)
			}
		})
	}
}

func TestOpenAIProviderAllowsAuxiliaryBaseURLFallback(t *testing.T) {
	if _, err := Load(writeConfig(t, `{
		"telegram_token":"t",
		"superadmins":[1],
		"postgres_dsn":"d",
		"ai":{
			"provider":"openai",
			"api_key":"k",
			"base_url":"https://openai-compatible.example/v1",
			"model":"m",
			"embed_model":"bge",
			"stt_model":"whisper"
		}
	}`)); err != nil {
		t.Fatalf("openai 兼容主端点应允许 embed/stt 回退 ai.base_url: %v", err)
	}
}

func TestLoadEmptySuperadminsAllowed(t *testing.T) {
	// 全新系统靠 /superadmin 引导首任超管，配置可留空。
	if _, err := Load(writeConfig(t, `{
		"telegram_token": "tok",
		"postgres_dsn": "postgres://x",
		"ai": {"api_key": "k", "model": "m"}
	}`)); err != nil {
		t.Errorf("superadmins 留空应合法: %v", err)
	}
}

func TestLoadTelegramOptional(t *testing.T) {
	if _, err := Load(writeConfig(t, `{
		"postgres_dsn": "postgres://x",
		"ai": {"api_key": "k", "model": "m"}
	}`)); err != nil {
		t.Errorf("telegram_token 留空应允许 HTTP/API/worker 独立运行: %v", err)
	}
}

func TestLoadTelegramAPIURL(t *testing.T) {
	cfg, err := Load(writeConfig(t, `{
		"telegram_token":"t",
		"telegram_api_url":"http://127.0.0.1:8081/",
		"postgres_dsn":"d",
		"ai":{"api_key":"k","model":"m"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TelegramAPIURL != "http://127.0.0.1:8081" {
		t.Fatalf("TelegramAPIURL = %q", cfg.TelegramAPIURL)
	}
	for _, raw := range []string{"file:///tmp/tg", "https://example.com/api?token=x", "//example.com"} {
		_, err := Load(writeConfig(t, `{
			"telegram_token":"t",
			"telegram_api_url":"`+raw+`",
			"postgres_dsn":"d",
			"ai":{"api_key":"k","model":"m"}
		}`))
		if err == nil || !strings.Contains(err.Error(), "telegram_api_url") {
			t.Errorf("telegram_api_url=%q 应被拒绝，got %v", raw, err)
		}
	}
}

func TestLoadRejectsCLIEngines(t *testing.T) {
	for _, engine := range []string{"claudecli", "codexcli"} {
		_, err := Load(writeConfig(t, `{
			"telegram_token": "tok",
			"superadmins": [1],
			"postgres_dsn": "postgres://x",
			"ai": {"engine": "`+engine+`"}
		}`))
		if err == nil || !strings.Contains(err.Error(), "中枢只支持 eino") {
			t.Errorf("%s 应被拒绝，got %v", engine, err)
		}
	}
}
