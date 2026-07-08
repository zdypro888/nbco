// Package scripttool runs user-defined script tools in a constrained runtime.
package scripttool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.starlark.net/starlark"
)

const (
	RuntimeStarlark = "starlark"

	defaultTimeout = 2 * time.Second
	defaultMaxStep = uint64(200000)
)

type RunOptions struct {
	Timeout     time.Duration
	MaxSteps    uint64
	Predeclared starlark.StringDict
}

func Run(ctx context.Context, name, source string, args json.RawMessage, opts RunOptions) (string, error) {
	if strings.TrimSpace(source) == "" {
		return "", errors.New("脚本源码不能为空")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxStep
	}
	var decoded any
	if len(args) == 0 {
		decoded = map[string]any{}
	} else if err := json.Unmarshal(args, &decoded); err != nil {
		return "", fmt.Errorf("脚本入参不是 JSON: %w", err)
	}
	argVal, err := toStarlark(decoded)
	if err != nil {
		return "", err
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	thread := &starlark.Thread{Name: "script_tool:" + name}
	thread.SetMaxExecutionSteps(opts.MaxSteps)
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			thread.Cancel(runCtx.Err().Error())
		case <-done:
		}
	}()
	defer close(done)

	globals, err := starlark.ExecFile(thread, name+".star", source, predeclared(opts.Predeclared))
	if err != nil {
		return "", err
	}
	run := globals["run"]
	if run == nil {
		return "", errors.New("脚本必须定义 run(args) 函数")
	}
	callable, ok := run.(starlark.Callable)
	if !ok {
		return "", errors.New("run 必须是函数")
	}
	ret, err := starlark.Call(thread, callable, starlark.Tuple{argVal}, nil)
	if err != nil {
		return "", err
	}
	return renderValue(ret), nil
}

func Validate(ctx context.Context, name, source string, opts RunOptions) error {
	if strings.TrimSpace(source) == "" {
		return errors.New("脚本源码不能为空")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = defaultTimeout
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = defaultMaxStep
	}
	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	thread := &starlark.Thread{Name: "script_tool_validate:" + name}
	thread.SetMaxExecutionSteps(opts.MaxSteps)
	done := make(chan struct{})
	go func() {
		select {
		case <-runCtx.Done():
			thread.Cancel(runCtx.Err().Error())
		case <-done:
		}
	}()
	defer close(done)
	globals, err := starlark.ExecFile(thread, name+".star", source, predeclared(opts.Predeclared))
	if err != nil {
		return err
	}
	run := globals["run"]
	if run == nil {
		return errors.New("脚本必须定义 run(args) 函数")
	}
	if _, ok := run.(starlark.Callable); !ok {
		return errors.New("run 必须是函数")
	}
	return nil
}

func predeclared(extra starlark.StringDict) starlark.StringDict {
	out := starlark.StringDict{
		"json_encode": starlark.NewBuiltin("json_encode", func(_ *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {
			var v starlark.Value
			if err := starlark.UnpackArgs("json_encode", args, kwargs, "value", &v); err != nil {
				return nil, err
			}
			goVal, err := fromStarlark(v)
			if err != nil {
				return nil, err
			}
			raw, err := json.Marshal(goVal)
			if err != nil {
				return nil, err
			}
			return starlark.String(raw), nil
		}),
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func FromStarlark(v starlark.Value) (any, error) {
	return fromStarlark(v)
}

func ToStarlark(v any) (starlark.Value, error) {
	return toStarlark(v)
}

func toStarlark(v any) (starlark.Value, error) {
	switch x := v.(type) {
	case nil:
		return starlark.None, nil
	case bool:
		return starlark.Bool(x), nil
	case string:
		return starlark.String(x), nil
	case float64:
		return starlark.Float(x), nil
	case []any:
		items := make([]starlark.Value, 0, len(x))
		for _, item := range x {
			sv, err := toStarlark(item)
			if err != nil {
				return nil, err
			}
			items = append(items, sv)
		}
		return starlark.NewList(items), nil
	case map[string]any:
		d := starlark.NewDict(len(x))
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sv, err := toStarlark(x[k])
			if err != nil {
				return nil, err
			}
			if err := d.SetKey(starlark.String(k), sv); err != nil {
				return nil, err
			}
		}
		return d, nil
	default:
		return nil, fmt.Errorf("不支持的 JSON 类型 %T", v)
	}
}

func fromStarlark(v starlark.Value) (any, error) {
	switch x := v.(type) {
	case starlark.NoneType:
		return nil, nil
	case starlark.Bool:
		return bool(x), nil
	case starlark.Int:
		if i, ok := x.Int64(); ok {
			return i, nil
		}
		return x.String(), nil
	case starlark.Float:
		return float64(x), nil
	case starlark.String:
		return string(x), nil
	case *starlark.List:
		out := make([]any, 0, x.Len())
		iter := x.Iterate()
		defer iter.Done()
		var item starlark.Value
		for iter.Next(&item) {
			vv, err := fromStarlark(item)
			if err != nil {
				return nil, err
			}
			out = append(out, vv)
		}
		return out, nil
	case *starlark.Dict:
		out := map[string]any{}
		for _, item := range x.Items() {
			k, ok := starlark.AsString(item[0])
			if !ok {
				return nil, errors.New("dict key 必须是字符串")
			}
			vv, err := fromStarlark(item[1])
			if err != nil {
				return nil, err
			}
			out[k] = vv
		}
		return out, nil
	default:
		return v.String(), nil
	}
}

func renderValue(v starlark.Value) string {
	if s, ok := starlark.AsString(v); ok {
		return s
	}
	goVal, err := fromStarlark(v)
	if err != nil {
		return v.String()
	}
	raw, err := json.MarshalIndent(goVal, "", "  ")
	if err != nil {
		return v.String()
	}
	return string(raw)
}
