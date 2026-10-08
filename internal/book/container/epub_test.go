package container

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func sampleEPUB(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "sample.epub"))
	if err != nil {
		t.Fatalf("read sample.epub: %v", err)
	}
	return data
}

func TestEPUBTopicsFollowSpineOrder(t *testing.T) {
	archive, err := Open(sampleEPUB(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer archive.Close()
	if archive.Kind() != KindEPUB {
		t.Fatalf("Kind = %v, want EPUB", archive.Kind())
	}
	topics, err := archive.Topics()
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	if len(topics) != len(epubTopics) {
		t.Fatalf("topics = %d, want %d", len(topics), len(epubTopics))
	}
	for i, topic := range epubTopics {
		if topics[i].Path != "OEBPS/"+topic.file {
			t.Fatalf("topics[%d].Path = %q, want %q", i, topics[i].Path, "OEBPS/"+topic.file)
		}
		if !bytes.Contains(topics[i].HTML, []byte(topic.title)) {
			t.Fatalf("topics[%d] does not contain title %q", i, topic.title)
		}
	}
	if _, ok := archive.Sitemap(); ok {
		t.Fatalf("Sitemap should not be available for EPUB archives")
	}
}

func TestEPUBTocTreeMirrorsNavDocument(t *testing.T) {
	archive, err := Open(sampleEPUB(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer archive.Close()
	tree, ok := archive.TocTree()
	if !ok {
		t.Fatalf("TocTree missing for EPUB fixture")
	}
	if len(tree) != 4 {
		t.Fatalf("top-level entries = %d, want 4", len(tree))
	}
	wantTitles := []string{"第一章 初雪", "第二章 夜行", "第三章 归途", "附录"}
	for i, want := range wantTitles {
		if tree[i].Title != want {
			t.Fatalf("tree[%d].Title = %q, want %q", i, tree[i].Title, want)
		}
	}
	// 附录 > 附录图表 > 附录A/附录B, with the grouped leaves at level three.
	group := tree[3]
	if len(group.Children) != 1 || group.Children[0].Title != "附录图表" {
		t.Fatalf("附录 children = %+v, want a single 附录图表 group", group.Children)
	}
	section := group.Children[0]
	if len(section.Children) != 2 {
		t.Fatalf("附录图表 children = %d, want 2", len(section.Children))
	}
	if section.Children[0].Title != "附录A" || section.Children[0].Path != "OEBPS/ch04.xhtml" {
		t.Fatalf("附录图表 first child = %+v", section.Children[0])
	}
}

func TestOpenRejectsRenamedZipDocuments(t *testing.T) {
	// A zip whose content marks it as an Office document must be rejected
	// instead of being read as an EPUB.
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	file, err := writer.Create("[Content_Types].xml")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := file.Write([]byte(`<?xml version="1.0"?><Types/>`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Open(buf.Bytes()); err == nil {
		t.Fatalf("Open(docx-shaped zip) unexpectedly succeeded")
	}
}
