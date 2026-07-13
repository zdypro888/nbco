package vectorstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	qdrant "github.com/qdrant/go-client/qdrant"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultQdrantPort = 6334
	qdrantPageSize    = 256
)

// QdrantConfig contains the transport-only settings needed by the official
// gRPC client. URL must target Qdrant's gRPC port (6334 by default).
type QdrantConfig struct {
	URL              string
	APIKey           string
	CollectionPrefix string
}

// Qdrant implements Store with the official Qdrant Go client. A physical
// collection is created per model+dimension tag, so changing embedding models
// never mixes incompatible vectors and does not require destructive migration.
type Qdrant struct {
	client *qdrant.Client
	prefix string

	ensureMu sync.Mutex
	ensured  map[string]bool
}

func NewQdrant(cfg QdrantConfig) (*Qdrant, error) {
	rawURL := strings.TrimSpace(cfg.URL)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, fmt.Errorf("qdrant url 必须是无路径和查询参数的 http/https gRPC 地址")
	}
	port := defaultQdrantPort
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("qdrant url 端口无效")
		}
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.CollectionPrefix), "_-")
	if prefix == "" {
		prefix = "nbco_semantic"
	}
	for _, r := range prefix {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return nil, fmt.Errorf("qdrant collection_prefix 只能包含字母、数字、下划线和连字符")
		}
	}
	client, err := qdrant.NewClient(&qdrant.Config{
		Host:                   parsed.Hostname(),
		Port:                   port,
		APIKey:                 strings.TrimSpace(cfg.APIKey),
		UseTLS:                 parsed.Scheme == "https",
		PoolSize:               1,
		SkipCompatibilityCheck: true, // client v1.18.x cannot read its own module version under Go 1.26
		RetryConfig:            &qdrant.RetryConfig{MaxRetries: 2},
	})
	if err != nil {
		return nil, fmt.Errorf("构建 qdrant 客户端: %w", err)
	}
	return &Qdrant{client: client, prefix: prefix, ensured: make(map[string]bool)}, nil
}

func (q *Qdrant) Close() error { return q.client.Close() }

func (q *Qdrant) Ping(ctx context.Context) error {
	_, err := q.client.HealthCheck(ctx)
	return err
}

func (q *Qdrant) Upsert(ctx context.Context, modelTag string, points []Point) error {
	if len(points) == 0 {
		return nil
	}
	dim := len(points[0].Vector)
	if dim == 0 {
		return fmt.Errorf("qdrant upsert: 空向量")
	}
	for _, point := range points {
		if strings.TrimSpace(point.Source) == "" || strings.TrimSpace(point.EntityID) == "" {
			return fmt.Errorf("qdrant upsert: source/entity_id 必填")
		}
		if len(point.Vector) != dim {
			return fmt.Errorf("qdrant upsert: 批次向量维度不一致")
		}
		if err := validQdrantVector(point.Vector); err != nil {
			return fmt.Errorf("qdrant upsert %s: %w", point.Ref.Key(), err)
		}
	}
	collection, err := q.ensure(ctx, modelTag, dim)
	if err != nil {
		return err
	}
	qpoints := make([]*qdrant.PointStruct, 0, len(points))
	for _, point := range points {
		payload := make(map[string]any, len(point.Payload)+3)
		for key, value := range point.Payload {
			payload[key] = value
		}
		payload[PayloadSource] = point.Source
		payload[PayloadEntityID] = point.EntityID
		payload[PayloadContentHash] = point.ContentHash
		valueMap, err := payloadValueMap(payload)
		if err != nil {
			return fmt.Errorf("qdrant upsert payload %s: %w", point.Ref.Key(), err)
		}
		qpoints = append(qpoints, &qdrant.PointStruct{
			Id:      qdrant.NewIDUUID(pointUUID(point.Ref)),
			Vectors: qdrant.NewVectors(point.Vector...),
			Payload: valueMap,
		})
	}
	wait := true
	_, err = q.client.Upsert(ctx, &qdrant.UpsertPoints{CollectionName: collection, Points: qpoints, Wait: &wait})
	if err != nil {
		return fmt.Errorf("qdrant upsert %s: %w", collection, err)
	}
	return nil
}

func payloadValueMap(payload map[string]any) (map[string]*qdrant.Value, error) {
	normalized := make(map[string]any, len(payload))
	for key, value := range payload {
		normalized[key] = normalizePayloadValue(value)
	}
	return qdrant.TryValueMap(normalized)
}

func normalizePayloadValue(value any) any {
	switch values := value.(type) {
	case []string:
		out := make([]any, len(values))
		for i := range values {
			out[i] = values[i]
		}
		return out
	case []any:
		out := make([]any, len(values))
		for i := range values {
			out[i] = normalizePayloadValue(values[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(values))
		for key, item := range values {
			out[key] = normalizePayloadValue(item)
		}
		return out
	default:
		return value
	}
}

func (q *Qdrant) Search(ctx context.Context, modelTag string, vector []float32, filter Filter, limit int, minScore float32) ([]Hit, error) {
	if len(vector) == 0 {
		return nil, nil
	}
	if err := validQdrantVector(vector); err != nil {
		return nil, fmt.Errorf("qdrant query: %w", err)
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 500 {
		limit = 500
	}
	collection, err := q.ensure(ctx, modelTag, len(vector))
	if err != nil {
		return nil, err
	}
	qfilter, err := buildFilter(filter)
	if err != nil {
		return nil, err
	}
	qLimit := uint64(limit)
	request := &qdrant.QueryPoints{
		CollectionName: collection,
		Query:          qdrant.NewQuery(vector...),
		Filter:         qfilter,
		Limit:          &qLimit,
		WithPayload:    qdrant.NewWithPayloadInclude(PayloadSource, PayloadEntityID),
		WithVectors:    qdrant.NewWithVectors(false),
	}
	if minScore > 0 {
		request.ScoreThreshold = &minScore
	}
	points, err := q.client.Query(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("qdrant query %s: %w", collection, err)
	}
	out := make([]Hit, 0, len(points))
	for _, point := range points {
		ref, ok := refFromPayload(point.Payload)
		if !ok {
			continue
		}
		out = append(out, Hit{Ref: ref, Score: point.Score})
	}
	return out, nil
}

func validQdrantVector(vector []float32) error {
	for i, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return fmt.Errorf("向量第 %d 维不是有限数", i)
		}
	}
	return nil
}

func (q *Qdrant) Hashes(ctx context.Context, modelTag string, dimension int, refs []Ref) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	collection, err := q.ensure(ctx, modelTag, dimension)
	if err != nil {
		return nil, err
	}
	ids := make([]*qdrant.PointId, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, qdrant.NewIDUUID(pointUUID(ref)))
	}
	points, err := q.client.Get(ctx, &qdrant.GetPoints{
		CollectionName: collection,
		Ids:            ids,
		WithPayload:    qdrant.NewWithPayloadInclude(PayloadSource, PayloadEntityID, PayloadContentHash),
		WithVectors:    qdrant.NewWithVectors(false),
	})
	if err != nil {
		return nil, fmt.Errorf("qdrant get %s: %w", collection, err)
	}
	for _, point := range points {
		ref, ok := refFromPayload(point.Payload)
		if !ok {
			continue
		}
		out[ref.Key()] = payloadString(point.Payload, PayloadContentHash)
	}
	return out, nil
}

func (q *Qdrant) List(ctx context.Context, modelTag string, dimension int, source string) ([]Metadata, error) {
	collection, err := q.ensure(ctx, modelTag, dimension)
	if err != nil {
		return nil, err
	}
	filter, err := buildFilter(Filter{Must: map[string]any{PayloadSource: source}})
	if err != nil {
		return nil, err
	}
	limit := uint32(qdrantPageSize)
	var offset *qdrant.PointId
	var out []Metadata
	for {
		points, next, err := q.client.ScrollAndOffset(ctx, &qdrant.ScrollPoints{
			CollectionName: collection,
			Filter:         filter,
			Offset:         offset,
			Limit:          &limit,
			WithPayload:    qdrant.NewWithPayloadInclude(PayloadSource, PayloadEntityID, PayloadContentHash),
			WithVectors:    qdrant.NewWithVectors(false),
		})
		if err != nil {
			return nil, fmt.Errorf("qdrant scroll %s: %w", collection, err)
		}
		for _, point := range points {
			if ref, ok := refFromPayload(point.Payload); ok {
				out = append(out, Metadata{Ref: ref, ContentHash: payloadString(point.Payload, PayloadContentHash)})
			}
		}
		if next == nil || len(points) == 0 {
			break
		}
		offset = next
	}
	return out, nil
}

func (q *Qdrant) Delete(ctx context.Context, modelTag string, dimension int, refs []Ref) error {
	if len(refs) == 0 {
		return nil
	}
	collection, err := q.ensure(ctx, modelTag, dimension)
	if err != nil {
		return err
	}
	ids := make([]*qdrant.PointId, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, qdrant.NewIDUUID(pointUUID(ref)))
	}
	wait := true
	_, err = q.client.Delete(ctx, &qdrant.DeletePoints{
		CollectionName: collection,
		Points:         qdrant.NewPointsSelector(ids...),
		Wait:           &wait,
	})
	if err != nil {
		return fmt.Errorf("qdrant delete %s: %w", collection, err)
	}
	return nil
}

func (q *Qdrant) ensure(ctx context.Context, modelTag string, dimension int) (string, error) {
	if dimension <= 0 {
		return "", fmt.Errorf("qdrant collection: 向量维度必须大于 0")
	}
	collection := q.collectionName(modelTag)
	q.ensureMu.Lock()
	defer q.ensureMu.Unlock()
	if q.ensured[collection] {
		return collection, nil
	}
	exists, err := q.client.CollectionExists(ctx, collection)
	if err != nil {
		return "", fmt.Errorf("qdrant collection 探测: %w", err)
	}
	if !exists {
		if err := q.client.CreateCollection(ctx, &qdrant.CreateCollection{
			CollectionName: collection,
			VectorsConfig: qdrant.NewVectorsConfig(&qdrant.VectorParams{
				Size: uint64(dimension), Distance: qdrant.Distance_Cosine,
			}),
		}); err != nil && status.Code(err) != codes.AlreadyExists {
			return "", fmt.Errorf("创建 qdrant collection %s: %w", collection, err)
		}
	}
	if err := q.ensurePayloadIndexes(ctx, collection); err != nil {
		return "", err
	}
	q.ensured[collection] = true
	return collection, nil
}

func (q *Qdrant) ensurePayloadIndexes(ctx context.Context, collection string) error {
	fields := []struct {
		name  string
		type_ qdrant.FieldType
	}{
		{PayloadSource, qdrant.FieldType_FieldTypeKeyword},
		{PayloadEntityID, qdrant.FieldType_FieldTypeKeyword},
		{PayloadKind, qdrant.FieldType_FieldTypeKeyword},
		{"author_id", qdrant.FieldType_FieldTypeInteger},
		{PayloadSessionUser, qdrant.FieldType_FieldTypeInteger},
		{PayloadChannel, qdrant.FieldType_FieldTypeKeyword},
		{PayloadConversationScope, qdrant.FieldType_FieldTypeKeyword},
		{"tags", qdrant.FieldType_FieldTypeKeyword},
		{"pinned", qdrant.FieldType_FieldTypeBool},
	}
	wait := true
	for _, field := range fields {
		_, err := q.client.CreateFieldIndex(ctx, &qdrant.CreateFieldIndexCollection{
			CollectionName: collection,
			FieldName:      field.name,
			FieldType:      field.type_.Enum(),
			Wait:           &wait,
		})
		if err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("创建 qdrant payload index %s.%s: %w", collection, field.name, err)
		}
	}
	return nil
}

func (q *Qdrant) collectionName(modelTag string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(modelTag)))
	return q.prefix + "_" + hex.EncodeToString(sum[:6])
}

func pointUUID(ref Ref) string {
	sum := sha256.Sum256([]byte("nbco-vector-point\x00" + ref.Key()))
	b := sum[:16]
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func buildFilter(filter Filter) (*qdrant.Filter, error) {
	if len(filter.Must) == 0 && len(filter.MustNot) == 0 {
		return nil, nil
	}
	out := &qdrant.Filter{}
	for _, entry := range sortedFilterEntries(filter.Must) {
		condition, err := matchCondition(entry.key, entry.value)
		if err != nil {
			return nil, err
		}
		out.Must = append(out.Must, condition)
	}
	for _, entry := range sortedFilterEntries(filter.MustNot) {
		condition, err := matchCondition(entry.key, entry.value)
		if err != nil {
			return nil, err
		}
		out.MustNot = append(out.MustNot, condition)
	}
	return out, nil
}

type filterEntry struct {
	key   string
	value any
}

func sortedFilterEntries(values map[string]any) []filterEntry {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]filterEntry, 0, len(keys))
	for _, key := range keys {
		out = append(out, filterEntry{key: key, value: values[key]})
	}
	return out
}

func matchCondition(key string, value any) (*qdrant.Condition, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("qdrant filter 字段名不能为空")
	}
	switch value := value.(type) {
	case string:
		return qdrant.NewMatchKeyword(key, value), nil
	case bool:
		return qdrant.NewMatchBool(key, value), nil
	case int:
		return qdrant.NewMatchInt(key, int64(value)), nil
	case int64:
		return qdrant.NewMatchInt(key, value), nil
	case []string:
		if len(value) == 0 {
			return nil, fmt.Errorf("qdrant filter %s 的候选不能为空", key)
		}
		return qdrant.NewMatchKeywords(key, value...), nil
	case []int64:
		if len(value) == 0 {
			return nil, fmt.Errorf("qdrant filter %s 的候选不能为空", key)
		}
		return qdrant.NewMatchInts(key, value...), nil
	default:
		return nil, fmt.Errorf("qdrant filter %s 不支持 %T", key, value)
	}
}

func refFromPayload(payload map[string]*qdrant.Value) (Ref, bool) {
	ref := Ref{
		Source:   payloadString(payload, PayloadSource),
		EntityID: payloadString(payload, PayloadEntityID),
	}
	return ref, ref.Source != "" && ref.EntityID != ""
}

func payloadString(payload map[string]*qdrant.Value, key string) string {
	if payload == nil || payload[key] == nil {
		return ""
	}
	return payload[key].GetStringValue()
}
