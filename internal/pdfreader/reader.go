package pdfreader

import (
	"bytes"
	"fmt"
	"image/png"
	"log"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/enums"
	"github.com/klippa-app/go-pdfium/references"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
)

// pool and instance are lazily initialized once per process.
// WASM workers use significant memory; we keep a single worker.
var (
	poolOnce sync.Once
	pool     pdfium.Pool
	instance pdfium.Pdfium
	initErr  error
)

func ensureInstance() (pdfium.Pdfium, error) {
	poolOnce.Do(func() {
		var p pdfium.Pool
		p, initErr = webassembly.Init(webassembly.Config{
			MinIdle:  1,
			MaxIdle:  1,
			MaxTotal: 1,
		})
		if initErr != nil {
			log.Printf("[pdfreader] failed to init WASM pool: %v", initErr)
			return
		}
		pool = p
		instance, initErr = pool.GetInstance(time.Second * 30)
		if initErr != nil {
			log.Printf("[pdfreader] failed to get WASM instance: %v", initErr)
		}
	})
	return instance, initErr
}

// Reader wraps a single PDF document for page-level operations.
type Reader struct {
	inst pdfium.Pdfium
	doc  references.FPDF_DOCUMENT // document handle reference
	data []byte                   // keep reference to avoid GC of underlying bytes
}

// Open parses PDF bytes and returns a Reader.
// The caller must call Close() when done.
func Open(data []byte) (*Reader, error) {
	inst, err := ensureInstance()
	if err != nil {
		return nil, fmt.Errorf("初始化 PDF 引擎失败: %w", err)
	}

	doc, err := inst.OpenDocument(&requests.OpenDocument{
		File: &data,
	})
	if err != nil {
		return nil, fmt.Errorf("打开 PDF 失败: %w", err)
	}

	return &Reader{
		inst: inst,
		doc:  doc.Document,
		data: data,
	}, nil
}

// Close releases the PDF document resources.
func (r *Reader) Close() error {
	if r.inst == nil || r.doc == "" {
		return nil
	}
	_, err := r.inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{
		Document: r.doc,
	})
	r.doc = ""
	return err
}

// PageCount returns the total number of pages in the document.
func (r *Reader) PageCount() (int, error) {
	resp, err := r.inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{
		Document: r.doc,
	})
	if err != nil {
		return 0, fmt.Errorf("获取页数失败: %w", err)
	}
	return resp.PageCount, nil
}

// GetPageText extracts the plain text content of a page.
// Returns empty string if the page has no text layer.
func (r *Reader) GetPageText(pageIndex int) (string, error) {
	resp, err := r.inst.GetPageText(&requests.GetPageText{
		Page: requests.Page{
			ByIndex: &requests.PageByIndex{
				Document: r.doc,
				Index:    pageIndex,
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("提取第 %d 页文本失败: %w", pageIndex+1, err)
	}
	return resp.Text, nil
}

// GetPageTextStructured extracts text blocks with position information.
// Used for reading order detection in vertical (tategaki) Japanese text.
func (r *Reader) GetPageTextStructured(pageIndex int) ([]TextBlock, error) {
	resp, err := r.inst.GetPageTextStructured(&requests.GetPageTextStructured{
		Page: requests.Page{
			ByIndex: &requests.PageByIndex{
				Document: r.doc,
				Index:    pageIndex,
			},
		},
		Mode: requests.GetPageTextStructuredModeRects,
	})
	if err != nil {
		return nil, fmt.Errorf("提取第 %d 页结构化文本失败: %w", pageIndex+1, err)
	}

	blocks := make([]TextBlock, 0, len(resp.Rects))
	for _, rect := range resp.Rects {
		if rect.Text == "" {
			continue
		}
		blocks = append(blocks, TextBlock{
			Text:   rect.Text,
			Left:   rect.PointPosition.Left,
			Top:    rect.PointPosition.Top,
			Right:  rect.PointPosition.Right,
			Bottom: rect.PointPosition.Bottom,
		})
	}
	return blocks, nil
}

// HasPageImages detects whether a page contains embedded image objects.
// Uses FPDFPage_CountObjects + FPDFPage_GetObject + FPDFPageObj_GetType to scan.
func (r *Reader) HasPageImages(pageIndex int) (bool, error) {
	page := requests.Page{
		ByIndex: &requests.PageByIndex{
			Document: r.doc,
			Index:    pageIndex,
		},
	}

	countResp, err := r.inst.FPDFPage_CountObjects(&requests.FPDFPage_CountObjects{Page: page})
	if err != nil {
		return false, fmt.Errorf("获取第 %d 页对象数失败: %w", pageIndex+1, err)
	}

	for i := 0; i < countResp.Count; i++ {
		objResp, err := r.inst.FPDFPage_GetObject(&requests.FPDFPage_GetObject{Page: page, Index: i})
		if err != nil {
			continue
		}
		typeResp, err := r.inst.FPDFPageObj_GetType(&requests.FPDFPageObj_GetType{PageObject: objResp.PageObject})
		if err != nil {
			continue
		}
		// FPDF_PAGEOBJ_IMAGE = 3
		if typeResp.Type == enums.FPDF_PAGEOBJ_IMAGE {
			return true, nil
		}
	}
	return false, nil
}

// RenderPage renders a page at the given DPI and returns PNG bytes.
func (r *Reader) RenderPage(pageIndex int, dpi int) ([]byte, error) {
	if dpi <= 0 {
		dpi = 300
	}

	resp, err := r.inst.RenderPageInDPI(&requests.RenderPageInDPI{
		DPI: dpi,
		Page: requests.Page{
			ByIndex: &requests.PageByIndex{
				Document: r.doc,
				Index:    pageIndex,
			},
		},
		RenderForm: true,
	})
	if err != nil {
		return nil, fmt.Errorf("渲染第 %d 页失败 (DPI=%d): %w", pageIndex+1, dpi, err)
	}
	defer resp.Cleanup()

	var buf bytes.Buffer
	if err := png.Encode(&buf, resp.Result.Image); err != nil {
		return nil, fmt.Errorf("编码第 %d 页 PNG 失败: %w", pageIndex+1, err)
	}
	return buf.Bytes(), nil
}

// AnalyzePage performs full analysis of a single page:
// text extraction, reading order detection, image detection, and conditional rendering.
func (r *Reader) AnalyzePage(pageIndex int, dpi int, trustTextLayer bool) (PageContent, error) {
	pc := PageContent{PageIndex: pageIndex}

	// 1. Try text layer extraction
	text, err := r.GetPageText(pageIndex)
	if err != nil {
		log.Printf("[pdfreader] page %d text extraction error: %v", pageIndex+1, err)
	}

	// 2. Check for embedded images
	hasImages, err := r.HasPageImages(pageIndex)
	if err != nil {
		log.Printf("[pdfreader] page %d image detection error: %v", pageIndex+1, err)
	}
	pc.HasImages = hasImages

	// 3. Decide whether text layer is usable
	textUsable := false
	if trustTextLayer && len([]rune(text)) > 10 {
		// Text layer exists with meaningful content. Decide whether its reading
		// order is trustworthy.
		blocks, err := r.GetPageTextStructured(pageIndex)
		if err == nil && len(blocks) > 0 {
			if isVerticalLayout(blocks) {
				// Vertical (tategaki) text: PDFium's native order may be garbled,
				// so verify it against the expected right-to-left, top-to-bottom
				// order and fall back to rendering if it disagrees.
				_, reordered := DetectReadingOrder(blocks)
				if !reordered {
					textUsable = true
				} else {
					log.Printf("[pdfreader] page %d: vertical text order unreliable, will render", pageIndex+1)
				}
			} else {
				// Horizontal text: PDFium extracts in correct reading order natively.
				textUsable = true
			}
		} else {
			// No structured blocks but plain text exists — trust it.
			textUsable = true
		}
	}

	if textUsable && !hasImages {
		// Pure text page with reliable order — no rendering needed
		pc.Text = text
		pc.NeedsRender = false
		return pc, nil
	}

	// 4. Render page as image for vision model analysis
	pc.NeedsRender = true
	if textUsable && hasImages {
		// Mixed page: keep text but also render for image analysis
		pc.Text = text
	}

	rendered, err := r.RenderPage(pageIndex, dpi)
	if err != nil {
		return pc, fmt.Errorf("渲染第 %d 页失败: %w", pageIndex+1, err)
	}
	pc.RenderedImage = rendered

	return pc, nil
}
