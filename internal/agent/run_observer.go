package agent

import (
	"context"
	"strings"
	"sync"
	"time"
)

type runObserverKey struct{}

// LLMOutcome captures bounded metadata from the latest model response in one run.
type LLMOutcome struct {
	FinishReason      string
	RequestedTools    []string
	ProviderRequestID string
}

// BudgetSnapshot 记录一次模型请求前评估的上下文预算，供工具准入控制（middleware）
// 在执行工具前判断"本次工具注入会不会把上下文推出工具安全窗口"。ProjectedTokens
// 是当前历史 + 预留的预计占用；ToolSafeWindow 是工具模式物理安全窗口（含双重上限）。
type BudgetSnapshot struct {
	ProjectedTokens int
	ToolSafeWindow  int
}

// RunObserver records durable state for one Agent run without changing model-visible behavior.
type RunObserver struct {
	ledger         *RunLedger
	runID          string
	sessionID      string
	reviewThreadID string
	rootSpanID     string
	llmSpanID      string
	lastLLMOutcome LLMOutcome
	budget         BudgetSnapshot
	// injectedSinceSnapshot 自最近一次 RecordBudgetSnapshot 以来，工具结果实际回填
	// 进上下文的累计 token。同一模型请求发出的一批工具调用中，前一个的结果还没进入
	// 下一轮 projected，后一个的注入准入必须计入这个累计，避免整批合计超出窗口。
	injectedSinceSnapshot int
	// injectedBytesSinceSnapshot 自最近一次 RecordBudgetSnapshot 以来，工具结果实际回填
	// 进上下文的累计字节。同一模型请求发出的一批工具调用里，前一个的结果还没进入下一轮
	// projected，后一个的注入必须计入这个累计，避免整批合计超出"单消息工具结果聚合预算"
	// （对应 Claude Code MAX_TOOL_RESULTS_PER_MESSAGE_CHARS）。
	injectedBytesSinceSnapshot int
	pendingTools               map[string]*traceSpanHandle
	// readDedup 记录本次 run 内 read_file 已返回给模型的窗口（key 为规范化绝对路径）。
	// 条目带文件 size+modTime，重读时文件未变且窗口相同则判定为重复注入。
	readDedup map[string]readDedupEntry
	mu        sync.Mutex
}

// readDedupEntry 记录一次 read_file 窗口返回时的文件状态，用于同 run 去重判定。
type readDedupEntry struct {
	offset  int
	limit   int
	size    int64
	modTime time.Time
}

func newRunObserver(ledger *RunLedger, rootSpanID string) *RunObserver {
	runID := ""
	if ledger != nil {
		runID = strings.TrimSpace(ledger.ID())
	}
	return newRunObserverWithIdentity(ledger, rootSpanID, runID, "", "")

}

func newRunObserverWithIdentity(ledger *RunLedger, rootSpanID, runID, sessionID, reviewThreadID string) *RunObserver {
	return &RunObserver{
		ledger:         ledger,
		runID:          strings.TrimSpace(runID),
		sessionID:      strings.TrimSpace(sessionID),
		reviewThreadID: strings.TrimSpace(reviewThreadID),
		rootSpanID:     rootSpanID,
		pendingTools:   map[string]*traceSpanHandle{},
	}
}

func ContextWithRunObserver(ctx context.Context, observer *RunObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, runObserverKey{}, observer)
}

func RunObserverFromContext(ctx context.Context) *RunObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(runObserverKey{}).(*RunObserver)
	return observer
}

func (o *RunObserver) RecordLLMSpan(spanID string) {
	if o == nil || spanID == "" {
		return
	}
	o.mu.Lock()
	o.llmSpanID = spanID
	o.mu.Unlock()
}

func (o *RunObserver) RecordLLMOutcome(outcome LLMOutcome) {
	if o == nil {
		return
	}
	outcome.FinishReason = strings.TrimSpace(outcome.FinishReason)
	outcome.ProviderRequestID = strings.TrimSpace(outcome.ProviderRequestID)
	outcome.RequestedTools = append([]string(nil), outcome.RequestedTools...)
	o.mu.Lock()
	o.lastLLMOutcome = outcome
	o.mu.Unlock()
}

func (o *RunObserver) LastLLMOutcome() LLMOutcome {
	if o == nil {
		return LLMOutcome{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	outcome := o.lastLLMOutcome
	outcome.RequestedTools = append([]string(nil), outcome.RequestedTools...)
	return outcome
}

// RecordBudgetSnapshot 记录一次模型请求前的预算快照。run 流程在每次组装请求后、
// 调用模型前更新，工具执行时（同一 run 的 ctx）由准入控制读取。每次记录会重置
// injectedSinceSnapshot：新一轮模型请求意味着上一批工具结果已进入下一轮 projected。
func (o *RunObserver) RecordBudgetSnapshot(projectedTokens, toolSafeWindow int) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.budget = BudgetSnapshot{ProjectedTokens: projectedTokens, ToolSafeWindow: toolSafeWindow}
	o.injectedSinceSnapshot = 0
	o.injectedBytesSinceSnapshot = 0
	o.mu.Unlock()
}

// BudgetSnapshot 返回最近一次记录的预算快照；未记录时返回零值。
func (o *RunObserver) BudgetSnapshot() BudgetSnapshot {
	if o == nil {
		return BudgetSnapshot{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.budget
}

// AddInjectedTokens 累加一批工具调用中实际回填进上下文的 token 数（按返回字节估算），
// 供同一批内后续工具的注入准入判断。工具执行成功后由 middleware 调用。
func (o *RunObserver) AddInjectedTokens(tokens int) {
	if o == nil || tokens <= 0 {
		return
	}
	o.mu.Lock()
	o.injectedSinceSnapshot += tokens
	o.mu.Unlock()
}

// InjectedTokensSinceSnapshot 返回自最近一次 RecordBudgetSnapshot 以来已注入的累计 token。
func (o *RunObserver) InjectedTokensSinceSnapshot() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.injectedSinceSnapshot
}

// AddInjectedBytes 累加一批工具调用中实际回填进上下文的字节数，供"单消息工具结果聚合预算"
// 判断。工具执行成功后由 middleware 调用。
func (o *RunObserver) AddInjectedBytes(bytes int) {
	if o == nil || bytes <= 0 {
		return
	}
	o.mu.Lock()
	o.injectedBytesSinceSnapshot += bytes
	o.mu.Unlock()
}

// InjectedBytesSinceSnapshot 返回自最近一次 RecordBudgetSnapshot 以来已注入的累计字节。
func (o *RunObserver) InjectedBytesSinceSnapshot() int {
	if o == nil {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.injectedBytesSinceSnapshot
}

// RecordReadWindow 记录 read_file 在本次 run 内已把文件 [offset, offset+limit) 窗口
// 返回给模型。size/modTime 是返回时的文件状态，供重读时判定"内容未变化"。
// 仅 run 作用域（RunObserver 每次 run 新建），不跨 run 持久化。
func (o *RunObserver) RecordReadWindow(path string, offset, limit int, size int64, modTime time.Time) {
	if o == nil || path == "" {
		return
	}
	o.mu.Lock()
	if o.readDedup == nil {
		o.readDedup = map[string]readDedupEntry{}
	}
	o.readDedup[path] = readDedupEntry{offset: offset, limit: limit, size: size, modTime: modTime}
	o.mu.Unlock()
}

// HasReadWindow 报告同一文件的同一窗口是否已在本 run 内返回过且文件未变化
// （size+modTime 均一致）。read_file 据此返回"已在上下文中"占位，避免重复注入全文。
func (o *RunObserver) HasReadWindow(path string, offset, limit int, size int64, modTime time.Time) bool {
	if o == nil || path == "" {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	entry, ok := o.readDedup[path]
	return ok && entry.offset == offset && entry.limit == limit && entry.size == size && entry.modTime.Equal(modTime)
}

// RunID returns the durable run identity available to tools in this context.
// It is intentionally metadata-only; tools must not depend on the run ledger
// contents when applying workspace changes.
func (o *RunObserver) RunID() string {
	if o == nil {
		return ""
	}
	return o.runID
}

// SessionID identifies the user-visible conversation that owns this run.
func (o *RunObserver) SessionID() string {
	if o == nil {
		return ""
	}
	return o.sessionID
}

// ReviewThreadID links this run to a multi-run review without changing the
// run-scoped ChangeGroup/Undo boundary.
func (o *RunObserver) ReviewThreadID() string {
	if o == nil {
		return ""
	}
	return o.reviewThreadID
}

func (o *RunObserver) RecordToolDecision(decision ToolDecision) {
	if o == nil || o.ledger == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.ledger.RecordToolDecision(decision)
	attrs := map[string]any{
		"tool_name":           decision.ToolName,
		"tool_call_id":        decision.ToolCallID,
		"source":              decision.Source,
		"capability":          decision.Capability,
		"action":              decision.Action,
		"reason":              decision.Reason,
		"mutates_workspace":   decision.MutatesWorkspace,
		"requires_post_check": decision.RequiresPostCheck,
		"target":              decision.Target,
	}
	if decision.ArgsBytes > 0 {
		attrs["args_bytes"] = decision.ArgsBytes
	}
	if decision.ArgsComplete != nil {
		attrs["args_complete"] = *decision.ArgsComplete
	}
	if decision.ModelFinishReason != "" {
		attrs["model_finish_reason"] = decision.ModelFinishReason
	}
	o.pendingTools[o.toolKey(decision.ToolCallID, decision.ToolName)] = newTraceSpanHandle(o.ledger.ID(), o.ledger, o.parentSpanID(), "tool_call", attrs)
}

func (o *RunObserver) RecordToolExecution(result ToolExecutionRecord) {
	if o == nil || o.ledger == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.ledger.RecordToolExecution(result)
	key := o.toolKey(result.ToolCallID, result.ToolName)
	span := o.pendingTools[key]
	delete(o.pendingTools, key)
	if span == nil {
		span = newTraceSpanHandle(o.ledger.ID(), o.ledger, o.parentSpanID(), "tool_call", map[string]any{
			"tool_name":    result.ToolName,
			"tool_call_id": result.ToolCallID,
		})
	}
	status := result.Status
	if status == "" {
		status = "success"
	}
	attrs := map[string]any{
		"tool_name":       result.ToolName,
		"tool_call_id":    result.ToolCallID,
		"capability":      result.Capability,
		"original_bytes":  result.OriginalBytes,
		"returned_bytes":  result.ReturnedBytes,
		"truncated":       result.Truncated,
		"target":          result.Target,
		"idempotency_key": result.IdempotencyKey,
		"error":           result.Error,
		"recorded_at":     time.Now().UTC().Format(time.RFC3339Nano),
	}
	if result.DomainStatus != "" {
		attrs["domain_status"] = result.DomainStatus
		attrs["domain_diagnostic_count"] = result.DomainDiagnosticCount
		attrs["retry_modules"] = append([]string(nil), result.RetryModules...)
	}
	if result.Workspace != "" {
		attrs["workspace"] = result.Workspace
	}
	if result.ChangeGroupID != "" {
		attrs["change_group_id"] = result.ChangeGroupID
	}
	if result.ReviewThreadID != "" {
		attrs["review_thread_id"] = result.ReviewThreadID
	}
	if result.ChangeSetID != "" {
		attrs["change_set_id"] = result.ChangeSetID
	}
	if result.BaseRevision != "" {
		attrs["base_revision"] = result.BaseRevision
	}
	if result.Revision != "" {
		attrs["revision"] = result.Revision
	}
	if result.ArgsBytes > 0 {
		attrs["args_bytes"] = result.ArgsBytes
	}
	if result.ArgsComplete != nil {
		attrs["args_complete"] = *result.ArgsComplete
	}
	if result.ModelFinishReason != "" {
		attrs["model_finish_reason"] = result.ModelFinishReason
	}
	span.Finish(status, attrs)
}

func (o *RunObserver) RecordMutations(mutations []ToolMutation) {
	if o == nil || o.ledger == nil || len(mutations) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.ledger.RecordMutations(mutations)
}

func (o *RunObserver) RecordVerification(verification PostRunVerification) {
	if o == nil || o.ledger == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	_ = o.ledger.RecordVerification(verification)
}

func (o *RunObserver) toolKey(callID, name string) string {
	if callID != "" {
		return callID
	}
	return name
}

func (o *RunObserver) parentSpanID() string {
	if o.llmSpanID != "" {
		return o.llmSpanID
	}
	return o.rootSpanID
}
