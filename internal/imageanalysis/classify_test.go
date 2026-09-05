package imageanalysis

import "testing"

func TestParseBookClassification(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    BookType
		wantErr bool
	}{
		{
			name:    "plain json manga",
			content: `{"type": "manga", "confidence": "high", "reason": "panels"}`,
			want:    BookTypeManga,
		},
		{
			name:    "json in code fence novel",
			content: "```json\n{\"type\": \"novel\", \"confidence\": \"medium\", \"reason\": \"text\"}\n```",
			want:    BookTypeNovel,
		},
		{
			name:    "illustration",
			content: `{"type": "illustration", "confidence": "high", "reason": "full page art"}`,
			want:    BookTypeIllustration,
		},
		{
			name:    "mixed",
			content: `{"type": "mixed", "confidence": "low", "reason": "both"}`,
			want:    BookTypeMixed,
		},
		{
			name:    "unknown type errors",
			content: `{"type": "poetry", "confidence": "high", "reason": "x"}`,
			wantErr: true,
		},
		{
			name:    "no json errors",
			content: "I cannot determine the type.",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBookClassification(tt.content)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTextPageResult(t *testing.T) {
	img := ImageItem{PageIndex: 3, FileName: "p3.png", Text: "extracted text layer"}
	res := textPageResult(img)
	if res.Status != "success" {
		t.Errorf("status = %q, want success", res.Status)
	}
	if res.Content != "extracted text layer" {
		t.Errorf("content = %q", res.Content)
	}
	if len(res.Items) != 1 || res.Items[0].Type != "dialogue" {
		t.Errorf("unexpected items: %+v", res.Items)
	}
	if res.PageIndex != 3 {
		t.Errorf("page index = %d, want 3", res.PageIndex)
	}
}
