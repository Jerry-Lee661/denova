package handlers

import (
	"context"
	"io"
	"mime/multipart"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/protocol/consts"

	"denova/internal/api/sse"
	"denova/internal/imageanalysis"
)

// HandleImageAnalysisBatchCreate POST /api/image-analysis/batch — 提交批量图片分析。
// Accepts multipart form with image files and a comma-separated "intents" field.
func (h *Handlers) HandleImageAnalysisBatchCreate(ctx context.Context, c *app.RequestContext) {
	if !h.requireWorkspace(c) {
		return
	}
	// Parse intents from form value.
	intentsStr := strings.TrimSpace(string(c.FormValue("intents")))
	intents := parseIntents(intentsStr)

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

	images := make([]imageanalysis.UploadedImage, 0, len(files))
	for _, fh := range files {
		data, mime, err := readUploadedImage(fh)
		if err != nil {
			writeError(c, consts.StatusBadRequest, "读取图片失败: "+err.Error())
			return
		}
		images = append(images, imageanalysis.UploadedImage{
			FileName: fh.Filename,
			Data:     data,
			MIMEType: mime,
		})
	}

	task, state, err := h.app.ImageAnalysis().StartBatch(ctx, images, intents)
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
	return data, mime, nil
}

// detectImageMIME sniffs the first bytes to determine image MIME type.
func detectImageMIME(data []byte) string {
	if len(data) < 4 {
		return "application/octet-stream"
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
