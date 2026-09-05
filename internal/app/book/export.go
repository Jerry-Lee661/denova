package bookapp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"denova/internal/book"
)

// ErrUnsupportedBookExportFormat indicates that the requested export format is not implemented.
var ErrUnsupportedBookExportFormat = errors.New("unsupported book export format")

// BookExportFormat identifies the output format for a book export.
type BookExportFormat string

const (
	// BookExportFormatTXT exports a plain UTF-8 text manuscript.
	BookExportFormatTXT BookExportFormat = "txt"
	// BookExportFormatEPUB exports an EPUB 3 e-book package.
	BookExportFormatEPUB BookExportFormat = "epub"
	// BookExportFormatDOCX exports an OOXML Word document with heading styles.
	BookExportFormatDOCX BookExportFormat = "docx"
)

// BookExportRequest describes a format-specific book export request.
type BookExportRequest struct {
	Path   string           `json:"path"`
	Format BookExportFormat `json:"format"`
}

// BookExportResult carries a generated export file back to the API layer.
type BookExportResult struct {
	Filename     string
	ContentType  string
	Data         []byte
	ChapterCount int
}

// Export exports a book workspace in the requested format.
func (service *Service) Export(req BookExportRequest) (BookExportResult, error) {
	format := normalizeBookExportFormat(req.Format)
	if format == "" {
		return BookExportResult{}, fmt.Errorf("%w: %s", ErrUnsupportedBookExportFormat, req.Format)
	}
	absPath, err := validateWorkspacePath(req.Path)
	if err != nil {
		return BookExportResult{}, err
	}
	layout, err := service.metadataLayout(absPath)
	if err != nil {
		return BookExportResult{}, err
	}
	meta, err := service.metadata.Read(layout.ContentRoot, layout.StoreRoot)
	if err != nil {
		return BookExportResult{}, err
	}

	switch format {
	case BookExportFormatTXT:
		result, err := book.NewService(absPath).ExportText(meta)
		if err != nil {
			return BookExportResult{}, err
		}
		return BookExportResult{
			Filename:     bookExportFilename(meta, absPath, format),
			ContentType:  "text/plain; charset=utf-8",
			Data:         []byte(result.Content),
			ChapterCount: result.ChapterCount,
		}, nil
	case BookExportFormatEPUB:
		data, err := book.NewService(absPath).ExportEPUB(meta)
		if err != nil {
			return BookExportResult{}, err
		}
		return BookExportResult{
			Filename:    bookExportFilename(meta, absPath, format),
			ContentType: "application/epub+zip",
			Data:        data,
		}, nil
	case BookExportFormatDOCX:
		data, err := book.NewService(absPath).ExportDOCX(meta)
		if err != nil {
			return BookExportResult{}, err
		}
		return BookExportResult{
			Filename:    bookExportFilename(meta, absPath, format),
			ContentType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			Data:        data,
		}, nil
	default:
		return BookExportResult{}, fmt.Errorf("%w: %s", ErrUnsupportedBookExportFormat, req.Format)
	}
}

func normalizeBookExportFormat(format BookExportFormat) BookExportFormat {
	switch BookExportFormat(strings.ToLower(strings.TrimSpace(string(format)))) {
	case BookExportFormatTXT:
		return BookExportFormatTXT
	case BookExportFormatEPUB:
		return BookExportFormatEPUB
	case BookExportFormatDOCX:
		return BookExportFormatDOCX
	default:
		return ""
	}
}

func bookExportFilename(meta book.BookMeta, workspace string, format BookExportFormat) string {
	name := strings.TrimSpace(meta.Title)
	if name == "" {
		name = filepath.Base(workspace)
	}
	name = sanitizeDownloadFilenameBase(name)
	if name == "" {
		name = "book"
	}
	return name + "." + string(format)
}

func sanitizeDownloadFilenameBase(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		default:
			return r
		}
	}, value)
	return strings.Trim(value, ". ")
}
