package book

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// chapterSuffixRunes are the trailing characters that mark a Chinese numeral
// as a chapter ordinal (第三章, 第五卷, 第七话).
const chapterSuffixRunes = "章节卷回集话部册辑"

// assignChapterIndexes numbers chapters in reading order while keeping the
// ordinals the author wrote in titles (第一章 → 1). Chapters without an
// ordinal keep their reading position instead of jumping to the end: leading
// front matter starts at 0 (前言 → ch00000), a chapter between numbered ones
// takes the gap just past its left neighbor (间章 between 第一章 and 第二章 →
// ch00001.5), and several unnumbered chapters in one gap spread evenly across
// it. Files with no ordinals at all are numbered sequentially.
func assignChapterIndexes(chapters []parsedNovelChapter) {
	n := len(chapters)
	if n == 0 {
		return
	}
	ordinals := make([]int, n)
	used := make(map[int]bool, n)
	anchors := make([]int, 0, n)
	for i := range chapters {
		ordinal, ok := chapterNumberFromTitle(chapters[i].Title)
		if !ok || ordinal <= 0 || ordinal > 99999 || used[ordinal] {
			continue
		}
		used[ordinal] = true
		ordinals[i] = ordinal
		anchors = append(anchors, i)
	}
	if len(anchors) == 0 {
		for i := range chapters {
			chapters[i].Index = float64(i + 1)
		}
		return
	}
	// Front matter before the first numbered chapter: 前言 keeps 0 and any
	// further leading chapter spreads below 1 so all of it sorts first.
	leading := anchors[0]
	for i := 0; i < leading; i++ {
		chapters[i].Index = roundChapterIndex(float64(i) / float64(leading))
	}
	for a, position := range anchors {
		ordinal := float64(ordinals[position])
		chapters[position].Index = ordinal
		end := n
		right := ordinal + 1
		if a+1 < len(anchors) {
			end = anchors[a+1]
			right = float64(ordinals[anchors[a+1]])
		}
		gap := end - position - 1
		if gap == 0 {
			continue
		}
		if right <= ordinal {
			// The next title's ordinal goes backwards; keep reading order.
			for k := 1; k <= gap; k++ {
				chapters[position+k].Index = ordinal + float64(k)
			}
			continue
		}
		for k := 1; k <= gap; k++ {
			chapters[position+k].Index = roundChapterIndex(ordinal + (right-ordinal)*float64(k)/float64(gap+1))
		}
	}
}

// roundChapterIndex caps fractional indexes at three decimals so generated
// file names stay short and stable.
func roundChapterIndex(index float64) float64 {
	return math.Round(index*1000) / 1000
}

// chapterOrderText formats an index as the hidden ordering token in chapter
// file names: ch00001 for whole numbers, ch00000 for front matter and
// ch00001.5 for a chapter between 第一章 and 第二章.
func chapterOrderText(index float64) string {
	index = roundChapterIndex(index)
	whole := math.Trunc(index)
	if index == whole {
		return fmt.Sprintf("ch%05d", int64(whole))
	}
	fraction := strings.TrimRight(strings.TrimPrefix(strconv.FormatFloat(index-whole, 'f', 3, 64), "0"), "0")
	return fmt.Sprintf("ch%05d%s", int64(whole), fraction)
}

// chapterNumberFromTitle derives a chapter's own ordinal from its title, so
// imported filenames can match the numbering the author used (第一章 → 1,
// 第03话 → 3, "07、离开" → 7). The second result reports whether the title
// carries a usable ordinal; titles that merely start with a number-like word
// (三体, 2001太空漫游) do not.
func chapterNumberFromTitle(title string) (int, bool) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return 0, false
	}
	if rest, ok := strings.CutPrefix(trimmed, "第"); ok {
		return leadingOrdinal(rest, chapterSuffixRunes)
	}
	if rest, ok := strings.CutPrefix(strings.ToLower(trimmed), "chapter"); ok {
		return leadingOrdinal(rest, "")
	}
	return leadingOrdinal(trimmed, "")
}

// leadingOrdinal parses a leading positive ordinal from s: Arabic digits, or
// Chinese numerals optionally ending with one of suffixRunes. For bare titles
// (no 第/Chapter marker) the Arabic form must end at a separator or the end of
// the title, and the Chinese form requires a chapter suffix rune.
func leadingOrdinal(s, suffixRunes string) (int, bool) {
	s = strings.TrimLeft(s, " \t　:：、.．-—–")
	if s == "" {
		return 0, false
	}
	digits := 0
	for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
		digits++
	}
	if digits > 0 {
		n, err := strconv.Atoi(s[:digits])
		if err != nil || n <= 0 || n > 99999 {
			return 0, false
		}
		if suffixRunes != "" || restStartsWithSeparator(s[digits:]) {
			return n, true
		}
		return 0, false
	}
	return chineseNumeralPrefix(s, suffixRunes)
}

func restStartsWithSeparator(s string) bool {
	if s == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(s)
	return strings.ContainsRune(" \t　:：、.．-—–", r)
}

// chineseNumeralPrefix parses a leading Chinese numeral (一, 十二, 一百零三,
// 三万五千) from s. Bare numerals count as ordinals only when followed by one
// of suffixRunes, so titles like 三体 are not mistaken for numbering.
func chineseNumeralPrefix(s, suffixRunes string) (int, bool) {
	total, section, current := 0, 0, 0
	runes := []rune(s)
	for i, r := range runes {
		switch r {
		case '零', '〇':
		case '一':
			current += 1
		case '二', '两':
			current += 2
		case '三':
			current += 3
		case '四':
			current += 4
		case '五':
			current += 5
		case '六':
			current += 6
		case '七':
			current += 7
		case '八':
			current += 8
		case '九':
			current += 9
		case '十':
			if current == 0 {
				current = 1
			}
			current *= 10
		case '百':
			if current == 0 {
				current = 1
			}
			current *= 100
		case '千':
			if current == 0 {
				current = 1
			}
			current *= 1000
		case '万':
			total += (section + current) * 10000
			section, current = 0, 0
		default:
			value := total + section + current
			if i > 0 && value > 0 && value <= 99999 && strings.ContainsRune(chapterSuffixRunes, r) {
				return value, true
			}
			return 0, false
		}
	}
	value := total + section + current
	if value <= 0 || value > 99999 {
		return 0, false
	}
	if suffixRunes == "" {
		return value, true
	}
	return 0, false
}
