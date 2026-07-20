package einoengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestRedactSessionEventPayloadPreservesEscapedJSON(t *testing.T) {
	secret := strings.Repeat("a", 48)
	command := `curl -H "X-Auth-Token: ` + secret + `" https://example.test 2>&1`
	event := &adk.SessionEvent[*schema.Message]{
		EventID: "tool-event",
		Message: schema.ToolMessage(`{"command":`+strconvQuote(command)+`,"token":"`+secret+`"}`, "call-1"),
	}
	serializer := &schema.HumanReadableSerializer{}
	payload, err := serializer.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := redactSessionEventPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(redacted) {
		t.Fatalf("redacted event is invalid JSON: %s", redacted)
	}
	if strings.Contains(string(redacted), secret) {
		t.Fatalf("redacted event leaked secret: %s", redacted)
	}
	var decoded adk.SessionEvent[*schema.Message]
	if err := serializer.Unmarshal(redacted, &decoded); err != nil {
		t.Fatalf("redacted event no longer round-trips: %v\n%s", err, redacted)
	}
	if decoded.Message == nil || !strings.Contains(decoded.Message.Content, "[redacted]") {
		t.Fatalf("redacted message lost structure: %#v", decoded.Message)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(decoded.Message.Content), &content); err != nil {
		t.Fatalf("nested tool payload is invalid JSON: %v\n%s", err, decoded.Message.Content)
	}
	if !strings.Contains(content["command"], "X-Auth-Token: [redacted]") || !strings.Contains(content["command"], "2>&1") ||
		content["token"] != "[redacted]" {
		t.Fatalf("nested tool payload lost structure: %#v", content)
	}
	if !strings.Contains(event.Message.Content, secret) {
		t.Fatal("redaction mutated the live event")
	}
}

func strconvQuote(value string) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}
