package book

import "testing"

func TestChapterNumberFromTitle(t *testing.T) {
	cases := []struct {
		title string
		want  int
		ok    bool
	}{
		{"第一章 初雪", 1, true},
		{"第12章 巡航", 12, true},
		{"第一百零四章 长夜", 104, true},
		{"第三卷 风暴", 3, true},
		{"第03话 归途", 3, true},
		{"Chapter 7", 7, true},
		{"07、离开", 7, true},
		{"2001太空漫游", 0, false},
		{"三体", 0, false},
		{"序章 风起", 0, false},
		{"附录A", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		got, ok := chapterNumberFromTitle(tc.title)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("chapterNumberFromTitle(%q) = %d/%v, want %d/%v", tc.title, got, ok, tc.want, tc.ok)
		}
	}
}
