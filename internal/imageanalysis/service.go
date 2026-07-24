package imageanalysis

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"github.com/cloudwego/eino/schema"

	"denova/config"
)

// Service orchestrates batch image analysis using a configurable vision model.
type Service struct {
	cfg   *config.Config
	store *Store
}

// NewService creates an image analysis service for the given workspace.
func NewService(cfg *config.Config, workspace string) *Service {
	return &Service{
		cfg:   cfg,
		store: NewStore(workspace),
	}
}

// Store returns the underlying state store.
func (s *Service) Store() *Store {
	return s.store
}

// ProcessBatch runs the full analysis pipeline for a batch.
// It processes images sequentially, emitting progress events via the callback.
// Failed pages are marked but do not abort the batch.
func (s *Service) ProcessBatch(ctx context.Context, state *BatchState, onProgress func(ProgressEvent)) error {
	if onProgress == nil {
		onProgress = func(ProgressEvent) {}
	}
	now := time.Now().UTC()
	state.Status = BatchRunning
	state.StartedAt = &now
	if err := s.store.Save(state); err != nil {
		return fmt.Errorf("保存批次状态失败: %w", err)
	}

	modelCfg := s.modelConfig()
	log.Printf("[image-analysis] batch begin id=%s pages=%d model=%q base_url=%q", state.ID, state.TotalPages, modelCfg.Model, modelCfg.BaseURL)

	for i := range state.Images {
		if ctx.Err() != nil {
			state.Status = BatchAborted
			s.store.Save(state)
			return ctx.Err()
		}
		// Skip pages already successfully processed (retry scenario).
		if state.Results[i].Status == "success" {
			continue
		}
		state.CurrentPage = i
		state.Results[i].Status = "processing"
		s.store.Save(state)

		onProgress(ProgressEvent{
			BatchID:     state.ID,
			Status:      BatchRunning,
			CurrentPage: i,
			TotalPages:  state.TotalPages,
			FileName:    state.Images[i].FileName,
		})

		result, err := s.analyzePage(ctx, modelCfg, state, i)
		if err != nil {
			log.Printf("[image-analysis] page failed id=%s page=%d file=%q err=%v", state.ID, i, state.Images[i].FileName, err)
			state.Results[i].Status = "failed"
			state.Results[i].Error = err.Error()
			onProgress(ProgressEvent{
				BatchID:     state.ID,
				Status:      BatchRunning,
				CurrentPage: i,
				TotalPages:  state.TotalPages,
				FileName:    state.Images[i].FileName,
				Error:       err.Error(),
			})
		} else {
			state.Results[i] = result
			preview := result.Content
			if len([]rune(preview)) > 100 {
				preview = string([]rune(preview)[:100]) + "…"
			}
			onProgress(ProgressEvent{
				BatchID:     state.ID,
				Status:      BatchRunning,
				CurrentPage: i,
				TotalPages:  state.TotalPages,
				FileName:    state.Images[i].FileName,
				Preview:     preview,
			})
		}
		s.store.Save(state)
	}

	// Determine final status.
	failedCount := 0
	for _, r := range state.Results {
		if r.Status == "failed" {
			failedCount++
		}
	}
	finishTime := time.Now().UTC()
	state.FinishedAt = &finishTime
	if failedCount == 0 {
		state.Status = BatchCompleted
	} else if failedCount == state.TotalPages {
		state.Status = BatchFailed
	} else {
		state.Status = BatchPartial
	}
	s.store.Save(state)
	log.Printf("[image-analysis] batch done id=%s status=%s success=%d failed=%d", state.ID, state.Status, state.TotalPages-failedCount, failedCount)
	return nil
}

// RetryPages re-processes specific failed pages (or all failed if indices empty).
func (s *Service) RetryPages(ctx context.Context, batchID string, pageIndices []int, onProgress func(ProgressEvent)) error {
	state, err := s.store.Load(batchID)
	if err != nil {
		return err
	}
	if state.Status == BatchRunning {
		return fmt.Errorf("批次正在处理中，请等待完成")
	}

	// Determine which pages to retry.
	retrySet := make(map[int]bool)
	if len(pageIndices) == 0 {
		for i, r := range state.Results {
			if r.Status == "failed" {
				retrySet[i] = true
			}
		}
	} else {
		for _, idx := range pageIndices {
			if idx >= 0 && idx < len(state.Results) && state.Results[idx].Status == "failed" {
				retrySet[idx] = true
			}
		}
	}
	if len(retrySet) == 0 {
		return fmt.Errorf("没有需要重试的失败页面")
	}

	now := time.Now().UTC()
	state.Status = BatchRunning
	state.StartedAt = &now
	state.FinishedAt = nil
	s.store.Save(state)

	modelCfg := s.modelConfig()
	for i := range state.Images {
		if !retrySet[i] {
			continue
		}
		if ctx.Err() != nil {
			state.Status = BatchAborted
			s.store.Save(state)
			return ctx.Err()
		}
		state.CurrentPage = i
		state.Results[i].Status = "processing"
		s.store.Save(state)

		result, err := s.analyzePage(ctx, modelCfg, state, i)
		retryTime := time.Now().UTC()
		if err != nil {
			state.Results[i].Status = "failed"
			state.Results[i].Error = err.Error()
			state.Results[i].RetriedAt = &retryTime
		} else {
			result.RetriedAt = &retryTime
			state.Results[i] = result
		}
		s.store.Save(state)
	}

	failedCount := 0
	for _, r := range state.Results {
		if r.Status == "failed" {
			failedCount++
		}
	}
	finishTime := time.Now().UTC()
	state.FinishedAt = &finishTime
	if failedCount == 0 {
		state.Status = BatchCompleted
	} else {
		state.Status = BatchPartial
	}
	s.store.Save(state)
	return nil
}

// Aggregate collects all successful results into a BatchResult with
// per-intent aggregated markdown sections.
func (s *Service) Aggregate(state *BatchState) *BatchResult {
	result := &BatchResult{
		BatchID:    state.ID,
		Status:     state.Status,
		TotalPages: state.TotalPages,
		Sections:   make(map[AnalysisIntent]string),
	}
	var allItems []ExtractedItem
	for _, r := range state.Results {
		if r.Status == "success" {
			result.SuccessCount++
			allItems = append(allItems, r.Items...)
		} else if r.Status == "failed" {
			result.FailedCount++
			result.FailedPages = append(result.FailedPages, r)
		}
	}
	for _, intent := range state.Intents {
		result.Sections[intent] = aggregateSection(intent, state, allItems)
	}
	return result
}

// analyzePage sends a single image to the vision model and parses the response.
func (s *Service) analyzePage(ctx context.Context, modelCfg openai.ChatModelConfig, state *BatchState, pageIndex int) (PageResult, error) {
	img := state.Images[pageIndex]
	result := PageResult{
		PageIndex: pageIndex,
		FileName:  img.FileName,
		Status:    "success",
	}

	// Read image data from storage.
	imgData, err := s.readImageData(img)
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		return result, err
	}

	// Build multimodal message using Base64Data + MIMEType (eino recommended
	// approach over data-URI URLs for inline binary data).
	b64 := base64.StdEncoding.EncodeToString(imgData)
	messages := []*schema.Message{
		schema.SystemMessage(systemInstruction(state.Intents)),
		{
			Role: schema.User,
			UserInputMultiContent: []schema.MessageInputPart{
				{
					Type: schema.ChatMessagePartTypeImageURL,
					Image: &schema.MessageInputImage{
						MessagePartCommon: schema.MessagePartCommon{
							Base64Data: &b64,
							MIMEType:   img.MIMEType,
						},
					},
				},
				{
					Type: schema.ChatMessagePartTypeText,
					Text: pageInstruction(pageIndex, img.FileName, state.Intents),
				},
			},
		},
	}

	cm, err := openai.NewChatModel(ctx, &modelCfg)
	if err != nil {
		return result, fmt.Errorf("创建视觉模型失败: %w", err)
	}

	log.Printf("[image-analysis] analyze page id=%s page=%d file=%q model=%q", state.ID, pageIndex, img.FileName, modelCfg.Model)
	msg, err := cm.Generate(ctx, messages)
	if err != nil {
		return result, fmt.Errorf("视觉模型推理失败: %w", err)
	}
	if msg == nil || strings.TrimSpace(msg.Content) == "" {
		return result, fmt.Errorf("视觉模型返回为空")
	}

	// Parse JSON response.
	items, content, err := parseModelResponse(msg.Content)
	if err != nil {
		// If JSON parsing fails, store raw content as fallback.
		result.Content = msg.Content
		result.Items = []ExtractedItem{{Type: "other", Content: msg.Content}}
		log.Printf("[image-analysis] parse fallback id=%s page=%d err=%v", state.ID, pageIndex, err)
		return result, nil
	}
	result.Items = items
	result.Content = content
	return result, nil
}

// modelConfig builds the OpenAI chat model config for the image_analysis agent.
func (s *Service) modelConfig() openai.ChatModelConfig {
	resolved := config.ResolveAgentModel(s.cfg, config.AgentKindImageAnalysis)
	modelCfg := openai.ChatModelConfig{
		APIKey:  resolved.OpenAIAPIKey,
		Model:   resolved.OpenAIModel,
		BaseURL: resolved.OpenAIBaseURL,
	}
	if resolved.Temperature != nil {
		temperature := float32(*resolved.Temperature)
		modelCfg.Temperature = &temperature
	}
	return modelCfg
}

// readImageData reads the raw image bytes from workspace storage.
func (s *Service) readImageData(img ImageItem) ([]byte, error) {
	path := s.store.workspace + "/" + img.StoragePath
	data, err := readFileBytes(path)
	if err != nil {
		return nil, fmt.Errorf("读取图片文件失败 %s: %w", img.StoragePath, err)
	}
	return data, nil
}

// parseModelResponse extracts structured items from the model's JSON output.
func parseModelResponse(content string) ([]ExtractedItem, string, error) {
	// Try to extract JSON from markdown code block if present.
	jsonStr := content
	if idx := strings.Index(content, "```json"); idx >= 0 {
		start := idx + len("```json")
		end := strings.Index(content[start:], "```")
		if end >= 0 {
			jsonStr = strings.TrimSpace(content[start : start+end])
		}
	} else if idx := strings.Index(content, "```"); idx >= 0 {
		start := idx + len("```")
		end := strings.Index(content[start:], "```")
		if end >= 0 {
			jsonStr = strings.TrimSpace(content[start : start+end])
		}
	}

	var parsed struct {
		Items []ExtractedItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(jsonStr), &parsed); err != nil {
		return nil, "", fmt.Errorf("解析模型 JSON 输出失败: %w", err)
	}
	if len(parsed.Items) == 0 {
		return nil, "", fmt.Errorf("模型未返回任何提取条目")
	}

	// Build a combined content string from all items.
	var sb strings.Builder
	for _, item := range parsed.Items {
		sb.WriteString(fmt.Sprintf("### %s\n\n%s\n\n", itemTypeLabel(item.Type), item.Content))
	}
	return parsed.Items, sb.String(), nil
}

// aggregateSection builds a markdown section for one intent from all extracted items.
func aggregateSection(intent AnalysisIntent, state *BatchState, items []ExtractedItem) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("## %s\n\n", intentTitle(intent)))
	sb.WriteString(fmt.Sprintf("*来源：批次 %s，共 %d 页，成功 %d 页*\n\n", state.ID, state.TotalPages, countSuccess(state)))

	// Filter items relevant to this intent (all items contribute to all intents
	// in MVP; future versions can filter by intent-specific types).
	for _, item := range items {
		sb.WriteString(fmt.Sprintf("- **%s**：%s\n", itemTypeLabel(item.Type), truncateRunes(item.Content, 200)))
	}
	return sb.String()
}

func countSuccess(state *BatchState) int {
	n := 0
	for _, r := range state.Results {
		if r.Status == "success" {
			n++
		}
	}
	return n
}

func intentTitle(intent AnalysisIntent) string {
	switch intent {
	case IntentOutline:
		return "大纲提取"
	case IntentProgress:
		return "进度分析"
	case IntentInspiration:
		return "灵感收集"
	case IntentState:
		return "角色状态"
	case IntentLore:
		return "资料库设定"
	default:
		return string(intent)
	}
}

func itemTypeLabel(t string) string {
	switch t {
	case "character":
		return "角色"
	case "scene":
		return "场景"
	case "plot_point":
		return "剧情点"
	case "world_building":
		return "世界观"
	case "dialogue":
		return "对话"
	default:
		return "其他"
	}
}

func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "…"
}

func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}
