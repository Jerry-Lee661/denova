package imageanalysis

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoverStaleBatches(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)

	now := time.Now().UTC()

	// Create a "running" batch (simulates crash mid-processing).
	running := &BatchState{
		ID:         "running-batch",
		Status:     BatchRunning,
		CreatedAt:  now,
		StartedAt:  &now,
		TotalPages: 3,
		Results: []PageResult{
			{PageIndex: 0, FileName: "a.png", Status: "success", Content: "ok"},
			{PageIndex: 1, FileName: "b.png", Status: "processing"},
			{PageIndex: 2, FileName: "c.png", Status: "pending"},
		},
	}
	if err := store.Save(running); err != nil {
		t.Fatalf("save running batch: %v", err)
	}

	// Create a "pending" batch (simulates crash before processing started).
	pending := &BatchState{
		ID:         "pending-batch",
		Status:     BatchPending,
		CreatedAt:  now,
		TotalPages: 1,
		Results: []PageResult{
			{PageIndex: 0, FileName: "x.png", Status: "pending"},
		},
	}
	if err := store.Save(pending); err != nil {
		t.Fatalf("save pending batch: %v", err)
	}

	// Create a "completed" batch (should NOT be touched).
	completed := &BatchState{
		ID:         "completed-batch",
		Status:     BatchCompleted,
		CreatedAt:  now,
		TotalPages: 1,
		Results: []PageResult{
			{PageIndex: 0, FileName: "y.png", Status: "success", Content: "done"},
		},
	}
	if err := store.Save(completed); err != nil {
		t.Fatalf("save completed batch: %v", err)
	}

	// Run recovery.
	recovered := store.RecoverStaleBatches()
	if recovered != 2 {
		t.Fatalf("expected 2 recovered batches, got %d", recovered)
	}

	// Verify running batch is now failed with proper page statuses.
	got, err := store.Load("running-batch")
	if err != nil {
		t.Fatalf("load running batch: %v", err)
	}
	if got.Status != BatchFailed {
		t.Errorf("running batch status = %q, want %q", got.Status, BatchFailed)
	}
	if got.FinishedAt == nil {
		t.Error("running batch FinishedAt should be set")
	}
	if got.Results[0].Status != "success" {
		t.Errorf("page 0 status = %q, want success (should be preserved)", got.Results[0].Status)
	}
	if got.Results[1].Status != "failed" {
		t.Errorf("page 1 status = %q, want failed (was processing)", got.Results[1].Status)
	}
	if got.Results[2].Status != "failed" {
		t.Errorf("page 2 status = %q, want failed (was pending)", got.Results[2].Status)
	}

	// Verify pending batch is now failed.
	got, err = store.Load("pending-batch")
	if err != nil {
		t.Fatalf("load pending batch: %v", err)
	}
	if got.Status != BatchFailed {
		t.Errorf("pending batch status = %q, want %q", got.Status, BatchFailed)
	}

	// Verify completed batch is untouched.
	got, err = store.Load("completed-batch")
	if err != nil {
		t.Fatalf("load completed batch: %v", err)
	}
	if got.Status != BatchCompleted {
		t.Errorf("completed batch status = %q, want %q (should be untouched)", got.Status, BatchCompleted)
	}

	// Second recovery run should find nothing to recover.
	if n := store.RecoverStaleBatches(); n != 0 {
		t.Errorf("second recovery recovered %d, want 0", n)
	}
}

func TestRecoverStaleBatchesEmptyDir(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)

	// No analysis directory at all — should not error.
	if n := store.RecoverStaleBatches(); n != 0 {
		t.Errorf("recovered %d from empty workspace, want 0", n)
	}
}

func TestRecoverStaleBatchesMalformedState(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)

	// Create a valid batch first.
	valid := &BatchState{
		ID:         "valid",
		Status:     BatchRunning,
		CreatedAt:  time.Now().UTC(),
		TotalPages: 1,
		Results:    []PageResult{{PageIndex: 0, FileName: "a.png", Status: "pending"}},
	}
	if err := store.Save(valid); err != nil {
		t.Fatalf("save valid batch: %v", err)
	}

	// Create a malformed state.json alongside.
	badDir := filepath.Join(workspace, "assets", "analysis", "batch-broken")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "state.json"), []byte("{invalid json"), 0o644); err != nil {
		t.Fatalf("write bad state: %v", err)
	}

	// Should recover the valid one and skip the malformed one without error.
	if n := store.RecoverStaleBatches(); n != 1 {
		t.Errorf("recovered %d, want 1 (malformed should be skipped)", n)
	}
}
