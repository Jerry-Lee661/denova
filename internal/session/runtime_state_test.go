package session

import "testing"

// TestRuntimeStatePersistsAndResumes proves the durable runtime-state record
// survives a reload and can be replayed so resume inherits the same work site
// (Plan.md M11). Without it, a reloaded session looks identical on screen but
// the model inherits a different working context.
func TestRuntimeStatePersistsAndResumes(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}

	// Record the runtime state left behind by the previous turn.
	state := RuntimeState{
		AgentKind: "ide",
		Mode:      "writing",
		StoryID:   "",
		BranchID:  "",
		LastRead:  "chapters/ch00003.md",
		Settings:  map[string]string{"teller_id": "classic"},
	}
	record, err := sess.AppendRuntimeState(state)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID == "" {
		t.Fatal("runtime state should be assigned an id")
	}

	// Reload and verify the record survives.
	reloadedStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloadedStore.Get("default")
	if err != nil {
		t.Fatal(err)
	}

	got, ok := reloaded.LatestRuntimeState()
	if !ok {
		t.Fatal("expected reloaded runtime state")
	}
	if got.AgentKind != "ide" || got.Mode != "writing" || got.LastRead != "chapters/ch00003.md" {
		t.Fatalf("reloaded runtime state mismatch: %#v", got)
	}
	if got.Settings["teller_id"] != "classic" {
		t.Fatalf("reloaded runtime state settings mismatch: %#v", got.Settings)
	}

	// A later run records a new work site; resume must return the newest one.
	_, err = sess.AppendRuntimeState(RuntimeState{AgentKind: "automation", Mode: "batch", LastRead: "logs/run.log"})
	if err != nil {
		t.Fatal(err)
	}
	reloaded3, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess3, err := reloaded3.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	got2, ok := sess3.LatestRuntimeState()
	if !ok {
		t.Fatal("expected reloaded runtime state after second append")
	}
	if got2.AgentKind != "automation" {
		t.Fatalf("newest runtime state should win: %#v", got2)
	}
}

func TestLatestRuntimeStateEmptyForFreshSession(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sess.LatestRuntimeState(); ok {
		t.Fatal("fresh session should have no runtime state")
	}
}
