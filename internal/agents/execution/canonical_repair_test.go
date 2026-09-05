package execution

import (
	"context"
	"errors"
	"strings"
	"testing"

	agentsession "github.com/alfredxw/denova/agent/session"

	agent "github.com/alfredxw/denova/agent"
)

// oracleModel satisfies agent.BaseChatModel without ever being called:
// LoadCanonicalMessages only rebuilds the durable transcript.
type oracleModel struct{}

func (oracleModel) Generate(context.Context, []*agent.Message, ...agent.ModelOption) (*agent.Message, error) {
	return nil, errors.New("oracle model must not generate")
}

func (oracleModel) Stream(context.Context, []*agent.Message, ...agent.ModelOption) (*agent.StreamReader[*agent.Message], error) {
	return nil, errors.New("oracle model must not stream")
}

func assistantWithCalls(content string, ids ...string) *agent.Message {
	calls := make([]agent.ToolCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, agent.ToolCall{
			ID:       id,
			Type:     "function",
			Function: agent.FunctionCall{Name: "execute", Arguments: `{}`},
		})
	}
	return agent.AssistantMessage(content, calls)
}

func toolResult(id string) *agent.Message {
	return agent.ToolMessage(agent.TextToolResult("done"), id, agent.WithToolName("execute"))
}

// journalTailTranscript mirrors the crash shape observed in a durable journal:
// a protocol-clean head, then an assistant run killed between tool calls and
// their results (four dangling batches), then the next user turn.
func journalTailTranscript() []*agent.Message {
	return []*agent.Message{
		agent.UserMessage("start"),
		assistantWithCalls("", "call-a", "call-b"),
		toolResult("call-a"),
		toolResult("call-b"),
		assistantWithCalls("", "call-c"),
		toolResult("call-c"),
		assistantWithCalls("", "call-d"),
		assistantWithCalls("", "call-e"),
		assistantWithCalls("", "call-f"),
		assistantWithCalls("", "call-g"),
		agent.UserMessage("continue"),
	}
}

func newOracleSession(t *testing.T) *agent.Session {
	t.Helper()
	owner, err := agent.New(context.Background(), agent.Definition{
		Name:  "canonical-repair-oracle",
		Model: oracleModel{},
	}, agent.WithSessionStore(agentsession.Memory()))
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	session, err := owner.Session(context.Background(), agent.NamedSession("canonical-repair-oracle-session"))
	if err != nil {
		t.Fatalf("owner.Session: %v", err)
	}
	return session
}

// TestJournalTailCrashBricksCanonicalImport reproduces the durable-journal
// crash shape: a run killed between tool calls and their results makes every
// later run fail canonical import with "splits an incomplete tool-result
// batch". The repair must make the same transcript importable.
func TestJournalTailCrashBricksCanonicalImport(t *testing.T) {
	ctx := context.Background()
	transcript := journalTailTranscript()

	session := newOracleSession(t)
	err := session.LoadCanonicalMessages(ctx, transcript)
	if err == nil {
		t.Fatal("expected the raw crash-shaped transcript to be rejected (bug precondition)")
	}
	if !strings.Contains(err.Error(), "incomplete tool-result batch") {
		t.Fatalf("unexpected rejection reason: %v", err)
	}

	repaired, dropped := repairDanglingToolHalves(transcript)
	if dropped.Calls != 4 || dropped.Messages != 4 {
		t.Fatalf("unexpected repair summary: %+v", dropped)
	}
	if err := session.LoadCanonicalMessages(ctx, repaired); err != nil {
		t.Fatalf("repaired transcript must import cleanly: %v", err)
	}
}

// TestRepairDanglingToolHalvesPreservesWellFormedHistory pins the repair's
// no-op guarantee on protocol-clean transcripts.
func TestRepairDanglingToolHalvesPreservesWellFormedHistory(t *testing.T) {
	transcript := []*agent.Message{
		agent.UserMessage("start"),
		assistantWithCalls("", "call-a"),
		toolResult("call-a"),
		agent.AssistantMessage("final answer", nil),
		agent.UserMessage("next"),
	}
	repaired, dropped := repairDanglingToolHalves(transcript)
	if dropped.Calls != 0 || dropped.Messages != 0 {
		t.Fatalf("well-formed transcript must be untouched: %+v", dropped)
	}
	if len(repaired) != len(transcript) {
		t.Fatalf("message count changed: %d -> %d", len(transcript), len(repaired))
	}
}

// TestRepairDanglingToolHalvesCoversEdgeShapes covers the remaining recovery
// cases: orphaned results, misplaced tool fields on user messages, and empty
// assistant batches.
func TestRepairDanglingToolHalvesCoversEdgeShapes(t *testing.T) {
	transcript := []*agent.Message{
		agent.UserMessage("start"),
		assistantWithCalls("", "call-a"),
		toolResult("orphan-id"),                        // orphaned result: dropped
		assistantWithCalls("kept reasoning", "call-b"), // dangling call, assistant keeps content
		toolResult("call-b"),                           // stale result after interruption: dropped
		func() *agent.Message { // user message carrying misplaced tool fields
			m := agent.UserMessage("reply")
			m.ToolCallID = "call-x"
			return m
		}(),
		assistantWithCalls("", "call-c"),
		assistantWithCalls("", "call-c"), // duplicate pending call: dropped
	}
	repaired, dropped := repairDanglingToolHalves(transcript)

	var got []string
	for _, m := range repaired {
		got = append(got, string(m.Role))
	}
	// call-a dropped with its empty assistant; the orphaned result is dropped;
	// call-b keeps its assistant and result; the interrupted call-c pair
	// collapses to nothing; the final user message loses its misplaced fields.
	want := []string{"user", "assistant", "tool", "user"}
	if len(got) != len(want) {
		t.Fatalf("repaired roles = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("repaired roles = %v, want %v", got, want)
		}
	}
	if repaired[1].Content != "kept reasoning" {
		t.Fatalf("assistant independent content was lost: %q", repaired[1].Content)
	}
	// call-b kept its result: the pair is protocol-complete and must survive.
	if len(repaired[1].ToolCalls) != 1 || repaired[1].ToolCalls[0].ID != "call-b" {
		t.Fatalf("completed call-b pair was not preserved: %+v", repaired[1].ToolCalls)
	}
	if repaired[2].ToolCallID != "call-b" {
		t.Fatalf("result of completed call-b was lost: %+v", repaired[2])
	}
	if repaired[3].ToolCallID != "" {
		t.Fatalf("misplaced tool field survived on user message: %q", repaired[3].ToolCallID)
	}
	if dropped.Calls != 3 || dropped.Messages != 4 {
		t.Fatalf("unexpected repair summary: %+v", dropped)
	}
}

// TestRepairDanglingToolHalvesIdempotent guards repeated imports of the same
// journal: repairing an already-repaired transcript changes nothing.
func TestRepairDanglingToolHalvesIdempotent(t *testing.T) {
	once, _ := repairDanglingToolHalves(journalTailTranscript())
	twice, dropped := repairDanglingToolHalves(once)
	if dropped.Calls != 0 || dropped.Messages != 0 {
		t.Fatalf("second repair was not a no-op: %+v", dropped)
	}
	if len(once) != len(twice) {
		t.Fatalf("idempotence violated: %d vs %d", len(once), len(twice))
	}
}
