package httpapi

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDataReadQueryFromRequest(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/data/chat_messages?q=weekly&q=report&id=7&id=9&filter.role=user&limit=80&offset=20", nil)
	r.SetPathValue("source", "chat_messages")
	query, err := dataReadQueryFromRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if query.Source != "chat_messages" || query.Limit != 80 || query.Offset != 20 ||
		!reflect.DeepEqual(query.Terms, []string{"weekly", "report"}) ||
		!reflect.DeepEqual(query.EntityIDs, []string{"7", "9"}) || query.Filters["role"] != "user" {
		t.Fatalf("query = %+v", query)
	}
}

func TestDataReadQueryRejectsAmbiguousOrUnboundedParameters(t *testing.T) {
	for _, rawURL := range []string{
		"/api/data/users?limit=101",
		"/api/data/users?offset=-1",
		"/api/data/users?filter.status=active&filter.status=disabled",
		"/api/data/users?filter.=active",
		"/api/data/users?unknown=value",
		"/api/data/users?q=1&q=2&q=3&q=4&q=5&q=6&q=7&q=8&q=9",
	} {
		r := httptest.NewRequest("GET", rawURL, nil)
		r.SetPathValue("source", "users")
		if _, err := dataReadQueryFromRequest(r); err == nil {
			t.Fatalf("expected invalid query error for %s", rawURL)
		}
	}
}

func TestVisibleDataSourcesDescribeStableIDSupport(t *testing.T) {
	for _, view := range visibleDataSourceViews(false) {
		if view.Name != "users" {
			continue
		}
		if view.StableIDField != "user_id" {
			t.Fatalf("users stable ID = %q", view.StableIDField)
		}
		return
	}
	t.Fatal("users data source is missing")
}

func TestIHTMLDataAPIsPublishPermissionAwareContract(t *testing.T) {
	joined := ""
	for _, spec := range nbcoIHTMLAPIs() {
		if strings.HasPrefix(spec.Path, "/api/data") {
			joined += spec.Path + " " + spec.Description
		}
	}
	for _, expected := range []string{"/api/data/sources", "/api/data/{source}", "稳定实体ID", "filter.<field>", "page_full", "权限"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("data API catalog is missing %q: %s", expected, joined)
		}
	}
}
