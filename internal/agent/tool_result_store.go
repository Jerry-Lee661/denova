package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"denova/internal/workspacepath"
)

// newResultStoreForWorkspace 为工作区创建结果存储目录。
// 存储路径为 <workspace>/.denova/results/，按会话 ID 分目录、按 idempotencyKey
// 定位文件，保证同一工具调用每次映射到同一位置（预览逐字节稳定）。
func newResultStoreForWorkspace(workspace string) (*ResultStore, error) {
	if strings.TrimSpace(workspace) == "" {
		return &ResultStore{}, nil
	}
	dir := filepath.Join(workspacepath.Dir(workspace), "results")
	sessionID := strings.TrimPrefix(filepath.Base(workspace), ".")
	if sessionID == "" {
		sessionID = "workspace"
	}
	store, err := NewResultStore(dir, sessionID)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// ResultStore 将过大的工具结果整体写盘，接口视图只放预览 + 位置引用。
//
// 与 FilterToolResultForModel 的字节上限裁剪不同：外置是「整体写盘」——不语义摘要、
// 不截断正文，模型需要细节时按位置读取。预览一旦发出进入前缀即逐字节稳定，
// 之后每轮原样重现、不再改动（改一个字后续缓存即作废）。
type ResultStore struct {
	dir       string
	sessionID string
}

// NewResultStore 创建结果存储目录（不存在则自动创建）。
func NewResultStore(dir, sessionID string) (*ResultStore, error) {
	if strings.TrimSpace(dir) == "" {
		return &ResultStore{sessionID: sessionID}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建结果存储目录失败: %w", err)
	}
	return &ResultStore{dir: dir, sessionID: sessionID}, nil
}

// ExternalizeThreshold 判断是否需要外置的原始字节阈值。超过该阈值的结果整体写盘。
const ExternalizeThreshold = 64 * 1024

// Store 将内容整体写盘，返回稳定的相对位置引用与字节有界预览。
// idempotencyKey 保证同一工具调用每次映射到同一位置，预览逐字节稳定。
// 调用方需先通过 FilterToolResultForModelWithLimit 获得 idempotencyKey。
func (s *ResultStore) Store(idempotencyKey, content string) (location, preview string) {
	if strings.TrimSpace(idempotencyKey) == "" {
		idempotencyKey = "result"
	}
	location = filepath.ToSlash(filepath.Join(s.sessionID, idempotencyKey+".txt"))
	if s.dir == "" {
		return location, PreviewBytes(content, defaultExternalizePreviewBytes)
	}
	fullPath := filepath.Join(s.dir, s.sessionID, idempotencyKey+".txt")
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return location, PreviewBytes(content, defaultExternalizePreviewBytes)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return location, PreviewBytes(content, defaultExternalizePreviewBytes)
	}
	return location, PreviewBytes(content, defaultExternalizePreviewBytes)
}

// Load 从磁盘读取指定位置的内容。
func (s *ResultStore) Load(idempotencyKey string) (string, error) {
	if s.dir == "" {
		return "", fmt.Errorf("结果存储目录未配置")
	}
	fullPath := filepath.Join(s.dir, s.sessionID, idempotencyKey+".txt")
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// PreviewBytes 返回字节有界的预览（按 rune 边界截断，追加省略标记）。
func PreviewBytes(content string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = defaultExternalizePreviewBytes
	}
	value := strings.TrimSpace(content)
	if len(value) <= maxBytes {
		return value
	}
	limit := maxBytes
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	if limit <= 0 {
		return "…"
	}
	return value[:limit] + "…"
}

// ExternalizeLocationReference 生成工具结果外置的位置引用文本。
func ExternalizeLocationReference(location string) string {
	if strings.TrimSpace(location) == "" {
		return "[tool result externalized to disk]"
	}
	return "[tool result externalized to disk: " + location + "]"
}

// defaultExternalizePreviewBytes 外置结果的预览字节上限。
const defaultExternalizePreviewBytes = 2 * 1024

// microCompactPlaceholder 微压缩占位文本：替换冷缓存中更早的工具结果。
func microCompactPlaceholder(location string) string {
	if strings.TrimSpace(location) != "" {
		return "[tool result preview cached at " + location + ", full content on disk]"
	}
	return "[tool result preview cached, full content on disk]"
}

// MicroCompactResult 根据缓存状态决定旧工具结果在下一轮接口视图中的形态。
// cacheHot=true：缓存仍热，预览逐字节不变（走接口层缓存编辑）。
// cacheHot=false：冷缓存，前缀反正重建，换成占位文字省 token。
func MicroCompactResult(result FilteredToolResult, cacheHot bool) string {
	if cacheHot {
		return result.Content
	}
	// 冷缓存：用占位文本替换正文，保留位置引用以便模型按需读取。
	location := result.Location
	body := microCompactPlaceholder(location)
	metadata := formatToolResultMetadata(result.Manifest, result.OriginalBytes, len(body), result.Truncated, location, result.IdempotencyKey)
	return body + "\n\n" + metadata
}

// externalizeContent 将已过滤的结果整体写盘并组装外置视图（预览+位置引用+元数据）。
// 预览逐字节稳定，进入前缀后不再改动。
func (s *ResultStore) externalizeContent(filtered FilteredToolResult, content string) FilteredToolResult {
	location, preview := s.Store(filtered.IdempotencyKey, content)
	body := preview + "\n" + ExternalizeLocationReference(location)
	metadata := formatToolResultMetadata(filtered.Manifest, filtered.OriginalBytes, len(body), false, location, filtered.IdempotencyKey)
	result := strings.TrimRight(body, "\n")
	if result != "" {
		result += "\n\n"
	}
	result += metadata
	filtered.Content = result
	filtered.Location = location
	filtered.Preview = preview
	filtered.Externalized = true
	return filtered
}

// Externalize 无条件将内容整体写盘，返回外置后的完整 Content（预览+位置引用+元数据）。
// 供聚合预算路径使用：一条消息内累计注入超限时对结果做外置降级，不受单工具阈值约束
// （小结果也可能因累计超预算而外置）。
func (s *ResultStore) Externalize(toolName, args, content string, maxBytes int) FilteredToolResult {
	filtered := FilterToolResultForModelWithLimit(toolName, args, content, maxBytes)
	return s.externalizeContent(filtered, content)
}

// ExternalizeIfNeeded 判断内容是否需要外置并执行外置。
// 返回是否外置、外置后的完整 Content（含位置引用与元数据）。
// 外置阈值按工具区分：read_file 使用更低的 readFileExternalizeThreshold
// （大文件读取默认整体外置，模型只看到预览+位置引用，按需 offset/limit 重读），
// 其余工具使用默认 ExternalizeThreshold。
func (s *ResultStore) ExternalizeIfNeeded(toolName, args, content string, maxBytes int) (FilteredToolResult, bool) {
	filtered := FilterToolResultForModelWithLimit(toolName, args, content, maxBytes)
	if filtered.OriginalBytes <= externalizeThresholdForTool(toolName) {
		return filtered, false
	}
	return s.externalizeContent(filtered, content), true
}

// externalizeRecord 持久化一次外置决定的说明记录，供 resume 重放。
type externalizeRecord struct {
	Type           string    `json:"type"`
	SessionID      string    `json:"session_id,omitempty"`
	IdempotencyKey string    `json:"idempotency_key"`
	Location       string    `json:"location"`
	CreatedAt      time.Time `json:"created_at"`
}
