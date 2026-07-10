package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/scripttool"
	"github.com/zdypro888/nbco/store"
	"go.starlark.net/starlark"
)

const scriptToolListLimit = 50

var scriptToolNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{2,63}$`)

func scriptToolManagementTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("list_script_tools", "列出脚本工具。脚本工具是超管创建的可执行 tool；适合稳定的数据转换/计算/格式化，不用于 shell、文件系统或网络操作。",
			obj(map[string]any{"enabled_only": p("boolean", "是否只看已启用脚本工具，可选")}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					EnabledOnly bool `json:"enabled_only"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				items, err := d.Store.ListScriptTools(ctx, args.EnabledOnly, scriptToolListLimit)
				if err != nil {
					return "", err
				}
				return renderScriptToolList(items), nil
			}),

		tool("create_script_tool", "创建一个 Starlark 脚本工具。脚本必须定义 run(args) 函数；args 是 JSON 对象。先创建为 disabled，必须 test_script_tool 通过后再 enable_script_tool。不要创建 shell/文件/网络类能力。",
			obj(map[string]any{
				"name":        p("string", "工具名，snake_case，不能与内置工具重名"),
				"description": p("string", "给 AI 看的工具说明：什么时候用、返回什么"),
				"input_schema": map[string]any{
					"type":        "object",
					"description": "JSON Schema object；必须是 object 顶层",
				},
				"source":          p("string", "Starlark 源码，必须定义 run(args)"),
				"required_action": p("string", "调用该脚本工具需要的主动权限，可选；空=所有已加入员工可用"),
			}, "name", "description", "input_schema", "source"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args scriptToolArgs
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				schema, msg := normalizeScriptToolSchema(args.InputSchema)
				if msg != "" {
					return msg, nil
				}
				if msg := validateScriptToolSpec(ctx, d, u, args.Name, args.Description, args.Source, args.RequiredAction); msg != "" {
					return msg, nil
				}
				st, err := d.Store.CreateScriptTool(ctx, store.ScriptTool{
					Name:           strings.TrimSpace(args.Name),
					Description:    strings.TrimSpace(args.Description),
					Runtime:        scripttool.RuntimeStarlark,
					InputSchema:    schema,
					Source:         strings.TrimSpace(args.Source),
					Enabled:        false,
					RequiredAction: strings.TrimSpace(args.RequiredAction),
					CreatedBy:      u.ID,
				})
				if err != nil {
					if errors.Is(err, store.ErrConflict) {
						return "同名脚本工具已存在。", nil
					}
					return "", err
				}
				return fmt.Sprintf("已创建脚本工具（%s）%s（当前 disabled）。请先 test_script_tool，确认通过后再 enable_script_tool。", internalRef("脚本工具", st.ID), st.Name), nil
			}),

		tool("update_script_tool", "更新脚本工具。空字段不改；required_action 传 none 可清空。更新后会自动 disabled，必须重新测试后启用。",
			obj(map[string]any{
				"id":              p("integer", "脚本工具 ID"),
				"name":            p("string", "新工具名，可选"),
				"description":     p("string", "新说明，可选"),
				"input_schema":    map[string]any{"type": "object", "description": "新 JSON Schema object，可选"},
				"source":          p("string", "新 Starlark 源码，可选"),
				"required_action": p("string", "新主动权限；none 清空；可选"),
			}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args scriptToolUpdateArgs
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				cur, err := d.Store.ScriptToolByID(ctx, args.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "脚本工具不存在。", nil
					}
					return "", err
				}
				next := *cur
				if strings.TrimSpace(args.Name) != "" {
					next.Name = strings.TrimSpace(args.Name)
				}
				if strings.TrimSpace(args.Description) != "" {
					next.Description = strings.TrimSpace(args.Description)
				}
				if len(args.InputSchema) > 0 {
					schema, msg := normalizeScriptToolSchema(args.InputSchema)
					if msg != "" {
						return msg, nil
					}
					next.InputSchema = schema
				}
				if strings.TrimSpace(args.Source) != "" {
					next.Source = strings.TrimSpace(args.Source)
				}
				if args.RequiredAction != nil {
					action := strings.TrimSpace(*args.RequiredAction)
					if strings.EqualFold(action, "none") {
						action = ""
					}
					next.RequiredAction = action
				}
				if msg := validateScriptToolSpec(ctx, d, u, next.Name, next.Description, next.Source, next.RequiredAction); msg != "" {
					return msg, nil
				}
				updated, err := d.Store.UpdateScriptTool(ctx, args.ID, next)
				if err != nil {
					return "", err
				}
				_ = d.Store.SetScriptToolEnabled(ctx, args.ID, false)
				return fmt.Sprintf("已更新脚本工具（%s）%s，并已自动 disabled。请重新测试后启用。", internalRef("脚本工具", updated.ID), updated.Name), nil
			}),

		tool("test_script_tool", "测试脚本工具。传入 JSON args，返回脚本输出；测试结果会记录到脚本工具上。",
			obj(map[string]any{
				"id":   p("integer", "脚本工具 ID"),
				"args": map[string]any{"type": "object", "description": "测试入参 JSON 对象，可选"},
			}, "id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID   int64          `json:"id"`
					Args map[string]any `json:"args"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				st, err := d.Store.ScriptToolByID(ctx, args.ID)
				if err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "脚本工具不存在。", nil
					}
					return "", err
				}
				out, err := runStoredScriptTool(ctx, st, args.Args)
				result := out
				if err != nil {
					result = "ERROR: " + err.Error()
				}
				_ = d.Store.SetScriptToolTestResult(ctx, st.ID, truncate(result, 2000))
				if err != nil {
					return result, nil
				}
				return "测试通过，输出：\n" + out, nil
			}),

		tool("enable_script_tool", "启用或停用脚本工具。启用前必须确认 test_script_tool 通过；启用后会作为普通 tool 出现在权限允许的对话里。",
			obj(map[string]any{
				"id":      p("integer", "脚本工具 ID"),
				"enabled": p("boolean", "true 启用，false 停用"),
			}, "id", "enabled"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					ID      int64 `json:"id"`
					Enabled bool  `json:"enabled"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				if _, err := d.Store.ScriptToolByID(ctx, args.ID); err != nil {
					if errors.Is(err, store.ErrNotFound) {
						return "脚本工具不存在。", nil
					}
					return "", err
				}
				if err := d.Store.SetScriptToolEnabled(ctx, args.ID, args.Enabled); err != nil {
					return "", err
				}
				if args.Enabled {
					return "脚本工具已启用。", nil
				}
				return "脚本工具已停用。", nil
			}),
	}
}

type scriptToolArgs struct {
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	InputSchema    map[string]any `json:"input_schema"`
	Source         string         `json:"source"`
	RequiredAction string         `json:"required_action"`
}

type scriptToolUpdateArgs struct {
	ID             int64          `json:"id"`
	Name           string         `json:"name"`
	Description    string         `json:"description"`
	InputSchema    map[string]any `json:"input_schema"`
	Source         string         `json:"source"`
	RequiredAction *string        `json:"required_action"`
}

func dynamicScriptTools(ctx context.Context, d Deps, u *store.User, grants []store.Grant) []ai.Tool {
	if d.Store == nil || u == nil {
		return nil
	}
	items, err := d.Store.ListScriptTools(ctx, true, 100)
	if err != nil {
		slog.Warn("加载动态脚本工具失败，本轮按无脚本工具降级", "user", u.ID, "err", err)
		return nil
	}
	var out []ai.Tool
	for _, st := range items {
		if st == nil || !scriptToolAllowed(u, grants, st.RequiredAction) {
			continue
		}
		schema := map[string]any{}
		if err := json.Unmarshal(st.InputSchema, &schema); err != nil || schema["type"] != "object" {
			schema = obj(nil)
		}
		script := *st
		toolDef := tool(script.Name, script.Description, schema, func(ctx context.Context, raw json.RawMessage) (string, error) {
			const timeout = 2 * time.Minute
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			out, err := scripttool.Run(runCtx, script.Name, script.Source, raw, scripttool.RunOptions{
				Timeout:     timeout,
				Predeclared: scriptBuiltins(runCtx, d, u, grants, script.Name),
			})
			if err != nil {
				return "脚本工具执行失败：" + err.Error(), nil
			}
			return truncate(out, 12000), nil
		})
		// 脚本可组合领域工具和 AI，保守地按执行型能力审计。
		toolDef.Effect = ai.ToolEffectExecute
		toolDef.RequiredAction = script.RequiredAction
		toolDef.GroupSensitive = true
		out = append(out, toolDef)
	}
	return out
}

func scriptToolAllowed(u *store.User, grants []store.Grant, required string) bool {
	if u.IsWorker {
		return false
	}
	required = strings.TrimSpace(required)
	if required == "" {
		return true
	}
	if u.IsSuperadmin {
		return true
	}
	return hasAnyActive(grants, required)
}

func scriptValidationBuiltins() starlark.StringDict {
	return starlark.StringDict{
		"nbco_tool": starlark.NewBuiltin("nbco_tool", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var name string
			var input starlark.Value = starlark.NewDict(0)
			if err := starlark.UnpackArgs("nbco_tool", args, kwargs, "name", &name, "args?", &input); err != nil {
				return nil, err
			}
			return starlark.String("{}"), nil
		}),
		"nbco_ai": starlark.NewBuiltin("nbco_ai", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var prompt string
			if err := starlark.UnpackArgs("nbco_ai", args, kwargs, "prompt", &prompt); err != nil {
				return nil, err
			}
			return starlark.String(""), nil
		}),
	}
}

func scriptBuiltins(ctx context.Context, d Deps, u *store.User, grants []store.Grant, selfName string) starlark.StringDict {
	return starlark.StringDict{
		"nbco_tool": starlark.NewBuiltin("nbco_tool", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var name string
			var input starlark.Value = starlark.NewDict(0)
			if err := starlark.UnpackArgs("nbco_tool", args, kwargs, "name", &name, "args?", &input); err != nil {
				return nil, err
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return nil, fmt.Errorf("tool name 不能为空")
			}
			if name == selfName {
				return nil, fmt.Errorf("脚本不能递归调用自己")
			}
			goVal, err := scripttool.FromStarlark(input)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(goVal)
			if err != nil {
				return nil, err
			}
			for _, t := range filterByPerm(staticToolsForScript(d, u), u, grants) {
				if t.Name != name {
					continue
				}
				if d.Store != nil {
					t = withAudit(d.Store, u.ID, nil, withApproval(d.Store, u.ID, t))
				}
				out, err := t.Handler(ctx, raw)
				if err != nil {
					return nil, err
				}
				return starlark.String(out), nil
			}
			return nil, fmt.Errorf("当前用户不可调用工具 %s", name)
		}),
		"nbco_ai": starlark.NewBuiltin("nbco_ai", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var prompt string
			if err := starlark.UnpackArgs("nbco_ai", args, kwargs, "prompt", &prompt); err != nil {
				return nil, err
			}
			if d.ScriptAI == nil {
				return nil, fmt.Errorf("脚本 AI 能力未配置")
			}
			out, err := d.ScriptAI(ctx, u, prompt)
			if err != nil {
				return nil, err
			}
			return starlark.String(out), nil
		}),
	}
}

func normalizeScriptToolSchema(schema map[string]any) ([]byte, string) {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if typ, _ := schema["type"].(string); typ == "" {
		schema["type"] = "object"
	} else if typ != "object" {
		return nil, "input_schema 顶层 type 必须是 object。"
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, "input_schema 不是合法 JSON 对象。"
	}
	if len(raw) > 12000 {
		return nil, "input_schema 太长。"
	}
	return raw, ""
}

func validateScriptToolSpec(ctx context.Context, d Deps, u *store.User, name, desc, source, requiredAction string) string {
	name = strings.TrimSpace(name)
	if !scriptToolNameRe.MatchString(name) {
		return "工具名必须是 3-64 位 snake_case/标识符格式。"
	}
	if staticToolNames(d, u)[name] {
		return "工具名与内置工具重名，请换一个名字。"
	}
	if strings.TrimSpace(desc) == "" || strings.TrimSpace(source) == "" {
		return "description 和 source 都不能为空。"
	}
	if len(source) > 20000 {
		return "脚本源码太长。"
	}
	requiredAction = strings.TrimSpace(requiredAction)
	if requiredAction != "" && !perm.ValidActiveAction(requiredAction) {
		return "required_action 不是合法主动权限。"
	}
	if err := scripttool.Validate(ctx, name, source, scripttool.RunOptions{Predeclared: scriptValidationBuiltins()}); err != nil {
		return "脚本自检失败：" + err.Error()
	}
	return ""
}

func staticToolNames(d Deps, u *store.User) map[string]bool {
	out := map[string]bool{}
	for _, t := range staticToolsForScript(d, u) {
		out[t.Name] = true
	}
	return out
}

func staticToolsForScript(d Deps, u *store.User) []ai.Tool {
	ts := []ai.Tool{}
	ts = append(ts, profileTools(d, u)...)
	ts = append(ts, permTools(d, u)...)
	ts = append(ts, taskTools(d, u)...)
	ts = append(ts, reviewTools(d, u)...)
	ts = append(ts, scheduleTools(d, u)...)
	ts = append(ts, roleTools(d, u)...)
	ts = append(ts, knowledgeTools(d, u)...)
	ts = append(ts, memoryTools(d, u)...)
	ts = append(ts, fileTools(d, u)...)
	ts = append(ts, ruleTools(d, u)...)
	ts = append(ts, skillTools(d, u)...)
	ts = append(ts, learningTools(d, u)...)
	ts = append(ts, scriptToolManagementTools(d, u)...)
	ts = append(ts, workerTools(d, u)...)
	ts = append(ts, materialTools(d, u)...)
	ts = append(ts, telegramGroupTools(d, u)...)
	ts = append(ts, adminTools(d, u)...)
	ts = append(ts, d.Extra...)
	return ts
}

func runStoredScriptTool(ctx context.Context, st *store.ScriptTool, args map[string]any) (string, error) {
	if st.Runtime != scripttool.RuntimeStarlark {
		return "", fmt.Errorf("不支持的脚本运行时 %q", st.Runtime)
	}
	if args == nil {
		args = map[string]any{}
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	return scripttool.Run(ctx, st.Name, st.Source, raw, scripttool.RunOptions{})
}

func renderScriptToolList(items []*store.ScriptTool) string {
	if len(items) == 0 {
		return "暂无脚本工具。"
	}
	var b strings.Builder
	b.WriteString("脚本工具：\n")
	for _, st := range items {
		state := "disabled"
		if st.Enabled {
			state = "enabled"
		}
		req := st.RequiredAction
		if strings.TrimSpace(req) == "" {
			req = "无"
		}
		fmt.Fprintf(&b, "- %s：%s（%s，runtime=%s，required_action=%s）：%s\n",
			internalRef("脚本工具", st.ID), st.Name, state, st.Runtime, req, st.Description)
		if strings.TrimSpace(st.LastTestResult) != "" {
			fmt.Fprintf(&b, "  最近测试：%s\n", truncate(st.LastTestResult, 300))
		}
	}
	return strings.TrimSpace(b.String())
}
