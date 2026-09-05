package pdfreader

import "testing"

func TestDetectReadingOrder_SingleBlock(t *testing.T) {
	blocks := []TextBlock{{Text: "a", Left: 100, Top: 200, Right: 110, Bottom: 190}}
	sorted, reordered := DetectReadingOrder(blocks)
	if reordered {
		t.Error("single block should not be reordered")
	}
	if len(sorted) != 1 || sorted[0].Text != "a" {
		t.Errorf("unexpected sorted result: %+v", sorted)
	}
}

func TestDetectReadingOrder_VerticalJapanese(t *testing.T) {
	// Vertical (tategaki) layout: tall narrow columns read right-to-left,
	// characters within a column top-to-bottom. Use tall blocks (height > width)
	// so the layout is detected as vertical.
	// PDF origin is bottom-left, so higher Y = higher on page.
	blocks := []TextBlock{
		{Text: "L-top", Left: 40, Top: 720, Right: 55, Bottom: 650},
		{Text: "L-bot", Left: 40, Top: 640, Right: 55, Bottom: 570},
		{Text: "R-top", Left: 190, Top: 720, Right: 205, Bottom: 650},
		{Text: "R-bot", Left: 190, Top: 640, Right: 205, Bottom: 570},
	}
	sorted, reordered := DetectReadingOrder(blocks)

	// Expected reading order: R-top, R-bot, L-top, L-bot
	want := []string{"R-top", "R-bot", "L-top", "L-bot"}
	for i, w := range want {
		if sorted[i].Text != w {
			t.Errorf("position %d: got %q, want %q", i, sorted[i].Text, w)
		}
	}
	if !reordered {
		t.Error("expected reordered=true since input order differs from reading order")
	}
}

func TestDetectReadingOrder_AlreadyOrdered(t *testing.T) {
	// Blocks already in correct vertical reading order report reordered=false.
	blocks := []TextBlock{
		{Text: "R-top", Left: 190, Top: 720, Right: 205, Bottom: 650},
		{Text: "R-bot", Left: 190, Top: 640, Right: 205, Bottom: 570},
		{Text: "L-top", Left: 40, Top: 720, Right: 55, Bottom: 650},
		{Text: "L-bot", Left: 40, Top: 640, Right: 55, Bottom: 570},
	}
	_, reordered := DetectReadingOrder(blocks)
	if reordered {
		t.Error("already-ordered blocks should report reordered=false")
	}
}

func TestDetectReadingOrder_HorizontalLayout(t *testing.T) {
	// Horizontal layout (wide blocks): read top-to-bottom, left-to-right.
	// PDFium already extracts horizontal text in correct order, so a correctly
	// ordered horizontal page must report reordered=false.
	blocks := []TextBlock{
		{Text: "line1", Left: 50, Top: 700, Right: 400, Bottom: 685},
		{Text: "line2", Left: 50, Top: 660, Right: 400, Bottom: 645},
		{Text: "line3", Left: 50, Top: 620, Right: 400, Bottom: 605},
	}
	sorted, reordered := DetectReadingOrder(blocks)
	want := []string{"line1", "line2", "line3"}
	for i, w := range want {
		if sorted[i].Text != w {
			t.Errorf("position %d: got %q, want %q", i, sorted[i].Text, w)
		}
	}
	if reordered {
		t.Error("correctly ordered horizontal text should report reordered=false")
	}
}
