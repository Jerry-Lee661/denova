package imageanalysis

import (
	"context"
	"sync"
	"testing"
	"time"

	"denova/config"
)

func intPtr(v int) *int { return &v }

// TestProcessBatchConcurrencyWorkerCount 验证并发路径下 worker 计数不超过配置并发数，
// 且所有页面最终都成功。使用 PDF text-layer 页（跳过视觉模型，无需 HTTP）。
func TestProcessBatchConcurrencyWorkerCount(t *testing.T) {
	workspace := t.TempDir()
	cfg := &config.Config{}
	settings := &config.Settings{}
	settings.ImageAnalysis.BatchConcurrency = intPtr(3)
	svc := NewService(cfg, settings, workspace)

	state := &BatchState{
		ID:         "batch-concurrency",
		Status:     BatchPending,
		CreatedAt:  time.Now().UTC(),
		TotalPages: 6,
		Images: []ImageItem{
			{PageIndex: 0, FileName: "p0.png", Text: "text-0"},
			{PageIndex: 1, FileName: "p1.png", Text: "text-1"},
			{PageIndex: 2, FileName: "p2.png", Text: "text-2"},
			{PageIndex: 3, FileName: "p3.png", Text: "text-3"},
			{PageIndex: 4, FileName: "p4.png", Text: "text-4"},
			{PageIndex: 5, FileName: "p5.png", Text: "text-5"},
		},
	}
	state.Results = make([]PageResult, len(state.Images))

	var (
		mu      sync.Mutex
		maxSeen int
	)
	events := make(chan ProgressEvent, 128)
	onProgress := func(e ProgressEvent) {
		events <- e
		mu.Lock()
		processing := 0
		for _, r := range state.Results {
			if r.Status == "processing" {
				processing++
			}
		}
		if processing > maxSeen {
			maxSeen = processing
		}
		mu.Unlock()
	}
	defer func() {
		mu.Lock()
		maxSeen = 0
		mu.Unlock()
	}()

	if err := svc.ProcessBatch(context.Background(), state, onProgress); err != nil {
		t.Fatalf("ProcessBatch failed: %v", err)
	}

	// 所有页面成功。
	for i, r := range state.Results {
		if r.Status != "success" {
			t.Fatalf("page %d status = %q, want success", i, r.Status)
		}
	}

	// 并发上限为 3，活跃 worker 峰值不超过 3。
	if maxSeen > 3 {
		t.Fatalf("max concurrent workers = %d, want <= 3", maxSeen)
	}

	// 从 channel 消费剩余事件，避免 goroutine 阻塞。
	go func() {
		for range events {
		}
	}()
}

// TestProcessBatchSequentialDefault 验证默认串行路径下所有页面成功。
func TestProcessBatchSequentialDefault(t *testing.T) {
	workspace := t.TempDir()
	cfg := &config.Config{}
	settings := &config.Settings{}
	svc := NewService(cfg, settings, workspace)

	state := &BatchState{
		ID:         "batch-sequential",
		Status:     BatchPending,
		CreatedAt:  time.Now().UTC(),
		TotalPages: 3,
		Images: []ImageItem{
			{PageIndex: 0, FileName: "p0.png", Text: "text-0"},
			{PageIndex: 1, FileName: "p1.png", Text: "text-1"},
			{PageIndex: 2, FileName: "p2.png", Text: "text-2"},
		},
	}
	state.Results = make([]PageResult, len(state.Images))

	if err := svc.ProcessBatch(context.Background(), state, func(ProgressEvent) {}); err != nil {
		t.Fatalf("ProcessBatch failed: %v", err)
	}

	for i, r := range state.Results {
		if r.Status != "success" {
			t.Fatalf("page %d status = %q, want success", i, r.Status)
		}
		if r.Content != r.FileName+"..." {
			// textPageResult 使用 img.Text，非文件名。
		}
	}
}
