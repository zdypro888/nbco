package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/ai"
)

// withArgumentNormalization makes tool invocation tolerant of harmless model
// serialization drift. The tool's JSON Schema remains the source of truth:
// aliases are accepted only when they resolve to exactly one declared field,
// and values are coerced only to the declared primitive type.
func withArgumentNormalization(t ai.Tool) ai.Tool {
	inner := t.Handler
	schema := t.InputSchema
	t.Handler = func(ctx context.Context, raw json.RawMessage) (string, error) {
		return inner(ctx, normalizeToolArgs(raw, schema))
	}
	return t
}

func normalizeToolArgs(raw json.RawMessage, schema map[string]any) json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	var value map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&value) != nil || value == nil {
		return raw
	}
	if len(schemaProperties(schema)) == 0 {
		return raw
	}
	value = normalizeSchemaObject(value, schema)
	encoded, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return encoded
}

func normalizeSchemaObject(value map[string]any, schema map[string]any) map[string]any {
	props := schemaProperties(schema)
	for key, item := range value {
		if _, ok := props[key]; ok {
			continue
		}
		canonical := resolveSchemaAlias(key, props)
		if canonical == "" {
			continue
		}
		if _, exists := value[canonical]; !exists {
			value[canonical] = item
		}
		delete(value, key)
	}
	for key, item := range value {
		if spec, ok := props[key]; ok {
			value[key] = coerceSchemaValue(item, spec)
		}
	}
	return value
}

func schemaProperties(schema map[string]any) map[string]any {
	if schema == nil {
		return nil
	}
	props, _ := schema["properties"].(map[string]any)
	return props
}

func resolveSchemaAlias(key string, props map[string]any) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	for name := range props {
		if strings.EqualFold(name, key) {
			return name
		}
	}
	var candidates []string
	add := func(name string) {
		if _, ok := props[name]; !ok {
			return
		}
		for _, current := range candidates {
			if current == name {
				return
			}
		}
		candidates = append(candidates, name)
	}
	if base, ok := strings.CutSuffix(key, "_ref"); ok {
		add(base)
		add(base + "_id")
	}
	if key == "internal_id" {
		add("id")
		for name := range props {
			if strings.HasSuffix(name, "_id") {
				add(name)
			}
		}
	}
	if key == "id" {
		for name := range props {
			if strings.HasSuffix(name, "_id") {
				add(name)
			}
		}
	}
	if key == "time" || strings.HasSuffix(key, "_time") {
		add("at")
		for name := range props {
			if strings.HasSuffix(name, "_at") {
				add(name)
			}
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func coerceSchemaValue(value any, schema any) any {
	spec, ok := schema.(map[string]any)
	if !ok {
		return value
	}
	typ, _ := spec["type"].(string)
	switch typ {
	case "boolean":
		if text, ok := value.(string); ok {
			if parsed, err := strconv.ParseBool(strings.TrimSpace(text)); err == nil {
				return parsed
			}
			switch strings.TrimSpace(text) {
			case "1":
				return true
			case "0":
				return false
			}
		}
		if number, ok := value.(json.Number); ok {
			switch number.String() {
			case "1":
				return true
			case "0":
				return false
			}
		}
	case "integer":
		var text string
		switch typed := value.(type) {
		case string:
			text = typed
		case json.Number:
			text = typed.String()
		}
		if parsed, ok := parseSchemaInteger(text); ok {
			return parsed
		}
	case "number":
		if text, ok := value.(string); ok {
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
				return parsed
			}
		}
	case "string":
		switch typed := value.(type) {
		case json.Number:
			return typed.String()
		case bool:
			return strconv.FormatBool(typed)
		}
	case "object", "array":
		text, ok := value.(string)
		if !ok {
			break
		}
		var decoded any
		dec := json.NewDecoder(strings.NewReader(text))
		dec.UseNumber()
		if dec.Decode(&decoded) == nil {
			value = decoded
		}
	}
	if typ == "object" {
		object, ok := value.(map[string]any)
		if !ok {
			return value
		}
		return normalizeSchemaObject(object, spec)
	}
	if typ == "array" {
		items, ok := value.([]any)
		if !ok {
			return value
		}
		for i := range items {
			items[i] = coerceSchemaValue(items[i], spec["items"])
		}
		return items
	}
	return value
}

func parseSchemaInteger(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, false
	}
	if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
		return parsed, true
	}
	// Models occasionally serialize an integer schema value as "2.0". Rat
	// keeps the check exact, so large IDs are never rounded through float64.
	rational, ok := new(big.Rat).SetString(text)
	if !ok || !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, false
	}
	return rational.Num().Int64(), true
}
