package vectorstore

import "testing"

func TestPointUUIDStableAndNamespaced(t *testing.T) {
	a := pointUUID(Ref{Source: "tasks", EntityID: "42"})
	b := pointUUID(Ref{Source: "tasks", EntityID: "42"})
	c := pointUUID(Ref{Source: "projects", EntityID: "42"})
	if a != b || a == c || len(a) != 36 {
		t.Fatalf("point UUID 不稳定或未按 source 隔离: %q %q %q", a, b, c)
	}
}

func TestNewQdrantValidatesURLWithoutConnecting(t *testing.T) {
	client, err := NewQdrant(QdrantConfig{URL: "http://127.0.0.1:6334/", CollectionPrefix: "nbco_test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"", "ftp://localhost:6334", "http://user:pass@localhost:6334", "http://localhost:6334/path"} {
		if client, err := NewQdrant(QdrantConfig{URL: raw}); err == nil {
			_ = client.Close()
			t.Fatalf("应拒绝 Qdrant URL %q", raw)
		}
	}
}

func TestBuildFilterSupportsTypedPredicates(t *testing.T) {
	filter, err := buildFilter(Filter{
		Must: map[string]any{
			PayloadSource: []string{"tasks", "projects"},
			"author_id":   int64(9),
			"pinned":      false,
		},
		MustNot: map[string]any{"kind": "policy"},
	})
	if err != nil || len(filter.Must) != 3 || len(filter.MustNot) != 1 {
		t.Fatalf("buildFilter = %+v, %v", filter, err)
	}
	if _, err := buildFilter(Filter{Must: map[string]any{"bad": 1.25}}); err == nil {
		t.Fatal("不支持的过滤值类型应报错")
	}
}
