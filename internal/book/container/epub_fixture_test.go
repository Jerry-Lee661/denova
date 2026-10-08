package container

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The sample.epub fixture under internal/book/testdata is a minimal EPUB 3
// holding five UTF-8 Chinese XHTML topics plus one spine-only orphan page,
// with a three-level nav document (flat chapters, then a grouped appendix).
// buildSampleEPUB constructs those bytes; run
// `EPUB_REGEN_FIXTURE=1 go test ./internal/book/container/ -run TestRegenerateEPUBFixture`
// from the repository root to rewrite the committed fixture after changing it.

type epubTopic struct {
	file      string
	title     string
	body      string
	noHeading bool
}

var epubTopics = []epubTopic{
	{"ch01.xhtml", "第一章 初雪", "夜色沉沉，长街上的灯笼一盏盏熄灭。", false},
	{"ch02.xhtml", "第二章 夜行", "城门下的守卫缩在火盆旁，谁也没有拦她。", false},
	{"ch03.xhtml", "第三章 归途", "驿站的老马认得她，打了个响鼻。", false},
	{"ch04.xhtml", "附录A", "附录A 的第一段内容。", true},
	{"ch05.xhtml", "附录B", "附录B 的第一段内容。", true},
	{"orphan.xhtml", "孤儿页", "这一页不在目录里。", false},
}

func epubXHTML(t *testing.T, topic epubTopic) []byte {
	t.Helper()
	heading := ""
	if !topic.noHeading {
		heading = "<h1>" + topic.title + "</h1>\n"
	}
	doc := `<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml">
<head><title>` + topic.title + `</title></head>
<body>
` + heading + `<p>` + topic.body + `</p>
</body>
</html>
`
	return []byte(doc)
}

func buildSampleEPUB(t *testing.T) []byte {
	t.Helper()

	entries := []struct {
		name   string
		method uint16
		body   []byte
	}{
		{name: "mimetype", method: zip.Store, body: []byte("application/epub+zip")},
		{name: "META-INF/container.xml", method: zip.Deflate, body: []byte(`<?xml version="1.0" encoding="utf-8"?>
<container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container">
  <rootfiles>
    <rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/>
  </rootfiles>
</container>
`)},
		{name: "OEBPS/content.opf", method: zip.Deflate, body: []byte(`<?xml version="1.0" encoding="utf-8"?>
<package xmlns="http://www.idpf.org/2007/opf" version="3.0" unique-identifier="uid">
  <metadata xmlns:dc="http://purl.org/dc/elements/1.1/">
    <dc:identifier id="uid">urn:uuid:denova-sample-epub</dc:identifier>
    <dc:title>初雪测试</dc:title>
    <dc:language>zh</dc:language>
    <meta property="dcterms:modified">2026-01-01T00:00:00Z</meta>
  </metadata>
  <manifest>
    <item id="nav" href="nav.xhtml" media-type="application/xhtml+xml" properties="nav"/>
    <item id="ch01" href="ch01.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch02" href="ch02.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch03" href="ch03.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch04" href="ch04.xhtml" media-type="application/xhtml+xml"/>
    <item id="ch05" href="ch05.xhtml" media-type="application/xhtml+xml"/>
    <item id="orphan" href="orphan.xhtml" media-type="application/xhtml+xml"/>
  </manifest>
  <spine>
    <itemref idref="ch01"/>
    <itemref idref="ch02"/>
    <itemref idref="ch03"/>
    <itemref idref="ch04"/>
    <itemref idref="ch05"/>
    <itemref idref="orphan"/>
  </spine>
</package>
`)},
		{name: "OEBPS/nav.xhtml", method: zip.Deflate, body: []byte(`<?xml version="1.0" encoding="utf-8"?>
<!DOCTYPE html>
<html xmlns="http://www.w3.org/1999/xhtml" xmlns:epub="http://www.idpf.org/2007/ops">
<head><title>目录</title></head>
<body>
<nav epub:type="toc" id="toc">
  <ol>
    <li><a href="ch01.xhtml">第一章 初雪</a></li>
    <li><a href="ch02.xhtml">第二章 夜行</a></li>
    <li><a href="ch03.xhtml">第三章 归途</a></li>
    <li>
      <span>附录</span>
      <ol>
        <li>
          <span>附录图表</span>
          <ol>
            <li><a href="ch04.xhtml">附录A</a></li>
            <li><a href="ch05.xhtml">附录B</a></li>
          </ol>
        </li>
      </ol>
    </li>
  </ol>
</nav>
</body>
</html>
`)},
	}
	for _, topic := range epubTopics {
		entries = append(entries, struct {
			name   string
			method uint16
			body   []byte
		}{name: "OEBPS/" + topic.file, method: zip.Deflate, body: epubXHTML(t, topic)})
	}

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for _, entry := range entries {
		file, err := writer.CreateHeader(&zip.FileHeader{Name: entry.name, Method: entry.method})
		if err != nil {
			t.Fatalf("create %s: %v", entry.name, err)
		}
		if _, err := file.Write(entry.body); err != nil {
			t.Fatalf("write %s: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestRegenerateEPUBFixture(t *testing.T) {
	if os.Getenv("EPUB_REGEN_FIXTURE") == "" {
		t.Skip("set EPUB_REGEN_FIXTURE=1 to rewrite internal/book/testdata/sample.epub")
	}
	path := filepath.Join("..", "testdata", "sample.epub")
	if err := os.WriteFile(path, buildSampleEPUB(t), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Logf("fixture written to %s", path)
}
