package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestProductionBinaryUsesSharedIHTMLAgent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("list nbco dependencies: %v", err)
	}
	seenIHTMLCore := false
	for _, dependency := range strings.Fields(string(output)) {
		switch {
		case dependency == "github.com/zdypro888/ihtml":
			seenIHTMLCore = true
		case dependency == "github.com/zdypro888/ihtml/chat",
			strings.HasPrefix(dependency, "github.com/zdypro888/ihtml/chat/"):
			t.Fatalf("nbco must inject its shared Agent through ihtml.ChatBackend; linked standalone runtime %q", dependency)
		}
	}
	if !seenIHTMLCore {
		t.Fatal("nbco production binary no longer links the ihtml core integration")
	}
}
