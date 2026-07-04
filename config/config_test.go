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
	if cfg.AI.MaxTokens != 4096 || cfg.AI.MaxTurns != 16 {
		t.Errorf("MaxTokens/MaxTurns 默认值 = %d/%d", cfg.AI.MaxTokens, cfg.AI.MaxTurns)
	}
	if cfg.AI.StreamReasoning {
		t.Error("stream_reasoning 默认不应展示推理过程")
	}
	if cfg.FileStorePath != "files" {
		t.Errorf("FileStorePath 默认值 = %q", cfg.FileStorePath)
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
		{"未知引擎",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"engine":"gpt"}}`,
			"ai.engine 不支持"},
		{"未知 provider",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","ai":{"provider":"gemini","api_key":"k","model":"m"}}`,
			"ai.provider 不支持"},
		{"mcp server 缺 url",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","mcp_servers":[{"name":"x"}],"ai":{"api_key":"k","model":"m"}}`,
			"mcp_servers[0]"},
		{"TLS 证书缺 key",
			`{"telegram_token":"t","superadmins":[1],"postgres_dsn":"d","tls_cert_file":"/x/cert.pem","ai":{"api_key":"k","model":"m"}}`,
			"tls_cert_file"},
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
