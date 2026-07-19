// Package vectorstore defines the rebuildable semantic-index boundary.
// PostgreSQL remains authoritative; implementations only store vectors and
// non-sensitive routing metadata needed to find source entity IDs.
package vectorstore

import "context"

const (
	PayloadSource            = "source"
	PayloadEntityID          = "entity_id"
	PayloadKind              = "kind"
	PayloadContentHash       = "content_hash"
	PayloadSessionUser       = "session_user_id"
	PayloadChannel           = "channel_scope"
	PayloadConversationScope = "conversation_scope"
	PayloadRole              = "role"
)

// Ref identifies one authoritative record without depending on its mutable
// display name.
type Ref struct {
	Source   string
	EntityID string
}

func (r Ref) Key() string { return r.Source + "\x00" + r.EntityID }

// Point is one vector plus routing metadata. Payload must not contain secrets
// or full source content; callers re-read the authoritative row after search.
type Point struct {
	Ref
	Vector      []float32
	ContentHash string
	Payload     map[string]any
}

// Filter is translated into vector-database payload predicates. Values may be
// string, bool, int64, []string, or []int64. Slice values mean match-any.
type Filter struct {
	Must    map[string]any
	MustNot map[string]any
}

// Hit contains only a stable source reference and score. The source database
// must still apply row and field permissions before any content is returned.
type Hit struct {
	Ref
	Score float32
}

// Metadata is used by reconciliation to skip unchanged vectors and remove
// points whose authoritative row has been deleted.
type Metadata struct {
	Ref
	ContentHash string
}

// Store is the low-level vector index. modelTag includes the embedding model
// and dimension; implementations may map each tag to a separate collection.
type Store interface {
	Upsert(ctx context.Context, modelTag string, points []Point) error
	Search(ctx context.Context, modelTag string, vector []float32, filter Filter, limit int, minScore float32) ([]Hit, error)
	Hashes(ctx context.Context, modelTag string, dimension int, refs []Ref) (map[string]string, error)
	List(ctx context.Context, modelTag string, dimension int, source string) ([]Metadata, error)
	Delete(ctx context.Context, modelTag string, dimension int, refs []Ref) error
	Ping(ctx context.Context) error
	Close() error
}
