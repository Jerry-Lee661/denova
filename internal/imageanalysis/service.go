package imageanalysis

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"denova/config"
)

// visionModelConfig holds the resolved model endpoint for direct HTTP calls.
// We bypass the eino-ext OpenAI adapter because its underlying go-openai fork
// (meguminnnnnnnnn/go-openai) lacks a custom MarshalJSON for
// ChatCompletionMessage.MultiContent, causing the multimodal content array to
// be serialized under the wrong JSON key and rejected by LM Studio / local
// vision servers.
type visionModelConfig struct {
	APIKey      string
	Model       string
	BaseURL     string
	Temperature *float64 // 非 nil 时透传给视觉模型，nil 则不传（用 llama-server 默认值）
}

// --- OpenAI-compatible wire types (minimal, vision-focused) ---

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"` // 非 nil 时覆盖 llama-server 默认采样温度
	Stream      bool          `json:"stream,omitempty"`      // 流式读取：SSE 逐 token，超时语义改为空闲超时
}

// chatStreamChunk 是 SSE 流中单个 data: 行的 JSON（OpenAI 兼容格式）。
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

// floatPtr 返回指向该 float64 的指针。
func floatPtr(v float64) *float64 { return &v }

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Service orchestrates batch image analysis using a configurable vision model.
type Service struct {
	cfg   *config.Config
	store *Store
}

// NewService creates an image analysis service for the given workspace.
func NewService(cfg *config.Config, workspace string) *Service {
	return &Service{
		cfg:   cfg,
		store: NewStore(workspace),
	}
}

// Store returns the underlying state store.
func (s *Service) Store() *Store {
	return s.store
}

// isMemoryError 识别服务端资源类错误（bad allocation / out of memory 等）。
// 本地视觉模型在长 prefill 或高并发下可能触发 std::bad_alloc；这类错误
// 纳入批次状态观测，便于用户判断是显存不足还是模型故障。
func isMemoryError(err error) bool {
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

// ProcessBatch runs the full analysis pipeline for a batch.
// It processes images with bounded concurrency (default 1 = sequential),
// emitting progress events via the callback. Failed pages are marked but do
// not abort the batch. Memory errors (bad allocation) are observed and logged
// so users can distinguish VRAM exhaustion from model failures.
func (s *Service) ProcessBatch(ctx context.Context, state *BatchState, onProgress func(ProgressEvent)) error {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	now := time.Now().UTC()
	state.Status = BatchRunning
	state.StartedAt = &now
	if err := s.store.Save(state); err != nil {
		return fmt.Errorf("保存批次状态失败: %w", err)
	}

	modelCfg := s.modelConfig(state.ModelID)
	log.Printf("[image-analysis] batch begin id=%s pages=%d model=%q base_url=%q", state.ID, state.TotalPages, modelCfg.Model, modelCfg.BaseURL)

	// For PDF batches, classify the document's nature once (manga/novel/
	// illustration/mixed) so each page can pick the right extraction strategy.
	if state.IsPDF && state.BookType == "" {
		state.BookType = string(s.classifyBookFromState(ctx, modelCfg, state))
		s.store.Save(state)
		log.Printf("[image-analysis] book classified id=%s type=%s", state.ID, state.BookType)
	}

	// Bounded concurrency: default 1 (sequential), configurable 2-3 for local
	// vision models. Higher concurrency risks bad allocation on limited VRAM.
	concurrency := s.cfg.ImageAnalysisBatchConcurrency()
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 4 {
		concurrency = 4
	}

	var (
		mu             sync.Mutex
		memoryErrCount int
	)

	// processPage handles a single page index with state protection.
	processPage := func(i int) {
		if ctx.Err() != nil {
			return
		}
		mu.Lock()
		if state.Results[i].Status == "success" {
			mu.Unlock()
			return
		}
		state.CurrentPage = i
		state.Results[i].Status = "processing"
		s.store.Save(state)
		mu.Unlock()

		onProgress(ProgressEvent{
			BatchID:     state.ID,
			Status:      BatchRunning,
			CurrentPage: i,
			TotalPages:  state.TotalPages,
			FileName:    state.Images[i].FileName,
		})

		// PDF text-layer pages skip the vision model entirely.
		if state.Images[i].Text != "" {
			mu.Lock()
			state.Results[i] = textPageResult(state.Images[i])
			s.store.Save(state)
			mu.Unlock()
			return
		}

		// Compute prev-page summary under the lock to avoid a data race when
		// pages run concurrently (prevPageSummary reads state.Results[i-1]).
		mu.Lock()
		prevSummary := s.prevPageSummary(state, i)
		mu.Unlock()

		result, err := s.analyzePage(ctx, modelCfg, state, i, prevSummary)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			log.Printf("[image-analysis] page failed id=%s page=%d file=%q err=%v", state.ID, i, state.Images[i].FileName, err)
			if isMemoryError(err) {
				memoryErrCount++
				log.Printf("[image-analysis] MEMORY ERROR id=%s page=%d file=%q err=%v (concurrency=%d)", state.ID, i, state.Images[i].FileName, err, concurrency)
			}
			state.Results[i].Status = "failed"
			state.Results[i].Error = err.Error()
			onProgress(ProgressEvent{
				BatchID:     state.ID,
				Status:      BatchRunning,
				CurrentPage: i,
				TotalPages:  state.TotalPages,
				FileName:    state.Images[i].FileName,
				Error:       err.Error(),
			})
		} else {
			state.Results[i] = result
			preview := result.Content
			if len([]rune(preview)) > 100 {
				preview = string([]rune(preview)[:100]) + "…"
			}
			onProgress(ProgressEvent{
				BatchID:     state.ID,
				Status:      BatchRunning,
				CurrentPage: i,
				TotalPages:  state.TotalPages,
				FileName:    state.Images[i].FileName,
				Preview:     preview,
			})
		}
		s.store.Save(state)
	}

	if concurrency == 1 {
		// Sequential path (default).
		for i := range state.Images {
			if ctx.Err() != nil {
				state.Status = BatchAborted
				s.store.Save(state)
				return ctx.Err()
			}
			processPage(i)
		}
	} else {
		// Concurrent worker pool.
		sem := make(chan struct{}, concurrency)
		var wg sync.WaitGroup
		for i := range state.Images {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(idx int) {
				defer wg.Done()
				defer func() { <-sem }()
				processPage(idx)
			}(i)
		}
		wg.Wait()
		if ctx.Err() != nil {
			state.Status = BatchAborted
			s.store.Save(state)
			return ctx.Err()
		}
	}

	// Determine final status.
	failedCount := 0
	for _, r := range state.Results {
		if r.Status == "failed" {
			failedCount++
		}
	}
	finishTime := time.Now().UTC()
	state.FinishedAt = &finishTime
	if failedCount == 0 {
		state.Status = BatchCompleted
	} else if failedCount == state.TotalPages {
		state.Status = BatchFailed
	} else {
		state.Status = BatchPartial
	}
	s.store.Save(state)
	log.Printf("[image-analysis] batch done id=%s status=%s success=%d failed=%d memory_errors=%d concurrency=%d", state.ID, state.Status, state.TotalPages-failedCount, failedCount, memoryErrCount, concurrency)

	// 原始上传图片在批次结束后保留（供切换模型重测 / 失败页重试），不在此处
	// 删除；由保留期惰性清扫（SweepExpiredUploads）或用户手动清理负责回收。
	return nil
}

// RetryPages re-processes specific failed pages (or all failed if indices empty).
func (s *Service) RetryPages(ctx context.Context, batchID string, pageIndices []int, onProgress func(ProgressEvent)) error {
	state, err := s.store.Load(batchID)
	if err != nil {
		return err
	}
	if state.Status == BatchRunning {
		return fmt.Errorf("批次正在处理中，请等待完成")
	}

	// Determine which pages to retry.
	retrySet := make(map[int]bool)
	if len(pageIndices) == 0 {
		for i, r := range state.Results {
			if r.Status == "failed" {
				retrySet[i] = true
			}
		}
	} else {
		for _, idx := range pageIndices {
			if idx >= 0 && idx < len(state.Results) && state.Results[idx].Status == "failed" {
				retrySet[idx] = true
			}
		}
	}
	if len(retrySet) == 0 {
		return fmt.Errorf("没有需要重试的失败页面")
	}

	now := time.Now().UTC()
	state.Status = BatchRunning
	state.StartedAt = &now
	state.FinishedAt = nil
	s.store.Save(state)

	modelCfg := s.modelConfig(state.ModelID)
	for i := range state.Images {
		if !retrySet[i] {
			continue
		}
		if ctx.Err() != nil {
			state.Status = BatchAborted
			s.store.Save(state)
			return ctx.Err()
		}
		state.CurrentPage = i
		state.Results[i].Status = "processing"
		s.store.Save(state)

		// Text-layer pages bypass the vision model entirely.
		if state.Images[i].Text != "" {
			result := textPageResult(state.Images[i])
			retryTime := time.Now().UTC()
			result.RetriedAt = &retryTime
			state.Results[i] = result
			s.store.Save(state)
			continue
		}

		result, err := s.analyzePage(ctx, modelCfg, state, i, s.prevPageSummary(state, i))
		retryTime := time.Now().UTC()
		if err != nil {
			state.Results[i].Status = "failed"
			state.Results[i].Error = err.Error()
			state.Results[i].RetriedAt = &retryTime
		} else {
			result.RetriedAt = &retryTime
			state.Results[i] = result
		}
		s.store.Save(state)
	}

	failedCount := 0
	for _, r := range state.Results {
		if r.Status == "failed" {
			failedCount++
		}
	}
	finishTime := time.Now().UTC()
	state.FinishedAt = &finishTime
	if failedCount == 0 {
		state.Status = BatchCompleted
	} else {
		state.Status = BatchPartial
	}
	s.store.Save(state)
	return nil
}

// Aggregate collects all successful results into a BatchResult with
// per-intent aggregated markdown sections.
func (s *Service) Aggregate(state *BatchState) *BatchResult {
	result := &BatchResult{
		BatchID:    state.ID,
		Status:     state.Status,
		TotalPages: state.TotalPages,
		Sections:   make(map[AnalysisIntent]string),
	}
	var allItems []ExtractedItem
	for _, r := range state.Results {
		if r.Status == "success" {
			result.SuccessCount++
			// Apply per-page dedup before noise filtering.
			deduped := dedupConsecutiveDuplicates(r.Items)
			for _, item := range deduped {
				if !isNoiseItem(item) {
					allItems = append(allItems, item)
				}
			}
		} else if r.Status == "failed" {
			result.FailedCount++
			result.FailedPages = append(result.FailedPages, r)
		}
	}
	for _, intent := range state.Intents {
		result.Sections[intent] = aggregateSection(intent, state, allItems)
	}
	result.FactStream = buildFactStream(state)
	return result
}

// pageStrategy resolves the extraction strategy for a single page: which system
// prompt to use and whether to enable panel-aware two-stage extraction.
//
// Non-PDF batches (empty BookType) preserve the original behavior. For PDF
// batches the classified BookType drives the choice; mixed documents fall back
// to a per-page classification call.
func (s *Service) pageStrategy(ctx context.Context, modelCfg visionModelConfig, state *BatchState, pageIndex int, dataURI string) (sysPrompt string, usePanelAware bool) {
	bt := BookType(state.BookType)

	// Non-PDF batch: keep existing manga-oriented behavior.
	if state.BookType == "" {
		return systemInstruction(state.Intents), s.cfg.ImageAnalysisPanelAware()
	}

	// Mixed documents: classify this individual page to pick a strategy.
	if bt == BookTypeMixed {
		bt = s.classifySinglePage(ctx, modelCfg, dataURI)
	}

	switch bt {
	case BookTypeNovel:
		return systemInstructionForNovel(state.Intents), false
	case BookTypeIllustration:
		return systemInstructionForIllustration(state.Intents), false
	default: // BookTypeManga or unknown
		return systemInstruction(state.Intents), s.cfg.ImageAnalysisPanelAware()
	}
}

// classifySinglePage makes one vision call to determine a single page's type.
// Used only for mixed documents. Falls back to BookTypeManga on failure.
func (s *Service) classifySinglePage(ctx context.Context, modelCfg visionModelConfig, dataURI string) BookType {
	sysContent, _ := json.Marshal(bookClassificationInstruction())
	userParts := []contentPart{
		{Type: "text", Text: "请判断这一页属于哪种类型。"},
		{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
	}
	userContent, _ := json.Marshal(userParts)
	reqBody := chatRequest{
		Model:     modelCfg.Model,
		MaxTokens: 256,
		Messages: []chatMessage{
			{Role: "system", Content: sysContent},
			{Role: "user", Content: userContent},
		},
	}
	resp, err := s.callVisionAPI(ctx, modelCfg, reqBody)
	if err != nil {
		log.Printf("[image-analysis] single-page classify failed, defaulting to manga: %v", err)
		return BookTypeManga
	}
	bt, err := parseBookClassification(resp)
	if err != nil {
		return BookTypeManga
	}
	// A single page can't be "mixed"; coerce to manga as a safe default.
	if bt == BookTypeMixed {
		return BookTypeManga
	}
	return bt
}

// textPageResult builds a PageResult directly from an extracted PDF text layer,
// bypassing the vision model entirely. Used for PDF pages with a usable text
// layer where rendering + OCR would be wasteful.
func textPageResult(img ImageItem) PageResult {
	return PageResult{
		PageIndex: img.PageIndex,
		FileName:  img.FileName,
		Status:    "success",
		Content:   img.Text,
		Items:     []ExtractedItem{{Type: "dialogue", Content: img.Text}},
	}
}

// analyzePage sends a single image to the vision model via direct HTTP and
// parses the structured JSON response.
// analyzePage processes a single page. prevSummary is passed in by the caller
// (computed under the batch lock) to avoid a data race when pages run
// concurrently; it is the bounded summary of the previous page's extraction.
func (s *Service) analyzePage(ctx context.Context, modelCfg visionModelConfig, state *BatchState, pageIndex int, prevSummary string) (PageResult, error) {
	img := state.Images[pageIndex]
	result := PageResult{
		PageIndex: pageIndex,
		FileName:  img.FileName,
		Status:    "success",
	}

	// Read image data from storage.
	imgData, err := s.readImageData(img)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		return result, err
	}

	// 发送前图片缩放/压缩：按视觉模型分辨率上限压缩后再 base64，降低视觉 token 与 prefill 耗时。
	resizedData := imgData
	if maxDim := s.cfg.ImageAnalysisImageResizeMaxDim(); maxDim > 0 {
		q := s.cfg.ImageAnalysisImageResizeQuality()
		if q <= 0 {
			q = 82
		}
		resized, err := resizeImage(imgData, img.MIMEType, maxDim, q)
		if err != nil {
			log.Printf("[image-analysis] resize failed id=%s page=%d err=%v (use original)", state.ID, pageIndex, err)
		} else if len(resized) < len(imgData) {
			resizedData = resized
			log.Printf("[image-analysis] resized id=%s page=%d %d->%d bytes", state.ID, pageIndex, len(imgData), len(resized))
		}
	}

	// Build the OpenAI-compatible multimodal request body directly.
	// We bypass the eino-ext adapter because its underlying go-openai fork
	// serializes MultiContent under the wrong JSON key.
	dataURI := fmt.Sprintf("data:%s;base64,%s", img.MIMEType, base64.StdEncoding.EncodeToString(resizedData))

	maxTokens := s.cfg.ImageAnalysisMaxResponseTokens()

	// Resolve the extraction strategy (system prompt + panel-aware toggle)
	// based on the classified book type.
	sysPrompt, usePanelAware := s.pageStrategy(ctx, modelCfg, state, pageIndex, dataURI)

	// Panel-aware two-stage extraction for manga with dense panels.
	// Falls back to single-stage if enumeration fails or is disabled.
	if usePanelAware {
		items, content, err := s.analyzePagePanelAware(ctx, modelCfg, state, pageIndex, dataURI, maxTokens)
		if err == nil {
			result.Items = items
			result.Content = content
			return result, nil
		}
		log.Printf("[image-analysis] panel-aware failed, falling back to single-stage id=%s page=%d err=%v", state.ID, pageIndex, err)
	}

	// Single-stage path (default for non-panel-aware or as fallback).
	sysContent, _ := json.Marshal(sysPrompt)
	userParts := []contentPart{
		{Type: "text", Text: pageInstruction(pageIndex, img.FileName, state.Intents, prevSummary)},
		{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
	}
	userContent, _ := json.Marshal(userParts)

	reqBody := chatRequest{
		Model:     modelCfg.Model,
		MaxTokens: maxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: sysContent},
			{Role: "user", Content: userContent},
		},
	}

	log.Printf("[image-analysis] analyze page id=%s page=%d file=%q model=%q", state.ID, pageIndex, img.FileName, modelCfg.Model)

	respContent, err := s.callVisionAPI(ctx, modelCfg, reqBody)
	if err != nil {
		return result, fmt.Errorf("视觉模型推理失败: %w", err)
	}
	if strings.TrimSpace(respContent) == "" {
		return result, fmt.Errorf("视觉模型返回为空")
	}

	// Parse JSON response.
	items, content, err := parseModelResponse(respContent)
	if err != nil {
		// If JSON parsing fails, store raw content as fallback.
		result.Content = respContent
		result.Items = []ExtractedItem{{Type: "other", Content: respContent}}
		result.Truncated = looksTruncated(respContent)
		log.Printf("[image-analysis] parse fallback id=%s page=%d truncated=%v err=%v", state.ID, pageIndex, result.Truncated, err)
		return result, nil
	}
	result.Items = items
	result.Content = content
	return result, nil
}

// prevPageSummary returns a bounded summary of the previous page's extraction
// for cross-page continuity. Returns empty string if disabled or unavailable.
func (s *Service) prevPageSummary(state *BatchState, pageIndex int) string {
	maxChars := s.cfg.ImageAnalysisPrevPageSummaryChars()
	if maxChars <= 0 || pageIndex <= 0 {
		return ""
	}
	prev := state.Results[pageIndex-1]
	if prev.Status != "success" || strings.TrimSpace(prev.Content) == "" {
		return ""
	}
	return truncateRunes(prev.Content, maxChars)
}

// analyzePagePanelAware performs two-stage extraction: first enumerate panels
// in reading order, then deep-extract each panel individually. All items are
// merged into a single PageResult. Individual panel failures are recorded as
// "other" items rather than aborting the page.
func (s *Service) analyzePagePanelAware(ctx context.Context, modelCfg visionModelConfig, state *BatchState, pageIndex int, dataURI string, maxTokens int) ([]ExtractedItem, string, error) {
	img := state.Images[pageIndex]

	// Stage 1: enumerate panels.
	enumSys, _ := json.Marshal(panelEnumerationInstruction())
	enumUser, _ := json.Marshal([]contentPart{
		{Type: "text", Text: "请识别本页所有分镜，按阅读顺序输出。"},
		{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
	})
	enumReq := chatRequest{
		Model:     modelCfg.Model,
		MaxTokens: maxTokens,
		Messages: []chatMessage{
			{Role: "system", Content: enumSys},
			{Role: "user", Content: enumUser},
		},
	}
	log.Printf("[image-analysis] panel enumerate id=%s page=%d", state.ID, pageIndex)
	enumResp, err := s.callVisionAPI(ctx, modelCfg, enumReq)
	if err != nil {
		return nil, "", fmt.Errorf("分镜枚举失败: %w", err)
	}
	panels, err := parsePanelEnumeration(enumResp)
	if err != nil || len(panels) == 0 {
		return nil, "", fmt.Errorf("分镜枚举解析失败: %w", err)
	}
	log.Printf("[image-analysis] panel enumerate done id=%s page=%d panels=%d", state.ID, pageIndex, len(panels))

	// Stage 2: per-panel deep extraction (capped to avoid token explosion).
	maxPanels := s.cfg.ImageAnalysisMaxPanelsPerRequest()
	sysContent, _ := json.Marshal(systemInstruction(state.Intents))

	var allItems []ExtractedItem
	var contentSB strings.Builder
	for pi, panel := range panels {
		if pi >= maxPanels {
			log.Printf("[image-analysis] panel cap reached id=%s page=%d cap=%d", state.ID, pageIndex, maxPanels)
			break
		}
		userText := panelExtractionInstruction(pageIndex, img.FileName, state.Intents, panel.Index, panel.Summary)
		log.Printf("[image-analysis] panel extract id=%s page=%d panel=%d/%d", state.ID, pageIndex, pi+1, len(panels))
		items, err := s.extractPanel(ctx, modelCfg, sysContent, userText, dataURI, maxTokens, panel.Index)
		if err != nil {
			log.Printf("[image-analysis] panel extract failed id=%s page=%d panel=%d err=%v", state.ID, pageIndex, pi+1, err)
			allItems = append(allItems, ExtractedItem{Type: "other", Content: fmt.Sprintf("[分镜%d] 提取失败: %v", panel.Index, err)})
			continue
		}
		for i := range items {
			items[i].Content = fmt.Sprintf("[分镜%d] %s", panel.Index, items[i].Content)
		}
		allItems = append(allItems, items...)
		for _, item := range items {
			contentSB.WriteString(fmt.Sprintf("### [分镜%d] %s\n\n%s\n\n", panel.Index, itemTypeLabel(item.Type), item.Content))
		}
	}
	if len(allItems) == 0 {
		return nil, "", fmt.Errorf("分镜提取未产生任何条目")
	}
	return allItems, contentSB.String(), nil
}

// extractPanel 对单个分镜做一次视觉提取。失败（超时/服务端 5xx）时先做一次
// 降级重试：降低 max_tokens 或切换温度，仍失败才返回错误，避免单次抖动直接丢内容。
func (s *Service) extractPanel(ctx context.Context, modelCfg visionModelConfig, sysContent json.RawMessage, userText string, dataURI string, maxTokens int, panelIndex int) ([]ExtractedItem, error) {
	buildReq := func(tokens int, temp *float64) chatRequest {
		userParts := []contentPart{
			{Type: "text", Text: userText},
			{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
		}
		userContent, _ := json.Marshal(userParts)
		reqBody := chatRequest{
			Model:       modelCfg.Model,
			MaxTokens:   tokens,
			Temperature: temp,
			Messages:    []chatMessage{{Role: "system", Content: sysContent}, {Role: "user", Content: userContent}},
		}
		return reqBody
	}

	// 首次尝试：使用配置的 max_tokens。
	resp, err := s.callVisionAPI(ctx, modelCfg, buildReq(maxTokens, modelCfg.Temperature))
	if err != nil {
		log.Printf("[image-analysis] panel extract first try failed, retrying with lower tokens err=%v", err)
		// 降级重试：max_tokens 减半（下限 256），温度调低以稳定输出。
		retryTokens := maxTokens / 2
		if retryTokens < 256 {
			retryTokens = 256
		}
		resp, err = s.callVisionAPI(ctx, modelCfg, buildReq(retryTokens, floatPtr(0.1)))
		if err != nil {
			return nil, fmt.Errorf("分镜提取失败: %w", err)
		}
	}

	items, _, err := parseModelResponse(resp)
	if err != nil {
		// 保留原始内容并带分镜索引，便于追溯。
		return []ExtractedItem{{Type: "other", Content: fmt.Sprintf("[分镜%d] %s", panelIndex, resp)}}, nil
	}
	return items, nil
}

// callVisionAPI sends a chat completion request directly to the OpenAI-compatible
// endpoint and returns the assistant's text content.
func (s *Service) callVisionAPI(ctx context.Context, modelCfg visionModelConfig, reqBody chatRequest) (string, error) {
	// 将 profile 级 temperature 注入请求体（覆盖 llama-server 全局 --temp）。
	if modelCfg.Temperature != nil && reqBody.Temperature == nil {
		reqBody.Temperature = modelCfg.Temperature
	}
	// 本函数按 SSE 流解析；兼容端点仅在 stream=true 时返回 SSE，否则返回
	// 完整 JSON（无 data: 行），解析结果恒为空，因此在此统一强制开启。
	reqBody.Stream = true
	url := strings.TrimRight(modelCfg.BaseURL, "/") + "/chat/completions"
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("序列化请求失败: %w", err)
	}

	// Debug: log the first 500 bytes of the payload to diagnose wire format issues.
	preview := string(payload)
	if len(preview) > 500 {
		preview = preview[:500]
	}
	log.Printf("[image-analysis] request payload preview: %s", preview)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("创建 HTTP 请求失败: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if modelCfg.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+modelCfg.APIKey)
	}

	timeoutSeconds := s.cfg.ImageAnalysisRequestTimeoutSeconds()
	// 不使用 http.Client.Timeout：它覆盖整个响应周期，长生成会被整体切断。
	// 超时语义为空闲超时：相邻数据间隔超过阈值才失败；连接与首字节同样受其
	// 兜底（首个数据到达前不会 Reset）。timeoutSeconds<=0 表示无限等待。
	client := &http.Client{}
	log.Printf("[image-analysis] http idle timeout=%vs url=%q", timeoutSeconds, url)

	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("HTTP 请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("API 返回 %d: %s", resp.StatusCode, string(body))
	}

	// 逐行读取 SSE 放到独立 goroutine，主循环 select 空闲超时；提前退出时
	// cancel 让读 goroutine 随 Body.Close() 的错误返回一起退出，避免泄漏。
	readCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type streamLine struct {
		data string
		err  error
	}
	lines := make(chan streamLine)
	go func() {
		defer close(lines)
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadBytes('\n')
			select {
			case lines <- streamLine{data: string(line), err: err}:
			case <-readCtx.Done():
				return
			}
		}
	}()

	var sb strings.Builder
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	if timeoutSeconds > 0 {
		idleTimer = time.NewTimer(time.Duration(timeoutSeconds) * time.Second)
		defer idleTimer.Stop()
		idleC = idleTimer.C
	}

loop:
	for {
		select {
		case <-idleC:
			return "", fmt.Errorf("空闲超时: %v 内未收到新数据", time.Duration(timeoutSeconds)*time.Second)
		case lr, ok := <-lines:
			if !ok {
				break loop
			}
			if lr.err != nil {
				if errors.Is(lr.err, io.EOF) {
					break loop
				}
				return "", fmt.Errorf("读取 SSE 流失败: %w", lr.err)
			}
			trimmed := strings.TrimSpace(lr.data)
			if !strings.HasPrefix(trimmed, "data:") {
				continue
			}
			data := strings.TrimPrefix(trimmed, "data:")
			data = strings.TrimSpace(data)
			if data == "[DONE]" {
				break loop
			}
			var chunk chatStreamChunk
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				continue
			}
			for _, choice := range chunk.Choices {
				if strings.TrimSpace(choice.Delta.Content) != "" {
					sb.WriteString(choice.Delta.Content)
				}
			}
			if idleTimer != nil {
				if !idleTimer.Stop() {
					select {
					case <-idleTimer.C:
					default:
					}
				}
				idleTimer.Reset(time.Duration(timeoutSeconds) * time.Second)
			}
		}
	}

	content := strings.TrimSpace(sb.String())
	if content == "" {
		return "", fmt.Errorf("视觉模型返回为空")
	}
	return content, nil
}

// modelConfig resolves the vision model endpoint for the image_analysis agent.
// If modelID is non-empty, the specified profile is used instead of the
// agent-level default, allowing per-batch model override from the UI.
func (s *Service) modelConfig(modelID string) visionModelConfig {
	if modelID != "" {
		resolved := config.ResolveProfileModel(s.cfg, modelID)
		log.Printf("[image-analysis] using profile override model_id=%q model=%q temp=%v", modelID, resolved.OpenAIModel, resolved.Temperature)
		return visionModelConfig{
			APIKey:      resolved.OpenAIAPIKey,
			Model:       resolved.OpenAIModel,
			BaseURL:     resolved.OpenAIBaseURL,
			Temperature: resolved.Temperature,
		}
	}
	resolved := config.ResolveAgentModel(s.cfg, config.AgentKindImageAnalysis)
	return visionModelConfig{
		APIKey:      resolved.OpenAIAPIKey,
		Model:       resolved.OpenAIModel,
		BaseURL:     resolved.OpenAIBaseURL,
		Temperature: resolved.Temperature,
	}
}

// readImageData reads the raw image bytes from workspace storage.
func (s *Service) readImageData(img ImageItem) ([]byte, error) {
	path := s.store.workspace + "/" + img.StoragePath
	data, err := readFileBytes(path)
	if err != nil {
		return nil, fmt.Errorf("读取图片文件失败 %s: %w", img.StoragePath, err)
	}
	return data, nil
}

// resizeImage 在发送前对图片做缩放/压缩，降低视觉 token 与 prefill 耗时。
// 超过 maxDim 时等比缩放到 maxDim 以内；JPEG 按 quality 压缩。
// 保留阅读顺序检测所需清晰度：默认 maxDim=1600、quality=82。
func resizeImage(data []byte, mime string, maxDim int, quality int) ([]byte, error) {
	if maxDim <= 0 || quality <= 0 {
		return data, nil
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		// 无法解析元数据（如 WebP）原样返回。
		return data, nil
	}
	maxW, maxH := int(cfg.Width), int(cfg.Height)
	if maxW <= maxDim && maxH <= maxDim {
		return data, nil // 已足够小，无需缩放
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, nil
	}
	scale := math.Min(float64(maxDim)/float64(maxW), float64(maxDim)/float64(maxH))
	newW := int(math.Ceil(float64(maxW) * scale))
	newH := int(math.Ceil(float64(maxH) * scale))

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			sx := int(float64(x) / scale)
			sy := int(float64(y) / scale)
			if sx >= maxW {
				sx = maxW - 1
			}
			if sy >= maxH {
				sy = maxH - 1
			}
			dst.Set(x, y, src.At(sx, sy))
		}
	}

	var out bytes.Buffer
	switch strings.ToLower(format) {
	case "jpeg", "jpg":
		q := quality
		if q > 100 {
			q = 100
		}
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: q}); err != nil {
			return data, fmt.Errorf("压缩 JPEG 失败: %w", err)
		}
	default:
		// PNG/其他格式：尽量转 JPEG 以减小体积。
		if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: quality}); err != nil {
			return data, fmt.Errorf("转码 JPEG 失败: %w", err)
		}
	}
	return out.Bytes(), nil
}

// classifyBookFromState gathers rendered images from the first N image-bearing
// pages and classifies the document's nature. Text-layer-only pages carry no
// image bytes and are skipped. Falls back to BookTypeManga when no images are
// available for sampling.
func (s *Service) classifyBookFromState(ctx context.Context, modelCfg visionModelConfig, state *BatchState) BookType {
	sampleN := s.cfg.ImageAnalysisPDFClassifySamplePages()
	if sampleN <= 0 {
		sampleN = config.DefaultImageAnalysisPDFClassifyPages
	}
	samples := make([][]byte, 0, sampleN)
	for i := range state.Images {
		if len(samples) >= sampleN {
			break
		}
		img := state.Images[i]
		if img.Text != "" {
			continue // pure text page, no rendered image
		}
		data, err := s.readImageData(img)
		if err != nil {
			log.Printf("[image-analysis] classify sample read failed id=%s page=%d err=%v", state.ID, i, err)
			continue
		}
		samples = append(samples, data)
	}
	if len(samples) == 0 {
		return BookTypeManga
	}
	return s.classifyBook(ctx, modelCfg, samples)
}

// parseModelResponse extracts structured items from the model's JSON output.
// It tries multiple extraction strategies and tolerates trailing commas.
func parseModelResponse(content string) ([]ExtractedItem, string, error) {
	jsonStr := extractJSON(content)
	if jsonStr == "" {
		return nil, "", fmt.Errorf("未找到 JSON 内容")
	}

	var parsed struct {
		Items []ExtractedItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		// Retry after stripping trailing commas (common LLM output quirk).
		cleaned := stripTrailingCommas(jsonStr)
		if err2 := json.Unmarshal([]byte(cleaned), &parsed); err2 != nil {
			return nil, "", fmt.Errorf("解析模型 JSON 输出失败: %w", err)
		}
	}
	// Filter thinking-chain items before they enter state.json.
	parsed.Items = filterThoughtItems(parsed.Items)

	if len(parsed.Items) == 0 {
		return nil, "", fmt.Errorf("模型未返回任何提取条目")
	}

	// Build a combined content string from all items.
	var sb strings.Builder
	for _, item := range parsed.Items {
		sb.WriteString(fmt.Sprintf("### %s\n\n%s\n\n", itemTypeLabel(item.Type), item.Content))
	}
	return parsed.Items, sb.String(), nil
}

// filterThoughtItems removes items that are model thinking-chain leakage.
// Uses two strategies:
//  1. String markers (isModelThought) — fast, covers most known patterns.
//  2. Structural outlier detection — a single item that is >4x the median
//     length of other items on the same page AND contains self-referential
//     patterns is almost certainly leaked reasoning.
func filterThoughtItems(items []ExtractedItem) []ExtractedItem {
	if len(items) <= 2 {
		// With 2 or fewer items, only use marker detection.
		filtered := make([]ExtractedItem, 0, len(items))
		for _, item := range items {
			if isModelThought(item.Content) {
				continue
			}
			filtered = append(filtered, item)
		}
		return filtered
	}

	// Compute median content length for outlier detection.
	lengths := make([]int, len(items))
	for i, item := range items {
		lengths[i] = len(item.Content)
	}
	sort.Ints(lengths)
	medianLen := lengths[len(lengths)/2]
	// Outlier threshold: 4× median OR >2000 chars (thinking chains are verbose).
	if medianLen < 256 {
		medianLen = 256 // floor: avoid false positives on short pages
	}
	outlierThreshold := medianLen * 4
	if outlierThreshold > 8000 {
		outlierThreshold = 8000 // cap: don't flag legit long descriptions
	}

	filtered := make([]ExtractedItem, 0, len(items))
	for _, item := range items {
		// Strategy 1: string markers.
		if isModelThought(item.Content) {
			continue
		}
		// Strategy 2: outlier + self-referential patterns.
		if len(item.Content) > outlierThreshold {
			if isSelfReferential(item.Content) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// isSelfReferential returns true if content looks like the model is describing
// its own analysis process rather than actual manga content.
func isSelfReferential(content string) bool {
	// Count first-person / task-oriented markers.
	hits := 0
	for _, m := range selfReferentialMarkers {
		if strings.Contains(content, m) {
			hits++
		}
	}
	// Require at least 2 hits to reduce false positives on legit analysis.
	return hits >= 2
}

// selfReferentialMarkers are short phrases that appear in model thinking chains
// but are extremely unlikely in legitimate manga content analysis.
var selfReferentialMarkers = []string{
	"我需要",
	"让我",
	"让我们",
	"观察图片",
	"定位到",
	"用户要求",
	"按照阅读顺序",
	"划分分镜",
	"这个任务",
	"首先，",
	"现在，我",
	"接下来",
	"等等",
	"重新定义",
	"通常",
	"仔细看",
}

// panelSummary is a single panel entry from stage-1 enumeration.
type panelSummary struct {
	Index       int    `json:"index"`
	Summary     string `json:"summary"`
	HasDialogue bool   `json:"has_dialogue"`
}

// parsePanelEnumeration parses the stage-1 panel list response.
func parsePanelEnumeration(content string) ([]panelSummary, error) {
	jsonStr := extractJSON(content)
	if jsonStr == "" {
		return nil, fmt.Errorf("未找到 JSON")
	}
	var parsed struct {
		Panels []panelSummary `json:"panels"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		cleaned := stripTrailingCommas(jsonStr)
		if err2 := json.Unmarshal([]byte(cleaned), &parsed); err2 != nil {
			return nil, fmt.Errorf("解析分镜列表失败: %w", err)
		}
	}
	return parsed.Panels, nil
}

// extractJSON tries to locate the JSON object in the model response using
// multiple strategies: direct JSON, ```json code block, generic code block,
// and first-brace-to-last-brace substring.
func extractJSON(content string) string {
	trimmed := strings.TrimSpace(content)
	// Strategy 1: direct JSON (starts with {).
	if strings.HasPrefix(trimmed, "{") {
		return trimmed
	}
	// Strategy 2: ```json code block.
	if idx := strings.Index(content, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(content[start:], "```"); end >= 0 {
			return strings.TrimSpace(content[start : start+end])
		}
	}
	// Strategy 3: generic ``` code block.
	if idx := strings.Index(content, "```"); idx >= 0 {
		start := idx + len("```")
		if end := strings.Index(content[start:], "```"); end >= 0 {
			return strings.TrimSpace(content[start : start+end])
		}
	}
	// Strategy 4: first { to last }.
	first := strings.Index(content, "{")
	last := strings.LastIndex(content, "}")
	if first >= 0 && last > first {
		return content[first : last+1]
	}
	return ""
}

// stripTrailingCommas removes trailing commas before } and ] (common LLM quirk).
func stripTrailingCommas(s string) string {
	s = strings.ReplaceAll(s, ",}", "}")
	s = strings.ReplaceAll(s, ",]", "]")
	return s
}

// looksTruncated heuristically detects a likely-cut-off JSON response:
// it opens with { but does not close with }.
func looksTruncated(content string) bool {
	trimmed := strings.TrimSpace(content)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	return !strings.HasSuffix(trimmed, "}")
}

// aggregateSection builds a markdown section for one intent from all extracted items.
func aggregateSection(intent AnalysisIntent, state *BatchState, items []ExtractedItem) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## %s\n\n", intentTitle(intent)))
	sb.WriteString(fmt.Sprintf("*来源：批次 %s，共 %d 页，成功 %d 页*\n\n", state.ID, state.TotalPages, countSuccess(state)))

	// Filter items relevant to this intent (all items contribute to all intents
	// in MVP; future versions can filter by intent-specific types).
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("- **%s**：%s\n", itemTypeLabel(item.Type), truncateRunes(item.Content, 200)))
	}
	return sb.String()
}

func countSuccess(state *BatchState) int {
	n := 0
	for _, r := range state.Results {
		if r.Status == "success" {
			n++
		}
	}
	return n
}

func intentTitle(intent AnalysisIntent) string {
	switch intent {
	case IntentOutline:
		return "大纲提取"
	case IntentProgress:
		return "进度分析"
	case IntentInspiration:
		return "灵感收集"
	case IntentState:
		return "角色状态"
	case IntentLore:
		return "资料库设定"
	default:
		return string(intent)
	}
}

func itemTypeLabel(t string) string {
	switch t {
	case "character":
		return "角色"
	case "scene":
		return "场景"
	case "plot_point":
		return "剧情点"
	case "world_building":
		return "世界观"
	case "dialogue":
		return "对话"
	default:
		return "其他"
	}
}

// buildFactStream 按页序拼接完整事实流，不截断内容，保留页锚点和阅读序。
// 供跨页整合模型（如 DeepSeek）重建时间线和角色追踪。
func buildFactStream(state *BatchState) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 事实流 — 批次 %s，共 %d 页，成功 %d 页\n\n", state.ID, state.TotalPages, countSuccess(state)))

	// 按 PageIndex 排序（Results 通常已有序，显式 sort 更稳）
	sorted := make([]PageResult, len(state.Results))
	copy(sorted, state.Results)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].PageIndex < sorted[j].PageIndex
	})

	for _, r := range sorted {
		if r.Status != "success" {
			continue
		}
		sb.WriteString(fmt.Sprintf("## 第 %d 页（%s）\n", r.PageIndex+1, r.FileName))
		for _, item := range r.Items {
			// Skip items whose content is raw, unparsed JSON (model returned
			// malformed JSON that parseModelResponse couldn't handle).
			if isRawJSON(item.Content) {
				continue
			}
			// Skip items that are purely onomatopoeia / sound-effect labels
			// with no analytical value (e.g. "[拟声词]：xxx").
			if isSoundEffectItem(item.Content) {
				continue
			}
			// Skip items that are model internal reasoning leaked into
			// content (parse failure fallback dumped the raw response).
			if isModelThought(item.Content) {
				continue
			}
			// Skip empty dialogue items (model hallucination loops).
			if isEmptyDialogue(item.Content) {
				continue
			}
			content := dedupOnomatopoeia(item.Content, 3)
			if content == "" {
				continue
			}
			sb.WriteString(fmt.Sprintf("- [%s] %s\n", itemTypeLabel(item.Type), content))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// isSoundEffectItem returns true if the entire item content is a single
// onomatopoeia/sound-effect line with no analytical value.
func isSoundEffectItem(content string) bool {
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "[拟声词]") ||
		strings.HasPrefix(trimmed, "[擬声語]") ||
		strings.HasPrefix(trimmed, "[音效]") ||
		strings.HasPrefix(trimmed, "[音效/拟声]") ||
		strings.HasPrefix(trimmed, "[音效/擬声]") {
		return true
	}
	// Also catch items where the content is a single sound-tag line.
	if soundEffectLineRE.MatchString(trimmed) {
		return true
	}
	// Standalone onomatopoeia line (pure kana, no brackets).
	return isOnomatopoeiaLine(trimmed)
}

// soundEffectLineRE matches lines that are model-extracted sound-effect
// annotations: optional [分镜N] / [角色名] prefix, then a [拟声词]： or
// [音效]： tag. These lines are always noise with no analytical value.
var soundEffectLineRE = regexp.MustCompile(`^(\[[^\]]+\]\s*)*\[(?:拟声词|擬声語|音效|擬音|音效[/／]拟声|音效[/／]擬声)\][：:]`)

// stripSoundEffectLines removes lines that are purely model-extracted sound
// effects (e.g. "[音效]：ミッ！", "[分镜5] [拟声词]：グッ") from content
// blocks before dedup.
func stripSoundEffectLines(content string) string {
	lines := strings.Split(content, "\n")
	var kept []string
	for _, line := range lines {
		if soundEffectLineRE.MatchString(strings.TrimSpace(line)) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// isRawJSON returns true if content looks like unparsed JSON from the model
// (starts with `{`, “ ```json “, or contains only a JSON object/array).
// These items represent parse failures that should not appear in output.
func isRawJSON(content string) bool {
	trimmed := strings.TrimSpace(content)
	// Fenced JSON block anywhere in content.
	if strings.Contains(trimmed, "```json") {
		return true
	}
	// JSON object with "items" key.
	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed, `"items"`) {
		return true
	}
	return false
}

// modelThoughtMarkers are Chinese phrases that indicate model internal
// reasoning rather than actual analysis content. When parseModelResponse fails,
// the raw response (including chain-of-thought) is stored as a fallback item.
// These markers are extremely unlikely to appear in legitimate manga analysis.
var modelThoughtMarkers = []string{
	// Self-direction / reconsideration patterns.
	"等等，再",
	"让我重新梳理",
	"让我重新",
	"让我再",
	"让我更仔细",
	"让我们假设",
	"让我们重新",
	"让我们尝试",
	"让我们再",
	"让我们仔细",
	"让我们按",
	// Prompt instruction artifacts.
	"提取内容",
	"提取要求",
	"提取台词",
	"备注：",
	// Rule-reading patterns (model reading its own instructions aloud).
	"根据规则",
	"非台词",
	"如果是拟声词则忽略",
	"不提取为",
	// Model thinking-chain leakage (model describing its own analysis process
	// instead of actual manga content). These are distinct from narrative
	// analysis because they speak in first-person about the task itself.
	"用户要求我",
	"我需要先确定",
	"我需要先",
	"我需要仔细",
	"我需要更",
	"观察图片结构",
	"我们来分析",
	"请分析",
	"请提取",
	"定位到",
	// Procedural thinking about layout panels.
	"按照标准的日式漫画阅读顺序",
	"按照漫画的阅读顺序",
	"按照阅读顺序",
	"尝试划分分镜",
	"划分分镜",
	"重新定义",
	"通常阅读顺序",
	"让我来",
	"如果我",
	// First-person task framing (model talking ABOUT doing the task).
	"这个任务涉及",
	"这个任务",
	"首先，我要",
	"现在，我",
}

// isModelThought returns true when content looks like model internal reasoning
// that leaked into output due to parse failure.
func isModelThought(content string) bool {
	for _, m := range modelThoughtMarkers {
		if strings.Contains(content, m) {
			return true
		}
	}
	return false
}

// inlineOnomatopoeiaRE matches inline onomatopoeia / sound-effect annotations
// embedded within content lines, e.g. [拟声词：'ぱっ!'], [音效："ズン"],
// （拟声词：あみみみんっっ！）, and tag-only patterns like [拟声词]："xxx".
var inlineOnomatopoeiaRE = regexp.MustCompile(`[\[（\(]\s*(?:拟声词|擬声語|音效|擬音|环境拟声词)\s*[\]）\)]?\s*[：:]\s*['"「][^'"」]*['"」]`)

// isNoiseItem returns true for items whose display content is entirely
// onomatopoeia / sound effects (no semantic value after deduplication),
// or whose dialogue content is empty (model hallucination).
func isNoiseItem(item ExtractedItem) bool {
	cleaned := dedupOnomatopoeia(item.Content, 3)
	if len(strings.TrimSpace(cleaned)) == 0 {
		return true
	}
	// Filter empty dialogue items: [角色]："" or [角色]： with nothing after.
	if isEmptyDialogue(item.Content) {
		return true
	}
	return false
}

// dialogueEmptyRE matches dialogue lines that have a speaker label but no
// actual spoken content — model hallucination loop producing empty utterances.
// Examples: [电视中的模糊人影]："", [角色]：, [角色]："  "
var dialogueEmptyRE = regexp.MustCompile(`^\[[^\]]+\][：:]\s*(?:["'」「『]?\s*["'」』]?)?\s*$`)

// speakerPrefixRE matches a speaker label prefix like [角色名]： at the start
// of a line, used to normalize content for dedup comparison.
var speakerPrefixRE = regexp.MustCompile(`^\[[^\]]+\][：:]\s*`)

// normalizeForDedup strips speaker labels and normalizes whitespace for
// comparison. This is NOT applied to stored content — only used to determine
// whether two items are "the same" for dedup purposes.
func normalizeForDedup(content string) string {
	s := speakerPrefixRE.ReplaceAllString(content, "")
	s = strings.TrimSpace(s)
	// Collapse runs of whitespace.
	s = strings.Join(strings.Fields(s), " ")
	// Normalize heart variants and fullwidth punctuation.
	s = strings.ReplaceAll(s, "♥", "♡")
	s = strings.ReplaceAll(s, "！", "!")
	s = strings.ReplaceAll(s, "？", "?")
	return s
}

// isShortNoiseContent returns true when the content, after stripping speaker
// labels, contains fewer than minRunes meaningful characters. Empty strings
// and pure-punctuation are always noise.
func isShortNoiseContent(content string, minRunes int) bool {
	cleaned := normalizeForDedup(content)
	if cleaned == "" {
		return true
	}
	// Count meaningful runes (letters, CJK, digits — not punctuation/emoji).
	count := 0
	for _, r := range cleaned {
		if r > 127 || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			count++
		}
	}
	return count < minRunes
}

// isEmptyDialogue returns true when the entire content is just a labeled
// empty dialogue with no actual spoken words.
func isEmptyDialogue(content string) bool {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return false
	}
	if dialogueEmptyRE.MatchString(trimmed) {
		return true
	}
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= 1 {
		return false
	}
	nonEmpty := 0
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !dialogueEmptyRE.MatchString(strings.TrimSpace(line)) {
			nonEmpty++
		}
	}
	return nonEmpty == 0
}

// dedupConsecutiveDuplicates is a general-purpose run-length cap for model
// hallucination loops. Consecutive items whose normalized content is identical
// are capped at maxRun. Short-noise items (e.g. bare onomatopoeia, empty
// dialogues) use a stricter cap. This replaces the earlier pattern-specific
// dedupConsecutiveEmpty with a universal strategy.
func dedupConsecutiveDuplicates(items []ExtractedItem) []ExtractedItem {
	if len(items) <= 1 {
		return items
	}

	const (
		normalMaxRun  = 5 // cap for normal content
		shortMaxRun   = 2 // stricter cap for short-noise (empty, bare onomatopoeia)
		shortMinRunes = 3 // fewer than 3 CJK/ASCII letters → short noise
	)

	var result []ExtractedItem
	for _, item := range items {
		norm := normalizeForDedup(item.Content)
		isShort := isShortNoiseContent(item.Content, shortMinRunes)

		maxRun := normalMaxRun
		if isShort || isEmptyDialogue(item.Content) {
			maxRun = shortMaxRun
		}

		// Count how many consecutive items with the same normalized content
		// already appear at the tail of result.
		run := 0
		for i := len(result) - 1; i >= 0; i-- {
			if normalizeForDedup(result[i].Content) != norm {
				break
			}
			run++
		}
		if run < maxRun {
			result = append(result, item)
		}
	}
	return result
}

// dedupOnomatopoeia 按行拆分内容，识别拟声词行并做次数去重。
// 同时清除行内拟声词标注（如 [拟声词：'ぱっ!']）。
// 对疑似拟声词的行（以"拟声词："或"音效："开头，或全假名），
// 如果同一行重复超过 repeatLimit 次，只保留首次出现。
// repeatLimit 为 0 时不做去重。
func dedupOnomatopoeia(content string, repeatLimit int) string {
	// Step 0: strip inline onomatopoeia annotations first.
	content = inlineOnomatopoeiaRE.ReplaceAllString(content, "")
	// Clean up separators that become dangling after stripping.
	content = strings.ReplaceAll(content, "、，", "，")
	content = strings.ReplaceAll(content, "，，", "，")
	content = strings.ReplaceAll(content, "\n、", "\n")
	content = strings.ReplaceAll(content, " 、", " ")
	// Strip lines that are purely [音效]：xxx or [音效/拟声]：xxx with no
	// analytical value — these are model-extracted sound effects leaking
	// into content blocks.
	content = stripSoundEffectLines(content)

	if repeatLimit <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	type lineMeta struct {
		text        string
		isOnomato   bool
		occurrences int // count so far (before this one)
	}
	metas := make([]lineMeta, len(lines))
	seen := make(map[string]int) // normalized -> count

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		norm := strings.TrimRight(trimmed, "♡♥!?‼⁉☆★◆◇○● 　\t")
		isOno := isOnomatopoeiaLine(trimmed)
		metas[i] = lineMeta{text: line, isOnomato: isOno}
		if isOno {
			if c, ok := seen[norm]; ok {
				metas[i].occurrences = c
			}
			seen[norm]++
		}
	}

	var kept []string
	for _, m := range metas {
		if m.isOnomato && m.occurrences >= repeatLimit {
			continue // 重复过多，跳过
		}
		kept = append(kept, m.text)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// isOnomatopoeiaLine 判断单行是否为拟声词/音效行。
func isOnomatopoeiaLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	// 显式标注行（无括号格式）
	if strings.HasPrefix(trimmed, "拟声词：") || strings.HasPrefix(trimmed, "音效：") ||
		strings.HasPrefix(trimmed, "擬声語：") || strings.HasPrefix(trimmed, "擬音：") {
		return true
	}
	// 带括号的标注行，如 [拟声词]：xxx, [音效]：xxx, [音效/拟声]：xxx
	if strings.HasPrefix(trimmed, "[拟声词]") || strings.HasPrefix(trimmed, "[擬声語]") ||
		strings.HasPrefix(trimmed, "[音效]") || strings.HasPrefix(trimmed, "[擬音]") {
		return true
	}
	// 引用包裹的标注行 "拟声词：\"xxx\""
	if strings.HasPrefix(trimmed, "\"拟声词：") || strings.HasPrefix(trimmed, "\"音效：") {
		return true
	}
	// 纯假名行（去引号后）
	cleaned := strings.Trim(trimmed, "\"」「『』\"' ")
	return isPureOnomatopoeia(cleaned)
}

// isPureOnomatopoeia checks if a string is composed entirely of kana
// characters, small-tsu (っ/ッ), long-vowel marks (ー), punctuation marks
// commonly used in manga SFX, and whitespace — indicating it's likely a sound
// effect rather than meaningful dialogue or description.
func isPureOnomatopoeia(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) == 0 {
		return false
	}
	hasKana := false
	for _, r := range s {
		switch {
		case r >= 0x3040 && r <= 0x309F: // hiragana
			hasKana = true
		case r >= 0x30A0 && r <= 0x30FF: // katakana
			hasKana = true
		case r == 0x30FC: // ー (long vowel mark)
		case r == 0x3000 || r == 0x0020: // fullwidth or ASCII space
		case r == '\n' || r == '\r' || r == '\t':
		case r == '…' || r == '〜' || r == '♡' || r == '♥':
		case r == '!' || r == '?' || r == '‼' || r == '⁉':
		case r == '☆' || r == '★' || r == '◆' || r == '◇' || r == '○' || r == '●':
		default:
			// Contains non-kana character — likely real content.
			return false
		}
	}
	return hasKana
}
