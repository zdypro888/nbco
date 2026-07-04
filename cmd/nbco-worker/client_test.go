package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientMe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/me" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok-worker-a" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":7,"name":"worker-a","is_superadmin":false,"is_worker":true,"owner_id":1}`))
	}))
	defer srv.Close()

	ident, err := newClient(srv.URL, "tok-worker-a").Me(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ident.ID != 7 || ident.Name != "worker-a" || !ident.IsWorker || ident.OwnerID == nil || *ident.OwnerID != 1 {
		t.Fatalf("identity = %+v", ident)
	}
}
