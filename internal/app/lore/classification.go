package loreapp

import (
	"context"
	"encoding/json"
	"strings"

	agentcontext "denova/internal/agents/context"
	booklore "denova/internal/book/lore"
)

const (
	classificationPreviewMaxBytes = 256 * 1024
	classificationBodyMaxBytes    = 8 * 1024
)

type ClassificationPreviewRequest struct {
	ItemIDs []string `json:"item_ids,omitempty"`
	Mode    string   `json:"mode,omitempty"`
	// ForceSemantic sends every selected item to the model, including items the
	// local name rules already resolved. It implies the semantic mode.
	ForceSemantic bool `json:"force_semantic,omitempty"`
}

type ClassificationPreviewItem struct {
	ID                string `json:"id"`
	Name              string `json:"name"`
	CurrentType       string `json:"current_type"`
	CurrentTypeSource string `json:"current_type_source"`
	SuggestedType     string `json:"suggested_type"`
	Confidence        string `json:"confidence"`
	Reason            string `json:"reason,omitempty"`
	SuggestionSource  string `json:"suggestion_source"`
}

type ClassificationPreview struct {
	Revision string                      `json:"revision"`
	Mode     string                      `json:"mode"`
	Items    []ClassificationPreviewItem `json:"items"`
	Counts   map[string]int              `json:"counts"`
	Warning  string                      `json:"warning,omitempty"`
}

type ClassificationApplyRequest struct {
	Revision string                `json:"revision"`
	Changes  []booklore.TypeChange `json:"changes"`
}

// ClassifyItems runs semantic classification with the selected Project's
// settings and durable Tool Agent session. It never consults the foreground
// Book, so imports can safely target a background AgentChat Project.
func (service *Service) ClassifyItems(ctx context.Context, projectID string, inputs []booklore.ClassificationInput) ([]booklore.ClassificationSuggestion, error) {
	if service == nil || service.host == nil {
		return nil, ErrNoWorkspace
	}
	return service.host.ClassifyLoreItems(ctx, projectID, inputs)
}

func (service *Service) PreviewClassification(ctx context.Context, projectID string, request ClassificationPreviewRequest) (ClassificationPreview, error) {
	if service == nil || service.images == nil || service.host == nil {
		return ClassificationPreview{}, ErrNoWorkspace
	}
	runtime, err := service.images.AcquireProjectRuntime(ctx, projectID)
	if err != nil {
		return ClassificationPreview{}, err
	}
	defer runtime.Release()

	store := booklore.NewStore(runtime.Workspace)
	items, err := store.ListAll()
	if err != nil {
		return ClassificationPreview{}, err
	}
	revision, err := store.AllRevision()
	if err != nil {
		return ClassificationPreview{}, err
	}
	selected := selectClassificationCandidates(items, request.ItemIDs)
	mode := strings.ToLower(strings.TrimSpace(request.Mode))
	if request.ForceSemantic {
		mode = booklore.ClassificationModeSemantic
	} else if mode != booklore.ClassificationModeSemantic {
		mode = booklore.ClassificationModeHeuristic
	}
	preview := ClassificationPreview{
		Revision: revision,
		Mode:     mode,
		Items:    make([]ClassificationPreviewItem, 0, len(selected)),
		Counts:   make(map[string]int),
	}
	semanticInputs := make([]booklore.ClassificationInput, 0, len(selected))
	previewIndexByID := make(map[string]int, len(selected))
	for _, item := range selected {
		input := classificationInputFromItem(item)
		suggestion := booklore.ClassifyItemHeuristic(input)
		preview.Items = append(preview.Items, ClassificationPreviewItem{
			ID: item.ID, Name: item.Name, CurrentType: item.Type, CurrentTypeSource: item.TypeSource,
			SuggestedType: suggestion.Type, Confidence: suggestion.Confidence, Reason: suggestion.Reason,
			SuggestionSource: booklore.TypeSourceHeuristic,
		})
		previewIndexByID[item.ID] = len(preview.Items) - 1
		if mode != booklore.ClassificationModeSemantic {
			continue
		}
		// A forced run consults the model for every item; otherwise only the
		// entries the local name rules could not settle.
		if !request.ForceSemantic && suggestion.Confidence == booklore.ClassificationConfidenceHigh {
			continue
		}
		semanticInputs = append(semanticInputs, input)
	}
	if mode == booklore.ClassificationModeSemantic && len(semanticInputs) > 0 {
		batches, batchErr := splitClassificationBatches(semanticInputs)
		if batchErr != nil {
			return ClassificationPreview{}, batchErr
		}
		for _, batch := range batches {
			suggestions, classifyErr := service.host.ClassifyLoreItems(runtime.Context(), projectID, batch)
			if classifyErr != nil {
				preview.Warning = "Semantic classification is temporarily unavailable; local name analysis is shown. / 语义分类暂时不可用，当前展示本地名称分析结果：" + classifyErr.Error()
				break
			}
			for _, suggestion := range suggestions {
				index, ok := previewIndexByID[strings.TrimSpace(suggestion.ID)]
				if !ok {
					continue
				}
				preview.Items[index].SuggestedType = suggestion.Type
				preview.Items[index].Confidence = suggestion.Confidence
				preview.Items[index].Reason = suggestion.Reason
				preview.Items[index].SuggestionSource = booklore.TypeSourceSemantic
			}
		}
	}
	for _, item := range preview.Items {
		preview.Counts[item.SuggestedType]++
	}
	return preview, nil
}

// splitClassificationBatches keeps every model call within the input limit so a
// large library is covered by several calls instead of keeping the overflow on
// name-rule results. An oversized single input still forms its own batch.
func splitClassificationBatches(inputs []booklore.ClassificationInput) ([][]booklore.ClassificationInput, error) {
	batches := make([][]booklore.ClassificationInput, 0, 1)
	current := make([]booklore.ClassificationInput, 0, len(inputs))
	usedBytes := 2
	for _, input := range inputs {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		size := len(encoded) + 1
		if len(current) > 0 && usedBytes+size > classificationPreviewMaxBytes {
			batches = append(batches, current)
			current = make([]booklore.ClassificationInput, 0, len(inputs))
			usedBytes = 2
		}
		current = append(current, input)
		usedBytes += size
	}
	if len(current) > 0 {
		batches = append(batches, current)
	}
	return batches, nil
}

func (service *Service) ApplyClassification(ctx context.Context, projectID string, request ClassificationApplyRequest) (booklore.TypeApplyResult, error) {
	var result booklore.TypeApplyResult
	_, err := service.withStore(ctx, projectID, func(store *booklore.Store) error {
		var applyErr error
		result, applyErr = store.ApplyTypeChanges(request.Revision, request.Changes)
		return applyErr
	})
	return result, err
}

func selectClassificationCandidates(items []booklore.Item, requestedIDs []string) []booklore.Item {
	wanted := make(map[string]struct{}, len(requestedIDs))
	for _, id := range requestedIDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}
	explicit := len(wanted) > 0
	result := make([]booklore.Item, 0, len(items))
	for _, item := range items {
		if explicit {
			if _, selected := wanted[item.ID]; selected {
				result = append(result, item)
			}
			continue
		}
		// Preview is read-only and every suggested change still requires explicit
		// confirmation, so all source formats remain visible for review.
		result = append(result, item)
	}
	return result
}

func classificationInputFromItem(item booklore.Item) booklore.ClassificationInput {
	content, _ := agentcontext.TrimUTF8Bytes(item.Content, classificationBodyMaxBytes)
	return booklore.ClassificationInput{
		ID: item.ID, Name: item.Name, Tags: append([]string(nil), item.Tags...), Keywords: append([]string(nil), item.Keywords...),
		BriefDescription: item.BriefDescription, Content: content, CurrentType: item.Type,
	}
}
