package book

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newExportTestWorkspace builds a book workspace with one volume and three
// chapters, one of which is empty and must be skipped by every exporter.
func newExportTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	chapterDir := filepath.Join(root, "chapters", "v00001-第一卷-风起")
	if err := os.MkdirAll(chapterDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"ch00001-第一章-开局.md": "第一章 开局\n\n天亮了。\n\n林川看向窗外。",
		"ch00002-第二章-追光.md":  "# 第二章 追光\n\n林川踏入雨夜。",
		"ch00003-第三章-空章.md":  "",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(chapterDir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestServiceExportDocumentKeepsVolumeContext(t *testing.T) {
	root := newExportTestWorkspace(t)
	document, err := NewService(root).ExportDocument(BookMeta{Title: "星河边境", Author: "Denova"})
	if err != nil {
		t.Fatal(err)
	}
	if document.ChapterCount != 2 || len(document.Chapters) != 2 {
		t.Fatalf("expected 2 exportable chapters, got count=%d len=%d", document.ChapterCount, len(document.Chapters))
	}
	if document.Chapters[0].Volume != "第一卷 风起" {
		t.Fatalf("first chapter should carry the volume header, got %q", document.Chapters[0].Volume)
	}
	if document.Chapters[1].Volume != "" {
		t.Fatalf("same-volume chapters should not repeat the volume header, got %q", document.Chapters[1].Volume)
	}
	if document.Chapters[1].Title != "第二章 追光" || strings.Contains(document.Chapters[1].Body, "第二章") {
		t.Fatalf("chapter title should be separated from body: %#v", document.Chapters[1])
	}
}

func TestServiceExportEPUBProducesValidPackage(t *testing.T) {
	root := newExportTestWorkspace(t)
	data, err := NewService(root).ExportEPUB(BookMeta{Title: "星河边境", Author: "Denova"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("epub should be a valid zip: %v", err)
	}
	foundMimetype := false
	names := make([]string, 0, len(reader.File))
	for _, f := range reader.File {
		names = append(names, f.Name)
		if f.Name == "mimetype" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			content, _ := io.ReadAll(rc)
			rc.Close()
			if strings.TrimSpace(string(content)) != "application/epub+zip" {
				t.Fatalf("mimetype = %q", content)
			}
			foundMimetype = true
		}
	}
	if !foundMimetype {
		t.Fatalf("epub package missing mimetype entry, files=%v", names)
	}
	joined := strings.Join(names, "\n")
	if !strings.Contains(joined, ".opf") || !strings.Contains(joined, ".ncx") {
		t.Fatalf("epub package missing opf/ncx, files=%v", names)
	}
}

func TestServiceExportDOCXContainsHeadings(t *testing.T) {
	root := newExportTestWorkspace(t)
	data, err := NewService(root).ExportDOCX(BookMeta{Title: "星河边境", Author: "Denova"})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("docx should be a valid zip: %v", err)
	}
	var documentXML []byte
	for _, f := range reader.File {
		if f.Name == "word/document.xml" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			documentXML, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	if documentXML == nil {
		t.Fatal("docx package missing word/document.xml")
	}
	text := string(documentXML)
	for _, want := range []string{"星河边境", "第一卷 风起", "第一章 开局", "林川看向窗外。", "Heading1", "Heading2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("document.xml should contain %q", want)
		}
	}
	if strings.Contains(text, "第三章") {
		t.Fatal("empty chapter must not be exported")
	}
}
