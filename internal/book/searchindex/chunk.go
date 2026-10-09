package searchindex

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// TargetChunkRunes bounds one retrieval chunk. Larger chunks keep scenes
	// coherent for the embedding model; 1200 runes is far below the 8K model
	// context and keeps snippet output readable.
	TargetChunkRunes = 1200
	// ChunkOverlapRunes is the window overlap used when a single paragraph
	// exceeds the target, so sentences crossing a boundary stay retrievable.
	ChunkOverlapRunes = 200
)

type chunkPiece struct {
	Text  string
	Start int // rune offset in the source file
}

type paragraph struct {
	text  string
	start int
}

func splitParagraphs(text string) []paragraph {
	var out []paragraph
	offset := 0
	for _, part := range strings.SplitAfter(text, "\n\n") {
		lead := strings.IndexFunc(part, func(r rune) bool { return !unicode.IsSpace(r) })
		if lead >= 0 {
			out = append(out, paragraph{text: strings.TrimSpace(part), start: offset + utf8.RuneCountInString(part[:lead])})
		}
		offset += utf8.RuneCountInString(part)
	}
	return out
}

// chunkText splits one file body into retrieval chunks. Paragraph boundaries
// are preferred; oversized paragraphs are windowed with overlap.
func chunkText(text string) []chunkPiece {
	paragraphs := splitParagraphs(text)
	var pieces []chunkPiece
	var pending []paragraph
	pendingRunes := 0
	flush := func() {
		if len(pending) == 0 {
			return
		}
		texts := make([]string, 0, len(pending))
		for _, p := range pending {
			texts = append(texts, p.text)
		}
		pieces = append(pieces, chunkPiece{Text: strings.Join(texts, "\n\n"), Start: pending[0].start})
		pending = pending[:0]
		pendingRunes = 0
	}
	for _, p := range paragraphs {
		runes := utf8.RuneCountInString(p.text)
		if runes > TargetChunkRunes {
			flush()
			pieces = append(pieces, splitOversizedParagraph(p)...)
			continue
		}
		if pendingRunes+runes > TargetChunkRunes {
			flush()
		}
		pending = append(pending, p)
		pendingRunes += runes
	}
	flush()
	return pieces
}

func splitOversizedParagraph(p paragraph) []chunkPiece {
	runes := []rune(p.text)
	step := TargetChunkRunes - ChunkOverlapRunes
	var pieces []chunkPiece
	for start := 0; start < len(runes); start += step {
		end := start + TargetChunkRunes
		if end > len(runes) {
			end = len(runes)
		}
		pieces = append(pieces, chunkPiece{Text: string(runes[start:end]), Start: p.start + start})
		if end == len(runes) {
			break
		}
	}
	return pieces
}
