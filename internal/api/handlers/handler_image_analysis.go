package handlers

import (
	"bytes"
	"context"
	"image/png"
	"io"
	"log"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"golang.org/x/image/webp"

	"denova/internal/api/sse"
	"denova/internal/imageanalysis"
)

// ImageAnalysisMaxUploadBytes limits the total request body size for image
// analysis batch uploads.
const ImageAnalysisMaxUploadBytes int64 = 1024 * 1024 * 1024

// HandleImageAnalysisBatchCreate POST /api/image-analysis/batch — 提交批量图片分析。
// Accepts multipart form with image files and a comma-separated "intents" field.
func (h *Handlers) HandleImageAnalysisBatchCreate(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	// Parse intents and optional model override from form values.
	intentsStr := strings.TrimSpace(string(c.FormValue("intents")))
	intents := parseIntents(intentsStr)
	modelID := strings.TrimSpace(string(c.FormValue("model_id")))

	// Optional PDF page range (1-based, inclusive). Zero means "from start"/"to end".
	pageStart, pageEnd := parsePageRange(c)

	// Read uploaded image files.
	form, err := c.MultipartForm()
	if err != nil {
		writeError(c, consts.StatusBadRequest, "解析上传表单失败: "+err.Error())
		return
	}
	files := form.File["images"]
	if len(files) == 0 {
		writeErrorKey(c, consts.StatusBadRequest, "api.imageAnalysis.noImages")
		return
	}

	cfg := h.app.Config()
	images := make([]imageanalysis.UploadedImage, 0, len(files))
	for _, fh := range files {
		data, mime, err := readUploadedImage(fh)
		if err != nil {
			writeError(c, consts.StatusBadRequest, "读取图片失败: "+err.Error())
			return
		}
		if mime == "application/pdf" {
			expanded, err := expandPDF(data, fh.Filename, cfg, pageStart, pageEnd)
			if err != nil {
				writeError(c, consts.StatusBadRequest, "解析 PDF 失败: "+err.Error())
				return
			}
			images = append(images, expanded...)
			continue
		}
		images = append(images, imageanalysis.UploadedImage{
			FileName: fh.Filename,
			Data:     data,
			MIMEType: mime,
		})
	}

	if len(images) == 0 {
		writeErrorKey(c, consts.StatusBadRequest, "api.imageAnalysis.noImages")
		return
	}

	task, state, err := h.app.ImageAnalysis().StartBatch(ctx, images, intents, modelID)
	if err != nil {
		writeError(c, consts.StatusInternalServerError, err.Error())
		return
	}
	_ = task
	writeJSON(c, consts.StatusOK, state)
}

// HandleImageAnalysisBatchGet GET /api/image-analysis/batch/:id — 查询批次状态。
func (h *Handlers) HandleImageAnalysisBatchGet(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	state, err := h.app.ImageAnalysis().GetBatchState(batchID)
	if err != nil {
		writeError(c, consts.StatusNotFound, "批次不存在: "+err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, state)
}

// HandleImageAnalysisBatchStream GET /api/image-analysis/batch/:id/stream — SSE 进度流。
func (h *Handlers) HandleImageAnalysisBatchStream(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	task, ok := h.app.ImageAnalysis().ActiveBatchTask(batchID)
	if !ok {
		writeError(c, consts.StatusNotFound, "批次未在运行中")
		return
	}
	sse.StreamTaskUI(c, task)
}

// HandleImageAnalysisBatchResult GET /api/image-analysis/batch/:id/result — 获取聚合结果。
func (h *Handlers) HandleImageAnalysisBatchResult(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	result, err := h.app.ImageAnalysis().GetBatchResult(batchID)
	if err != nil {
		writeError(c, consts.StatusNotFound, "批次不存在: "+err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, result)
}

// HandleImageAnalysisBatchRetry POST /api/image-analysis/batch/:id/retry — 重试失败页面。
func (h *Handlers) HandleImageAnalysisBatchRetry(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	var req struct {
		PageIndices []int `json:"page_indices"`
	}
	c.BindJSON(&req)

	task, err := h.app.ImageAnalysis().RetryBatch(ctx, batchID, req.PageIndices)
	if err != nil {
		writeError(c, consts.StatusBadRequest, err.Error())
		return
	}
	_ = task
	writeJSON(c, consts.StatusOK, map[string]string{"status": "retrying"})
}

// HandleImageAnalysisBatchReExtract POST /api/image-analysis/batch/:id/re-extract — 切换模型重新提取。
// Reuses uploaded images from the source batch and creates a new batch with the given model.
func (h *Handlers) HandleImageAnalysisBatchReExtract(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	var req struct {
		ModelID string `json:"model_id"`
	}
	if err := c.BindJSON(&req); err != nil || strings.TrimSpace(req.ModelID) == "" {
		writeErrorKey(c, consts.StatusBadRequest, "api.imageAnalysis.modelRequired")
		return
	}

	task, state, err := h.app.ImageAnalysis().ReExtractBatch(ctx, batchID, strings.TrimSpace(req.ModelID))
	if err != nil {
		writeError(c, consts.StatusInternalServerError, err.Error())
		return
	}
	_ = task
	writeJSON(c, consts.StatusOK, state)
}

// HandleImageAnalysisBatchAbort POST /api/image-analysis/batch/:id/abort — 中止批次。
func (h *Handlers) HandleImageAnalysisBatchAbort(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	if !h.app.ImageAnalysis().AbortBatch(batchID) {
		writeError(c, consts.StatusNotFound, "批次未在运行中")
		return
	}
	writeJSON(c, consts.StatusOK, map[string]string{"status": "aborted"})
}

// HandleImageAnalysisBatchCleanupImages POST /api/image-analysis/batch/:id/cleanup-images —
// 手动删除批次原始上传图片。重试/重测依赖原始图片，删除后不可用；运行中的批次拒绝清理。
func (h *Handlers) HandleImageAnalysisBatchCleanupImages(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	if err := h.app.ImageAnalysis().CleanupBatchImages(batchID); err != nil {
		writeError(c, consts.StatusBadRequest, err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, map[string]string{"status": "cleaned"})
}

// HandleImageAnalysisBatchApply POST /api/image-analysis/batch/:id/apply — 将结果写入文件/资料库。
// MVP: returns the aggregated result markdown for the frontend/agent to consume.
// Actual file writes are delegated to the IDE agent via the chat flow.
func (h *Handlers) HandleImageAnalysisBatchApply(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	batchID := c.Param("id")
	result, err := h.app.ImageAnalysis().GetBatchResult(batchID)
	if err != nil {
		writeError(c, consts.StatusNotFound, "批次不存在: "+err.Error())
		return
	}
	// Return the result so the caller (frontend or agent) can decide how to apply.
	writeJSON(c, consts.StatusOK, result)
}

// HandleImageAnalysisBatchesRecent GET /api/image-analysis/batches/recent — 列出最近批次（用于刷新恢复）。
func (h *Handlers) HandleImageAnalysisBatchesRecent(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	limit := 10
	if v := string(c.Query("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	batches, err := h.app.ImageAnalysis().ListRecentBatches(limit)
	if err != nil {
		writeError(c, consts.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, batches)
}

// HandleImageAnalysisModels GET /api/image-analysis/models — 列出可选视觉模型配置。
func (h *Handlers) HandleImageAnalysisModels(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	models := h.app.ImageAnalysis().ListModels()
	writeJSON(c, consts.StatusOK, models)
}

// HandleImageAnalysisBatchMerge POST /api/image-analysis/batches/merge — 合并多个批次的分析结果。
// 接收 JSON body: { "source_ids": ["id1", "id2", ...] }
// 返回新合并批次的 state。
func (h *Handlers) HandleImageAnalysisBatchMerge(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	var req struct {
		SourceIDs []string `json:"source_ids"`
	}
	if err := c.BindJSON(&req); err != nil {
		writeError(c, consts.StatusBadRequest, "请求格式错误: "+err.Error())
		return
	}
	if len(req.SourceIDs) < 2 {
		writeError(c, consts.StatusBadRequest, "至少需要两个源批次才能合并")
		return
	}

	merged, err := h.app.ImageAnalysis().MergeBatches(req.SourceIDs)
	if err != nil {
		writeError(c, consts.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(c, consts.StatusOK, merged)
}

// parseIntents parses a comma-separated intent string into AnalysisIntent slice.
func parseIntents(s string) []imageanalysis.AnalysisIntent {
	if strings.TrimSpace(s) == "" {
		return imageanalysis.AllIntents()
	}
	parts := strings.Split(s, ",")
	intents := make([]imageanalysis.AnalysisIntent, 0, len(parts))
	valid := make(map[imageanalysis.AnalysisIntent]bool)
	for _, i := range imageanalysis.AllIntents() {
		valid[i] = true
	}
	for _, raw := range parts {
		intent := imageanalysis.AnalysisIntent(strings.TrimSpace(raw))
		if valid[intent] {
			intents = append(intents, intent)
		}
	}
	if len(intents) == 0 {
		return imageanalysis.AllIntents()
	}
	return intents
}

// readUploadedImage reads the full content and detects MIME type of an uploaded file.
// WebP images are transparently converted to PNG because llama-server's stb_image.h
// decoder has known compatibility issues with certain WebP encoding parameters
// (VP8L variants, alpha channel, etc.), causing intermittent vision failures.
func readUploadedImage(fh *multipart.FileHeader) ([]byte, string, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	mime := fh.Header.Get("Content-Type")
	if mime == "" {
		mime = detectImageMIME(data)
	}
	// Convert WebP → PNG to avoid stb_image.h decode failures in llama-server.
	if mime == "image/webp" {
		if pngData, convErr := convertWebPToPNG(data); convErr != nil {
			log.Printf("[image-analysis] webp→png conversion failed for %q, sending raw: %v", fh.Filename, convErr)
		} else {
			log.Printf("[image-analysis] converted webp→png for %q (%d→%d bytes)", fh.Filename, len(data), len(pngData))
			return pngData, "image/png", nil
		}
	}
	return data, mime, nil
}

// convertWebPToPNG decodes WebP bytes using Go's robust decoder and re-encodes
// as PNG. This eliminates stb_image.h compatibility issues in llama-server.
func convertWebPToPNG(data []byte) ([]byte, error) {
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// detectImageMIME sniffs the first bytes to determine image MIME type.
func detectImageMIME(data []byte) string {
	if len(data) < 4 {
		return "application/octet-stream"
	}
	// PDF: 25 50 44 46 ("%PDF")
	if data[0] == 0x25 && data[1] == 0x50 && data[2] == 0x44 && data[3] == 0x46 {
		return "application/pdf"
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return "image/png"
	}
	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return "image/jpeg"
	}
	// GIF: 47 49 46 38
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x38 {
		return "image/gif"
	}
	// WebP: 52 49 46 46 ... 57 45 42 50
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[2] == 0x46 && data[3] == 0x46 &&
		data[8] == 0x57 && data[9] == 0x45 && data[10] == 0x42 && data[11] == 0x50 {
		return "image/webp"
	}
	return "image/png" // default fallback
}
