package book

import (
	"os"
	"path/filepath"
	"testing"
)

// writeChapterFile 在 workspace 下创建章节文件（自动建目录）。
func writeChapterFile(t *testing.T, workspace, rel string) {
	t.Helper()
	abs := filepath.Join(workspace, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("创建章节目录失败: %v", err)
	}
	if err := os.WriteFile(abs, []byte("正文内容\n"), 0o644); err != nil {
		t.Fatalf("写入章节失败: %v", err)
	}
}

func TestChapterAliasesEmptyWorkspace(t *testing.T) {
	workspace := t.TempDir()
	svc := NewService(workspace)
	aliases, err := svc.ChapterAliases()
	if err != nil {
		t.Fatalf("ChapterAliases 返回错误: %v", err)
	}
	if len(aliases) != 0 {
		t.Fatalf("空 workspace 应返回空列表，得到 %d 条", len(aliases))
	}
}

func TestChapterAliasesFlatAndVolumes(t *testing.T) {
	workspace := t.TempDir()
	writeChapterFile(t, workspace, "chapters/ch00002-第二章.md")
	writeChapterFile(t, workspace, "chapters/ch00001-正文.md")
	writeChapterFile(t, workspace, "chapters/v00001-第一卷/ch00003-第三卷内.md")
	// 非章节文件与隐藏文件应被忽略
	if err := os.WriteFile(filepath.Join(workspace, "chapters", "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写入 README 失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "chapters", ".hidden.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("写入隐藏文件失败: %v", err)
	}

	svc := NewService(workspace)
	aliases, err := svc.ChapterAliases()
	if err != nil {
		t.Fatalf("ChapterAliases 返回错误: %v", err)
	}
	if len(aliases) != 3 {
		t.Fatalf("应返回 3 个章节，得到 %d: %+v", len(aliases), aliases)
	}
	// 按 index 升序
	for i := 1; i < len(aliases); i++ {
		if aliases[i-1].Index >= aliases[i].Index {
			t.Fatalf("章节未按 index 排序: %+v", aliases)
		}
	}
	byIndex := map[int]ChapterAlias{}
	for _, a := range aliases {
		byIndex[a.Index] = a
	}
	if got := byIndex[1].Path; got != "chapters/ch00001-正文.md" {
		t.Fatalf("index 1 期望 chapters/ch00001-正文.md，得到 %q", got)
	}
	if got := byIndex[3].Path; got != "chapters/v00001-第一卷/ch00003-第三卷内.md" {
		t.Fatalf("index 3 期望含卷路径，得到 %q", got)
	}
	if byIndex[3].Volume == "" {
		t.Fatalf("卷内章节应有 Volume 信息: %+v", byIndex[3])
	}
}
