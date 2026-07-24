package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"

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
	imageAnalysisMu     sync.Mutex
	imageAnalysisBatches = make(map[string]*Task)
)

// StartImageAnalysisBatch stores uploaded images, creates a batch state, and
// launches a background Task that processes pages sequentially.
func (s *ImageAnalysisAppService) StartBatch(ctx context.Context, images []imageanalysis.UploadedImage, intents []imageanalysis.AnalysisIntent) (*Task, *imageanalysis.BatchState, error) {
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
		}
	}

	state := imageanalysis.NewBatchState(batchID, items, intents)
	if err := store.Save(state); err != nil {
		return nil, nil, fmt.Errorf("保存批次状态失败: %w", err)
	}

	// Persist uploaded images to workspace storage.
	for i, img := range sorted {
		if err := store.SaveImage(batchID, items[i], img.Data); err != nil {
			return nil, nil, fmt.Errorf("保存图片 %s 失败: %w", img.FileName, err)
		}
	}

	log.Printf("[image-analysis] batch created id=%s pages=%d intents=%v workspace=%q", batchID, len(items), intents, workspace)

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
			log.Printf("[image-analysis] batch error id=%s err=%v", batchID, err)
			emit(agent.Event{Type: "error", Data: map[string]string{"message": err.Error()}})
			return
		}

		// Aggregate and persist result.
		result := svc.Aggregate(state)
		markdown := buildResultMarkdown(result)
		if saveErr := store.SaveResult(batchID, markdown); saveErr != nil {
			log.Printf("[image-analysis] save result failed id=%s err=%v", batchID, saveErr)
		}
		emit(agent.Event{Type: "image_analysis_done", Data: result})
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
			emit(agent.Event{Type: "error", Data: map[string]string{"message": err.Error()}})
			return
		}
		result := svc.Aggregate(state)
		markdown := buildResultMarkdown(result)
		store.SaveResult(batchID, markdown)
		emit(agent.Event{Type: "image_analysis_done", Data: result})
	})
	return task, nil
}

// AbortBatch cancels a running batch task.
func (s *ImageAnalysisAppService) AbortBatch(batchID string) bool {
	imageAnalysisMu.Lock()
	task, ok := imageAnalysisBatches[batchID]
	imageAnalysisMu.Unlock()
	if !ok {
		return false
	}
	task.Abort()
	return true
}

// ActiveBatchTask returns the running task for a batch, if any.
func (s *ImageAnalysisAppService) ActiveBatchTask(batchID string) (*Task, bool) {
	imageAnalysisMu.Lock()
	defer imageAnalysisMu.Unlock()
	task, ok := imageAnalysisBatches[batchID]
	return task, ok
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
