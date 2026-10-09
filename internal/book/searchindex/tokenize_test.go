package searchindex

import (
	"reflect"
	"testing"
)

func TestTokenizeMixedScripts(t *testing.T) {
	tokens := tokenize("林澈ABC 123，剑法sunset。")
	want := []string{"林", "澈", "林澈", "abc", "123", "剑", "法", "剑法", "sunset"}
	if !reflect.DeepEqual(tokens, want) {
		t.Fatalf("tokenize mismatch:\n got %v\nwant %v", tokens, want)
	}
}

func TestTokenizeCJKUnigramsAndBigrams(t *testing.T) {
	tokens := tokenize("云海")
	want := []string{"云", "海", "云海"}
	if !reflect.DeepEqual(tokens, want) {
		t.Fatalf("got %v want %v", tokens, want)
	}
	if single := tokenize("剑"); !reflect.DeepEqual(single, []string{"剑"}) {
		t.Fatalf("single CJK rune: got %v", single)
	}
}

func TestTokenizeLowercasesAndSplitsOnPunctuation(t *testing.T) {
	tokens := tokenize("Hello, World! 剑-客")
	want := []string{"hello", "world", "剑", "客"}
	if !reflect.DeepEqual(tokens, want) {
		t.Fatalf("got %v want %v", tokens, want)
	}
}
