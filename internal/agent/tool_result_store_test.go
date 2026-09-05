package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreviewBytesRuneBoundary 验证字节有界预览在 rune 边界截断并追加省略标记。
func TestPreviewBytesRuneBoundary(t *testing.T) {
	const max = 10
	// "abcde" 5 字节，直接返回。
	if got := PreviewBytes("abcde", max); got != "abcde" {
		t.Fatalf("short content should be returned unchanged: %q", got)
	}
	// 中文每个字符 3 字节，10 字节只能容纳 2 个完整字符（6 字节）后截断到 rune 边界。
	cn := "你好世界"
	got := PreviewBytes(cn, max)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("preview should end with ellipsis: %q", got)
	}
	// 截断点必须落在 rune 边界上（去掉省略标记后是完整字符）。
	body := strings.TrimSuffix(got, "…")
	if len([]byte(body))%3 != 0 {
		t.Fatalf("truncated body should land on a UTF-8 rune boundary: %q", body)
	}
}

// TestPreviewBytesMultiByteTruncation 验证截断不会切到多字节字符中间。
func TestPreviewBytesMultiByteTruncation(t *testing.T) {
	got := PreviewBytes(strings.Repeat("你", 20), 7)
	// 省略标记「…」占 3 字节，正文部分落在 7 字节以内。
	body := strings.TrimSuffix(got, "…")
	if len([]byte(body)) > 7 {
		t.Fatalf("body byte limit violated: %d", len([]byte(body)))
	}
	if len([]byte(body))%3 != 0 {
		t.Fatalf("truncated body should land on a UTF-8 rune boundary: %q", body)
	}
}

// TestStoreLoadRoundTrip 验证 Store/Load 往返一致，位置引用稳定。
func TestStoreLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewResultStore(dir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("line\n", 5000)
	location, preview := store.Store("result-key", content)
	if location == "" {
		t.Fatal("store must return a non-empty location")
	}
	// 同一 key 返回相同位置（逐字节稳定）。
	location2, _ := store.Store("result-key", content)
	if location != location2 {
		t.Fatalf("location must be stable across calls: %q vs %q", location, location2)
	}
	loaded, err := store.Load("result-key")
	if err != nil {
		t.Fatalf("load should succeed: %v", err)
	}
	if loaded != content {
		t.Fatal("loaded content must equal original (externalize does not truncate)")
	}
	// 预览字节有界（含省略标记「…」3 字节，允许 +3）。
	if len([]byte(preview)) > defaultExternalizePreviewBytes+3 {
		t.Fatalf("preview exceeds configured bytes: %d", len([]byte(preview)))
	}
}

// TestStoreEmptyDir 验证未配置目录时仍返回位置引用与预览（不写盘）。
func TestStoreEmptyDir(t *testing.T) {
	store := &ResultStore{sessionID: "s"}
	location, preview := store.Store("key", "hello")
	if location == "" || preview == "" {
		t.Fatal("empty-dir store must still return location and preview")
	}
	// Load 在未配置目录时应报错。
	if _, err := store.Load("key"); err == nil {
		t.Fatal("Load on empty-dir store should error")
	}
}

// TestExternalizeIfNeededThreshold 验证超过阈值整体外置、未超过则原地裁剪。
func TestExternalizeIfNeededThreshold(t *testing.T) {
	dir := t.TempDir()
	store, err := NewResultStore(dir, "sess")
	if err != nil {
		t.Fatal(err)
	}
	// 小结果（低于 read_file 专属阈值）：不外置。
	small := strings.Repeat("x", readFileExternalizeThreshold-1)
	filtered, ext := store.ExternalizeIfNeeded("read_file", `{"file_path":"a"}`, small, defaultToolResultMaxBytes)
	if ext {
		t.Fatal("below threshold should not externalize")
	}
	if filtered.Location != "" || filtered.Preview != "" || filtered.Externalized {
		t.Fatal("non-externalized result should leave location/preview/externalized empty")
	}

	// 大结果：外置，预览 + 位置引用 + 元数据。
	big := strings.Repeat("y", ExternalizeThreshold+1024)
	filtered, ext = store.ExternalizeIfNeeded("read_file", `{"file_path":"b"}`, big, defaultToolResultMaxBytes)
	if !ext {
		t.Fatal("above threshold should externalize")
	}
	if !filtered.Externalized || filtered.Location == "" || filtered.Preview == "" {
		t.Fatalf("externalized result must carry location and preview: %#v", filtered)
	}
	if !strings.Contains(filtered.Content, "tool result externalized to disk") {
		t.Fatalf("content should include location reference: %s", filtered.Content)
	}
	if !strings.Contains(filtered.Content, "idempotency_key:") {
		t.Fatal("content should include metadata with idempotency key")
	}
}

// TestExternalizeThresholdPerTool 验证外置阈值按工具区分：
// read_file 使用更低的 readFileExternalizeThreshold（更激进），
// 其余工具使用默认 ExternalizeThreshold。
func TestExternalizeThresholdPerTool(t *testing.T) {
	// 阈值函数本身的行为。
	if got := externalizeThresholdForTool("read_file"); got != readFileExternalizeThreshold {
		t.Fatalf("read_file threshold = %d, want %d", got, readFileExternalizeThreshold)
	}
	if got := externalizeThresholdForTool("grep"); got != ExternalizeThreshold {
		t.Fatalf("grep threshold = %d, want %d", got, ExternalizeThreshold)
	}
	if got := externalizeThresholdForTool("bash"); got != ExternalizeThreshold {
		t.Fatalf("bash threshold = %d, want %d", got, ExternalizeThreshold)
	}
	// 大小写归一化。
	if got := externalizeThresholdForTool("Read_File"); got != readFileExternalizeThreshold {
		t.Fatalf("Read_File threshold = %d, want %d", got, readFileExternalizeThreshold)
	}

	dir := t.TempDir()
	store, err := NewResultStore(dir, "sess")
	if err != nil {
		t.Fatal(err)
	}
	// 介于两个阈值之间的结果：read_file 外置，grep 不外置。
	mid := strings.Repeat("m", readFileExternalizeThreshold+1024)
	if midLen := len(mid); midLen >= ExternalizeThreshold {
		t.Fatalf("test setup: mid size %d should be below ExternalizeThreshold %d", midLen, ExternalizeThreshold)
	}
	_, extRead := store.ExternalizeIfNeeded("read_file", `{"file_path":"mid"}`, mid, defaultToolResultMaxBytes)
	if !extRead {
		t.Fatal("read_file above its lower threshold should externalize")
	}
	_, extGrep := store.ExternalizeIfNeeded("grep", `{"pattern":"x"}`, mid, defaultToolResultMaxBytes)
	if extGrep {
		t.Fatal("grep below default threshold should not externalize")
	}
}

// TestExternalizeStablePreview 验证外置预览进入前缀后逐字节稳定（同一 key）。
func TestExternalizeStablePreview(t *testing.T) {
	dir := t.TempDir()
	store, err := NewResultStore(dir, "sess")
	if err != nil {
		t.Fatal(err)
	}
	big := strings.Repeat("z", ExternalizeThreshold+512)
	first, _ := store.ExternalizeIfNeeded("read_file", `{"file_path":"c"}`, big, defaultToolResultMaxBytes)
	// 再次调用同一参数，预览应完全一致。
	filtered, _ := store.ExternalizeIfNeeded("read_file", `{"file_path":"c"}`, big, defaultToolResultMaxBytes)
	if filtered.Preview != first.Preview {
		t.Fatalf("preview must be byte-stable across rounds: %q vs %q", first.Preview, filtered.Preview)
	}
}

// TestMicroCompactResultHotCold 验证微压缩热/冷两种形态。
func TestMicroCompactResultHotCold(t *testing.T) {
	result := FilteredToolResult{
		Content:        "full body content",
		Location:       "sess/key.txt",
		IdempotencyKey: "read_file:abc",
		Manifest:       ToolManifest{Name: "read_file"},
		OriginalBytes:  100,
	}
	// 热缓存：保留正文。
	hot := MicroCompactResult(result, true)
	if hot != "full body content" {
		t.Fatalf("hot cache should keep full content: %q", hot)
	}
	// 冷缓存：换成占位文本，含位置引用。
	cold := MicroCompactResult(result, false)
	if !strings.Contains(cold, "sess/key.txt") {
		t.Fatalf("cold cache placeholder should reference location: %q", cold)
	}
	if strings.Contains(cold, "full body content") {
		t.Fatal("cold cache should drop the full body")
	}
}

// TestExternalizeLocationReference 验证位置引用文案。
func TestExternalizeLocationReference(t *testing.T) {
	if got := ExternalizeLocationReference(""); got != "[tool result externalized to disk]" {
		t.Fatalf("empty location reference wrong: %q", got)
	}
	if got := ExternalizeLocationReference("sess/a.txt"); !strings.Contains(got, "sess/a.txt") {
		t.Fatalf("location reference should embed path: %q", got)
	}
}

// TestFilterToolResultMiddleware 验证 middleware.filterToolResult 在配置存储时走外置分支。
func TestFilterToolResultMiddleware(t *testing.T) {
	dir := t.TempDir()
	store, err := NewResultStore(dir, "sess")
	if err != nil {
		t.Fatal(err)
	}
	mw := &toolOrchestratorMiddleware{resultStore: store, toolResultMaxBytes: defaultToolResultMaxBytes}
	big := strings.Repeat("q", ExternalizeThreshold+256)
	filtered := mw.filterToolResult("read_file", `{"file_path":"d"}`, big)
	if !filtered.Externalized {
		t.Fatal("middleware should externalize large results when store configured")
	}
	if filtered.Manifest.Name == "" {
		t.Fatal("manifest name must be populated")
	}
}

// TestNewResultStoreForWorkspace 验证按工作区派生存储目录与会话 ID。
func TestNewResultStoreForWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), ".denova-workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	store, err := newResultStoreForWorkspace(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if store.sessionID == "" {
		t.Fatal("session id should be derived from workspace base name")
	}
	if store.dir == "" {
		t.Fatal("dir should be configured for non-empty workspace")
	}
	// 空工作区退化为无目录存储（dir 与 sessionID 均空）。
	empty, err := newResultStoreForWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	if empty.dir != "" || empty.sessionID != "" {
		t.Fatal("empty workspace should yield empty-dir store with empty session id")
	}
}
