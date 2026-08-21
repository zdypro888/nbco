package workerproto

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateStructuredResultContract(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["items"],
		"additionalProperties":false,
		"properties":{"items":{"type":"array","items":{"type":"string"}}}
	}`)
	got, err := ValidateStructuredResult(true, schema, json.RawMessage(` { "items": ["a"] } `))
	if err != nil || string(got) != `{"items":["a"]}` {
		t.Fatalf("valid result = %s, %v", got, err)
	}
	if _, err := ValidateStructuredResult(true, schema, nil); !errors.Is(err, ErrStructuredResultRequired) {
		t.Fatalf("missing required result error = %v", err)
	}
	if _, err := ValidateStructuredResult(false, schema, json.RawMessage(`[]`)); err == nil {
		t.Fatal("array result accepted")
	}
	if _, err := ValidateStructuredResult(false, schema, json.RawMessage(`{"other":1}`)); err == nil {
		t.Fatal("schema mismatch accepted")
	}
}

func TestResultSchemaRejectsExternalReferencesAndOversize(t *testing.T) {
	for _, keyword := range []string{"$ref", "$dynamicRef", "$recursiveRef"} {
		schema := json.RawMessage(fmt.Sprintf(`{"%s":"https://example.com/schema.json"}`, keyword))
		if _, err := NormalizeResultSchema(schema); err == nil {
			t.Fatalf("external schema reference accepted for %s", keyword)
		}
	}
	oversize := json.RawMessage(`{"value":"` + strings.Repeat("x", ResultSchemaMaxBytes) + `"}`)
	if _, err := NormalizeResultSchema(oversize); err == nil {
		t.Fatal("oversize schema accepted")
	}
}
