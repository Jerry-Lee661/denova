package handlers

import (
	"context"
	"testing"

	"denova/config"
)

func TestBuildRetryableToolByName(t *testing.T) {
	cfg := &config.Config{Workspace: t.TempDir()}
	ctx := context.Background()
	for _, name := range []string{"glob", "grep", "read", "count_words"} {
		if _, err := buildRetryableToolByName(ctx, cfg, name); err != nil {
			t.Errorf("tool %q should be retryable: %v", name, err)
		}
	}
	// Session-bound toolsets must not be reachable through standalone retry.
	if _, err := buildRetryableToolByName(ctx, cfg, "ask"); err == nil {
		t.Error("session-bound tools must not be retryable")
	}
	if _, err := buildRetryableToolByName(ctx, cfg, "no_such_tool"); err == nil {
		t.Error("unknown tool should return an error")
	}
}
