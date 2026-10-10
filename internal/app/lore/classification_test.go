package loreapp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	booklore "denova/internal/book/lore"
)

func TestSplitClassificationBatchesStaysWithinInputLimit(t *testing.T) {
	body := strings.Repeat("a", 64*1024)
	inputs := make([]booklore.ClassificationInput, 0, 6)
	for index := 0; index < 6; index++ {
		inputs = append(inputs, booklore.ClassificationInput{ID: fmt.Sprintf("item-%d", index), Name: "名称", Content: body})
	}
	batches, err := splitClassificationBatches(inputs)
	if err != nil {
		t.Fatalf("splitClassificationBatches returned an error: %v", err)
	}
	if len(batches) < 2 {
		t.Fatalf("expected several batches, got %d", len(batches))
	}
	covered := 0
	for index, batch := range batches {
		encoded, err := json.Marshal(batch)
		if err != nil {
			t.Fatalf("marshal batch %d: %v", index, err)
		}
		if len(encoded) > classificationPreviewMaxBytes {
			t.Fatalf("batch %d exceeds the input limit: %d bytes", index, len(encoded))
		}
		covered += len(batch)
	}
	if covered != len(inputs) {
		t.Fatalf("batched %d of %d inputs", covered, len(inputs))
	}
}

func TestSplitClassificationBatchesKeepsOversizedInputAlone(t *testing.T) {
	oversized := booklore.ClassificationInput{ID: "big", Name: "big", Content: strings.Repeat("a", classificationPreviewMaxBytes)}
	small := booklore.ClassificationInput{ID: "small", Name: "small"}
	batches, err := splitClassificationBatches([]booklore.ClassificationInput{oversized, small})
	if err != nil {
		t.Fatalf("splitClassificationBatches returned an error: %v", err)
	}
	if len(batches) != 2 {
		t.Fatalf("expected the oversized input to be batched alone, got %d batches", len(batches))
	}
	if len(batches[0]) != 1 || batches[0][0].ID != "big" {
		t.Fatalf("expected the first batch to hold only the oversized input, got %+v", batches[0])
	}
	if len(batches[1]) != 1 || batches[1][0].ID != "small" {
		t.Fatalf("expected the second batch to hold the remaining input, got %+v", batches[1])
	}
}
