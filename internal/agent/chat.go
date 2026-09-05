package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"strings"
	"time"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/schema"

	"denova/config"
	"denova/internal/book"
	"denova/internal/observability"
	"denova/internal/prompts"
	"denova/internal/session"
)

const (
	maxReferenceFileBytes  = 128 * 1024
	maxReferenceTotalBytes = 200 * 1024

	// Tool-heavy turns need additional reserve for function arguments + protocol overhead.
	toolModeMinReserveTokens = 8000
	toolModeSafeWindowRatio  = 0.70
	// Hard safety cap for tool-heavy runs. Local backends may advertise very large
	// windows (for example 256k), but tool-call JSON reliability drops long before that.
	toolModeHardWindowCapTokens = 81920
	// toolModePhysicalSafetyRatio 在钳制后的窗口上再打折，作为 pre-run 预算闸门的
	// 物理安全触发线。宣称窗口（如 200k/256k）常远超 24GB 显存等硬件能实际承受的
	// 上下文长度（KV+长 prefill 缓冲会先 OOM），直接信任宣称值会让压缩触发过晚。
	// 与配置对齐（把 context_window_tokens 设成物理可承受值）配合实现"预警压缩"。
	toolModePhysicalSafetyRatio = 0.85
)

// Event 表示 Agent 输出的传输无关事件。
type Event struct {
	Type string
	Data interface{}
}

// ChatRequest 表示一次聊天请求的传输无关参数。
type ChatRequest struct {
	Message        string             `json:"message"`
	References     []string           `json:"references"`
	LoreReferences []string           `json:"lore_references"`
	StyleScenes    []string           `json:"style_scenes"`
	Selections     []TextSelectionRef `json:"selections"`
	IDEContext     IDEContextRef      `json:"ide_context,omitempty"`
	ReviewFeedback ReviewFeedbackRefs `json:"review_feedback,omitempty"`
	PlanMode       bool               `json:"plan_mode"`
	WritingSkill   string             `json:"writing_skill"`
	ImagePresetID  string             `json:"image_preset_id"`
	TellerID       string             `json:"teller_id"`
	Locale         string             `json:"-"`

	// StyleRules 由后端按当前导演配置注入（场景 → 共享文风参考索引）。
	// StyleScenes 非空时只注入用户本轮通过 # 指定的场景；为空时作为场景化建议参与本轮上下文。
	StyleRules []StyleRule `json:"-"`

	// ImagePreset is resolved by the app layer from ImagePresetID or workspace settings.
	ImagePreset ImagePresetContext `json:"-"`

	// ResolvedReviewFeedback is populated by the app layer from a canonical
	// workspace review ledger. Clients may submit IDs only, never comment text.
	ResolvedReviewFeedback ReviewFeedbackContexts `json:"-"`
}

// StyleRule 是 prompts.StyleRule 的镜像，避免调用方直接依赖 prompts 包。
type StyleRule = prompts.StyleRule

// StyleReference 是 prompts.StyleReference 的镜像，避免调用方直接依赖 prompts 包。
type StyleReference = prompts.StyleReference

// IDEContextRef carries lightweight, model-visible IDE state for one turn.
// It must describe UI focus only and must not include editor file content.
type IDEContextRef struct {
	CurrentFile string   `json:"current_file,omitempty"`
	OpenFiles   []string `json:"open_files,omitempty"`
}

// ImagePresetContext is a bounded visual style preset for image generation only.
type ImagePresetContext struct {
	ID                string
	Name              string
	AgentSystemPrompt string
	ToolRequestPrompt string
}

// TextSelectionRef 表示用户在编辑器中选中的一段文本引用。
type TextSelectionRef struct {
	FileName  string `json:"file_name"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
}

// ChatService 编排会话历史、文件引用和 Agent 流式响应。
type ChatService struct {
	policy  LoopPolicy
	runtime *Runtime
}

// Runtime owns the task-level Agent loop: context assembly, tool observation,
// durable run state, post-run verification, and final lifecycle events.
type Runtime struct {
	policy LoopPolicy
}

// NewChatService 创建聊天服务。
func NewChatService() *ChatService {
	return NewChatServiceWithPolicy(DefaultLoopPolicy())
}

// NewChatServiceWithPolicy 创建带显式 loop policy 的聊天服务，主要用于测试和后续分 Agent 配置。
func NewChatServiceWithPolicy(policy LoopPolicy) *ChatService {
	policy = policy.normalized()
	return &ChatService{policy: policy, runtime: NewRuntime(policy)}
}

func NewRuntime(policy LoopPolicy) *Runtime {
	return &Runtime{policy: policy.normalized()}
}

func (s *ChatService) RunWithOptions(
	ctx context.Context,
	runner *adk.Runner,
	conversation Conversation,
	bookService *book.Service,
	req ChatRequest,
	options RunOptions,
	emit func(Event),
) {
	runtime := NewRuntime(DefaultLoopPolicy())
	if s != nil {
		if s.runtime != nil {
			runtime = s.runtime
		} else {
			runtime = NewRuntime(s.policy)
		}
	}
	runtime.Run(ctx, runner, conversation, bookService, req, options, emit)
}

func (r *Runtime) Run(
	ctx context.Context,
	runner *adk.Runner,
	conversation Conversation,
	bookService *book.Service,
	req ChatRequest,
	options RunOptions,
	emit func(Event),
) {
	if emit == nil {
		emit = func(Event) {}
	}
	runLogger := observability.Logger("agent-run")
	policy := DefaultLoopPolicy()
	if r != nil {
		policy = r.policy.normalized()
	}
	workspace := ""
	if bookService != nil {
		workspace = bookService.Workspace()
	}
	options = options.normalized(workspace)
	options.SystemPromptLog.logForRun(options)
	runLedger, ledgerErr := newRunLedgerWithOptions(workspace, policy.RunLedger, options)
	if ledgerErr != nil {
		runLogger.Warn("run_ledger_unavailable", slog.String("workspace", workspace), slog.Any("error", ledgerErr))
	}
	rootSpan := StartRootTraceSpan(runLedger, map[string]any{
		"workspace":        workspace,
		"task_id":          options.TaskID,
		"agent_kind":       options.AgentKind,
		"session_id":       options.SessionID,
		"review_thread_id": options.ReviewThreadID,
		"story_id":         options.StoryID,
		"branch_id":        options.BranchID,
		"turn_id":          options.TurnID,
		"maintenance_task": options.MaintenanceTask,
		"mode":             options.Mode,
	})
	rootSpanID := ""
	if rootSpan != nil {
		rootSpanID = rootSpan.SpanID()
	}
	runID := ""
	if runLedger != nil {
		runID = runLedger.ID()
	}
	if runID == "" {
		runID = options.TaskID
	}
	traceCtx := ContextWithRunTrace(ctx, runID, runLedger, rootSpanID)
	checkpointID := options.checkpointID(runID)
	observer := newRunObserverWithIdentity(runLedger, rootSpanID, runID, options.SessionID, options.ReviewThreadID)
	// T1: Probe the actual context window from the model server's /v1/models
	// endpoint (llama-server exposes meta.n_ctx). The result is cached per
	// base URL; if the probe fails or the server does not expose n_ctx, the
	// declared window is used unchanged. This does not modify user config.
	{
		probeModel := config.ResolveAgentModel(conversationConfig(conversation), options.AgentKind)
		if probeModel.OpenAIBaseURL != "" {
			ensureActualContextWindow(ctx, probeModel.OpenAIBaseURL, probeModel.OpenAIAPIKey)
		}
	}
	usageCollector := newRunTokenUsageCollector(runID, options.AgentKind)
	if runLedger != nil {
		defer func() {
			if err := runLedger.Close(); err != nil {
				runLogger.Warn("run_ledger_close_failed", slog.String("run_id", runID), slog.Any("error", err))
			}
		}()
	}
	finished := false
	finishRun := func(status, reason string, generatedBytes int) {
		if finished {
			return
		}
		finished = true
		usageCollector.EmitIfAny(emit, generatedBytes)
		traceMetadata := runTraceMetadataForConversation(options, conversation)
		if !traceMetadata.empty() {
			if err := runLedger.Record("run_context", traceMetadata.record()); err != nil {
				runLogger.Warn("run_ledger_context_metadata_failed", slog.String("run_id", runID), slog.Any("error", err))
			}
		}
		recordRuntimeStateIfNeeded(conversation, options)
		if rootSpan != nil {
			rootSpan.Finish(status, map[string]any{
				"reason":           strings.TrimSpace(reason),
				"generated_bytes":  generatedBytes,
				"story_id":         traceMetadata.StoryID,
				"branch_id":        traceMetadata.BranchID,
				"turn_id":          traceMetadata.TurnID,
				"maintenance_task": traceMetadata.MaintenanceTask,
			})
		}
		if err := runLedger.RecordFinish(status, reason, generatedBytes); err != nil {
			runLogger.Warn("run_ledger_finish_failed", slog.String("run_id", runID), slog.Any("error", err))
		}
	}

	assistantMetadata := session.MessageMetadata{
		RunID:         runID,
		AgentKind:     options.AgentKind,
		AgentName:     options.RootAgentName,
		RootAgentName: options.RootAgentName,
	}
	if options.RootAgentName != "" {
		assistantMetadata.RunPath = []string{options.RootAgentName}
	}
	subAgentSessions := newSubAgentSessionTracker(runID)
	recorder := newDisplayEventRecorder(conversation)
	toolContextRecorder := newToolResultContextRecorder(conversation)
	mutations := newMutationTracker()
	rawEmit := emit
	emit = func(ev Event) {
		mutations.Observe(ev)
		recorder.Record(ev)
		if err := runLedger.RecordEvent(ev); err != nil {
			runLogger.Warn("run_ledger_event_failed", slog.String("run_id", runID), slog.String("event_type", ev.Type), slog.Any("error", err))
		}
		rawEmit(ev)
	}
	emit(Event{Type: "run_state", Data: map[string]string{
		"run_id":           runID,
		"task_id":          options.TaskID,
		"agent_kind":       options.AgentKind,
		"session_id":       options.SessionID,
		"review_thread_id": options.ReviewThreadID,
		"story_id":         options.StoryID,
		"branch_id":        options.BranchID,
		"turn_id":          options.TurnID,
		"maintenance_task": options.MaintenanceTask,
		"root_agent_name":  options.RootAgentName,
		"phase":            "started",
	}})
	originalMessage := req.Message
	if err := runLedger.Record("run_started", map[string]any{
		"workspace":        workspace,
		"task_id":          options.TaskID,
		"agent_kind":       options.AgentKind,
		"session_id":       options.SessionID,
		"review_thread_id": options.ReviewThreadID,
		"story_id":         options.StoryID,
		"branch_id":        options.BranchID,
		"turn_id":          options.TurnID,
		"maintenance_task": options.MaintenanceTask,
		"mode":             options.Mode,
		"message":          textSummary{Bytes: len(originalMessage), Chars: len([]rune(originalMessage)), Preview: safeLogPreview(originalMessage, policy.RunLedger.PreviewChars)},
		"references":       len(req.References),
		"lore_references":  len(req.LoreReferences),
		"style_scenes":     len(req.StyleScenes),
		"selections":       len(req.Selections),
		"plan_mode":        req.PlanMode,
		"writing_skill":    req.WritingSkill,
		"checkpoint_id":    checkpointID,
	}); err != nil {
		runLogger.Warn("run_ledger_start_failed", slog.String("run_id", runID), slog.Any("error", err))
	}
	var pendingInterruption *session.Interruption
	if shouldResumeInterruptedRequest(req.Message) {
		pendingInterruption = conversation.PendingInterruption()
	}
	contextBuildStarted := time.Now()
	composition := composeAgentInput(req, pendingInterruption, bookService, policy)
	req = composition.Request
	originalMessage = composition.OriginalMessage
	resumeInterruption := composition.ResumeInterruption
	defer func() {
		if recovered := recover(); recovered != nil {
			runLogger.Error("panic_recovered", slog.Any("error", recovered))
			markInterruptionIfNeeded(conversation, resumeInterruption, originalMessage, "", fmt.Sprint(recovered))
			finishRun("panic", fmt.Sprint(recovered), 0)
			emit(Event{Type: "error", Data: map[string]string{"message": "Agent 异常中断"}})
		}
	}()

	agentMessage := composition.AgentMessage
	contextLog := composition.ContextLog
	if setter, ok := conversation.(UserMessageReferencesSetter); ok {
		setter.SetUserMessageReferences(userMessageReferencesForRequest(req))
	}
	// Bind the active model name so it can be surfaced per message (Plan.md 11 / F2).
	if setter, ok := conversation.(ModelNameSetter); ok {
		resolved := config.ResolveAgentModel(conversationConfig(conversation), options.AgentKind)
		if strings.TrimSpace(resolved.OpenAIModel) != "" {
			setter.SetModelName(resolved.OpenAIModel)
		}
	}

	history, err := conversation.PrepareMessages(originalMessage, agentMessage)
	if err != nil {
		runLogger.Error("prepare_messages_failed", slog.Any("error", err))
		finishRun("error", err.Error(), 0)
		emit(Event{Type: "error", Data: map[string]string{"message": err.Error()}})
		return
	}
	if options.OnUserMessageCommitted != nil {
		if err := options.OnUserMessageCommitted(traceCtx); err != nil {
			runLogger.Error("commit_user_message_side_effect_failed", slog.Any("error", err))
			finishRun("error", err.Error(), 0)
			emit(Event{Type: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
		emit(Event{Type: "workspace_change", Data: map[string]interface{}{
			"workspace":        options.Workspace,
			"review_thread_id": options.ReviewThreadID,
			"action":           "review_feedback_consumed",
		}})
	}
	if compactor, ok := conversation.(ContextCompactionConversation); ok {
		compactionStarted := time.Now()
		completionReserve, toolReserve := runtimeContextProjectionReserves(conversation, options.AgentKind)
		compactedHistory, compactionResult, compactErr := compactor.CompactContextIfNeeded(ContextWithRunObserver(traceCtx, observer), ContextCompactionInput{
			Messages:                 history,
			AgentMessage:             agentMessage,
			Phase:                    contextCompactionPhasePreRun,
			Emit:                     emit,
			ReservedCompletionTokens: completionReserve,
			ReservedToolResultTokens: toolReserve,
		})
		if compactErr != nil {
			runLogger.Error("context_compaction_failed", slog.Any("error", compactErr), slog.Int("tokens_before", compactionResult.TokensBefore), slog.Int("context_window_tokens", compactionResult.ContextWindowTokens))
			finishRun("error", compactErr.Error(), 0)
			emit(Event{Type: "error", Data: map[string]string{"message": compactErr.Error()}})
			return
		}
		history = compactedHistory
		history, _ = enforceToolSafeBudgetBeforeRun(traceCtx, conversation, options, runLogger, history, agentMessage, emit)
		if compactionResult.Triggered {
			runLogger.Info("context_compacted", slog.String("phase", compactionResult.Phase), slog.Int("epoch", compactionResult.Epoch), slog.Int("tokens_before", compactionResult.TokensBefore), slog.Int("projected_tokens_before", compactionResult.ProjectedTokensBefore), slog.Int("tokens_after", compactionResult.TokensAfter), slog.Int("projected_tokens_after", compactionResult.ProjectedTokensAfter), slog.Int("context_window_tokens", compactionResult.ContextWindowTokens))
			if err := runLedger.Record("context_compaction", map[string]any{
				"phase":                       compactionResult.Phase,
				"epoch":                       compactionResult.Epoch,
				"tokens_before":               compactionResult.TokensBefore,
				"projected_tokens_before":     compactionResult.ProjectedTokensBefore,
				"reserved_completion_tokens":  compactionResult.ReservedCompletionTokens,
				"reserved_tool_result_tokens": compactionResult.ReservedToolResultTokens,
				"tokens_after":                compactionResult.TokensAfter,
				"projected_tokens_after":      compactionResult.ProjectedTokensAfter,
				"context_window_tokens":       compactionResult.ContextWindowTokens,
				"threshold":                   compactionResult.Threshold,
			}); err != nil {
				runLogger.Warn("run_ledger_context_compaction_failed", slog.String("run_id", runID), slog.Any("error", err))
			}
			RecordCompletedTraceSpan(traceCtx, "context_compaction", compactionStarted, "success", map[string]any{
				"phase":                   compactionResult.Phase,
				"epoch":                   compactionResult.Epoch,
				"tokens_before":           compactionResult.TokensBefore,
				"projected_tokens_before": compactionResult.ProjectedTokensBefore,
				"tokens_after":            compactionResult.TokensAfter,
				"projected_tokens_after":  compactionResult.ProjectedTokensAfter,
				"context_window_tokens":   compactionResult.ContextWindowTokens,
				"threshold":               compactionResult.Threshold,
			})
		}
	}
	contextLedgerParts := contextLedgerPartsForConversation(contextLog, conversation, history)
	if err := runLedger.RecordContext(contextLedgerParts); err != nil {
		runLogger.Warn("run_ledger_context_failed", slog.String("run_id", runID), slog.Any("error", err))
	}
	RecordCompletedTraceSpan(traceCtx, "context_build", contextBuildStarted, "success", map[string]any{
		"history_messages":    len(history),
		"context_parts":       len(contextLedgerParts),
		"message_chars":       len([]rune(originalMessage)),
		"agent_message_chars": len([]rune(agentMessage)),
		"plan_mode":           req.PlanMode,
		"writing_skill":       req.WritingSkill,
	})
	runLogger.Info(
		"context_composition",
		slog.String("history", messageListSummary(history)),
		slog.String("original", promptPartSummary(originalMessage)),
		slog.String("agent_message", promptPartSummary(agentMessage)),
		slog.String("references", stringListSummary(req.References)),
		slog.String("lore_references", stringListSummary(req.LoreReferences)),
		slog.String("style_scenes", stringListSummary(req.StyleScenes)),
		slog.Int("style_rules", len(req.StyleRules)),
		slog.String("selections", selectionListSummary(req.Selections)),
		slog.Bool("plan_mode", req.PlanMode),
		slog.String("writing_skill", req.WritingSkill),
		slog.Bool("resumed", resumeInterruption != nil),
	)
	runLogger.Info("context_sources", slog.String("summary", contextLog.String()), slog.Any("sources", contextLog.Audit()))
	if reporter, ok := conversation.(ContextSourceReporter); ok {
		if sources := strings.TrimSpace(reporter.ContextSourceSummary()); sources != "" {
			runLogger.Info("conversation_context_sources", slog.String("sources", sources))
		}
	}

	runCtx, cancelRun := context.WithCancel(contextWithCompactionController(ContextWithRunObserver(traceCtx, observer), conversation))
	defer cancelRun()
	runOptions := []adk.AgentRunOption{}
	if options.AgentKind == AgentKindInteractiveStory {
		cancelOption, cancelAgent := adk.WithCancel()
		runCtx = withInteractiveTurnCancel(runCtx, cancelAgent)
		runOptions = append(runOptions, cancelOption)
	} else if isInteractiveDirectorPlanRun(options.AgentKind, options.MaintenanceTask) {
		cancelOption, cancelAgent := adk.WithCancel()
		runCtx = withInteractiveDirectorPlanCancel(runCtx, cancelAgent)
		runOptions = append(runOptions, cancelOption)
	}
	if checkpointID != "" {
		runOptions = append(runOptions, adk.WithCheckPointID(checkpointID))
	}
	events := runner.Run(runCtx, history, runOptions...)
	var fullContent strings.Builder
	var fullThinking strings.Builder
	var planParser *planProtocolParser
	if req.PlanMode {
		planMeta := agentEventMetadata{
			AgentKind:     options.AgentKind,
			RunID:         runID,
			AgentName:     options.RootAgentName,
			RootAgentName: options.RootAgentName,
		}
		if options.RootAgentName != "" {
			planMeta.RunPath = []string{options.RootAgentName}
		}
		planParser = newPlanProtocolParser(planMeta, emit)
	}
	runLogger.Info("run_started", slog.Int("history", len(history)), slog.Int("message_len", len(req.Message)), slog.Int("agent_message_len", len(agentMessage)), slog.Bool("plan_mode", req.PlanMode), slog.String("writing_skill", req.WritingSkill), slog.Int("style_scenes", len(req.StyleScenes)), slog.Int("style_rules", len(req.StyleRules)))
	parseToolArgsRetryUsed := false

	for {
		if err := ctx.Err(); err != nil {
			runLogger.Warn("run_interrupted", slog.String("reason", "context"), slog.Any("error", err), slog.Int("generated_bytes", fullContent.Len()))
			flushPlanProtocolParser(planParser, &fullContent, emit)
			discardPlanAssistantContentIfNeeded(req.PlanMode, planParser, &fullContent, &fullThinking)
			generatedBytes := fullContent.Len()
			if _, persistErr := appendAssistantIfAny(conversation, &fullContent, &fullThinking, assistantMetadata); persistErr != nil {
				runLogger.Error("persist_interrupted_assistant_failed", slog.Any("error", persistErr))
			}
			finishRun("aborted", err.Error(), generatedBytes)
			emit(Event{Type: "aborted", Data: map[string]string{}})
			return
		}
		event, ok, waitErr := waitForRunnerEvent(runCtx, events, options.IdleTimeout)
		if waitErr != nil {
			flushPlanProtocolParser(planParser, &fullContent, emit)
			discardPlanAssistantContentIfNeeded(req.PlanMode, planParser, &fullContent, &fullThinking)
			generated, persistErr := appendAssistantIfAny(conversation, &fullContent, &fullThinking, assistantMetadata)
			if persistErr != nil {
				runLogger.Error("persist_interrupted_assistant_failed", slog.Any("error", persistErr))
			}
			if ctx.Err() != nil {
				runLogger.Warn("run_interrupted", slog.String("reason", "context"), slog.Any("error", ctx.Err()), slog.Int("generated_bytes", len(generated)))
				finishRun("aborted", ctx.Err().Error(), len(generated))
				emit(Event{Type: "aborted", Data: map[string]string{}})
				return
			}
			cancelRun()
			runLogger.Error("run_interrupted", slog.String("reason", "idle_timeout"), slog.Any("error", waitErr), slog.Int("generated_bytes", len(generated)))
			markInterruptionIfNeeded(conversation, resumeInterruption, originalMessage, generated, waitErr.Error())
			finishRun("error", waitErr.Error(), len(generated))
			emit(Event{Type: "error", Data: map[string]string{"message": waitErr.Error()}})
			return
		}
		if !ok {
			break
		}
		if event.Err != nil {
			// 响应式压缩（被动兜底）：三类服务端/客户端错误之一命中即强制压缩上下文
			// 后重跑一次，避免无限重试——对应 Plan.md §3.5「响应式压缩防循环」：
			//   1. 工具参数 JSON 解析失败（截断）；
			//   2. 模型服务端内存不足（本地模型长上下文 bad allocation / out of memory）；
			//   3. 服务端因上下文超长拒绝请求（客户端 token 估算偏差导致）。
			// 整条恢复链只允许尝试一次，失败就把原始错误直接暴露出去，绝不循环。
			parseArgsErr := isToolArgumentsParseError(event.Err)
			memoryErr := isModelMemoryError(event.Err)
			serverReject := isServerRejectionError(event.Err)
			// T1 被动兜底校准：服务端因上下文超长拒绝请求时，把当次 projected
			// token 数（×0.9 安全系数）记录为该模型服务端的实际窗口上界。后续
			// run 的压缩策略与预算闸门用 min(声明窗口, 校准窗口) 提前生效。
			// 仅运行态缓存，不落盘用户配置。
			if serverReject {
				rejectedTokens := 0
				if obs := RunObserverFromContext(traceCtx); obs != nil {
					rejectedTokens = obs.BudgetSnapshot().ProjectedTokens
				}
				if rejectedTokens <= 0 {
					rejectedTokens = EstimateContextTokens(history, nil)
				}
				if rejectedTokens > 0 {
					rejectModel := config.ResolveAgentModel(conversationConfig(conversation), options.AgentKind)
					recordServerRejectionCalibration(rejectModel.OpenAIBaseURL, rejectedTokens)
				}
			}
			if !parseToolArgsRetryUsed && (parseArgsErr || memoryErr || serverReject) {
				parseToolArgsRetryUsed = true
				retryReason := "server_rejection"
				switch {
				case parseArgsErr:
					retryReason = "tool_arguments_parse_error"
				case memoryErr:
					retryReason = "model_memory_error"
				}
				runLogger.Warn("tool_error_retry_with_compaction", slog.String("reason", retryReason), slog.Any("error", event.Err))
				if compactor, ok := conversation.(ContextCompactionConversation); ok {
					completionReserve, toolReserve := runtimeContextProjectionReserves(conversation, options.AgentKind)
					compactedHistory, compactionResult, compactErr := compactor.CompactContextIfNeeded(ContextWithRunObserver(traceCtx, observer), ContextCompactionInput{
						Messages:                 history,
						AgentMessage:             agentMessage,
						Phase:                    contextCompactionPhasePreRun,
						Emit:                     emit,
						Force:                    true,
						KeepLatestUser:           true,
						ReservedCompletionTokens: completionReserve,
						ReservedToolResultTokens: toolReserve,
					})
					if compactErr == nil {
						history = compactedHistory
						runLogger.Info("tool_arguments_parse_error_recovered", slog.Bool("compacted", compactionResult.Triggered), slog.Int("tokens_before", compactionResult.TokensBefore), slog.Int("projected_before", compactionResult.ProjectedTokensBefore), slog.Int("tokens_after", compactionResult.TokensAfter), slog.Int("projected_after", compactionResult.ProjectedTokensAfter))
					}
				}
				fullContent.Reset()
				fullThinking.Reset()
				runOptionsRetry := []adk.AgentRunOption{}
				if options.AgentKind == AgentKindInteractiveStory {
					cancelOption, cancelAgent := adk.WithCancel()
					runCtx = withInteractiveTurnCancel(runCtx, cancelAgent)
					runOptionsRetry = append(runOptionsRetry, cancelOption)
				} else if isInteractiveDirectorPlanRun(options.AgentKind, options.MaintenanceTask) {
					cancelOption, cancelAgent := adk.WithCancel()
					runCtx = withInteractiveDirectorPlanCancel(runCtx, cancelAgent)
					runOptionsRetry = append(runOptionsRetry, cancelOption)
				}
				if checkpointID != "" {
					runOptionsRetry = append(runOptionsRetry, adk.WithCheckPointID(checkpointID))
				}
				events = runner.Run(runCtx, history, runOptionsRetry...)
				continue
			}
			if interactiveTurnCompletedByCancel(event.Err, options.AgentKind, conversation, fullContent.Len()) {
				if err := removeCheckpoint(options.Workspace, options.AgentKind, checkpointID); err != nil {
					runLogger.Warn("interactive_completion_checkpoint_cleanup_failed", slog.String("checkpoint_id", checkpointID), slog.Any("error", err))
				}
				runLogger.Info("interactive_turn_completed_after_submission", slog.Int("generated_bytes", fullContent.Len()))
				break
			}
			if interactiveDirectorPlanCompletedByCancel(event.Err, options.AgentKind, options.MaintenanceTask) {
				if err := removeCheckpoint(options.Workspace, options.AgentKind, checkpointID); err != nil {
					runLogger.Warn("interactive_director_completion_checkpoint_cleanup_failed", slog.String("checkpoint_id", checkpointID), slog.Any("error", err))
				}
				runLogger.Info("interactive_director_plan_completed_after_submission")
				break
			}
			if reason, retrying := interactiveCompletionRetryFromError(event.Err); retrying {
				runLogger.Info("interactive_completion_retry", slog.String("code", reason.Code), slog.Int("generated_bytes", fullContent.Len()))
				continue
			}
			runLogger.Error("run_interrupted", slog.String("reason", "runner_error"), slog.Any("error", event.Err), slog.Int("generated_bytes", fullContent.Len()))
			flushPlanProtocolParser(planParser, &fullContent, emit)
			discardPlanAssistantContentIfNeeded(req.PlanMode, planParser, &fullContent, &fullThinking)
			generated, persistErr := appendAssistantIfAny(conversation, &fullContent, &fullThinking, assistantMetadata)
			if persistErr != nil {
				runLogger.Error("persist_interrupted_assistant_failed", slog.Any("error", persistErr))
			}
			markInterruptionIfNeeded(conversation, resumeInterruption, originalMessage, generated, event.Err.Error())
			finishRun("error", event.Err.Error(), len(generated))
			emit(Event{Type: "error", Data: map[string]string{"message": event.Err.Error()}})
			return
		}

		if event.Output == nil || event.Output.MessageOutput == nil {
			runLogger.Warn("invalid_output_skipped", slog.Bool("output_nil", event.Output == nil), slog.Bool("message_output_nil", event.Output != nil && event.Output.MessageOutput == nil))
			continue
		}

		eventMeta := subAgentSessions.decorate(metadataForAgentEvent(event, options.RootAgentName))
		eventMeta.AgentKind = options.AgentKind
		mv := event.Output.MessageOutput
		if mv.Role == schema.Tool {
			if mv.Message == nil {
				continue
			}
			content, drainErr := drainContent(runCtx, mv, options.IdleTimeout)
			if drainErr != nil {
				discardPlanAssistantContentIfNeeded(req.PlanMode, planParser, &fullContent, &fullThinking)
				generated, persistErr := appendAssistantIfAny(conversation, &fullContent, &fullThinking, assistantMetadata)
				if persistErr != nil {
					runLogger.Error("persist_interrupted_assistant_failed", slog.Any("error", persistErr))
				}
				if ctx.Err() != nil {
					runLogger.Warn("run_interrupted", slog.String("reason", "context"), slog.Any("error", ctx.Err()), slog.Int("generated_bytes", len(generated)))
					finishRun("aborted", ctx.Err().Error(), len(generated))
					emit(Event{Type: "aborted", Data: map[string]string{}})
					return
				}
				cancelRun()
				runLogger.Error("run_interrupted", slog.String("reason", "tool_result_idle_timeout"), slog.Any("error", drainErr), slog.Int("generated_bytes", len(generated)))
				markInterruptionIfNeeded(conversation, resumeInterruption, originalMessage, generated, drainErr.Error())
				finishRun("error", drainErr.Error(), len(generated))
				emit(Event{Type: "error", Data: map[string]string{"message": drainErr.Error()}})
				return
			}
			fullToolContent := content
			if content == "" {
				content = "(无返回内容)"
			}
			logToolResult(mv.Message.ToolName, mv.Message.ToolCallID, content)
			usageCollector.NoteToolResult(mv.Message.ToolName)
			data := eventMeta.appendTo(map[string]interface{}{
				"id":      mv.Message.ToolCallID,
				"name":    mv.Message.ToolName,
				"content": content,
			})
			if itemIDs, deletedIDs := parseWriteLoreItemsToolResult(mv.Message.ToolName, fullToolContent); len(itemIDs) > 0 || len(deletedIDs) > 0 {
				data["item_ids"] = itemIDs
				data["deleted_ids"] = deletedIDs
			}
			if illustrationResult, parseErr := parseChapterIllustrationToolResult(mv.Message.ToolName, fullToolContent); parseErr != nil {
				runLogger.Warn("parse_chapter_illustration_result_failed", slog.String("tool", mv.Message.ToolName), slog.Any("error", parseErr))
			} else if illustrationResult != nil {
				data["illustration"] = illustrationResult
				data["target"] = illustrationResult.MetaPath
			} else if interactiveImageResult, parseErr := parseInteractiveImageToolResult(mv.Message.ToolName, fullToolContent); parseErr != nil {
				runLogger.Warn("parse_interactive_image_result_failed", slog.String("tool", mv.Message.ToolName), slog.Any("error", parseErr))
			} else if interactiveImageResult != nil {
				data["interactive_image"] = interactiveImageResult
				data["target"] = interactiveImageResult.MetaPath
			} else if target := parseGeneratedImageToolTarget(mv.Message.ToolName, fullToolContent); target != "" {
				data["target"] = target
			}
			if receipt, ok := parseWorkspaceChangeToolReceipt(mv.Message.ToolName, fullToolContent); ok {
				data["workspace_change"] = receipt
				workspaceChangeData := eventMeta.appendTo(map[string]interface{}{
					"id":               receipt.ChangeSetID,
					"workspace":        receipt.Workspace,
					"change_group_id":  receipt.ChangeGroupID,
					"review_thread_id": receipt.ReviewThreadID,
					"change_set_id":    receipt.ChangeSetID,
					"path":             receipt.Path,
					"affected_paths":   []string{receipt.Path},
					"base_revision":    receipt.BaseRevision,
					"revision":         receipt.Revision,
					"review_status":    receipt.ReviewStatus,
					"apply_state":      receipt.ApplyState,
					"workspace_change": receipt,
				})
				emit(Event{Type: "workspace_change", Data: workspaceChangeData})
			}
			toolContextRecorder.RecordToolResult(mv.Message.ToolName, mv.Message.ToolCallID, content, eventMeta)
			emit(Event{Type: "tool_result", Data: data})
			continue
		}

		if mv.Role != schema.Assistant && mv.Role != "" {
			continue
		}
		if mv.IsStreaming && mv.MessageStream != nil {
			msg, streamErr := processStreamingEvent(runCtx, mv, &fullContent, &fullThinking, options.IdleTimeout, options.ToolResultMaxBytes, eventMeta, planParser, emit)
			if streamErr != nil {
				// A completion-guard retry arrives after all response frames. Preserve
				// the rejected call's provider usage even though its prose is discarded.
				usageCollector.AddMessage(msg)
				if reason, retrying := interactiveCompletionRetryFromError(streamErr); retrying {
					runLogger.Info("interactive_completion_retry", slog.String("code", reason.Code), slog.Int("generated_bytes", fullContent.Len()))
					continue
				}
				flushPlanProtocolParser(planParser, &fullContent, emit)
				discardPlanAssistantContentIfNeeded(req.PlanMode, planParser, &fullContent, &fullThinking)
				generated, persistErr := appendAssistantIfAny(conversation, &fullContent, &fullThinking, assistantMetadata)
				if persistErr != nil {
					runLogger.Error("persist_interrupted_assistant_failed", slog.Any("error", persistErr))
				}
				if ctx.Err() != nil {
					runLogger.Warn("run_interrupted", slog.String("reason", "context"), slog.Any("error", ctx.Err()), slog.Int("generated_bytes", len(generated)))
					finishRun("aborted", ctx.Err().Error(), len(generated))
					emit(Event{Type: "aborted", Data: map[string]string{}})
					return
				}
				cancelRun()
				markInterruptionIfNeeded(conversation, resumeInterruption, originalMessage, generated, streamErr.Error())
				finishRun("error", streamErr.Error(), len(generated))
				return
			}
			toolContextRecorder.RecordAssistantToolCalls(msg, eventMeta)
			usageCollector.AddMessage(msg)
			if req.PlanMode && planParser != nil && planParser.HasSuccessfulBlock() {
				cancelRun()
				break
			}
			continue
		}
		if mv.Message != nil {
			processNonStreamingEvent(mv, &fullContent, &fullThinking, options.ToolResultMaxBytes, eventMeta, planParser, emit)
			toolContextRecorder.RecordAssistantToolCalls(mv.Message, eventMeta)
			usageCollector.AddMessage(mv.Message)
			if req.PlanMode && planParser != nil && planParser.HasSuccessfulBlock() {
				cancelRun()
				break
			}
		}
	}

	flushPlanProtocolParser(planParser, &fullContent, emit)
	discardPlanAssistantContentIfNeeded(req.PlanMode, planParser, &fullContent, &fullThinking)
	generatedBytes := fullContent.Len()
	if _, persistErr := appendAssistantIfAny(conversation, &fullContent, &fullThinking, assistantMetadata); persistErr != nil {
		runLogger.Error("persist_assistant_failed", slog.Any("error", persistErr), slog.Int("generated_bytes", generatedBytes))
		finishRun("error", persistErr.Error(), generatedBytes)
		emit(Event{Type: "run_state", Data: map[string]string{
			"run_id":           runID,
			"task_id":          options.TaskID,
			"agent_kind":       options.AgentKind,
			"session_id":       options.SessionID,
			"review_thread_id": options.ReviewThreadID,
			"root_agent_name":  options.RootAgentName,
			"phase":            "finished",
			"status":           "error",
		}})
		emit(Event{Type: "error", Data: map[string]string{"message": fmt.Sprintf("生成结果持久化失败: %v", persistErr)}})
		return
	}
	if resumeInterruption != nil {
		if err := conversation.ResolveInterruption(resumeInterruption.ID); err != nil {
			runLogger.Error("resolve_interruption_failed", slog.String("interruption_id", resumeInterruption.ID), slog.Any("error", err))
		}
	}
	observedMutations := mutations.Mutations()
	observer.RecordMutations(observedMutations)
	verification := VerifyPostRunMutations(bookService, observedMutations)
	observer.RecordVerification(verification)
	if options.OnMutationsVerified != nil && len(observedMutations) > 0 {
		options.OnMutationsVerified(ctx, observedMutations, verification)
	}
	if verification.Mutations > 0 {
		runLogger.Info("post_run_verification", slog.String("status", verification.Status), slog.Int("mutations", verification.Mutations), slog.Int("checks", len(verification.Checks)), slog.Any("warnings", verification.Warnings))
		emit(Event{Type: "post_run_verification", Data: verification})
		emit(Event{Type: "verification", Data: verification})
	}
	runLogger.Info("run_completed")
	// Background memory-note writer (Plan.md M7): capture this turn's incremental
	// source so the next compaction can reuse structured notes instead of
	// re-summarizing raw turns. Best-effort and non-blocking.
	writeMemoryNoteIfNeeded(traceCtx, conversation, conversationConfig(conversation), options.AgentKind)
	preheatCompactionIfNeeded(traceCtx, conversation, conversationConfig(conversation), options.AgentKind)
	finishRun("success", "", generatedBytes)
	emit(Event{Type: "run_state", Data: map[string]string{
		"run_id":           runID,
		"task_id":          options.TaskID,
		"agent_kind":       options.AgentKind,
		"session_id":       options.SessionID,
		"review_thread_id": options.ReviewThreadID,
		"root_agent_name":  options.RootAgentName,
		"phase":            "finished",
		"status":           "success",
	}})
	emit(Event{Type: "done", Data: map[string]string{}})
}

func interactiveTurnCompletedByCancel(err error, agentKind string, conversation Conversation, generatedBytes int) bool {
	if err == nil || agentKind != AgentKindInteractiveStory || generatedBytes == 0 {
		return false
	}
	reporter, ok := conversation.(InteractiveNarrativeReadinessReporter)
	if !ok || !reporter.InteractiveNarrativeReady() {
		return false
	}
	var cancelErr *adk.CancelError
	return errors.As(err, &cancelErr) && cancelErr.Info != nil && cancelErr.Info.Mode&adk.CancelAfterToolCalls != 0
}

func runtimeContextProjectionReserves(conversation Conversation, fallbackAgentKind string) (completionReserve, toolReserve int) {
	sc, ok := conversation.(*SessionConversation)
	if !ok || sc == nil || sc.cfg == nil {
		return EstimateContextProjectionReserves(nil, fallbackAgentKind, 0)
	}
	agentKind := conversationAgentKind(sc, fallbackAgentKind)
	completionReserve, toolReserve = EstimateContextProjectionReserves(sc.cfg, agentKind, 0)
	if agentHasAnyToolEnabled(sc.cfg, agentKind) {
		if toolReserve < toolModeMinReserveTokens {
			toolReserve = toolModeMinReserveTokens
		}
	}
	return completionReserve, toolReserve
}

// conversationConfig returns the config bound to a session conversation, or nil
// when the conversation is not a session-backed one.
func conversationConfig(conversation Conversation) *config.Config {
	sc, ok := conversation.(*SessionConversation)
	if !ok || sc == nil {
		return nil
	}
	return sc.cfg
}

func conversationAgentKind(sc *SessionConversation, fallback string) string {
	if sc == nil {
		return strings.TrimSpace(fallback)
	}
	agentKind := strings.TrimSpace(sc.agentKind)
	if agentKind != "" {
		return agentKind
	}
	return strings.TrimSpace(fallback)
}

// recordRuntimeStateIfNeeded persists the agent's runtime context (agent kind,
// working mode, active story/branch, last read file, bounded settings) so a
// later resume inherits the exact "work site" it left behind (Plan.md M11).
// It is best-effort: only durable conversations that implement RuntimeState
// recorder are affected, and any persistence error is logged but never fails
// the run.
func recordRuntimeStateIfNeeded(conversation Conversation, options RunOptions) {
	if conversation == nil {
		return
	}
	recorder, ok := conversation.(RuntimeStateRecorder)
	if !ok || recorder == nil {
		return
	}
	state := conversation.RuntimeState()
	if state.AgentKind == "" && state.Mode == "" && state.StoryID == "" && state.BranchID == "" && state.LastRead == "" && len(state.Settings) == 0 {
		return
	}
	state.AgentKind = strings.TrimSpace(state.AgentKind)
	state.Mode = strings.TrimSpace(state.Mode)
	state.StoryID = strings.TrimSpace(state.StoryID)
	state.BranchID = strings.TrimSpace(state.BranchID)
	state.LastRead = strings.TrimSpace(state.LastRead)
	if err := recorder.RecordRuntimeState(state); err != nil {
		log.Printf("[agent-run] record runtime state failed run_id=%s agent_kind=%s mode=%s story_id=%s branch_id=%s last_read=%s error=%v", options.TaskID, state.AgentKind, state.Mode, state.StoryID, state.BranchID, state.LastRead, err)
	}
}

// writeMemoryNoteIfNeeded writes one bounded background memory note after a run
// (Plan.md M7). It is best-effort and runs in the background so it never blocks
// the response; any error is logged but never fails the run. The note follows
// the source agent's model and is only written when memory notes are enabled for
// that agent kind.
func writeMemoryNoteIfNeeded(ctx context.Context, conversation Conversation, cfg *config.Config, agentKind string) {
	if conversation == nil || cfg == nil {
		return
	}
	settings := config.ResolveAgentContext(cfg, agentKind)
	if !settings.MemoryNotesEnabled {
		return
	}
	sc, ok := conversation.(*SessionConversation)
	if !ok || sc == nil {
		return
	}
	session_ := sc.Session()
	if session_ == nil {
		return
	}
	existing := session_.LatestMemoryNotes(agentKind, 0)
	source := sc.memoryNoteSource()
	if len(source) == 0 && len(existing) == 0 {
		return
	}
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("[agent-run] memory note writer recovered panic agent_kind=%s error=%v", agentKind, recovered)
			}
		}()
		note, err := generateMemoryNote(ctx, cfg, agentKind, existing, source)
		if err != nil {
			log.Printf("[agent-run] memory note generation failed agent_kind=%s error=%v", agentKind, err)
			return
		}
		if strings.TrimSpace(note.Goals) == "" && strings.TrimSpace(note.Progress) == "" && strings.TrimSpace(note.Conclusions) == "" {
			return
		}
		if _, err := session_.AppendMemoryNote(note); err != nil {
			log.Printf("[agent-run] memory note persist failed agent_kind=%s error=%v", agentKind, err)
		} else {
			log.Printf("[agent-run] memory_note_written agent_kind=%s note_id=%s title=%s", agentKind, note.ID, note.Title)
		}
	}()
}

func agentHasAnyToolEnabled(cfg *config.Config, agentKind string) bool {
	settings := config.ResolveAgentTools(cfg, agentKind)
	return settings.FileRead || settings.FileWrite || settings.ShellExecute || settings.Skills || settings.LoreRead || settings.LoreWrite || settings.Todo || settings.WebSearch || settings.ImageGeneration || settings.AgentConfigRead || settings.AgentConfigWrite
}

func enforceToolSafeBudgetBeforeRun(ctx context.Context, conversation Conversation, options RunOptions, runLogger *slog.Logger, history []*schema.Message, agentMessage string, emit func(Event)) ([]*schema.Message, bool) {
	sc, ok := conversation.(*SessionConversation)
	if !ok || sc == nil || sc.cfg == nil || !agentHasAnyToolEnabled(sc.cfg, options.AgentKind) {
		return history, false
	}
	compactor, ok := conversation.(ContextCompactionConversation)
	if !ok || compactor == nil {
		return history, false
	}
	agentKind := conversationAgentKind(sc, options.AgentKind)
	model := config.ResolveAgentModel(sc.cfg, agentKind)
	if model.ContextWindowTokens <= 0 {
		return history, false
	}
	// T1: Use the actual context window (from /v1/models probe or server
	// rejection calibration) when it is smaller than the declared window, so
	// the tool budget guard triggers against the real server limit.
	declaredWindow := model.ContextWindowTokens
	actualWindow := knownActualContextWindow(model.OpenAIBaseURL)
	effectiveWindow := clampToolWindowTokens(effectiveContextWindow(declaredWindow, actualWindow))
	// Only apply the extra tool budget guard when the selected model explicitly
	// configures one. Otherwise the model's resolved context window is the
	// source of truth; applying the local 81920-token heuristic here would make
	// large-context providers such as DeepSeek compact prematurely.
	if model.ToolSafeWindowTokens <= 0 {
		return history, false
	}
	// T1: The physical window is the minimum of the configured tool-safe
	// window and the actual server window, so a server with a smaller real
	// context limit is not over-trusted.
	physicalWindow := model.ToolSafeWindowTokens
	if actualWindow > 0 && actualWindow < physicalWindow {
		physicalWindow = actualWindow
	}
	completionReserve, toolReserve := runtimeContextProjectionReserves(conversation, options.AgentKind)
	// 动态输出预留：小窗口模型上 toolReserve 收缩到窗口 15% 以内，避免固定 8000
	// 把 projected 虚高（既可能误触发压缩，也可能挤占本就有限的输入空间）。
	toolReserve = min(toolReserve, int(float64(physicalWindow)*0.15))
	projected := EstimateContextTokens(history, nil) + max(0, completionReserve) + max(0, toolReserve)
	safeLimit := int(float64(physicalWindow) * toolModeSafeWindowRatio)
	if projected <= safeLimit {
		recordBudgetSnapshot(ctx, projected, physicalWindow)
		runLogger.Info("tool_safe_budget_guard_skipped", slog.Int("projected_tokens", projected), slog.Int("safe_limit", safeLimit), slog.Int("context_window_tokens", model.ContextWindowTokens), slog.Int("effective_window_tokens", effectiveWindow), slog.Int("physical_window_tokens", physicalWindow), slog.Int("reserve_completion_tokens", completionReserve), slog.Int("reserve_tool_tokens", toolReserve), slog.String("agent_kind", agentKind))
		return history, false
	}
	runLogger.Warn("tool_safe_budget_guard", slog.Int("projected_tokens", projected), slog.Int("safe_limit", safeLimit), slog.Int("context_window_tokens", model.ContextWindowTokens), slog.Int("effective_window_tokens", effectiveWindow), slog.Int("physical_window_tokens", physicalWindow), slog.String("agent_kind", agentKind), slog.Int("reserve_completion_tokens", completionReserve), slog.Int("reserve_tool_tokens", toolReserve))
	compactedHistory, result, err := compactor.CompactContextIfNeeded(ContextWithRunObserver(ctx, RunObserverFromContext(ctx)), ContextCompactionInput{
		Messages:                 history,
		AgentMessage:             agentMessage,
		Phase:                    contextCompactionPhasePreRun,
		Emit:                     emit,
		Force:                    true,
		KeepLatestUser:           true,
		ContextWindowTokens:      effectiveWindow,
		ReservedCompletionTokens: completionReserve,
		ReservedToolResultTokens: toolReserve,
	})
	if err != nil {
		recordBudgetSnapshot(ctx, projected, physicalWindow)
		runLogger.Warn("tool_safe_budget_guard_failed", slog.Any("error", err))
		return history, false
	}
	compactedProjected := EstimateContextTokens(compactedHistory, nil) + max(0, completionReserve) + max(0, toolReserve)
	recordBudgetSnapshot(ctx, compactedProjected, physicalWindow)
	runLogger.Info("tool_safe_budget_guard_compacted", slog.Bool("triggered", result.Triggered), slog.Int("tokens_before", result.TokensBefore), slog.Int("projected_before", result.ProjectedTokensBefore), slog.Int("tokens_after", result.TokensAfter), slog.Int("projected_after", result.ProjectedTokensAfter))
	return compactedHistory, true
}

// recordBudgetSnapshot 把一次模型请求前的预算快照写入 RunObserver，供同一 run 内
// 的工具准入控制（middleware）在执行工具前评估"本次注入会不会超工具安全窗口"。
func recordBudgetSnapshot(ctx context.Context, projectedTokens, toolSafeWindow int) {
	if obs := RunObserverFromContext(ctx); obs != nil {
		obs.RecordBudgetSnapshot(projectedTokens, toolSafeWindow)
	}
}

// clampToolWindowTokens 将上下文窗口钳制到工具模式硬上限以内。
// 即便外部 provider 声明更大窗口（例如 256k），工具安全策略仍按
// 81920 预算执行，避免"放大窗口绕过内部安全策略"导致工具参数 JSON
// 在长上下文下被截断。返回 0 表示窗口不可用，调用方按"无窗口"处理。
func clampToolWindowTokens(window int) int {
	if window <= 0 {
		return 0
	}
	if window > toolModeHardWindowCapTokens {
		window = toolModeHardWindowCapTokens
	}
	return window
}

// physicalToolSafetyWindow 返回工具模式用于 pre-run 预算触发的物理安全窗口：
// 在 clampToolWindowTokens 的钳制窗口上再乘 toolModePhysicalSafetyRatio，避免
// 信任宣称窗口（可能远超硬件显存/内存能承受的上下文）。返回 0 表示窗口不可用。
func physicalToolSafetyWindow(window int) int {
	clamped := clampToolWindowTokens(window)
	if clamped <= 0 {
		return 0
	}
	scaled := int(float64(clamped) * toolModePhysicalSafetyRatio)
	if scaled < 1 {
		return 1
	}
	return scaled
}

// resolvedToolSafeWindow 返回显式配置的工具安全窗口。未配置时返回 0，
// 表示应由模型自身的 context_window_tokens 和普通压缩阈值负责判定。
func resolvedToolSafeWindow(model config.ResolvedModelSettings) int {
	if model.ToolSafeWindowTokens > 0 {
		return model.ToolSafeWindowTokens
	}
	return 0
}

func isToolArgumentsParseError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	if strings.Contains(text, "failed to parse tool call arguments as json") {
		return true
	}
	return strings.Contains(text, "parse error") && strings.Contains(text, "expected '}'")
}

// isModelMemoryError 识别模型服务端的内存不足错误。本地模型在长上下文下会因
// KV + 长 prefill/解码缓冲内存不足而抛出 bad allocation / out of memory。命中后
// 与工具参数截断走同一恢复路径：强制压缩上下文后重试一次。
func isModelMemoryError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	for _, marker := range []string{
		"bad allocation",
		"bad_alloc",
		"out of memory",
		"allocation failed",
		"resource exhausted",
		"not enough memory",
		"memory error",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// isServerRejectionError 识别服务端因上下文超长而拒绝请求。客户端估算 token
// 靠近似，总有估不准的时候；当服务端明确返回上下文超限错误时，走与强制压缩
// 相同的恢复路径：压缩历史后重试一次。这是 Plan.md §3.5「响应式压缩」的触发
// 场景（区别于工具参数解析错误/内存错误的被动兜底）。
func isServerRejectionError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(err.Error()))
	if text == "" {
		return false
	}
	for _, marker := range []string{
		"context length exceeded",
		"context_length_exceeded",
		"context length",
		"context too long",
		"maximum context length",
		"too many tokens",
		"prompt too long",
		"exceeds maximum context",
		"max context length",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
