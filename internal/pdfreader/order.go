package pdfreader

import "sort"

// DetectReadingOrder sorts text blocks into natural reading order and reports
// whether the input order differed from that natural order. A true result means
// the PDF's native extraction order is unreliable, so the caller should render
// the page and let the vision model OCR it instead of trusting the text layer.
//
// It first detects whether the page layout is predominantly vertical (Japanese
// tategaki: columns read right-to-left, characters top-to-bottom) or horizontal
// (standard: lines read top-to-bottom, left-to-right), then sorts accordingly.
//
// PDF coordinate origin is bottom-left, so a higher Y value is higher on page.
func DetectReadingOrder(blocks []TextBlock) ([]TextBlock, bool) {
	if len(blocks) <= 1 {
		return blocks, false
	}

	sorted := make([]TextBlock, len(blocks))
	copy(sorted, blocks)

	if isVerticalLayout(blocks) {
		// Vertical (tategaki): right column first, then top-to-bottom.
		sort.SliceStable(sorted, func(i, j int) bool {
			ci, cj := columnOf(sorted[i]), columnOf(sorted[j])
			if ci != cj {
				return ci > cj // right column (higher X) first
			}
			return midY(sorted[i]) > midY(sorted[j]) // top first
		})
	} else {
		// Horizontal: top-to-bottom, then left-to-right.
		sort.SliceStable(sorted, func(i, j int) bool {
			ri, rj := rowOf(sorted[i]), rowOf(sorted[j])
			if ri != rj {
				return ri > rj // higher row (higher Y) first
			}
			return midX(sorted[i]) < midX(sorted[j]) // left first
		})
	}

	reordered := false
	for i := range blocks {
		if blocks[i].Text != sorted[i].Text {
			reordered = true
			break
		}
	}
	return sorted, reordered
}

// isVerticalLayout reports whether the page's text blocks are predominantly
// vertical (tall, narrow columns) rather than horizontal (wide, short lines).
// Vertical Japanese text columns have height >> width; horizontal lines have
// width >> height.
func isVerticalLayout(blocks []TextBlock) bool {
	vertical, horizontal := 0, 0
	for _, b := range blocks {
		w := b.Right - b.Left
		h := b.Top - b.Bottom
		switch {
		case h > w:
			vertical++
		case w > h:
			horizontal++
		}
	}
	return vertical > horizontal
}

func midX(b TextBlock) float64 { return (b.Left + b.Right) / 2 }
func midY(b TextBlock) float64 { return (b.Top + b.Bottom) / 2 }

// columnOf quantizes a block's horizontal center into a column index.
// Blocks within ~20 points share a column (used for vertical layout).
func columnOf(b TextBlock) int { return int(midX(b) / 20) }

// rowOf quantizes a block's vertical center into a row index.
// Blocks within ~20 points share a row (used for horizontal layout).
func rowOf(b TextBlock) int { return int(midY(b) / 20) }
