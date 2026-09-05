package agent

import (
	"testing"

	"github.com/cloudwego/eino/schema"
)

// TestCutMessagesKeepsToolCallResultPair 验证裁剪时工具调用与其结果不被拆开。
func TestCutMessagesKeepsToolCallResultPair(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("u0"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "call-1", Function: schema.FunctionCall{Name: "read_file", Arguments: "{}"}}}),
		schema.ToolMessage("result", "call-1", schema.WithToolName("read_file")),
		schema.AssistantMessage("plain assistant", nil),
	}

	// 预算极小：至少保留第一个单元（user0），但 tool call 与 result 必须成对。
	cut := CutMessagesToFitBudget(msgs, 1)
	if len(cut) < 1 {
		t.Fatalf("至少应保留一条消息，实际 %d", len(cut))
	}

	// 预算足够大：全部保留。
	full := CutMessagesToFitBudget(msgs, 100000)
	if len(full) != len(msgs) {
		t.Fatalf("预算充足时应保留全部 %d 条，实际 %d", len(msgs), len(full))
	}

	// 预算刚好容纳 user0 + tool pair，但不含末尾 plain assistant。
	partial := CutMessagesToFitBudget(msgs, 50)
	if len(partial) != 3 {
		t.Fatalf("应保留 user0 + tool pair 共 3 条，实际 %d: %#v", len(partial), partial)
	}
	if partial[1].Role != schema.Assistant || len(partial[1].ToolCalls) != 1 {
		t.Fatalf("中间应为带工具调用的 assistant 消息: %#v", partial[1])
	}
	if partial[2].Role != schema.Tool || partial[2].ToolCallID != "call-1" {
		t.Fatalf("尾部应为该调用的结果消息: %#v", partial[2])
	}
}

// TestCutMessagesKeepsFirstBlockWhole 验证首个块即使超预算也整体保留、后续块按预算贪心裁剪。
func TestCutMessagesKeepsFirstBlockWhole(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("a"),
		schema.AssistantMessage("很长的正文内容需要很多 token 来估算", nil),
	}
	// 预算极小：首个块（user "a"）整体保留，第二个块超预算被裁。
	cut := CutMessagesToFitBudget(msgs, 1)
	if len(cut) != 1 {
		t.Fatalf("首个块应整体保留、第二块超预算被裁，实际 %d", len(cut))
	}
	if cut[0].Role != schema.User {
		t.Fatalf("保留的应是首条 user 消息: %#v", cut[0])
	}
}

// TestCutMessagesEmptyInput 验证空输入与零预算的原样返回。
func TestCutMessagesEmptyInput(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("x"),
		schema.ToolMessage("y", "call-1", schema.WithToolName("read_file")),
	}
	if got := CutMessagesToFitBudget(nil, 100); len(got) != 0 {
		t.Fatalf("nil 输入应返回空列表")
	}
	if got := CutMessagesToFitBudget(msgs, 0); len(got) != 2 {
		t.Fatalf("零预算应原样返回全部消息")
	}
}

// TestBuildCutUnitsGroupsCallAndResult 验证单元划分正确归并 tool call 与其结果。
func TestBuildCutUnitsGroupsCallAndResult(t *testing.T) {
	msgs := []*schema.Message{
		schema.UserMessage("u"),
		schema.AssistantMessage("", []schema.ToolCall{{ID: "c1", Function: schema.FunctionCall{Name: "bash", Arguments: "{}"}}}),
		schema.ToolMessage("out", "c1", schema.WithToolName("bash")),
		schema.AssistantMessage("other", nil),
	}
	blocks := buildCutBlocks(msgs)
	var multi []messageBlock
	for _, b := range blocks {
		if len(b.indexes) >= 2 {
			multi = append(multi, b)
		}
	}
	if len(multi) != 1 {
		t.Fatalf("应恰好形成 1 个多元素块，实际 %d", len(multi))
	}
	if len(multi[0].indexes) != 2 {
		t.Fatalf("tool call 与结果应归并为同一块（2 条），实际 %d", len(multi[0].indexes))
	}
}
