package tools

import (
	"encoding/json"
	"strconv"
	"testing"
)

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
