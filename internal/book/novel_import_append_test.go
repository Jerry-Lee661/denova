package book

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildAppendEPUB builds a small EPUB whose nav declares a leaf under the
// existing 附录 group plus a fresh 卷丙 group with two leaves.
func buildAppendEPUB(t *testing.T) []byte {
	t.Helper()
	titles := map[string]string{
		"xuxu.xhtml": "附录续篇",
		"x1.xhtml":   "新章一",
		"x2.xhtml":   "新章二",
	}
	files := []struct {
		name string
		body string
	}{
		{"mimetype", "application/epub+zip"},
		{"META-INF/container.xml", `<?xml version="1.0" encoding="utf-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles>
</container>`},
		{"OEBPS/content.opf", `<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="uid">urn:uuid:denova-append</dc:identifier>
    <dc:title>续篇</dc:title>
    <dc:language>zh</dc:language>
    <meta property="dcterms:modified">2026-01-02T00:00:00Z</meta>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="xuxu" href="xuxu.xhtml" media-type="application/xhtml+xml"/>
    <item id="x1" href="x1.xhtml" media-type="application/xhtml+xml"/>
    <item id="x2" href="x2.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="xuxu"/>
    <itemref idref="x1"/>
    <itemref idref="x2"/>
  </spine>
</package>`},
		{"OEBPS/nav.xhtml", `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml">
<head><title>目录</title></head>
<body>
<nav epub:type="toc" xmlns:epub="http://www.idpf.org/2007/ops" id="toc">
  <ol>
    <li>
      <span>附录</span>
      <ol><li><a href="xuxu.xhtml">附录续篇</a></li></ol>
    </li>
    <li>
      <span>卷丙</span>
      <ol>
        <li><a href="x1.xhtml">新章一</a></li>
        <li><a href="x2.xhtml">新章二</a></li>
      </ol>
    </li>
  </ol>
</nav>
</body>
</html>`},
	}
	for _, id := range []string{"xuxu", "x1", "x2"} {
		files = append(files, struct{ name, body string }{"OEBPS/" + id + ".xhtml", `<?xml version="1.0" encoding="utf-8"?>
<html xmlns="http://www.w3.org/1999/xhtml">
<head><title>` + titles[id+".xhtml"] + `</title></head>
<body><h1>` + titles[id+".xhtml"] + `</h1><p>` + titles[id+".xhtml"] + `的正文内容，讲述了一段新的故事情节，足以撑起一个正式章节。</p></body>
</html>`})
	}

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, file := range files {
		entry, err := writer.Create(file.name)
		if err != nil {
			t.Fatalf("create %s: %v", file.name, err)
		}
		if _, err := entry.Write([]byte(file.body)); err != nil {
			t.Fatalf("write %s: %v", file.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestImportNovelToWorkspaceAppendsToExistingBook(t *testing.T) {
	workspace := t.TempDir()
	if err := NewState(workspace).InitWorkspace(); err != nil {
		t.Fatalf("InitWorkspace failed: %v", err)
	}
	// First import seeds the book: ch00001..ch00004 plus volume v00001-附录.
	if _, _, _, err := ImportNovelToWorkspace(workspace, "初雪.epub", sampleEPUBBytes(t)); err != nil {
		t.Fatalf("seed import failed: %v", err)
	}

	preview, paths, skipped, err := ImportNovelToWorkspace(workspace, "续篇.epub", buildAppendEPUB(t))
	if err != nil {
		t.Fatalf("append import failed: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 new chapters", paths)
	}
	wantPaths := []string{
		"chapters/v00001-附录/ch00007-附录续篇.md",
		"chapters/v00002-卷丙/ch00008-新章一.md",
		"chapters/v00002-卷丙/ch00009-新章二.md",
	}
	for i, want := range wantPaths {
		if paths[i] != want {
			t.Fatalf("paths[%d] = %q, want %q", i, paths[i], want)
		}
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(want))); err != nil {
			t.Fatalf("written chapter missing: %v", err)
		}
	}
	// The seeded chapters are untouched.
	for _, seeded := range []string{
		"chapters/ch00001-第一章-初雪.md",
		"chapters/v00001-附录/ch00004-附录图表-·-附录A.md",
	} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(seeded))); err != nil {
			t.Fatalf("seeded chapter vanished: %v", err)
		}
	}
	if !strings.Contains(preview.Title, "续篇") {
		t.Fatalf("preview title = %q", preview.Title)
	}
}

func TestImportNovelToWorkspaceSkipsExistingFilesOnRepeat(t *testing.T) {
	workspace := t.TempDir()
	data := buildAppendEPUB(t)
	if _, _, _, err := ImportNovelToWorkspace(workspace, "续篇.epub", data); err != nil {
		t.Fatalf("first import failed: %v", err)
	}
	_, paths, skipped, err := ImportNovelToWorkspace(workspace, "续篇.epub", data)
	if err != nil {
		t.Fatalf("repeat import failed: %v", err)
	}
	// Numbering always continues past existing chapters, so a repeated import
	// appends new copies instead of overwriting seeded content.
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if len(paths) != 3 {
		t.Fatalf("paths = %v, want 3 appended chapters", paths)
	}
	want := "chapters/v00002-卷丙/ch00005-新章一.md"
	if paths[1] != want {
		t.Fatalf("paths[1] = %q, want %q", paths[1], want)
	}
}
