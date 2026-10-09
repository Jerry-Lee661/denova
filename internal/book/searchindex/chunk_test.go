package searchindex

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestChunkTextPacksParagraphsAndKeepsOffsets(t *testing.T) {
	text := "第一段落。\n\n第二段落。\n\n第三段落。"
	pieces := chunkText(text)
	if len(pieces) != 1 {
		t.Fatalf("want 1 packed chunk, got %d", len(pieces))
	}
	if pieces[0].Start != 0 {
		t.Fatalf("start offset: %d", pieces[0].Start)
	}
	if !strings.Contains(pieces[0].Text, "第一段落") || !strings.Contains(pieces[0].Text, "第三段落") {
		t.Fatalf("chunk lost content: %q", pieces[0].Text)
	}
}

func TestChunkTextSplitsLargeFilesAtParagraphBoundaries(t *testing.T) {
	paragraph := strings.Repeat("云", 500)
	text := paragraph + "\n\n" + paragraph + "\n\n" + paragraph + "\n\n" + paragraph
	pieces := chunkText(text)
	if len(pieces) < 2 {
		t.Fatalf("want multiple chunks for %d runes, got %d", utf8.RuneCountInString(text), len(pieces))
	}
	for index, piece := range pieces {
		if runes := utf8.RuneCountInString(piece.Text); runes > TargetChunkRunes {
			t.Fatalf("chunk %d too large: %d runes", index, runes)
		}
	}
	if pieces[1].Start <= pieces[0].Start {
		t.Fatalf("chunk offsets must increase: %d then %d", pieces[0].Start, pieces[1].Start)
	}
}

func TestChunkTextWindowsOversizedParagraphWithOverlap(t *testing.T) {
	text := strings.Repeat("剑", TargetChunkRunes*2+50)
	pieces := chunkText(text)
	if len(pieces) < 2 {
		t.Fatalf("want windowed chunks, got %d", len(pieces))
	}
	step := TargetChunkRunes - ChunkOverlapRunes
	if pieces[1].Start != step {
		t.Fatalf("second window starts at %d, want %d", pieces[1].Start, step)
	}
	last := pieces[len(pieces)-1]
	if last.Start+utf8.RuneCountInString(last.Text) != utf8.RuneCountInString(text) {
		t.Fatalf("windowing lost the tail: start %d len %d", last.Start, utf8.RuneCountInString(last.Text))
	}
}

func TestChunkTextSkipsWhitespaceOnlyFiles(t *testing.T) {
	if pieces := chunkText("\n\n   \n\n"); len(pieces) != 0 {
		t.Fatalf("want no chunks, got %d", len(pieces))
	}
}

func TestFileTitlePrefersHeadingThenFilename(t *testing.T) {
	if title := fileTitle("chapters/第001章.md", "# 初入宗门\n\n正文"); title != "初入宗门" {
		t.Fatalf("heading title: %q", title)
	}
	if title := fileTitle("chapters/第002章.md", "正文没有标题"); title != "第002章" {
		t.Fatalf("filename title: %q", title)
	}
}
