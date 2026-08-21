package workerproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	TaskIORelDir                 = ".nbco-task/current"
	StructuredResultRelativePath = TaskIORelDir + "/structured-result.json"
	StructuredResultMaxBytes     = 1 << 20
	ResultSchemaMaxBytes         = 64 << 10
)

var (
	ErrStructuredResultRequired = errors.New("structured result is required")
)

// NormalizeResultSchema validates and compacts one local JSON Schema. Worker
// result contracts deliberately reject network references: a task claim must
// not turn schema validation into hidden I/O.
func NormalizeResultSchema(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	if len(raw) > ResultSchemaMaxBytes {
		return nil, fmt.Errorf("result schema exceeds %d bytes", ResultSchemaMaxBytes)
	}
	value, err := decodeSingleJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid result schema: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("result schema must be a JSON object")
	}
	if ref := externalSchemaRef(value); ref != "" {
		return nil, fmt.Errorf("external result schema reference is not allowed: %s", ref)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(compact.Bytes()), nil
}

// NormalizeStructuredResult accepts exactly one JSON object and returns its
// compact representation. Human summaries and machine results stay separate.
func NormalizeStructuredResult(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > StructuredResultMaxBytes {
		return nil, fmt.Errorf("structured result exceeds %d bytes", StructuredResultMaxBytes)
	}
	value, err := decodeSingleJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid structured result: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("structured result must be a JSON object")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return json.RawMessage(compact.Bytes()), nil
}

// ValidateStructuredResult enforces the durable run contract. An empty schema
// still accepts any object; Required independently controls presence.
func ValidateStructuredResult(required bool, schemaRaw, resultRaw json.RawMessage) (json.RawMessage, error) {
	schemaRaw, err := NormalizeResultSchema(schemaRaw)
	if err != nil {
		return nil, err
	}
	resultRaw, err = NormalizeStructuredResult(resultRaw)
	if err != nil {
		return nil, err
	}
	if len(resultRaw) == 0 {
		if required {
			return nil, ErrStructuredResultRequired
		}
		return nil, nil
	}
	compiled, err := compileResultSchema(schemaRaw)
	if err != nil {
		return nil, fmt.Errorf("compile result schema: %w", err)
	}
	value, err := decodeSingleJSON(resultRaw)
	if err != nil {
		return nil, err
	}
	if err := compiled.Validate(value); err != nil {
		return nil, fmt.Errorf("structured result does not match schema: %w", err)
	}
	return resultRaw, nil
}

func compileResultSchema(raw json.RawMessage) (*jsonschema.Schema, error) {
	value, err := decodeSingleJSON(raw)
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	const location = "nbco://worker/result-schema.json"
	if err := compiler.AddResource(location, value); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
}

func decodeSingleJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return value, nil
}

func externalSchemaRef(value any) string {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			if key == "$ref" || key == "$dynamicRef" || key == "$recursiveRef" {
				if ref, ok := item.(string); ok && !strings.HasPrefix(strings.TrimSpace(ref), "#") {
					return ref
				}
			}
			if ref := externalSchemaRef(item); ref != "" {
				return ref
			}
		}
	case []any:
		for _, item := range value {
			if ref := externalSchemaRef(item); ref != "" {
				return ref
			}
		}
	}
	return ""
}
