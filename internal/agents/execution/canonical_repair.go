package execution

import (
	"strings"

	agent "github.com/alfredxw/denova/agent"
)

// canonicalRepairSummary reports what the import repair removed.
type canonicalRepairSummary struct {
	// Calls counts tool-call instances removed as dangling halves.
	Calls int
	// Messages counts whole messages removed (nil records, orphaned results,
	// assistants left empty after their dangling calls were dropped).
	Messages int
}

// repairDanglingToolHalves drops tool-call halves whose completion never
// reached the durable journal. A run killed between an assistant tool call and
// its tool results leaves the transcript tail split; without this repair every
// later run fails canonical import with "splits an incomplete tool-result
// batch" and the session is unusable.
//
// The pass mirrors validateImportedTranscript's own state machine, so anything
// it would reject as a split/orphaned half is removed before import: dangling
// calls (pending when any non-tool message or the transcript end arrives),
// orphaned tool results, repeated pending calls, and misplaced tool protocol
// fields on non-tool messages. The durable journal is never rewritten; the
// repair only shapes the in-memory projection that LoadCanonicalMessages
// re-validates.
func repairDanglingToolHalves(messages []*agent.Message) ([]*agent.Message, canonicalRepairSummary) {
	summary := canonicalRepairSummary{}

	type callRef struct{ messageIndex, callIndex int }
	pending := make(map[string]callRef)
	danglingCalls := make(map[callRef]bool)
	orphanResults := make(map[int]bool)
	nilMessages := make(map[int]bool)

	for index, message := range messages {
		if message == nil {
			nilMessages[index] = true
			summary.Messages++
			continue
		}
		switch message.Role {
		case agent.ToolRole:
			id := strings.TrimSpace(message.ToolCallID)
			ref, ok := pending[id]
			if id == "" || !ok {
				orphanResults[index] = true
				summary.Messages++
				continue
			}
			delete(pending, id)
			_ = ref
		case agent.Assistant:
			for _, ref := range pending {
				danglingCalls[ref] = true
				summary.Calls++
			}
			pending = make(map[string]callRef)
			for callIndex, call := range message.ToolCalls {
				id := strings.TrimSpace(call.ID)
				ref := callRef{messageIndex: index, callIndex: callIndex}
				if id == "" || strings.TrimSpace(call.Function.Name) == "" {
					danglingCalls[ref] = true
					summary.Calls++
					continue
				}
				if _, repeated := pending[id]; repeated {
					danglingCalls[ref] = true
					summary.Calls++
					continue
				}
				pending[id] = ref
			}
		default:
			for _, ref := range pending {
				danglingCalls[ref] = true
				summary.Calls++
			}
			pending = make(map[string]callRef)
		}
	}
	for _, ref := range pending {
		danglingCalls[ref] = true
		summary.Calls++
	}

	repaired := make([]*agent.Message, 0, len(messages))
	for index, message := range messages {
		if message == nil {
			continue
		}
		switch message.Role {
		case agent.ToolRole:
			if orphanResults[index] {
				continue
			}
			repaired = append(repaired, message)
		case agent.Assistant:
			next := message.Clone()
			kept := make([]agent.ToolCall, 0, len(next.ToolCalls))
			for callIndex, call := range next.ToolCalls {
				if danglingCalls[callRef{messageIndex: index, callIndex: callIndex}] {
					continue
				}
				kept = append(kept, call)
			}
			next.ToolCalls = kept
			// Misplaced result-only fields are malformed halves; the ordinary
			// assistant content stays independently meaningful.
			next.ToolCallID = ""
			next.ToolName = ""
			next.ToolResult = nil
			if len(next.ToolCalls) == 0 && !assistantHasImportableContent(next) {
				summary.Messages++
				continue
			}
			repaired = append(repaired, next)
		default:
			next := message.Clone()
			next.ToolCalls = nil
			next.ToolCallID = ""
			next.ToolName = ""
			next.ToolResult = nil
			repaired = append(repaired, next)
		}
	}
	return repaired, summary
}

// assistantHasImportableContent reports whether an assistant message remains
// meaningful after its tool calls were removed.
func assistantHasImportableContent(message *agent.Message) bool {
	if message == nil {
		return false
	}
	return message.Content != "" || message.Name != "" || message.ReasoningContent != "" ||
		len(message.MultiContent) > 0 || len(message.AssistantGenMultiContent) > 0
}
