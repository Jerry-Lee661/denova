package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	localbk "github.com/cloudwego/eino-ext/adk/backend/local"
	"github.com/cloudwego/eino/adk/filesystem"
)

func TestAgentFilesystemBackendDefaultsReadWindow(t *testing.T) {
	inner := &capturingReadBackend{}
	backend := newAgentFilesystemBackend(inner)
	req := &filesystem.ReadRequest{FilePath: "/tmp/story.md"}

	content, err := backend.Read(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if content.Content != "ok" {
		t.Fatalf("unexpected read content: %q", content.Content)
	}
	if inner.lastRead == nil {
		t.Fatalf("expected underlying backend to receive read request")
	}
	if inner.lastRead.Offset != 1 {
		t.Fatalf("default read offset = %d, want 1", inner.lastRead.Offset)
	}
	if inner.lastRead.Limit != agentFileReadDefaultLimitLines {
		t.Fatalf("default read limit = %d, want %d", inner.lastRead.Limit, agentFileReadDefaultLimitLines)
	}
	if req.Offset != 0 || req.Limit != 0 {
		t.Fatalf("wrapper should not mutate caller request, got offset=%d limit=%d", req.Offset, req.Limit)
	}
}

func TestAgentFilesystemBackendPreservesExplicitReadWindow(t *testing.T) {
	inner := &capturingReadBackend{}
	backend := newAgentFilesystemBackend(inner)

	_, err := backend.Read(context.Background(), &filesystem.ReadRequest{
		FilePath: "/tmp/story.md",
		Offset:   2001,
		Limit:    agentFileReadDefaultLimitLines + 400,
	})
	if err != nil {
		t.Fatal(err)
	}
	if inner.lastRead == nil {
		t.Fatalf("expected underlying backend to receive read request")
	}
	if inner.lastRead.Offset != 2001 {
		t.Fatalf("explicit read offset = %d, want 2001", inner.lastRead.Offset)
	}
	if inner.lastRead.Limit != agentFileReadDefaultLimitLines+400 {
		t.Fatalf("explicit read limit = %d, want %d", inner.lastRead.Limit, agentFileReadDefaultLimitLines+400)
	}
}

func TestAgentFilesystemBackendNormalizesTrailingWhitespaceForUniqueEditMatch(t *testing.T) {
	filePath := writeTempFile(t, "alpha   \nbeta\t\nomega   \n")
	backend := newTestAgentFilesystemBackend(t)

	err := backend.Edit(context.Background(), &filesystem.EditRequest{
		FilePath:   filePath,
		OldString:  "alpha\nbeta\n",
		NewString:  "ALPHA\nBETA\n",
		ReplaceAll: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	got := readFile(t, filePath)
	want := "ALPHA\nBETA\nomega   \n"
	if got != want {
		t.Fatalf("edited content mismatch\ngot:  %q\nwant: %q", got, want)
	}
}

func TestAgentFilesystemBackendRejectsAmbiguousNormalizedEditMatch(t *testing.T) {
	content := "target   \nkeep\ntarget\t\n"
	filePath := writeTempFile(t, content)
	backend := newTestAgentFilesystemBackend(t)

	err := backend.Edit(context.Background(), &filesystem.EditRequest{
		FilePath:   filePath,
		OldString:  "target\n",
		NewString:  "changed\n",
		ReplaceAll: false,
	})
	if err == nil || !strings.Contains(err.Error(), "appears 2 times") {
		t.Fatalf("expected ambiguous normalized match error, got %v", err)
	}
	if got := readFile(t, filePath); got != content {
		t.Fatalf("ambiguous edit should not change file\ngot:  %q\nwant: %q", got, content)
	}
}

func TestAgentFilesystemBackendDoesNotUsePartialPrefixMatch(t *testing.T) {
	content := "alpha\nbeta\n"
	filePath := writeTempFile(t, content)
	backend := newTestAgentFilesystemBackend(t)

	err := backend.Edit(context.Background(), &filesystem.EditRequest{
		FilePath:   filePath,
		OldString:  "alpha\nchanged\n",
		NewString:  "ALPHA\nchanged\n",
		ReplaceAll: false,
	})
	if err == nil || !strings.Contains(err.Error(), "string not found") {
		t.Fatalf("expected original string not found error, got %v", err)
	}
	if got := readFile(t, filePath); got != content {
		t.Fatalf("failed edit should not change file\ngot:  %q\nwant: %q", got, content)
	}
}

func newTestAgentFilesystemBackend(t *testing.T, workspaces ...string) filesystem.Backend {
	t.Helper()
	inner, err := localbk.NewBackend(context.Background(), &localbk.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return newAgentFilesystemBackend(inner, workspaces...)
}

type capturingReadBackend struct {
	filesystem.Backend
	lastRead *filesystem.ReadRequest
}

func (b *capturingReadBackend) Read(_ context.Context, req *filesystem.ReadRequest) (*filesystem.FileContent, error) {
	if req == nil {
		return nil, fmt.Errorf("read request is nil")
	}
	next := *req
	b.lastRead = &next
	return &filesystem.FileContent{Content: "ok"}, nil
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	filePath := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return filePath
}

func readFile(t *testing.T, filePath string) string {
	t.Helper()
	content, err := os.ReadFile(filePath)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestFileReadCacheHitReturnsCachedContent(t *testing.T) {
	path := writeTempFile(t, "line one\nline two\nline three\n")
	cache := newFileReadCache(fileReadCacheDefaultMaxBytes)
	cache.set(path, "line one\nline two\nline three\n", 1, 3)

	got, ok := cache.get(path, 1, 4)
	if !ok {
		t.Fatal("cache should return cached content")
	}
	if got != "line one\nline two\nline three\n" {
		t.Fatalf("cached content = %q, want %q", got, "line one\nline two\nline three\n")
	}
}

func TestFileReadCacheMissesWhenFileModified(t *testing.T) {
	path := writeTempFile(t, "original\n")
	cache := newFileReadCache(fileReadCacheDefaultMaxBytes)
	cache.set(path, "original\n", 1, 1)

	// Modify the file on disk
	time.Sleep(10 * time.Millisecond) // ensure mtime changes on fast filesystems
	if err := os.WriteFile(path, []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, ok := cache.get(path, 1, 2); ok {
		t.Fatal("cache should miss when file mtime changed")
	}
}

func TestFileReadCacheEvictsLRUWhenFull(t *testing.T) {
	dir := t.TempDir()
	cache := newFileReadCache(20) // tiny cache, only ~20 bytes

	// Write two files, each ~15 bytes of content (total ~30 > 20)
	path1 := filepath.Join(dir, "a.txt")
	path2 := filepath.Join(dir, "b.txt")
	writeFile(t, path1, "aaaaaaaaaaaaaaa\n")
	writeFile(t, path2, "bbbbbbbbbbbbbbb\n")

	cache.set(path1, "aaaaaaaaaaaaaaa\n", 1, 1)
	cache.set(path2, "bbbbbbbbbbbbbbb\n", 1, 1)

	// The first entry should have been evicted
	if _, ok := cache.get(path1, 1, 2); ok {
		t.Fatal("oldest entry should be evicted when cache overflows")
	}
	// The second entry should still be present
	if _, ok := cache.get(path2, 1, 2); !ok {
		t.Fatal("newer entry should remain in cache")
	}
}

func TestApplyFileWindowSlicesByOffsetAndLimit(t *testing.T) {
	full := "line1\nline2\nline3\nline4\nline5"
	got, err := applyFileWindow(full, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got != "line2\nline3" {
		t.Fatalf("window [2,2] = %q, want %q", got, "line2\nline3")
	}
}

func TestApplyFileWindowClampsToEnd(t *testing.T) {
	full := "a\nb\nc"
	got, err := applyFileWindow(full, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got != "b\nc" {
		t.Fatalf("window beyond end = %q, want %q", got, "b\nc")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// capturingPathBackend 捕获 ls/glob/grep 请求，不真实访问文件系统。
type capturingPathBackend struct {
	filesystem.Backend
	lastLs   *filesystem.LsInfoRequest
	lastGlob *filesystem.GlobInfoRequest
	lastGrep *filesystem.GrepRequest
}

func (b *capturingPathBackend) LsInfo(_ context.Context, req *filesystem.LsInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, fmt.Errorf("ls request is nil")
	}
	next := *req
	b.lastLs = &next
	return nil, nil
}

func (b *capturingPathBackend) GlobInfo(_ context.Context, req *filesystem.GlobInfoRequest) ([]filesystem.FileInfo, error) {
	if req == nil {
		return nil, fmt.Errorf("glob request is nil")
	}
	next := *req
	b.lastGlob = &next
	return nil, nil
}

func (b *capturingPathBackend) GrepRaw(_ context.Context, req *filesystem.GrepRequest) ([]filesystem.GrepMatch, error) {
	if req == nil {
		return nil, fmt.Errorf("grep request is nil")
	}
	next := *req
	b.lastGrep = &next
	return nil, nil
}

func TestAgentFilesystemBackendLsAnchorsRelativeAndRejectsOutsideWorkspacePaths(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "作品 - 主")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// 存在性检查：目标路径必须真实存在，ls 才会透传到底层；这里提前建好目录。
	if err := os.MkdirAll(filepath.Join(workspace, "relative", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "卷一 - 名"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(workspace, "卷一 - 名", "第一章 开局.md"), "内容")
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)

	// 相对路径锚定到 workspace 根，避免模型复制长绝对路径时漂移。
	if _, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: "relative/dir"}); err != nil {
		t.Fatalf("relative path should anchor to workspace root: %v", err)
	}
	if inner.lastLs == nil || inner.lastLs.Path != filepath.Join(workspace, "relative", "dir") {
		t.Fatalf("relative path should anchor to workspace root, got %+v", inner.lastLs)
	}

	outside := filepath.Join(t.TempDir(), "outside.md")
	if _, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: outside}); err == nil || !strings.Contains(err.Error(), "outside the active workspace") {
		t.Fatalf("outside-workspace path should be rejected, got %v", err)
	}

	inside := filepath.Join(workspace, "卷一 - 名", "第一章 开局.md")
	if _, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: inside}); err != nil {
		t.Fatalf("in-workspace path should be accepted: %v", err)
	}
	if inner.lastLs == nil {
		t.Fatal("expected inner backend to receive ls request")
	}
	if got, want := inner.lastLs.Path, filepath.Clean(inside); got != want {
		t.Fatalf("ls path = %q, want normalized %q", got, want)
	}
}

func TestAgentFilesystemBackendLsAcceptsDotPath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)

	if _, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: "."}); err != nil {
		t.Fatalf("ls . should resolve to workspace root: %v", err)
	}
	if inner.lastLs == nil || inner.lastLs.Path != filepath.Clean(workspace) {
		t.Fatalf("ls . should resolve to workspace root, got %+v", inner.lastLs)
	}
}

func TestAgentFilesystemBackendLsDefaultsEmptyPathToWorkspaceRoot(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)

	if _, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: ""}); err != nil {
		t.Fatalf("empty ls path should fall back to workspace root: %v", err)
	}
	if inner.lastLs == nil || inner.lastLs.Path != filepath.Clean(workspace) {
		t.Fatalf("empty ls path should resolve to workspace root, got %+v", inner.lastLs)
	}
}

func TestAgentFilesystemBackendGlobAndGrepEnforceWorkspaceBounds(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	// 存在性检查：glob 的搜索根路径必须真实存在才透传到底层。
	if err := os.MkdirAll(filepath.Join(workspace, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)

	outside := filepath.Join(t.TempDir(), "outside")
	if _, err := backend.GlobInfo(context.Background(), &filesystem.GlobInfoRequest{Pattern: "*.go", Path: outside}); err == nil || !strings.Contains(err.Error(), "outside the active workspace") {
		t.Fatalf("glob outside workspace should be rejected, got %v", err)
	}
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Pattern: "foo", Path: outside}); err == nil || !strings.Contains(err.Error(), "outside the active workspace") {
		t.Fatalf("grep outside workspace should be rejected, got %v", err)
	}

	inside := filepath.Join(workspace, "src")
	if _, err := backend.GlobInfo(context.Background(), &filesystem.GlobInfoRequest{Pattern: "*.go", Path: inside}); err != nil {
		t.Fatalf("glob inside workspace should be accepted: %v", err)
	}
	if inner.lastGlob == nil || inner.lastGlob.Path != filepath.Clean(inside) {
		t.Fatalf("glob path should be passed through normalized, got %+v", inner.lastGlob)
	}
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Pattern: "foo", Path: inside}); err != nil {
		t.Fatalf("grep inside workspace should be accepted: %v", err)
	}
	if inner.lastGrep == nil || inner.lastGrep.Path != filepath.Clean(inside) {
		t.Fatalf("grep path should be passed through normalized, got %+v", inner.lastGrep)
	}

	// 空路径默认 workspace 根，grep 不落到进程工作目录。
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Pattern: "foo"}); err != nil {
		t.Fatalf("empty grep path should fall back to workspace root: %v", err)
	}
	if inner.lastGrep == nil || inner.lastGrep.Path != filepath.Clean(workspace) {
		t.Fatalf("empty grep path should resolve to workspace root, got %+v", inner.lastGrep)
	}
}

func TestAgentFilesystemBackendLsAndGlobReportMissingPath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)

	// ls 不存在的路径 → 显式 file not found（触发 path_resolution_failed 恢复提示），
	// 而非静默空结果让模型误以为是空目录。
	if _, err := backend.LsInfo(context.Background(), &filesystem.LsInfoRequest{Path: "サマーハレーション"}); err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("ls missing path should report file not found, got %v", err)
	}
	if inner.lastLs != nil {
		t.Fatalf("ls missing path should not reach inner backend, got %+v", inner.lastLs)
	}

	// glob 不存在的搜索根 → 同样显式 file not found。
	if _, err := backend.GlobInfo(context.Background(), &filesystem.GlobInfoRequest{Path: "サマーハレーション", Pattern: "**/*.txt"}); err == nil || !strings.Contains(err.Error(), "file not found") {
		t.Fatalf("glob missing root should report file not found, got %v", err)
	}
	if inner.lastGlob != nil {
		t.Fatalf("glob missing root should not reach inner backend, got %+v", inner.lastGlob)
	}
}

func TestGrepRawFallsBackOnTruncatedAndURLEncodedPath(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(workspace, "chapters", "ch00001-正文.md")
	if err := os.WriteFile(realFile, []byte("正文内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)

	// 截断路径 chapters/ch00001（缺 -正文）→ 父目录前缀唯一回退到 ch00001-正文.md。
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Path: "chapters/ch00001", Pattern: "x"}); err != nil {
		t.Fatalf("grep truncated path should fall back: %v", err)
	}
	if inner.lastGrep == nil || filepath.Clean(inner.lastGrep.Path) != filepath.Clean(realFile) {
		t.Fatalf("grep truncated path should resolve to %q, got %+v", realFile, inner.lastGrep)
	}

	// URL 编码文件名 正文 → %E6%AD%A3%E6%96%87 → URL 解码回退。
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Path: "chapters/ch00001-%E6%AD%A3%E6%96%87.md", Pattern: "x"}); err != nil {
		t.Fatalf("grep URL-encoded path should fall back: %v", err)
	}
	if inner.lastGrep == nil || filepath.Clean(inner.lastGrep.Path) != filepath.Clean(realFile) {
		t.Fatalf("grep URL-encoded path should resolve to %q, got %+v", realFile, inner.lastGrep)
	}

	// 乱编编号不唯一匹配 → 保持原路径，让底层暴露真实"路径不存在"。
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Path: "chapters/ch00099.md", Pattern: "x"}); err != nil {
		t.Fatalf("unmatched grep path should pass through, got %v", err)
	}
	if inner.lastGrep == nil || !strings.HasSuffix(filepath.ToSlash(inner.lastGrep.Path), "chapters/ch00099.md") {
		t.Fatalf("unmatched grep path should pass through unchanged, got %+v", inner.lastGrep)
	}
}
