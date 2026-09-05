package imageanalysis

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
)

// MergeBatches merges results from multiple source batch states into a new
// merged batch. The merge strategy:
//  1. Pages are deduplicated by file name — the first batch's image metadata
//     wins for ordering and file info.
//  2. For the result content, the best available status is chosen across all
//     sources: "success" > "failed" > "pending". If multiple sources provide
//     a "success" result for the same page, the first source in the list to
//     have a success result is used.
//
// Use case: two batches with the same model covering complementary page ranges
// (e.g. one aborted early, another started from mid-book) can be merged into a
// single complete analysis. Or multiple-model batches analyzing the same pages
// can be merged for cross-model consensus.
func MergeBatches(sourceStates []*BatchState, mergedID string, intents []AnalysisIntent) (*BatchState, error) {
	if len(sourceStates) == 0 {
		return nil, fmt.Errorf("没有提供源批次")
	}
	if len(intents) == 0 {
		intents = AllIntents()
	}

	// Phase 1: collect all unique files across all sources, and for each file,
	// pick the best available result (success > failed > pending).
	//
	// We iterate source batches in order. The first batch sets the image order.
	// For results, we scan ALL sources and pick the best status.
	seen := make(map[string]bool) // normalized file name -> seen
	var mergedImages []ImageItem
	var mergedResults []PageResult

	// Pre-build filename→result lookups for all sources so we can cross-reference.
	sourceLookups := make([]map[string]fileResult, len(sourceStates))
	for si, state := range sourceStates {
		lk := make(map[string]fileResult, len(state.Results))
		for _, r := range state.Results {
			lk[r.FileName] = fileResult{
				PageIndex: r.PageIndex,
				Result:    r,
			}
		}
		sourceLookups[si] = lk
	}

	for si, state := range sourceStates {
		for _, img := range state.Images {
			normName := normalizeFileName(img.FileName)
			if seen[normName] {
				continue
			}
			seen[normName] = true

			mergedImages = append(mergedImages, ImageItem{
				PageIndex:   len(mergedImages), // reassign sequential index
				FileName:    img.FileName,
				StoragePath: img.StoragePath,
				MIMEType:    img.MIMEType,
				SizeBytes:   img.SizeBytes,
				Text:        img.Text,
			})

			// Pick the best result for this file across ALL sources.
			// Prefer: success > failed > pending.
			best := pickBestResult(img.FileName, sourceLookups)
			if best != nil {
				best.PageIndex = len(mergedResults)
				mergedResults = append(mergedResults, *best)
			} else {
				mergedResults = append(mergedResults, PageResult{
					PageIndex: len(mergedResults),
					FileName:  img.FileName,
					Status:    "pending",
				})
			}
		}
		_ = si
	}

	now := time.Now().UTC()

	merged := &BatchState{
		ID:         mergedID,
		Status:     BatchCompleted, // will be corrected below
		Intents:    intents,
		Images:     mergedImages,
		Results:    mergedResults,
		CreatedAt:  now,
		StartedAt:  &now,
		FinishedAt: &now,
		TotalPages: len(mergedImages),
		ModelID:    collectModelID(sourceStates),
	}

	// Determine final status.
	successCount := 0
	failedCount := 0
	pendingCount := 0
	for _, r := range merged.Results {
		switch r.Status {
		case "success":
			successCount++
		case "failed":
			failedCount++
		default:
			pendingCount++
		}
	}
	if pendingCount > 0 {
		// Has pending pages — mark as partial.
		merged.Status = BatchPartial
	} else if failedCount == 0 {
		merged.Status = BatchCompleted
	} else if successCount == 0 {
		merged.Status = BatchFailed
	} else {
		merged.Status = BatchPartial
	}

	log.Printf("[image-analysis] merge done id=%s source_count=%d merged_pages=%d success=%d failed=%d pending=%d",
		mergedID, len(sourceStates), merged.TotalPages, successCount, failedCount, pendingCount)

	return merged, nil
}

// fileResult associates a PageResult with its original page index.
type fileResult struct {
	PageIndex int
	Result    PageResult
}

// pickBestResult scans all source lookups for the given file name and returns
// the best available PageResult. Priority: success > failed > pending.
// If no source has a result, returns nil.
func pickBestResult(fileName string, sourceLookups []map[string]fileResult) *PageResult {
	// Priority order: success first, then look for any non-pending result.
	var best *PageResult
	for _, lk := range sourceLookups {
		fr, ok := lk[fileName]
		if !ok {
			continue
		}
		r := fr.Result
		if r.Status == "success" {
			return &r // success is best, return immediately
		}
		if best == nil || (r.Status == "failed" && best.Status == "pending") {
			cp := r
			best = &cp
		}
	}
	return best
}

// normalizeFileName standardises a file name for deduplication comparison.
// Case-insensitive on Windows; strips leading/trailing whitespace.
func normalizeFileName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}

// collectModelID returns the first non-empty model_id across source states,
// or the concatenation if multiple different model IDs are found.
func collectModelID(states []*BatchState) string {
	var models []string
	seen := make(map[string]bool)
	for _, s := range states {
		id := strings.TrimSpace(s.ModelID)
		if id != "" && !seen[id] {
			seen[id] = true
			models = append(models, id)
		}
	}
	if len(models) == 0 {
		return ""
	}
	if len(models) == 1 {
		return models[0]
	}
	// Multiple different models — join them.
	sort.Strings(models)
	return strings.Join(models, "+")
}
