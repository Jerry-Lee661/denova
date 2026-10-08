package book

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleEPUBBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "sample.epub"))
	if err != nil {
		t.Fatalf("read testdata/sample.epub: %v", err)
	}
	return data
}

func TestPreviewNovelImportEPUBMirrorsNavTree(t *testing.T) {
	preview, err := PreviewNovelImport("初雪.epub", sampleEPUBBytes(t))
	if err != nil {
		t.Fatalf("PreviewNovelImport: %v", err)
	}
	if preview.SplitStrategy != NovelImportSplitStrategyTOCTree {
		t.Fatalf("SplitStrategy = %q, want %q", preview.SplitStrategy, NovelImportSplitStrategyTOCTree)
	}
	if preview.ChapterCount != 6 {
		t.Fatalf("ChapterCount = %d, want 6", preview.ChapterCount)
	}
	if preview.Language != NovelImportLanguageChinese {
		t.Fatalf("Language = %q, want %q", preview.Language, NovelImportLanguageChinese)
	}
	type want struct {
		title  string
		volume string
	}
	wants := []want{
		{"第一章 初雪", ""},
		{"第二章 夜行", ""},
		{"第三章 归途", ""},
		{"附录图表 · 附录A", "附录"},
		{"附录图表 · 附录B", "附录"},
		{"孤儿页", ""},
	}
	for i, w := range wants {
		if preview.Chapters[i].Title != w.title {
			t.Fatalf("Chapters[%d].Title = %q, want %q", i, preview.Chapters[i].Title, w.title)
		}
		if preview.Chapters[i].Volume != w.volume {
			t.Fatalf("Chapters[%d].Volume = %q, want %q", i, preview.Chapters[i].Volume, w.volume)
		}
		if preview.Chapters[i].Chars == 0 {
			t.Fatalf("Chapters[%d] is empty", i)
		}
	}
}

func TestImportNovelToWorkspaceWritesEPUBChapters(t *testing.T) {
	workspace := t.TempDir()
	preview, paths, _, err := ImportNovelToWorkspace(workspace, "初雪.epub", sampleEPUBBytes(t))
	if err != nil {
		t.Fatalf("ImportNovelToWorkspace: %v", err)
	}
	if len(paths) != 6 {
		t.Fatalf("paths = %v, want 6 chapters", paths)
	}
	// The grouped appendix leaves land in the 附录 volume directory.
	content, err := os.ReadFile(filepath.Join(workspace, preview.Chapters[3].Path))
	if err != nil {
		t.Fatalf("read chapter 4: %v", err)
	}
	if !strings.Contains(string(content), "附录A 的第一段内容。") {
		t.Fatalf("chapter 4 content lost UTF-8 text: %q", content)
	}
	if !strings.Contains(preview.Chapters[3].VolumePath, "附录") {
		t.Fatalf("chapter 4 VolumePath = %q, want the 附录 volume dir", preview.Chapters[3].VolumePath)
	}
}

func TestPreviewNovelImportRejectsBrokenEPUB(t *testing.T) {
	if _, err := PreviewNovelImport("broken.epub", []byte("not an epub file")); err == nil {
		t.Fatalf("expected broken epub error")
	}
}
