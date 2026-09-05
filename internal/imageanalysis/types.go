// Package imageanalysis implements batch image analysis for extracting
// structured creative content (outline, progress, inspiration, character
// states, lore) from uploaded manga/novel page images using a configurable
// local multimodal model.
package imageanalysis

import "time"

// AnalysisIntent identifies what kind of creative content to extract.
type AnalysisIntent string

const (
	IntentOutline     AnalysisIntent = "outline"
	IntentProgress    AnalysisIntent = "progress"
	IntentInspiration AnalysisIntent = "inspiration"
	IntentState       AnalysisIntent = "state"
	IntentLore        AnalysisIntent = "lore"
)

// AllIntents returns all supported analysis intents.
func AllIntents() []AnalysisIntent {
	return []AnalysisIntent{IntentOutline, IntentProgress, IntentInspiration, IntentState, IntentLore}
}

// BatchStatus represents the lifecycle state of a batch analysis job.
type BatchStatus string

const (
	BatchPending   BatchStatus = "pending"
	BatchRunning   BatchStatus = "running"
	BatchCompleted BatchStatus = "completed"
	BatchFailed    BatchStatus = "failed"
	BatchAborted   BatchStatus = "aborted"
	BatchPartial   BatchStatus = "partial" // completed with some failures
)

// ImageItem represents a single image in the batch, ordered by page index.
type ImageItem struct {
	PageIndex   int    `json:"page_index"`   // 0-based order after natural sort
	FileName    string `json:"file_name"`    // original filename for display
	StoragePath string `json:"storage_path"` // relative path within workspace
	MIMEType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	// Text holds extracted PDF text-layer content. When non-empty the vision
	// model is skipped and this text is used directly as the page result.
	Text string `json:"text,omitempty"`
}

// PageResult is the analysis output for a single page.
type PageResult struct {
	PageIndex int             `json:"page_index"`
	FileName  string          `json:"file_name"`
	Status    string          `json:"status"` // "success", "failed", "pending"
	Error     string          `json:"error,omitempty"`
	Content   string          `json:"content,omitempty"` // extracted text/description
	Items     []ExtractedItem `json:"items,omitempty"`   // structured extracted items
	RetriedAt *time.Time      `json:"retried_at,omitempty"`
	// Truncated indicates the model response was likely cut off (incomplete
	// JSON). The frontend can warn the user to retry or increase max_tokens.
	Truncated bool `json:"truncated,omitempty"`
}

// ExtractedItem is one structured piece of content extracted from a page.
type ExtractedItem struct {
	Type    string `json:"type"`    // "character", "scene", "plot_point", "world_building", "dialogue", "other"
	Content string `json:"content"` // extracted content in Chinese Markdown
}

// BatchRequest is the input for starting a batch analysis.
type BatchRequest struct {
	Intents []AnalysisIntent `json:"intents"`
	Images  []UploadedImage  `json:"-"` // populated from multipart form
}

// UploadedImage carries raw image data from the multipart upload.
// For PDF text-layer pages, Data is nil and Text carries the extracted content;
// the vision model is skipped entirely for such pages.
type UploadedImage struct {
	FileName string
	Data     []byte
	MIMEType string
	Text     string // non-empty for PDF pages with a usable text layer (skips vision model)
	IsPDF    bool   // marks pages produced by PDF expansion (triggers book classification)
}

// BatchState is the persisted state of a batch analysis job.
type BatchState struct {
	ID          string           `json:"id"`
	Status      BatchStatus      `json:"status"`
	Intents     []AnalysisIntent `json:"intents"`
	Images      []ImageItem      `json:"images"`
	Results     []PageResult     `json:"results"`
	CreatedAt   time.Time        `json:"created_at"`
	StartedAt   *time.Time       `json:"started_at,omitempty"`
	FinishedAt  *time.Time       `json:"finished_at,omitempty"`
	CurrentPage int              `json:"current_page"` // index of page being processed
	TotalPages  int              `json:"total_pages"`
	ModelID     string           `json:"model_id,omitempty"`
	// BookType records the classified nature of a PDF document (manga/novel/
	// illustration/mixed). Empty for non-PDF batches or before classification.
	BookType string `json:"book_type,omitempty"`
	// IsPDF marks whether this batch originated from PDF expansion.
	IsPDF bool `json:"is_pdf,omitempty"`
	// ImagesDeleted 标记本批次的原始上传图片已被删除（保留期清扫或用户手动清理）。
	// 重试失败页与切换模型重测（re-extract）都依赖原始图片，删除后不可用；
	// 前端据此隐藏这两个入口。
	ImagesDeleted bool `json:"images_deleted,omitempty"`
}

// BatchResult is the aggregated output after all pages are processed.
type BatchResult struct {
	BatchID      string                    `json:"batch_id"`
	Status       BatchStatus               `json:"status"`
	TotalPages   int                       `json:"total_pages"`
	SuccessCount int                       `json:"success_count"`
	FailedCount  int                       `json:"failed_count"`
	Sections     map[AnalysisIntent]string `json:"sections"`              // intent -> aggregated markdown
	FactStream   string                    `json:"fact_stream,omitempty"` // 按页序拼接的完整事实流，供跨页整合模型使用
	FailedPages  []PageResult              `json:"failed_pages,omitempty"`
}

// ProgressEvent is emitted via SSE during batch processing.
type ProgressEvent struct {
	BatchID     string      `json:"batch_id"`
	Status      BatchStatus `json:"status"`
	CurrentPage int         `json:"current_page"`
	TotalPages  int         `json:"total_pages"`
	FileName    string      `json:"file_name,omitempty"`
	Preview     string      `json:"preview,omitempty"` // brief content preview
	Error       string      `json:"error,omitempty"`
}
