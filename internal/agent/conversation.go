package agent

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"denova/config"
	agentcontext "denova/internal/agent/context"
	"denova/internal/session"
)

// Conversation 抽象 Agent 对话的上下文读取与结果写入。
// 写作模式写入普通 session，游戏模式可写入 interactive/story。
type Conversation interface {
	PrepareMessages(originalMessage, agentMessage string) ([]*schema.Message, error)
	AppendAssistant(content string) error
	MarkInterrupted(userMessage, assistantContent, reason string) error
	PendingInterruption() *session.Interruption
	ResolveInterruption(id string) error
	RuntimeState() session.RuntimeState
}

// RuntimeStateRecorder lets a durable conversation persist the agent's runtime
// context (agent kind, mode, story/branch, last read file, bounded settings)
// so resume can replay the exact "work site" (Plan.md M11). It is an optional
// interface; the runtime records runtime state after a successful run when the
// conversation implements it.
type RuntimeStateRecorder interface {
	RecordRuntimeState(state session.RuntimeState) error
}

// UserMessageReferencesSetter lets a durable conversation attach bounded,
// display-only references to the next persisted user message.
type UserMessageReferencesSetter interface {
	SetUserMessageReferences([]session.UserMessageReference)
}

// ModelNameSetter lets a durable conversation bind the model name used for the
// next turn so it can be surfaced in the UI (Plan.md 11 / F2).
type ModelNameSetter interface {
	SetModelName(string)
}

// ContextSourceReporter 可由 Conversation 提供本轮已拼装的业务上下文来源。
// ChatService 会在 PrepareMessages 后追加打印，便于排查非通用注入内容。
type ContextSourceReporter interface {
	ContextSourceSummary() string
}

// ContextLedgerReporter exposes bounded metadata for the actual domain context
// fragments assembled by a Conversation. Full fragment content is never
// persisted by the runtime.
type ContextLedgerReporter interface {
	ContextLedgerParts() []ContextLedgerPart
}

// FinalContextLedgerReporter rebuilds domain context audit metadata from the
// exact message list sent to the model after context compaction. Implementers
// must not retain full message bodies in the returned durable records.
type FinalContextLedgerReporter interface {
	ContextLedgerPartsForMessages(messages []*schema.Message) []ContextLedgerPart
}

// RunTraceMetadata is the bounded interactive identity attached to one run.
// A Conversation may fill fields such as TurnID only after its final output is
// committed, so the runtime resolves it again during finish.
type RunTraceMetadata struct {
	StoryID         string `json:"story_id,omitempty"`
	BranchID        string `json:"branch_id,omitempty"`
	TurnID          string `json:"turn_id,omitempty"`
	MaintenanceTask string `json:"maintenance_task,omitempty"`
}

type RunTraceMetadataReporter interface {
	RunTraceMetadata() RunTraceMetadata
}

// RuntimeStateReporter lets a durable conversation expose the bounded runtime
// context (agent kind, working mode, active story/branch, last read file, and
// any bounded settings) that affects the next turn. The runtime records it via
// RuntimeStateRecorder after a successful run so resume can replay the exact
// "work site" (Plan.md M11). It is an optional interface; conversations that
// do not track runtime state simply omit it.
type RuntimeStateReporter interface {
	RuntimeState() session.RuntimeState
}

// InteractiveNarrativeReadinessReporter marks the protocol boundary after a
// Game Agent has successfully staged its hidden TurnResult. Runtime uses it to
// recognize cancellation after submission as successful turn completion.
type InteractiveNarrativeReadinessReporter interface {
	InteractiveNarrativeReady() bool
}

type SessionConversation struct {
	session               *session.Session
	cfg                   *config.Config
	agentKind             string
	modelName             string
	stableContextTitle    string
	stableContext         string
	dynamicContextTitle   string
	dynamicContext        string
	userMessageReferences []session.UserMessageReference
}

func (c *SessionConversation) SetUserMessageReferences(references []session.UserMessageReference) {
	if c == nil {
		return
	}
	c.userMessageReferences = append([]session.UserMessageReference(nil), references...)
}

// SetModelName binds the model name for this conversation (Plan.md 11 / F2).
func (c *SessionConversation) SetModelName(name string) {
	if c == nil {
		return
	}
	c.modelName = strings.TrimSpace(name)
}

func NewSessionConversation(sess *session.Session, options ...SessionConversationOption) *SessionConversation {
	c := &SessionConversation{session: sess}
	for _, option := range options {
		if option != nil {
			option(c)
		}
	}
	return c
}

func NewSessionConversationForAgent(sess *session.Session, cfg *config.Config, agentKind string) *SessionConversation {
	return NewSessionConversation(
		sess,
		WithSessionContextConfig(cfg, agentKind),
	)
}

func NewSessionConversationForAgentWithRuntimeContext(sess *session.Session, cfg *config.Config, agentKind, title, content string) *SessionConversation {
	return NewSessionConversation(
		sess,
		WithSessionContextConfig(cfg, agentKind),
		WithSessionRuntimeContext(title, content),
	)
}

func NewSessionConversationForAgentWithRuntimeContexts(sess *session.Session, cfg *config.Config, agentKind, stableTitle, stableContent, dynamicTitle, dynamicContent string) *SessionConversation {
	return NewSessionConversation(
		sess,
		WithSessionContextConfig(cfg, agentKind),
		WithSessionStableRuntimeContext(stableTitle, stableContent),
		WithSessionRuntimeContext(dynamicTitle, dynamicContent),
	)
}

type SessionConversationOption func(*SessionConversation)

func WithSessionContextConfig(cfg *config.Config, agentKind string) SessionConversationOption {
	return func(c *SessionConversation) {
		c.cfg = cfg
		c.agentKind = agentKind
	}
}

func WithSessionRuntimeContext(title, content string) SessionConversationOption {
	return func(c *SessionConversation) {
		c.dynamicContextTitle = title
		c.dynamicContext = content
	}
}

func WithSessionStableRuntimeContext(title, content string) SessionConversationOption {
	return func(c *SessionConversation) {
		c.stableContextTitle = title
		c.stableContext = content
	}
}

// WithSessionModelName binds the model name used for this conversation so it can
// be surfaced in the UI (Plan.md 11 / F2). It is best-effort: an empty value
// simply means the model name is not shown.
func WithSessionModelName(name string) SessionConversationOption {
	return func(c *SessionConversation) {
		c.modelName = strings.TrimSpace(name)
	}
}

func (c *SessionConversation) PrepareMessages(originalMessage, agentMessage string) ([]*schema.Message, error) {
	if c == nil || c.session == nil {
		return nil, fmt.Errorf("会话不存在")
	}
	metadata := session.MessageMetadata{UserReferences: c.userMessageReferences}
	if c.modelName != "" {
		metadata.ModelName = c.modelName
	}
	if err := c.session.AppendWithMetadata(schema.UserMessage(originalMessage), metadata); err != nil {
		return nil, err
	}
	result, err := agentcontext.Build(context.Background(), agentcontext.Request{
		Messages: c.modelMessages(agentMessage),
		Sources:  c.runtimeContextSources(),
	})
	if err != nil {
		return nil, err
	}
	return result.Messages, nil
}

func (c *SessionConversation) ContextSourceSummary() string {
	if c == nil || (strings.TrimSpace(c.stableContext) == "" && strings.TrimSpace(c.dynamicContext) == "") {
		return ""
	}
	return agentcontext.SourceSummary(c.runtimeContextSources(), defaultContextLedgerPreviewChars)
}

func (c *SessionConversation) CompactContextIfNeeded(ctx context.Context, input ContextCompactionInput) ([]*schema.Message, ContextCompactionResult, error) {
	policy := c.compactionPolicy()
	// Do not replace the selected model's context window with a local tool cap.
	// The normal compaction decision must use the active agent profile; an
	// optional tool_safe_window_tokens is enforced separately before tool runs.
	if input.ContextWindowTokens > 0 {
		policy.ContextWindowTokens = input.ContextWindowTokens
	}
	input = withDefaultContextProjectionReserves(c.cfg, c.agentKind, input, 0)
	phase := strings.TrimSpace(input.Phase)
	if phase == "" {
		phase = contextCompactionPhasePreRun
	}
	tokensBefore := EstimateContextTokens(input.Messages, input.Tools)
	projectedTokensBefore := projectedContextTokens(tokensBefore, input)
	result := ContextCompactionResult{
		Phase:                    phase,
		TokensBefore:             tokensBefore,
		ProjectedTokensBefore:    projectedTokensBefore,
		ReservedCompletionTokens: input.ReservedCompletionTokens,
		ReservedToolResultTokens: input.ReservedToolResultTokens,
		ContextWindowTokens:      policy.ContextWindowTokens,
		Strategy:                 policy.Strategy,
		Threshold:                policy.Threshold,
		MessageCountBefore:       len(input.Messages),
		RetainedTurns:            policy.RetainedTurns,
	}
	shouldCompact, skipped := policy.shouldCompact(projectedTokensBefore, input.Force)
	if !shouldCompact {
		result.SkippedReason = skipped
		return input.Messages, result, nil
	}
	source, existingCheckpoint, sourceStart, sourceEnd := c.compactionIncrementalSource(input.KeepLatestUser)
	if strings.TrimSpace(input.ExistingCheckpoint) != "" {
		existingCheckpoint = input.ExistingCheckpoint
	}
	// Seed compaction with bounded background memory notes (Plan.md M7) so the
	// summary can reuse structured goals/progress/conclusions instead of
	// re-summarizing raw turns. Only fill when the caller did not supply its own
	// reference context.
	if strings.TrimSpace(input.ReferenceContext) == "" {
		input.ReferenceContext = c.memoryNotesReferenceContext()
	}
	if len(source) == 0 && strings.TrimSpace(existingCheckpoint) == "" && strings.TrimSpace(input.ReferenceContext) == "" {
		result.SkippedReason = "empty_source"
		return input.Messages, result, nil
	}
	if !input.Force {
		if removal, ok := c.session.LatestContextCompactionRemoval(c.agentKind); ok && removal.SourceStartIndex == sourceStart && removal.SourceEndIndex >= sourceEnd {
			result.SkippedReason = "removed_same_source"
			return input.Messages, result, nil
		}
	}
	sourceTokens := EstimateContextTokens(source, nil)
	emitContextCompactionEvent(input.Emit, phase, "started", result)
	summary, inputChars, err := summarizeContextForCompaction(ctx, c.cfg, c.agentKind, existingCheckpoint, source, input.ReferenceContext, sourceTokens, policy, func(attempt int, delta string) {
		emitContextCompactionDeltaEvent(input.Emit, phase, result, attempt, delta)
	})
	if err != nil {
		emitContextCompactionEvent(input.Emit, phase, "failed", result)
		return input.Messages, result, err
	}
	// ── 三层分层压缩（Plan.md §12 / T2）─────────────────────────────────────
	// 上面的 summarizeContextForCompaction 即 T1 capture（原始增量 → 详细摘要）。
	// 若启用分层，把 T1 摘要并入摘要池，级联 T2 distill / T3 condense，投影使用
	// 最高可用层（写回 summary → 记录 Summary），保证投影恒定、不每轮重新压缩（H 修正）。
	var tieredPool *session.ContextTieredPool
	contextSettings := config.ResolveAgentContext(c.cfg, c.agentKind)
	if contextSettings.TieredCompactionEnabled {
		pool := session.ContextTieredPool{}
		if prev, ok := c.session.LatestContextCompaction(c.agentKind); ok && prev.Tiered != nil {
			pool = *prev.Tiered
		}
		pool.T1Summary = summary
		pool.T1Tokens = estimateStringTokens(summary)
		pool.RawTokens = 0 // 本轮增量已捕获
		pool, err = runTieredDistill(ctx, c.cfg, c.agentKind, pool, tieredThresholdsFromSettings(contextSettings), policy)
		if err != nil {
			emitContextCompactionEvent(input.Emit, phase, "failed", result)
			return input.Messages, result, err
		}
		summary = tieredProjectionSummary(pool)
		tieredPool = &pool
		slog.Info("tiered_compaction",
			slog.String("agent_kind", c.agentKind),
			slog.Int("t1_tokens", pool.T1Tokens),
			slog.Int("t2_tokens", pool.T2Tokens),
			slog.Int("t3_tokens", pool.T3Tokens),
		)
	}
	// 压缩质量门（T3，默认关闭）：校验摘要质量，失败只 Warn 不阻断。
	if contextSettings.CompactionQualityGateEnabled {
		var originalText strings.Builder
		for _, m := range source {
			if m != nil {
				originalText.WriteString(m.Content)
				originalText.WriteString("\n")
			}
		}
		logCompactionQuality(c.agentKind, phase, originalText.String(), summary)
	}
	epoch := c.nextCompactionEpoch()
	leading, compactableMessages := c.splitLeadingRuntimeMessages(input.Messages)
	newMessages := compactMessagesForModel(compactableMessages, summary, epoch, policy.RetainedTurns)
	if len(leading) > 0 {
		newMessages = append(append([]*schema.Message(nil), leading...), newMessages...)
	}
	result.Triggered = true
	result.Epoch = epoch
	result.Summary = summary
	result.TokensAfter = EstimateContextTokens(newMessages, input.Tools)
	result.ProjectedTokensAfter = projectedContextTokens(result.TokensAfter, input)
	result.TargetRatio = contextCompactionRatio(countRunes(summary), inputChars)
	result.SourceMessageCount = len(source)
	result.MessageCountAfter = len(newMessages)
	record := contextCompactionRecordFromResult(result, c.agentKind, sourceStart, sourceEnd, policy.RetainedTurns, summary)
	record.Tiered = tieredPool
	record, err = c.session.AppendContextCompaction(record)
	if err != nil {
		emitContextCompactionEvent(input.Emit, phase, "failed", result)
		return input.Messages, result, err
	}
	if record.Epoch != epoch {
		result.Epoch = record.Epoch
		newMessages = compactMessagesForModel(compactableMessages, summary, record.Epoch, policy.RetainedTurns)
		if len(leading) > 0 {
			newMessages = append(append([]*schema.Message(nil), leading...), newMessages...)
		}
		result.TokensAfter = EstimateContextTokens(newMessages, input.Tools)
		result.ProjectedTokensAfter = projectedContextTokens(result.TokensAfter, input)
		result.MessageCountAfter = len(newMessages)
	}
	emitContextCompactionEvent(input.Emit, phase, "completed", result)
	return newMessages, result, nil
}

// FoldContextIfNeeded folds the incremental source (messages after the last
// compaction) into a summary placeholder without creating a new boundary or
// deleting raw history. It is mutually exclusive with full compaction: when a
// compaction is active the fold is skipped so both mechanisms do not manage the
// same projection. The fold record is persisted so the projection survives
// reload; on reload the session re-applies the active fold.
func (c *SessionConversation) FoldContextIfNeeded(ctx context.Context, input ContextCompactionInput) ([]*schema.Message, ContextFoldResult, error) {
	if c == nil || c.session == nil {
		return input.Messages, ContextFoldResult{}, fmt.Errorf("会话不存在")
	}
	policy := c.compactionPolicy()
	if input.ContextWindowTokens > 0 {
		policy.ContextWindowTokens = input.ContextWindowTokens
	}
	input = withDefaultContextProjectionReserves(c.cfg, c.agentKind, input, 0)
	phase := strings.TrimSpace(input.Phase)
	if phase == "" {
		phase = contextCompactionPhasePreRun
	}
	result := ContextFoldResult{
		Phase:              phase,
		MessageCountBefore: len(input.Messages),
		RetainedTurns:      policy.RetainedTurns,
	}
	// Fold and full compaction manage the same "old history projection"; prefer
	// an active compaction so they never overlap.
	if !input.Force {
		if _, ok := c.session.LatestContextCompaction(c.agentKind); ok {
			result.SkippedReason = "compaction_active"
			return input.Messages, result, nil
		}
	}
	source, _, sourceStart, sourceEnd := c.compactionIncrementalSource(true)
	if strings.TrimSpace(input.ReferenceContext) == "" {
		input.ReferenceContext = c.memoryNotesReferenceContext()
	}
	if len(source) == 0 && strings.TrimSpace(input.ReferenceContext) == "" {
		result.SkippedReason = "empty_source"
		return input.Messages, result, nil
	}
	emitContextFoldEvent(input.Emit, phase, "started", result)
	newMessages, foldResult, err := agentFoldContext(ctx, c.cfg, c.agentKind, input, sourceStart, sourceEnd, phase)
	if err != nil {
		emitContextFoldEvent(input.Emit, phase, "failed", foldResult)
		return input.Messages, foldResult, err
	}
	foldRecord := session.ContextFold{
		Type:               "context_fold",
		AgentKind:          c.agentKind,
		Summary:            foldResult.Summary,
		SourceStartIndex:   sourceStart,
		SourceEndIndex:     sourceEnd,
		SourceMessageCount: foldResult.SourceMessageCount,
		RetainedTurns:      foldResult.RetainedTurns,
		TokensBefore:       foldResult.TokensBefore,
		TokensAfter:        foldResult.TokensAfter,
		Reason:             contextCompactionReasonLimit,
		Phase:              phase,
		CreatedAt:          time.Now().UTC(),
	}
	if _, err := c.session.AppendContextFold(foldRecord); err != nil {
		emitContextFoldEvent(input.Emit, phase, "failed", foldResult)
		return input.Messages, foldResult, err
	}
	emitContextFoldEvent(input.Emit, phase, "completed", foldResult)
	return newMessages, foldResult, nil
}

// agentFoldContext is the shared fold entry point used by both session and
// interactive conversations. It delegates to agent.FoldContext and returns the
// fold result so callers can persist a fold record.
func agentFoldContext(ctx context.Context, cfg *config.Config, agentKind string, input ContextCompactionInput, sourceStart, sourceEnd int, phase string) ([]*schema.Message, ContextFoldResult, error) {
	foldInput := FoldContextInput{
		Messages:         input.Messages,
		Tools:            input.Tools,
		Phase:            phase,
		Emit:             input.Emit,
		Force:            input.Force,
		ReferenceContext: input.ReferenceContext,
		FoldStart:        sourceStart,
		FoldEnd:          sourceEnd,
		RetainedTurns:    policyRetainedTurns(cfg, agentKind),
	}
	return FoldContext(ctx, cfg, agentKind, foldInput)
}

func policyRetainedTurns(cfg *config.Config, agentKind string) int {
	policy := resolveContextCompactionPolicy(cfg, agentKind)
	if policy.RetainedTurns <= 0 {
		return config.DefaultContextCompactionRetainedTurns
	}
	if policy.RetainedTurns > config.MaxContextCompactionRetainedTurns {
		return config.MaxContextCompactionRetainedTurns
	}
	return policy.RetainedTurns
}

// FoldPhasePreRun is the exported pre-run phase label used by callers that fold
// outside the agent package.
func FoldPhasePreRun() string { return contextCompactionPhasePreRun }

// FoldReasonLimit is the exported reason label for budget-driven folds.
func FoldReasonLimit() string { return contextCompactionReasonLimit }

// FoldEmit is the exported wrapper around emitContextFoldEvent so callers in
// other packages can reuse the same event shape.
func FoldEmit(emit func(Event), phase, status string, result ContextFoldResult) {
	emitContextFoldEvent(emit, phase, status, result)
}

// AgentFoldContext is the exported shared fold entry point used by interactive
// conversations. It delegates to agent.FoldContext with policy-clamped retained
// turns and returns the fold result so callers can persist a fold record.
func AgentFoldContext(ctx context.Context, cfg *config.Config, agentKind string, input ContextCompactionInput, sourceStart, sourceEnd int, phase string) ([]*schema.Message, ContextFoldResult, error) {
	return agentFoldContext(ctx, cfg, agentKind, input, sourceStart, sourceEnd, phase)
}

// PolicyRetainedTurns clamps a policy's retained-turns setting into the valid
// range for folding.
func PolicyRetainedTurns(cfg *config.Config, agentKind string) int {
	return policyRetainedTurns(cfg, agentKind)
}

func (c *SessionConversation) modelMessages(agentMessage string) []*schema.Message {
	history := append([]*schema.Message(nil), c.session.GetEffectiveMessages()...)
	policy := c.compactionPolicy()
	if compaction, ok := c.session.LatestContextCompaction(c.agentKind); ok && strings.TrimSpace(compaction.Summary) != "" {
		total := c.session.MessageCountTotal()
		_ = total // effectiveStart unused: projection now folds via compactMessagesForModel
		retainedTurns := compaction.RetainedTurns
		if retainedTurns <= 0 {
			retainedTurns = policy.RetainedTurns
		}
		// 与压缩时一致的折叠：摘要 + retainTailByUserTurns(全部有效消息, retainedTurns)。
		// 旧投影用 compactedMessagesAfterSource，只折叠 [0:SourceEndIndex)，压缩点之后新消息全保留，
		// 导致每轮投影恒 ~56k → 每轮都触发压缩 → 每轮重复生成摘要。
		history = compactMessagesForModel(history, compaction.Summary, compaction.Epoch, retainedTurns)
	}
	history = applyToolResultContextPolicy(history, c.ToolResultContextPolicy())
	// resume 结构修复（Plan.md Phase 5）：中断恢复时历史可能含悬空工具调用
	// 或连续同角色消息，送入模型前统一修复（只改投影，不改持久化历史）。
	history = repairResumeStructure(history)
	if len(history) > 0 {
		history[len(history)-1] = schema.UserMessage(agentMessage)
	}
	return history
}

func standaloneRuntimeContextMessage(title, content, note string) string {
	return agentcontext.StandaloneMessage(title, content, note)
}

func (c *SessionConversation) leadingRuntimeMessages() []*schema.Message {
	if c == nil {
		return nil
	}
	var leading []*schema.Message
	if source, ok := c.runtimeStateSource(); ok {
		leading = append(leading, schema.UserMessage(agentcontext.StandaloneMessage(source.Title, source.Content, "")))
	}
	if strings.TrimSpace(c.stableContext) != "" {
		content := standaloneRuntimeContextMessage(c.stableContextTitle, c.stableContext, "")
		if strings.TrimSpace(content) != "" {
			leading = append(leading, schema.UserMessage(content))
		}
	}
	return leading
}

func (c *SessionConversation) runtimeContextSources() []agentcontext.Source {
	if c == nil {
		return nil
	}
	var sources []agentcontext.Source
	if source, ok := c.runtimeStateSource(); ok {
		sources = append(sources, source)
	}
	if strings.TrimSpace(c.stableContext) != "" {
		title := strings.TrimSpace(c.stableContextTitle)
		if title == "" {
			title = "稳定上下文"
		}
		sources = append(sources, agentcontext.Source{
			Source:    "稳定上下文",
			Title:     title,
			Content:   c.stableContext,
			Placement: agentcontext.PlacementLeadingMessage,
			Included:  true,
			Note:      "prepended_to_model_messages",
		})
	}
	if strings.TrimSpace(c.dynamicContext) != "" {
		title := strings.TrimSpace(c.dynamicContextTitle)
		if title == "" {
			title = "本轮动态上下文"
		}
		sources = append(sources, agentcontext.Source{
			Source:    "本轮动态上下文",
			Title:     title,
			Content:   c.dynamicContext,
			Placement: agentcontext.PlacementFinalUserPrefix,
			Included:  true,
			Note:      "prepended_to_final_user_message",
		})
	}
	return sources
}

// runtimeStateSnapshotMaxBytes bounds the resume-runtime-state snapshot that is
// injected as a leading model message so resume inherits the exact "work site"
// (Plan.md M11). It stays well under the stable-context budget.
const runtimeStateSnapshotMaxBytes = 4 * 1024

// formatRuntimeStateSnapshot renders a bounded, human-readable snapshot of the
// last recorded runtime state so the next turn can inherit the work site.
func formatRuntimeStateSnapshot(rs session.RuntimeState) string {
	var sb strings.Builder
	sb.WriteString("上一轮离开时的运行时状态（resume 迁移，Plan.md M11）。下一轮应继承这些工作现场：\n\n")
	if strings.TrimSpace(rs.AgentKind) != "" {
		sb.WriteString(fmt.Sprintf("- Agent 种类：%s\n", rs.AgentKind))
	}
	if strings.TrimSpace(rs.Mode) != "" {
		sb.WriteString(fmt.Sprintf("- 工作模式：%s\n", rs.Mode))
	}
	if strings.TrimSpace(rs.StoryID) != "" || strings.TrimSpace(rs.BranchID) != "" {
		sb.WriteString(fmt.Sprintf("- 故事/分支：%s/%s\n", rs.StoryID, rs.BranchID))
	}
	if strings.TrimSpace(rs.LastRead) != "" {
		sb.WriteString(fmt.Sprintf("- 最后读取文件：%s\n", rs.LastRead))
	}
	if len(rs.Settings) > 0 {
		sb.WriteString("- 运行时设置：\n")
		keys := make([]string, 0, len(rs.Settings))
		for k := range rs.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(fmt.Sprintf("  - %s = %s\n", k, rs.Settings[k]))
		}
	}
	return strings.TrimSpace(sb.String())
}

// runtimeStateSource builds the leading model source that carries the last
// recorded runtime state for resume. It is empty when no runtime state exists.
func (c *SessionConversation) runtimeStateSource() (agentcontext.Source, bool) {
	if c == nil || c.session == nil {
		return agentcontext.Source{}, false
	}
	rs, ok := c.session.LatestRuntimeState()
	if !ok {
		return agentcontext.Source{}, false
	}
	content := formatRuntimeStateSnapshot(rs)
	if len([]byte(content)) > runtimeStateSnapshotMaxBytes {
		content = string([]rune(content)[:runtimeStateSnapshotMaxBytes])
	}
	if strings.TrimSpace(content) == "" {
		return agentcontext.Source{}, false
	}
	return agentcontext.Source{
		Source:    "运行时状态",
		Title:     "运行时状态",
		Purpose:   "resume 迁移：继承离开时的 agent/模式/故事/分支/最后读取文件/设置",
		Content:   content,
		Placement: agentcontext.PlacementLeadingMessage,
		Included:  true,
		Note:      "prepended_to_model_messages",
		Limit:     runtimeStateSnapshotMaxBytes,
	}, true
}

func (c *SessionConversation) splitLeadingRuntimeMessages(messages []*schema.Message) ([]*schema.Message, []*schema.Message) {
	leading := c.leadingRuntimeMessages()
	if len(leading) == 0 || len(messages) < len(leading) {
		return nil, messages
	}
	for i := range leading {
		if messages[i] == nil || leading[i] == nil || messages[i].Role != leading[i].Role || messages[i].Content != leading[i].Content {
			return nil, messages
		}
	}
	return messages[:len(leading)], messages[len(leading):]
}

func (c *SessionConversation) compactionPolicy() contextCompactionPolicy {
	if c == nil {
		return contextCompactionPolicy{}
	}
	agentKind := c.agentKind
	if strings.TrimSpace(agentKind) == "" {
		agentKind = config.AgentKindIDE
	}
	policy := resolveContextCompactionPolicy(c.cfg, agentKind)
	return policy
}

func (c *SessionConversation) nextCompactionEpoch() int {
	return c.session.NextContextCompactionEpoch(c.agentKind)
}

func (c *SessionConversation) compactionIncrementalSource(keepLatestUser bool) ([]*schema.Message, string, int, int) {
	if c == nil || c.session == nil {
		return nil, "", 0, 0
	}
	messages := c.session.GetMessages()
	total := len(messages)
	sourceStart := total - c.session.MessageCountSinceClear()
	if sourceStart < 0 {
		sourceStart = 0
	}
	existingCheckpoint := ""
	if compaction, ok := c.session.LatestContextCompaction(c.agentKind); ok {
		existingCheckpoint = compaction.Summary
		if compaction.SourceEndIndex > sourceStart {
			sourceStart = compaction.SourceEndIndex
		}
	}
	if sourceStart > total {
		sourceStart = total
	}
	sourceEnd := total
	if !keepLatestUser && sourceEnd > sourceStart {
		sourceEnd--
	}
	if sourceEnd < sourceStart {
		sourceEnd = sourceStart
	}
	source := compactionSourceMessages(applyToolResultContextPolicy(messages[sourceStart:sourceEnd], c.ToolResultContextPolicy()), true)
	return source, existingCheckpoint, sourceStart, sourceEnd
}

// memoryNotesReferenceContext renders the latest bounded background memory notes
// for an agent kind into a single bounded string so compaction can reuse them
// directly instead of re-summarizing raw turns (Plan.md M7). It is best-effort:
// a nil session or empty notes yield an empty string.
func (c *SessionConversation) memoryNotesReferenceContext() string {
	if c == nil || c.session == nil {
		return ""
	}
	agentKind := c.agentKind
	if strings.TrimSpace(agentKind) == "" {
		agentKind = config.AgentKindIDE
	}
	notes := c.session.LatestMemoryNotes(agentKind, 0)
	if len(notes) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, note := range notes {
		block := formatMemoryNote(note)
		if block == "" {
			continue
		}
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(block)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func compactionSourceMessages(messages []*schema.Message, keepLatestUser bool) []*schema.Message {
	source := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil {
			continue
		}
		if isContextCompactionMessage(msg) {
			continue
		}
		source = append(source, sanitizeCompactionSourceMessage(msg))
	}
	if !keepLatestUser && len(source) > 0 && source[len(source)-1].Role == schema.User {
		source = source[:len(source)-1]
	}
	return source
}

func sanitizeCompactionSourceMessage(msg *schema.Message) *schema.Message {
	if msg == nil {
		return nil
	}
	copied := *msg
	copied.ReasoningContent = ""
	return &copied
}

func retainTailByUserTurns(messages []*schema.Message, retainedTurns int) []*schema.Message {
	if retainedTurns <= 0 {
		retainedTurns = config.DefaultContextCompactionRetainedTurns
	}
	if retainedTurns > config.MaxContextCompactionRetainedTurns {
		retainedTurns = config.MaxContextCompactionRetainedTurns
	}
	userCount := 0
	start := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i] == nil || messages[i].Role != schema.User {
			continue
		}
		userCount++
		if userCount == retainedTurns {
			start = i
			break
		}
	}
	if userCount < retainedTurns {
		return messages
	}
	return append([]*schema.Message(nil), messages[start:]...)
}

// memoryNoteSource returns the raw incremental messages since the last clear
// marker (or last compaction) for the background memory-note writer (Plan.md M7).
// It keeps reasoning stripped and includes the latest user turn so the note
// captures the full turn, unlike compaction which drops the trailing user
// message from its summary source.
func (c *SessionConversation) memoryNoteSource() []*schema.Message {
	if c == nil || c.session == nil {
		return nil
	}
	messages := c.session.GetMessages()
	total := len(messages)
	sourceStart := total - c.session.MessageCountSinceClear()
	if sourceStart < 0 {
		sourceStart = 0
	}
	if compaction, ok := c.session.LatestContextCompaction(c.agentKind); ok && compaction.SourceEndIndex > sourceStart {
		sourceStart = compaction.SourceEndIndex
	}
	if sourceStart > total {
		sourceStart = total
	}
	if sourceStart >= len(messages) {
		return nil
	}
	source := make([]*schema.Message, 0, len(messages)-sourceStart)
	for _, msg := range messages[sourceStart:] {
		if msg == nil {
			continue
		}
		source = append(source, sanitizeCompactionSourceMessage(msg))
	}
	return source
}

// Session exposes the underlying session so the runtime can attach it to the
// run context (used by middleware to persist externalize decisions).
func (c *SessionConversation) Session() *session.Session {
	if c == nil {
		return nil
	}
	return c.session
}

func (c *SessionConversation) AppendAssistant(content string) error {
	return c.AppendAssistantWithMetadata(content, "", session.MessageMetadata{})
}

func (c *SessionConversation) AppendAssistantWithMetadata(content, _ string, metadata session.MessageMetadata) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	// 若调用方未显式提供模型名，回退到该会话绑定的模型名（Plan.md 11 / F2）。
	if strings.TrimSpace(metadata.ModelName) == "" && c.modelName != "" {
		metadata.ModelName = c.modelName
	}
	return c.session.AppendWithMetadata(schema.AssistantMessage(content, nil), metadata)
}

func (c *SessionConversation) AppendContextMessage(msg *schema.Message) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.AppendContextMessage(msg)
}

func (c *SessionConversation) ToolResultContextPolicy() ToolResultContextPolicy {
	if c == nil {
		return ToolResultContextPolicy{}
	}
	agentKind := c.agentKind
	if strings.TrimSpace(agentKind) == "" {
		agentKind = config.AgentKindIDE
	}
	return resolveToolResultContextPolicy(c.cfg, agentKind)
}

func (c *SessionConversation) AppendDisplayEvent(event session.DisplayEvent) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.AppendDisplayEvent(event)
}

func (c *SessionConversation) UpdateDisplayToolStatus(id, name, status string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.UpdateDisplayToolStatus(id, name, status)
}

func (c *SessionConversation) AppendDisplayToolArgs(id, name, delta string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.AppendDisplayToolArgs(id, name, delta)
}

func (c *SessionConversation) UpdateDisplayToolResult(id, name, status, result string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.UpdateDisplayToolResult(id, name, status, result)
}

func (c *SessionConversation) UpdateDisplayToolIllustration(id, name string, illustration *session.ChapterIllustration) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.UpdateDisplayToolIllustration(id, name, illustration)
}

func (c *SessionConversation) MarkInterrupted(userMessage, assistantContent, reason string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.MarkInterrupted(userMessage, assistantContent, reason)
}

func (c *SessionConversation) PendingInterruption() *session.Interruption {
	if c == nil || c.session == nil {
		return nil
	}
	return c.session.PendingInterruption()
}

// RuntimeStateRecorder lets a durable conversation persist the agent's runtime
// context (agent kind, mode, story/branch, last read file, bounded settings)
// so resume can replay the exact "work site" (Plan.md M11). It is an optional
// interface; the runtime records runtime state after a successful run when the
// conversation implements it.
func (c *SessionConversation) RecordRuntimeState(state session.RuntimeState) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	record, err := c.session.AppendRuntimeState(state)
	if err != nil {
		return err
	}
	log.Printf("[agent-run] recorded runtime state session_id=%s agent_kind=%s mode=%s last_read=%s", record.ID, record.AgentKind, record.Mode, record.LastRead)
	return nil
}

// RuntimeState exposes the bounded runtime context this conversation tracks so
// the runtime can persist it for resume (Plan.md M11). It reports the agent
// kind and the session's active branch (work tree ownership follows the active
// branch); mode/story/last-read are filled by callers that know them.
func (c *SessionConversation) RuntimeState() session.RuntimeState {
	if c == nil {
		return session.RuntimeState{}
	}
	state := session.RuntimeState{AgentKind: strings.TrimSpace(c.agentKind)}
	if c.session != nil {
		state.BranchID = strings.TrimSpace(c.session.ActiveBranch())
	}
	return state
}

func (c *SessionConversation) ResolveInterruption(id string) error {
	if c == nil || c.session == nil {
		return fmt.Errorf("会话不存在")
	}
	return c.session.ResolveInterruption(id)
}

// ── ContextToolConversation 实现（模型自主压缩工具，Plan.md §12 / T1）─────────────

// CompressContextRange 把原始消息的 1-based 闭区间 [Start, End] 压缩为摘要占位符。
// fold 模式：保留磁盘原始历史，只改投影（可重投影）；compact 模式：写新压缩边界（M6 语义）。
func (c *SessionConversation) CompressContextRange(ctx context.Context, input ContextToolCompressInput) (ContextToolCompressResult, error) {
	if c == nil || c.session == nil {
		return ContextToolCompressResult{}, fmt.Errorf("会话不存在")
	}
	raw := c.session.GetMessages()
	total := len(raw)
	start := input.Start - 1 // 1-based → 0-based
	end := input.End         // 闭区间 → 半开区间右端
	if start < 0 {
		start = 0
	}
	if end > total {
		end = total
	}
	if start >= end {
		return ContextToolCompressResult{SkippedReason: "empty_range"}, nil
	}
	tokensBefore := EstimateContextTokens(raw, nil)
	mode := strings.TrimSpace(input.Mode)
	if mode == "" {
		mode = contextToolModeFold
	}
	retainedTurns := policyRetainedTurns(c.cfg, c.agentKind)

	if mode == contextToolModeFold {
		var summary string
		var newMessages []*schema.Message
		if strings.TrimSpace(input.Summary) != "" {
			summary = strings.TrimSpace(input.Summary)
			newMessages = foldMessagesForModel(raw, summary, start, end, retainedTurns)
		} else {
			var foldResult ContextFoldResult
			var err error
			newMessages, foldResult, err = FoldContext(ctx, c.cfg, c.agentKind, FoldContextInput{
				Messages:      raw,
				FoldStart:     start,
				FoldEnd:       end,
				Phase:         contextCompactionPhasePreRun,
				RetainedTurns: retainedTurns,
			})
			if err != nil {
				return ContextToolCompressResult{}, err
			}
			summary = foldResult.Summary
		}
		tokensAfter := EstimateContextTokens(newMessages, nil)
		record := session.ContextFold{
			Type:               "context_fold",
			AgentKind:          c.agentKind,
			Summary:            summary,
			SourceStartIndex:   start,
			SourceEndIndex:     end,
			SourceMessageCount: end - start,
			RetainedTurns:      retainedTurns,
			TokensBefore:       tokensBefore,
			TokensAfter:        tokensAfter,
			Reason:             "model_compress_tool",
			Phase:              contextCompactionPhasePreRun,
			CreatedAt:          time.Now().UTC(),
		}
		if _, err := c.session.AppendContextFold(record); err != nil {
			return ContextToolCompressResult{}, err
		}
		return ContextToolCompressResult{
			Triggered:    true,
			Summary:      summary,
			TokensBefore: tokensBefore,
			TokensAfter:  tokensAfter,
			Note:         "fold: raw history preserved, projection updated next run",
		}, nil
	}

	// compact 模式：写新压缩边界（M6 语义）
	source := compactionContextMessages(raw[start:end])
	existingCheckpoint := ""
	if compaction, ok := c.session.LatestContextCompaction(c.agentKind); ok {
		existingCheckpoint = compaction.Summary
	}
	var summary string
	if strings.TrimSpace(input.Summary) != "" {
		summary = strings.TrimSpace(input.Summary)
	} else {
		policy := c.compactionPolicy()
		sourceTokens := EstimateContextTokens(source, nil)
		var err error
		summary, _, err = summarizeContextForCompaction(ctx, c.cfg, c.agentKind, existingCheckpoint, source, c.memoryNotesReferenceContext(), sourceTokens, policy, func(_ int, _ string) {})
		if err != nil {
			return ContextToolCompressResult{}, err
		}
	}
	epoch := c.nextCompactionEpoch()
	policy := c.compactionPolicy()
	newMessages := compactMessagesForModel(raw, summary, epoch, retainedTurns)
	tokensAfter := EstimateContextTokens(newMessages, nil)
	record := session.ContextCompaction{
		Type:                "context_compaction",
		AgentKind:           c.agentKind,
		Epoch:               epoch,
		Summary:             summary,
		SourceStartIndex:    start,
		SourceEndIndex:      end,
		SourceMessageCount:  end - start,
		RetainedTurns:       retainedTurns,
		TokensBefore:        tokensBefore,
		TokensAfter:         tokensAfter,
		ContextWindowTokens: policy.ContextWindowTokens,
		Strategy:            policy.Strategy,
		Threshold:           policy.Threshold,
		Reason:              "model_compress_tool",
		Phase:               contextCompactionPhasePreRun,
		CreatedAt:           time.Now().UTC(),
	}
	record, err := c.session.AppendContextCompaction(record)
	if err != nil {
		return ContextToolCompressResult{}, err
	}
	return ContextToolCompressResult{
		Triggered:    true,
		Summary:      summary,
		TokensBefore: tokensBefore,
		TokensAfter:  tokensAfter,
		Note:         fmt.Sprintf("compact: new boundary epoch=%d", record.Epoch),
	}, nil
}

// SearchContextHistory 在原始会话消息中做大小写不敏感子串搜索，返回最多 8 条命中。
func (c *SessionConversation) SearchContextHistory(query string) []contextToolSearchHit {
	if c == nil || c.session == nil {
		return nil
	}
	raw := c.session.GetMessages()
	q := strings.ToLower(query)
	hits := make([]contextToolSearchHit, 0, contextToolSearchMaxHits)
	for i, msg := range raw {
		if msg == nil || msg.Content == "" {
			continue
		}
		pos := strings.Index(strings.ToLower(msg.Content), q)
		if pos < 0 {
			continue
		}
		half := contextToolSearchSnippetChars / 2
		snippetStart := pos - half
		if snippetStart < 0 {
			snippetStart = 0
		}
		snippetEnd := pos + half
		if snippetEnd > len(msg.Content) {
			snippetEnd = len(msg.Content)
		}
		hits = append(hits, contextToolSearchHit{
			Index:   i + 1,
			Role:    string(msg.Role),
			Snippet: msg.Content[snippetStart:snippetEnd],
		})
		if len(hits) >= contextToolSearchMaxHits {
			break
		}
	}
	return hits
}

// ContextToolStatus 报告当前上下文管理状态（token 占比、压缩/折叠块、最近消息预览）。
func (c *SessionConversation) ContextToolStatus(tools []*schema.ToolInfo) contextToolStatusResult {
	if c == nil || c.session == nil {
		return contextToolStatusResult{Schema: "acp_status.v1"}
	}
	policy := c.compactionPolicy()
	effective := c.session.GetEffectiveMessages()
	estimated := EstimateContextTokens(effective, tools)
	window := policy.ContextWindowTokens
	ratio := 0.0
	if window > 0 {
		ratio = float64(estimated) / float64(window)
	}
	compaction, compactionActive := c.session.LatestContextCompaction(c.agentKind)
	fold, foldActive := c.session.LatestContextFold(c.agentKind)
	result := contextToolStatusResult{
		Schema:              "acp_status.v1",
		EstimatedTokens:     estimated,
		ContextWindowTokens: window,
		UsageRatio:          ratio,
		RawMessageCount:     c.session.MessageCountTotal(),
		CompactionActive:    compactionActive,
		FoldActive:          foldActive,
	}
	if compactionActive {
		result.RecentCompactions = append(result.RecentCompactions, contextToolStatusRecord{
			IndexRange:   fmt.Sprintf("[%d, %d]", compaction.SourceStartIndex+1, compaction.SourceEndIndex),
			TokensBefore: compaction.TokensBefore,
			TokensAfter:  compaction.TokensAfter,
			CreatedAt:    compaction.CreatedAt.Format(time.RFC3339),
		})
	}
	if foldActive {
		result.RecentFolds = append(result.RecentFolds, contextToolStatusRecord{
			IndexRange:   fmt.Sprintf("[%d, %d]", fold.SourceStartIndex+1, fold.SourceEndIndex),
			TokensBefore: fold.TokensBefore,
			TokensAfter:  fold.TokensAfter,
			CreatedAt:    fold.CreatedAt.Format(time.RFC3339),
		})
	}
	raw := c.session.GetMessages()
	total := len(raw)
	startIdx := total - contextToolPreviewMessages
	if startIdx < 0 {
		startIdx = 0
	}
	for i := startIdx; i < total; i++ {
		msg := raw[i]
		if msg == nil {
			continue
		}
		preview := msg.Content
		if len(preview) > 120 {
			preview = preview[:120] + "…"
		}
		result.RecentMessages = append(result.RecentMessages, contextToolStatusMessage{
			Index:   i + 1,
			Role:    string(msg.Role),
			Preview: preview,
		})
	}
	return result
}
