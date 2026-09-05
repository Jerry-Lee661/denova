package agent

import (
	"strings"

	"github.com/cloudwego/eino/schema"
)

// applyMicroCompaction replaces old tool results (those before the last user
// message) with compact placeholders when the cache is cold. This saves tokens
// without losing the ability to reference results by location/idempotency key.
//
// Cache state heuristic:
//   - cold: the prefix will be rebuilt anyway (first request, long gap, or
//     compaction just fired), so replacing old results with placeholders is free.
//   - hot: the KV cache is still warm; we must not change the prefix. In this
//     case we skip micro-compaction (the hot path would use API-level cache
//     editing, which is provider-specific and deferred).
func applyMicroCompaction(history []*schema.Message, cacheHot bool) []*schema.Message {
	if cacheHot || len(history) == 0 {
		return history
	}
	// Find the last user message index; tool results before it are "old".
	lastUserIdx := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i] != nil && history[i].Role == schema.User {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx <= 0 {
		return history
	}
	changed := false
	result := make([]*schema.Message, len(history))
	copy(result, history)
	for i := 0; i < lastUserIdx; i++ {
		msg := result[i]
		if msg == nil || msg.Role != schema.Tool {
			continue
		}
		compacted := microCompactToolResultContent(msg.Content)
		if compacted != msg.Content {
			result[i] = &schema.Message{
				Role:       schema.Tool,
				Content:    compacted,
				ToolCallID: msg.ToolCallID,
				Name:       msg.Name,
			}
			changed = true
		}
	}
	if !changed {
		return history
	}
	return result
}

// microCompactToolResultContent replaces the body of a tool result with a short
// placeholder while preserving the metadata block (idempotency key, location,
// original bytes) so the model can still reference the result.
func microCompactToolResultContent(content string) string {
	idx := strings.Index(content, toolResultMetadataHeader)
	if idx < 0 {
		// No metadata block; leave unchanged to avoid losing information.
		return content
	}
	metadata := strings.TrimSpace(content[idx:])
	body := strings.TrimSpace(content[:idx])
	if body == "" {
		return content
	}
	// If the result was externalized, preserve the location reference so the
	// model can read the full content from disk on demand.
	var placeholder string
	if locRef := extractExternalizeLocation(body); locRef != "" {
		placeholder = microCompactPlaceholder(locRef)
	} else {
		placeholder = "[tool result preview cached, full content on disk]"
	}
	return placeholder + "\n\n" + metadata
}

// extractExternalizeLocation finds the location reference in an externalized
// tool result body. Returns "" if not externalized.
func extractExternalizeLocation(body string) string {
	const marker = "externalized to disk: "
	idx := strings.Index(body, marker)
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := strings.Index(body[start:], "\n")
	if end < 0 {
		return strings.TrimSpace(body[start:])
	}
	return strings.TrimSpace(body[start : start+end])
}
