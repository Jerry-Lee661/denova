package searchindex

import (
	"strings"
	"unicode"
)

// isCJKRune reports whether the rune belongs to a script written without word
// separators. Such runs are indexed with character n-grams instead of words.
func isCJKRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r)
}

// tokenize splits text into indexable tokens: lowercase ASCII word runs plus
// CJK unigrams and bigrams. Bigrams give Chinese queries precision, while
// unigrams keep single-character names and rare terms matchable. BM25
// down-weights the noise that unigrams add.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	var tokens []string
	var word []rune
	var cjk []rune

	flushWord := func() {
		if len(word) > 0 {
			tokens = append(tokens, string(word))
			word = word[:0]
		}
	}
	flushCJK := func() {
		for _, r := range cjk {
			tokens = append(tokens, string(r))
		}
		for i := 0; i+1 < len(cjk); i++ {
			tokens = append(tokens, string(cjk[i:i+2]))
		}
		cjk = cjk[:0]
	}
	for _, r := range text {
		switch {
		case isCJKRune(r):
			flushWord()
			cjk = append(cjk, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushWord()
			flushCJK()
		}
	}
	flushWord()
	flushCJK()
	return tokens
}
