package session

import (
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"
)

func TestMemoryNotePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	note, err := sess.AppendMemoryNote(MemoryNote{
		AgentKind:   "ide",
		Title:       "本轮笔记",
		Goals:       "写第三章",
		Progress:    "大纲已定",
		Conclusions: "主角进入废城",
	})
	if err != nil {
		t.Fatal(err)
	}
	if note.ID == "" {
		t.Fatal("note should be assigned an id")
	}
	if note.SourceEndIndex != 0 {
		t.Fatalf("source end index = %d, want 0", note.SourceEndIndex)
	}

	reloadedStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloadedStore.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	notes := reloaded.LatestMemoryNotes("ide", 0)
	if len(notes) != 1 {
		t.Fatalf("expected 1 reloaded note, got %d", len(notes))
	}
	got := notes[0]
	if got.AgentKind != "ide" || got.Goals != "写第三章" || got.Progress != "大纲已定" || got.Conclusions != "主角进入废城" {
		t.Fatalf("unexpected reloaded note: %#v", got)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("reloaded note should keep created at")
	}
}

func TestLatestMemoryNotesFiltersByAgentKind(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendMemoryNote(MemoryNote{AgentKind: "ide", Goals: "IDE goal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendMemoryNote(MemoryNote{AgentKind: "interactive_story", Goals: "Story goal"}); err != nil {
		t.Fatal(err)
	}
	notes := sess.LatestMemoryNotes("interactive_story", 0)
	if len(notes) != 1 {
		t.Fatalf("expected 1 story note, got %d", len(notes))
	}
	if notes[0].Goals != "Story goal" {
		t.Fatalf("unexpected note: %#v", notes[0])
	}
}

func TestLatestMemoryNotesBoundedByDefault(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < DefaultMemoryNotesLimit+5; i++ {
		if _, err := sess.AppendMemoryNote(MemoryNote{AgentKind: "ide", Title: "note"}); err != nil {
			t.Fatal(err)
		}
	}
	notes := sess.LatestMemoryNotes("ide", 0)
	if len(notes) > DefaultMemoryNotesLimit {
		t.Fatalf("expected at most %d notes, got %d", DefaultMemoryNotesLimit, len(notes))
	}
}

func TestMemoryNoteClearMarkerDropsStale(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("旧消息")); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendMemoryNote(MemoryNote{AgentKind: "ide", Goals: "stale"}); err != nil {
		t.Fatal(err)
	}
	// A message after the stale note pushes clearAfterIndex past its source end.
	if err := sess.Append(schema.UserMessage("更旧的消息")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendMemoryNote(MemoryNote{AgentKind: "ide", Goals: "fresh"}); err != nil {
		t.Fatal(err)
	}
	notes := sess.LatestMemoryNotes("ide", 0)
	if len(notes) != 1 {
		t.Fatalf("expected 1 fresh note after clear, got %d", len(notes))
	}
	if notes[0].Goals != "fresh" {
		t.Fatalf("unexpected note: %#v", notes[0])
	}
	if notes[0].Goals != "fresh" {
		t.Fatalf("unexpected note: %#v", notes[0])
	}
}

func TestMemoryNoteCreatedAtDefaultsToUpdatedAt(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	if _, err := sess.AppendMemoryNote(MemoryNote{AgentKind: "ide"}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UTC()
	notes := sess.LatestMemoryNotes("ide", 0)
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(notes))
	}
	if notes[0].CreatedAt.Before(before.Add(-time.Second)) || notes[0].CreatedAt.After(after.Add(time.Second)) {
		t.Fatalf("created at %v out of expected range", notes[0].CreatedAt)
	}
}
