package book

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode"
)

// ErrNoExportableChapters indicates that a book has no non-empty chapters to export.
var ErrNoExportableChapters = errors.New("没有可导出的非空章节")

// TextExport is the assembled plain-text reading export for a book workspace.
type TextExport struct {
	Content      string
	ChapterCount int
}

// ExportChapter is one exportable chapter with the volume context it appeared in.
type ExportChapter struct {
	Volume string
	Title  string
	Body   string
}

// DocumentExport is the structured manuscript shared by every export format.
type DocumentExport struct {
	Title        string
	Author       string
	Chapters     []ExportChapter
	ChapterCount int
}

// ExportDocument assembles all non-empty chapters into a structured manuscript
// that format-specific exporters (txt, epub, docx) render.
func (s *Service) ExportDocument(meta BookMeta) (DocumentExport, error) {
	summary, err := s.Summary()
	if err != nil {
		return DocumentExport{}, err
	}

	document := DocumentExport{
		Title:    firstNonEmptyText(meta.Title, summary.Title, filepath.Base(s.workspace)),
		Author:   firstNonEmptyText(meta.Author, summary.Author),
		Chapters: make([]ExportChapter, 0, len(summary.Chapters)),
	}

	lastVolumePath := ""
	for _, chapter := range summary.Chapters {
		if chapter.Words == 0 {
			continue
		}
		content, err := s.ReadFile(chapter.Path)
		if err != nil {
			return DocumentExport{}, err
		}
		body := exportChapterBody(content, chapter.DisplayTitle)
		if strings.TrimSpace(body) == "" {
			continue
		}
		volume := ""
		if shouldWriteExportVolume(chapter, lastVolumePath) {
			volume = chapter.Volume
		}
		if chapter.VolumePath != "" {
			lastVolumePath = chapter.VolumePath
		}
		document.Chapters = append(document.Chapters, ExportChapter{
			Volume: volume,
			Title:  chapter.DisplayTitle,
			Body:   body,
		})
		document.ChapterCount++
	}
	if document.ChapterCount == 0 {
		return DocumentExport{}, ErrNoExportableChapters
	}
	return document, nil
}

// ExportText assembles all non-empty chapters into a single plain-text manuscript.
func (s *Service) ExportText(meta BookMeta) (TextExport, error) {
	document, err := s.ExportDocument(meta)
	if err != nil {
		return TextExport{}, err
	}

	blocks := make([]string, 0, len(document.Chapters)*3+1)
	headerLines := []string{}
	if document.Title != "" {
		headerLines = append(headerLines, document.Title)
	}
	if document.Author != "" {
		headerLines = append(headerLines, "作者: "+document.Author)
	}
	if len(headerLines) > 0 {
		blocks = append(blocks, strings.Join(headerLines, "\n"))
	}
	for _, chapter := range document.Chapters {
		if chapter.Volume != "" {
			blocks = append(blocks, chapter.Volume)
		}
		blocks = append(blocks, chapter.Title, chapter.Body)
	}
	return TextExport{
		Content:      strings.TrimSpace(strings.Join(blocks, "\n\n")) + "\n",
		ChapterCount: document.ChapterCount,
	}, nil
}

func shouldWriteExportVolume(chapter ChapterSummary, lastVolumePath string) bool {
	return chapter.VolumePath != "" && chapter.VolumePath != "chapters" && chapter.VolumePath != lastVolumePath && strings.TrimSpace(chapter.Volume) != ""
}

func exportChapterBody(content, displayTitle string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = strings.TrimPrefix(content, "\ufeff")
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if sameExportTitle(line, displayTitle) {
			lines = append(lines[:i], lines[i+1:]...)
		}
		break
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func sameExportTitle(line, title string) bool {
	left := normalizeExportTitle(line)
	right := normalizeExportTitle(title)
	return left != "" && left == right
}

func normalizeExportTitle(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "# \t")
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if unicode.IsSpace(r) || isExportTitlePunctuation(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

func isExportTitlePunctuation(r rune) bool {
	return strings.ContainsRune("#*_`~[]()（）【】《》<>:：-—_、，,.．。", r)
}

func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
