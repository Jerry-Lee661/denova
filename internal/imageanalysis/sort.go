package imageanalysis

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// SortByNaturalOrder sorts image items by natural filename order.
// Numeric sequences in filenames are compared numerically so that
// "2.jpg" < "10.jpg" < "100.jpg". Falls back to lexicographic order
// for non-numeric segments.
func SortByNaturalOrder(items []UploadedImage) []UploadedImage {
	sorted := make([]UploadedImage, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool {
		return naturalLess(sorted[i].FileName, sorted[j].FileName)
	})
	return sorted
}

// naturalLess returns true if a should sort before b using natural ordering.
func naturalLess(a, b string) bool {
	aLower := strings.ToLower(a)
	bLower := strings.ToLower(b)
	ai, bi := 0, 0
	for ai < len(aLower) && bi < len(bLower) {
		ac := aLower[ai]
		bc := bLower[bi]
		if unicode.IsDigit(rune(ac)) && unicode.IsDigit(rune(bc)) {
			// Extract numeric segments and compare numerically.
			aNum, aEnd := extractNumber(aLower, ai)
			bNum, bEnd := extractNumber(bLower, bi)
			if aNum != bNum {
				return aNum < bNum
			}
			// Same numeric value: shorter representation first (e.g. "01" vs "1").
			aLen := aEnd - ai
			bLen := bEnd - bi
			if aLen != bLen {
				return aLen < bLen
			}
			ai = aEnd
			bi = bEnd
		} else {
			if ac != bc {
				return ac < bc
			}
			ai++
			bi++
		}
	}
	return len(aLower) < len(bLower)
}

// extractNumber parses a decimal integer starting at position i.
// Returns the numeric value and the index past the last digit.
func extractNumber(s string, i int) (int64, int) {
	j := i
	for j < len(s) && unicode.IsDigit(rune(s[j])) {
		j++
	}
	n, _ := strconv.ParseInt(s[i:j], 10, 64)
	return n, j
}
