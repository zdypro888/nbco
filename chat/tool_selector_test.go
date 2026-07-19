package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/ai/einoengine"
	"github.com/zdypro888/nbco/config"
	"github.com/zdypro888/nbco/store"
	"github.com/zdypro888/nbco/tools"
)

func TestSelectPreferredToolsUsesAuthorizedSemanticCatalog(t *testing.T) {
	all := make([]ai.Tool, 0, 16)
	all = append(all,
		ai.Tool{Name: "list_workers", Domain: "workers", Effect: ai.ToolEffectRead, Description: "读取可用 Worker 与稳定 ID。"},
		ai.Tool{Name: "delegate_worker_agent", Domain: "workers", Effect: ai.ToolEffectExecute, Description: "把需要持续判断的工作委派给 Worker Agent。"},
	)
	for i := 0; i < 14; i++ {
		all = append(all, ai.Tool{
			Name: fmt.Sprintf("unrelated_%02d", i), Domain: "other", Effect: ai.ToolEffectRead,
			Description: "与当前目标无关的测试能力。",
		})
	}

	called := false
	orchestrator := &Orchestrator{deps: tools.Deps{SubcallAI: func(
		_ context.Context, _ *store.User, purpose, prompt string,
	) (string, error) {
		called = true
		if purpose != "tool_selector" {
			t.Fatalf("purpose = %q", purpose)
		}
		for _, want := range []string{"delegate_worker_agent", "workers", "execute", "接着让它处理"} {
			if !strings.Contains(prompt, want) {
				t.Fatalf("selector prompt missing %q", want)
			}
		}
		return `{"tools":["list_workers","delegate_worker_agent","not_authorized","delegate_worker_agent"]}`, nil
	}}}
	selection := orchestrator.selectPreferredTools(context.Background(), &store.User{ID: 7},
		"接着让它处理", []store.ChatMessage{{Role: "user", Content: "用 NBAI 分析"}}, all)
	if !called {
		t.Fatal("semantic selector was not called")
	}
	if selection.Source != "ai" || strings.Join(selection.Names, ",") != "list_workers,delegate_worker_agent" {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestSelectPreferredToolsAllowsNoToolConversation(t *testing.T) {
	all := make([]ai.Tool, preferredToolLimit+1)
	for i := range all {
		all[i] = ai.Tool{Name: fmt.Sprintf("tool_%02d", i)}
	}
	orchestrator := &Orchestrator{deps: tools.Deps{SubcallAI: func(
		context.Context, *store.User, string, string,
	) (string, error) {
		return `{"tools":[]}`, nil
	}}}
	selection := orchestrator.selectPreferredTools(context.Background(), &store.User{ID: 1}, "你好", nil, all)
	if selection.Source != "ai_empty" || len(selection.Names) != 0 {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestSelectPreferredToolsSmallCatalogNeedsNoSubcall(t *testing.T) {
	all := []ai.Tool{{Name: "read_one"}, {Name: "write_one"}}
	orchestrator := &Orchestrator{deps: tools.Deps{SubcallAI: func(
		context.Context, *store.User, string, string,
	) (string, error) {
		t.Fatal("small catalog should not call AI selector")
		return "", nil
	}}}
	selection := orchestrator.selectPreferredTools(context.Background(), &store.User{ID: 1}, "处理", nil, all)
	if selection.Source != "small_catalog" || strings.Join(selection.Names, ",") != "read_one,write_one" {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestSelectPreferredToolsFallsBackToNativeSearch(t *testing.T) {
	all := make([]ai.Tool, preferredToolLimit+1)
	for i := range all {
		all[i] = ai.Tool{Name: fmt.Sprintf("tool_%02d", i)}
	}
	for _, tc := range []struct {
		name   string
		output string
		err    error
		source string
	}{
		{name: "subcall error", err: errors.New("model unavailable"), source: "ai_error"},
		{name: "invalid selection", output: `{"tools":["not_authorized"]}`, source: "ai_invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orchestrator := &Orchestrator{deps: tools.Deps{SubcallAI: func(
				context.Context, *store.User, string, string,
			) (string, error) {
				return tc.output, tc.err
			}}}
			selection := orchestrator.selectPreferredTools(context.Background(), &store.User{ID: 1}, "处理", nil, all)
			if selection.Source != tc.source || len(selection.Names) != 0 {
				t.Fatalf("selection = %+v", selection)
			}
		})
	}
}

func TestSmokeRealSemanticToolSelection(t *testing.T) {
	if os.Getenv("NBCO_SMOKE_SELECTOR") == "" {
		t.Skip("set NBCO_SMOKE_SELECTOR=1 and NBCO_SMOKE_* to run real semantic tool selection")
	}
	engine, err := einoengine.New(context.Background(), config.AIConfig{
		Engine:   config.EngineEino,
		Provider: os.Getenv("NBCO_SMOKE_PROVIDER"),
		APIKey:   os.Getenv("NBCO_SMOKE_KEY"),
		BaseURL:  os.Getenv("NBCO_SMOKE_BASE"),
		Model:    os.Getenv("NBCO_SMOKE_MODEL"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var workerCalls atomic.Int32
	all := []ai.Tool{
		{
			Name: "list_workers", Domain: "workers", Effect: ai.ToolEffectRead,
			Description: "列出当前用户可调用的 Worker、稳定 ID 与在线状态。",
			InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				return `{"workers":[{"id":2,"name":"NBAI","online":true}]}`, nil
			},
		},
		{
			Name: "run_worker_command", Domain: "workers", Effect: ai.ToolEffectExecute,
			Description: "让指定 Worker 执行无需持续交互的原子命令。",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"worker_id": map[string]any{"type": "integer"},
					"command":   map[string]any{"type": "string"},
				}, "required": []string{"worker_id", "command"},
			},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				workerCalls.Add(1)
				return `{"queued":true,"worker_id":2}`, nil
			},
		},
		{
			Name: "delegate_worker_agent", Domain: "workers", Effect: ai.ToolEffectExecute,
			Description: "把需要持续判断的目标交给指定 Worker 上的 Codex 或 Claude Agent。",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{
					"worker_id": map[string]any{"type": "integer"},
					"task":      map[string]any{"type": "string"},
				}, "required": []string{"worker_id", "task"},
			},
			Handler: func(context.Context, json.RawMessage) (string, error) {
				workerCalls.Add(1)
				return `{"queued":true,"worker_id":2,"agent":"codex"}`, nil
			},
		},
		{
			Name: "fetch_url", Domain: "research", Effect: ai.ToolEffectRead,
			Description: "读取一个已知公开 URL 的内容。",
			InputSchema: map[string]any{"type": "object"},
			Handler:     func(context.Context, json.RawMessage) (string, error) { return `{}`, nil },
		},
	}
	for i := 0; i < 170; i++ {
		all = append(all, ai.Tool{
			Name: fmt.Sprintf("unrelated_capability_%03d", i), Domain: "other", Effect: ai.ToolEffectRead,
			Description: "与当前请求无关的占位能力。", InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, json.RawMessage) (string, error) { return `{}`, nil },
		})
	}
	orchestrator := &Orchestrator{deps: tools.Deps{SubcallAI: func(
		ctx context.Context, _ *store.User, _ string, prompt string,
	) (string, error) {
		result, runErr := engine.RunTurn(ctx, &ai.TurnRequest{
			Mode: ai.TurnModeOneShot, System: "只完成能力检索并输出要求的 JSON。", UserText: prompt,
		})
		if runErr != nil {
			return "", runErr
		}
		return result.Text, nil
	}}}
	selection := orchestrator.selectPreferredTools(context.Background(), &store.User{ID: 1, Name: "PRO"},
		"让 NBAI 用 Codex 查询法国和英格兰世界杯谁赢了",
		[]store.ChatMessage{{Role: "user", Content: "NBAI 能联网查询吗"}}, all)
	if selection.Source != "ai" {
		t.Fatalf("selection = %+v", selection)
	}
	if !slices.Contains(selection.Names, "delegate_worker_agent") &&
		!slices.Contains(selection.Names, "run_worker_command") {
		t.Fatalf("worker execution capability was not selected: %+v", selection)
	}
	result, err := engine.RunTurn(context.Background(), &ai.TurnRequest{
		Mode:     ai.TurnModeDeep,
		System:   "最相关工具已预加载。用户明确要求执行时直接调用可见工具；只有缺少能力时才搜索其余目录。",
		UserText: "让 NBAI 用 Codex 查询法国和英格兰世界杯谁赢了",
		Tools:    all, PreferredTools: selection.Names,
	})
	if err != nil {
		t.Fatal(err)
	}
	if workerCalls.Load() == 0 || result.CompletionOutcome != ai.CompletionOutcomeActionResult {
		t.Fatalf("semantic selection did not lead to worker execution: selection=%+v result=%+v", selection, result)
	}
	t.Logf("real semantic execution passed: selection=%+v result=%q exposure=%+v", selection, result.Text, result.ToolExposure)
}
