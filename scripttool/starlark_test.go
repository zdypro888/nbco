package scripttool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRunStarlark(t *testing.T) {
	src := `
def run(args):
    name = args.get("name", "world")
    return {"message": "hello " + name, "ok": True}
`
	out, err := Run(context.Background(), "hello_tool", src, json.RawMessage(`{"name":"nbco"}`), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"message": "hello nbco"`) || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestValidateRequiresRun(t *testing.T) {
	if err := Validate(context.Background(), "bad_tool", `x = 1`, RunOptions{}); err == nil {
		t.Fatal("expected missing run error")
	}
}

func TestRunCancelsOnStepLimit(t *testing.T) {
	src := `
def run(args):
    n = 0
    for i in range(100000000):
        n = n + i
    return n
`
	_, err := Run(context.Background(), "slow_tool", src, nil, RunOptions{MaxSteps: 1000})
	if err == nil {
		t.Fatal("expected max step cancellation")
	}
	if !strings.Contains(err.Error(), "too many steps") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestJSONEncodeBuiltin(t *testing.T) {
	src := `
def run(args):
    return json_encode({"items": [args["x"], 2]})
`
	out, err := Run(context.Background(), "json_tool", src, json.RawMessage(`{"x":1}`), RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"items":[1,2]}` {
		t.Fatalf("json_encode output = %q", out)
	}
}
