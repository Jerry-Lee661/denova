package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	"denova/config"
)

// toolOrchestratorMiddleware centralizes Nova's internal tool execution policy.
type toolOrchestratorMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
	agentKind           string
	policyKind          string
	workspace           string
	toolSettings        config.ResolvedAgentToolSettings
	enforceToolSettings bool
	toolResultMaxBytes  int
	// toolResultBatchLimitBytes 限制一条 assistant 消息内所有并行工具结果总和上限（字节）。
	// 对应 Claude Code MAX_TOOL_RESULTS_PER_MESSAGE_CHARS。0 表示不设置聚合上限。
	toolResultBatchLimitBytes int
	executionGate             *toolExecutionGate
	resultStore               *ResultStore
}

type interactiveStoryToolMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

type interactiveDirectorPlanFileMiddleware struct {
	*adk.BaseChatModelAgentMiddleware
}

func newInteractiveStoryToolMiddleware() *interactiveStoryToolMiddleware {
	return &interactiveStoryToolMiddleware{}
}

func newInteractiveDirectorPlanFileMiddleware() *interactiveDirectorPlanFileMiddleware {
	return &interactiveDirectorPlanFileMiddleware{}
}

func (m *interactiveDirectorPlanFileMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		if msg := m.blockedDirectorToolMessage(toolName(toolCtx), args); msg != "" {
			return msg, nil
		}
		return endpoint(ctx, args, opts...)
	}, nil
}

func (m *interactiveDirectorPlanFileMiddleware) WrapStreamableToolCall(
	_ context.Context,
	endpoint adk.StreamableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		if msg := m.blockedDirectorToolMessage(toolName(toolCtx), args); msg != "" {
			return singleChunkReader(msg), nil
		}
		return endpoint(ctx, args, opts...)
	}, nil
}

func (m *interactiveDirectorPlanFileMiddleware) blockedDirectorToolMessage(name, _ string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "read_event_cards", "list_lore_items", "read_lore_items", "search_story_history", submitDirectorPlanUpdateToolName:
		return ""
	case "read_file", "write_file", "edit_file":
		return fmt.Sprintf("[tool error] Director 规划文档已在上下文中完整提供；请用 %s 提交带 base_hash 的 Markdown Patch，拒绝工具: %s", submitDirectorPlanUpdateToolName, name)
	case "apply_actor_state_patch":
		return fmt.Sprintf("[tool error] Director 只维护 ArcPlan，不能写 Actor State，拒绝工具: %s", name)
	default:
		return fmt.Sprintf("[tool error] Director 只能使用 %s、历史检索、资料库只读和事件卡工具，拒绝工具: %s", submitDirectorPlanUpdateToolName, name)
	}
}

func (m *interactiveStoryToolMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		if isInteractiveStoryWriteTool(toolName(toolCtx)) {
			return interactiveStoryWriteToolBlockedMessage(toolName(toolCtx)), nil
		}
		return endpoint(ctx, args, opts...)
	}, nil
}

func (m *interactiveStoryToolMiddleware) WrapStreamableToolCall(
	_ context.Context,
	endpoint adk.StreamableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		if isInteractiveStoryWriteTool(toolName(toolCtx)) {
			return singleChunkReader(interactiveStoryWriteToolBlockedMessage(toolName(toolCtx))), nil
		}
		return endpoint(ctx, args, opts...)
	}, nil
}

func toolName(toolCtx *adk.ToolContext) string {
	if toolCtx == nil {
		return ""
	}
	return toolCtx.Name
}

func isInteractiveStoryWriteTool(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "write_file", "edit_file", "delete_file", "create_file", "move_file", "copy_file", "rename_file", "mkdir", "remove_file":
		return true
	}
	return strings.HasPrefix(name, "write_") ||
		strings.HasPrefix(name, "edit_") ||
		strings.HasPrefix(name, "delete_") ||
		strings.HasPrefix(name, "create_") ||
		strings.HasPrefix(name, "move_") ||
		strings.HasPrefix(name, "copy_") ||
		strings.HasPrefix(name, "rename_")
}

func interactiveStoryWriteToolBlockedMessage(name string) string {
	return fmt.Sprintf("[tool error] 游戏模式禁止使用写文件工具 %q。请不要修改 workspace 文件；先直接输出完整故事正文，再用 submit_interactive_turn 提交一致的隐藏回合结果。", name)
}

type ToolDecision struct {
	ToolName          string     `json:"tool_name"`
	ToolCallID        string     `json:"tool_call_id,omitempty"`
	Source            ToolSource `json:"source"`
	Capability        string     `json:"capability,omitempty"`
	Action            string     `json:"action"`
	Reason            string     `json:"reason,omitempty"`
	MutatesWorkspace  bool       `json:"mutates_workspace"`
	RequiresPostCheck bool       `json:"requires_post_check"`
	Target            string     `json:"target,omitempty"`
	ArgsBytes         int        `json:"args_bytes,omitempty"`
	ArgsComplete      *bool      `json:"args_complete,omitempty"`
	ModelFinishReason string     `json:"model_finish_reason,omitempty"`
}

type ToolExecutionRecord struct {
	ToolName              string   `json:"tool_name"`
	ToolCallID            string   `json:"tool_call_id,omitempty"`
	Workspace             string   `json:"workspace,omitempty"`
	Status                string   `json:"status"`
	DomainStatus          string   `json:"domain_status,omitempty"`
	DomainDiagnosticCount int      `json:"domain_diagnostic_count,omitempty"`
	RetryModules          []string `json:"retry_modules,omitempty"`
	Capability            string   `json:"capability,omitempty"`
	OriginalBytes         int      `json:"original_bytes,omitempty"`
	ReturnedBytes         int      `json:"returned_bytes,omitempty"`
	Truncated             bool     `json:"truncated,omitempty"`
	Target                string   `json:"target,omitempty"`
	IdempotencyKey        string   `json:"idempotency_key,omitempty"`
	Externalized          bool     `json:"externalized,omitempty"`
	Error                 string   `json:"error,omitempty"`
	ArgsBytes             int      `json:"args_bytes,omitempty"`
	ArgsComplete          *bool    `json:"args_complete,omitempty"`
	ModelFinishReason     string   `json:"model_finish_reason,omitempty"`
	ChangeGroupID         string   `json:"change_group_id,omitempty"`
	ReviewThreadID        string   `json:"review_thread_id,omitempty"`
	ChangeSetID           string   `json:"change_set_id,omitempty"`
	BaseRevision          string   `json:"base_revision,omitempty"`
	Revision              string   `json:"revision,omitempty"`
}

func (m *toolOrchestratorMiddleware) WrapInvokableToolCall(
	_ context.Context,
	endpoint adk.InvokableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.InvokableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (string, error) {
		decision := m.buildToolDecision(toolCtx, args)
		observer := RunObserverFromContext(ctx)
		outcome := LLMOutcome{}
		if observer != nil {
			outcome = observer.LastLLMOutcome()
		}
		decision = applyToolArgumentValidation(decision, args, outcome)
		observer.RecordToolDecision(decision)
		// 工具注入准入：read_file 会向上下文注入大量内容，当预算快照接近工具安全
		// 窗口时先拒绝并引导分块，避免文档整章灌入上下文（本地模型会因 KV + 长
		// prefill 缓冲内存不足而崩溃）。这是"ctx 用量统计反馈到工具调用"的闭环。
		if msg, ok := m.toolInjectionAdmission(ctx, toolName(toolCtx), args); ok {
			decision.Action = "blocked"
			decision.Reason = msg
			observer.RecordToolDecision(decision)
			observer.RecordToolExecution(blockedToolExecutionRecord(decision, msg))
			return msg, nil
		}
		if decision.Action == "blocked" {
			msg := decision.Reason
			if msg == "" {
				msg = fmt.Sprintf("[tool error] 工具 %q 被当前 Agent 策略阻止。", decision.ToolName)
			}
			observer.RecordToolExecution(blockedToolExecutionRecord(decision, msg))
			return msg, nil
		}
		release := m.acquireToolExecution(decision)
		defer release()
		result, err := endpoint(ctx, args, opts...)
		if err != nil {
			if _, ok := compose.IsInterruptRerunError(err); ok {
				return "", err
			}
			msg := m.toolEndpointErrorMessage(decision.ToolName, err)
			observer.RecordToolExecution(ToolExecutionRecord{
				ToolName:   decision.ToolName,
				ToolCallID: decision.ToolCallID,
				Status:     "error",
				Capability: decision.Capability,
				Target:     decision.Target,
				Error:      err.Error(),
			})
			return msg, nil
		}
		filtered := m.applyToolResultPolicy(ctx, toolName(toolCtx), args, result)
		// 同一模型请求发出的一批工具调用里，前一个的结果还没进入下一轮 projected，
		// 按实际回填字节累加注入量，供本批后续工具的准入判断。聚合预算由 applyToolResultPolicy
		// 在同一 ctx 内累计，避免 N 个并行工具结果总和冲爆上下文。
		if obs := RunObserverFromContext(ctx); obs != nil {
			obs.AddInjectedTokens(filtered.ReturnedBytes / bytesPerTokenEstimate)
		}
		record := ToolExecutionRecord{
			ToolName:       filtered.Manifest.Name,
			ToolCallID:     decision.ToolCallID,
			Status:         "success",
			Capability:     filtered.Manifest.Capability,
			OriginalBytes:  filtered.OriginalBytes,
			ReturnedBytes:  filtered.ReturnedBytes,
			Truncated:      filtered.Truncated,
			Target:         filtered.Target,
			IdempotencyKey: filtered.IdempotencyKey,
			Externalized:   filtered.Externalized,
		}
		applyWorkspaceChangeReceiptToExecutionRecord(&record, result)
		applyInteractiveTurnReceiptToExecutionRecord(&record, result)
		observer.RecordToolExecution(record)
		return filtered.Content, nil
	}, nil
}

// filterToolResult 对工具结果做字节上限裁剪；超过外置阈值时整体写盘，
// 接口视图只放逐字节稳定的预览 + 位置引用。未配置存储目录时退化为纯裁剪。
func (m *toolOrchestratorMiddleware) filterToolResult(toolName, args, result string) FilteredToolResult {
	if m.resultStore != nil && strings.TrimSpace(m.resultStore.sessionID) != "" {
		filtered, externalized := m.resultStore.ExternalizeIfNeeded(toolName, args, result, m.toolResultLimitBytes())
		if externalized {
			return filtered
		}
		return FilterToolResultForModelWithLimit(toolName, args, result, m.toolResultLimitBytes())
	}
	return FilterToolResultForModelWithLimit(toolName, args, result, m.toolResultLimitBytes())
}

// applyToolResultPolicy 先按单结果上限过滤，再应用"一条消息内工具结果聚合预算"：
// 把该结果回填字节累加进累计值，累计超限时对后续结果做外置降级（整体写盘、
// 只放预览+位置引用），避免 N 个并行 read_file/glob 各自不超限但总和冲爆上下文。
func (m *toolOrchestratorMiddleware) applyToolResultPolicy(ctx context.Context, toolName, args, result string) FilteredToolResult {
	filtered := m.filterToolResult(toolName, args, result)
	return m.applyToolResultBatchBudget(ctx, toolName, args, result, filtered)
}

// applyToolResultBatchBudget 在单结果过滤后应用聚合预算。cumulative 超过
// toolResultBatchLimitBytes 时对该结果做外置降级。已外置或无存储目录时按单结果上限返回。
func (m *toolOrchestratorMiddleware) applyToolResultBatchBudget(ctx context.Context, toolName, args string, result string, filtered FilteredToolResult) FilteredToolResult {
	if m == nil || m.toolResultBatchLimitBytes <= 0 {
		return filtered
	}
	obs := RunObserverFromContext(ctx)
	if obs == nil {
		return filtered
	}
	cumulative := obs.InjectedBytesSinceSnapshot() + filtered.ReturnedBytes
	if cumulative <= m.toolResultBatchLimitBytes {
		obs.AddInjectedBytes(filtered.ReturnedBytes)
		return filtered
	}
	// 累计超限：对该结果做外置降级（整体写盘、只放预览+位置引用），
	// 避免一条消息内 N 个并行工具结果总和冲爆上下文。
	// 用无条件 Externalize（不受单工具阈值约束）：小结果也可能因累计超预算而外置。
	if !filtered.Externalized && m.resultStore != nil && strings.TrimSpace(m.resultStore.sessionID) != "" {
		ext := m.resultStore.Externalize(toolName, args, result, m.toolResultLimitBytes())
		obs.AddInjectedBytes(ext.ReturnedBytes)
		return ext
	}
	obs.AddInjectedBytes(filtered.ReturnedBytes)
	return filtered
}

func applyInteractiveTurnReceiptToExecutionRecord(record *ToolExecutionRecord, result string) {
	if record == nil || !IsInteractiveTurnSubmissionTool(record.ToolName) {
		return
	}
	var receipt struct {
		Ready        bool              `json:"ready"`
		ModuleStatus map[string]string `json:"module_status"`
		Diagnostics  []json.RawMessage `json:"diagnostics"`
		RetryModules []string          `json:"retry_modules"`
	}
	if err := json.Unmarshal([]byte(result), &receipt); err != nil || receipt.ModuleStatus == nil {
		return
	}
	record.DomainDiagnosticCount = len(receipt.Diagnostics)
	record.RetryModules = append([]string(nil), receipt.RetryModules...)
	switch {
	case receipt.Ready:
		record.DomainStatus = "accepted"
	case turnSubmissionReceiptHasStatus(receipt.ModuleStatus, "rejected"):
		record.DomainStatus = "rejected"
	default:
		record.DomainStatus = "pending"
	}
}

func turnSubmissionReceiptHasStatus(statuses map[string]string, target string) bool {
	for _, status := range statuses {
		if status == target {
			return true
		}
	}
	return false
}

func (m *toolOrchestratorMiddleware) WrapStreamableToolCall(
	_ context.Context,
	endpoint adk.StreamableToolCallEndpoint,
	toolCtx *adk.ToolContext,
) (adk.StreamableToolCallEndpoint, error) {
	return func(ctx context.Context, args string, opts ...tool.Option) (*schema.StreamReader[string], error) {
		decision := m.buildToolDecision(toolCtx, args)
		observer := RunObserverFromContext(ctx)
		outcome := LLMOutcome{}
		if observer != nil {
			outcome = observer.LastLLMOutcome()
		}
		decision = applyToolArgumentValidation(decision, args, outcome)
		observer.RecordToolDecision(decision)
		if decision.Action == "blocked" {
			msg := decision.Reason
			if msg == "" {
				msg = fmt.Sprintf("[tool error] 工具 %q 被当前 Agent 策略阻止。", decision.ToolName)
			}
			observer.RecordToolExecution(blockedToolExecutionRecord(decision, msg))
			return singleChunkReader(msg), nil
		}
		release := m.acquireToolExecution(decision)
		sr, err := endpoint(ctx, args, opts...)
		if err != nil {
			release()
			if _, ok := compose.IsInterruptRerunError(err); ok {
				return nil, err
			}
			observer.RecordToolExecution(ToolExecutionRecord{
				ToolName:   decision.ToolName,
				ToolCallID: decision.ToolCallID,
				Status:     "error",
				Capability: decision.Capability,
				Target:     decision.Target,
				Error:      err.Error(),
			})
			return singleChunkReader(m.toolEndpointErrorMessage(decision.ToolName, err)), nil
		}
		return filterToolResultReader(ctx, sr, toolCtx, args, m.toolResultLimitBytes(), release), nil
	}, nil
}

func (m *toolOrchestratorMiddleware) toolEndpointErrorMessage(toolName string, err error) string {
	if msg, ok := formatWorkspaceChangeToolError(toolName, err); ok {
		return msg
	}
	workspace := ""
	if m != nil {
		workspace = m.workspace
	}
	if hint, ok := filePathRecoveryHint(workspace, toolName, err); ok {
		return hint
	}
	return fmt.Sprintf("[tool error] %v", err)
}

func filePathRecoveryHint(workspace, toolName string, err error) (string, bool) {
	if err == nil {
		return "", false
	}
	name := strings.TrimSpace(toolName)
	if name != "read_file" && name != "ls" && name != "glob" {
		return "", false
	}
	var aliasErr *agentFileAliasError
	if errors.As(err, &aliasErr) {
		return aliasRecoveryHint(workspace, toolName, aliasErr), true
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	if msg == "" {
		return "", false
	}
	if !strings.Contains(msg, "file not found") &&
		!strings.Contains(msg, "outside the active workspace") &&
		!strings.Contains(msg, "must be absolute") &&
		!strings.Contains(msg, "failed to walk directory") {
		return "", false
	}
	ws := strings.TrimSpace(workspace)
	wsField := ""
	wsHint := ""
	if ws != "" {
		wsField = "workspace: " + ws + "\n"
		wsHint = fmt.Sprintf("当前作品根目录：%s。\n", ws)
	}
	return fmt.Sprintf(`[tool error]
type: path_resolution_failed
tool: %s
retryable: true
workspace_mutated: false
%s
中文：文件路径解析失败：%v。
%s请严格按以下顺序恢复，不要猜测、手写新绝对路径或改写文件名：
0) 需要定位时，先对作品根目录直接调用 ls（省略 path 参数即可列出作品根；ls/glob/read_file 都支持相对作品根目录的相对路径，如 ls chapters、read_file chapters/ch00001.md），再逐层 ls/glob 子目录；
1) 回到最近一次成功工具结果中的 target 路径；
2) 先对目标目录执行 ls，确认真实子目录名；
3) 再对目标子目录执行 ls 或 glob，复制真实文件名（不要改写文件名或做编码转换）；
4) 最后调用 read_file。
5) 若 ls/glob 已确认该目录下确实没有此文件（例如进度文件尚未创建），说明是文件缺失而非路径错误：停止重试，直接告知用户缺少哪些文件。
若连续两次仍失败，停止重复猜测绝对路径：一律改用相对作品根目录的相对路径（如 read_file setting/progress.md、ls chapters）；仍失败再请求用户确认路径。

English: Path resolution failed: %v.
%sRecovery order only: call ls on the workspace root first (omit the path parameter to list it; ls/glob/read_file all accept paths relative to the workspace root, e.g. ls chapters, read_file chapters/ch00001.md), then use the last successful tool target, then ls parent -> ls/glob child -> read_file. Do not guess absolute paths or re-encode filenames. If ls/glob confirm the file genuinely does not exist (e.g. progress files not created yet), stop retrying — it is a missing file, not a path issue — and tell the user which files are missing. If two consecutive attempts fail, stop guessing absolute paths and switch to paths relative to the workspace root (e.g. read_file setting/progress.md, ls chapters); only then ask the user to confirm the path.`,
		name, wsField, err, wsHint, err, wsHint), true
}

func (m *toolOrchestratorMiddleware) acquireToolExecution(decision ToolDecision) func() {
	if m == nil || m.executionGate == nil {
		return func() {}
	}
	manifest := ManifestForTool(decision.ToolName)
	return m.executionGate.acquire(executionModeForTool(manifest))
}

func singleChunkReader(msg string) *schema.StreamReader[string] {
	r, w := schema.Pipe[string](1)
	_ = w.Send(msg, nil)
	w.Close()
	return r
}

func filterToolResultReader(ctx context.Context, sr *schema.StreamReader[string], toolCtx *adk.ToolContext, args string, maxBytes int, releases ...func()) *schema.StreamReader[string] {
	r, w := schema.Pipe[string](1)
	go func() {
		defer w.Close()
		defer func() {
			for _, release := range releases {
				if release != nil {
					release()
				}
			}
		}()
		defer func() {
			if recovered := recover(); recovered != nil {
				_ = w.Send(fmt.Sprintf("\n[tool error] panic while reading tool result: %v", recovered), nil)
			}
		}()
		if sr == nil {
			_ = w.Send("\n[tool error] streamable tool returned a nil result stream", nil)
			return
		}
		defer sr.Close()
		name := toolName(toolCtx)
		manifest := ManifestForTool(name)
		manifest.MaxResultBytes = normalizeToolResultLimitBytes(maxBytes)
		limit := normalizedToolResultLimit(manifest)
		var content strings.Builder
		originalBytes := 0
		for {
			chunk, err := sr.Recv()
			if errors.Is(err, io.EOF) {
				filtered := filteredToolResultFromBody(manifest, args, content.String(), originalBytes, originalBytes > content.Len())
				record := ToolExecutionRecord{
					ToolName:       filtered.Manifest.Name,
					ToolCallID:     toolCallID(toolCtx),
					Status:         "success",
					Capability:     filtered.Manifest.Capability,
					OriginalBytes:  filtered.OriginalBytes,
					ReturnedBytes:  filtered.ReturnedBytes,
					Truncated:      filtered.Truncated,
					Target:         filtered.Target,
					IdempotencyKey: filtered.IdempotencyKey,
				}
				applyWorkspaceChangeReceiptToExecutionRecord(&record, content.String())
				RunObserverFromContext(ctx).RecordToolExecution(record)
				_ = w.Send(filtered.Content, nil)
				return
			}
			if err != nil {
				RunObserverFromContext(ctx).RecordToolExecution(ToolExecutionRecord{
					ToolName:   manifest.Name,
					ToolCallID: toolCallID(toolCtx),
					Status:     "error",
					Capability: manifest.Capability,
					Target:     toolPathFromArgs(args),
					Error:      err.Error(),
				})
				_ = w.Send(fmt.Sprintf("\n[tool error] %v", err), nil)
				return
			}
			originalBytes += len(chunk)
			if limit <= 0 {
				content.WriteString(chunk)
				continue
			}
			if content.Len() >= limit {
				continue
			}
			remaining := limit - content.Len()
			if len(chunk) <= remaining {
				content.WriteString(chunk)
				continue
			}
			fragment, _ := truncateUTF8Bytes(chunk, remaining)
			content.WriteString(strings.TrimSuffix(fragment, "\n[tool result truncated]"))
		}
	}()
	return r
}

func applyWorkspaceChangeReceiptToExecutionRecord(record *ToolExecutionRecord, content string) {
	if record == nil {
		return
	}
	receipt, ok := parseWorkspaceChangeToolReceipt(record.ToolName, content)
	if !ok {
		return
	}
	record.Workspace = receipt.Workspace
	record.ChangeGroupID = receipt.ChangeGroupID
	record.ReviewThreadID = receipt.ReviewThreadID
	record.ChangeSetID = receipt.ChangeSetID
	record.BaseRevision = receipt.BaseRevision
	record.Revision = receipt.Revision
	if strings.TrimSpace(receipt.Path) != "" {
		record.Target = receipt.Path
	}
}

func blockedToolExecutionRecord(decision ToolDecision, msg string) ToolExecutionRecord {
	return ToolExecutionRecord{
		ToolName:          decision.ToolName,
		ToolCallID:        decision.ToolCallID,
		Status:            "blocked",
		Capability:        decision.Capability,
		Target:            decision.Target,
		Error:             msg,
		ArgsBytes:         decision.ArgsBytes,
		ArgsComplete:      decision.ArgsComplete,
		ModelFinishReason: decision.ModelFinishReason,
	}
}

func (m *toolOrchestratorMiddleware) toolResultLimitBytes() int {
	if m == nil {
		return 0
	}
	return normalizeToolResultLimitBytes(m.toolResultMaxBytes)
}

// bytesPerTokenEstimate 与 avgLineBytesEstimate 用于不读全文的轻量注入估算。
// 中文约 1 token/字 ≈ 3 bytes；小说行平均字节取 80（偏保守，宁高勿低）。
const (
	bytesPerTokenEstimate = 3
	avgLineBytesEstimate  = 80
)

// toolInjectionAdmission 是工具注入的准入控制：当一次 read_file 估算的注入量会把
// 上下文推出工具安全窗口（来自 pre-run 预算快照）时，拒绝执行并返回分块引导，
// 从源头堵住"文档整章灌入上下文"。无预算快照或无法估算时放行（保持现状）。
func (m *toolOrchestratorMiddleware) toolInjectionAdmission(ctx context.Context, name, args string) (string, bool) {
	if m == nil || name != "read_file" {
		return "", false
	}
	obs := RunObserverFromContext(ctx)
	if obs == nil {
		return "", false
	}
	snap := obs.BudgetSnapshot()
	if snap.ToolSafeWindow <= 0 {
		return "", false
	}
	injected, err := estimateReadFileInjectionTokens(m.workspace, args)
	if err != nil {
		// 无法估算（文件缺失/路径错误）交给正常错误路径处理。
		return "", false
	}
	// 计入同一批内此前已注入的量：前一个工具的结果还没回填进 projected，
	// 后续工具的准入必须把累计也加上，避免整批合计超出工具安全窗口。
	after := snap.ProjectedTokens + obs.InjectedTokensSinceSnapshot() + injected
	if after <= snap.ToolSafeWindow {
		return "", false
	}
	return fmt.Sprintf(`[tool error]
type: tool_injection_blocked
tool: read_file
retryable: false
workspace_mutated: false
中文：本次读取预计注入约 %d tokens，会把上下文推到 %d/%d tokens，超出工具安全窗口。请改用 offset+limit 只读需要的片段（例如 offset=N、limit=200），或先用 grep 定位行号再定向读取，不要整章全量读取。
English: this read is estimated to inject ~%d tokens, pushing context to %d/%d beyond the tool safe window. Read only the needed range via offset/limit, or grep for line numbers first.`,
		injected, after, snap.ToolSafeWindow,
		injected, after, snap.ToolSafeWindow), true
}

// estimateReadFileInjectionTokens 轻量估算一次 read_file 会向上下文注入的 token 数：
// 按请求的 limit 行 × 平均行字节估算（不超过文件实际大小），再按中文字节/token 折算。
// 只做安全方向偏高的近似，用于准入判断，不做精确统计。
func estimateReadFileInjectionTokens(workspace, args string) (int, error) {
	var input workspaceReadFileInput
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return 0, err
	}
	if strings.TrimSpace(input.FilePath) == "" {
		return 0, fmt.Errorf("read_file missing file_path")
	}
	absolute, _, err := resolveAgentToolPath(workspace, input.FilePath)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return 0, fmt.Errorf("cannot estimate read_file target")
	}
	limit := input.Limit
	if limit <= 0 {
		limit = agentFileReadDefaultLimitLines
	}
	bytesInjected := int64(limit) * avgLineBytesEstimate
	if bytesInjected > info.Size() {
		bytesInjected = info.Size()
	}
	tokens := int(bytesInjected / bytesPerTokenEstimate)
	if tokens < 1 {
		tokens = 1
	}
	return tokens, nil
}

func (m *toolOrchestratorMiddleware) buildToolDecision(toolCtx *adk.ToolContext, args string) ToolDecision {
	name := toolName(toolCtx)
	manifest := ManifestForTool(name)
	decision := ToolDecision{
		ToolName:          manifest.Name,
		ToolCallID:        toolCallID(toolCtx),
		Source:            manifest.Source,
		Capability:        manifest.Capability,
		Action:            "allowed",
		MutatesWorkspace:  manifest.MutatesWorkspace,
		RequiresPostCheck: manifest.RequiresPostCheck,
		Target:            toolPathFromArgs(args),
		ArgsBytes:         len(args),
	}
	if m != nil && m.effectivePolicyKind() == AgentKindInteractiveStory && isInteractiveStoryWriteTool(name) {
		decision.Action = "blocked"
		decision.Reason = interactiveStoryWriteToolBlockedMessage(name)
		return decision
	}
	if m != nil && m.enforceToolSettings && manifest.Capability != "" && !config.AgentToolAllowed(m.toolSettings, manifest.Capability) {
		decision.Action = "blocked"
		decision.Reason = disabledToolCapabilityMessage(manifest.Name, manifest.Capability)
	}
	return decision
}

func disabledToolCapabilityMessage(name, capability string) string {
	return fmt.Sprintf("[tool error] 工具 %q 需要当前 Agent 启用 %s 能力，但该能力已关闭。请改用已授权工具，或请用户在 Agent Tools 中开启该能力。 / Tool %q requires capability %s, which is disabled for this Agent.", name, capability, name, capability)
}

func applyToolArgumentValidation(decision ToolDecision, args string, outcome LLMOutcome) ToolDecision {
	if decision.Action == "blocked" {
		return decision
	}
	if err := validateToolArgumentsJSON(args); err != nil {
		argsComplete := false
		decision.ArgsComplete = &argsComplete
		decision.ModelFinishReason = strings.TrimSpace(outcome.FinishReason)
		decision.Action = "blocked"
		decision.Reason = invalidToolArgumentsMessage(decision, args, err, outcome)
	}
	return decision
}

func invalidToolArgumentsMessage(decision ToolDecision, args string, err error, outcome LLMOutcome) string {
	if isContentFilterInterruptedArguments(err, decision, outcome) {
		target := strings.TrimSpace(decision.Target)
		if target == "" {
			target = "(unknown)"
		}
		return fmt.Sprintf(`[tool error]
type: invalid_tool_arguments
tool: %s
reason: model_output_interrupted_by_content_filter
retryable: false
workspace_mutated: false
args_complete: false
args_bytes: %d
model_finish_reason: %s
target: %s

中文：模型在生成工具参数时被内容过滤中断，arguments 不是完整 JSON 对象：%v。Denova 已阻止工具执行，文件未写入。请直接告知用户本次写入失败的原因，不要重试同一个写入工具。
English: The model output was stopped by content filtering while producing tool arguments, so arguments are not a complete JSON object: %v. Denova blocked tool execution and no file was written. Tell the user what happened; do not retry the same write tool.`, decision.ToolName, len(args), strings.TrimSpace(outcome.FinishReason), target, err, err)
	}
	return fmt.Sprintf(`[tool error]
type: invalid_tool_arguments
tool: %s
retryable: true
workspace_mutated: false
args_complete: false
args_bytes: %d

中文：工具 %q 的参数不是完整 JSON 对象：%v。请修正 arguments，确保它是完整、合法的 JSON object；字符串里的换行、引号和反斜杠必须正确转义。
English: Tool %q arguments are not a complete JSON object: %v. Tool arguments must be a complete JSON object; fix arguments and escape newlines, quotes, and backslashes inside strings.`, decision.ToolName, len(args), decision.ToolName, err, decision.ToolName, err)
}

func isContentFilterInterruptedArguments(err error, decision ToolDecision, outcome LLMOutcome) bool {
	if !isIncompleteJSONArgumentsError(err) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(outcome.FinishReason), "content_filter") {
		return false
	}
	return decision.MutatesWorkspace || decision.Source == ToolSourceWrite
}

func isIncompleteJSONArgumentsError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		strings.Contains(strings.ToLower(err.Error()), "unexpected eof")
}

func validateToolArgumentsJSON(args string) error {
	args = strings.TrimSpace(args)
	if args == "" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(args))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	if payload == nil {
		return fmt.Errorf("arguments must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("arguments contain trailing JSON data")
		}
		return fmt.Errorf("arguments contain trailing data: %w", err)
	}
	return nil
}

func (m *toolOrchestratorMiddleware) effectivePolicyKind() string {
	if m == nil {
		return ""
	}
	if strings.TrimSpace(m.policyKind) != "" {
		return m.policyKind
	}
	return m.agentKind
}

func toolCallID(toolCtx *adk.ToolContext) string {
	if toolCtx == nil {
		return ""
	}
	return toolCtx.CallID
}
