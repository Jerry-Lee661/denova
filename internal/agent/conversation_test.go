package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/session"
)

func TestSessionConversationKeepsFullEffectiveHistoryBeforeCompaction(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		if err := sess.Append(schema.UserMessage("user " + string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
		if err := sess.Append(schema.AssistantMessage("assistant "+string(rune('0'+i)), nil)); err != nil {
			t.Fatal(err)
		}
	}
	conversation := NewSessionConversation(sess)
	history, err := conversation.PrepareMessages("user 5", "agent user 5")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 9 {
		t.Fatalf("history length = %d, want 9", len(history))
	}
	want := []string{
		"user 1", "assistant 1",
		"user 2", "assistant 2",
		"user 3", "assistant 3",
		"user 4", "assistant 4",
		"agent user 5",
	}
	for i := range want {
		if history[i].Content != want[i] {
			t.Fatalf("history[%d] = %q, want %q; all=%#v", i, history[i].Content, want[i], history)
		}
	}
}

func TestSessionConversationPersistsUserMessageReferencesOutsideModelContent(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	conversation := NewSessionConversation(sess)
	conversation.SetUserMessageReferences([]session.UserMessageReference{
		{Kind: "file", Label: "chapters/ch01.md"},
		{Kind: "review_comment", ID: "comment-1", Label: "setting/progress.md", Detail: "需要增加爽点"},
	})

	history, err := conversation.PrepareMessages("请统一修改", "请统一修改")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Content != "请统一修改" {
		t.Fatalf("display references must not be injected into model content: %#v", history)
	}
	visible := sess.History()
	if len(visible) != 1 || len(visible[0].UserReferences) != 2 {
		t.Fatalf("user references were not persisted: %#v", visible)
	}
	if visible[0].UserReferences[1].Detail != "需要增加爽点" {
		t.Fatalf("review comment display detail was lost: %#v", visible[0].UserReferences)
	}
}

func TestSessionConversationPrependsDynamicContextInsideFinalUserMessageOnly(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("旧用户请求")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("旧助手回复", nil)); err != nil {
		t.Fatal(err)
	}

	conversation := NewSessionConversationForAgentWithRuntimeContext(
		sess,
		&config.Config{},
		config.AgentKindIDE,
		"本轮动态作品状态",
		"## 大纲\n\n主角进入废城。",
	)
	history, err := conversation.PrepareMessages("继续写", "继续写")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history length = %d, want 3: %#v", len(history), history)
	}
	final := history[len(history)-1].Content
	dynamicIndex := strings.Index(final, "# 本轮动态作品状态")
	requestIndex := strings.Index(final, "# 本轮用户请求（最高优先级）")
	if dynamicIndex < 0 || requestIndex < 0 || dynamicIndex >= requestIndex {
		t.Fatalf("final model message should place dynamic context before the current request:\n%s", final)
	}
	if !strings.Contains(final, "主角进入废城") || !strings.HasSuffix(strings.TrimSpace(final), "继续写") {
		t.Fatalf("final model message missing dynamic state or bottom request:\n%s", final)
	}
	visible := sess.History()
	if got := visible[len(visible)-1].Content; got != "继续写" {
		t.Fatalf("visible session history should keep original user message, got %q", got)
	}
	if sources := conversation.ContextSourceSummary(); !strings.Contains(sources, "本轮动态上下文") || !strings.Contains(sources, "prepended_to_final_user_message") {
		t.Fatalf("runtime context source summary missing dynamic context: %s", sources)
	}
}

func TestSessionConversationPrependsStableContextBeforeHistory(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("旧用户请求")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("旧助手回复", nil)); err != nil {
		t.Fatal(err)
	}

	conversation := NewSessionConversationForAgentWithRuntimeContexts(
		sess,
		&config.Config{},
		config.AgentKindIDE,
		"稳定作品上下文",
		"## 当前大纲\n\n主角进入废城。",
		"本轮动态作品状态",
		"## 当前进度\n\n刚抵达废城。",
	)
	history, err := conversation.PrepareMessages("继续写", "继续写")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("history length = %d, want 4: %#v", len(history), history)
	}
	if !strings.Contains(history[0].Content, "# 稳定作品上下文") || !strings.Contains(history[0].Content, "主角进入废城") {
		t.Fatalf("first model message should be stable context: %s", history[0].Content)
	}
	if history[1].Content != "旧用户请求" || history[2].Content != "旧助手回复" {
		t.Fatalf("stable context should precede persisted history: %#v", messageContents(history))
	}
	if !strings.Contains(history[3].Content, "# 本轮动态作品状态") || !strings.HasSuffix(strings.TrimSpace(history[3].Content), "继续写") {
		t.Fatalf("final model message should contain dynamic context then request: %s", history[3].Content)
	}
	if visible := sess.History(); len(visible) != 3 || visible[2].Content != "继续写" {
		t.Fatalf("visible session history should only include raw user request: %#v", visible)
	}
	if sources := conversation.ContextSourceSummary(); !strings.Contains(sources, "prepended_to_model_messages") || !strings.Contains(sources, "prepended_to_final_user_message") {
		t.Fatalf("runtime context source summary missing stable/dynamic locations: %s", sources)
	}
}

func TestSessionConversationKeepsStableContextBeforeCompactionSummary(t *testing.T) {
	previous := summarizeContextForCompaction
	defer func() { summarizeContextForCompaction = previous }()
	summarizeContextForCompaction = func(_ context.Context, _ *config.Config, _ string, _ string, _ []*schema.Message, _ string, _ int, _ contextCompactionPolicy, _ func(int, string)) (string, int, error) {
		return "压缩摘要：旧对话已合并。", 100, nil
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("旧用户请求")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("旧助手回复", nil)); err != nil {
		t.Fatal(err)
	}
	conversation := NewSessionConversationForAgentWithRuntimeContexts(
		sess,
		&config.Config{},
		config.AgentKindIDE,
		"稳定作品上下文",
		"## 当前大纲\n\n主角进入废城。",
		"本轮动态作品状态",
		"## 当前进度\n\n刚抵达废城。",
	)
	history, err := conversation.PrepareMessages("继续写", "继续写")
	if err != nil {
		t.Fatal(err)
	}
	compacted, result, err := conversation.CompactContextIfNeeded(context.Background(), ContextCompactionInput{
		Messages: history,
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered {
		t.Fatalf("expected compaction to trigger: %#v", result)
	}
	if len(compacted) < 3 {
		t.Fatalf("compacted messages too short: %#v", compacted)
	}
	if !strings.Contains(compacted[0].Content, "# 稳定作品上下文") {
		t.Fatalf("stable context should remain first after compaction: %#v", messageContents(compacted))
	}
	if !isContextCompactionMessage(compacted[1]) {
		t.Fatalf("compaction summary should follow stable context: %#v", messageContents(compacted))
	}
}

func TestSessionConversationCompactsOnlyMessagesAfterPreviousCompaction(t *testing.T) {
	previous := summarizeContextForCompaction
	defer func() { summarizeContextForCompaction = previous }()

	var capturedExisting string
	var capturedSource []*schema.Message
	summarizeContextForCompaction = func(_ context.Context, _ *config.Config, _ string, existingCheckpoint string, source []*schema.Message, _ string, _ int, _ contextCompactionPolicy, _ func(int, string)) (string, int, error) {
		capturedExisting = existingCheckpoint
		capturedSource = source
		return "新压缩摘要：旧目标与新增进展都已合并。", 200, nil
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	messages := []*schema.Message{
		schema.UserMessage("已压缩用户 1"),
		schema.AssistantMessage("已压缩助手 1", nil),
		schema.UserMessage("新增用户 2"),
		schema.AssistantMessage("新增助手 2", nil),
	}
	for _, msg := range messages {
		if err := sess.Append(msg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sess.AppendContextCompaction(session.ContextCompaction{
		AgentKind:        config.AgentKindIDE,
		Epoch:            1,
		Summary:          "旧压缩摘要：用户 1 已处理。",
		SourceStartIndex: 0,
		SourceEndIndex:   2,
		RetainedTurns:    1,
	}); err != nil {
		t.Fatal(err)
	}

	conversation := NewSessionConversationForAgent(sess, &config.Config{}, config.AgentKindIDE)
	_, result, err := conversation.CompactContextIfNeeded(context.Background(), ContextCompactionInput{
		Messages:       sess.GetEffectiveMessages(),
		Force:          true,
		KeepLatestUser: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered {
		t.Fatalf("expected compaction to trigger: %#v", result)
	}
	if capturedExisting != "旧压缩摘要：用户 1 已处理。" {
		t.Fatalf("existing memory = %q", capturedExisting)
	}
	got := messageContents(capturedSource)
	want := []string{"新增用户 2", "新增助手 2"}
	if len(got) != len(want) {
		t.Fatalf("source len = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("source[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
	if record, ok := sess.LatestContextCompaction(config.AgentKindIDE); !ok || record.SourceStartIndex != 2 || record.SourceEndIndex != 4 {
		t.Fatalf("new compaction should record incremental source range, got ok=%v record=%#v", ok, record)
	}
}

func TestSessionConversationUsesCompactionSummaryRetainedTailAndAppendedMessages(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if err := sess.Append(schema.UserMessage("user " + string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
		if err := sess.Append(schema.AssistantMessage("assistant "+string(rune('0'+i)), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sess.AppendContextCompaction(session.ContextCompaction{
		AgentKind:        config.AgentKindIDE,
		Summary:          "用户目标：继续写作。",
		SourceStartIndex: 0,
		SourceEndIndex:   2,
		RetainedTurns:    2,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	conversation := NewSessionConversationForAgent(sess, cfg, config.AgentKindIDE)
	history, err := conversation.PrepareMessages("user 3", "agent user 3")
	if err != nil {
		t.Fatal(err)
	}
	// 新投影语义（目标 H）：摘要 + retainTailByUserTurns(全部有效消息, retainedTurns)。
	// 有效消息 = [user1, asst1, user2, asst2, user3]（含新追加的 user3）。
	// RetainedTurns=2 → 从末尾数 2 个用户轮：user3(count=1), user2(count=2) → 保留 [user2, asst2, user3]。
	// PrepareMessages 替换末尾为 agent user 3 → [summary, user2, asst2, agent user 3] = 4 条。
	if len(history) != 4 {
		t.Fatalf("history length = %d, want 4: %#v", len(history), history)
	}
	if !isContextCompactionMessage(history[0]) || history[0].Role != schema.User {
		t.Fatalf("first message should be compaction summary: %#v", history[0])
	}
	if history[1].Content != "user 2" || history[2].Content != "assistant 2" || history[3].Content != "agent user 3" {
		t.Fatalf("unexpected compacted history tail: %#v", history)
	}
	if visible := sess.History(); len(visible) != 5 {
		t.Fatalf("visible raw history should include only raw messages and current user: %#v", visible)
	}
}

func TestSessionConversationKeepsPostCompactionTurnsUntilNextCompaction(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if err := sess.Append(schema.UserMessage("user " + string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
		if err := sess.Append(schema.AssistantMessage("assistant "+string(rune('0'+i)), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sess.AppendContextCompaction(session.ContextCompaction{
		AgentKind:        config.AgentKindIDE,
		Summary:          "用户目标：继续写作。",
		SourceStartIndex: 0,
		SourceEndIndex:   4,
		RetainedTurns:    1,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	conversation := NewSessionConversationForAgent(sess, cfg, config.AgentKindIDE)
	history, err := conversation.PrepareMessages("user 6", "agent user 6")
	if err != nil {
		t.Fatal(err)
	}
	got := messageContents(history)
	// 新投影语义（目标 H）：摘要 + retainTailByUserTurns(全部有效消息, retainedTurns)。
	// 有效消息含新追加的 user6。RetainedTurns=1 → 仅保留最近 1 个用户轮（user6），
	// user5/assistant5 已折叠进摘要（source 覆盖到 total-1）。Prepare 替换末尾为 agent user 6。
	want := []string{
		history[0].Content,
		"agent user 6",
	}
	if !isContextCompactionMessage(history[0]) {
		t.Fatalf("first message should be compaction summary: %#v", history[0])
	}
	if len(got) != len(want) {
		t.Fatalf("history length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("history[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
}

// TestSessionConversationProjectionFoldsOldTurnsIntoSummary verifies target H:
// after compaction the model-visible projection must fold history the same way
// compactMessagesForModel does — summary + retainTailByUserTurns(all effective
// messages, retainedTurns) — so old turns are compressed into the summary rather
// than appended verbatim (the old compactedMessagesAfterSource bug that kept
// every post-source message and triggered compaction every round).
func TestSessionConversationProjectionFoldsOldTurnsIntoSummary(t *testing.T) {
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 5; i++ {
		if err := sess.Append(schema.UserMessage("user " + string(rune('0'+i)))); err != nil {
			t.Fatal(err)
		}
		if err := sess.Append(schema.AssistantMessage("assistant "+string(rune('0'+i)), nil)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sess.AppendContextCompaction(session.ContextCompaction{
		AgentKind:        config.AgentKindIDE,
		Summary:          "用户目标：继续写作。",
		SourceStartIndex: 0,
		SourceEndIndex:   4,
		RetainedTurns:    2,
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	conversation := NewSessionConversationForAgent(sess, cfg, config.AgentKindIDE)
	history, err := conversation.PrepareMessages("user 6", "agent user 6")
	if err != nil {
		t.Fatal(err)
	}
	got := messageContents(history)
	// RetainedTurns=2 → 从末尾数 2 个用户轮：user6(count=1), user5(count=2) → 保留 [user5, asst5, user6]。
	// 前 4 轮（user1-user4）折叠进摘要。Prepare 替换末尾为 agent user 6。
	want := []string{
		history[0].Content, // summary
		"user 5",
		"assistant 5",
		"agent user 6",
	}
	if !isContextCompactionMessage(history[0]) {
		t.Fatalf("first message should be compaction summary: %#v", history[0])
	}
	if len(got) != len(want) {
		t.Fatalf("projection length = %d, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("projection[%d] = %q, want %q; all=%#v", i, got[i], want[i], got)
		}
	}
}

func TestSessionConversationCompactionAppliesToolModeWindowCap(t *testing.T) {
	previous := summarizeContextForCompaction
	defer func() { summarizeContextForCompaction = previous }()
	summarizeContextForCompaction = func(_ context.Context, _ *config.Config, _ string, _ string, _ []*schema.Message, _ string, _ int, _ contextCompactionPolicy, _ func(int, string)) (string, int, error) {
		return "压缩摘要：旧对话已合并。", 100, nil
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("旧用户请求")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("旧助手回复", nil)); err != nil {
		t.Fatal(err)
	}
	// 工具模式（IDE 默认启用工具）：压缩判定沿用模型声明的 1M 窗口。
	cfg := &config.Config{OpenAIContextWindowTokens: 1_000_000}
	conversation := NewSessionConversationForAgent(sess, cfg, config.AgentKindIDE)
	history, err := conversation.PrepareMessages("继续写", "继续写")
	if err != nil {
		t.Fatal(err)
	}
	// 调用方不传 ContextWindowTokens 时，仍应从当前 agent profile 解析模型窗口。
	compacted, result, err := conversation.CompactContextIfNeeded(context.Background(), ContextCompactionInput{
		Messages: history,
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered {
		t.Fatalf("expected compaction to trigger: %#v", result)
	}
	if result.ContextWindowTokens != config.ResolveAgentModel(cfg, config.AgentKindIDE).ContextWindowTokens {
		t.Fatalf("context window tokens = %d, want provider window %d", result.ContextWindowTokens, config.ResolveAgentModel(cfg, config.AgentKindIDE).ContextWindowTokens)
	}
	if len(compacted) == 0 || !isContextCompactionMessage(compacted[0]) {
		t.Fatalf("compaction summary should lead compacted history: %#v", compacted)
	}
}

func TestSessionConversationCompactionKeepsProviderWindowWithoutTools(t *testing.T) {
	previous := summarizeContextForCompaction
	defer func() { summarizeContextForCompaction = previous }()
	summarizeContextForCompaction = func(_ context.Context, _ *config.Config, _ string, _ string, _ []*schema.Message, _ string, _ int, _ contextCompactionPolicy, _ func(int, string)) (string, int, error) {
		return "压缩摘要：旧对话已合并。", 100, nil
	}

	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.UserMessage("旧用户请求")); err != nil {
		t.Fatal(err)
	}
	if err := sess.Append(schema.AssistantMessage("旧助手回复", nil)); err != nil {
		t.Fatal(err)
	}
	// 非工具模式（version_summary 默认禁用所有工具）：不应触发工具模式硬上限，
	// 压缩决策沿用 provider 声明的窗口。
	cfg := &config.Config{OpenAIContextWindowTokens: 1_000_000}
	conversation := NewSessionConversationForAgent(sess, cfg, config.AgentKindVersionSummary)
	history, err := conversation.PrepareMessages("继续写", "继续写")
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := conversation.CompactContextIfNeeded(context.Background(), ContextCompactionInput{
		Messages: history,
		Force:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Triggered {
		t.Fatalf("expected compaction to trigger: %#v", result)
	}
	wantWindow := config.ResolveAgentModel(cfg, config.AgentKindVersionSummary).ContextWindowTokens
	if result.ContextWindowTokens != wantWindow {
		t.Fatalf("context window tokens = %d, want provider window %d (no tool-mode cap without tools)", result.ContextWindowTokens, wantWindow)
	}
}

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

func TestContextLedgerPartsForConversationIncludesDomainFragments(t *testing.T) {
	log := newContextBuildLog(DefaultLoopPolicy().ContextLedger)
	log.add("用户输入", "本轮原始请求", "推门", "")
	conversation := &contextLedgerReportingConversation{parts: []ContextLedgerPart{{
		Source: "LoreContext", Title: "常驻资料", Bytes: 128, Chars: 64, Included: true,
	}}}

	parts := contextLedgerPartsForConversation(log, conversation, []*schema.Message{schema.UserMessage("推门")})
	if len(parts) != 2 || parts[0].Source != "用户输入" || parts[1].Source != "LoreContext" {
		t.Fatalf("domain context fragments were not merged into the durable ledger: %#v", parts)
	}
}

func TestContextLedgerPartsForConversationUsesPostCompactionMessages(t *testing.T) {
	log := newContextBuildLog(DefaultLoopPolicy().ContextLedger)
	log.add("文件引用", "removed.md", "压缩前引用正文", "")
	conversation := &finalContextLedgerReportingConversation{}
	finalMessages := []*schema.Message{
		NewContextCompactionSummaryMessage(2, "有界摘要"),
		schema.UserMessage("最终用户消息"),
	}

	parts := contextLedgerPartsForConversation(log, conversation, finalMessages)
	if len(parts) != 2 || parts[0].Source != "文件引用" || parts[0].Included || !parts[0].Truncated || !strings.Contains(parts[0].Note, "not_present_after_final_compaction") || parts[1].Source != "final_messages" || conversation.messageCount != len(finalMessages) || conversation.lastContent != "最终用户消息" {
		t.Fatalf("ledger reporter did not receive the post-compaction message list: parts=%#v conversation=%#v", parts, conversation)
	}
}

func TestSingleInstructionConversationReportsStablePrefixToContextLedger(t *testing.T) {
	conversation := &singleInstructionConversation{
		stableContextTitle:    "常驻资料（complete=true; revision=rev-1）",
		stableContext:         "世界规则正文",
		stableContextMaxBytes: 1024,
	}
	parts := conversation.ContextLedgerParts()
	if len(parts) != 1 || parts[0].Source != "ResidentLore" || parts[0].Limit != 1024 || parts[0].Hash == "" || !strings.Contains(parts[0].Note, "complete=true") || !strings.Contains(parts[0].Note, "message_max_bytes=1024") {
		t.Fatalf("stable resident prefix missing from durable ledger: %#v", parts)
	}
}

type contextLedgerReportingConversation struct {
	parts    []ContextLedgerPart
	metadata RunTraceMetadata
}

type finalContextLedgerReportingConversation struct {
	contextLedgerReportingConversation
	messageCount int
	lastContent  string
}

func (c *finalContextLedgerReportingConversation) ContextLedgerPartsForMessages(messages []*schema.Message) []ContextLedgerPart {
	c.messageCount = len(messages)
	if len(messages) > 0 && messages[len(messages)-1] != nil {
		c.lastContent = messages[len(messages)-1].Content
	}
	return []ContextLedgerPart{{Source: "final_messages", Included: true}}
}

func (c *contextLedgerReportingConversation) PrepareMessages(string, string) ([]*schema.Message, error) {
	return nil, nil
}
func (c *contextLedgerReportingConversation) AppendAssistant(string) error { return nil }
func (c *contextLedgerReportingConversation) MarkInterrupted(string, string, string) error {
	return nil
}
func (c *contextLedgerReportingConversation) PendingInterruption() *session.Interruption { return nil }
func (c *contextLedgerReportingConversation) ResolveInterruption(string) error           { return nil }
func (c *contextLedgerReportingConversation) ContextLedgerParts() []ContextLedgerPart {
	return append([]ContextLedgerPart(nil), c.parts...)
}
func (c *contextLedgerReportingConversation) RunTraceMetadata() RunTraceMetadata {
	return c.metadata
}
func (c *contextLedgerReportingConversation) RuntimeState() session.RuntimeState {
	return session.RuntimeState{}
}

// TestSessionConversationRecordsRuntimeStateForResume proves the production
// recording path: a SessionConversation built with agent runtime context exposes
// itself as a RuntimeStateRecorder, and RecordRuntimeState persists the state so
// resume can replay it (Plan.md M11).
func TestSessionConversationRecordsRuntimeStateForResume(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}

	// A conversation built with agent runtime context must expose itself as a
	// RuntimeStateRecorder so the run loop can persist it after each run. Route
	// through the Conversation interface exactly like the run loop does.
	var conversation Conversation = NewSessionConversationForAgent(sess, &config.Config{}, config.AgentKindIDE)
	recorder, ok := conversation.(RuntimeStateRecorder)
	if !ok || recorder == nil {
		t.Fatal("SessionConversation must implement RuntimeStateRecorder")
	}

	// RuntimeState reports the bounded runtime context that affects the next turn.
	state := conversation.RuntimeState()
	if strings.TrimSpace(state.AgentKind) != config.AgentKindIDE {
		t.Fatalf("RuntimeState should report agent kind, got %#v", state)
	}

	// Recording via the recorder interface must persist and be replayable on resume.
	if err := recorder.RecordRuntimeState(state); err != nil {
		t.Fatal(err)
	}

	reloadedStore, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := reloadedStore.Get("default")
	if err != nil {
		t.Fatal(err)
	}
	got, found := reloaded.LatestRuntimeState()
	if !found {
		t.Fatal("expected persisted runtime state after RecordRuntimeState")
	}
	if got.AgentKind != config.AgentKindIDE {
		t.Fatalf("reloaded runtime state agent kind mismatch: %#v", got)
	}
}

// TestSessionConversationPrependsRuntimeStateForResume proves the M11
// consumption side: a conversation whose session has a recorded runtime state
// prepends that state as a leading model message so resume inherits the exact
// "work site" (Plan.md M11).
func TestSessionConversationPrependsRuntimeStateForResume(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}

	conversation := NewSessionConversationForAgent(sess, &config.Config{}, config.AgentKindIDE)

	// Record a runtime state with several fields populated.
	state := session.RuntimeState{
		AgentKind: config.AgentKindIDE,
		Mode:      "writing",
		StoryID:   "story-1",
		BranchID:  "branch-a",
		LastRead:  "chapters/ch00003.md",
		Settings:  map[string]string{"teller_id": "classic"},
	}
	if err := conversation.RecordRuntimeState(state); err != nil {
		t.Fatal(err)
	}

	messages, err := conversation.PrepareMessages("继续写第三章", "继续写第三章")
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 2 {
		t.Fatalf("expected runtime-state leading message plus final user message, got %d messages", len(messages))
	}

	leading := messages[0]
	if leading.Role != schema.User {
		t.Fatalf("runtime state should be a leading user message, role=%s", leading.Role)
	}
	for _, want := range []string{"story-1", "branch-a", "ch00003.md", "classic"} {
		if !strings.Contains(leading.Content, want) {
			t.Fatalf("runtime state leading message missing %q:\n%s", want, leading.Content)
		}
	}
}

// TestSessionConversationNoRuntimeStateWhenEmpty proves the leading message is
// omitted when no runtime state has been recorded.
func TestSessionConversationNoRuntimeStateWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}

	conversation := NewSessionConversationForAgent(sess, &config.Config{}, config.AgentKindIDE)
	messages, err := conversation.PrepareMessages("开始", "开始")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := conversation.runtimeStateSource(); ok {
		t.Fatal("expected empty runtime state source for fresh session")
	}
	if len(messages) < 1 {
		t.Fatalf("expected at least one message, got %d", len(messages))
	}
}

// TestSessionConversationRuntimeStateIncludesActiveBranch proves that
// RuntimeState reports the session's active branch so resume can inherit
// work tree ownership (Plan.md M11).
func TestSessionConversationRuntimeStateIncludesActiveBranch(t *testing.T) {
	dir := t.TempDir()
	store, err := session.NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := store.GetOrCreate("default")
	if err != nil {
		t.Fatal(err)
	}

	// Append messages and fork to create an active branch.
	for i := 0; i < 3; i++ {
		if err := sess.Append(schema.UserMessage("base")); err != nil {
			t.Fatal(err)
		}
	}
	if err := sess.Fork("branch-x", "1"); err != nil {
		t.Fatal(err)
	}

	conversation := NewSessionConversationForAgent(sess, &config.Config{}, config.AgentKindIDE)
	state := conversation.RuntimeState()

	if strings.TrimSpace(state.BranchID) != "branch-x" {
		t.Fatalf("RuntimeState should report active branch, got %#v", state)
	}
}
