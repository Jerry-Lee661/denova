package handlers

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/cloudwego/hertz/pkg/app"

	"denova/config"
	"denova/internal/imageanalysis"
	"denova/internal/pdfreader"
)

// parsePageRange reads optional 1-based inclusive page_start/page_end form
// values. Zero values mean "from the first page" / "to the last page".
func parsePageRange(c *app.RequestContext) (start, end int) {
	if v := strings.TrimSpace(string(c.FormValue("page_start"))); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			start = n
		}
	}
	if v := strings.TrimSpace(string(c.FormValue("page_end"))); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			end = n
		}
	}
	return start, end
}

// expandPDF parses a PDF document and converts it into a slice of
// UploadedImage entries suitable for the existing batch pipeline.
//
// Pages with a usable text layer become text-only entries (Data empty, Text
// populated) so the vision model is skipped. Pages without a reliable text
// layer (scanned pages, illustrations, or garbled vertical text) are rendered
// to PNG at the configured DPI and carried as normal image entries.
//
// pageStart/pageEnd are 1-based inclusive bounds; zero means unbounded.
// The total number of processed pages is capped by cfg.ImageAnalysisPDFMaxPages().
func expandPDF(data []byte, fileName string, cfg *config.Config, pageStart, pageEnd int) ([]imageanalysis.UploadedImage, error) {
	reader, err := pdfreader.Open(data)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	total, err := reader.PageCount()
	if err != nil {
		return nil, err
	}
	if total == 0 {
		return nil, fmt.Errorf("PDF 没有页面")
	}

	// Resolve 1-based inclusive bounds to 0-based [lo, hi).
	lo := 0
	hi := total
	if pageStart > 0 {
		lo = pageStart - 1
	}
	if pageEnd > 0 && pageEnd < hi {
		hi = pageEnd
	}
	if lo < 0 {
		lo = 0
	}
	if hi > total {
		hi = total
	}
	if lo >= hi {
		return nil, fmt.Errorf("页码范围无效: start=%d end=%d total=%d", pageStart, pageEnd, total)
	}

	maxPages := cfg.ImageAnalysisPDFMaxPages()
	if maxPages > 0 && hi-lo > maxPages {
		log.Printf("[image-analysis] pdf %q truncated to %d pages (requested %d)", fileName, maxPages, hi-lo)
		hi = lo + maxPages
	}

	dpi := cfg.ImageAnalysisPDFRenderDPI()
	trustText := cfg.ImageAnalysisPDFTextLayerTrust()

	base := strings.TrimSuffix(fileName, ".pdf")
	base = strings.TrimSuffix(base, ".PDF")

	out := make([]imageanalysis.UploadedImage, 0, hi-lo)
	for page := lo; page < hi; page++ {
		pc, err := reader.AnalyzePage(page, dpi, trustText)
		if err != nil {
			return nil, fmt.Errorf("处理第 %d 页失败: %w", page+1, err)
		}

		pageName := fmt.Sprintf("%s_p%03d.png", base, page+1)
		if pc.NeedsRender {
			// Rendered page goes through the vision model. We intentionally do
			// NOT carry pc.Text here: a non-empty Text makes ProcessBatch skip
			// the vision model, which would drop image/illustration analysis.
			out = append(out, imageanalysis.UploadedImage{
				FileName: pageName,
				Data:     pc.RenderedImage,
				MIMEType: "image/png",
				IsPDF:    true,
			})
		} else {
			// Pure text-layer page: no image bytes, vision model skipped.
			out = append(out, imageanalysis.UploadedImage{
				FileName: fmt.Sprintf("%s_p%03d.txt", base, page+1),
				MIMEType: "text/plain",
				Text:     pc.Text,
				IsPDF:    true,
			})
		}
	}

	log.Printf("[image-analysis] pdf %q expanded to %d pages (range %d-%d of %d)", fileName, len(out), lo+1, hi, total)
	return out, nil
}
