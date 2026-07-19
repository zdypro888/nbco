package tools

import (
	"strings"
	"testing"
)

func TestValidatePublicURL(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd",
		"http://localhost/admin",
		"http://127.0.0.1/admin",
		"http://10.0.0.1/",
		"https://user:pass@example.com/",
	} {
		if _, err := validatePublicURL(raw); err == nil {
			t.Fatalf("validatePublicURL(%q) should fail", raw)
		}
	}
	if got, err := validatePublicURL("https://example.com/docs?q=1"); err != nil || got.Hostname() != "example.com" {
		t.Fatalf("public URL = %v, %v", got, err)
	}
}

func TestReadableHTML(t *testing.T) {
	title, text, err := readableHTML([]byte(`<html><head><title> Demo </title><style>hidden</style></head><body><h1>Hello</h1><p>World &amp; team</p><script>ignore()</script></body></html>`))
	if err != nil {
		t.Fatal(err)
	}
	if title != "Demo" || !strings.Contains(text, "Hello") || !strings.Contains(text, "World & team") {
		t.Fatalf("title=%q text=%q", title, text)
	}
	if strings.Contains(text, "hidden") || strings.Contains(text, "ignore") {
		t.Fatalf("hidden content leaked: %q", text)
	}
}
