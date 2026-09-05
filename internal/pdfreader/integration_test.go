//go:build integration

package pdfreader

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests drive the real go-pdfium WASM engine against genuine PDF files.
// They are gated behind the `integration` build tag because WASM pool
// initialization is slow (>1s) and must not run in the default unit suite.
//
// Run with: go test -tags integration ./internal/pdfreader/...

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("读取测试夹具 %s 失败: %v", name, err)
	}
	return data
}

func TestOpenAndPageCount(t *testing.T) {
	r, err := Open(loadFixture(t, "hello_world.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	n, err := r.PageCount()
	if err != nil {
		t.Fatalf("PageCount 失败: %v", err)
	}
	if n < 1 {
		t.Errorf("页数 = %d, 期望 >= 1", n)
	}
	t.Logf("hello_world.pdf 页数 = %d", n)
}

func TestGetPageText_HelloWorld(t *testing.T) {
	r, err := Open(loadFixture(t, "hello_world.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	text, err := r.GetPageText(0)
	if err != nil {
		t.Fatalf("GetPageText 失败: %v", err)
	}
	t.Logf("提取文本: %q", text)
	if !strings.Contains(strings.ToLower(text), "hello") {
		t.Errorf("期望文本包含 'hello', 实际 %q", text)
	}
}

func TestHasPageImages_EmbeddedImages(t *testing.T) {
	r, err := Open(loadFixture(t, "embedded_images.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	n, err := r.PageCount()
	if err != nil {
		t.Fatalf("PageCount 失败: %v", err)
	}
	found := false
	for p := 0; p < n; p++ {
		has, err := r.HasPageImages(p)
		if err != nil {
			t.Fatalf("HasPageImages(page %d) 失败: %v", p, err)
		}
		t.Logf("page %d hasImages=%v", p, has)
		if has {
			found = true
		}
	}
	if !found {
		t.Error("embedded_images.pdf 至少应有一页包含图片对象")
	}
}

func TestHasPageImages_TextOnly(t *testing.T) {
	r, err := Open(loadFixture(t, "hello_world.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	has, err := r.HasPageImages(0)
	if err != nil {
		t.Fatalf("HasPageImages 失败: %v", err)
	}
	if has {
		t.Error("hello_world.pdf 是纯文本页，不应检测到图片对象")
	}
}

func TestRenderPage(t *testing.T) {
	r, err := Open(loadFixture(t, "hello_world.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	png, err := r.RenderPage(0, 150)
	if err != nil {
		t.Fatalf("RenderPage 失败: %v", err)
	}
	// PNG magic: 89 50 4E 47
	if len(png) < 8 || png[0] != 0x89 || png[1] != 0x50 || png[2] != 0x4E || png[3] != 0x47 {
		t.Errorf("渲染输出不是有效 PNG (len=%d)", len(png))
	}
	t.Logf("渲染 PNG 大小 = %d bytes", len(png))
}

func TestGetPageTextStructured_VerticalText(t *testing.T) {
	r, err := Open(loadFixture(t, "vertical_text.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	blocks, err := r.GetPageTextStructured(0)
	if err != nil {
		t.Fatalf("GetPageTextStructured 失败: %v", err)
	}
	t.Logf("结构化文本块数 = %d", len(blocks))
	for i, b := range blocks {
		if i >= 5 {
			break
		}
		t.Logf("  block[%d]: %q L=%.0f T=%.0f R=%.0f B=%.0f", i, b.Text, b.Left, b.Top, b.Right, b.Bottom)
	}
	if len(blocks) == 0 {
		t.Log("警告: vertical_text.pdf 未提取到结构化文本块（可能为纯字形渲染）")
	}
}

func TestAnalyzePage_TextLayer(t *testing.T) {
	r, err := Open(loadFixture(t, "hello_world.pdf"))
	if err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	defer r.Close()

	pc, err := r.AnalyzePage(0, 150, true)
	if err != nil {
		t.Fatalf("AnalyzePage 失败: %v", err)
	}
	t.Logf("AnalyzePage: text=%q hasImages=%v needsRender=%v renderedLen=%d",
		pc.Text, pc.HasImages, pc.NeedsRender, len(pc.RenderedImage))
	// A pure text page with a reliable text layer should not need rendering.
	if pc.Text == "" {
		t.Error("hello_world.pdf 应有可提取的文本层")
	}
}
