package notify

import "testing"

func TestParseDecision(t *testing.T) {
	for _, tt := range []struct {
		name    string
		input   string
		notify  bool
		message string
		wantErr bool
	}{
		{name: "notify", input: `{"notify":true,"message":"需要处理"}`, notify: true, message: "需要处理"},
		{name: "skip", input: `{"notify":false,"message":"unused"}`},
		{name: "missing flag", input: `{"message":"x"}`, wantErr: true},
		{name: "missing message", input: `{"notify":true,"message":""}`, wantErr: true},
		{name: "natural language sentinel", input: `NO_NOTIFY`, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDecision(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDecision error = %v, wantErr=%t", err, tt.wantErr)
			}
			if err == nil && (got.Notify != tt.notify || got.Message != tt.message) {
				t.Fatalf("ParseDecision = %+v", got)
			}
		})
	}
}
