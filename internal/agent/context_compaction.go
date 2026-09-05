package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/observability"
	"denova/internal/session"
)

const (
	contextCompactionPhasePreRun = "pre_run"
	contextCompactionPhaseMidRun = "mid_run"
	contextCompactionReasonLimit = "context_usage_threshold"

	contextCompactionSummaryPrefix = "[Denova Context Compaction]"
	contextCompactionMaxAttempts   = 2
)

type contextCompactionPolicy struct {
	AgentKind           string
	Enabled             bool
	Strategy            string
	ContextWindowTokens int
	Threshold           float64
	RetainedTurns       int
	TargetMinRatio      float64
	TargetMaxRatio      float64
}

type ContextCompactionResult struct {
	Triggered                bool
	SkippedReason            string
	Phase                    string
	TokensBefore             int
	TokensAfter              int
	ProjectedTokensBefore    int
	ProjectedTokensAfter     int
	ReservedCompletionTokens int
	ReservedToolResultTokens int
	ContextWindowTokens      int
	Strategy                 string
	Threshold                float64
	Epoch                    int
	Summary                  string
	TargetRatio              float64
	SourceMessageCount       int
	MessageCountBefore       int
	MessageCountAfter        int
	RetainedTurns            int
}

type contextCompactionSummaryFunc func(ctx context.Context, cfg *config.Config, agentKind string, existingCheckpoint string, source []*schema.Message, referenceContext string, sourceTokens int, policy contextCompactionPolicy, emitDelta func(attempt int, delta string)) (string, int, error)

type contextCompactionController struct {
	conversation ContextCompactionConversation
}

// ContextCompactionConversation is implemented by conversations that can
// persist and rebuild model-visible compaction epochs.
type ContextCompactionConversation interface {
	CompactContextIfNeeded(ctx context.Context, input ContextCompactionInput) ([]*schema.Message, ContextCompactionResult, error)
}

// FoldConversation is implemented by conversations that can fold the incremental
// source into a summary placeholder without creating a new boundary or deleting
// raw history. It is mutually exclusive with full compaction.
type FoldConversation interface {
	FoldContextIfNeeded(ctx context.Context, input ContextCompactionInput) ([]*schema.Message, ContextFoldResult, error)
}

type ContextCompactionInput struct {
	Messages            []*schema.Message
	SourceMessages      []*schema.Message
	Tools               []*schema.ToolInfo
	AgentMessage        string
	Phase               string
	Emit                func(Event)
	Force               bool
	ExistingCheckpoint  string
	ContextWindowTokens int
	// ReservedCompletionTokens and ReservedToolResultTokens make compaction
	// decisions against projected context usage, not only the prompt assembled
	// before the next model/tool step.
	ReservedCompletionTokens int
	ReservedToolResultTokens int
	ReferenceContext         string
	KeepLatestUser           bool
}

type contextCompactionContextKey struct{}

var summarizeContextForCompaction contextCompactionSummaryFunc = generateContextCompactionSummary

// SummarizeContextForCompactionForTest returns the current summarize function so
// tests can capture and restore it. It is exported only to allow cross-package
// test stubbing of the LLM-backed summarizer.
func SummarizeContextForCompactionForTest() contextCompactionSummaryFunc {
	return summarizeContextForCompaction
}

// RestoreSummarizeContextForCompactionForTest restores a previously captured
// summarize function. It is exported only for test use.
func RestoreSummarizeContextForCompactionForTest(fn contextCompactionSummaryFunc) {
	summarizeContextForCompaction = fn
}

// StubSummarizeContextForCompactionForTest installs a summarize function that
// always returns the given summary with the reported input character count. It
// is exported only for cross-package test stubbing.
func StubSummarizeContextForCompactionForTest(summary string, inputChars int) func() {
	previous := summarizeContextForCompaction
	summarizeContextForCompaction = func(_ context.Context, _ *config.Config, _ string, _ string, source []*schema.Message, _ string, _ int, _ contextCompactionPolicy, _ func(int, string)) (string, int, error) {
		return summary, inputChars, nil
	}
	return func() { summarizeContextForCompaction = previous }
}

func contextWithCompactionController(ctx context.Context, conversation Conversation) context.Context {
	compaction, ok := conversation.(ContextCompactionConversation)
	if !ok || compaction == nil {
		return ctx
	}
	return context.WithValue(ctx, contextCompactionContextKey{}, &contextCompactionController{conversation: compaction})
}

func compactionControllerFromContext(ctx context.Context) *contextCompactionController {
	controller, _ := ctx.Value(contextCompactionContextKey{}).(*contextCompactionController)
	return controller
}

func resolveContextCompactionPolicy(cfg *config.Config, agentKind string) contextCompactionPolicy {
	contextSettings := config.ResolveAgentContext(cfg, agentKind)
	compactionSettings := config.ResolveAgentContext(cfg, config.AgentKindContextCompaction)
	modelSettings := config.ResolveAgentModel(cfg, agentKind)
	// T1: Use the actual context window (from /v1/models probe or server
	// rejection calibration) when it is smaller than the declared window.
	// Both sources are cached per base URL; if neither has produced a value,
	// the declared window is used unchanged.
	declaredWindow := modelSettings.ContextWindowTokens
	actualWindow := knownActualContextWindow(modelSettings.OpenAIBaseURL)
	return contextCompactionPolicy{
		AgentKind:           agentKind,
		Enabled:             contextSettings.CompactionEnabled,
		Strategy:            contextSettings.CompactionStrategy,
		ContextWindowTokens: effectiveContextWindow(declaredWindow, actualWindow),
		Threshold:           contextSettings.CompactionThreshold,
		RetainedTurns:       compactionSettings.CompactionRecentTurns,
		TargetMinRatio:      compactionSettings.CompactionTargetMin,
		TargetMaxRatio:      compactionSettings.CompactionTargetMax,
	}
}

func (p contextCompactionPolicy) triggerTokens() int {
	if !p.Enabled || p.ContextWindowTokens <= 0 || p.Threshold <= 0 {
		return 0
	}
	// T1: Apply the physical safety ratio so the compaction trigger fires
	// before the hardware (KV cache + prefill buffers) OOMs on long context.
	// The declared/actual window may be far larger than what 24GB VRAM can
	// sustain; scaling by toolModePhysicalSafetyRatio (0.85) produces a
	// conservative trigger line.
	physical := physicalSafeWindow(p.ContextWindowTokens)
	return int(float64(physical) * p.Threshold)
}

func (p contextCompactionPolicy) shouldCompact(tokens int, force bool) (bool, string) {
	if force {
		return true, ""
	}
	if !p.Enabled {
		return false, "disabled"
	}
	if p.ContextWindowTokens <= 0 {
		return false, "context_window_tokens_missing"
	}
	trigger := p.triggerTokens()
	if trigger <= 0 {
		return false, "threshold_invalid"
	}
	if tokens < trigger {
		return false, "below_threshold"
	}
	return true, ""
}

// ── 三层分层压缩（Plan.md §12 / T2，借鉴 ACP / billion-context）─────────────────
//
// 摘要池分三层：T1 capture（原始→详细 ~45×）、T2 distill（T1→决策/结果 ~10×）、
// T3 condense（T2→裸事实 ~5×）。每层独立触发阈值；高层蒸馏后低层 token 清零
// （被高层取代）。投影使用最高可用层，保证投影确定性、不每轮重新压缩（H 修正）。
//
// 纯函数（nextTier/projectionSummary）便于单测；runTieredDistill 负责实际 LLM 蒸馏。

// contextTieredThresholds 是三层触发阈值（token 数），0 表示该层不触发。
type contextTieredThresholds struct {
	T1 int
	T2 int
	T3 int
}

// tieredThresholdsFromSettings 从解析后的配置读取三层阈值（token 数）。
func tieredThresholdsFromSettings(s config.ResolvedAgentContextSettings) contextTieredThresholds {
	return contextTieredThresholds{
		T1: int(s.TieredT1Threshold),
		T2: int(s.TieredT2Threshold),
		T3: int(s.TieredT3Threshold),
	}
}

// tieredNextTier 决定下一层应执行的压缩层级（纯函数，便于单测）。
// 返回 (tier, reason)：tier ∈ {1,2,3} 表示应执行该层；0 表示无需压缩。
// T1 优先级最高（!shouldInject 守卫）：原始池超限时先捕获，再考虑蒸馏。
func tieredNextTier(p session.ContextTieredPool, th contextTieredThresholds) (int, string) {
	if th.T1 > 0 && p.RawTokens >= th.T1 {
		return 1, "raw_pool_over_t1"
	}
	if th.T2 > 0 && p.T1Tokens >= th.T2 {
		return 2, "t1_pool_over_t2"
	}
	if th.T3 > 0 && p.T2Tokens >= th.T3 {
		return 3, "t2_pool_over_t3"
	}
	return 0, "below_threshold"
}

// tieredProjectionSummary 返回投影应使用的摘要（最高可用层：T3 > T2 > T1）。
// 投影恒定：同一池状态总是返回同一摘要，避免每轮重新压缩。
func tieredProjectionSummary(p session.ContextTieredPool) string {
	switch {
	case strings.TrimSpace(p.T3Summary) != "":
		return p.T3Summary
	case strings.TrimSpace(p.T2Summary) != "":
		return p.T2Summary
	case strings.TrimSpace(p.T1Summary) != "":
		return p.T1Summary
	default:
		return ""
	}
}

// tieredPoolActive 报告池是否已有任意层摘要。
func tieredPoolActive(p session.ContextTieredPool) bool {
	return strings.TrimSpace(p.T1Summary) != "" ||
		strings.TrimSpace(p.T2Summary) != "" ||
		strings.TrimSpace(p.T3Summary) != ""
}

// runTieredDistill 在 T1 捕获完成后执行 T2/T3 蒸馏级联（高层蒸馏后低层清零）。
// 返回更新后的池。T1 捕获由调用方完成（复用现有增量摘要），这里只做 T2→T3 级联。
func runTieredDistill(ctx context.Context, cfg *config.Config, agentKind string, pool session.ContextTieredPool, th contextTieredThresholds, policy contextCompactionPolicy) (session.ContextTieredPool, error) {
	// T2 distill：T1 摘要 → 决策/结果
	if th.T2 > 0 && pool.T1Tokens >= th.T2 {
		summary, _, err := summarizeContextForCompaction(ctx, cfg, agentKind, "", []*schema.Message{schema.UserMessage(pool.T1Summary)}, "", pool.T1Tokens, policy, func(_ int, _ string) {})
		if err != nil {
			return pool, err
		}
		pool.T2Summary = summary
		pool.T2Tokens = estimateStringTokens(summary)
		pool.T1Tokens = 0 // T1 被 T2 取代
	}
	// T3 condense：T2 摘要 → 裸事实
	if th.T3 > 0 && pool.T2Tokens >= th.T3 {
		summary, _, err := summarizeContextForCompaction(ctx, cfg, agentKind, "", []*schema.Message{schema.UserMessage(pool.T2Summary)}, "", pool.T2Tokens, policy, func(_ int, _ string) {})
		if err != nil {
			return pool, err
		}
		pool.T3Summary = summary
		pool.T3Tokens = estimateStringTokens(summary)
		pool.T2Tokens = 0 // T2 被 T3 取代
	}
	return pool, nil
}

// ── 压缩候选优先级排序（Plan.md §12 / T4，借鉴 ACP T1 优先级）─────────────────
//
// 当模型/系统选择压缩范围时，按内容类型决定"先压什么"：
// 子代理审查/咨询 > 冗长命令输出 > 死路探索 > 冗余工具结果 > 多步任务中间步骤
// > 已解决讨论线程 > 大文件已使用。
// 排序结果用于引导（提示词/工具描述），不强制截断，保持投影稳定。

// compactionCandidatePriority 是压缩候选的优先级类别（数值越小越先压缩）。
type compactionCandidatePriority int

const (
	compactionPrioritySubAgentReview        compactionCandidatePriority = iota // 子代理审查/咨询
	compactionPriorityVerboseCommand                                           // 冗长命令输出
	compactionPriorityDeadEnd                                                  // 死路探索
	compactionPriorityRedundantToolResult                                      // 冗余工具结果
	compactionPriorityMultiStepIntermediate                                    // 多步任务中间步骤
	compactionPriorityResolvedDiscussion                                       // 已解决讨论线程
	compactionPriorityLargeFileUsed                                            // 大文件已使用
	compactionPriorityDefault                                                  // 未分类（最后压缩）
)

// compactionCandidate 是一条可压缩消息的轻量描述（用于排序，不持有完整消息）。
type compactionCandidate struct {
	Index    int // 0-based 消息下标
	Priority compactionCandidatePriority
	Tokens   int
}

// classifyCompactionCandidate 按内容类型给单条消息分类（纯函数，便于单测）。
// 规则保守：仅当特征明显时归入高优先级类别，否则归入 Default。
func classifyCompactionCandidate(msg *schema.Message) compactionCandidatePriority {
	if msg == nil {
		return compactionPriorityDefault
	}
	content := strings.ToLower(msg.Content)
	switch {
	case msg.Role == schema.Tool:
		switch {
		// 冗长命令输出：工具结果且内容很长（命令/构建/测试输出）。
		case estimateStringTokens(msg.Content) >= 800:
			return compactionPriorityVerboseCommand
		// 冗余工具结果：重复/空/纯确认类短结果。
		case isRedundantToolResult(msg):
			return compactionPriorityRedundantToolResult
		}
		return compactionPriorityDefault
	case msg.Role == schema.Assistant:
		switch {
		// 子代理审查/咨询：assistant 消息引用子代理委派或审查结论。
		case strings.Contains(content, "subagent") || strings.Contains(content, "sub_agent") ||
			strings.Contains(content, "子代理") || strings.Contains(content, "审查结论"):
			return compactionPrioritySubAgentReview
		// 死路探索：明确标记尝试失败/走不通。
		case strings.Contains(content, "dead end") || strings.Contains(content, "死路") ||
			strings.Contains(content, "走不通") || strings.Contains(content, "此路不通"):
			return compactionPriorityDeadEnd
		// 多步任务中间步骤：编号步骤/中间产物描述。
		case strings.Contains(content, "step ") || strings.Contains(content, "步骤 ") ||
			strings.Contains(content, "intermediate"):
			return compactionPriorityMultiStepIntermediate
		// 已解决讨论线程：明确标记已解决/已确认。
		case strings.Contains(content, "resolved") || strings.Contains(content, "已解决") ||
			strings.Contains(content, "已确认"):
			return compactionPriorityResolvedDiscussion
		}
		return compactionPriorityDefault
	case msg.Role == schema.User:
		// 大文件已使用：用户粘贴的大段内容（通常已被后续引用）。
		if estimateStringTokens(msg.Content) >= 1500 {
			return compactionPriorityLargeFileUsed
		}
		return compactionPriorityDefault
	}
	return compactionPriorityDefault
}

// isRedundantToolResult 判断工具结果是否冗余（空/纯确认/重复标记）。
func isRedundantToolResult(msg *schema.Message) bool {
	trimmed := strings.TrimSpace(msg.Content)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, marker := range []string{"ok", "success", "done", "completed", "no output", "（无输出）", "成功"} {
		if lower == marker {
			return true
		}
	}
	return false
}

// rankCompactionCandidates 把消息序列转成按优先级排序的候选列表。
// 排序键：优先级类别（小→大），同类内按 token 降序（大的先压），再按下标升序（稳定）。
// 纯函数，便于单测。
func rankCompactionCandidates(messages []*schema.Message) []compactionCandidate {
	candidates := make([]compactionCandidate, 0, len(messages))
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		candidates = append(candidates, compactionCandidate{
			Index:    i,
			Priority: classifyCompactionCandidate(msg),
			Tokens:   estimateStringTokens(msg.Content),
		})
	}
	sort.SliceStable(candidates, func(a, b int) bool {
		if candidates[a].Priority != candidates[b].Priority {
			return candidates[a].Priority < candidates[b].Priority
		}
		if candidates[a].Tokens != candidates[b].Tokens {
			return candidates[a].Tokens > candidates[b].Tokens
		}
		return candidates[a].Index < candidates[b].Index
	})
	return candidates
}

// compactionPriorityGuidance 是注入压缩提示词的优先级引导文本（双语）。
// 对应 ACP T1 优先级 + billion-context STAGED_COMPRESS_GUIDANCE 的 tail-biased 思想。
func compactionPriorityGuidance() string {
	return strings.TrimSpace(`
压缩优先级（先压什么）：
- 子代理审查/咨询输出 > 冗长命令输出 > 死路探索 > 冗余工具结果 > 多步任务中间步骤 > 已解决讨论线程 > 大文件已使用。
- 优先压缩上述高优先级内容；用户消息与未闭环事项最后压缩。

尾部偏置（tail-biased）折叠引导：
- 压缩范围尽量偏向对话尾部（最近的消息），保持较早的稳定前缀不变，以最大化 KV cache 命中。
- 若必须压缩较早内容，优先选择上述高优先级类别，避免无差别从头截断。
`)
}

func BuildContextCompaction(ctx context.Context, cfg *config.Config, agentKind string, input ContextCompactionInput, epoch int) ([]*schema.Message, ContextCompactionResult, error) {
	policy := resolveContextCompactionPolicy(cfg, agentKind)
	if input.ContextWindowTokens > 0 {
		policy.ContextWindowTokens = input.ContextWindowTokens
	}
	phase := strings.TrimSpace(input.Phase)
	if phase == "" {
		phase = contextCompactionPhasePreRun
	}
	input = withDefaultContextProjectionReserves(cfg, agentKind, input, 0)
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
	source := compactionSourceMessages(compactionSourceBaseMessages(input), input.KeepLatestUser)
	if len(source) == 0 && strings.TrimSpace(input.ExistingCheckpoint) == "" && strings.TrimSpace(input.ReferenceContext) == "" {
		result.SkippedReason = "empty_source"
		return input.Messages, result, nil
	}
	sourceTokens := EstimateContextTokens(source, nil)
	emitContextCompactionEvent(input.Emit, phase, "started", result)
	summary, inputChars, err := summarizeContextForCompaction(ctx, cfg, agentKind, input.ExistingCheckpoint, source, input.ReferenceContext, sourceTokens, policy, func(attempt int, delta string) {
		emitContextCompactionDeltaEvent(input.Emit, phase, result, attempt, delta)
	})
	if err != nil {
		emitContextCompactionEvent(input.Emit, phase, "failed", result)
		return input.Messages, result, err
	}
	if epoch <= 0 {
		epoch = 1
	}
	newMessages := compactMessagesForModel(input.Messages, summary, epoch, policy.RetainedTurns)
	result.Triggered = true
	result.Epoch = epoch
	result.Summary = summary
	result.TokensAfter = EstimateContextTokens(newMessages, input.Tools)
	result.ProjectedTokensAfter = projectedContextTokens(result.TokensAfter, input)
	result.TargetRatio = contextCompactionRatio(countRunes(summary), inputChars)
	result.SourceMessageCount = len(source)
	result.MessageCountAfter = len(newMessages)
	emitContextCompactionEvent(input.Emit, phase, "completed", result)
	return newMessages, result, nil
}

// EstimateContextProjectionReserves returns bounded reserves for completion and
// retained tool results. expectedOutputChars should be the user-configured
// target when one exists; otherwise a small model-relative reserve is used.
func EstimateContextProjectionReserves(cfg *config.Config, agentKind string, expectedOutputChars int) (completionTokens, toolResultTokens int) {
	model := config.ResolveAgentModel(cfg, agentKind)
	window := model.ContextWindowTokens
	completionTokens = expectedOutputChars
	if completionTokens <= 0 {
		completionTokens = max(2048, window/50)
	} else {
		// Leave room for the hidden structured result and normal completion
		// variance around the visible user-configured target.
		completionTokens += max(1024, expectedOutputChars/4)
	}
	if window > 0 {
		completionTokens = min(completionTokens, max(2048, window/4))
	}
	toolPolicy := resolveToolResultContextPolicy(cfg, agentKind).normalized()
	if toolPolicy.Enabled {
		// A result is bounded at the tool boundary before it is persisted. Reserve
		// for one such result; older exchanges are owned by normal compaction.
		toolResultTokens = toolPolicy.MaxResultBytes / 3
		if window > 0 {
			toolResultTokens = min(toolResultTokens, max(1024, window/10))
		}
	}
	return completionTokens, toolResultTokens
}

func withDefaultContextProjectionReserves(cfg *config.Config, agentKind string, input ContextCompactionInput, expectedOutputChars int) ContextCompactionInput {
	completion, tools := EstimateContextProjectionReserves(cfg, agentKind, expectedOutputChars)
	if input.ReservedCompletionTokens <= 0 {
		input.ReservedCompletionTokens = completion
	}
	if input.ReservedToolResultTokens <= 0 {
		input.ReservedToolResultTokens = tools
	}
	return input
}

func projectedContextTokens(promptTokens int, input ContextCompactionInput) int {
	return max(1, promptTokens+max(0, input.ReservedCompletionTokens)+max(0, input.ReservedToolResultTokens))
}

func compactionSourceBaseMessages(input ContextCompactionInput) []*schema.Message {
	if len(input.SourceMessages) > 0 {
		return input.SourceMessages
	}
	return input.Messages
}

func EstimateContextTokens(messages []*schema.Message, tools []*schema.ToolInfo) int {
	tokens := 0
	for _, msg := range messages {
		tokens += estimateMessageTokens(msg)
	}
	if len(tools) > 0 {
		data, err := json.Marshal(tools)
		if err == nil {
			tokens += estimateStringTokens(string(data))
		} else {
			tokens += len(tools) * 128
		}
	}
	if tokens < 1 {
		return 1
	}
	return tokens
}

func estimateMessageTokens(msg *schema.Message) int {
	if msg == nil {
		return 0
	}
	tokens := 4 + estimateStringTokens(string(msg.Role)) + estimateStringTokens(msg.Content)
	tokens += estimateStringTokens(msg.ReasoningContent)
	if len(msg.ToolCalls) > 0 {
		if data, err := json.Marshal(msg.ToolCalls); err == nil {
			tokens += estimateStringTokens(string(data))
		}
	}
	if len(msg.MultiContent) > 0 {
		if data, err := json.Marshal(msg.MultiContent); err == nil {
			tokens += estimateStringTokens(string(data))
		}
	}
	if len(msg.UserInputMultiContent) > 0 {
		if data, err := json.Marshal(msg.UserInputMultiContent); err == nil {
			tokens += estimateStringTokens(string(data))
		}
	}
	if len(msg.AssistantGenMultiContent) > 0 {
		if data, err := json.Marshal(msg.AssistantGenMultiContent); err == nil {
			tokens += estimateStringTokens(string(data))
		}
	}
	if msg.ToolName != "" {
		tokens += estimateStringTokens(msg.ToolName)
	}
	if msg.ToolCallID != "" {
		tokens += estimateStringTokens(msg.ToolCallID)
	}
	return tokens
}

func estimateStringTokens(content string) int {
	if content == "" {
		return 0
	}
	tokens := 0
	asciiRunes := 0
	flushASCII := func() {
		if asciiRunes == 0 {
			return
		}
		tokens += (asciiRunes + 3) / 4
		asciiRunes = 0
	}
	for _, r := range content {
		if r <= unicode.MaxASCII {
			asciiRunes++
			continue
		}
		flushASCII()
		tokens++
	}
	flushASCII()
	if tokens < 1 {
		return 1
	}
	return tokens
}

func NewContextCompactionSummaryMessage(epoch int, summary string) *schema.Message {
	return schema.UserMessage(fmt.Sprintf("%s epoch=%d\n\n%s", contextCompactionSummaryPrefix, epoch, strings.TrimSpace(summary)))
}

func isContextCompactionMessage(msg *schema.Message) bool {
	return msg != nil && strings.HasPrefix(strings.TrimSpace(msg.Content), contextCompactionSummaryPrefix)
}

// IsContextCompactionSummaryMessage reports whether msg is a model-visible
// context-checkpoint record produced by Denova's compaction pipeline.
func IsContextCompactionSummaryMessage(msg *schema.Message) bool {
	return isContextCompactionMessage(msg)
}

func compactMessagesForModel(messages []*schema.Message, summary string, epoch, retainedTurns int) []*schema.Message {
	systemMessages := make([]*schema.Message, 0)
	contextMessages := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || isContextCompactionMessage(msg) {
			continue
		}
		if msg.Role == schema.System {
			systemMessages = append(systemMessages, msg)
			continue
		}
		contextMessages = append(contextMessages, msg)
	}
	tail := retainTailByUserTurns(contextMessages, retainedTurns)
	result := make([]*schema.Message, 0, len(systemMessages)+1+len(tail))
	result = append(result, systemMessages...)
	result = append(result, NewContextCompactionSummaryMessage(epoch, summary))
	result = append(result, tail...)
	return result
}

func compactionContextMessages(messages []*schema.Message) []*schema.Message {
	filtered := make([]*schema.Message, 0, len(messages))
	for _, msg := range messages {
		if msg == nil || isContextCompactionMessage(msg) {
			continue
		}
		filtered = append(filtered, msg)
	}
	return filtered
}

// BuildCompactedModelMessages rebuilds model-visible history after a compaction
// record is persisted and its final epoch is known.
func BuildCompactedModelMessages(messages []*schema.Message, summary string, epoch, retainedTurns int) []*schema.Message {
	return compactMessagesForModel(messages, summary, epoch, retainedTurns)
}

func generateContextCompactionSummary(ctx context.Context, cfg *config.Config, agentKind string, existingCheckpoint string, source []*schema.Message, referenceContext string, sourceTokens int, policy contextCompactionPolicy, emitDelta func(attempt int, delta string)) (string, int, error) {
	var runErr error
	traceCtx, finishTrace := withStandaloneRunTrace(ctx, cfg, config.AgentKindContextCompaction, "context_compaction", "generate", map[string]any{
		"source_agent_kind": strings.TrimSpace(agentKind),
		"source_messages":   len(source),
		"source_tokens":     sourceTokens,
	})
	defer func() { finishTrace(runErr) }()
	// The compaction prompt is owned by the compaction agent, but the model must
	// follow the agent whose context is being compacted. This keeps a stale
	// dedicated compaction profile from routing every summary through an old
	// model.
	modelCfg := chatModelConfigForAgent(cfg, agentKind)
	inputChars := contextCompactionInputChars(existingCheckpoint, source, referenceContext)
	cm, err := openai.NewChatModel(traceCtx, &modelCfg)
	if err != nil {
		runErr = err
		return "", inputChars, fmt.Errorf("创建上下文压缩模型失败: %w", err)
	}
	systemPrompt := protectedSystemInstruction(cfg, config.AgentKindContextCompaction, contextCompactionSystemInstruction())
	var summary string
	var retryReason string
	for attempt := 1; attempt <= contextCompactionMaxAttempts; attempt++ {
		input := []*schema.Message{
			schema.SystemMessage(systemPrompt),
			schema.UserMessage(buildContextCompactionTranscript(source, existingCheckpoint, referenceContext, sourceTokens, inputChars, retryReason, policy)),
		}
		mode := fmt.Sprintf("stream_attempt_%d", attempt)
		span, callID, llmTraceCtx := beginLLMCallTrace(traceCtx, config.AgentKindContextCompaction, "context_compaction", mode, modelCfg, input, nil, true)
		msg, err := streamContextCompactionAttempt(llmTraceCtx, cm, input, attempt, emitDelta)
		if err != nil {
			finishLLMCallTrace(span, callID, config.AgentKindContextCompaction, "context_compaction", mode, modelCfg.Model, attempt, nil, err, nil)
			runErr = err
			return "", inputChars, fmt.Errorf("上下文压缩失败: %w", err)
		}
		finishLLMCallTrace(span, callID, config.AgentKindContextCompaction, "context_compaction", mode, modelCfg.Model, attempt, msg, nil, nil)
		summary = strings.TrimSpace(msg.Content)
		if summary == "" {
			runErr = fmt.Errorf("上下文压缩结果为空")
			return "", inputChars, runErr
		}
		summaryChars := countRunes(summary)
		minChars, maxChars := compactionTargetCharRange(inputChars, policy)
		if inputChars <= 0 || (summaryChars >= minChars && summaryChars <= maxChars) {
			return summary, inputChars, nil
		}
		if attempt == contextCompactionMaxAttempts {
			break
		}
		retryReason = contextCompactionRetryInstruction(summaryChars, minChars, maxChars)
	}
	return summary, inputChars, nil
}

func streamContextCompactionAttempt(ctx context.Context, cm *openai.ChatModel, input []*schema.Message, attempt int, emitDelta func(attempt int, delta string)) (*schema.Message, error) {
	stream, err := cm.Stream(ctx, input)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	var chunks []*schema.Message
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if msg == nil {
			continue
		}
		chunks = append(chunks, msg)
		if msg.Content != "" && emitDelta != nil {
			emitDelta(attempt, msg.Content)
		}
	}
	return schema.ConcatMessages(chunks)
}

func contextCompactionRatio(partChars, inputChars int) float64 {
	if inputChars <= 0 {
		return 0
	}
	return float64(partChars) / float64(inputChars)
}

func compactionTargetRange(policy contextCompactionPolicy) string {
	minRatio := policy.TargetMinRatio
	if minRatio <= 0 {
		minRatio = 0.05
	}
	maxRatio := policy.TargetMaxRatio
	if maxRatio <= 0 {
		maxRatio = 0.20
	}
	if maxRatio < minRatio {
		maxRatio = minRatio
	}
	return fmt.Sprintf("%.0f%%-%.0f%%", minRatio*100, maxRatio*100)
}

func compactionTargetCharRange(inputChars int, policy contextCompactionPolicy) (int, int) {
	if inputChars <= 0 {
		return 0, 0
	}
	minRatio := policy.TargetMinRatio
	if minRatio <= 0 {
		minRatio = 0.05
	}
	maxRatio := policy.TargetMaxRatio
	if maxRatio <= 0 {
		maxRatio = 0.20
	}
	if maxRatio < minRatio {
		maxRatio = minRatio
	}
	minChars := int(float64(inputChars)*minRatio + 0.5)
	maxChars := int(float64(inputChars)*maxRatio + 0.5)
	if minChars < 1 {
		minChars = 1
	}
	if maxChars < minChars {
		maxChars = minChars
	}
	return minChars, maxChars
}

func contextCompactionRetryInstruction(summaryChars, minChars, maxChars int) string {
	if summaryChars > maxChars {
		return fmt.Sprintf("The previous summary was too long: %d characters. Compress it to %d-%d characters while preserving required facts.", summaryChars, minChars, maxChars)
	}
	return fmt.Sprintf("The previous summary was too short: %d characters. Expand it to %d-%d characters by restoring omitted user goals, events, relationships, tasks, and state changes.", summaryChars, minChars, maxChars)
}

func contextCompactionSystemInstruction() string {
	return strings.TrimSpace(`
你是 Denova 的独立“互动小说上下文压缩器”，用于类似酒馆/SillyTavern 的高轮次互动小说和长对话创作场景。

你的任务是从有界输入生成可重建的“历史上下文 checkpoint”，同时保留所有会对后续剧情、写作任务或用户意图产生长期影响的信息。checkpoint 不是新的事实真源；游戏模式的历史事实仍以带 turn_id 的 Turn 事件为准。

输入可能包含：
1. existing_checkpoint：此前已经从同一来源链压缩出的 checkpoint，可能为空。
2. reference_context：调用点显式提供的有界参考上下文，可能为空；不得假定存在任何额外记忆库。
3. new_context：上次压缩后新增的原始有效对话链或互动回合链，包括用户行动、用户对白、LLM 剧情推进、NPC 反应、环境变化、任务状态等。

处理目标：
- 将 existing_checkpoint、reference_context 与 new_context 合并，输出一份新的历史 checkpoint。
- 如果 existing_checkpoint 为空，则从 new_context 初始化；否则增量合并，不要重复记录同一事件。
- 游戏 Turn 输入包含 source turn_id 时，事件和因果结论必须保留相应 turn_id；无法确定来源时明确标记来源缺失，不得自造 ID。
- 不要删除旧 checkpoint 中的长期影响信息，除非 new_context 明确说明该信息已经失效、解决或被推翻。
- 如果出现矛盾，不要自行修正；保留矛盾并标记为“待确认矛盾”。
- 已完成任务可以压缩，但必须保留最终结果和遗留影响。
- 未完成任务、伏笔、承诺、债务、秘密、危险不能删除。
- 如果不确定某信息是否有长期影响，默认保留。
- 游戏模式的当前 Actor 数值、位置、资源和可计算关系以调用方单独注入的 Actor State 为准；checkpoint 只保留这些变化的历史原因和来源 Turn，不复制一份“当前状态真源”。
- 未来安排属于 DirectorPlan，稳定世界设定属于 Lore；只记录它们在输入中已经发生的变更或对历史事件的解释，不把计划写成既成事实。

压缩重点：
- 必须保留事件时间顺序。
- 必须保留所有用户消息的核心意图、关键行动、选择、对白、承诺、拒绝、欺骗、威胁、安慰、交易、背叛、失败尝试及其后果。
- 必须保留行动造成的后果和所有长期影响信息。
- 必须保留角色关系和状态变化的原因、世界/阵营变化、物品资源变动、能力变化、线索、秘密、伏笔、任务、危险与倒计时；游戏模式的当前值不在 checkpoint 中重复建账。
- 可以删除或合并氛围描写、重复心理描写、无后果闲聊、纯修辞性文本。
- 不要写成小说文风；要写成清晰、紧凑、可供后续模型继续创作的事实账本。
- 排除 thinking/reasoning 内容、传输噪音、展示用日志、重复工具卡片和无结果的实现过程。
- 必须保留会改变后续行为的工具证据：文件读取/搜索发现、资料库查询结论、工具报错原因、文件/状态写入结果、版本恢复/工作区状态变化，以及需要后续复用的 tool_result metadata（target、idempotency_key、truncated 等）。
- 禁止编造事实；不确定时明确标记“不确定”。
- 目标长度由用户消息配置控制，并按 existing_checkpoint、reference_context 与 new_context 的合计字符数计算，默认是输入字符数的 5%-20%。信息密度高时使用目标范围的上半区，不要为了短而丢长期影响信息。

长期影响信息判定：
只要某条信息未来可能影响角色反应、剧情分支、世界状态、任务推进、关系变化或玩家可用选择，就必须保留。以下信息一律视为长期影响信息：
- 用户行动：关键选择、重要话语、承诺/拒绝/欺骗/威胁/安慰/交易/背叛、失败的重要行动、尚未显现的后果。
- 角色关系：信任、好感、敌意、怀疑、依赖、恐惧、暧昧、愧疚、承诺、误会、秘密、冲突、债务、交易、NPC 间联盟/敌对/背叛/隐瞒。
- 角色状态：受伤、死亡、失踪、昏迷、被俘、身份暴露、能力觉醒/削弱、诅咒、污染、精神状态变化、已知/未知/误解的信息、目标/动机/立场变化。
- 世界与阵营状态：地点被破坏/封锁/占领/发现/改变、阵营态度变化、组织行动、通缉、追捕、战争、政治变化、世界规则、禁忌、异常现象、公共事件。
- 物品、资源与能力：获得、失去、损坏、使用、隐藏的重要物品；金钱、补给、武器、钥匙、信物、证据、药物、装备；技能、权限、身份、通行资格变化。
- 线索、秘密与伏笔：已发现线索、未解谜团、未兑现威胁、未完成任务、倒计时事件、约定地点/时间/暗号、隐藏身份/目的/计划、叙事确认但角色未必知道的信息。
- 用户意图与约束：用户明确目标、拒绝、偏好、任务边界、尚未完成的请求和已确认决策。

输出必须使用以下格式：

【历史事件时间线】
- [source turn_id 或消息序号] 谁做了什么，造成了什么长期影响

【历史因果与来源】
- 结论或变化 ← 原因与 source turn_id；矛盾和不确定性在这里标明

【未闭环事项】
- 伏笔、承诺、债务、秘密、危险、任务、倒计时及其来源

【用户意图与任务约束】
- 仍会影响后续行为的目标、偏好、拒绝、边界与已确认决策

` + compactionPriorityGuidance() + `
`)
}

func buildContextCompactionTranscript(messages []*schema.Message, existingCheckpoint, referenceContext string, sourceTokens, inputChars int, retryInstruction string, policy contextCompactionPolicy) string {
	blocks := make([]string, 0, len(messages))
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		blocks = append(blocks, formatCompactionMessage(i+1, msg))
	}
	minChars, maxChars := compactionTargetCharRange(inputChars, policy)
	var sb strings.Builder
	sb.WriteString("请按系统要求压缩以下 Denova 上下文。基于 existing_checkpoint、reference_context 与 new_context 增量生成新的历史 checkpoint，保留所有会影响后续剧情、任务、关系、世界状态或用户偏好的信息。\n")
	sb.WriteString(fmt.Sprintf("Estimated new context tokens: %d. Input characters across existing checkpoint, reference context, and new context: %d. Target summary length: %d-%d characters (%s of input characters). 不得低于下限；信息密度高时使用目标范围上半区。\n\n", sourceTokens, inputChars, minChars, maxChars, compactionTargetRange(policy)))
	if retryInstruction = strings.TrimSpace(retryInstruction); retryInstruction != "" {
		sb.WriteString("Retry instruction:\n")
		sb.WriteString(retryInstruction)
		sb.WriteString("\n\n")
	}
	sb.WriteString("<existing_checkpoint>\n")
	if existingCheckpoint = strings.TrimSpace(existingCheckpoint); existingCheckpoint != "" {
		sb.WriteString(existingCheckpoint)
		sb.WriteString("\n")
	} else {
		sb.WriteString("（未提供；本次输入从新增上下文与有界参考上下文初始化 checkpoint。）\n")
	}
	sb.WriteString("</existing_checkpoint>\n\n")
	if referenceContext = strings.TrimSpace(referenceContext); referenceContext != "" {
		sb.WriteString("<reference_context>\n")
		sb.WriteString(referenceContext)
		sb.WriteString("\n</reference_context>\n\n")
	}
	sb.WriteString("<new_context>\n")
	if len(blocks) == 0 {
		sb.WriteString("（无新增原始消息。）\n")
	} else {
		for _, block := range blocks {
			sb.WriteString(block)
		}
	}
	sb.WriteString("</new_context>\n")
	return sb.String()
}

func contextCompactionInputChars(existingCheckpoint string, messages []*schema.Message, referenceContext string) int {
	total := countRunes(existingCheckpoint)
	total += countRunes(referenceContext)
	for i, msg := range messages {
		if msg == nil {
			continue
		}
		total += countRunes(formatCompactionMessage(i+1, msg))
	}
	return total
}

func countRunes(value string) int {
	return utf8.RuneCountInString(strings.TrimSpace(value))
}

func formatCompactionMessage(index int, msg *schema.Message) string {
	role := string(msg.Role)
	content := strings.TrimSpace(msg.Content)
	if len(msg.ToolCalls) > 0 {
		data, _ := json.Marshal(msg.ToolCalls)
		content = strings.TrimSpace(content + "\nTool calls: " + string(data))
	}
	if msg.ToolName != "" {
		content = strings.TrimSpace(fmt.Sprintf("tool=%s call_id=%s\n%s", msg.ToolName, msg.ToolCallID, content))
	}
	return fmt.Sprintf("\n--- message %d role=%s ---\n%s\n", index, role, content)
}

func emitContextCompactionEvent(emit func(Event), phase, status string, result ContextCompactionResult) {
	if emit == nil {
		return
	}
	emit(Event{Type: "context_compaction", Data: map[string]any{
		"phase":                       phase,
		"status":                      status,
		"tokens_before":               result.TokensBefore,
		"projected_tokens_before":     result.ProjectedTokensBefore,
		"reserved_completion_tokens":  result.ReservedCompletionTokens,
		"reserved_tool_result_tokens": result.ReservedToolResultTokens,
		"tokens_after":                result.TokensAfter,
		"context_window_tokens":       result.ContextWindowTokens,
		"strategy":                    result.Strategy,
		"threshold":                   result.Threshold,
		"target_ratio":                result.TargetRatio,
		"epoch":                       result.Epoch,
		"source_message_count":        result.SourceMessageCount,
		"message_count_before":        result.MessageCountBefore,
		"message_count_after":         result.MessageCountAfter,
		"skipped_reason":              result.SkippedReason,
		"summary":                     result.Summary,
	}})
}

func emitContextCompactionDeltaEvent(emit func(Event), phase string, result ContextCompactionResult, attempt int, delta string) {
	if emit == nil || delta == "" {
		return
	}
	emit(Event{Type: "context_compaction", Data: map[string]any{
		"phase":                       phase,
		"status":                      "delta",
		"attempt":                     attempt,
		"delta":                       delta,
		"tokens_before":               result.TokensBefore,
		"projected_tokens_before":     result.ProjectedTokensBefore,
		"reserved_completion_tokens":  result.ReservedCompletionTokens,
		"reserved_tool_result_tokens": result.ReservedToolResultTokens,
		"context_window_tokens":       result.ContextWindowTokens,
		"strategy":                    result.Strategy,
		"threshold":                   result.Threshold,
		"message_count_before":        result.MessageCountBefore,
	}})
}

type contextCompactionMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	agentKind string
}

func (m *contextCompactionMiddleware) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, _ *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	if state == nil {
		return ctx, state, nil
	}
	controller := compactionControllerFromContext(ctx)
	if controller == nil || controller.conversation == nil {
		return ctx, state, nil
	}
	messages := append([]*schema.Message(nil), state.Messages...)
	newMessages, result, err := controller.conversation.CompactContextIfNeeded(ctx, ContextCompactionInput{
		Messages: messages,
		Tools:    state.ToolInfos,
		Phase:    contextCompactionPhaseMidRun,
	})
	if err != nil {
		observability.Logger("agent-run").Warn("mid_run_context_compaction_failed", slog.String("agent_kind", m.agentKind), slog.Any("error", err))
		return ctx, state, nil
	}
	if !result.Triggered {
		return ctx, state, nil
	}
	next := *state
	next.Messages = newMessages
	return ctx, &next, nil
}

type contextCompactionUsage struct {
	PromptTokens           int `json:"prompt_tokens,omitempty"`
	CachedPromptTokens     int `json:"cached_prompt_tokens,omitempty"`
	CompletionTokens       int `json:"completion_tokens,omitempty"`
	ReasoningTokens        int `json:"reasoning_tokens,omitempty"`
	TotalTokens            int `json:"total_tokens,omitempty"`
	ContextWindowTokens    int `json:"context_window_tokens,omitempty"`
	EstimatedContextTokens int `json:"estimated_context_tokens,omitempty"`
}

func contextCompactionRecordFromResult(result ContextCompactionResult, agentKind string, sourceStart, sourceEnd, retainedTurns int, summary string) session.ContextCompaction {
	return session.ContextCompaction{
		Type:                "context_compaction",
		AgentKind:           agentKind,
		Epoch:               result.Epoch,
		Summary:             summary,
		SourceStartIndex:    sourceStart,
		SourceEndIndex:      sourceEnd,
		SourceMessageCount:  sourceEnd - sourceStart,
		RetainedTurns:       retainedTurns,
		TokensBefore:        result.TokensBefore,
		TokensAfter:         result.TokensAfter,
		TargetRatio:         result.TargetRatio,
		ContextWindowTokens: result.ContextWindowTokens,
		Strategy:            result.Strategy,
		Threshold:           result.Threshold,
		Reason:              contextCompactionReasonLimit,
		Phase:               result.Phase,
		CreatedAt:           time.Now().UTC(),
	}
}
