// Package pdfreader provides PDF parsing, text extraction, image detection,
// and on-demand page rendering for the image analysis pipeline.
// It uses go-pdfium (WASM backend via wazero) — zero external binary dependencies.
package pdfreader

// PageContent is the analysis result for a single PDF page.
// It carries either extracted text (text layer) or rendered image bytes,
// depending on whether the page has a usable text layer.
type PageContent struct {
	PageIndex     int    // 0-based page index within the PDF
	Text          string // extracted text layer content (empty if no text layer or order is unreliable)
	HasImages     bool   // whether the page contains embedded image objects
	NeedsRender   bool   // whether this page must be rendered as image for vision model analysis
	RenderedImage []byte // PNG bytes of the rendered page (populated when NeedsRender=true)
}

// TextBlock represents a positioned text segment extracted from a PDF page.
// Coordinates are in PDF points (origin at bottom-left, 1 point = 1/72 inch).
type TextBlock struct {
	Text   string
	Left   float64
	Top    float64
	Right  float64
	Bottom float64
}

// BookType classifies the overall nature of a PDF document.
type BookType string

const (
	BookTypeManga        BookType = "manga"
	BookTypeNovel        BookType = "novel"
	BookTypeIllustration BookType = "illustration"
	BookTypeMixed        BookType = "mixed"
)
