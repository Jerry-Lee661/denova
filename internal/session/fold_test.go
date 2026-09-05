package session

import (
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestContextFoldPersistsOutsideVisibleHistory(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("第一轮")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("第一答", nil)); err != nil {
		t.Fatal(err)
	}
	record, err := sess.AppendContextFold(ContextFold{
		AgentKind:        "ide",
		Summary:          "折叠摘要：旧对话已合并。",
		SourceStartIndex: 0,
		SourceEndIndex:   2,
		RetainedTurns:    8,
		TokensBefore:     900,
		TokensAfter:      120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(record.ID) == "" {
		t.Fatal("fold record should be assigned an id")
	}
	if len(sess.GetEffectiveMessages()) != 2 {
		t.Fatalf("fold must not alter effective raw messages: %#v", sess.GetEffectiveMessages())
	}
	if history := sess.History(); len(history) != 2 {
		t.Fatalf("fold must not appear in user-visible history: %#v", history)
	}

	reloadedStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloadedStore.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := reloaded.LatestContextFold("ide")
	if !ok {
		t.Fatal("expected reloaded fold record")
	}
	if latest.Summary != "折叠摘要：旧对话已合并。" || latest.SourceEndIndex != 2 {
		t.Fatalf("unexpected reloaded fold: %#v", latest)
	}
	if history := reloaded.History(); len(history) != 2 {
		t.Fatalf("reloaded visible history should stay raw: %#v", history)
	}
}

func TestContextFoldRemovalSoftDisablesActiveFold(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("第一轮")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("第一答", nil)); err != nil {
		t.Fatal(err)
	}
	record, err := sess.AppendContextFold(ContextFold{
		AgentKind:        "ide",
		Summary:          "旧摘要",
		SourceStartIndex: 0,
		SourceEndIndex:   2,
		RetainedTurns:    8,
	})
	if err != nil {
		t.Fatal(err)
	}
	removal, removed, err := sess.RemoveLatestContextFold("ide", "user_rejected")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected active fold to be removed")
	}
	if removal.FoldID != record.ID || removal.SourceEndIndex != record.SourceEndIndex {
		t.Fatalf("unexpected removal record: %#v for fold %#v", removal, record)
	}
	if _, ok := sess.LatestContextFold("ide"); ok {
		t.Fatal("removed fold should not be active")
	}
	if latestRemoval, ok := sess.LatestContextFoldRemoved("ide"); !ok || latestRemoval.FoldID != record.ID {
		t.Fatalf("expected latest removal for record %s, got %#v ok=%v", record.ID, latestRemoval, ok)
	}

	reloadedStore, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloadedStore.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.LatestContextFold("ide"); ok {
		t.Fatal("removed fold should stay inactive after reload")
	}
}

func TestContextFoldRespectsClearMarker(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("清理前用户")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("清理前助手", nil)); err != nil {
		t.Fatal(err)
	}
	if err := sess.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("清理后用户")); err != nil {
		t.Fatal(err)
	}
	// Fold over the post-clear message (index 2..3); it must stay active.
	_, err = sess.AppendContextFold(ContextFold{
		AgentKind:        "ide",
		Summary:          "折叠摘要",
		SourceStartIndex: 2,
		SourceEndIndex:   3,
		RetainedTurns:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := sess.LatestContextFold("ide")
	if !ok {
		t.Fatal("expected fold to stay active after clear")
	}
	if latest.SourceEndIndex != 3 {
		t.Fatalf("unexpected fold source end index: %#v", latest)
	}

	// A fold entirely before the clear marker is filtered out even if it is
	// the newest record; an older post-clear fold stays active.
	_, err = sess.AppendContextFold(ContextFold{
		AgentKind:        "ide",
		Summary:          "清除前摘要",
		SourceStartIndex: 0,
		SourceEndIndex:   2,
		RetainedTurns:    1,
	})
	if err != nil {
		t.Fatal(err)
	}
	latest2, ok := sess.LatestContextFold("ide")
	if !ok || latest2.SourceEndIndex != 3 {
		t.Fatalf("newest pre-clear fold skipped, older post-clear fold still active: %#v ok=%v", latest2, ok)
	}
}
