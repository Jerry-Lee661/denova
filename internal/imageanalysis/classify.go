package imageanalysis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
)

// BookType classifies the overall nature of a PDF document so the batch can
// pick the right extraction strategy (prompt + panel-aware toggle).
type BookType string

const (
	BookTypeManga        BookType = "manga"        // 漫画：分镜密集，走两阶段提取
	BookTypeNovel        BookType = "novel"        // 小说：连续文字，整页提取
	BookTypeIllustration BookType = "illustration" // 插图集：整页画面描述
	BookTypeMixed        BookType = "mixed"        // 混合：逐页回退分类
)

// classifyBook samples the first N rendered pages and makes a single vision
// call to determine the document's nature. All sample images are packed as
// image_url content parts in one user message.
//
// On any failure it falls back to BookTypeManga to preserve existing behavior.
func (s *Service) classifyBook(ctx context.Context, modelCfg visionModelConfig, sampleImages [][]byte) BookType {
	if len(sampleImages) == 0 {
		return BookTypeManga
	}

	parts := make([]contentPart, 0, len(sampleImages)+1)
	parts = append(parts, contentPart{
		Type: "text",
		Text: fmt.Sprintf("以下是同一本书的前 %d 页，请综合判断这本书的类型。", len(sampleImages)),
	})
	for _, img := range sampleImages {
		dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(img)
		parts = append(parts, contentPart{Type: "image_url", ImageURL: &imageURL{URL: dataURI}})
	}

	sysContent, _ := json.Marshal(bookClassificationInstruction())
	userContent, _ := json.Marshal(parts)
	reqBody := chatRequest{
		Model:     modelCfg.Model,
		MaxTokens: 512,
		Messages: []chatMessage{
			{Role: "system", Content: sysContent},
			{Role: "user", Content: userContent},
		},
	}

	log.Printf("[image-analysis] classify book samples=%d model=%q", len(sampleImages), modelCfg.Model)
	resp, err := s.callVisionAPI(ctx, modelCfg, reqBody)
	if err != nil {
		log.Printf("[image-analysis] classify book failed, defaulting to manga: %v", err)
		return BookTypeManga
	}

	bt, err := parseBookClassification(resp)
	if err != nil {
		log.Printf("[image-analysis] classify book parse failed, defaulting to manga: %v", err)
		return BookTypeManga
	}
	log.Printf("[image-analysis] classify book result=%s", bt)
	return bt
}

// parseBookClassification extracts the book type from the model's JSON response.
func parseBookClassification(content string) (BookType, error) {
	var parsed struct {
		Type string `json:"type"`
	}
	raw := extractJSON(content)
	if raw == "" {
		return "", fmt.Errorf("响应中未找到 JSON")
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", fmt.Errorf("解析分类 JSON 失败: %w", err)
	}
	switch BookType(parsed.Type) {
	case BookTypeManga, BookTypeNovel, BookTypeIllustration, BookTypeMixed:
		return BookType(parsed.Type), nil
	default:
		return "", fmt.Errorf("未知书本类型: %q", parsed.Type)
	}
}
