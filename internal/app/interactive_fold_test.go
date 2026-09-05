package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/agent"
	"denova/internal/interactive"
)

func messageContents(messages []*schema.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		contents = append(contents, msg.Content)
	}
	return contents
}

func newFoldTestStory(t *testing.T, store *interactive.Store) interactive.StorySummary {
	t.Helper()
	story, err := store.CreateStory(interactive.CreateStoryRequest{
		Title:            "折叠测试",
		Origin:           "主角进入旧城",
		StoryTellerID:    "classic",
		ReplyTargetChars: 700,
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		if _, err := store.AppendTurn(story.ID, interactive.AppendTurnRequest{
			User:      fmt.Sprintf("第%d次行动", i),
			Narrative: fmt.Sprintf("第%d段剧情", i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return story
}

func TestInteractiveFoldProjectsSummaryAndRetainedTail(t *testing.T) {
	workspace := t.TempDir()
	novaDir := t.TempDir()
	store := interactive.NewStore(workspace)
	story := newFoldTestStory(t, store)

	restore := agent.StubSummarizeContextForCompactionForTest("折叠摘要：前几轮已合并。", 300)
	defer restore()

	cfg := &config.Config{}
	conversation := newInteractiveConversation(store, novaDir, workspace, story.ID, "", "我继续探索", story.ReplyTargetChars, cfg)
	history, err := conversation.PrepareMessages("我继续探索", "我继续探索")
	if err != nil {
		t.Fatal(err)
	}

	// Fold the first 5 turns into a summary, retaining the tail.
	_, result, err := conversation.FoldContextIfNeeded(context.Background(), agent.ContextCompactionInput{
		Messages: history,
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered {
		t.Fatalf("expected fold to trigger: %#v", result)
	}
	if strings.TrimSpace(result.Summary) == "" {
		t.Fatal("fold should produce a non-empty summary")
	}

	// Reload the snapshot and project through PrepareMessages.
	reloadedConv := newInteractiveConversation(store, novaDir, workspace, story.ID, "", "我继续探索", story.ReplyTargetChars, cfg)
	projected, err := reloadedConv.PrepareMessages("我继续探索", "我继续探索")
	if err != nil {
		t.Fatal(err)
	}
	// Expect: fold summary placeholder is injected into the model-visible history.
	foundSummary := false
	for _, msg := range projected {
		if strings.Contains(msg.Content, "[Denova Context Fold]") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatalf("projected history should include fold summary: %#v", messageContents(projected))
	}
	// The retained tail turn (第6次行动) must survive the projection.
	foundTail := false
	for _, msg := range projected {
		if strings.Contains(msg.Content, "第6次行动") {
			foundTail = true
		}
	}
	if !foundTail {
		t.Fatalf("retained tail turn should be present: %#v", messageContents(projected))
	}
}

func TestInteractiveFoldOmitsFoldedTurnsWithNonZeroSource(t *testing.T) {
	workspace := t.TempDir()
	novaDir := t.TempDir()
	store := interactive.NewStore(workspace)
	story := newFoldTestStory(t, store)

	// Directly append a fold with a non-zero SourceTurnCount so the projection
	// omits turns before it and retains the tail.
	if _, err := store.AppendContextFold(story.ID, "main", interactive.ContextFoldEvent{
		AgentKind:       config.AgentKindInteractiveStory,
		Summary:         "折叠摘要：前几轮已合并。",
		SourceTurnCount: 3,
		RetainedTurns:   1,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	conversation := newInteractiveConversation(store, novaDir, workspace, story.ID, "", "我继续探索", story.ReplyTargetChars, cfg)
	projected, err := conversation.PrepareMessages("我继续探索", "我继续探索")
	if err != nil {
		t.Fatal(err)
	}
	foundSummary := false
	for _, msg := range projected {
		if strings.Contains(msg.Content, "[Denova Context Fold]") {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatalf("projected history should include fold summary: %#v", messageContents(projected))
	}
	// Turn 1 (folded, before retained tail) must be omitted; turn 6 (retained
	// tail) must survive.
	foundOmitted := false
	foundTail := false
	for _, msg := range projected {
		if strings.Contains(msg.Content, "第1次行动") {
			foundOmitted = true
		}
		if strings.Contains(msg.Content, "第6次行动") {
			foundTail = true
		}
	}
	if foundOmitted {
		t.Fatalf("folded turn should be omitted: %#v", messageContents(projected))
	}
	if !foundTail {
		t.Fatalf("retained tail turn should be present: %#v", messageContents(projected))
	}
}

func TestInteractiveFoldSkipsWhenCompactionActive(t *testing.T) {
	workspace := t.TempDir()
	novaDir := t.TempDir()
	store := interactive.NewStore(workspace)
	story := newFoldTestStory(t, store)

	if _, err := store.AppendContextCompaction(story.ID, "main", interactive.ContextCompactionEvent{
		AgentKind:       config.AgentKindInteractiveStory,
		Summary:         "压缩摘要：主角已进入旧城。",
		SourceTurnCount: 6,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	conversation := newInteractiveConversation(store, novaDir, workspace, story.ID, "", "我继续探索", story.ReplyTargetChars, cfg)
	history, err := conversation.PrepareMessages("我继续探索", "我继续探索")
	if err != nil {
		t.Fatal(err)
	}

	_, result, err := conversation.FoldContextIfNeeded(context.Background(), agent.ContextCompactionInput{
		Messages: history,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Triggered {
		t.Fatalf("expected fold to skip with compaction active, got triggered: %#v", result)
	}
	if result.SkippedReason != "compaction_active" {
		t.Fatalf("expected compaction_active skip reason, got %q", result.SkippedReason)
	}
}

func TestInteractiveFoldRemovalRestoresRawHistory(t *testing.T) {
	workspace := t.TempDir()
	store := interactive.NewStore(workspace)
	story := newFoldTestStory(t, store)

	app := &App{
		interactive: store,
		cfg:         &config.Config{},
		workspace:   workspace,
	}
	service := &InteractiveAppService{app: app}

	// Seed an active fold so removal has something to soft-disable.
	if _, err := store.AppendContextFold(story.ID, "main", interactive.ContextFoldEvent{
		AgentKind:       config.AgentKindInteractiveStory,
		Summary:         "折叠摘要：前几轮已合并。",
		SourceTurnCount: 3,
		RetainedTurns:   1,
	}); err != nil {
		t.Fatal(err)
	}

	removed, err := service.RemoveInteractiveContextFold(story.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("expected fold removal to report a fold was present")
	}

	storyCtx, err := store.StoryContext(story.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	// Soft removal deactivates the fold in the projection but keeps the record.
	if storyCtx.Snapshot.ContextFold != nil {
		t.Fatalf("fold should be deactivated after removal: %#v", storyCtx.Snapshot.ContextFold)
	}
	if storyCtx.Snapshot.ContextFoldRemoval == nil {
		t.Fatal("fold removal event should be recorded")
	}

	// A second removal is idempotent.
	removed2, err := service.RemoveInteractiveContextFold(story.ID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if removed2 {
		t.Fatal("second removal should report no active fold")
	}
}
