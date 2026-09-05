package book

import (
	"bytes"
	"strings"

	godocx "github.com/gomutex/godocx"
)

// ExportDOCX renders the structured manuscript as an OOXML Word document and
// returns its bytes. Volume headers and chapter titles become real heading
// styles so navigation panes and readers keep the document outline.
func (s *Service) ExportDOCX(meta BookMeta) ([]byte, error) {
	document, err := s.ExportDocument(meta)
	if err != nil {
		return nil, err
	}

	root, err := godocx.NewDocument()
	if err != nil {
		return nil, err
	}
	if document.Title != "" {
		if _, err := root.AddHeading(document.Title, 1); err != nil {
			return nil, err
		}
	}
	if document.Author != "" {
		root.AddParagraph("作者: " + document.Author)
	}

	currentVolume := ""
	for _, chapter := range document.Chapters {
		if chapter.Volume != "" && chapter.Volume != currentVolume {
			if _, err := root.AddHeading(chapter.Volume, 1); err != nil {
				return nil, err
			}
			currentVolume = chapter.Volume
		}
		if chapter.Title != "" {
			if _, err := root.AddHeading(chapter.Title, 2); err != nil {
				return nil, err
			}
		}
		for _, paragraph := range splitDocxParagraphs(chapter.Body) {
			root.AddParagraph(paragraph)
		}
	}

	var buffer bytes.Buffer
	if err := root.Write(&buffer); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func splitDocxParagraphs(body string) []string {
	blocks := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	paragraphs := make([]string, 0, len(blocks))
	for _, block := range blocks {
		text := strings.TrimSpace(block)
		if text == "" {
			continue
		}
		paragraphs = append(paragraphs, text)
	}
	return paragraphs
}
