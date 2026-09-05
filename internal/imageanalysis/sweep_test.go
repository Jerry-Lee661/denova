package imageanalysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"denova/config"
)

// TestSweepExpiredUploads 验证惰性清扫：仅清理已结束且超过保留期的批次图片，
// 运行中的批次绝不触碰；retention<=0 直接跳过；已清扫的批次不重复处理。
func TestSweepExpiredUploads(t *testing.T) {
	workspace := t.TempDir()
	store := NewStore(workspace)
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	fresh := now.Add(-30 * time.Minute)

	makeBatch := func(id string, status BatchStatus, finished *time.Time, deleted bool) {
		t.Helper()
		state := &BatchState{
			ID:            id,
			Status:        status,
			CreatedAt:     now,
			FinishedAt:    finished,
			ImagesDeleted: deleted,
			TotalPages:    1,
			Results:       []PageResult{{PageIndex: 0, FileName: "a.png", Status: "success"}},
		}
		if err := store.Save(state); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		dir := store.uploadDir(id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "000_a.png"), []byte("img"), 0o644); err != nil {
			t.Fatalf("write image %s: %v", id, err)
		}
	}

	makeBatch("expired-completed", BatchCompleted, &old, false)
	makeBatch("fresh-completed", BatchCompleted, &fresh, false)
	makeBatch("expired-running", BatchRunning, &old, false)
	makeBatch("already-swept", BatchPartial, &old, true)

	if got := store.SweepExpiredUploads(0); got != 0 {
		t.Fatalf("retention=0 应永不清理, swept=%d", got)
	}

	if got := store.SweepExpiredUploads(time.Hour); got != 1 {
		t.Fatalf("应恰好清理 1 个过期批次, swept=%d", got)
	}

	uploadsGone := func(id string) bool {
		_, err := os.Stat(store.uploadDir(id))
		return os.IsNotExist(err)
	}
	if !uploadsGone("expired-completed") {
		t.Error("过期已完成批次的图片应被删除")
	}
	for _, id := range []string{"fresh-completed", "expired-running", "already-swept"} {
		if uploadsGone(id) {
			t.Errorf("批次 %s 的图片不应被清理", id)
		}
	}

	state, err := store.Load("expired-completed")
	if err != nil {
		t.Fatalf("load swept batch: %v", err)
	}
	if !state.ImagesDeleted {
		t.Error("清扫后应落 images_deleted 标记")
	}

	// 重复清扫幂等：标记位阻止二次处理。
	if got := store.SweepExpiredUploads(time.Hour); got != 0 {
		t.Fatalf("重复清扫应幂等, swept=%d", got)
	}
}

// TestProcessBatchRetainsUploads 验证批次完成后原始图片不被删除——保留期内
// 用户可随时切换模型重测（re-extract）或重试失败页。使用 text-layer 页跳过
// 视觉模型，无需 HTTP。
func TestProcessBatchRetainsUploads(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(&config.Config{}, workspace)
	store := svc.Store()

	state := &BatchState{
		ID:         "batch-retain",
		Status:     BatchPending,
		CreatedAt:  time.Now().UTC(),
		TotalPages: 2,
		Images: []ImageItem{
			{PageIndex: 0, FileName: "p0.png", Text: "text-0"},
			{PageIndex: 1, FileName: "p1.png", Text: "text-1"},
		},
	}
	state.Results = make([]PageResult, len(state.Images))

	// 预置上传目录与图片文件，模拟真实上传落盘。
	dir := store.uploadDir(state.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir uploads: %v", err)
	}
	for _, img := range state.Images {
		name := filepath.Join(dir, "000_"+img.FileName)
		if err := os.WriteFile(name, []byte("img"), 0o644); err != nil {
			t.Fatalf("write upload: %v", err)
		}
	}

	if err := svc.ProcessBatch(context.Background(), state, func(ProgressEvent) {}); err != nil {
		t.Fatalf("ProcessBatch failed: %v", err)
	}
	if state.Status != BatchCompleted {
		t.Fatalf("status = %q, want completed", state.Status)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("批次完成后图片目录不应被删除: %v", err)
	}

	// 手动清理路径：删除图片并落标记。
	if err := store.DeleteUploadedImages(state.ID); err != nil {
		t.Fatalf("manual cleanup: %v", err)
	}
	state.ImagesDeleted = true
	if err := store.Save(state); err != nil {
		t.Fatalf("save images_deleted flag: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("手动清理后图片目录应不存在")
	}
	loaded, err := store.Load(state.ID)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if !loaded.ImagesDeleted {
		t.Error("手动清理后 images_deleted 标记应持久化")
	}
}
