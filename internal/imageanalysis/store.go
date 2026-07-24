package imageanalysis

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func (s *Store) batchDir(batchID string) string {
	return filepath.Join(s.workspace, "assets", "analysis", "batch-"+batchID)
}

func (s *Store) statePath(batchID string) string {
	return filepath.Join(s.batchDir(batchID), "state.json")
}

// ResultPath returns the workspace-relative path for the aggregated result file.
func ResultPath(batchID string) string {
	return filepath.Join("assets", "analysis", "batch-"+batchID, "result.md")
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

// SaveImage writes an uploaded image to the batch storage directory.
func (s *Store) SaveImage(batchID string, item ImageItem, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.workspace, "assets", "uploads", "batch-"+batchID)
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
