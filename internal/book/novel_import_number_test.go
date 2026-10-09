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

func TestAssignChapterIndexes(t *testing.T) {
	cases := []struct {
		name   string
		titles []string
		want   []float64
	}{
		{
			name:   "titles keep their own ordinals",
			titles: []string{"第一章 初雪", "第二章 夜行", "第三章 归途"},
			want:   []float64{1, 2, 3},
		},
		{
			name:   "leading front matter takes index 0",
			titles: []string{"前言", "第一章 初雪", "第二章 夜行"},
			want:   []float64{0, 1, 2},
		},
		{
			name:   "several leading chapters spread below 1",
			titles: []string{"制作说明", "前言", "第一章 初雪"},
			want:   []float64{0, 0.5, 1},
		},
		{
			name:   "unnumbered chapter between numbered ones takes x.5",
			titles: []string{"第一章 初雪", "间章 某人的一天", "第二章 夜行"},
			want:   []float64{1, 1.5, 2},
		},
		{
			name:   "several unnumbered chapters spread across the gap",
			titles: []string{"第一章 初雪", "间章 甲", "间章 乙", "第二章 夜行"},
			want:   []float64{1, 1.333, 1.667, 2},
		},
		{
			name:   "trailing chapters stay past the last ordinal",
			titles: []string{"第一章 初雪", "第二章 夜行", "后记"},
			want:   []float64{1, 2, 2.5},
		},
		{
			name:   "no ordinals at all numbers sequentially",
			titles: []string{"初雪", "夜行", "归途"},
			want:   []float64{1, 2, 3},
		},
		{
			name:   "a repeated ordinal becomes unnumbered",
			titles: []string{"第一章 初雪", "第一章 重来", "第二章 夜行"},
			want:   []float64{1, 1.5, 2},
		},
		{
			name:   "out-of-order ordinals keep reading order",
			titles: []string{"第五章 初雪", "第三章 夜行", "间章"},
			want:   []float64{5, 3, 3.5},
		},
		{
			name:   "backwards gap numbers sequentially",
			titles: []string{"第五章 初雪", "间章", "第三章 夜行"},
			want:   []float64{5, 6, 3},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chapters := make([]parsedNovelChapter, len(tc.titles))
			for i, title := range tc.titles {
				chapters[i].Title = title
			}
			assignChapterIndexes(chapters)
			for i := range chapters {
				if chapters[i].Index != tc.want[i] {
					t.Fatalf("chapters[%d] (%q).Index = %v, want %v", i, chapters[i].Title, chapters[i].Index, tc.want[i])
				}
			}
		})
	}
}

func TestChapterOrderText(t *testing.T) {
	cases := []struct {
		index float64
		want  string
	}{
		{0, "ch00000"},
		{1, "ch00001"},
		{1.5, "ch00001.5"},
		{1.333, "ch00001.333"},
		{2.25, "ch00002.25"},
		{100000 - 1, "ch99999"},
	}
	for _, tc := range cases {
		if got := chapterOrderText(tc.index); got != tc.want {
			t.Fatalf("chapterOrderText(%v) = %q, want %q", tc.index, got, tc.want)
		}
	}
}
