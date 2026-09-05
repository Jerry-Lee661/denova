package imageanalysis

import "testing"

func TestSortByNaturalOrder(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected []string
	}{
		{
			name:     "numeric ordering",
			input:    []string{"10.jpg", "2.jpg", "1.jpg", "100.jpg", "20.jpg"},
			expected: []string{"1.jpg", "2.jpg", "10.jpg", "20.jpg", "100.jpg"},
		},
		{
			name:     "mixed prefix",
			input:    []string{"page_10.png", "page_2.png", "page_1.png"},
			expected: []string{"page_1.png", "page_2.png", "page_10.png"},
		},
		{
			name:     "no numbers falls back to lexicographic",
			input:    []string{"banana.jpg", "apple.jpg", "cherry.jpg"},
			expected: []string{"apple.jpg", "banana.jpg", "cherry.jpg"},
		},
		{
			name:     "leading zeros",
			input:    []string{"01.jpg", "001.jpg", "1.jpg"},
			expected: []string{"1.jpg", "01.jpg", "001.jpg"},
		},
		{
			name:     "case insensitive",
			input:    []string{"B.jpg", "a.jpg", "C.jpg"},
			expected: []string{"a.jpg", "B.jpg", "C.jpg"},
		},
		{
			name:     "single file",
			input:    []string{"only.png"},
			expected: []string{"only.png"},
		},
		{
			name:     "empty",
			input:    []string{},
			expected: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			images := make([]UploadedImage, len(tt.input))
			for i, name := range tt.input {
				images[i] = UploadedImage{FileName: name}
			}
			sorted := SortByNaturalOrder(images)
			if len(sorted) != len(tt.expected) {
				t.Fatalf("expected %d items, got %d", len(tt.expected), len(sorted))
			}
			for i, expected := range tt.expected {
				if sorted[i].FileName != expected {
					t.Errorf("position %d: expected %q, got %q", i, expected, sorted[i].FileName)
				}
			}
		})
	}
}

func TestNaturalLess(t *testing.T) {
	tests := []struct {
		a, b     string
		expected bool
	}{
		{"2.jpg", "10.jpg", true},
		{"10.jpg", "2.jpg", false},
		{"1.jpg", "1.jpg", false},
		{"a.jpg", "b.jpg", true},
		{"page_2.png", "page_10.png", true},
		{"001.jpg", "01.jpg", false}, // same numeric value, longer representation sorts after
	}
	for _, tt := range tests {
		t.Run(tt.a+"_vs_"+tt.b, func(t *testing.T) {
			if got := naturalLess(tt.a, tt.b); got != tt.expected {
				t.Errorf("naturalLess(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.expected)
			}
		})
	}
}
