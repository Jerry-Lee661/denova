package session

import (
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
)

const (
	defaultSessionID             = "default"
	defaultSessionTitle          = "新会话"
	displayToolArgsPersistBytes  = 4 * 1024
	maxTokenUsageDisplayEvents   = 10
	historyTypeMessage           = "message"
	historyTypeContextMessage    = "context_message"
	historyTypeDisplay           = "display"
	historyTypeClear             = "clear"
	historyTypeInterrupt         = "interrupt"
	historyTypeCompaction        = "context_compaction"
	historyTypeCompactionRemoved = "context_compaction_removed"
	historyTypeFold              = "context_fold"
	historyTypeFoldRemoved       = "context_fold_removed"
	historyTypeExternalize       = "externalized_result"
	historyTypeMemoryNote        = "memory_note"
	historyTypeRuntimeState      = "runtime_state"
	historyTypeCheckpoint        = "checkpoint"
	historyTypeTruncate          = "truncate"

	InterruptionPending  = "pending"
	InterruptionResolved = "resolved"
)

// HistoryEntry 表示用于前端展示的会话历史记录。
type HistoryEntry struct {
	Type         string               `json:"type"`
	ID           string               `json:"id,omitempty"`
	Role         string               `json:"role,omitempty"`
	Content      string               `json:"content,omitempty"`
	Name         string               `json:"name,omitempty"`
	Args         string               `json:"args,omitempty"`
	Status       string               `json:"status,omitempty"`
	Result       string               `json:"result,omitempty"`
	Illustration *ChapterIllustration `json:"illustration,omitempty"`
	Message      *schema.Message      `json:"-"`
	CreatedAt    time.Time            `json:"created_at,omitempty"`

	// MessageIndex 是该条目在原始消息数组（s.messages）中的索引，仅对
	// historyTypeMessage 条目有效（-1 表示非消息条目）。前端用它做截断/重试
	// 的 message_index 参数，避免用"全历史数组下标"（含展示记录）导致索引偏移。
	MessageIndex int `json:"message_index,omitempty"`

	ModelName string `json:"model_name,omitempty"`

	RunID                string                 `json:"run_id,omitempty"`
	AgentKind            string                 `json:"agent_kind,omitempty"`
	AgentName            string                 `json:"agent_name,omitempty"`
	RootAgentName        string                 `json:"root_agent_name,omitempty"`
	RunPath              []string               `json:"run_path,omitempty"`
	SubAgent             bool                   `json:"subagent,omitempty"`
	SubAgentSessionID    string                 `json:"subagent_session_id,omitempty"`
	SubAgentType         string                 `json:"subagent_type,omitempty"`
	PromptTokens         int                    `json:"prompt_tokens,omitempty"`
	CachedPromptTokens   int                    `json:"cached_prompt_tokens,omitempty"`
	UncachedPromptTokens int                    `json:"uncached_prompt_tokens,omitempty"`
	CacheHitRate         float64                `json:"cache_hit_rate,omitempty"`
	CompletionTokens     int                    `json:"completion_tokens,omitempty"`
	ReasoningTokens      int                    `json:"reasoning_tokens,omitempty"`
	TotalTokens          int                    `json:"total_tokens,omitempty"`
	ModelCalls           int                    `json:"model_calls,omitempty"`
	GeneratedBytes       int                    `json:"generated_bytes,omitempty"`
	UsageCalls           []TokenUsageCall       `json:"usage_calls,omitempty"`
	SSEHiddenFields      []string               `json:"sse_hidden_fields,omitempty"`
	SSEHiddenReason      string                 `json:"sse_hidden_reason,omitempty"`
	SSEDisplayNotice     string                 `json:"sse_display_notice,omitempty"`
	SSEGeneratedChars    int                    `json:"sse_generated_chars,omitempty"`
	UserReferences       []UserMessageReference `json:"user_references,omitempty"`
}

// UserMessageReference is display-only context attached to one durable user
// message. It is intentionally excluded from the model-visible message body.
type UserMessageReference struct {
	Kind      string `json:"kind"`
	ID        string `json:"id,omitempty"`
	Label     string `json:"label"`
	Detail    string `json:"detail,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

type MessageMetadata struct {
	RunID             string                 `json:"run_id,omitempty"`
	AgentKind         string                 `json:"agent_kind,omitempty"`
	AgentName         string                 `json:"agent_name,omitempty"`
	RootAgentName     string                 `json:"root_agent_name,omitempty"`
	RunPath           []string               `json:"run_path,omitempty"`
	SubAgent          bool                   `json:"subagent,omitempty"`
	SubAgentSessionID string                 `json:"subagent_session_id,omitempty"`
	SubAgentType      string                 `json:"subagent_type,omitempty"`
	ModelName         string                 `json:"model_name,omitempty"`
	UserReferences    []UserMessageReference `json:"user_references,omitempty"`
}

type historyRecord struct {
	kind              string
	message           *schema.Message
	messageMetadata   MessageMetadata
	display           *DisplayEvent
	interruption      *Interruption
	compaction        *ContextCompaction
	compactionRemoval *ContextCompactionRemoval
	fold              *ContextFold
	foldRemoved       *ContextFoldRemoved
	externalize       *ExternalizedResultEntry
	fork              *forkRecord
	runtimeState      *RuntimeState
	memoryNote        *MemoryNote
	checkpoint        *Checkpoint
	truncate          *Truncate
	createdAt         time.Time

	displayArgsPersistedBytes int

	// chain_link 记录字段：消息索引、父指针索引、分支标识。
	messageIndex int
	prevIndex    int
	branchID     string
}

type messageRecord struct {
	Type      string         `json:"type"`
	CreatedAt time.Time      `json:"created_at,omitempty"`
	Message   schema.Message `json:"message"`
	MessageMetadata
}

// DisplayEvent 表示只用于前端展示的非上下文事件，例如 thinking 和工具卡片。
type DisplayEvent struct {
	ID           string               `json:"id,omitempty"`
	Role         string               `json:"role"`
	Content      string               `json:"content,omitempty"`
	Name         string               `json:"name,omitempty"`
	Args         string               `json:"args,omitempty"`
	Status       string               `json:"status,omitempty"`
	Result       string               `json:"result,omitempty"`
	Illustration *ChapterIllustration `json:"illustration,omitempty"`
	CreatedAt    time.Time            `json:"created_at,omitempty"`

	RunID                string           `json:"run_id,omitempty"`
	AgentKind            string           `json:"agent_kind,omitempty"`
	AgentName            string           `json:"agent_name,omitempty"`
	RootAgentName        string           `json:"root_agent_name,omitempty"`
	RunPath              []string         `json:"run_path,omitempty"`
	SubAgent             bool             `json:"subagent,omitempty"`
	SubAgentSessionID    string           `json:"subagent_session_id,omitempty"`
	SubAgentType         string           `json:"subagent_type,omitempty"`
	ModelName            string           `json:"model_name,omitempty"`
	PromptTokens         int              `json:"prompt_tokens,omitempty"`
	CachedPromptTokens   int              `json:"cached_prompt_tokens,omitempty"`
	UncachedPromptTokens int              `json:"uncached_prompt_tokens,omitempty"`
	CacheHitRate         float64          `json:"cache_hit_rate,omitempty"`
	CompletionTokens     int              `json:"completion_tokens,omitempty"`
	ReasoningTokens      int              `json:"reasoning_tokens,omitempty"`
	TotalTokens          int              `json:"total_tokens,omitempty"`
	ModelCalls           int              `json:"model_calls,omitempty"`
	GeneratedBytes       int              `json:"generated_bytes,omitempty"`
	UsageCalls           []TokenUsageCall `json:"usage_calls,omitempty"`
	SSEHiddenFields      []string         `json:"sse_hidden_fields,omitempty"`
	SSEHiddenReason      string           `json:"sse_hidden_reason,omitempty"`
	SSEDisplayNotice     string           `json:"sse_display_notice,omitempty"`
	SSEGeneratedChars    int              `json:"sse_generated_chars,omitempty"`
}

type ChapterIllustration struct {
	Schema        string `json:"schema"`
	ChapterPath   string `json:"chapter_path"`
	ImagePath     string `json:"image_path"`
	MetaPath      string `json:"meta_path"`
	Markdown      string `json:"markdown"`
	AltText       string `json:"alt_text"`
	ProfileID     string `json:"profile_id"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Size          string `json:"size,omitempty"`
	Quality       string `json:"quality,omitempty"`
	OutputFormat  string `json:"output_format,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
	RevisedPrompt string `json:"revised_prompt,omitempty"`
	MIMEType      string `json:"mime_type,omitempty"`
	SizeBytes     int    `json:"size_bytes,omitempty"`
}

type TokenUsageCall struct {
	Index                int      `json:"index,omitempty"`
	CreatedAt            string   `json:"created_at,omitempty"`
	FinishReason         string   `json:"finish_reason,omitempty"`
	RequestedTools       []string `json:"requested_tools,omitempty"`
	AfterTools           []string `json:"after_tools,omitempty"`
	PromptTokens         int      `json:"prompt_tokens,omitempty"`
	CachedPromptTokens   int      `json:"cached_prompt_tokens,omitempty"`
	UncachedPromptTokens int      `json:"uncached_prompt_tokens,omitempty"`
	CacheHitRate         float64  `json:"cache_hit_rate,omitempty"`
	CompletionTokens     int      `json:"completion_tokens,omitempty"`
	ReasoningTokens      int      `json:"reasoning_tokens,omitempty"`
	TotalTokens          int      `json:"total_tokens,omitempty"`
}

// Interruption 表示一次异常中断后可恢复的对话轮次。
type Interruption struct {
	ID               string     `json:"id"`
	Status           string     `json:"status"`
	UserMessage      string     `json:"user_message"`
	AssistantContent string     `json:"assistant_content,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
}

// ContextCompaction records a model-visible summary epoch without modifying the
// raw user-facing transcript.
type ContextCompaction struct {
	Type                string  `json:"type"`
	ID                  string  `json:"id"`
	AgentKind           string  `json:"agent_kind,omitempty"`
	Epoch               int     `json:"epoch"`
	Summary             string  `json:"summary"`
	SourceStartIndex    int     `json:"source_start_index"`
	SourceEndIndex      int     `json:"source_end_index"`
	SourceMessageCount  int     `json:"source_message_count"`
	RetainedTurns       int     `json:"retained_turns"`
	TokensBefore        int     `json:"tokens_before"`
	TokensAfter         int     `json:"tokens_after"`
	TargetRatio         float64 `json:"target_ratio,omitempty"`
	ContextWindowTokens int     `json:"context_window_tokens"`
	Strategy            string  `json:"strategy,omitempty"`
	Threshold           float64 `json:"threshold"`
	Reason              string  `json:"reason,omitempty"`
	Phase               string  `json:"phase,omitempty"`
	// Tiered 是三层分层压缩的摘要池（Plan.md §12 / T2）。非 nil 时表示该压缩
	// 由分层内核产生；Summary 字段始终等于池的最高可用层摘要（投影恒定）。
	Tiered    *ContextTieredPool `json:"tiered,omitempty"`
	CreatedAt time.Time          `json:"created_at"`
}

// ContextTieredPool 是三层分层压缩的摘要池（Plan.md §12 / T2，借鉴 ACP / billion-context）。
//
//   - T1 capture：原始消息 → 详细摘要（~45×），原始池超 T1 阈值触发。
//   - T2 distill：T1 摘要 → 决策/结果（~10×），T1 池超 T2 阈值触发。
//   - T3 condense：T2 摘要 → 裸事实（~5×），T2 池超 T3 阈值触发。
//
// 每层独立触发阈值；高层蒸馏后低层 token 清零（被高层取代）。投影使用最高
// 可用层（T3 > T2 > T1），保证投影确定性、不每轮重新压缩（承接 H 修正）。
type ContextTieredPool struct {
	RawTokens int    `json:"raw_tokens"`
	T1Summary string `json:"t1_summary,omitempty"`
	T1Tokens  int    `json:"t1_tokens"`
	T2Summary string `json:"t2_summary,omitempty"`
	T2Tokens  int    `json:"t2_tokens"`
	T3Summary string `json:"t3_summary,omitempty"`
	T3Tokens  int    `json:"t3_tokens"`
}

// ContextCompactionRemoval soft-disables the active model-visible compaction
// without deleting raw transcript or historical compaction records.
type ContextCompactionRemoval struct {
	Type             string    `json:"type"`
	ID               string    `json:"id"`
	AgentKind        string    `json:"agent_kind,omitempty"`
	CompactionID     string    `json:"compaction_id,omitempty"`
	SourceStartIndex int       `json:"source_start_index"`
	SourceEndIndex   int       `json:"source_end_index"`
	Reason           string    `json:"reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ContextFold records a model-visible context fold: an old message interval is
// replaced by a summary placeholder in the projection without a new boundary and
// without deleting the raw transcript. It is mutually exclusive with full
// compaction (M6) because both manage the same "old history projection".
type ContextFold struct {
	Type               string    `json:"type"`
	ID                 string    `json:"id"`
	AgentKind          string    `json:"agent_kind,omitempty"`
	Summary            string    `json:"summary"`
	SourceStartIndex   int       `json:"source_start_index"`
	SourceEndIndex     int       `json:"source_end_index"`
	SourceMessageCount int       `json:"source_message_count"`
	RetainedTurns      int       `json:"retained_turns"`
	TokensBefore       int       `json:"tokens_before"`
	TokensAfter        int       `json:"tokens_after"`
	Reason             string    `json:"reason,omitempty"`
	Phase              string    `json:"phase,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
}

// ContextFoldRemoved soft-disables the latest active fold for an agent.
type ContextFoldRemoved struct {
	Type             string    `json:"type"`
	ID               string    `json:"id"`
	AgentKind        string    `json:"agent_kind,omitempty"`
	FoldID           string    `json:"fold_id,omitempty"`
	SourceStartIndex int       `json:"source_start_index"`
	SourceEndIndex   int       `json:"source_end_index"`
	Reason           string    `json:"reason,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
}

// ExternalizedResultEntry records a tool result that was externalized to disk
// (written whole, interface view only keeps preview + location reference). It is
// persisted so resume can replay the exact "externalized" decision and so the
// next request can micro-compact it on cold cache. Kept lightweight; the full
// body stays on disk at Location.
type ExternalizedResultEntry struct {
	Type           string    `json:"type"`
	IdempotencyKey string    `json:"idempotency_key"`
	Location       string    `json:"location"`
	OriginalBytes  int       `json:"original_bytes"`
	Truncated      bool      `json:"truncated"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// RuntimeState records the agent's runtime context that affects the *next* turn
// but is not part of the message transcript: which agent kind was active, the
// working mode, the active story/branch, the last read file, and any other
// bounded key/value pairs. It is persisted so resume can replay the exact
// "work site" (Plan.md M11) — without it, a reloaded session looks identical on
// screen but the model inherits a different working context.
type RuntimeState struct {
	Type      string            `json:"type"`
	ID        string            `json:"id,omitempty"`
	AgentKind string            `json:"agent_kind,omitempty"`
	Mode      string            `json:"mode,omitempty"`
	StoryID   string            `json:"story_id,omitempty"`
	BranchID  string            `json:"branch_id,omitempty"`
	LastRead  string            `json:"last_read,omitempty"`
	Settings  map[string]string `json:"settings,omitempty"`
	CreatedAt time.Time         `json:"created_at,omitempty"`
}

// DefaultMemoryNotesLimit bounds how many background memory notes a single
// compaction reads so a long conversation cannot feed an unbounded prompt into
// the summary model.
const DefaultMemoryNotesLimit = 8

// Checkpoint 记录一次"工作区 + 聊天"检查点：MessageIndex 是会话语义截断点
// （保留到该消息索引，含；之后的消息不再显示/进入模型），VersionID 是对应时刻
// 工作区的 go-git 版本 ID。每轮对话完成后自动创建（Reason=auto），用户也可手动
// 创建（Reason=manual）与还原。记录只追加、不修改旧行，保证可回滚。
type Checkpoint struct {
	Type         string    `json:"type"`
	ID           string    `json:"id"`
	MessageIndex int       `json:"message_index"`
	VersionID    string    `json:"version_id,omitempty"`
	Title        string    `json:"title,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// Truncate 记录一次会话逻辑截断：MessageIndex 之后的消息不再显示/进入模型，
// 但磁盘记录保留（append-only）。用于用户"重试生成"或"还原检查点"后的聊天截断。
type Truncate struct {
	Type         string    `json:"type"`
	MessageIndex int       `json:"message_index"`
	Reason       string    `json:"reason,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// RestoreResult 返回一次检查点还原的结果：工作区版本与会话语义截断点。
type RestoreResult struct {
	Checkpoint   Checkpoint `json:"checkpoint"`
	VersionID    string     `json:"version_id,omitempty"`
	MessageIndex int        `json:"message_index"`
}

// TruncateResult 返回一次会话逻辑截断的结果：最终可见消息索引。
type TruncateResult struct {
	MessageIndex int `json:"message_index"`
}

// Session 保存单个会话的内存状态。
//
// 逻辑链（副指针图）：每条消息通过 prevIdx 记录其在逻辑链中的上一条消息，
// branchID 记录所属分支。活动分支由 activeBranch/activeLast 追踪，
// 加载时从活动分支末端沿 prevIdx 回溯重建真正要继续的逻辑链。
// boundaryIndex 是压缩边界（投影起点），PrepareMessages 从此之后投影。
type Session struct {
	ID        string
	CreatedAt time.Time
	UpdatedAt time.Time

	filePath        string
	title           string
	clearAfterIndex int
	mu              sync.Mutex
	messages        []*schema.Message
	records         []historyRecord

	// 逻辑链副指针图：prevIdx[i] 为 messages[i] 在逻辑链中的上一条消息索引，-1 表示根（无前驱）。
	prevIdx []int
	// branchID[i] 为 messages[i] 所属逻辑分支标识；空字符串表示默认线性分支。
	branchID []string
	// activeBranch 为当前活动分支标识。
	activeBranch string
	// activeLast 为活动分支末端消息索引（-1 表示活动分支尚无消息）。
	activeLast int
	// forkPoint 记录当前活动分支从哪条消息分叉（-1 表示线性接续）。
	forkPoint int
	// branchTip 为当前活动分支末端消息索引，用于后续消息链接。
	branchTip int
	// boundaryIndex 为压缩边界：PrepareMessages / GetEffectiveMessages 从此之后投影。
	// 默认为 clearAfterIndex；全量压缩写入新边界时可前进。
	boundaryIndex int
	// hiddenStart / hiddenEnd 定义会话逻辑隐藏区间 [hiddenStart, hiddenEnd)：
	// 该区间内的消息不再显示/进入模型，但磁盘记录保留（append-only），可回滚。
	// 由用户"重试生成"或"还原检查点"时通过 AppendTruncate 设置并持久化；
	// 区间之后新增的消息索引 >= hiddenEnd，不受隐藏区间影响。
	hiddenStart int // -1 表示无隐藏区间
	hiddenEnd   int
}

// SessionMeta 是会话列表摘要。
type SessionMeta struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Active       bool      `json:"active"`
	MessageCount int       `json:"message_count"`
}
