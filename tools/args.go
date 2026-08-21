package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"sync"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/zdypro888/nbco/ai"
)

// withArgumentNormalization makes tool invocation tolerant of harmless model
// serialization drift and rejects structurally invalid calls before approval,
// audit, or business handlers run. The tool's JSON Schema remains the source of truth:
// aliases are accepted only when they resolve to exactly one declared field,
// and values are coerced only to the declared primitive type.
func withArgumentNormalization(t ai.Tool) ai.Tool {
	inner := t.Handler
	schema := t.InputSchema
	validator, compileErr := compileToolSchema(t.Name, schema)
	normalize := func(raw json.RawMessage) json.RawMessage {
		return normalizeToolArgs(raw, schema)
	}
	t.NormalizeInput = normalize
	t.Handler = func(ctx context.Context, raw json.RawMessage) (string, error) {
		normalized := normalize(raw)
		if compileErr != nil {
			return "", fmt.Errorf("工具 %s 的输入 schema 无效: %w", t.Name, compileErr)
		}
		if err := validateToolArgs(normalized, validator); err != nil {
			return invalidToolArgumentsResult(err), nil
		}
		return inner(ctx, normalized)
	}
	return t
}

func compileToolSchema(name string, schema map[string]any) (*jsonschema.Schema, error) {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	key := toolSchemaCacheKey{Name: name, Hash: sha256.Sum256(raw)}
	if entry, ok := toolSchemaCache.Load(key); ok {
		return entry.Schema, entry.Err
	}
	var document any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	location := "nbco://tools/" + name + "/input.json"
	if err := compiler.AddResource(location, document); err != nil {
		toolSchemaCache.Store(key, toolSchemaCacheEntry{Err: err})
		return nil, err
	}
	compiled, err := compiler.Compile(location)
	toolSchemaCache.Store(key, toolSchemaCacheEntry{Schema: compiled, Err: err})
	return compiled, err
}

type toolSchemaCacheKey struct {
	Name string
	Hash [sha256.Size]byte
}

type toolSchemaCacheEntry struct {
	Schema *jsonschema.Schema
	Err    error
}

const toolSchemaCacheLimit = 2048

type boundedToolSchemaCache struct {
	mu      sync.Mutex
	entries map[toolSchemaCacheKey]toolSchemaCacheEntry
	order   []toolSchemaCacheKey
	limit   int
}

func newBoundedToolSchemaCache(limit int) *boundedToolSchemaCache {
	if limit <= 0 {
		limit = 1
	}
	return &boundedToolSchemaCache{
		entries: make(map[toolSchemaCacheKey]toolSchemaCacheEntry, limit),
		order:   make([]toolSchemaCacheKey, 0, limit),
		limit:   limit,
	}
}

func (c *boundedToolSchemaCache) Load(key toolSchemaCacheKey) (toolSchemaCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok
}

func (c *boundedToolSchemaCache) Store(key toolSchemaCacheKey, entry toolSchemaCacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	if len(c.order) == c.limit {
		delete(c.entries, c.order[0])
		copy(c.order, c.order[1:])
		c.order = c.order[:len(c.order)-1]
	}
	c.entries[key] = entry
	c.order = append(c.order, key)
}

var toolSchemaCache = newBoundedToolSchemaCache(toolSchemaCacheLimit)

func validateToolArgs(raw json.RawMessage, schema *jsonschema.Schema) error {
	if schema == nil {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("参数包含多个 JSON 值")
	}
	if err := schema.Validate(value); err != nil {
		return err
	}
	return nil
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
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		// Preserve the original payload so validateToolArgs rejects trailing JSON.
		// Normalization must never turn an invalid multi-value input into a valid call.
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
