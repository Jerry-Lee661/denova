package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/cloudwego/eino/schema"

	"denova/config"
)

// 上下文折叠（M5）：把一整段旧消息折成一条摘要占位符，只改接口视图、不另起新历史、
// 也不真删原始记录——磁盘只追加一条折叠提交记录，恢复时按记录重投影。
//
// 与全量压缩（M6）语义不同：全量压缩会写下新边界、下一轮从新边界开始截取；
// 折叠只把选定区间替换成摘要占位符，原始消息仍在磁盘上，随时可重新投影回全文。
// 两者管理同一份“旧历史投影”，因此互斥——投影时优先使用激活的全量压缩，
// 若全量压缩未激活而折叠激活，则使用折叠投影。
//
// 折叠区间 [foldStart, foldEnd) 内的消息被摘要取代，但保留尾部 retainedTurns 条
// 用户消息作为“可见残留”，让模型仍能看见最近几轮原文；区间之后的消息原样保留。

const contextFoldSummaryPrefix = "[Denova Context Fold]"

// ContextFoldResult 描述一次折叠的结果。
type ContextFoldResult struct {
	Triggered          bool
	SkippedReason      string
	Phase              string
	SourceMessageCount int
	MessageCountBefore int
	MessageCountAfter  int
	TokensBefore       int
	TokensAfter        int
	RetainedTurns      int
	Summary            string
}

// FoldContextInput 是折叠操作的输入，与全量压缩输入结构对齐以便复用摘要逻辑。
type FoldContextInput struct {
	// Messages 是当前接口视图下的完整消息列表，折叠区间相对它定义。
	Messages []*schema.Message
	Tools    []*schema.ToolInfo
	Phase    string
	Emit     func(Event)
	Force    bool
	// ReferenceContext 是注入摘要请求的有界业务上下文（如资料摘要）。
	ReferenceContext string
	// FoldStart / FoldEnd 是要折叠的区间，相对 Messages 索引。
	FoldStart int
	FoldEnd   int
	// RetainedTurns 折叠后保留在占位符后的尾部用户轮数；0 时回退到配置默认值。
	RetainedTurns int
}

// FoldContext 把 input.Messages 的 [FoldStart, FoldEnd) 区间折成摘要占位符，
// 返回新的接口视图消息列表。它不修改磁盘、不另起逻辑链，只生成投影。
func FoldContext(ctx context.Context, cfg *config.Config, agentKind string, input FoldContextInput) ([]*schema.Message, ContextFoldResult, error) {
	retainedTurns := input.RetainedTurns
	if retainedTurns <= 0 {
		retainedTurns = config.DefaultContextCompactionRetainedTurns
	}
	if retainedTurns > config.MaxContextCompactionRetainedTurns {
		retainedTurns = config.MaxContextCompactionRetainedTurns
	}
	result := ContextFoldResult{
		Phase:              strings.TrimSpace(input.Phase),
		MessageCountBefore: len(input.Messages),
		RetainedTurns:      retainedTurns,
	}
	if result.Phase == "" {
		result.Phase = contextCompactionPhasePreRun
	}
	foldStart := input.FoldStart
	if foldStart < 0 {
		foldStart = 0
	}
	foldEnd := input.FoldEnd
	if foldEnd > len(input.Messages) {
		foldEnd = len(input.Messages)
	}
	if foldStart > foldEnd {
		foldStart = foldEnd
	}
	source := compactionContextMessages(input.Messages[foldStart:foldEnd])
	if len(source) == 0 && strings.TrimSpace(input.ReferenceContext) == "" {
		result.SkippedReason = "empty_source"
		return input.Messages, result, nil
	}
	sourceTokens := EstimateContextTokens(source, nil)
	emitContextFoldEvent(input.Emit, result.Phase, "started", result)
	summary, _, err := summarizeContextForCompaction(ctx, cfg, agentKind, "", source, input.ReferenceContext, sourceTokens, resolveContextCompactionPolicy(cfg, agentKind), func(_ int, delta string) {
		if input.Emit != nil {
			input.Emit(Event{Type: "context_fold", Data: map[string]any{
				"phase":   result.Phase,
				"status":  "delta",
				"delta":   delta,
				"summary": delta,
			}})
		}
	})
	if err != nil {
		emitContextFoldEvent(input.Emit, result.Phase, "failed", result)
		return input.Messages, result, err
	}
	newMessages := foldMessagesForModel(input.Messages, summary, foldStart, foldEnd, retainedTurns)
	result.Triggered = true
	result.Summary = strings.TrimSpace(summary)
	result.SourceMessageCount = len(source)
	result.TokensBefore = sourceTokens
	result.TokensAfter = EstimateContextTokens(newMessages, input.Tools)
	result.MessageCountAfter = len(newMessages)
	emitContextFoldEvent(input.Emit, result.Phase, "completed", result)
	return newMessages, result, nil
}

// foldMessagesForModel 把 messages 的 [foldStart, foldEnd) 区间替换成摘要占位符，
// 区间之前的稳定前缀原样保留，尾部保留 retainedTurns 条用户消息原文，
// 区间之后原样保留。折叠只改接口视图、不改变磁盘上的原始顺序。
func foldMessagesForModel(messages []*schema.Message, summary string, foldStart, foldEnd, retainedTurns int) []*schema.Message {
	if foldEnd < 0 {
		foldEnd = 0
	}
	if foldEnd > len(messages) {
		foldEnd = len(messages)
	}
	if foldStart < 0 {
		foldStart = 0
	}
	if foldStart > foldEnd {
		foldStart = foldEnd
	}
	// before：折叠区间之前的稳定前缀，逐字节不变（已进入 KV cache 前缀）。
	before := make([]*schema.Message, 0, foldStart)
	for i := 0; i < foldStart; i++ {
		if messages[i] != nil {
			before = append(before, messages[i])
		}
	}
	source := compactionContextMessages(messages[foldStart:foldEnd])
	sourceTail := retainTailByUserTurns(source, retainedTurns)
	appended := compactionContextMessages(messages[foldEnd:])
	result := make([]*schema.Message, 0, len(before)+len(sourceTail)+1+len(appended))
	result = append(result, before...)
	result = append(result, NewContextFoldSummaryMessage(summary))
	result = append(result, sourceTail...)
	result = append(result, appended...)
	return result
}

// FoldContextResultTokens 估算折叠后接口视图的 token 数（供调用方做预算判断）。
func FoldContextResultTokens(messages []*schema.Message, tools []*schema.ToolInfo) int {
	return EstimateContextTokens(messages, tools)
}

// NewContextFoldSummaryMessage 创建一条折叠摘要占位符消息。
func NewContextFoldSummaryMessage(summary string) *schema.Message {
	return schema.UserMessage(fmt.Sprintf("%s\n\n%s", contextFoldSummaryPrefix, strings.TrimSpace(summary)))
}

// isContextFoldMessage 判断 msg 是否为 Denova 折叠流水线生成的摘要占位符。
func isContextFoldMessage(msg *schema.Message) bool {
	return msg != nil && strings.HasPrefix(strings.TrimSpace(msg.Content), contextFoldSummaryPrefix)
}

// IsContextFoldSummaryMessage 导出版本，供 app 层识别折叠投影。
func IsContextFoldSummaryMessage(msg *schema.Message) bool {
	return isContextFoldMessage(msg)
}

func emitContextFoldEvent(emit func(Event), phase, status string, result ContextFoldResult) {
	if emit == nil {
		return
	}
	emit(Event{Type: "context_fold", Data: map[string]any{
		"phase":                phase,
		"status":               status,
		"tokens_before":        result.TokensBefore,
		"tokens_after":         result.TokensAfter,
		"source_message_count": result.SourceMessageCount,
		"message_count_before": result.MessageCountBefore,
		"message_count_after":  result.MessageCountAfter,
		"retained_turns":       result.RetainedTurns,
		"skipped_reason":       result.SkippedReason,
		"summary":              result.Summary,
		"created_at":           time.Now().UTC().Format(time.RFC3339Nano),
	}})
}
