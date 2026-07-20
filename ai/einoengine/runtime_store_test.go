package einoengine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"
)

func TestEncodeSessionEventPreservesCanonicalEscapedJSON(t *testing.T) {
	secret := strings.Repeat("a", 48)
	command := `curl -H "X-Auth-Token: ` + secret + `" https://example.test 2>&1`
	event := &adk.SessionEvent[*schema.Message]{
		EventID: "tool-event",
		Message: schema.ToolMessage(`{"command":`+strconvQuote(command)+`,"token":"`+secret+`"}`, "call-1"),
	}
	serializer := &schema.HumanReadableSerializer{}
	encoded, err := encodeSessionEvent(serializer, event)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(encoded) {
		t.Fatalf("encoded event is invalid JSON: %s", encoded)
	}
	if !strings.Contains(string(encoded), secret) {
		t.Fatalf("canonical event lost credential: %s", encoded)
	}
	var decoded adk.SessionEvent[*schema.Message]
	if err := serializer.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("canonical event no longer round-trips: %v\n%s", err, encoded)
	}
	if decoded.Message == nil || decoded.Message.Content != event.Message.Content {
		t.Fatalf("canonical message changed: %#v", decoded.Message)
	}
	var content map[string]string
	if err := json.Unmarshal([]byte(decoded.Message.Content), &content); err != nil {
		t.Fatalf("nested tool payload is invalid JSON: %v\n%s", err, decoded.Message.Content)
	}
	if content["command"] != command || content["token"] != secret {
		t.Fatalf("nested tool payload lost structure: %#v", content)
	}
}

func strconvQuote(value string) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}
