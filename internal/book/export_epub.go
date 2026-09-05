package book

import (
	"html"
	"os"
	"strings"

	epub "github.com/bmaupin/go-epub"
)

// ExportEPUB renders the structured manuscript as an EPUB 3 document and
// returns its bytes. Chapters become one section each; volume headers become
// divider sections so reader apps keep the two-level structure.
func (s *Service) ExportEPUB(meta BookMeta) ([]byte, error) {
	document, err := s.ExportDocument(meta)
	if err != nil {
		return nil, err
	}

	title := document.Title
	if title == "" {
		title = "book"
	}
	book := epub.NewEpub(title)
	if document.Author != "" {
		book.SetAuthor(document.Author)
	}

	currentVolume := ""
	for _, chapter := range document.Chapters {
		if chapter.Volume != "" && chapter.Volume != currentVolume {
			if _, err := book.AddSection(volumeSectionBody(chapter.Volume), chapter.Volume, "", ""); err != nil {
				return nil, err
			}
			currentVolume = chapter.Volume
		}
		if _, err := book.AddSection(paragraphSectionBody(chapter.Body), chapter.Title, "", ""); err != nil {
			return nil, err
		}
	}

	file, err := os.CreateTemp("", "denova-export-*.epub")
	if err != nil {
		return nil, err
	}
	tempPath := file.Name()
	if err := file.Close(); err != nil {
		return nil, err
	}
	defer os.Remove(tempPath)
	if err := book.Write(tempPath); err != nil {
		return nil, err
	}
	return os.ReadFile(tempPath)
}

func volumeSectionBody(volume string) string {
	return "<h1>" + html.EscapeString(volume) + "</h1>"
}

// paragraphSectionBody converts plain-text prose into minimal HTML: blank-line
// separated blocks become <p> elements, single newlines inside a block become
// line breaks.
func paragraphSectionBody(body string) string {
	blocks := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	paragraphs := make([]string, 0, len(blocks))
	for _, block := range blocks {
		text := strings.TrimSpace(block)
		if text == "" {
			continue
		}
		paragraphs = append(paragraphs, "<p>"+html.EscapeString(text)+"</p>")
	}
	return strings.Join(paragraphs, "\n")
}
