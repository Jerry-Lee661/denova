package imageanalysis

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store persists batch analysis state to the workspace.
// State files live under assets/analysis/batch-{id}/state.json.
type Store struct {
	workspace string
	mu        sync.Mutex
}

// NewStore creates a Store rooted at the given workspace directory.
func NewStore(workspace string) *Store {
	return &Store{workspace: workspace}
}

// analysisBase returns the absolute path to the analysis directory.
func (s *Store) analysisBase() string {
	return filepath.Join(s.workspace, "assets", "analysis")
}

func (s *Store) batchDir(batchID string) string {
	return filepath.Join(s.analysisBase(), "batch-"+batchID)
}

// findBatchDir locates the actual on-disk directory for a batch, even when the
// directory name carries an extra suffix (e.g. "batch-abc123-Qwen3.6-35B-A3B"
// instead of the canonical "batch-abc123"). It scans the analysis directory for
// directories matching "batch-{id}*", loads each state.json, and returns the
// first match. Returns the canonical path as a fallback (batchDir) when no
// match is found.
func (s *Store) findBatchDir(batchID string) string {
	canonical := s.batchDir(batchID)
	// Quick check: canonical path exists.
	if _, err := os.Stat(canonical); err == nil {
		return canonical
	}
	// Scan for alternate naming.
	base := s.analysisBase()
	entries, err := os.ReadDir(base)
	if err != nil {
		return canonical
	}
	prefix := "batch-" + batchID
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if e.Name() == "batch-"+batchID {
			continue // already checked above
		}
		// Verify the state.json inside matches the requested ID.
		statePath := filepath.Join(base, e.Name(), "state.json")
		if data, err := os.ReadFile(statePath); err == nil {
			var state BatchState
			if err := json.Unmarshal(data, &state); err == nil && state.ID == batchID {
				return filepath.Join(base, e.Name())
			}
		}
	}
	return canonical
}

func (s *Store) statePath(batchID string) string {
	return filepath.Join(s.findBatchDir(batchID), "state.json")
}

// ResultPath returns the workspace-relative path for the aggregated result file.
func ResultPath(batchID string) string {
	return filepath.Join("assets", "analysis", "batch-"+batchID, "result.md")
}

// FactStreamPath returns the workspace-relative path for the fact stream file.
func FactStreamPath(batchID string) string {
	return filepath.Join("assets", "analysis", "batch-"+batchID, "fact_stream.md")
}

// Save persists the batch state to disk.
func (s *Store) Save(state *BatchState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.batchDir(state.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建批次目录失败: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化批次状态失败: %w", err)
	}
	if err := os.WriteFile(s.statePath(state.ID), data, 0o644); err != nil {
		return fmt.Errorf("写入批次状态失败: %w", err)
	}
	return nil
}

// Load reads a batch state from disk.
func (s *Store) Load(batchID string) (*BatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.statePath(batchID))
	if err != nil {
		return nil, fmt.Errorf("读取批次状态失败: %w", err)
	}
	var state BatchState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("解析批次状态失败: %w", err)
	}
	return &state, nil
}

// SaveResult writes the aggregated markdown result to the workspace.
func (s *Store) SaveResult(batchID, markdown string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.batchDir(batchID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建批次目录失败: %w", err)
	}
	path := filepath.Join(s.workspace, ResultPath(batchID))
	if err := os.WriteFile(path, []byte(markdown), 0o644); err != nil {
		return fmt.Errorf("写入分析结果失败: %w", err)
	}
	return nil
}

// SaveFactStream writes the fact stream markdown to the workspace.
func (s *Store) SaveFactStream(batchID, factStream string) error {
	if factStream == "" {
		return nil // 空事实流不落盘
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.batchDir(batchID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建批次目录失败: %w", err)
	}
	path := filepath.Join(s.workspace, FactStreamPath(batchID))
	if err := os.WriteFile(path, []byte(factStream), 0o644); err != nil {
		return fmt.Errorf("写入事实流失败: %w", err)
	}
	return nil
}

// SaveImage writes an uploaded image to the batch storage directory.
func (s *Store) SaveImage(batchID string, item ImageItem, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.uploadDir(batchID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建上传目录失败: %w", err)
	}
	// Use page_index prefix to guarantee filesystem order matches display order.
	safeName := fmt.Sprintf("%03d_%s", item.PageIndex, sanitizeFileName(item.FileName))
	path := filepath.Join(dir, safeName)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("写入图片失败: %w", err)
	}
	return nil
}

func (s *Store) uploadDir(batchID string) string {
	return filepath.Join(s.workspace, "assets", "uploads", "batch-"+batchID)
}

// DeleteUploadedImages removes the raw uploaded image directory for a batch.
// 仅由保留期清扫（SweepExpiredUploads）或用户手动清理调用；批次完成/中止后
// 图片需保留，供失败页重试与切换模型重测使用。
func (s *Store) DeleteUploadedImages(batchID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.uploadDir(batchID)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("删除上传图片失败: %w", err)
	}
	return nil
}

// SweepExpiredUploads 惰性清理超过保留期的历史批次原始图片：对已结束
// （非 running/pending）且 FinishedAt 距今超过 retention 的批次，删除其
// uploads 目录并在 state 上落 images_deleted 标记。retention <= 0 表示
// 永不自动清理。调用方在应用启动与新建批次时触发（无后台常驻 goroutine）。
// 返回清理的批次数。
func (s *Store) SweepExpiredUploads(retention time.Duration) int {
	if retention <= 0 {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	base := filepath.Join(s.workspace, "assets", "analysis")
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	now := time.Now().UTC()
	swept := 0
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) <= len("batch-") || e.Name()[:len("batch-")] != "batch-" {
			continue
		}
		statePath := filepath.Join(base, e.Name(), "state.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var state BatchState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		// 运行中/待处理的批次图片正在使用，绝不清扫。
		if state.Status == BatchRunning || state.Status == BatchPending {
			continue
		}
		if state.ImagesDeleted || state.FinishedAt == nil || now.Sub(*state.FinishedAt) <= retention {
			continue
		}
		if err := os.RemoveAll(s.uploadDir(state.ID)); err != nil {
			log.Printf("[image-analysis] sweep uploads failed id=%s err=%v", state.ID, err)
			continue
		}
		state.ImagesDeleted = true
		out, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			continue
		}
		if err := os.WriteFile(statePath, out, 0o644); err != nil {
			log.Printf("[image-analysis] sweep mark failed id=%s err=%v", state.ID, err)
			continue
		}
		swept++
		log.Printf("[image-analysis] swept expired uploads id=%s retention=%v", state.ID, retention)
	}
	return swept
}

// NewBatchState creates a fresh batch state from sorted images and intents.
func NewBatchState(batchID string, images []ImageItem, intents []AnalysisIntent) *BatchState {
	now := time.Now().UTC()
	results := make([]PageResult, len(images))
	for i, img := range images {
		results[i] = PageResult{
			PageIndex: img.PageIndex,
			FileName:  img.FileName,
			Status:    "pending",
		}
	}
	return &BatchState{
		ID:         batchID,
		Status:     BatchPending,
		Intents:    intents,
		Images:     images,
		Results:    results,
		CreatedAt:  now,
		TotalPages: len(images),
	}
}

func sanitizeFileName(name string) string {
	replacer := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.', r == '-', r == '_', r == '(', r == ')':
			return r
		default:
			return '_'
		}
	}
	return string([]rune(mapRunes(name, replacer)))
}

func mapRunes(s string, f func(rune) rune) string {
	runes := []rune(s)
	for i, r := range runes {
		runes[i] = f(r)
	}
	return string(runes)
}

// RecoverStaleBatches marks any batch still in "running" or "pending" status
// as "failed". This is called on service startup to recover from a backend crash
// that left batches in an indeterminate state with no in-memory task to drive them.
// Pages that were mid-processing are marked failed so the user can retry them.
func (s *Store) RecoverStaleBatches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := filepath.Join(s.workspace, "assets", "analysis")
	entries, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	recovered := 0
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) <= len("batch-") || e.Name()[:len("batch-")] != "batch-" {
			continue
		}
		statePath := filepath.Join(base, e.Name(), "state.json")
		data, err := os.ReadFile(statePath)
		if err != nil {
			continue
		}
		var state BatchState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		if state.Status != BatchRunning && state.Status != BatchPending {
			continue
		}
		// Mark the batch as failed due to service interruption.
		originalStatus := state.Status
		now := time.Now().UTC()
		state.Status = BatchFailed
		state.FinishedAt = &now
		for i := range state.Results {
			if state.Results[i].Status == "processing" || state.Results[i].Status == "pending" {
				state.Results[i].Status = "failed"
				state.Results[i].Error = "服务中断，分析未完成"
			}
		}
		out, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			continue
		}
		if err := os.WriteFile(statePath, out, 0o644); err != nil {
			continue
		}
		recovered++
		log.Printf("[image-analysis] recovered stale batch id=%s (was %s, now failed)", state.ID, originalStatus)
	}
	return recovered
}

// ListRecent returns the most recent batch states on disk, newest first.
// It scans the analysis directory for batch-* subdirectories and loads each
// state.json. Malformed entries are skipped. limit <= 0 returns all found.
func (s *Store) ListRecent(limit int) ([]*BatchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := filepath.Join(s.workspace, "assets", "analysis")
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取分析目录失败: %w", err)
	}
	type entry struct {
		state *BatchState
		mtime time.Time
	}
	var found []entry
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) <= len("batch-") || e.Name()[:len("batch-")] != "batch-" {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, e.Name(), "state.json"))
		if err != nil {
			continue
		}
		var state BatchState
		if err := json.Unmarshal(data, &state); err != nil {
			continue
		}
		found = append(found, entry{state: &state, mtime: info.ModTime()})
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].mtime.After(found[j].mtime)
	})
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	out := make([]*BatchState, 0, len(found))
	for _, f := range found {
		out = append(out, f.state)
	}
	return out, nil
}
