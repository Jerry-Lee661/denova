package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"

	"denova/config"
	"denova/internal/imageanalysis"
)

// --- list_image_analysis_batches ---

type listImageAnalysisBatchesInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"description=返回的最近批次数量，默认 10，最大 50"`
}

type imageAnalysisBatchSummary struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	TotalPages   int    `json:"total_pages"`
	SuccessCount int    `json:"success_count"`
	FailedCount  int    `json:"failed_count"`
	ModelID      string `json:"model_id,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// --- get_image_analysis_result ---

type getImageAnalysisResultInput struct {
	BatchID           string `json:"batch_id" jsonschema:"required,description=批次 ID，从 list_image_analysis_batches 获取"`
	Intent            string `json:"intent,omitempty" jsonschema:"description=只返回指定分析意图的 section，例如 outline、progress、inspiration、state、lore；留空只返回可用意图和长度摘要"`
	Offset            int    `json:"offset,omitempty" jsonschema:"description=指定 section 的字符起点，默认 0；需要继续读取时使用上次返回的 next_offset"`
	Limit             int    `json:"limit,omitempty" jsonschema:"description=指定 section 返回的最大字符数，默认 12000，最大 12000；需要继续读取时使用 next_offset"`
	IncludeFactStream bool   `json:"include_fact_stream,omitempty" jsonschema:"description=是否返回完整按页事实流；默认 false，只有明确需要跨页事实时才启用，并配合 limit 分段读取"`
}

const (
	imageAnalysisResultDefaultLimit = 12000
	imageAnalysisResultMaxLimit     = 12000
)

type boundedImageAnalysisSection struct {
	Content    string `json:"content"`
	Offset     int    `json:"offset"`
	NextOffset int    `json:"next_offset,omitempty"`
	Truncated  bool   `json:"truncated"`
}

type imageAnalysisToolResult struct {
	BatchID      string                                 `json:"batch_id"`
	Status       imageanalysis.BatchStatus              `json:"status"`
	TotalPages   int                                    `json:"total_pages"`
	SuccessCount int                                    `json:"success_count"`
	FailedCount  int                                    `json:"failed_count"`
	Sections     map[string]boundedImageAnalysisSection `json:"sections,omitempty"`
	FactStream   *boundedImageAnalysisSection           `json:"fact_stream,omitempty"`
	SectionSizes map[string]int                         `json:"section_sizes,omitempty"`
}

func boundImageAnalysisText(content string, offset, limit int) boundedImageAnalysisSection {
	if offset < 0 {
		offset = 0
	}
	runes := []rune(content)
	if offset > len(runes) {
		offset = len(runes)
	}
	if limit <= 0 {
		limit = imageAnalysisResultDefaultLimit
	}
	if limit > imageAnalysisResultMaxLimit {
		limit = imageAnalysisResultMaxLimit
	}
	end := offset + limit
	if end > len(runes) {
		end = len(runes)
	}
	result := boundedImageAnalysisSection{
		Content:   string(runes[offset:end]),
		Offset:    offset,
		Truncated: end < len(runes),
	}
	if result.Truncated {
		result.NextOffset = end
	}
	return result
}

// newImageAnalysisTools creates read-only tools for the IDE agent to consume
// completed image analysis batch results without relying on fragile file-path
// conventions. The tools read directly from the workspace's persisted state.
func newImageAnalysisTools(cfg *config.Config) ([]tool.BaseTool, error) {
	if cfg == nil {
		return nil, nil
	}
	workspace := strings.TrimSpace(cfg.Workspace)

	listTool, err := utils.InferTool(
		"list_image_analysis_batches",
		"列出最近的图片分析批次（按时间倒序）。返回每个批次的 ID、状态、页数和成功/失败数。用于查找可消费的分析结果。",
		func(ctx context.Context, input listImageAnalysisBatchesInput) (string, error) {
			_ = ctx
			if workspace == "" {
				return "", fmt.Errorf("当前 workspace 不可用，无法读取图片分析批次")
			}
			limit := input.Limit
			if limit <= 0 {
				limit = 10
			}
			if limit > 50 {
				limit = 50
			}
			store := imageanalysis.NewStore(workspace)
			batches, err := store.ListRecent(limit)
			if err != nil {
				return "", fmt.Errorf("读取图片分析批次失败: %w", err)
			}
			summaries := make([]imageAnalysisBatchSummary, 0, len(batches))
			for _, b := range batches {
				success := 0
				failed := 0
				for _, r := range b.Results {
					if r.Status == "success" {
						success++
					} else if r.Status == "failed" {
						failed++
					}
				}
				summaries = append(summaries, imageAnalysisBatchSummary{
					ID:           b.ID,
					Status:       string(b.Status),
					TotalPages:   b.TotalPages,
					SuccessCount: success,
					FailedCount:  failed,
					ModelID:      b.ModelID,
					CreatedAt:    b.CreatedAt.Format("2006-01-02 15:04:05"),
				})
			}
			data, err := json.Marshal(summaries)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	)
	if err != nil {
		return nil, err
	}

	resultTool, err := utils.InferTool(
		"get_image_analysis_result",
		"获取指定图片分析批次的聚合结果。需要先用 list_image_analysis_batches 获取批次 ID。默认只返回有界 section 内容，不会写入文件；不要用 read_file、glob 或 execute 查找结果。若返回 truncated=true，使用同一 batch_id、同一 intent 和 next_offset 继续调用；只有明确需要跨页事实时才将 include_fact_stream=true，并同样分段读取。",
		func(ctx context.Context, input getImageAnalysisResultInput) (string, error) {
			_ = ctx
			if workspace == "" {
				return "", fmt.Errorf("当前 workspace 不可用，无法读取图片分析结果")
			}
			batchID := strings.TrimSpace(input.BatchID)
			if batchID == "" {
				return "", fmt.Errorf("batch_id 不能为空")
			}
			store := imageanalysis.NewStore(workspace)
			state, err := store.Load(batchID)
			if err != nil {
				return "", fmt.Errorf("批次 %s 不存在或读取失败: %w", batchID, err)
			}
			svc := imageanalysis.NewService(cfg, workspace)
			result := svc.Aggregate(state)
			limit := input.Limit
			if limit <= 0 {
				limit = imageAnalysisResultDefaultLimit
			}
			if limit > imageAnalysisResultMaxLimit {
				limit = imageAnalysisResultMaxLimit
			}
			output := imageAnalysisToolResult{
				BatchID:      result.BatchID,
				Status:       result.Status,
				TotalPages:   result.TotalPages,
				SuccessCount: result.SuccessCount,
				FailedCount:  result.FailedCount,
				Sections:     make(map[string]boundedImageAnalysisSection),
				SectionSizes: make(map[string]int),
			}
			if intent := strings.TrimSpace(input.Intent); intent != "" {
				content, ok := result.Sections[imageanalysis.AnalysisIntent(intent)]
				if !ok {
					return "", fmt.Errorf("图片分析批次 %s 不存在分析意图 %q；可用意图：%v", batchID, intent, imageanalysis.AllIntents())
				}
				output.Sections[intent] = boundImageAnalysisText(content, input.Offset, limit)
			} else {
				for intent, content := range result.Sections {
					output.SectionSizes[string(intent)] = len([]rune(content))
				}
			}
			if input.IncludeFactStream {
				factStream := boundImageAnalysisText(result.FactStream, input.Offset, limit)
				output.FactStream = &factStream
			}
			data, err := json.Marshal(output)
			if err != nil {
				return "", err
			}
			return string(data), nil
		},
	)
	if err != nil {
		return nil, err
	}

	return []tool.BaseTool{listTool, resultTool}, nil
}
