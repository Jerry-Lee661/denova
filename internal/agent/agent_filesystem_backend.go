package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
)

// agentFilesystemBackend wraps filesystem.Backend with Nova Agent-specific
// safety and recovery behavior while delegating ordinary filesystem operations.
type agentFilesystemBackend struct {
	filesystem.Backend
	workspace string
	cache     *fileReadCache
}

const agentFileReadDefaultLimitLines = 2000

// fileReadCache avoids repeated disk reads when the agent re-reads the same
// file across multiple turns (common when context-window limits push older
// tool results out). Entries are validated by file size + modification time
// and evicted LRU under a configurable byte limit.
type fileReadCache struct {
	mu       sync.RWMutex
	entries  map[string]*fileReadCacheEntry
	maxBytes int64
	curBytes int64
}

type fileReadCacheEntry struct {
	content string
	size    int64
	modTime time.Time
	atime   time.Time
	offset  int // 缓存内容对应的起始行（1-based）
	limit   int // 缓存内容覆盖的行数
}

const fileReadCacheDefaultMaxBytes = 64 * 1024 * 1024 // 64 MiB

func newFileReadCache(maxBytes int64) *fileReadCache {
	if maxBytes <= 0 {
		maxBytes = fileReadCacheDefaultMaxBytes
	}
	return &fileReadCache{
		entries:  map[string]*fileReadCacheEntry{},
		maxBytes: maxBytes,
	}
}

// get 仅在请求窗口 [reqStart, reqEnd) 完全落在缓存窗口内时命中。
func (c *fileReadCache) get(path string, reqStart, reqEnd int) (string, bool) {
	if c == nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	c.mu.RLock()
	entry, ok := c.entries[path]
	c.mu.RUnlock()
	if !ok {
		return "", false
	}
	if entry.size != info.Size() || !entry.modTime.Equal(info.ModTime()) {
		c.mu.Lock()
		delete(c.entries, path)
		c.curBytes -= int64(len(entry.content))
		c.mu.Unlock()
		return "", false
	}
	// 校验请求窗口是否完全落在缓存窗口 [entry.offset, entry.offset+entry.limit) 内
	cachedEnd := entry.offset + entry.limit
	if reqStart < entry.offset || reqEnd > cachedEnd {
		return "", false
	}
	c.mu.Lock()
	entry.atime = time.Now()
	c.mu.Unlock()
	return entry.content, true
}

// set 记录写入时的 offset/limit，表示缓存内容覆盖的窗口范围。
func (c *fileReadCache) set(path, content string, offset, limit int) {
	if c == nil {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	size := int64(len(content))
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[path]; ok {
		c.curBytes -= int64(len(existing.content))
	}
	for c.curBytes+size > c.maxBytes && len(c.entries) > 0 {
		c.evictOldestLocked()
	}
	c.entries[path] = &fileReadCacheEntry{
		content: content,
		size:    info.Size(),
		modTime: info.ModTime(),
		atime:   time.Now(),
		offset:  offset,
		limit:   limit,
	}
	c.curBytes += size
}

func (c *fileReadCache) evictOldestLocked() {
	var oldestKey string
	var oldestTime time.Time
	for key, entry := range c.entries {
		if oldestKey == "" || entry.atime.Before(oldestTime) {
			oldestKey = key
			oldestTime = entry.atime
		}
	}
	if oldestKey != "" {
		c.curBytes -= int64(len(c.entries[oldestKey].content))
		delete(c.entries, oldestKey)
	}
}

func newAgentFilesystemBackend(inner filesystem.Backend, workspaces ...string) filesystem.Backend {
	if inner == nil {
		return nil
	}
	workspace := ""
	if len(workspaces) > 0 {
		workspace = strings.TrimSpace(workspaces[0])
		if workspace != "" {
			workspace = filepath.Clean(workspace)
		}
	}
	return &agentFilesystemBackend{
		Backend:   inner,
		workspace: workspace,
		cache:     newFileReadCache(fileReadCacheDefaultMaxBytes),
	}
}

func (b *agentFilesystemBackend) Read(ctx context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	if req == nil {
		return nil, fmt.Errorf("read request is nil")
	}
	if b.Backend == nil {
		return nil, fmt.Errorf("filesystem backend is nil")
	}
	next := *req
	if next.Offset <= 0 {
		next.Offset = 1
	}
	if next.Limit <= 0 {
		next.Limit = agentFileReadDefaultLimitLines
	}
	return b.Backend.Read(ctx, &next)
}

// LsInfo / GlobInfo / GrepRaw 覆盖 eino 默认实现：它们接收真实绝对路径，但默认
// 实现既不校验相对路径也不校验 workspace 边界，且空路径会回退到磁盘根（ls/glob）
// 或进程工作目录（grep），可能让 Agent 看到 workspace 之外的内容。这里统一改为
// 走 resolveAgentToolPath 校验，空路径默认落到 workspace 根。

func (b *agentFilesystemBackend) LsInfo(ctx context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, fmt.Errorf("ls request is nil")
	}
	if b == nil || b.Backend == nil {
		return nil, fmt.Errorf("filesystem backend is nil")
	}
	next := *req
	path, err := resolveAgentToolPathOrWorkspaceRoot(b.workspace, next.Path)
	if err != nil {
		return nil, err
	}
	next.Path = path
	// 目标路径不存在时返回显式错误（触发 path_resolution_failed 恢复提示），
	// 而不是让底层 ls 静默返回空——静默空会让模型误以为是"空目录"而无法分辨
	// 路径写错，进而陷入反复猜测绝对路径。
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	return b.Backend.LsInfo(ctx, &next)
}

func (b *agentFilesystemBackend) GlobInfo(ctx context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, fmt.Errorf("glob request is nil")
	}
	if b == nil || b.Backend == nil {
		return nil, fmt.Errorf("filesystem backend is nil")
	}
	next := *req
	path, err := resolveAgentToolPathOrWorkspaceRoot(b.workspace, next.Path)
	if err != nil {
		return nil, err
	}
	next.Path = path
	// 与 ls 一致：搜索根路径不存在时返回显式错误（触发恢复提示），
	// 避免底层 glob 直接返回 "failed to walk directory" 这种无引导的错误。
	if _, statErr := os.Stat(path); statErr != nil {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	return b.Backend.GlobInfo(ctx, &next)
}

func (b *agentFilesystemBackend) GrepRaw(ctx context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	if req == nil {
		return nil, fmt.Errorf("grep request is nil")
	}
	if b == nil || b.Backend == nil {
		return nil, fmt.Errorf("filesystem backend is nil")
	}
	next := *req
	path, err := resolveAgentToolPathOrWorkspaceRoot(b.workspace, next.Path)
	if err != nil {
		return nil, err
	}
	// 与 read_file 同样的路径容错：模型传的 path 可能被截断（ch00001 → 缺 -正文）
	// 或 URL 编码。解析后目标不存在时，尝试候选变体与父目录唯一前缀回退。
	next.Path = resolveGrepPathWithFallback(b.workspace, next.Path, path)
	return b.Backend.GrepRaw(ctx, &next)
}

// resolveGrepPathWithFallback 在 grep 的解析路径不存在时尝试容错：
// 1) URL 解码 / 破折号折叠候选（复用 read_file 的 workspaceFilePathCandidates）；
// 2) 父目录文件名前缀唯一回退（复用 readFileUniquePrefixFallback，只匹配文件）。
// 都不命中时返回原始绝对路径，让底层暴露真实的"路径不存在"。
func resolveGrepPathWithFallback(workspace, input, absolute string) string {
	if _, err := os.Stat(absolute); err == nil {
		return absolute
	}
	for _, candidate := range workspaceFilePathCandidates(input) {
		if candidate == input {
			continue
		}
		if abs, _, err := resolveAgentToolPath(workspace, candidate); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				return abs
			}
		}
	}
	if workspace != "" {
		if _, relative, err := resolveAgentToolPath(workspace, input); err == nil {
			if fb := readFileUniquePrefixFallback(workspace, relative); fb != "" {
				if abs, _, err := resolveAgentToolPath(workspace, fb); err == nil {
					return abs
				}
			}
		}
	}
	return absolute
}

// resolveAgentToolPathOrWorkspaceRoot 在路径为空且存在 active workspace 时回退到
// workspace 根（避免 ls/glob/grep 的空路径落到磁盘根或进程工作目录）；否则走
// resolveAgentToolPath 统一校验。空路径 + 无 workspace 时原样透传，保持该场景的
// 既有行为。
func resolveAgentToolPathOrWorkspaceRoot(workspace, input string) (string, error) {
	if strings.TrimSpace(input) == "" {
		workspace = strings.TrimSpace(workspace)
		if workspace == "" {
			return "", nil
		}
		absolute, err := filepath.Abs(workspace)
		if err != nil {
			return "", err
		}
		return filepath.Clean(absolute), nil
	}
	absolute, _, err := resolveAgentToolPath(workspace, input)
	if err != nil {
		return "", err
	}
	return absolute, nil
}

func (b *agentFilesystemBackend) Edit(ctx context.Context, req *filesystem.EditRequest) error {
	if req == nil {
		return fmt.Errorf("edit request is nil")
	}
	err := b.Backend.Edit(ctx, req)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "string not found") {
		return err
	}

	content, readErr := os.ReadFile(req.FilePath)
	if readErr != nil {
		return err
	}
	text := string(content)
	normalizedText, indexMap := normalizeEditWhitespace(text)
	normalizedOld, _ := normalizeEditWhitespace(req.OldString)
	if normalizedOld == "" {
		return err
	}

	count := strings.Count(normalizedText, normalizedOld)
	if count == 0 {
		return err
	}
	if count > 1 {
		return fmt.Errorf("string (after trailing whitespace normalization) appears %d times; refusing fuzzy edit", count)
	}

	newText, ok := replaceNormalizedSpans(text, normalizedText, indexMap, normalizedOld, req.NewString)
	if !ok {
		return err
	}
	return os.WriteFile(req.FilePath, []byte(newText), 0644)
}

func normalizeEditWhitespace(s string) (string, []int) {
	var normalized strings.Builder
	indexMap := make([]int, 0, len(s))
	lineStart := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			appendTrimmedLine(&normalized, &indexMap, s, lineStart, i)
			normalized.WriteByte('\n')
			indexMap = append(indexMap, i)
			lineStart = i + 1
		}
	}
	appendTrimmedLine(&normalized, &indexMap, s, lineStart, len(s))
	return normalized.String(), indexMap
}

func appendTrimmedLine(normalized *strings.Builder, indexMap *[]int, s string, start, end int) {
	trimmedEnd := end
	for trimmedEnd > start && (s[trimmedEnd-1] == ' ' || s[trimmedEnd-1] == '\t') {
		trimmedEnd--
	}
	for i := start; i < trimmedEnd; i++ {
		normalized.WriteByte(s[i])
		*indexMap = append(*indexMap, i)
	}
}

func replaceNormalizedSpans(original, normalized string, indexMap []int, oldString, newString string) (string, bool) {
	var out strings.Builder
	searchFrom := 0
	originalFrom := 0
	replaced := false
	for {
		rel := strings.Index(normalized[searchFrom:], oldString)
		if rel < 0 {
			break
		}
		matchStart := searchFrom + rel
		matchEnd := matchStart + len(oldString)
		if matchStart >= len(indexMap) {
			return "", false
		}
		originalStart := indexMap[matchStart]
		originalEnd := len(original)
		if matchEnd < len(indexMap) {
			originalEnd = indexMap[matchEnd]
		}
		if originalStart < originalFrom || originalEnd < originalStart {
			return "", false
		}
		out.WriteString(original[originalFrom:originalStart])
		out.WriteString(newString)
		originalFrom = originalEnd
		replaced = true
		break
	}
	if !replaced {
		return "", false
	}
	out.WriteString(original[originalFrom:])
	return out.String(), true
}
