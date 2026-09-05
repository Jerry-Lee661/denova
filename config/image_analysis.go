package config

// ImageAnalysis 用户级偏好段：批量图像分析管线（internal/imageanalysis）的行为配置。
// 本文件自 v0.3.3 线移植，适配 v0.4.0 的 Settings/Config 双层配置架构：
// 模型端点解析仍走 Config（model_profiles.go），行为偏好走 Settings 分层合并。

// ImageAnalysisSettings 配置图片分析管线的行为。所有字段均为可选，
// 未设置时通过 Settings 上的访问器方法回退到默认值。
// 指针字段用于在分层配置合并中区分"未设置"与"显式零值"。
type ImageAnalysisSettings struct {
	// PanelAwareExtraction 启用两阶段分镜级提取：先枚举分镜，再逐格深度提取。
	// 适合分镜密集的漫画页；轻量模型或简单页面可关闭走单次调用。默认 true。
	PanelAwareExtraction *bool `toml:"panel_aware_extraction,omitempty" json:"panel_aware_extraction,omitempty"`
	// PrevPageSummaryChars 限制注入下一页 prompt 的上一页摘要字符数，用于跨页连续性。
	// 0 关闭注入。默认 500。
	PrevPageSummaryChars *int `toml:"prev_page_summary_chars,omitempty" json:"prev_page_summary_chars,omitempty"`
	// MaxPanelsPerRequest 限制第二阶段单次处理的最大分镜数，超过则截断，防止 token 爆炸。默认 20。
	MaxPanelsPerRequest *int `toml:"max_panels_per_request,omitempty" json:"max_panels_per_request,omitempty"`
	// MaxResponseTokens 设置视觉模型单次响应的 max_tokens 上限。默认 8192。
	MaxResponseTokens *int `toml:"max_response_tokens,omitempty" json:"max_response_tokens,omitempty"`
	// RequestTimeoutSeconds 设置视觉模型单次 HTTP 请求的超时秒数。
	// 本地推理模型（先思考后回答）单次生成可能超过 3 分钟，硬编码 180s 会切断长生成。
	// 默认 360s。0 表示不设置超时（无限等待）。
	RequestTimeoutSeconds *int `toml:"request_timeout_seconds,omitempty" json:"request_timeout_seconds,omitempty"`
	// ImageResizeMaxDim 发送前图片缩放的最大边长像素。超过则等比缩放到该值以内，
	// 降低视觉 token 与 prefill 耗时。默认 1600。0 表示不缩放。
	ImageResizeMaxDim *int `toml:"image_resize_max_dim,omitempty" json:"image_resize_max_dim,omitempty"`
	// ImageResizeQuality JPEG 压缩质量（1-100）。默认 82。
	ImageResizeQuality *int `toml:"image_resize_quality,omitempty" json:"image_resize_quality,omitempty"`
	// PDFRenderDPI 设置 PDF 页面渲染为图片时的 DPI。默认 300。
	PDFRenderDPI *int `toml:"pdf_render_dpi,omitempty" json:"pdf_render_dpi,omitempty"`
	// PDFMaxPages 限制单批次 PDF 处理的最大页数。默认 50。
	PDFMaxPages *int `toml:"pdf_max_pages,omitempty" json:"pdf_max_pages,omitempty"`
	// PDFTextLayerTrust 控制是否信任 PDF 文本层。true 时优先用文本层提取，
	// 乱序或无文本层时回退到视觉模型渲染。默认 true。
	PDFTextLayerTrust *bool `toml:"pdf_text_layer_trust,omitempty" json:"pdf_text_layer_trust,omitempty"`
	// PDFClassifySamplePages 设置书本性质预判断时抽样的页数。默认 5。
	PDFClassifySamplePages *int `toml:"pdf_classify_sample_pages,omitempty" json:"pdf_classify_sample_pages,omitempty"`
	// BatchConcurrency 设置批次内并行分析的最大并发数。默认 1（串行）。
	// 本地视觉模型显存有限，建议 2-3；过高会触发 bad allocation。
	BatchConcurrency *int `toml:"batch_concurrency,omitempty" json:"batch_concurrency,omitempty"`
	// ImageRetentionMinutes 批次结束后原始上传图片的保留分钟数。
	// 保留期内可随时切换模型重测（re-extract）或重试失败页；超期后在应用启动
	// 或新建批次时被惰性清理。默认 1440（24 小时）。0 表示永不自动清理，
	// 只能通过手动清理接口回收。
	ImageRetentionMinutes *int `toml:"image_retention_minutes,omitempty" json:"image_retention_minutes,omitempty"`
}

// 图片分析配置默认值。
const (
	DefaultImageAnalysisPanelAware        = true
	DefaultImageAnalysisPrevPageChars     = 500
	DefaultImageAnalysisMaxPanels         = 20
	DefaultImageAnalysisMaxResponseTokens = 8192
	// DefaultImageAnalysisRequestTimeoutSeconds 视觉模型单次请求超时（默认 360s，本地推理模型预留长生成）。
	DefaultImageAnalysisRequestTimeoutSeconds = 360
	DefaultImageAnalysisImageResizeMaxDim     = 1600
	DefaultImageAnalysisImageResizeQuality    = 82
	DefaultImageAnalysisPDFRenderDPI          = 300
	DefaultImageAnalysisPDFMaxPages           = 50
	DefaultImageAnalysisPDFTextLayerTrust     = true
	DefaultImageAnalysisPDFClassifyPages      = 5
	// DefaultImageAnalysisBatchConcurrency 批次内并行分析的最大并发数（默认 1 串行）。
	DefaultImageAnalysisBatchConcurrency = 1
	// DefaultImageAnalysisImageRetentionMinutes 批次图片保留分钟数（默认 24h）。
	DefaultImageAnalysisImageRetentionMinutes = 1440
)

// ImageAnalysisPanelAware 返回是否启用两阶段分镜提取（默认 true）。
func (c *Settings) ImageAnalysisPanelAware() bool {
	if c != nil && c.ImageAnalysis.PanelAwareExtraction != nil {
		return *c.ImageAnalysis.PanelAwareExtraction
	}
	return DefaultImageAnalysisPanelAware
}

// ImageAnalysisPrevPageSummaryChars 返回跨页上下文注入的字符上限（默认 500，0 关闭）。
func (c *Settings) ImageAnalysisPrevPageSummaryChars() int {
	if c != nil && c.ImageAnalysis.PrevPageSummaryChars != nil {
		return *c.ImageAnalysis.PrevPageSummaryChars
	}
	return DefaultImageAnalysisPrevPageChars
}

// ImageAnalysisMaxPanelsPerRequest 返回第二阶段单次处理的最大分镜数（默认 20）。
func (c *Settings) ImageAnalysisMaxPanelsPerRequest() int {
	if c != nil && c.ImageAnalysis.MaxPanelsPerRequest != nil {
		return *c.ImageAnalysis.MaxPanelsPerRequest
	}
	return DefaultImageAnalysisMaxPanels
}

// ImageAnalysisMaxResponseTokens 返回视觉模型单次响应的 max_tokens 上限（默认 8192）。
func (c *Settings) ImageAnalysisMaxResponseTokens() int {
	if c != nil && c.ImageAnalysis.MaxResponseTokens != nil {
		return *c.ImageAnalysis.MaxResponseTokens
	}
	return DefaultImageAnalysisMaxResponseTokens
}

// ImageAnalysisRequestTimeoutSeconds 返回视觉模型单次 HTTP 请求的超时秒数。
// 0 表示不设置超时（无限等待），对齐 AGENTS.md"写死超时"的约束——默认给足，用户可配置。
func (c *Settings) ImageAnalysisRequestTimeoutSeconds() int {
	if c != nil && c.ImageAnalysis.RequestTimeoutSeconds != nil {
		return *c.ImageAnalysis.RequestTimeoutSeconds
	}
	return DefaultImageAnalysisRequestTimeoutSeconds
}

// ImageAnalysisImageRetentionMinutes 返回批次结束后原始图片的保留分钟数。
// 0 表示永不自动清理（仅手动清理）。
func (c *Settings) ImageAnalysisImageRetentionMinutes() int {
	if c != nil && c.ImageAnalysis.ImageRetentionMinutes != nil {
		return *c.ImageAnalysis.ImageRetentionMinutes
	}
	return DefaultImageAnalysisImageRetentionMinutes
}

// ImageAnalysisImageResizeMaxDim 返回发送前图片缩放的最大边长像素。默认 1600。
func (c *Settings) ImageAnalysisImageResizeMaxDim() int {
	if c != nil && c.ImageAnalysis.ImageResizeMaxDim != nil {
		return *c.ImageAnalysis.ImageResizeMaxDim
	}
	return DefaultImageAnalysisImageResizeMaxDim
}

// ImageAnalysisImageResizeQuality 返回 JPEG 压缩质量（1-100）。默认 82。
func (c *Settings) ImageAnalysisImageResizeQuality() int {
	if c != nil && c.ImageAnalysis.ImageResizeQuality != nil {
		return *c.ImageAnalysis.ImageResizeQuality
	}
	return DefaultImageAnalysisImageResizeQuality
}

// ImageAnalysisPDFRenderDPI 返回 PDF 页面渲染 DPI（默认 300）。
func (c *Settings) ImageAnalysisPDFRenderDPI() int {
	if c != nil && c.ImageAnalysis.PDFRenderDPI != nil {
		return *c.ImageAnalysis.PDFRenderDPI
	}
	return DefaultImageAnalysisPDFRenderDPI
}

// ImageAnalysisPDFMaxPages 返回单批次 PDF 最大处理页数（默认 50）。
func (c *Settings) ImageAnalysisPDFMaxPages() int {
	if c != nil && c.ImageAnalysis.PDFMaxPages != nil {
		return *c.ImageAnalysis.PDFMaxPages
	}
	return DefaultImageAnalysisPDFMaxPages
}

// ImageAnalysisPDFTextLayerTrust 返回是否信任 PDF 文本层（默认 true）。
func (c *Settings) ImageAnalysisPDFTextLayerTrust() bool {
	if c != nil && c.ImageAnalysis.PDFTextLayerTrust != nil {
		return *c.ImageAnalysis.PDFTextLayerTrust
	}
	return DefaultImageAnalysisPDFTextLayerTrust
}

// ImageAnalysisPDFClassifySamplePages 返回书本性质预判断的抽样页数（默认 5）。
func (c *Settings) ImageAnalysisPDFClassifySamplePages() int {
	if c != nil && c.ImageAnalysis.PDFClassifySamplePages != nil {
		return *c.ImageAnalysis.PDFClassifySamplePages
	}
	return DefaultImageAnalysisPDFClassifyPages
}

// ImageAnalysisBatchConcurrency 返回批次内并行分析的最大并发数（默认 1 串行）。
// 本地视觉模型显存有限，建议 2-3；过高会触发 bad allocation。
func (c *Settings) ImageAnalysisBatchConcurrency() int {
	if c != nil && c.ImageAnalysis.BatchConcurrency != nil {
		return *c.ImageAnalysis.BatchConcurrency
	}
	return DefaultImageAnalysisBatchConcurrency
}

// MergeImageAnalysisSettings 用 child 的非 nil 字段覆盖 parent。
func MergeImageAnalysisSettings(parent, child ImageAnalysisSettings) ImageAnalysisSettings {
	out := parent
	if child.PanelAwareExtraction != nil {
		out.PanelAwareExtraction = child.PanelAwareExtraction
	}
	if child.PrevPageSummaryChars != nil {
		out.PrevPageSummaryChars = child.PrevPageSummaryChars
	}
	if child.MaxPanelsPerRequest != nil {
		out.MaxPanelsPerRequest = child.MaxPanelsPerRequest
	}
	if child.MaxResponseTokens != nil {
		out.MaxResponseTokens = child.MaxResponseTokens
	}
	if child.RequestTimeoutSeconds != nil {
		out.RequestTimeoutSeconds = child.RequestTimeoutSeconds
	}
	if child.ImageResizeMaxDim != nil {
		out.ImageResizeMaxDim = child.ImageResizeMaxDim
	}
	if child.ImageResizeQuality != nil {
		out.ImageResizeQuality = child.ImageResizeQuality
	}
	if child.PDFRenderDPI != nil {
		out.PDFRenderDPI = child.PDFRenderDPI
	}
	if child.PDFMaxPages != nil {
		out.PDFMaxPages = child.PDFMaxPages
	}
	if child.PDFTextLayerTrust != nil {
		out.PDFTextLayerTrust = child.PDFTextLayerTrust
	}
	if child.PDFClassifySamplePages != nil {
		out.PDFClassifySamplePages = child.PDFClassifySamplePages
	}
	if child.BatchConcurrency != nil {
		out.BatchConcurrency = child.BatchConcurrency
	}
	return out
}
