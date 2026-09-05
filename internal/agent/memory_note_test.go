package agent

import (
	"context"
	"testing"
	"time"

	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/session"
)

func TestFormatMemoryNoteRendersSections(t *testing.T) {
	note := session.MemoryNote{
		Title:       "本轮笔记",
		Goals:       "写第三章",
		Progress:    "大纲已定",
		Conclusions: "主角进入废城",
	}
	got := formatMemoryNote(note)
	want := "=== 本轮笔记 ===\n【目标】\n写第三章\n\n【进展】\n大纲已定\n\n【关键结论】\n主角进入废城"
	if got != want {
		t.Fatalf("formatMemoryNote = %q, want %q", got, want)
	}
}

func TestFormatMemoryNoteOmitsEmptySections(t *testing.T) {
	note := session.MemoryNote{Title: "本轮笔记", Goals: "只写目标"}
	got := formatMemoryNote(note)
	if got != "=== 本轮笔记 ===\n【目标】\n只写目标" {
		t.Fatalf("unexpected render: %q", got)
	}
}

func TestExtractSectionPullsTextUnderHeader(t *testing.T) {
	body := "【目标】\n写第三章\n\n【进展】\n大纲已定\n\n【关键结论】\n主角进入废城\n"
	if got := extractSection(body, "目标"); got != "写第三章" {
		t.Fatalf("goals = %q", got)
	}
	if got := extractSection(body, "进展"); got != "大纲已定" {
		t.Fatalf("progress = %q", got)
	}
	if got := extractSection(body, "关键结论"); got != "主角进入废城" {
		t.Fatalf("conclusions = %q", got)
	}
}

func TestParseMemoryNoteContentMapsFields(t *testing.T) {
	content := "【目标】\n写第三章\n\n【进展】\n大纲已定\n\n【关键结论】\n主角进入废城\n"
	note := parseMemoryNoteContent("ide", content)
	if note.Title != "本轮笔记" {
		t.Fatalf("title = %q", note.Title)
	}
	if note.Goals != "写第三章" || note.Progress != "大纲已定" || note.Conclusions != "主角进入废城" {
		t.Fatalf("unexpected parsed note: %#v", note)
	}
}

func TestBuildMemoryNoteInputPrependsExistingNotes(t *testing.T) {
	existing := []session.MemoryNote{{Title: "旧笔记", Goals: "旧目标"}}
	source := []*schema.Message{
		schema.UserMessage("新增用户"),
		schema.AssistantMessage("新增助手", nil),
	}
	input, total := buildMemoryNoteInput(existing, source)
	if input == "" {
		t.Fatal("expected non-empty input")
	}
	if total == 0 {
		t.Fatal("expected positive char count")
	}
}

func TestGenerateMemoryNoteFromModelEmptyInputReturnsEmptyNote(t *testing.T) {
	note, err := generateMemoryNoteFromModel(context.Background(), &config.Config{}, "ide", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if note.AgentKind != "ide" {
		t.Fatalf("agent kind = %q", note.AgentKind)
	}
}

func TestSessionConversationMemoryNoteSourceIncludesIncrementalMessages(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := sess.Append(schema.UserMessage("旧用户 " + string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
		if err := sess.Append(schema.AssistantMessage("旧助手 "+string(rune('0'+i)), nil)); err != nil {
			t.Fatal(err)
		}
	}
	conversation := NewSessionConversation(sess)
	source := conversation.memoryNoteSource()
	if len(source) != 4 {
		t.Fatalf("source len = %d, want 4: %#v", len(source), source)
	}
	if source[0].Content != "旧用户 1" || source[len(source)-1].Content != "旧助手 2" {
		t.Fatalf("unexpected source transcript: %#v", source)
	}
}

func TestSessionConversationMemoryNotesReferenceContextSeedsCompaction(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AppendMemoryNote(session.MemoryNote{AgentKind: "ide", Goals: "写第三章"}); err != nil {
		t.Fatal(err)
	}
	conversation := NewSessionConversation(sess)
	ref := conversation.memoryNotesReferenceContext()
	if ref == "" {
		t.Fatal("expected non-empty reference context from notes")
	}
}

func TestWriteMemoryNoteIfNeededPersistsNote(t *testing.T) {
	previous := generateMemoryNote
	defer func() { generateMemoryNote = previous }()
	generateMemoryNote = func(_ context.Context, _ *config.Config, agentKind string, _ []session.MemoryNote, _ []*schema.Message) (session.MemoryNote, error) {
		return session.MemoryNote{AgentKind: agentKind, Title: "本轮笔记", Goals: "增量目标"}, nil
	}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("本轮用户输入")); err != nil {
		t.Fatal(err)
	}
	conversation := NewSessionConversationForAgent(sess, &config.Config{}, config.AgentKindIDE)
	writeMemoryNoteIfNeeded(context.Background(), conversation, &config.Config{}, config.AgentKindIDE)
	// Poll for the persisted note: the writer goroutine appends after
	// generateMemoryNote returns, so `done` closing does not guarantee the
	// note is visible yet.
	var notes []session.MemoryNote
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		notes = sess.LatestMemoryNotes(config.AgentKindIDE, 0)
		if len(notes) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(notes) != 1 {
		t.Fatalf("expected 1 note, got %d", len(notes))
	}
	if notes[0].Goals != "增量目标" {
		t.Fatalf("unexpected note goals: %q", notes[0].Goals)
	}
}

func TestWriteMemoryNoteIfNeededSkipsWhenDisabled(t *testing.T) {
	done := make(chan struct{})
	previous := generateMemoryNote
	defer func() { generateMemoryNote = previous }()
	generateMemoryNote = func(_ context.Context, _ *config.Config, agentKind string, _ []session.MemoryNote, _ []*schema.Message) (session.MemoryNote, error) {
		defer close(done)
		return session.MemoryNote{AgentKind: agentKind, Goals: "增量目标"}, nil
	}
	disabled := false
	cfg := &config.Config{AgentContexts: config.AgentContextSettings{
		IDE: config.AgentContextOverride{MemoryNotesEnabled: &disabled},
	}}
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("本轮用户输入")); err != nil {
		t.Fatal(err)
	}
	conversation := NewSessionConversationForAgent(sess, cfg, config.AgentKindIDE)
	writeMemoryNoteIfNeeded(context.Background(), conversation, cfg, config.AgentKindIDE)
	select {
	case <-done:
		t.Fatal("note writer should not run when disabled")
	case <-time.After(500 * time.Millisecond):
	}
	notes := sess.LatestMemoryNotes(config.AgentKindIDE, 0)
	if len(notes) != 0 {
		t.Fatalf("expected no notes when disabled, got %d", len(notes))
	}
}
