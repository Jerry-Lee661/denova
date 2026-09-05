package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"

	"denova/config"
)

// 模型自主压缩工具集（借鉴 ACP / billion-context，Plan.md §12 / task-contract T1）。
//
// 两个工具让模型在长对话中主动管理上下文，与系统阈值触发（M6 全量压缩 / M9 响应式）
// 叠加而非互斥：模型调用 compress 后，系统兜底仍保留。
//
//   - compress：把指定原始消息区间折成摘要占位符（fold，默认，保留原始历史可重投影）
//     或写新压缩边界（compact，等同 M6）。模型指定范围，缺省摘要由压缩内核生成。
//   - acp_context：低频上下文操作的聚合 facade，用 op 区分：
//     search_context（检索已被压缩/折叠的历史消息块，与 grep 的区别：grep 搜工作区文件，
//     search_context 搜会话历史中被摘要取代的原文）与 acp_status（报告当前上下文占比、
//     压缩/折叠块数与最近压缩记录）；op=help 返回各 op 的参数 schema。
//
// 工具通过 context 中的 compaction controller 拿到当前会话（SessionConversation），
// 因此只在 agent run 期间可用；索引语义为「原始会话消息」的 1-based 闭区间 [start, end]。

const (
	contextToolNameCompress       = "compress"
	contextToolNameSearchContext  = "search_context"
	contextToolNameAcpStatus      = "acp_status"
	contextToolNameAcpContext     = "acp_context"
	contextToolModeFold           = "fold"
	contextToolModeCompact        = "compact"
	contextToolSearchMaxHits      = 8
	contextToolSearchSnippetChars = 240
	contextToolPreviewMessages    = 12
)

var contextToolDescriptionCompress = strings.TrimSpace(`Compress a range of conversation history into a summary placeholder to free context space.

- start / end: 1-based inclusive indices of RAW session messages. end >= start.
- summary: optional; if empty, the compaction kernel generates one.
- mode: "fold" (default) keeps raw history on disk (re-projectable); "compact" writes a new compaction boundary (heavier).
- Prefer small, tail-biased ranges (older, verbose segments) so the stable recent prefix stays intact for KV-cache prefix caching.
- Priority: subagent review output > verbose command output > dead-end exploration > redundant tool results > intermediate steps > resolved threads > large files already used.

压缩一段对话历史为摘要占位符，释放上下文空间。

- start / end：原始会话消息的 1-based 闭区间索引。end >= start。
- summary：可选；为空时由压缩内核自动生成摘要。
- mode："fold"（默认）保留磁盘原始历史（可重新投影）；"compact" 写新压缩边界（更重）。
- 优先选择更小、偏尾部（更早、更冗长）的区间，保持稳定近期前缀以命中 KV cache 前缀缓存。
- 压缩优先级：子代理审查输出 > 冗长命令输出 > 死路探索 > 冗余工具结果 > 中间步骤 > 已解决讨论线程 > 已使用的大文件。`)

var contextToolDescriptionAcpContext = strings.TrimSpace(`Aggregate low-frequency context-management operations behind one tool, selected by op.

- op=search_context: search the RAW conversation history, including ranges already compressed/folded into summaries. args: {query: substring or keyword, case-insensitive}. Returns up to 8 hits: raw message index, role, and a bounded snippet. (grep searches workspace FILES; this searches the conversation transcript.)
- op=acp_status: report current context tokens vs window, usage ratio, active compaction/fold blocks, recent compaction/fold records, and a preview of the last raw messages with 1-based indices (useful for choosing compress ranges). args: {} (empty object).
- op=help: return the argument schema of every op.

低频上下文管理操作的聚合工具，用 op 区分。

- op=search_context：检索原始对话历史，包括已被压缩/折叠成摘要的区间。args：{query: 子串或关键词，大小写不敏感}。最多返回 8 条命中：原始消息索引、角色、命中点附近的有界片段。（grep 搜工作区文件；本操作搜会话历史。）
- op=acp_status：报告当前上下文 token 数与窗口、占比、激活的压缩/折叠块数、最近压缩/折叠记录，以及最近若干条原始消息的 1-based 索引预览（便于选择 compress 区间）。args：{}（空对象）。
- op=help：返回各 op 的参数 schema。`)

type contextToolCompressInput struct {
	Start   int    `json:"start" jsonschema:"description=1-based index of the first raw session message to compress (inclusive)."`
	End     int    `json:"end" jsonschema:"description=1-based index of the last raw session message to compress (inclusive)."`
	Summary string `json:"summary,omitempty" jsonschema:"description=Optional summary for the range. If empty, the compaction kernel generates one."`
	Mode    string `json:"mode,omitempty" jsonschema:"description=Compress mode: fold (default, keeps raw history) or compact (new compaction boundary)."`
}

type contextToolCompressResult struct {
	Schema        string `json:"schema"`
	Mode          string `json:"mode"`
	Start         int    `json:"start"`
	End           int    `json:"end"`
	Triggered     bool   `json:"triggered"`
	SkippedReason string `json:"skipped_reason,omitempty"`
	Summary       string `json:"summary,omitempty"`
	TokensBefore  int    `json:"tokens_before"`
	TokensAfter   int    `json:"tokens_after"`
	Note          string `json:"note,omitempty"`
}

type contextToolSearchInput struct {
	Query string `json:"query" jsonschema:"description=Substring or keyword to search for in raw conversation history (case-insensitive)."`
}

type contextToolAcpContextInput struct {
	Op   string          `json:"op" jsonschema:"description=Operation to run: search_context, acp_status, or help."`
	Args json.RawMessage `json:"args,omitempty" jsonschema:"description=Operation-specific arguments (object). Run op=help to see the schema of each op."`
}

type contextToolSearchHit struct {
	Index   int    `json:"index"`
	Role    string `json:"role"`
	Snippet string `json:"snippet"`
}

type contextToolSearchResult struct {
	Schema string                 `json:"schema"`
	Query  string                 `json:"query"`
	Hits   []contextToolSearchHit `json:"hits"`
	Total  int                    `json:"total"`
}

type contextToolStatusResult struct {
	Schema              string                     `json:"schema"`
	EstimatedTokens     int                        `json:"estimated_tokens"`
	ContextWindowTokens int                        `json:"context_window_tokens"`
	UsageRatio          float64                    `json:"usage_ratio"`
	RawMessageCount     int                        `json:"raw_message_count"`
	CompactionActive    bool                       `json:"compaction_active"`
	FoldActive          bool                       `json:"fold_active"`
	RecentCompactions   []contextToolStatusRecord  `json:"recent_compactions,omitempty"`
	RecentFolds         []contextToolStatusRecord  `json:"recent_folds,omitempty"`
	RecentMessages      []contextToolStatusMessage `json:"recent_messages,omitempty"`
}

type contextToolStatusRecord struct {
	IndexRange   string `json:"index_range"`
	TokensBefore int    `json:"tokens_before"`
	TokensAfter  int    `json:"tokens_after"`
	CreatedAt    string `json:"created_at"`
}

type contextToolStatusMessage struct {
	Index   int    `json:"index"`
	Role    string `json:"role"`
	Preview string `json:"preview"`
}

// ContextToolConversation 是模型自主压缩工具需要的会话能力。
// SessionConversation 实现它；interactive 会话后续可按需实现。
type ContextToolConversation interface {
	CompressContextRange(ctx context.Context, input ContextToolCompressInput) (ContextToolCompressResult, error)
	SearchContextHistory(query string) []contextToolSearchHit
	ContextToolStatus(tools []*schema.ToolInfo) contextToolStatusResult
}

// ContextToolCompressInput 是 compress 工具对会话的调用入参（原始消息 1-based 闭区间）。
type ContextToolCompressInput struct {
	Start   int
	End     int
	Summary string
	Mode    string
}

// ContextToolCompressResult 是 compress 工具对会话的调用结果。
type ContextToolCompressResult struct {
	Triggered     bool
	SkippedReason string
	Summary       string
	TokensBefore  int
	TokensAfter   int
	Note          string
}

// NewContextTools 构建模型自主压缩工具集（compress / acp_context）。
// 工具在调用时从 context 取当前会话；若会话未实现 ContextToolConversation 则返回说明性错误。
func NewContextTools(cfg *config.Config, agentKind string) []tool.BaseTool {
	tools := make([]tool.BaseTool, 0, 2)
	if t, err := newContextToolCompress(cfg, agentKind); err == nil {
		tools = append(tools, t)
	}
	if t, err := newContextToolAcpContext(); err == nil {
		tools = append(tools, t)
	}
	return tools
}

func contextToolConversationFromCtx(ctx context.Context) (ContextToolConversation, error) {
	controller := compactionControllerFromContext(ctx)
	if controller == nil || controller.conversation == nil {
		return nil, fmt.Errorf("no active conversation in context（当前 context 中没有活动会话）")
	}
	conv, ok := controller.conversation.(ContextToolConversation)
	if !ok || conv == nil {
		return nil, fmt.Errorf("conversation does not implement context tools（会话未实现上下文工具接口）")
	}
	return conv, nil
}

func newContextToolCompress(cfg *config.Config, agentKind string) (tool.BaseTool, error) {
	return utils.InferTool(contextToolNameCompress, contextToolDescriptionCompress, func(ctx context.Context, input contextToolCompressInput) (string, error) {
		conv, err := contextToolConversationFromCtx(ctx)
		if err != nil {
			return "", err
		}
		mode := strings.TrimSpace(input.Mode)
		if mode == "" {
			mode = contextToolModeFold
		}
		if mode != contextToolModeFold && mode != contextToolModeCompact {
			return "", fmt.Errorf("invalid mode %q (want fold or compact)", input.Mode)
		}
		result, err := conv.CompressContextRange(ctx, ContextToolCompressInput{
			Start:   input.Start,
			End:     input.End,
			Summary: strings.TrimSpace(input.Summary),
			Mode:    mode,
		})
		if err != nil {
			return "", err
		}
		out := contextToolCompressResult{
			Schema:        "compress.v1",
			Mode:          mode,
			Start:         input.Start,
			End:           input.End,
			Triggered:     result.Triggered,
			SkippedReason: result.SkippedReason,
			Summary:       result.Summary,
			TokensBefore:  result.TokensBefore,
			TokensAfter:   result.TokensAfter,
			Note:          result.Note,
		}
		data, err := json.Marshal(out)
		if err != nil {
			return "", fmt.Errorf("serialize compress result: %w", err)
		}
		slog.Info("context_tool_compress",
			slog.String("agent_kind", agentKind),
			slog.Int("start", input.Start),
			slog.Int("end", input.End),
			slog.String("mode", mode),
			slog.Bool("triggered", result.Triggered),
			slog.String("skipped_reason", result.SkippedReason),
		)
		return string(data), nil
	})
}

// newContextToolAcpContext 构建 acp_context facade 工具：用 op 区分低频上下文操作
// （search_context / acp_status），op=help 返回各 op 的参数 schema。
func newContextToolAcpContext() (tool.BaseTool, error) {
	return utils.InferTool(contextToolNameAcpContext, contextToolDescriptionAcpContext, func(ctx context.Context, input contextToolAcpContextInput) (string, error) {
		op := strings.TrimSpace(input.Op)
		switch op {
		case contextToolNameSearchContext:
			var searchInput contextToolSearchInput
			if err := json.Unmarshal(input.Args, &searchInput); err != nil {
				return "", fmt.Errorf("parse search_context args: %w", err)
			}
			query := strings.TrimSpace(searchInput.Query)
			if query == "" {
				return "", fmt.Errorf("query is required")
			}
			conv, err := contextToolConversationFromCtx(ctx)
			if err != nil {
				return "", err
			}
			hits := conv.SearchContextHistory(query)
			out := contextToolSearchResult{
				Schema: "search_context.v1",
				Query:  query,
				Hits:   hits,
				Total:  len(hits),
			}
			data, err := json.Marshal(out)
			if err != nil {
				return "", fmt.Errorf("serialize search_context result: %w", err)
			}
			return string(data), nil
		case contextToolNameAcpStatus:
			conv, err := contextToolConversationFromCtx(ctx)
			if err != nil {
				return "", err
			}
			out := conv.ContextToolStatus(nil)
			data, err := json.Marshal(out)
			if err != nil {
				return "", fmt.Errorf("serialize acp_status result: %w", err)
			}
			return string(data), nil
		case "help":
			help := map[string]any{
				"ops": map[string]any{
					contextToolNameSearchContext: map[string]any{
						"args": map[string]any{
							"query": "substring or keyword to search for in raw conversation history (case-insensitive)",
						},
					},
					contextToolNameAcpStatus: map[string]any{
						"args": map[string]any{},
					},
				},
			}
			data, err := json.Marshal(help)
			if err != nil {
				return "", fmt.Errorf("serialize acp_context help: %w", err)
			}
			return string(data), nil
		default:
			return "", fmt.Errorf("unknown op %q (want search_context, acp_status, or help)", op)
		}
	})
}
