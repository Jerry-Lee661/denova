package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cloudwego/eino/adk/filesystem"
	"github.com/cloudwego/eino/components/tool"

	"denova/internal/workspacechange"
)

func TestWorkspaceReadFileToolReturnsPartialWindowWithoutRevision(t *testing.T) {
	content := "first\nsecond\nthird\nfourth"
	path := writeTempFile(t, content)
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t))
	if err != nil {
		t.Fatal(err)
	}
	result, err := base.(tool.InvokableTool).InvokableRun(context.Background(), `{"file_path":"`+filepath.ToSlash(path)+`","offset":2,"limit":1}`)
	if err != nil {
		t.Fatal(err)
	}
	metadataLine, body, ok := strings.Cut(result, "\n")
	if !ok {
		t.Fatalf("read result has no metadata line: %q", result)
	}
	var metadata workspaceReadFileMetadata
	if err := json.Unmarshal([]byte(metadataLine), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Schema != workspaceReadFileResultSchema || metadata.Offset != 2 || metadata.Limit != 1 {
		t.Fatalf("unexpected read metadata: %#v", metadata)
	}
	var rawMetadata map[string]any
	if err := json.Unmarshal([]byte(metadataLine), &rawMetadata); err != nil {
		t.Fatal(err)
	}
	if _, ok := rawMetadata["revision"]; ok {
		t.Fatalf("read_file exposed internal revision: %s", metadataLine)
	}
	if _, ok := rawMetadata["revision_scope"]; ok {
		t.Fatalf("read_file exposed revision metadata: %s", metadataLine)
	}
	if !strings.Contains(body, "     2\tsecond") || strings.Contains(body, "first") || strings.Contains(body, "third") {
		t.Fatalf("partial cat-n selection mismatch: %q", body)
	}
}

func TestWorkspaceReadFileToolPreservesDefaultWindowSchema(t *testing.T) {
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t))
	if err != nil {
		t.Fatal(err)
	}
	info, err := base.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	for _, property := range []string{`"file_path"`, `"offset"`, `"limit"`} {
		if !strings.Contains(string(raw), property) {
			t.Fatalf("read_file schema is missing %s: %s", property, raw)
		}
	}
}

func TestWorkspaceEditFileUsesCurrentRevisionWithoutReadDependency(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "ideas.md")
	if err := os.WriteFile(path, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("manual update"), 0o644); err != nil {
		t.Fatal(err)
	}
	service, err := workspacechange.NewService(workspace)
	if err != nil {
		t.Fatal(err)
	}
	editTool, err := newWorkspaceEditFileTool(service)
	if err != nil {
		t.Fatal(err)
	}
	_, err = editTool.(tool.InvokableTool).InvokableRun(context.Background(), `{"file_path":"ideas.md","edits":[{"old_string":"manual update","new_string":"agent update"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "agent update" {
		t.Fatalf("edit_file did not apply against its current snapshot: %q", content)
	}
}

func TestWorkspaceReadFileToolRejectsPathOutsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := writeTempFile(t, "outside")
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.(tool.InvokableTool).InvokableRun(context.Background(), `{"file_path":"`+filepath.ToSlash(outside)+`"}`)
	if err == nil || !strings.Contains(err.Error(), "outside the active workspace") {
		t.Fatalf("outside read should be rejected, got %v", err)
	}
}

func TestWorkspaceReadFileToolBoundsOneVeryLongLine(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "long.txt")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", workspaceReadFileMaxSelectedBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.(tool.InvokableTool).InvokableRun(context.Background(), `{"file_path":"`+filepath.ToSlash(path)+`"}`)
	if err == nil || !strings.Contains(err.Error(), "selected read_file window exceeds") {
		t.Fatalf("oversized selected line should be rejected, got %v", err)
	}
}

func TestWorkspaceReadFileToolRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	workspace := t.TempDir()
	outside := writeTempFile(t, "outside")
	link := filepath.Join(workspace, "escape.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.(tool.InvokableTool).InvokableRun(context.Background(), `{"file_path":"`+link+`"}`)
	if err == nil {
		t.Fatal("workspace read must not follow a symlink outside the active workspace")
	}
}

func TestNormalizeWorkspaceFilePathCollapsesSpacesAroundDashes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"chapters/v00001-第一卷 - 诅咒的觉醒/ch00001-第一章 - 真空外出的觉醒.md", "chapters/v00001-第一卷-诅咒的觉醒/ch00001-第一章-真空外出的觉醒.md"},
		{"chapters/v00001-第一卷-诅咒的觉醒/ch00001-第一章-真空外出的觉醒.md", "chapters/v00001-第一卷-诅咒的觉醒/ch00001-第一章-真空外出的觉醒.md"},
		{"chapters/v00001-第一卷 -诅咒的觉醒/ch00001-第一章- 真空外出的觉醒.md", "chapters/v00001-第一卷-诅咒的觉醒/ch00001-第一章-真空外出的觉醒.md"},
		{"no change needed", "no change needed"},
		{"a - b - c", "a-b-c"},
		{"", ""},
	}
	for _, tc := range tests {
		got := normalizeWorkspaceFilePath(tc.input)
		if got != tc.want {
			t.Fatalf("normalizeWorkspaceFilePath(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestOpenWorkspaceFileFallsBackToNormalizedPath(t *testing.T) {
	dir := t.TempDir()
	realFile := filepath.Join(dir, "ch00001-第一章-真空外出的觉醒.md")
	if err := os.WriteFile(realFile, []byte("test content"), 0o644); err != nil {
		t.Fatal(err)
	}

	// File exists at the normalized path; try opening the variant with spaces.
	f, err := openWorkspaceFile(dir, "ch00001-第一章 - 真空外出的觉醒.md")
	if err != nil {
		t.Fatalf("openWorkspaceFile should fall back to normalized path: %v", err)
	}
	f.Close()

	// Opening with the exact path should also work.
	f2, err := openWorkspaceFile(dir, "ch00001-第一章-真空外出的觉醒.md")
	if err != nil {
		t.Fatalf("openWorkspaceFile should work with exact path: %v", err)
	}
	f2.Close()
}

func TestOpenWorkspaceFileURLDecodesPercentEncodedName(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(dir, "chapters", "ch00001-正文.md")
	if err := os.WriteFile(realFile, []byte("正文内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模型把中文文件名编码成 %XX（“正文” → %E6%AD%A3%E6%96%87），应通过 URL 解码容错打开。
	f, err := openWorkspaceFile(dir, "chapters/ch00001-%E6%AD%A3%E6%96%87.md")
	if err != nil {
		t.Fatalf("openWorkspaceFile should URL-decode percent-encoded name: %v", err)
	}
	f.Close()

	// 原始真实路径也应正常打开。
	f2, err := openWorkspaceFile(dir, "chapters/ch00001-正文.md")
	if err != nil {
		t.Fatalf("openWorkspaceFile should work with exact path: %v", err)
	}
	f2.Close()
}

func TestOpenWorkspaceFileUniquePrefixFallback(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(dir, "chapters", "ch00001-正文.md")
	if err := os.WriteFile(realFile, []byte("正文内容"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 模型跨调用丢失 CJK 字符：ch00001-正文.md → ch00001-.md。
	f, err := openWorkspaceFile(dir, "chapters/ch00001-.md")
	if err != nil {
		t.Fatalf("unique prefix fallback should resolve ch00001-.md: %v", err)
	}
	f.Close()

	// 后缀记错：.txt vs .md。
	f2, err := openWorkspaceFile(dir, "chapters/ch00001-正文.txt")
	if err != nil {
		t.Fatalf("unique prefix fallback should resolve .txt variant: %v", err)
	}
	f2.Close()

	// 乱编的编号不唯一匹配，应保持失败。
	if _, err := openWorkspaceFile(dir, "chapters/ch00099.md"); err == nil {
		t.Fatal("guessed unrelated filename should not resolve via fallback")
	}
}

func TestOpenWorkspaceFileUniquePrefixFallbackRefusesAmbiguous(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chapters", "ch00001-正文.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chapters", "ch00001-序章.md"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 两个 ch00001 开头文件，前缀不唯一，不应回退。
	if _, err := openWorkspaceFile(dir, "chapters/ch00001-.md"); err == nil {
		t.Fatal("ambiguous prefix should not resolve via fallback")
	}
}

func TestFilePathRecoveryHintIncludesWorkspaceRoot(t *testing.T) {
	workspace := `D:\作品\克拉索奥斯的魔法少女`
	hint, ok := filePathRecoveryHint(workspace, "read_file", fmt.Errorf("file not found: x"))
	if !ok {
		t.Fatal("expected recovery hint for read_file file-not-found")
	}
	if !strings.Contains(hint, "当前作品根目录") || !strings.Contains(hint, workspace) {
		t.Fatalf("recovery hint should include workspace root: %s", hint)
	}
	if !strings.Contains(hint, "path_resolution_failed") {
		t.Fatalf("recovery hint should keep structured type: %s", hint)
	}
}

func TestFilePathRecoveryHintIgnoresUnrelatedTools(t *testing.T) {
	if hint, ok := filePathRecoveryHint(`D:\ws`, "write_file", fmt.Errorf("file not found: x")); ok {
		t.Fatalf("write_file should not get path recovery hint, got %q", hint)
	}
}

func TestResolveAgentToolPathAnchorsRelativeToWorkspaceRoot(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "作品")
	absolute, relative, err := resolveAgentToolPath(workspace, "chapters/ch00001-正文.md")
	if err != nil {
		t.Fatal(err)
	}
	wantAbs := filepath.Join(workspace, "chapters", "ch00001-正文.md")
	if absolute != filepath.Clean(wantAbs) {
		t.Fatalf("absolute = %q, want %q", absolute, filepath.Clean(wantAbs))
	}
	if relative != "chapters/ch00001-正文.md" {
		t.Fatalf("relative = %q, want chapters/ch00001-正文.md", relative)
	}
}

func TestResolveAgentToolPathRejectsRelativeEscape(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "作品")
	if _, _, err := resolveAgentToolPath(workspace, "../secret.md"); err == nil || !strings.Contains(err.Error(), "outside the active workspace") {
		t.Fatalf("relative escape should be rejected, got %v", err)
	}
	if _, _, err := resolveAgentToolPath(workspace, `..\..\etc\passwd`); err == nil || !strings.Contains(err.Error(), "outside the active workspace") {
		t.Fatalf("backslash relative escape should be rejected, got %v", err)
	}
}

func TestResolveAgentToolPathStripsWorkspaceRootNamePrefix(t *testing.T) {
	// 模型常把作品根目录名本身当作 path 首段传入（如 ls サマーハレーション、
	// read_file サマーハレーション/chapters/x.md），此时 workspace 根就是该目录名，
	// 双重嵌套（workspace/<根目录名>/...）必然失败。应剥掉该首段再锚定。
	rootName := "サマーハレーション"
	workspace := filepath.Join(t.TempDir(), rootName)

	// ls 作品根目录名 → 落到 workspace 根。
	absolute, relative, err := resolveAgentToolPath(workspace, rootName)
	if err != nil {
		t.Fatalf("workspace root name as path should be accepted: %v", err)
	}
	if absolute != filepath.Clean(workspace) {
		t.Fatalf("absolute = %q, want workspace root %q", absolute, filepath.Clean(workspace))
	}
	if relative != "." {
		t.Fatalf("relative = %q, want .", relative)
	}

	// 作品根目录名 + 子路径 → 剥掉首段后锚定到子路径。
	absolute, relative, err = resolveAgentToolPath(workspace, rootName+"/chapters/ch00001-正文.md")
	if err != nil {
		t.Fatalf("workspace root name + child path should be accepted: %v", err)
	}
	if want := filepath.Join(workspace, "chapters", "ch00001-正文.md"); absolute != filepath.Clean(want) {
		t.Fatalf("absolute = %q, want %q", absolute, filepath.Clean(want))
	}
	if relative != "chapters/ch00001-正文.md" {
		t.Fatalf("relative = %q, want chapters/ch00001-正文.md", relative)
	}
}

func TestResolveAgentToolPathKeepsNormalRelativePath(t *testing.T) {
	// 非作品根目录名首段的普通相对路径不受剥除影响。
	workspace := filepath.Join(t.TempDir(), "サマーハレーション")
	absolute, relative, err := resolveAgentToolPath(workspace, "chapters/ch00001-正文.md")
	if err != nil {
		t.Fatalf("normal relative path should be accepted: %v", err)
	}
	if want := filepath.Join(workspace, "chapters", "ch00001-正文.md"); absolute != filepath.Clean(want) {
		t.Fatalf("absolute = %q, want %q", absolute, filepath.Clean(want))
	}
	if relative != "chapters/ch00001-正文.md" {
		t.Fatalf("relative = %q, want chapters/ch00001-正文.md", relative)
	}
}

func TestReadFileDirectoryReturnsLsHint(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.(tool.InvokableTool).InvokableRun(context.Background(), `{"file_path":"chapters"}`)
	if err == nil {
		t.Fatal("reading a directory should error")
	}
	if !strings.Contains(err.Error(), "is a directory, not a file") || !strings.Contains(err.Error(), "ls") {
		t.Fatalf("expected directory→ls hint, got: %v", err)
	}
}

func TestDecodeUnicodeEscapeSequences(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain no escapes", "chapters/ch00001.md", "chapters/ch00001.md"},
		{"cjk escapes", `chapters/ch00001-\u514b\u6731.md`, "chapters/ch00001-克朱.md"},
		{"surrogate pair", `\uD83D\uDE00`, "😀"},
		{"control char kept", `a\u0000b`, `a\u0000b`},
		{"lone low surrogate kept", `a\uDE00b`, `a\uDE00b`},
		{"windows abs path untouched", `C:\Users\zy_le\x`, `C:\Users\zy_le\x`},
		{"mixed escape in middle", `x-\u514b y`, "x-克 y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decodeUnicodeEscapeSequences(c.in); got != c.want {
				t.Fatalf("decodeUnicodeEscapeSequences(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestOpenWorkspaceFileDecodesUnicodeEscapedName(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	realName := "ch00001-克朱.md"
	if err := os.WriteFile(filepath.Join(workspace, "chapters", realName), []byte("正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := openWorkspaceFile(workspace, `chapters/ch00001-\u514b\u6731.md`)
	if err != nil {
		t.Fatalf("escaped path should open real file: %v", err)
	}
	f.Close()
}

func TestGrepRawFallsBackOnUnicodeEscapedPath(t *testing.T) {
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	realFile := filepath.Join(workspace, "chapters", "ch00001-克朱.md")
	if err := os.WriteFile(realFile, []byte("正文"), 0o644); err != nil {
		t.Fatal(err)
	}
	inner := &capturingPathBackend{}
	backend := newAgentFilesystemBackend(inner, workspace)
	if _, err := backend.GrepRaw(context.Background(), &filesystem.GrepRequest{Path: `chapters/ch00001-\u514b\u6731.md`, Pattern: "x"}); err != nil {
		t.Fatalf("grep escaped path should fall back: %v", err)
	}
	if inner.lastGrep == nil || filepath.Clean(inner.lastGrep.Path) != filepath.Clean(realFile) {
		t.Fatalf("grep escaped path should resolve to %q, got %+v", realFile, inner.lastGrep)
	}
}

// T2a: 默认块足够大——346KB 章节（约 4300 行）应能在 1~3 次内读完。
// 默认 limit 为 agentFileReadDefaultLimitLines（2000 行），2 次即可覆盖 4300 行。
func TestReadFileDefaultLimitCoversLargeChapterInFewReads(t *testing.T) {
	if agentFileReadDefaultLimitLines < 1000 {
		t.Fatalf("default limit %d too small to read a 346KB chapter in 1~3 reads", agentFileReadDefaultLimitLines)
	}
	// 4300 行章节，默认 2000 行/次 → 3 次读完（2000+2000+300）
	chapterLines := 4300
	reads := 0
	offset := 1
	for offset <= chapterLines {
		batch := agentFileReadDefaultLimitLines
		if offset+batch-1 > chapterLines {
			batch = chapterLines - offset + 1
		}
		offset += batch
		reads++
	}
	if reads > 3 {
		t.Fatalf("default limit requires %d reads for a %d-line chapter, want <=3", reads, chapterLines)
	}
}

// T2a: 同 run 内重复读取同一窗口 → 返回去重占位（含 [dedup] 标记），而非再次返回全文。
func TestReadFileDedupOnSameRunSameWindow(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "ch.md")
	content := "line1\nline2\nline3\nline4\nline5"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	observer := newRunObserver(nil, "root")
	ctx := ContextWithRunObserver(context.Background(), observer)
	args := `{"file_path":"ch.md","offset":1,"limit":3}`

	first, err := base.(tool.InvokableTool).InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(first, "[dedup]") {
		t.Fatalf("first read should not be deduped: %q", first)
	}
	if !strings.Contains(first, "line1") || !strings.Contains(first, "line3") {
		t.Fatalf("first read should contain full content: %q", first)
	}

	second, err := base.(tool.InvokableTool).InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second, "[dedup]") {
		t.Fatalf("second read of same window should be deduped, got: %q", second)
	}
	if !strings.Contains(second, "grep") {
		t.Fatalf("dedup placeholder should guide to grep: %q", second)
	}
	// 占位不应再次注入全文正文
	if strings.Contains(second, "line1\t") || strings.Contains(second, "     1\tline1") {
		t.Fatalf("dedup placeholder should not re-inject full content: %q", second)
	}
}

// T2a: 不同窗口（不同 offset/limit）不应误判为重复 → 返回全文。
func TestReadFileDedupDifferentWindowNotMisHit(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "ch.md")
	content := "line1\nline2\nline3\nline4\nline5"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	observer := newRunObserver(nil, "root")
	ctx := ContextWithRunObserver(context.Background(), observer)

	if _, err := base.(tool.InvokableTool).InvokableRun(ctx, `{"file_path":"ch.md","offset":1,"limit":3}`); err != nil {
		t.Fatal(err)
	}
	// 不同 offset → 不应去重
	next, err := base.(tool.InvokableTool).InvokableRun(ctx, `{"file_path":"ch.md","offset":4,"limit":2}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(next, "[dedup]") {
		t.Fatalf("different offset should not be deduped: %q", next)
	}
	if !strings.Contains(next, "line4") {
		t.Fatalf("different offset should return its content: %q", next)
	}
}

// T2a: 文件内容变化（modTime/size 改变）后重读同一窗口 → 不应去重。
func TestReadFileDedupSkippedWhenFileChanged(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "ch.md")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	observer := newRunObserver(nil, "root")
	ctx := ContextWithRunObserver(context.Background(), observer)
	args := `{"file_path":"ch.md","offset":1,"limit":3}`

	if _, err := base.(tool.InvokableTool).InvokableRun(ctx, args); err != nil {
		t.Fatal(err)
	}
	// 修改文件内容（size 变化）
	time.Sleep(20 * time.Millisecond) // 确保 modTime 不同
	if err := os.WriteFile(path, []byte("line1\nline2\nline3\nline4\nline5\nline6"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := base.(tool.InvokableTool).InvokableRun(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second, "[dedup]") {
		t.Fatalf("changed file should not be deduped: %q", second)
	}
}

// T2a: 无 RunObserver（ctx 未注入）时不去重（保持向后兼容）。
func TestReadFileNoDedupWithoutObserver(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "ch.md")
	if err := os.WriteFile(path, []byte("line1\nline2\nline3"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t, workspace), workspace)
	if err != nil {
		t.Fatal(err)
	}
	args := `{"file_path":"ch.md","offset":1,"limit":3}`
	first, err := base.(tool.InvokableTool).InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	second, err := base.(tool.InvokableTool).InvokableRun(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(second, "[dedup]") {
		t.Fatalf("no observer should not dedup: %q", second)
	}
	if first != second {
		t.Fatalf("both reads should return identical full content without observer")
	}
}

// T2a: 工具描述包含 grep 引导（双语）。
func TestReadFileDescriptionContainsGrepGuidance(t *testing.T) {
	base, err := newWorkspaceReadFileTool(newTestAgentFilesystemBackend(t))
	if err != nil {
		t.Fatal(err)
	}
	info, err := base.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc := info.Desc
	if !strings.Contains(desc, "grep") {
		t.Fatalf("description should mention grep: %q", desc)
	}
	if !strings.Contains(desc, "关键词") {
		t.Fatalf("description should have Chinese grep guidance: %q", desc)
	}
	if !strings.Contains(desc, "dedup") && !strings.Contains(desc, "去重") {
		t.Fatalf("description should mention dedup: %q", desc)
	}
}
