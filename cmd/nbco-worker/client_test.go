package main

import (
	"context"
	"encoding/json"
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

func TestClientRequestInput(t *testing.T) {
	var body struct {
		TaskID  int64  `json:"task_id"`
		ClaimID string `json:"claim_id"`
		Content string `json:"content"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/worker/request-input" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok-worker-a" {
			t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":"1"}`))
	}))
	defer srv.Close()

	err := newClient(srv.URL, "tok-worker-a").RequestInput(context.Background(), 42, "claim-1", "请提供 repo URL")
	if err != nil {
		t.Fatal(err)
	}
	if body.TaskID != 42 || body.ClaimID != "claim-1" || body.Content != "请提供 repo URL" {
		t.Fatalf("request body = %+v", body)
	}
}
