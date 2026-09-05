package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"denova/config"
	"denova/internal/agent"
	"denova/internal/imageanalysis"
)

// ImageAnalysisAppService is a thin facade over the live App for batch image
// analysis operations.
type ImageAnalysisAppService struct {
	app *App
}

func (a *App) ImageAnalysis() *ImageAnalysisAppService {
	return &ImageAnalysisAppService{app: a}
}

// activeImageAnalysisBatches tracks running batch tasks so SSE clients can
// reconnect and abort works.
var (
	imageAnalysisMu      sync.Mutex
	imageAnalysisBatches = make(map[string]*Task)
)

// StartImageAnalysisBatch stores uploaded images, creates a batch state, and
// launches a background Task that processes pages sequentially.
func (s *ImageAnalysisAppService) StartBatch(ctx context.Context, images []imageanalysis.UploadedImage, intents []imageanalysis.AnalysisIntent, modelID string) (*Task, *imageanalysis.BatchState, error) {
	if len(images) == 0 {
		return nil, nil, fmt.Errorf("没有上传图片")
	}
	if len(intents) == 0 {
		intents = imageanalysis.AllIntents()
	}
	workspace := s.app.Workspace()
	cfg := s.app.cfg

	batchID := newBatchID()
	svc := imageanalysis.NewService(cfg, workspace)
	store := svc.Store()

	// 惰性清扫：新建批次时顺带回收超过保留期的历史批次图片（无后台常驻
	// goroutine）。retention=0 时内部直接跳过。
	if n := store.SweepExpiredUploads(time.Duration(cfg.ImageAnalysisImageRetentionMinutes()) * time.Minute); n > 0 {
		log.Printf("[image-analysis] swept %d expired batch(es) on start", n)
	}

	// Sort images by natural filename order and assign page indices.
	sorted := imageanalysis.SortByNaturalOrder(images)
	items := make([]imageanalysis.ImageItem, len(sorted))
	for i, img := range sorted {
		mime := img.MIMEType
		if mime == "" {
			mime = "image/png"
		}
		items[i] = imageanalysis.ImageItem{
			PageIndex:   i,
			FileName:    img.FileName,
			StoragePath: fmt.Sprintf("assets/uploads/batch-%s/%03d_%s", batchID, i, img.FileName),
			MIMEType:    mime,
			SizeBytes:   int64(len(img.Data)),
			Text:        img.Text,
		}
	}

	state := imageanalysis.NewBatchState(batchID, items, intents)
	state.ModelID = modelID
	// Mark the batch as PDF-sourced if any page came from PDF expansion, so the
	// pipeline runs book classification before per-page extraction.
	for _, img := range sorted {
		if img.IsPDF {
			state.IsPDF = true
			break
		}
	}
	if err := store.Save(state); err != nil {
		return nil, nil, fmt.Errorf("保存批次状态失败: %w", err)
	}

	// Persist uploaded images to workspace storage.
	// Text-layer pages carry no image bytes, so skip writing a file for them.
	for i, img := range sorted {
		if len(img.Data) == 0 {
			continue
		}
		if err := store.SaveImage(batchID, items[i], img.Data); err != nil {
			return nil, nil, fmt.Errorf("保存图片 %s 失败: %w", img.FileName, err)
		}
	}

	log.Printf("[image-analysis] batch created id=%s pages=%d intents=%v model_id=%q workspace=%q", batchID, len(items), intents, modelID, workspace)

	task := NewTask(func(taskCtx context.Context, task *Task, emit func(agent.Event)) {
		imageAnalysisMu.Lock()
		imageAnalysisBatches[batchID] = task
		imageAnalysisMu.Unlock()
		defer func() {
			imageAnalysisMu.Lock()
			delete(imageAnalysisBatches, batchID)
			imageAnalysisMu.Unlock()
		}()

		err := svc.ProcessBatch(taskCtx, state, func(progress imageanalysis.ProgressEvent) {
			emit(agent.Event{Type: "image_analysis_progress", Data: progress})
		})
		if err != nil {
			log.Printf("[image-analysis] batch aborted/paused id=%s err=%v", batchID, err)
			// Aggregate partial results on abort/pause — save whatever pages completed.
			saveBatchResult(store, svc, state, batchID, emit)
			return
		}

		// Aggregate and persist result.
		saveBatchResult(store, svc, state, batchID, emit)
	})

	return task, state, nil
}

// GetBatchState loads the persisted state for a batch.
func (s *ImageAnalysisAppService) GetBatchState(batchID string) (*imageanalysis.BatchState, error) {
	store := imageanalysis.NewStore(s.app.Workspace())
	return store.Load(batchID)
}

// GetBatchResult loads state and aggregates the result.
func (s *ImageAnalysisAppService) GetBatchResult(batchID string) (*imageanalysis.BatchResult, error) {
	workspace := s.app.Workspace()
	store := imageanalysis.NewStore(workspace)
	state, err := store.Load(batchID)
	if err != nil {
		return nil, err
	}
	svc := imageanalysis.NewService(s.app.cfg, workspace)
	return svc.Aggregate(state), nil
}

// RetryBatch re-processes failed pages in a batch.
func (s *ImageAnalysisAppService) RetryBatch(ctx context.Context, batchID string, pageIndices []int) (*Task, error) {
	svc := imageanalysis.NewService(s.app.cfg, s.app.Workspace())
	store := svc.Store()
	state, err := store.Load(batchID)
	if err != nil {
		return nil, err
	}

	task := NewTask(func(taskCtx context.Context, task *Task, emit func(agent.Event)) {
		imageAnalysisMu.Lock()
		imageAnalysisBatches[batchID] = task
		imageAnalysisMu.Unlock()
		defer func() {
			imageAnalysisMu.Lock()
			delete(imageAnalysisBatches, batchID)
			imageAnalysisMu.Unlock()
		}()

		err := svc.RetryPages(taskCtx, batchID, pageIndices, func(progress imageanalysis.ProgressEvent) {
			emit(agent.Event{Type: "image_analysis_progress", Data: progress})
		})
		if err != nil {
			log.Printf("[image-analysis] retry aborted/paused id=%s err=%v", batchID, err)
			// Reload state — RetryPages may have updated it before context cancelled.
			if reloaded, loadErr := store.Load(batchID); loadErr == nil {
				saveBatchResult(store, svc, reloaded, batchID, emit)
			}
			return
		}
		saveBatchResult(store, svc, state, batchID, emit)
	})
	return task, nil
}

// ReExtractBatch creates a new batch reusing the uploaded images from an
// existing batch, but with a different model. Returns the new batch state.
func (s *ImageAnalysisAppService) ReExtractBatch(ctx context.Context, sourceBatchID, newModelID string) (*Task, *imageanalysis.BatchState, error) {
	workspace := s.app.Workspace()
	store := imageanalysis.NewStore(workspace)

	// Load the source batch to get its images and intents.
	sourceState, err := store.Load(sourceBatchID)
	if err != nil {
		return nil, nil, fmt.Errorf("加载原批次失败: %w", err)
	}

	// Read image data from workspace storage.
	images := make([]imageanalysis.UploadedImage, len(sourceState.Images))
	for i, img := range sourceState.Images {
		imgPath := filepath.Join(workspace, img.StoragePath)
		data, err := os.ReadFile(imgPath)
		if err != nil {
			return nil, nil, fmt.Errorf("读取图片 %s 失败: %w", img.FileName, err)
		}
		images[i] = imageanalysis.UploadedImage{
			FileName: img.FileName,
			Data:     data,
			MIMEType: img.MIMEType,
		}
	}

	log.Printf("[image-analysis] re-extract source=%s pages=%d new_model=%q", sourceBatchID, len(images), newModelID)
	return s.StartBatch(ctx, images, sourceState.Intents, newModelID)
}

// CleanupBatchImages 手动删除指定批次的原始上传图片并落 images_deleted 标记，
// 供用户在确认不再重测/重试后主动回收磁盘空间。运行中的批次拒绝清理。
func (s *ImageAnalysisAppService) CleanupBatchImages(batchID string) error {
	store := imageanalysis.NewStore(s.app.Workspace())
	state, err := store.Load(batchID)
	if err != nil {
		return err
	}
	if state.Status == imageanalysis.BatchRunning || state.Status == imageanalysis.BatchPending {
		return fmt.Errorf("批次正在处理中，无法清理图片")
	}
	if err := store.DeleteUploadedImages(batchID); err != nil {
		return err
	}
	state.ImagesDeleted = true
	if err := store.Save(state); err != nil {
		return fmt.Errorf("写入 images_deleted 标记失败: %w", err)
	}
	log.Printf("[image-analysis] images cleaned up manually id=%s", batchID)
	return nil
}

// AbortBatch cancels a running batch task. If the batch is in "running" state
// on disk but has no in-memory task (e.g. after a crash + restart), it marks
// the batch as failed directly so the user isn't stuck.
func (s *ImageAnalysisAppService) AbortBatch(batchID string) bool {
	imageAnalysisMu.Lock()
	task, ok := imageAnalysisBatches[batchID]
	imageAnalysisMu.Unlock()
	if ok {
		task.Abort()
		return true
	}

	// No in-memory task — check if the batch is stale (running/pending on disk
	// but no task driving it). Mark it as failed so the UI can recover.
	store := imageanalysis.NewStore(s.app.Workspace())
	state, err := store.Load(batchID)
	if err != nil {
		return false
	}
	if state.Status != imageanalysis.BatchRunning && state.Status != imageanalysis.BatchPending {
		return false
	}
	now := time.Now().UTC()
	state.Status = imageanalysis.BatchFailed
	state.FinishedAt = &now
	for i := range state.Results {
		if state.Results[i].Status == "processing" || state.Results[i].Status == "pending" {
			state.Results[i].Status = "failed"
			state.Results[i].Error = "用户中止（服务重启后无运行中任务）"
		}
	}
	if err := store.Save(state); err != nil {
		log.Printf("[image-analysis] abort stale batch save failed id=%s err=%v", batchID, err)
		return false
	}
	log.Printf("[image-analysis] aborted stale batch id=%s (no in-memory task)", batchID)
	return true
}

// ActiveBatchTask returns the running task for a batch, if any.
func (s *ImageAnalysisAppService) ActiveBatchTask(batchID string) (*Task, bool) {
	imageAnalysisMu.Lock()
	defer imageAnalysisMu.Unlock()
	task, ok := imageAnalysisBatches[batchID]
	return task, ok
}

// saveBatchResult aggregates the current batch state and persists result.md and
// fact_stream.md. Used on both normal completion and abort/pause to save partial
// results from whatever pages have been processed so far.
func saveBatchResult(store *imageanalysis.Store, svc *imageanalysis.Service, state *imageanalysis.BatchState, batchID string, emit func(agent.Event)) {
	result := svc.Aggregate(state)
	markdown := buildResultMarkdown(result)
	if saveErr := store.SaveResult(batchID, markdown); saveErr != nil {
		log.Printf("[image-analysis] save result failed id=%s err=%v", batchID, saveErr)
	}
	if saveErr := store.SaveFactStream(batchID, result.FactStream); saveErr != nil {
		log.Printf("[image-analysis] save fact stream failed id=%s err=%v", batchID, saveErr)
	}
	emit(agent.Event{Type: "image_analysis_done", Data: result})
}

// ListModels returns available model profiles for image analysis model selection.
func (s *ImageAnalysisAppService) ListModels() []config.ModelProfileOption {
	return config.ListModelProfiles(s.app.cfg)
}

// ListRecentBatches returns the most recent persisted batch states, newest first.
// Used by the frontend to restore panel state after a page refresh.
func (s *ImageAnalysisAppService) ListRecentBatches(limit int) ([]*imageanalysis.BatchState, error) {
	store := imageanalysis.NewStore(s.app.Workspace())
	return store.ListRecent(limit)
}

func newBatchID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// buildResultMarkdown renders the aggregated result as a single markdown document.
func buildResultMarkdown(result *imageanalysis.BatchResult) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# 图片分析结果\n\n"))
	sb.WriteString(fmt.Sprintf("批次 ID：%s\n状态：%s\n总页数：%d，成功：%d，失败：%d\n\n",
		result.BatchID, result.Status, result.TotalPages, result.SuccessCount, result.FailedCount))
	for _, intent := range []imageanalysis.AnalysisIntent{
		imageanalysis.IntentOutline,
		imageanalysis.IntentProgress,
		imageanalysis.IntentInspiration,
		imageanalysis.IntentState,
		imageanalysis.IntentLore,
	} {
		if section, ok := result.Sections[intent]; ok {
			sb.WriteString(section)
			sb.WriteString("\n")
		}
	}
	if len(result.FailedPages) > 0 {
		sb.WriteString("## 失败页面\n\n")
		for _, p := range result.FailedPages {
			sb.WriteString(fmt.Sprintf("- 第 %d 页（%s）：%s\n", p.PageIndex+1, p.FileName, p.Error))
		}
	}
	return sb.String()
}

// MergeBatches merges the results of multiple source batch states into a new
// merged batch. Deduplicates pages by file name — the first source batch in the
// list that provides a result for a given file wins. Overlapping pages between
// batches are deduplicated automatically.
//
// Returns the newly created merged batch state. The caller can use GetBatchResult
// to retrieve the aggregated markdown result afterward.
func (s *ImageAnalysisAppService) MergeBatches(sourceIDs []string) (*imageanalysis.BatchState, error) {
	if len(sourceIDs) < 2 {
		return nil, fmt.Errorf("至少需要两个源批次才能合并")
	}

	workspace := s.app.Workspace()
	store := imageanalysis.NewStore(workspace)

	// Load all source states.
	states := make([]*imageanalysis.BatchState, 0, len(sourceIDs))
	for _, id := range sourceIDs {
		state, err := store.Load(id)
		if err != nil {
			return nil, fmt.Errorf("加载源批次 %s 失败: %w", id, err)
		}
		states = append(states, state)
	}

	// Collect intents from the first non-empty source.
	intents := states[0].Intents

	// Generate a new batch ID.
	mergedID := newBatchID()

	// Merge.
	merged, err := imageanalysis.MergeBatches(states, mergedID, intents)
	if err != nil {
		return nil, fmt.Errorf("合并批次失败: %w", err)
	}

	// Save the merged state.
	if err := store.Save(merged); err != nil {
		return nil, fmt.Errorf("保存合并批次状态失败: %w", err)
	}

	// Aggregate and persist result files.
	svc := imageanalysis.NewService(s.app.cfg, workspace)
	result := svc.Aggregate(merged)
	markdown := buildResultMarkdown(result)
	if saveErr := store.SaveResult(mergedID, markdown); saveErr != nil {
		log.Printf("[image-analysis] merge save result failed id=%s err=%v", mergedID, saveErr)
	}
	if saveErr := store.SaveFactStream(mergedID, result.FactStream); saveErr != nil {
		log.Printf("[image-analysis] merge save fact stream failed id=%s err=%v", mergedID, saveErr)
	}

	log.Printf("[image-analysis] merge completed id=%s sources=%v pages=%d success=%d failed=%d",
		mergedID, sourceIDs, merged.TotalPages, result.SuccessCount, result.FailedCount)

	return merged, nil
}
